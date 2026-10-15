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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

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
	"github.com/sagernet/sing-box/protocol/direct"
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

// embeddedEngine 管理双 sing-box 内核实例架构（Inbound Box 与 Outbound Box 物理生命周期彻底解耦）。
// inboundBox 承载所有公网入站业务与 Cloudflare QUIC 隧道，出口固定指向 sout-dynamic，永久不重启；
// outboundBox 专职承载 OpenVPN/WireGuard 出口与本地认证 SOCKS5，随节点状态增删切换独立重建。
type embeddedEngine struct {
	mu sync.Mutex

	inboundCtx  context.Context
	outboundCtx context.Context

	inboundBox  *sbox.Box
	outboundBox *sbox.Box

	routes           map[string]embeddedRoute
	userRoutes       map[string]embeddedRoute
	inboundConfigs   map[string]map[string]any
	inboundHash      [32]byte
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

	// 1. 初始化 inboundCtx：注册所有入站协议、sout-dynamic 动态出站以及本地 DNS
	inboundInbounds := sbinbound.NewRegistry()
	inboundOutbounds := sboutbound.NewRegistry()
	inboundEndpoints := sbendpoint.NewRegistry()
	inboundDNS := sbdns.NewTransportRegistry()

	socks.RegisterInbound(inboundInbounds)
	vless.RegisterInbound(inboundInbounds)
	vmess.RegisterInbound(inboundInbounds)
	trojan.RegisterInbound(inboundInbounds)
	hysteria2.RegisterInbound(inboundInbounds)
	tuic.RegisterInbound(inboundInbounds)
	registerCloudflaredInbound(inboundInbounds)

	sboutbound.Register[soutDynamicOutboundOptions](inboundOutbounds, soutDynamicOutboundType,
		func(_ context.Context, _ adapter.Router, _ log.ContextLogger, tag string, _ soutDynamicOutboundOptions) (adapter.Outbound, error) {
			return &soutDynamicOutbound{
				Adapter: sboutbound.NewAdapter(soutDynamicOutboundType, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
				engine:  engine,
			}, nil
		})
	direct.RegisterOutbound(inboundOutbounds)
	local.RegisterTransport(inboundDNS)

	engine.inboundCtx = sbox.Context(
		context.Background(),
		inboundInbounds,
		inboundOutbounds,
		inboundEndpoints,
		inboundDNS,
		sbservice.NewRegistry(),
		sbcertificate.NewRegistry(),
	)

	// 2. 初始化 outboundCtx：注册出口协议 (openvpn, wireguard, socks inbound, direct outbound, local dns)
	outboundInbounds := sbinbound.NewRegistry()
	outboundOutbounds := sboutbound.NewRegistry()
	outboundEndpoints := sbendpoint.NewRegistry()
	outboundDNS := sbdns.NewTransportRegistry()

	openvpn.RegisterEndpoint(outboundEndpoints)
	wireguard.RegisterEndpoint(outboundEndpoints)
	socks.RegisterInbound(outboundInbounds)
	direct.RegisterOutbound(outboundOutbounds)
	local.RegisterTransport(outboundDNS)

	engine.outboundCtx = sbox.Context(
		context.Background(),
		outboundInbounds,
		outboundOutbounds,
		outboundEndpoints,
		outboundDNS,
		sbservice.NewRegistry(),
		sbcertificate.NewRegistry(),
	)

	// 3. 启动空的初始 inboundBox 与 outboundBox
	engine.mu.Lock()
	defer engine.mu.Unlock()

	if err := engine.rebuildInboundBoxLocked(); err != nil {
		return nil, fmt.Errorf("创建初始 inboundBox 失败: %w", err)
	}
	if err := engine.rebuildOutboundBoxLocked(); err != nil {
		_ = safeCloseBox(engine.inboundBox)
		return nil, fmt.Errorf("创建初始 outboundBox 失败: %w", err)
	}


	return engine, nil
}

func (e *embeddedEngine) rebuildInboundBoxLocked() error {
	inboundsRaw := make([]any, 0, len(e.inboundConfigs))
	var tags []string
	for tag := range e.inboundConfigs {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		cfg := e.inboundConfigs[tag]
		typ, _ := cfg["type"].(string)
		if users, ok := cfg["users"].([]any); ok {
			for _, u := range users {
				if uMap, ok := u.(map[string]any); ok {
					if typ == "vless" || typ == "vmess" {
						delete(uMap, "password")
					} else if typ == "hysteria2" || typ == "hy2" || typ == "trojan" || typ == "shadowsocks" || typ == "ss" {
						delete(uMap, "uuid")
						delete(uMap, "flow")
					}
				}
			}
		}
		inboundsRaw = append(inboundsRaw, cfg)
	}

	dnsConfig := map[string]any{
		"strategy": "prefer_ipv6",
		"servers": []any{
			map[string]any{
				"tag":  "default-dns",
				"type": "local",
			},
		},
	}

	boxConfig := map[string]any{
		"log": map[string]any{
			"level":     "warn",
			"timestamp": true,
		},
		"dns":      dnsConfig,
		"inbounds": inboundsRaw,
		"outbounds": []any{
			map[string]any{
				"type": soutDynamicOutboundType,
				"tag":  soutDynamicOutboundType,
			},
		},
		"route": map[string]any{
			"final": soutDynamicOutboundType,
		},
	}

	opt, err := decodeSingBoxOptions[option.Options](e.inboundCtx, boxConfig)
	if err != nil {
		return fmt.Errorf("解析 inboundBox 配置失败: %w", err)
	}

	newBox, err := sbox.New(sbox.Options{
		Context: e.inboundCtx,
		Options: opt,
	})
	if err != nil {
		return fmt.Errorf("创建 inboundBox 失败: %w", err)
	}

	oldBox := e.inboundBox
	if oldBox != nil {
		_ = safeCloseBox(oldBox)
		e.inboundBox = nil
	}

	if err := newBox.Start(); err != nil {
		return fmt.Errorf("启动 inboundBox 失败: %w", err)
	}

	e.inboundBox = newBox
	return nil
}

func (e *embeddedEngine) rebuildOutboundBoxLocked() error {
	endpointsRaw := make([]any, 0, len(e.activeTunnels))
	inboundsRaw := make([]any, 0, len(e.activeTunnels))
	rulesRaw := make([]any, 0, len(e.activeTunnels))

	nextRoutes := cloneEmbeddedRoutes(e.routes)

	var slots []int
	for s := range e.activeTunnels {
		slots = append(slots, s)
	}
	sort.Ints(slots)

	activeEndpointTags := make(map[string]bool, len(slots))

	for _, slot := range slots {
		tunnel := e.activeTunnels[slot]
		isWG := tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard"
		endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
		if isWG {
			endpointTag = fmt.Sprintf("soutwireguard%d", slot)
		}
		activeEndpointTags[endpointTag] = true
		if tunnel.Node.HostName != "" {
			activeEndpointTags["sout"+sanitizeTag(tunnel.Node.HostName)] = true
		}

		if tunnel.Kind == "custom" && !isWG {
			// 自定义节点由独立 sing-box 进程监听本地端口，已登记 tags，不注入 outboundBox endpoints
			continue
		}
		socksTag := fmt.Sprintf("soutsocks%d", slot)

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

		endpointsRaw = append(endpointsRaw, endpointConfig)

		if tunnel.Port > 0 {
			socksConfig := map[string]any{
				"type":        "socks",
				"tag":         socksTag,
				"listen":      e.listenIP,
				"listen_port": tunnel.Port,
			}
			cred := tunnel.Cred
			if cred.User != "" && cred.Pass != "" {
				socksConfig["users"] = []any{
					map[string]any{
						"username": cred.User,
						"password": cred.Pass,
					},
				}
			}
			inboundsRaw = append(inboundsRaw, socksConfig)

			rulesRaw = append(rulesRaw, map[string]any{
				"inbound":  []any{socksTag},
				"outbound": endpointTag,
			})
			nextRoutes[socksTag] = embeddedRoute{endpoint: endpointTag}
		} else {
			delete(nextRoutes, socksTag)
		}
	}

	// 统一维护 routes 与 userRoutes 中的 block / unblock 状态
	for tag, r := range nextRoutes {
		if strings.HasPrefix(r.endpoint, "soutopenvpn") || strings.HasPrefix(r.endpoint, "soutwireguard") {
			if activeEndpointTags[r.endpoint] {
				nextRoutes[tag] = embeddedRoute{endpoint: r.endpoint, block: false}
			} else {
				nextRoutes[tag] = embeddedRoute{endpoint: r.endpoint, block: true}
			}
		}
	}
	// 清理已失效且未在 activeTunnels 中的 socksTag
	for tag := range nextRoutes {
		if strings.HasPrefix(tag, "soutsocks") {
			var slot int
			if _, err := fmt.Sscanf(tag, "soutsocks%d", &slot); err == nil {
				if _, active := e.activeTunnels[slot]; !active {
					delete(nextRoutes, tag)
				}
			}
		}
	}
	e.routes = nextRoutes

	if e.userRoutes != nil {
		nextUsers := cloneEmbeddedRoutes(e.userRoutes)
		for u, r := range nextUsers {
			if strings.HasPrefix(r.endpoint, "soutopenvpn") || strings.HasPrefix(r.endpoint, "soutwireguard") {
				if activeEndpointTags[r.endpoint] {
					nextUsers[u] = embeddedRoute{endpoint: r.endpoint, block: false}
				} else {
					nextUsers[u] = embeddedRoute{endpoint: r.endpoint, block: true}
				}
			}
		}
		e.userRoutes = nextUsers
	}

	dnsConfig := map[string]any{
		"strategy": "prefer_ipv6",
		"servers": []any{
			map[string]any{
				"tag":  "default-dns",
				"type": "local",
			},
		},
	}

	boxConfig := map[string]any{
		"log": map[string]any{
			"level":     "warn",
			"timestamp": true,
		},
		"dns":       dnsConfig,
		"endpoints": endpointsRaw,
		"inbounds":  inboundsRaw,
		"outbounds": []any{
			map[string]any{
				"type": "direct",
				"tag":  "direct",
			},
		},
		"route": map[string]any{
			"rules": rulesRaw,
			"final": "direct",
		},
	}

	opt, err := decodeSingBoxOptions[option.Options](e.outboundCtx, boxConfig)
	if err != nil {
		return fmt.Errorf("解析 outboundBox 配置失败: %w", err)
	}

	newBox, err := sbox.New(sbox.Options{
		Context: e.outboundCtx,
		Options: opt,
	})
	if err != nil {
		return fmt.Errorf("创建 outboundBox 失败: %w", err)
	}

	// 先安全关闭旧的 outboundBox 释放监听端口
	oldBox := e.outboundBox
	if oldBox != nil {
		_ = safeCloseBox(oldBox)
		e.outboundBox = nil
	}

	if err := newBox.Start(); err != nil {
		return fmt.Errorf("启动 outboundBox 失败: %w", err)
	}

	e.outboundBox = newBox
	return nil
}

func (e *embeddedEngine) addTunnel(tunnel *Tunnel) error {
	if tunnel == nil {
		return fmt.Errorf("tunnel 不能为空")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.activeTunnels[tunnel.Slot] = tunnel
	return e.rebuildOutboundBoxLocked()
}

func (e *embeddedEngine) removeTunnel(tunnel *Tunnel) {
	if tunnel == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.activeTunnels, tunnel.Slot)
	_ = e.rebuildOutboundBoxLocked()
}

func (e *embeddedEngine) setChainUpstream(tag string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.chainUpstreamTag == tag {
		return
	}
	e.chainUpstreamTag = tag
	_ = e.rebuildOutboundBoxLocked()
}

func (e *embeddedEngine) SyncInbounds(inbounds []map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	hash, tagMap, err := computeInboundsHash(inbounds)
	if err != nil {
		return fmt.Errorf("计算入站配置全局哈希失败: %w", err)
	}

	// 若哈希与 e.inboundHash 相同且 e.inboundBox != nil，直接返回 nil，连 inboundBox 都不重启！
	if hash == e.inboundHash && e.inboundBox != nil {
		return nil
	}

	e.inboundConfigs = tagMap
	e.inboundHash = hash
	return e.rebuildInboundBoxLocked()
}

func (e *embeddedEngine) AddInbound(rawConfig map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	tag, _ := rawConfig["tag"].(string)
	typ, _ := rawConfig["type"].(string)
	if tag == "" || typ == "" {
		return fmt.Errorf("入站缺少 tag 或 type")
	}

	e.inboundConfigs[tag] = rawConfig

	var list []map[string]any
	for _, cfg := range e.inboundConfigs {
		list = append(list, cfg)
	}
	hash, _, err := computeInboundsHash(list)
	if err != nil {
		return fmt.Errorf("计算入站配置全局哈希失败: %w", err)
	}
	e.inboundHash = hash

	return e.rebuildInboundBoxLocked()
}

func (e *embeddedEngine) RemoveInbound(tag string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.inboundConfigs[tag]; !exists {
		return nil
	}
	delete(e.inboundConfigs, tag)
	delete(e.routes, tag)

	var list []map[string]any
	for _, cfg := range e.inboundConfigs {
		list = append(list, cfg)
	}
	hash, _, err := computeInboundsHash(list)
	if err != nil {
		return fmt.Errorf("计算入站配置全局哈希失败: %w", err)
	}
	e.inboundHash = hash

	return e.rebuildInboundBoxLocked()
}

func computeInboundsHash(inbounds []map[string]any) ([32]byte, map[string]map[string]any, error) {
	tagMap := make(map[string]map[string]any, len(inbounds))
	var tags []string
	for _, raw := range inbounds {
		if raw == nil {
			continue
		}
		tag, _ := raw["tag"].(string)
		typ, _ := raw["type"].(string)
		if tag == "" || typ == "" {
			continue
		}
		tagMap[tag] = raw
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	hasher := sha256.New()
	for _, t := range tags {
		blob, err := json.Marshal(tagMap[t])
		if err != nil {
			return [32]byte{}, nil, err
		}
		hasher.Write([]byte(t))
		hasher.Write(blob)
	}
	var res [32]byte
	copy(res[:], hasher.Sum(nil))
	return res, tagMap, nil
}

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
	outboundBox := o.engine.outboundBox
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
	if outboundBox != nil {
		if endpoint, found := outboundBox.Endpoint().Get(route.endpoint); found {
			return endpoint, destination, nil
		}
	}

	// 兜底自愈 / 本地转发：如果内存 Endpoint 尚未就绪或属于独立监听的自定义节点，通过本地 SOCKS5 端口直接转发
	o.engine.mu.Lock()
	var fallbackTunnel *Tunnel
	slotFromUser := parseSlotFromClientName(metadata.User)
	if slotFromUser > 0 {
		if t, ok := o.engine.activeTunnels[slotFromUser]; ok && t != nil && t.Port > 0 {
			fallbackTunnel = t
		}
	}
	if fallbackTunnel == nil {
		for _, t := range o.engine.activeTunnels {
			if t != nil && t.Port > 0 {
				epTag := fmt.Sprintf("soutopenvpn%d", t.Slot)
				if t.CustomProto == "wireguard" || t.Node.Protocol == "wireguard" {
					epTag = fmt.Sprintf("soutwireguard%d", t.Slot)
				}
				customTag := "sout" + sanitizeTag(t.Node.HostName)
				if epTag == route.endpoint || customTag == route.endpoint || (t.Kind == "custom" && t.Node.HostName != "" && strings.Contains(route.endpoint, sanitizeTag(t.Node.HostName))) {
					fallbackTunnel = t
					break
				}
			}
		}
	}
	listenIP := o.engine.listenIP
	o.engine.mu.Unlock()

	if fallbackTunnel != nil {
		cred := fallbackTunnel.credential()
		return &socksFallbackDialer{
			serverAddr: fmt.Sprintf("%s:%d", listenIP, fallbackTunnel.Port),
			username:   cred.User,
			password:   cred.Pass,
		}, destination, nil
	}

	if metadata.User != "" {
		return nil, destination, fmt.Errorf("分流用户 %s (入站 %s) 指定出口 %s 不可用或未就绪", metadata.User, metadata.Inbound, route.endpoint)
	}
	return nil, destination, fmt.Errorf("入站 %s 指定出口 %s 不可用或未就绪", metadata.Inbound, route.endpoint)
}

type socksFallbackDialer struct {
	serverAddr string
	username   string
	password   string
}

func (d *socksFallbackDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	var auth *proxy.Auth
	if d.username != "" && d.password != "" {
		auth = &proxy.Auth{User: d.username, Password: d.password}
	}
	dialer, err := proxy.SOCKS5("tcp", d.serverAddr, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
		return ctxDialer.DialContext(ctx, network, destination.String())
	}
	return dialer.Dial(network, destination.String())
}

func (d *socksFallbackDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("SOCKS5 fallback 暂不支持 UDP 代理")
}

func isEmbeddedEndpointInbound(tag string) bool {
	return strings.HasPrefix(tag, "soutopenvpn") || strings.HasPrefix(tag, "sout-openvpn-") || strings.HasPrefix(tag, "fanoutopenvpn") || strings.HasPrefix(tag, "fanout-openvpn-") || strings.HasPrefix(tag, "soutwireguard") || strings.HasPrefix(tag, "sout-wireguard-")
}

func (e *embeddedEngine) dialTunnel(ctx context.Context, tunnel *Tunnel, network, address string) (net.Conn, error) {
	e.mu.Lock()
	outboundBox := e.outboundBox
	slot := tunnel.Slot
	endpointTag := fmt.Sprintf("soutopenvpn%d", slot)
	if tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard" {
		endpointTag = fmt.Sprintf("soutwireguard%d", slot)
	}
	e.mu.Unlock()

	if outboundBox == nil {
		return nil, fmt.Errorf("内嵌出口引擎未就绪")
	}
	endpoint, found := outboundBox.Endpoint().Get(endpointTag)
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

func (e *embeddedEngine) HasInbound(tag string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.inboundConfigs[tag]
	return ok
}

func (e *embeddedEngine) hasTunnel(slot int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.activeTunnels[slot]
	return ok && e.outboundBox != nil
}

func (e *embeddedEngine) portAvailable(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort(e.listenIP, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func (e *embeddedEngine) close() error {
	e.mu.Lock()
	inBox := e.inboundBox
	e.inboundBox = nil
	outBox := e.outboundBox
	e.outboundBox = nil
	e.mu.Unlock()

	globalEngineMu.Lock()
	if globalEngine == e {
		globalEngine = nil
	}
	globalEngineMu.Unlock()

	var wg sync.WaitGroup
	if inBox != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = safeCloseBox(inBox)
		}()
	}
	if outBox != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = safeCloseBox(outBox)
		}()
	}
	wg.Wait()
	return nil
}

func safeCloseBox(box *sbox.Box) error {
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

func wireguardEndpoint(configJSON, tag string) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return nil, fmt.Errorf("解析 WireGuard 配置 JSON 失败: %w", err)
	}

	server, _ := raw["server"].(string)

	var serverPort int
	switch p := raw["server_port"].(type) {
	case float64:
		serverPort = int(p)
	case int:
		serverPort = p
	case string:
		serverPort, _ = strconv.Atoi(p)
	}

	privKey, _ := raw["private_key"].(string)
	pubKey, _ := raw["peer_public_key"].(string)
	preSharedKey, _ := raw["pre_shared_key"].(string)
	reservedRaw, _ := raw["reserved"].([]any)
	localAddressRaw, _ := raw["local_address"].([]any)

	mtu := 1420
	switch m := raw["mtu"].(type) {
	case float64:
		if m > 0 {
			mtu = int(m)
		}
	case int:
		if m > 0 {
			mtu = m
		}
	}

	peer := map[string]any{
		"address":     server,
		"port":        serverPort,
		"public_key":  pubKey,
		"allowed_ips": []string{"0.0.0.0/0", "::/0"},
	}
	if preSharedKey != "" {
		peer["pre_shared_key"] = preSharedKey
	}
	if len(reservedRaw) > 0 {
		var reserved []uint8
		for _, v := range reservedRaw {
			switch num := v.(type) {
			case float64:
				reserved = append(reserved, uint8(num))
			case int:
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
		"mtu":         mtu,
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
