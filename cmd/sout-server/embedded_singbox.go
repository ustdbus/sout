package main

// Build with: -tags "with_gvisor with_quic with_utls with_wireguard with_cloudflared netgo osusergo".
// OpenVPN system:false requires gVisor; QUIC is supported for extensions.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
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

// embeddedEngine 管理唯一的 sing-box 用户态实例，接管所有入站与出口。
type embeddedEngine struct {
	mu sync.Mutex

	ctx context.Context
	box *sbox.Box

	routes           map[string]embeddedRoute
	userRoutes       map[string]embeddedRoute
	inboundConfigs   map[string]map[string]any
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

	if err := engine.rebuildBoxLocked(); err != nil {
		return nil, fmt.Errorf("创建内嵌 sing-box 初始实例失败: %w", err)
	}

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


// AddInbound 在内存态下发与热加载单个入站
func (e *embeddedEngine) AddInbound(rawConfig map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	tag, _ := rawConfig["tag"].(string)
	typ, _ := rawConfig["type"].(string)
	if tag == "" || typ == "" {
		return fmt.Errorf("入站缺少 tag 或 type")
	}

	e.inboundConfigs[tag] = rawConfig
	return e.rebuildBoxLocked()
}

// SyncInbounds 批量原子热同步所有配置的入站
func (e *embeddedEngine) SyncInbounds(inbounds []map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	nextConfigs := make(map[string]map[string]any, len(inbounds))
	for _, raw := range inbounds {
		tag, _ := raw["tag"].(string)
		typ, _ := raw["type"].(string)
		if tag == "" || typ == "" {
			continue
		}
		nextConfigs[tag] = raw
	}
	e.inboundConfigs = nextConfigs
	return e.rebuildBoxLocked()
}

// RemoveInbound 在内存态注销指定的入站
func (e *embeddedEngine) RemoveInbound(tag string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.inboundConfigs, tag)
	delete(e.routes, tag)
	return e.rebuildBoxLocked()
}

// HasInbound 检查入站是否处于运行状态
func (e *embeddedEngine) HasInbound(tag string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.box == nil {
		return false
	}
	_, found := e.inboundConfigs[tag]
	return found
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
	_ = e.rebuildBoxLocked()
}

func (e *embeddedEngine) addTunnel(tunnel *Tunnel) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.activeTunnels[tunnel.Slot] = tunnel
	return e.rebuildBoxLocked()
}

func (e *embeddedEngine) removeTunnel(tunnel *Tunnel) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.activeTunnels, tunnel.Slot)
	socksTag := fmt.Sprintf("soutsocks%d", tunnel.Slot)
	delete(e.routes, socksTag)
	_ = e.rebuildBoxLocked()
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

func (e *embeddedEngine) rebuildBoxLocked() error {
	var inbounds []option.Inbound
	for tag, raw := range e.inboundConfigs {
		opt, err := decodeSingBoxOptions[option.Inbound](e.ctx, raw)
		if err != nil {
			return fmt.Errorf("解析入站 %s 配置失败: %w", tag, err)
		}
		inbounds = append(inbounds, opt)
	}

	var endpoints []option.Endpoint
	nextRoutes := cloneEmbeddedRoutes(e.routes)

	for slot, tunnel := range e.activeTunnels {
		var endpointTag string
		var endpointConfig map[string]any
		isWG := tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard"
		if isWG {
			endpointTag = fmt.Sprintf("soutwireguard%d", slot)
			cfg, err := wireguardEndpoint(tunnel.Node.Config, endpointTag)
			if err != nil {
				return fmt.Errorf("转换 WireGuard 配置失败: %w", err)
			}
			endpointConfig = cfg
		} else {
			endpointTag = fmt.Sprintf("soutopenvpn%d", slot)
			cfg, err := openVPNEndpoint(tunnel.Node.Config, endpointTag)
			if err != nil {
				return fmt.Errorf("转换 VPN Gate 配置失败: %w", err)
			}
			endpointConfig = cfg
		}

		if endpointTag != e.chainUpstreamTag && !tunnel.IsChainUpstream && e.chainUpstreamTag != "" {
			endpointConfig["detour"] = e.chainUpstreamTag
		}

		endpointOpt, err := decodeSingBoxOptions[option.Endpoint](e.ctx, endpointConfig)
		if err != nil {
			return fmt.Errorf("解析 endpoint %s 失败: %w", endpointTag, err)
		}
		endpoints = append(endpoints, endpointOpt)

		// 为 s-ui 模式及本地端口保留 SOCKS5 监听
		if tunnel.Port > 0 {
			socksTag := fmt.Sprintf("soutsocks%d", slot)
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
				return fmt.Errorf("解析 SOCKS5 入站 %s 失败: %w", socksTag, err)
			}
			inbounds = append(inbounds, socksOpt)
			nextRoutes[socksTag] = embeddedRoute{endpoint: endpointTag}
		}
	}

	e.routes = nextRoutes

	outbounds := []option.Outbound{{
		Type:    soutDynamicOutboundType,
		Tag:     soutDynamicOutboundType,
		Options: &soutDynamicOutboundOptions{},
	}}

	newBox, err := sbox.New(sbox.Options{
		Context: e.ctx,
		Options: option.Options{
			Log:       &option.LogOptions{Level: "warn", Timestamp: true},
			Inbounds:  inbounds,
			Endpoints: endpoints,
			Outbounds: outbounds,
			Route:     &option.RouteOptions{Final: soutDynamicOutboundType},
		},
	})
	if err != nil {
		return fmt.Errorf("创建内嵌 sing-box 失败: %w", err)
	}

	if e.box != nil {
		oldBox := e.box
		closeDone := make(chan struct{})
		go func() {
			_ = oldBox.Close()
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(1 * time.Second):
		}
	}

	if err := newBox.Start(); err != nil {
		return fmt.Errorf("启动内嵌 sing-box 失败: %w", err)
	}

	e.box = newBox
	return nil
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
