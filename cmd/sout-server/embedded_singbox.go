package main

// Build with: -tags "with_gvisor with_quic with_utls with_wireguard with_cloudflared netgo osusergo".
// OpenVPN system:false requires gVisor; QUIC is supported for extensions.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	sbcertificate "github.com/sagernet/sing-box/adapter/certificate"
	sbendpoint "github.com/sagernet/sing-box/adapter/endpoint"
	sbinbound "github.com/sagernet/sing-box/adapter/inbound"
	sboutbound "github.com/sagernet/sing-box/adapter/outbound"
	sbservice "github.com/sagernet/sing-box/adapter/service"
	sbdns "github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/openvpn"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/trojan"
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing-box/protocol/vmess"
	"github.com/sagernet/sing-box/protocol/wireguard"
	SBJSON "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

const soutDynamicOutboundType = "sout-dynamic"

var (
	globalEngineMu sync.Mutex
	globalEngine   *embeddedEngine
)

func getEmbeddedEngine() *embeddedEngine {
	globalEngineMu.Lock()
	defer globalEngineMu.Unlock()
	return globalEngine
}

func setGlobalEmbeddedEngine(e *embeddedEngine) {
	globalEngineMu.Lock()
	defer globalEngineMu.Unlock()
	globalEngine = e
}

func getOrInitEmbeddedEngine(listenIP string) (*embeddedEngine, error) {
	globalEngineMu.Lock()
	defer globalEngineMu.Unlock()
	if globalEngine != nil {
		return globalEngine, nil
	}
	engine, err := newEmbeddedEngine(listenIP)
	if err != nil {
		return nil, err
	}
	globalEngine = engine
	return engine, nil
}

// embeddedEngine 管理唯一的 sing-box 用户态常驻实例，接管所有入站与出口。
// 出口与入站完全解耦，通过 Endpoint/Inbound Registry 进行细粒度动态热插拔。
type embeddedEngine struct {
	mu sync.Mutex

	ctx context.Context
	box *sbox.Box

	routes           map[string]embeddedRoute
	userRoutes       map[string]embeddedRoute
	inboundConfigs   map[string]map[string]any
	inboundHashes    map[string][32]byte
	activeTunnels    map[int]*Tunnel
	chainUpstreamTag string
	listenIP         string
}

type embeddedRoute struct {
	endpoint string
	direct   bool
	block    bool
}

type soutDynamicOutboundOptions struct{}

type soutDynamicOutbound struct {
	sboutbound.Adapter
	engine *embeddedEngine
}

func newEmbeddedEngine(listenIP string) (*embeddedEngine, error) {
	if listenIP == "" || listenIP == "0.0.0.0" {
		listenIP = "127.0.0.1"
	}
	engine := &embeddedEngine{
		routes:         make(map[string]embeddedRoute),
		userRoutes:     make(map[string]embeddedRoute),
		inboundConfigs: make(map[string]map[string]any),
		inboundHashes:  make(map[string][32]byte),
		activeTunnels:  make(map[int]*Tunnel),
		listenIP:       listenIP,
	}

	inboundRegistry := sbinbound.NewRegistry()
	outboundRegistry := sboutbound.NewRegistry()
	endpointRegistry := sbendpoint.NewRegistry()

	socks.RegisterInbound(inboundRegistry)
	vless.RegisterInbound(inboundRegistry)
	vmess.RegisterInbound(inboundRegistry)
	trojan.RegisterInbound(inboundRegistry)
	hysteria2.RegisterInbound(inboundRegistry)
	tuic.RegisterInbound(inboundRegistry)
	registerCloudflaredInbound(inboundRegistry)

	openvpn.RegisterEndpoint(endpointRegistry)
	wireguard.RegisterEndpoint(endpointRegistry)

	sboutbound.Register[soutDynamicOutboundOptions](outboundRegistry, soutDynamicOutboundType,
		func(_ context.Context, _ adapter.Router, _ log.ContextLogger, tag string, _ soutDynamicOutboundOptions) (adapter.Outbound, error) {
			return &soutDynamicOutbound{
				Adapter: sboutbound.NewAdapter(soutDynamicOutboundType, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
				engine:  engine,
			}, nil
		})

	dnsRegistry := sbdns.NewTransportRegistry()
	local.RegisterTransport(dnsRegistry)
	ctx := sbox.Context(context.Background(), inboundRegistry, outboundRegistry, endpointRegistry,
		dnsRegistry, sbservice.NewRegistry(), sbcertificate.NewRegistry())
	engine.ctx = ctx

	dnsConfig := map[string]any{
		"strategy": "prefer_ipv6",
		"servers": []any{
			map[string]any{
				"tag":  "default-dns",
				"type": "local",
			},
		},
	}
	dnsOpt, err := decodeSingBoxOptions[*option.DNSOptions](ctx, dnsConfig)
	if err != nil {
		return nil, fmt.Errorf("解析 DNS 配置失败: %w", err)
	}

	box, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: option.Options{
			Log: &option.LogOptions{Level: "warn", Timestamp: true},
			DNS: dnsOpt,
			Outbounds: []option.Outbound{{
				Type:    soutDynamicOutboundType,
				Tag:     soutDynamicOutboundType,
				Options: &soutDynamicOutboundOptions{},
			}},
			Route: &option.RouteOptions{Final: soutDynamicOutboundType},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("创建内嵌 sing-box 初始实例失败: %w", err)
	}

	if err := box.Start(); err != nil {
		return nil, fmt.Errorf("启动内嵌 sing-box 初始实例失败: %w", err)
	}

	engine.box = box
	return engine, nil
}

func (o *soutDynamicOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	dialer, destination, err := o.endpointFor(ctx, destination)
	if err != nil {
		return nil, err
	}
	return dialer.DialContext(ctx, network, destination)
}

func (o *soutDynamicOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	dialer, destination, err := o.endpointFor(ctx, destination)
	if err != nil {
		return nil, err
	}
	return dialer.ListenPacket(ctx, destination)
}

func (o *soutDynamicOutbound) endpointFor(ctx context.Context, destination M.Socksaddr) (N.Dialer, M.Socksaddr, error) {
	metadata := adapter.ContextFrom(ctx)
	if metadata == nil || metadata.Inbound == "" {
		return nil, destination, fmt.Errorf("sout 路由缺少入站标识")
	}
	if isEmbeddedEndpointInbound(metadata.Inbound) {
		return N.SystemDialer, destination, nil
	}

	o.engine.mu.Lock()
	var route embeddedRoute
	var found bool

	// 1. 优先匹配认证用户级路由（分流用户 soutu...）
	if metadata.User != "" && o.engine.userRoutes != nil {
		route, found = o.engine.userRoutes[metadata.User]
	}
	// 2. 若未匹配到用户路由，使用入站级路由
	if !found && o.engine.routes != nil {
		route, found = o.engine.routes[metadata.Inbound]
	}
	box := o.engine.box
	o.engine.mu.Unlock()

	if !found {
		return N.SystemDialer, destination, nil
	}
	if route.block {
		return nil, destination, fmt.Errorf("入站 %s 绑定的出口当前不可用", metadata.Inbound)
	}
	if route.direct || route.endpoint == "" || route.endpoint == "direct" {
		return N.SystemDialer, destination, nil
	}

	isWG := strings.HasPrefix(route.endpoint, "soutwireguard")
	if !isWG && destination.IsIPv6() {
		return nil, destination, fmt.Errorf("当前出口不支持 IPv6，已安全阻断以防真实 IPv6 泄露")
	}
	if destination.IsDomain() {
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", destination.Fqdn)
		if err != nil {
			return nil, destination, err
		}
		var chosen netip.Addr
		for _, address := range addresses {
			if address.Is4() {
				chosen = address
				break
			} else if isWG && address.Is6() && !chosen.IsValid() {
				chosen = address
			}
		}
		if chosen.IsValid() {
			destination = M.SocksaddrFrom(chosen, destination.Port)
		} else {
			return nil, destination, fmt.Errorf("域名 %s 仅解析到 IPv6 且当前出口不支持 IPv6，已安全阻断以防真实 IPv6 泄露", destination.Fqdn)
		}
	}
	if box == nil {
		return nil, destination, fmt.Errorf("内嵌 sing-box 已关闭")
	}
	endpoint, found := box.Endpoint().Get(route.endpoint)
	if !found {
		return nil, destination, fmt.Errorf("入站 %s 的出口已移除", metadata.Inbound)
	}
	return endpoint, destination, nil
}

func isEmbeddedEndpointInbound(tag string) bool {
	return strings.HasPrefix(tag, "soutopenvpn") || strings.HasPrefix(tag, "sout-openvpn-") || strings.HasPrefix(tag, "fanoutopenvpn") || strings.HasPrefix(tag, "fanout-openvpn-") || strings.HasPrefix(tag, "soutwireguard") || strings.HasPrefix(tag, "sout-wireguard-")
}

func (e *embeddedEngine) close() error {
	e.mu.Lock()
	box := e.box
	e.box = nil
	e.mu.Unlock()
	if box == nil {
		return nil
	}
	closeDone := make(chan struct{})
	go func() {
		_ = box.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (e *embeddedEngine) hasTunnel(slot int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, found := e.activeTunnels[slot]
	return found && e.box != nil
}

func (e *embeddedEngine) HasInbound(tag string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return false
	}
	if _, ok := e.inboundConfigs[tag]; ok {
		return true
	}
	_, found := e.box.Inbound().Get(tag)
	return found
}

func (e *embeddedEngine) portAvailable(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort(e.listenIP, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func hashInboundConfig(raw map[string]any) ([32]byte, error) {
	blob, err := json.Marshal(raw)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(blob), nil
}

// AddInbound 在内存态下发与热加载单个入站
func (e *embeddedEngine) AddInbound(rawConfig map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return fmt.Errorf("内嵌 sing-box 已关闭")
	}

	tag, _ := rawConfig["tag"].(string)
	typ, _ := rawConfig["type"].(string)
	if tag == "" || typ == "" {
		return fmt.Errorf("入站缺少 tag 或 type")
	}

	h, err := hashInboundConfig(rawConfig)
	if err != nil {
		return fmt.Errorf("计算入站 %s 哈希失败: %w", tag, err)
	}

	// 若已有同 tag，先 Remove
	if _, running := e.box.Inbound().Get(tag); running {
		_ = e.box.Inbound().Remove(tag)
	}

	opt, err := decodeSingBoxOptions[option.Inbound](e.ctx, rawConfig)
	if err != nil {
		return fmt.Errorf("解析入站 %s 配置失败: %w", tag, err)
	}
	if err := e.box.Inbound().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger("inbound"), opt.Tag, opt.Type, opt.Options); err != nil {
		return fmt.Errorf("启动入站 %s 失败: %w", tag, err)
	}

	e.inboundConfigs[tag] = rawConfig
	e.inboundHashes[tag] = h
	return nil
}

// SyncInbounds 批量原子热同步所有配置的入站，做细粒度差异比对，未变化的入站绝不重启
func (e *embeddedEngine) SyncInbounds(inbounds []map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return fmt.Errorf("内嵌 sing-box 已关闭")
	}

	desired := make(map[string]map[string]any, len(inbounds))
	desiredHashes := make(map[string][32]byte, len(inbounds))
	for _, raw := range inbounds {
		tag, _ := raw["tag"].(string)
		typ, _ := raw["type"].(string)
		if tag == "" || typ == "" {
			continue
		}
		h, err := hashInboundConfig(raw)
		if err != nil {
			return fmt.Errorf("计算入站 %s 哈希失败: %w", tag, err)
		}
		desired[tag] = raw
		desiredHashes[tag] = h
	}

	// 1. 已删除的入站：Remove
	for tag := range e.inboundConfigs {
		if _, exists := desired[tag]; !exists {
			if _, running := e.box.Inbound().Get(tag); running {
				_ = e.box.Inbound().Remove(tag)
			}
			delete(e.inboundConfigs, tag)
			delete(e.inboundHashes, tag)
			delete(e.routes, tag)
		}
	}

	// 2. 差异对比与同步
	var firstErr error
	for tag, raw := range desired {
		newHash := desiredHashes[tag]
		oldHash, exists := e.inboundHashes[tag]

		// 未变化的入站（尤其是稳定连接的 cf-tunnel-in 和已启动的公网节点）：完全不动，严禁 Remove/Create！
		if exists && oldHash == newHash {
			if _, running := e.box.Inbound().Get(tag); running {
				continue
			}
		}

		// 变化的入站：先 Remove 再 Create
		if exists {
			if _, running := e.box.Inbound().Get(tag); running {
				_ = e.box.Inbound().Remove(tag)
			}
			delete(e.inboundConfigs, tag)
			delete(e.inboundHashes, tag)
		}

		// 新增或变化的入站：Create
		opt, err := decodeSingBoxOptions[option.Inbound](e.ctx, raw)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("解析入站 %s 配置失败: %w", tag, err)
			}
			continue
		}
		if err := e.box.Inbound().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger("inbound"), opt.Tag, opt.Type, opt.Options); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("启动入站 %s 失败: %w", tag, err)
			}
			continue
		}

		e.inboundConfigs[tag] = raw
		e.inboundHashes[tag] = newHash
	}

	return firstErr
}

// RemoveInbound 在内存态注销指定的入站
func (e *embeddedEngine) RemoveInbound(tag string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return fmt.Errorf("内嵌 sing-box 已关闭")
	}

	if _, running := e.box.Inbound().Get(tag); running {
		if err := e.box.Inbound().Remove(tag); err != nil {
			return fmt.Errorf("移除入站 %s 失败: %w", tag, err)
		}
	}
	delete(e.inboundConfigs, tag)
	delete(e.inboundHashes, tag)
	delete(e.routes, tag)
	return nil
}

// SyncRoutes 热同步入站路由与用户级分流路由表
func (e *embeddedEngine) SyncRoutes(routes map[string]embeddedRoute, userRoutes map[string]embeddedRoute) {
	e.mu.Lock()
	defer e.mu.Unlock()

	nextRoutes := make(map[string]embeddedRoute, len(routes)+len(e.activeTunnels))
	for k, v := range routes {
		nextRoutes[k] = v
	}
	for slot, t := range e.activeTunnels {
		if t.Port > 0 {
			socksTag := fmt.Sprintf("soutsocks%d", slot)
			endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
			if t.CustomProto == "wireguard" || t.Node.Protocol == "wireguard" {
				endpointTag = fmt.Sprintf("soutwireguard%d", slot)
			}
			nextRoutes[socksTag] = embeddedRoute{endpoint: endpointTag}
		}
	}
	e.routes = nextRoutes

	nextUsers := make(map[string]embeddedRoute, len(userRoutes))
	for k, v := range userRoutes {
		nextUsers[k] = v
	}
	e.userRoutes = nextUsers
}

func (e *embeddedEngine) setChainUpstream(tag string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.chainUpstreamTag == tag {
		return
	}
	e.chainUpstreamTag = tag
	if e.box == nil {
		return
	}

	// 遍历已有的非 chainUpstream 的 activeTunnels，仅为其重新挂载 endpoint（更新 detour），绝不重启任何 Inbound
	for slot, tunnel := range e.activeTunnels {
		isWG := tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard"
		endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
		if isWG {
			endpointTag = fmt.Sprintf("soutwireguard%d", slot)
		}

		if endpointTag == e.chainUpstreamTag || tunnel.IsChainUpstream {
			continue
		}

		var endpointConfig map[string]any
		var err error
		if isWG {
			endpointConfig, err = wireguardEndpoint(tunnel.Node.Config, endpointTag)
		} else {
			endpointConfig, err = openVPNEndpoint(tunnel.Node.Config, endpointTag)
		}
		if err != nil {
			continue
		}

		if e.chainUpstreamTag != "" {
			endpointConfig["detour"] = e.chainUpstreamTag
		}

		opt, err := decodeSingBoxOptions[option.Endpoint](e.ctx, endpointConfig)
		if err != nil {
			continue
		}

		_ = e.box.Endpoint().Remove(endpointTag)
		_ = e.box.Endpoint().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger("endpoint"), opt.Tag, opt.Type, opt.Options)
	}
}

func (e *embeddedEngine) addTunnel(tunnel *Tunnel) error {
	if tunnel == nil {
		return fmt.Errorf("tunnel 不能为空")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return fmt.Errorf("内嵌 sing-box 已关闭")
	}

	slot := tunnel.Slot
	isWG := tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard"
	endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
	if isWG {
		endpointTag = fmt.Sprintf("soutwireguard%d", slot)
	}
	socksTag := fmt.Sprintf("soutsocks%d", slot)

	// 若该 slot 之前有 endpoint/socks，先调用 Remove
	if prev, exists := e.activeTunnels[slot]; exists {
		prevIsWG := prev.CustomProto == "wireguard" || prev.Node.Protocol == "wireguard"
		prevEndpointTag := fmt.Sprintf("soutopenvpn%d", slot)
		if prevIsWG {
			prevEndpointTag = fmt.Sprintf("soutwireguard%d", slot)
		}
		prevSocksTag := fmt.Sprintf("soutsocks%d", slot)
		_ = e.box.Inbound().Remove(prevSocksTag)
		_ = e.box.Endpoint().Remove(prevEndpointTag)
		delete(e.activeTunnels, slot)
	}

	var endpointConfig map[string]any
	var err error
	if isWG {
		endpointConfig, err = wireguardEndpoint(tunnel.Node.Config, endpointTag)
		if err != nil {
			return fmt.Errorf("转换 WireGuard 配置失败: %w", err)
		}
	} else {
		endpointConfig, err = openVPNEndpoint(tunnel.Node.Config, endpointTag)
		if err != nil {
			return fmt.Errorf("转换 VPN Gate 配置失败: %w", err)
		}
	}

	if endpointTag != e.chainUpstreamTag && !tunnel.IsChainUpstream && e.chainUpstreamTag != "" {
		endpointConfig["detour"] = e.chainUpstreamTag
	}

	opt, err := decodeSingBoxOptions[option.Endpoint](e.ctx, endpointConfig)
	if err != nil {
		return fmt.Errorf("解析 endpoint %s 配置失败: %w", endpointTag, err)
	}

	if err := e.box.Endpoint().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger("endpoint"), opt.Tag, opt.Type, opt.Options); err != nil {
		return fmt.Errorf("启动 endpoint %s 失败: %w", endpointTag, err)
	}

	nextRoutes := cloneEmbeddedRoutes(e.routes)
	// 如果之前因 removeTunnel 被置为 block 的路由指向该 endpointTag，在此予以解除 block
	for tag, r := range nextRoutes {
		if r.endpoint == endpointTag && r.block {
			nextRoutes[tag] = embeddedRoute{endpoint: endpointTag, block: false}
		}
	}

	if tunnel.Port > 0 {
		socksConfig := map[string]any{
			"type":        "socks",
			"tag":         socksTag,
			"listen":      e.listenIP,
			"listen_port": tunnel.Port,
		}
		cred := tunnel.Cred
		if cred.User != "" && cred.Pass != "" {
			socksConfig["users"] = []any{map[string]any{"username": cred.User, "password": cred.Pass}}
		}
		socksOpt, err := decodeSingBoxOptions[option.Inbound](e.ctx, socksConfig)
		if err != nil {
			_ = e.box.Endpoint().Remove(endpointTag)
			return fmt.Errorf("解析公网 SOCKS5 配置失败: %w", err)
		}
		if err := e.box.Inbound().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger("socks"), socksOpt.Tag, socksOpt.Type, socksOpt.Options); err != nil {
			_ = e.box.Endpoint().Remove(endpointTag)
			return fmt.Errorf("启动公网 SOCKS5 失败: %w", err)
		}
		nextRoutes[socksTag] = embeddedRoute{endpoint: endpointTag}
	} else {
		delete(nextRoutes, socksTag)
	}
	e.routes = nextRoutes

	if e.userRoutes != nil {
		nextUsers := cloneEmbeddedRoutes(e.userRoutes)
		for u, r := range nextUsers {
			if r.endpoint == endpointTag && r.block {
				nextUsers[u] = embeddedRoute{endpoint: endpointTag, block: false}
			}
		}
		e.userRoutes = nextUsers
	}

	e.activeTunnels[slot] = tunnel
	return nil
}

func (e *embeddedEngine) removeTunnel(tunnel *Tunnel) {
	if tunnel == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return
	}

	slot := tunnel.Slot
	isWG := tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard"
	endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
	if isWG {
		endpointTag = fmt.Sprintf("soutwireguard%d", slot)
	}
	socksTag := fmt.Sprintf("soutsocks%d", slot)

	_ = e.box.Inbound().Remove(socksTag)
	_ = e.box.Endpoint().Remove(endpointTag)

	nextRoutes := cloneEmbeddedRoutes(e.routes)
	delete(nextRoutes, socksTag)
	for tag, r := range nextRoutes {
		if r.endpoint == endpointTag {
			nextRoutes[tag] = embeddedRoute{endpoint: endpointTag, block: true}
		}
	}
	e.routes = nextRoutes

	if e.userRoutes != nil {
		nextUsers := cloneEmbeddedRoutes(e.userRoutes)
		for u, r := range nextUsers {
			if r.endpoint == endpointTag {
				nextUsers[u] = embeddedRoute{endpoint: endpointTag, block: true}
			}
		}
		e.userRoutes = nextUsers
	}

	delete(e.activeTunnels, slot)
}

func (e *embeddedEngine) dialTunnel(ctx context.Context, tunnel *Tunnel, network, address string) (net.Conn, error) {
	e.mu.Lock()
	box := e.box
	slot := tunnel.Slot
	endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
	if tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard" {
		endpointTag = fmt.Sprintf("soutwireguard%d", slot)
	}
	e.mu.Unlock()

	if box == nil {
		return nil, fmt.Errorf("内嵌 sing-box 已关闭")
	}
	endpoint, found := box.Endpoint().Get(endpointTag)
	if !found {
		return nil, fmt.Errorf("出口 endpoint %s 未运行", endpointTag)
	}
	destination := M.ParseSocksaddr(address)
	if !destination.IsValid() {
		return nil, fmt.Errorf("目标地址无效: %s", address)
	}
	if destination.IsDomain() {
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", destination.Fqdn)
		if err != nil || len(addresses) == 0 {
			return nil, fmt.Errorf("解析 IPv4 目标失败: %w", err)
		}
		destination = M.SocksaddrFrom(addresses[0], destination.Port)
	}
	return endpoint.DialContext(ctx, network, destination)
}

func wireguardEndpoint(configJSON, tag string) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return nil, fmt.Errorf("解析 WireGuard 配置 JSON 失败: %w", err)
	}

	server, _ := raw["server"].(string)
	serverPort, _ := raw["server_port"].(float64)
	privKey, _ := raw["private_key"].(string)
	pubKey, _ := raw["peer_public_key"].(string)
	preSharedKey, _ := raw["pre_shared_key"].(string)
	reservedRaw, _ := raw["reserved"].([]any)
	localAddressRaw, _ := raw["local_address"].([]any)
	mtu, _ := raw["mtu"].(float64)
	if mtu == 0 {
		mtu = 1420
	}

	peer := map[string]any{
		"address":     server,
		"port":        int(serverPort),
		"public_key":  pubKey,
		"allowed_ips": []string{"0.0.0.0/0", "::/0"},
	}
	if preSharedKey != "" {
		peer["pre_shared_key"] = preSharedKey
	}
	if len(reservedRaw) > 0 {
		var reserved []uint8
		for _, v := range reservedRaw {
			if num, ok := v.(float64); ok {
				reserved = append(reserved, uint8(num))
			}
		}
		if len(reserved) > 0 {
			peer["reserved"] = reserved
		}
	}

	var addresses []string
	for _, v := range localAddressRaw {
		if s, ok := v.(string); ok && s != "" {
			addresses = append(addresses, s)
		}
	}
	if len(addresses) == 0 {
		addresses = []string{"10.0.0.2/32"}
	}

	endpoint := map[string]any{
		"type":        "wireguard",
		"tag":         tag,
		"system":      false,
		"address":     addresses,
		"private_key": privKey,
		"mtu":         int(mtu),
		"peers":       []any{peer},
	}
	return endpoint, nil
}

func cloneEmbeddedRoutes(routes map[string]embeddedRoute) map[string]embeddedRoute {
	copyRoutes := make(map[string]embeddedRoute, len(routes))
	for tag, route := range routes {
		copyRoutes[tag] = route
	}
	return copyRoutes
}

func decodeSingBoxOptions[T any](ctx context.Context, raw map[string]any) (T, error) {
	var out T
	blob, err := json.Marshal(raw)
	if err != nil {
		return out, err
	}
	if err := SBJSON.UnmarshalContext(ctx, blob, &out); err != nil {
		return out, err
	}
	return out, nil
}
