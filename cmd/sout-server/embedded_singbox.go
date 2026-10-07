package main

// Build with: -tags "with_gvisor with_quic netgo osusergo".
// OpenVPN system:false requires gVisor; QUIC is supported for extensions.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

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
	"github.com/sagernet/sing-box/protocol/openvpn"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/wireguard"
	SBJSON "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

const soutDynamicOutboundType = "sout-dynamic"

// embeddedEngine 管理唯一的 sing-box 用户态实例。
// 每个 VPN Gate 出口以 userspace OpenVPN endpoint + 本地认证 SOCKS5 形式动态注册。
type embeddedEngine struct {
	mu sync.Mutex

	ctx context.Context
	box *sbox.Box

	routes           map[string]embeddedRoute
	tunnels          map[int]embeddedTunnelState
	activeTunnels    map[int]*Tunnel
	chainUpstreamTag string
	listenIP         string
}

type embeddedRoute struct {
	endpoint string
	direct   bool
	block    bool
}

type embeddedTunnelState struct {
	endpointTag string
	socksTag    string
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
		routes:        make(map[string]embeddedRoute),
		tunnels:       make(map[int]embeddedTunnelState),
		activeTunnels: make(map[int]*Tunnel),
		listenIP:      listenIP,
	}

	inboundRegistry := sbinbound.NewRegistry()
	outboundRegistry := sboutbound.NewRegistry()
	endpointRegistry := sbendpoint.NewRegistry()

	socks.RegisterInbound(inboundRegistry)
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

	box, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: option.Options{
			Log: &option.LogOptions{Level: "fatal", Timestamp: true},
			Outbounds: []option.Outbound{{
				Type:    soutDynamicOutboundType,
				Tag:     soutDynamicOutboundType,
				Options: &soutDynamicOutboundOptions{},
			}},
			Route: &option.RouteOptions{Final: soutDynamicOutboundType},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("创建内嵌 sing-box 失败: %w", err)
	}
	if err := box.Start(); err != nil {
		return nil, fmt.Errorf("启动内嵌 sing-box 失败: %w", err)
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
	route, found := o.engine.routes[metadata.Inbound]
	box := o.engine.box
	o.engine.mu.Unlock()
	if !found {
		return nil, destination, fmt.Errorf("入站 %s 没有可用出口", metadata.Inbound)
	}
	if route.block {
		return nil, destination, fmt.Errorf("入站 %s 绑定的出口当前不可用", metadata.Inbound)
	}
	if route.direct {
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
	return box.Close()
}

func (e *embeddedEngine) hasTunnel(slot int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, found := e.tunnels[slot]
	return found && e.box != nil
}

func (e *embeddedEngine) portAvailable(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort(e.listenIP, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// wireguardEndpoint 将 WireGuard 配置转换为 sing-box 1.14 用户态 endpoint 配置格式
func wireguardEndpoint(configJSON, tag string) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return nil, fmt.Errorf("解析 WireGuard 配置 JSON 失败: %w", err)
	}

	if _, hasPeers := raw["peers"]; hasPeers {
		raw["type"] = "wireguard"
		raw["tag"] = tag
		raw["system"] = false
		return raw, nil
	}

	server, _ := raw["server"].(string)
	if server == "" {
		server = "engage.cloudflareclient.com"
	}
	var serverPort uint16 = 2408
	if p, ok := raw["server_port"].(float64); ok && p > 0 {
		serverPort = uint16(p)
	} else if p, ok := raw["server_port"].(int); ok && p > 0 {
		serverPort = uint16(p)
	} else if pStr, ok := raw["server_port"].(string); ok {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			serverPort = uint16(p)
		}
	}

	privKey, _ := raw["private_key"].(string)
	peerPub, _ := raw["peer_public_key"].(string)
	if peerPub == "" {
		peerPub, _ = raw["public_key"].(string)
	}
	if peerPub == "" {
		peerPub = "bmXOC+F1FxEMF9dyiK2H5/1SUtzHZsVoW++jnWgmtEs="
	}

	var addresses []string
	if addrs, ok := raw["local_address"].([]any); ok {
		for _, a := range addrs {
			if aStr, ok := a.(string); ok && aStr != "" {
				addresses = append(addresses, aStr)
			}
		}
	} else if addrs, ok := raw["address"].([]any); ok {
		for _, a := range addrs {
			if aStr, ok := a.(string); ok && aStr != "" {
				addresses = append(addresses, aStr)
			}
		}
	} else if aStr, ok := raw["address"].(string); ok && aStr != "" {
		for _, part := range strings.Split(aStr, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				addresses = append(addresses, part)
			}
		}
	}

	if len(addresses) == 0 {
		addresses = []string{"172.16.0.2/32"}
	}

	for i, a := range addresses {
		if !strings.Contains(a, "/") {
			if strings.Contains(a, ":") {
				addresses[i] = a + "/128"
			} else {
				addresses[i] = a + "/32"
			}
		}
	}

	var reserved []int
	if res, ok := raw["reserved"].([]any); ok {
		for _, item := range res {
			if n, ok := item.(float64); ok {
				reserved = append(reserved, int(n))
			} else if n, ok := item.(int); ok {
				reserved = append(reserved, n)
			}
		}
	}

	mtu := 1280
	if m, ok := raw["mtu"].(float64); ok && m > 0 {
		mtu = int(m)
	}

	peerAddress := server
	if net.ParseIP(server) == nil {
		if server == "engage.cloudflareclient.com" {
			if systemSupportsIPv6() {
				// 双栈/纯 IPv6 环境下优先使用官方 IPv6 端点，避免 NAT 母机 IPv4 UDP 丢包阻断
				peerAddress = "2606:4700:d0::a29f:c001"
			} else {
				peerAddress = "162.159.192.1"
			}
		} else {
			targetProto := "ip4"
			if systemSupportsIPv6() && hostPublicIP() == "" {
				targetProto = "ip6"
			}
			if addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), targetProto, server); err == nil && len(addrs) > 0 {
				peerAddress = addrs[0].String()
			} else if addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", server); err == nil && len(addrs) > 0 {
				peerAddress = addrs[0].String()
			}
		}
	} else if systemSupportsIPv6() {
		// 若已指定为 Cloudflare IPv4 官方端点，但在支持 IPv6 的母机上，自适应切换为官方 IPv6 端点，避免 NAT 母机 UDP 黑洞导致的卡死
		if server == "162.159.192.1" || server == "162.159.193.1" || server == "162.159.195.1" {
			peerAddress = "2606:4700:d0::a29f:c001"
		}
	}

	peer := map[string]any{
		"address":                       peerAddress,
		"port":                          serverPort,
		"public_key":                    peerPub,
		"allowed_ips":                   []string{"0.0.0.0/0", "::/0"},
		"persistent_keepalive_interval": 25,
	}
	if len(reserved) > 0 {
		peer["reserved"] = reserved
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

	for slot, tunnel := range e.activeTunnels {
		state, found := e.tunnels[slot]
		if !found {
			continue
		}
		if state.endpointTag == tag || tunnel.IsChainUpstream {
			continue
		}
		_ = e.addTunnelLocked(tunnel)
	}
}

func (e *embeddedEngine) addTunnel(tunnel *Tunnel) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.addTunnelLocked(tunnel)
}

func (e *embeddedEngine) addTunnelLocked(tunnel *Tunnel) error {
	if e.box == nil {
		return fmt.Errorf("内嵌 sing-box 已关闭")
	}

	var endpointTag string
	var endpointConfig map[string]any
	var endpointLoggerName string
	socksTag := fmt.Sprintf("soutsocks%d", tunnel.Slot)

	if previous, exists := e.tunnels[tunnel.Slot]; exists {
		nextRoutes := cloneEmbeddedRoutes(e.routes)
		delete(nextRoutes, previous.socksTag)
		e.routes = nextRoutes
		_ = e.box.Inbound().Remove(previous.socksTag)
		_ = e.box.Endpoint().Remove(previous.endpointTag)
		delete(e.tunnels, tunnel.Slot)
	}

	isWG := tunnel.CustomProto == "wireguard" || tunnel.Node.Protocol == "wireguard"
	if isWG {
		endpointTag = fmt.Sprintf("soutwireguard%d", tunnel.Slot)
		endpointLoggerName = "wireguard"
		cfg, err := wireguardEndpoint(tunnel.Node.Config, endpointTag)
		if err != nil {
			return fmt.Errorf("转换 WireGuard 配置失败: %w", err)
		}
		endpointConfig = cfg
	} else {
		endpointTag = fmt.Sprintf("soutopenvpn%d", tunnel.Slot)
		endpointLoggerName = "openvpn"
		cfg, err := openVPNEndpoint(tunnel.Node.Config, endpointTag)
		if err != nil {
			return fmt.Errorf("转换 VPN Gate 配置失败: %w", err)
		}
		endpointConfig = cfg
	}

	// 链式出站：若不是前置链自身且存在活跃的 chainUpstreamTag，自动注入 detour
	if endpointTag != e.chainUpstreamTag && !tunnel.IsChainUpstream && e.chainUpstreamTag != "" {
		endpointConfig["detour"] = e.chainUpstreamTag
	}

	endpointOptions, err := decodeSingBoxOptions[option.Endpoint](e.ctx, endpointConfig)
	if err != nil {
		return fmt.Errorf("解析 %s endpoint 配置失败: %w", endpointLoggerName, err)
	}
	if err := e.box.Endpoint().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger(endpointLoggerName), endpointOptions.Tag, endpointOptions.Type, endpointOptions.Options); err != nil {
		return fmt.Errorf("启动 %s endpoint 失败: %w", endpointLoggerName, err)
	}

	socksConfig := map[string]any{
		"type": "socks", "tag": socksTag,
		"listen": e.listenIP, "listen_port": tunnel.Port,
	}
	cred := tunnel.Cred
	if cred.User != "" && cred.Pass != "" {
		socksConfig["users"] = []any{map[string]any{"username": cred.User, "password": cred.Pass}}
	}
	socksOptions, err := decodeSingBoxOptions[option.Inbound](e.ctx, socksConfig)
	if err != nil {
		_ = e.box.Endpoint().Remove(endpointTag)
		return fmt.Errorf("解析本地 SOCKS5 配置失败: %w", err)
	}

	nextRoutes := cloneEmbeddedRoutes(e.routes)
	nextRoutes[socksTag] = embeddedRoute{endpoint: endpointTag}
	e.routes = nextRoutes

	if err := e.box.Inbound().Create(e.ctx, e.box.Router(), e.box.LogFactory().NewLogger("socks"), socksOptions.Tag, socksOptions.Type, socksOptions.Options); err != nil {
		nextRoutes = cloneEmbeddedRoutes(e.routes)
		delete(nextRoutes, socksTag)
		e.routes = nextRoutes
		_ = e.box.Endpoint().Remove(endpointTag)
		return fmt.Errorf("启动本地 SOCKS5 失败: %w", err)
	}

	e.tunnels[tunnel.Slot] = embeddedTunnelState{endpointTag: endpointTag, socksTag: socksTag}
	e.activeTunnels[tunnel.Slot] = tunnel
	return nil
}

func (e *embeddedEngine) removeTunnel(tunnel *Tunnel) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.activeTunnels, tunnel.Slot)
	state, found := e.tunnels[tunnel.Slot]
	if !found || e.box == nil {
		return
	}
	nextRoutes := cloneEmbeddedRoutes(e.routes)
	delete(nextRoutes, state.socksTag)
	e.routes = nextRoutes
	_ = e.box.Inbound().Remove(state.socksTag)
	_ = e.box.Endpoint().Remove(state.endpointTag)
	delete(e.tunnels, tunnel.Slot)
}

func (e *embeddedEngine) dialTunnel(ctx context.Context, tunnel *Tunnel, network, address string) (net.Conn, error) {
	e.mu.Lock()
	state, found := e.tunnels[tunnel.Slot]
	box := e.box
	e.mu.Unlock()
	if !found || box == nil {
		return nil, fmt.Errorf("出口 endpoint 未运行")
	}
	endpoint, found := box.Endpoint().Get(state.endpointTag)
	if !found {
		return nil, fmt.Errorf("出口 endpoint 未运行")
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
