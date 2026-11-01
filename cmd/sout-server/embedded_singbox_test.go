package main

import (
	"context"
	"net/netip"
	"testing"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func TestIsLocalOrPrivateDestination(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		expected bool
	}{
		// 本地回环与局域网域名
		{"localhost domain", "localhost:80", true},
		{"localhost uppercase", "LOCALHOST:8080", true},
		{"localhost with dot", "localhost.:80", true},
		{"local mDNS domain", "printer.local:631", true},

		// 本地回环 IP
		{"loopback IPv4", "127.0.0.1:8899", true},
		{"loopback IPv4 other", "127.0.0.2:80", true},
		{"loopback IPv6", "[::1]:8899", true},

		// 私有/局域网 IPv4 (RFC 1918)
		{"private 10.x.x.x", "10.0.0.1:80", true},
		{"private 172.16.x.x", "172.16.1.1:443", true},
		{"private 192.168.x.x", "192.168.1.1:8899", true},

		// 链路本地与未指定地址
		{"link-local unicast IPv4", "169.254.1.1:80", true},
		{"link-local unicast IPv6", "[fe80::1]:80", true},
		{"unspecified IPv4", "0.0.0.0:80", true},
		{"unspecified IPv6", "[::]:80", true},

		// 公网域名（必须走代理/出口，不能判定为本地）
		{"public domain google", "google.com:443", false},
		{"public domain cloudflare", "cloudflare.com:443", false},
		{"public domain sub", "api.github.com:443", false},

		// 公网 IP（必须走代理/出口，不能判定为本地）
		{"public IPv4 cloudflare DNS", "1.1.1.1:53", false},
		{"public IPv4 google DNS", "8.8.8.8:53", false},
		{"public IPv4 web", "104.21.45.67:443", false},
		{"public IPv6 cloudflare DNS", "[2606:4700:4700::1111]:53", false},
		{"public IPv6 google DNS", "[2001:4860:4860::8888]:53", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := M.ParseSocksaddr(tc.addr)
			if !dst.IsValid() {
				t.Fatalf("ParseSocksaddr(%q) is invalid", tc.addr)
			}
			actual := isLocalOrPrivateDestination(dst)
			if actual != tc.expected {
				t.Errorf("isLocalOrPrivateDestination(%s) = %v, expected %v", tc.addr, actual, tc.expected)
			}
		})
	}

	// 额外验证由 netip.Addr 直接构造的 Socksaddr
	t.Run("from netip.Addr", func(t *testing.T) {
		localAddr := M.SocksaddrFrom(netip.MustParseAddr("127.0.0.1"), 8899)
		if !isLocalOrPrivateDestination(localAddr) {
			t.Errorf("expected 127.0.0.1 to be local")
		}

		privateAddr := M.SocksaddrFrom(netip.MustParseAddr("192.168.0.1"), 80)
		if !isLocalOrPrivateDestination(privateAddr) {
			t.Errorf("expected 192.168.0.1 to be local")
		}

		publicAddr := M.SocksaddrFrom(netip.MustParseAddr("1.1.1.1"), 53)
		if isLocalOrPrivateDestination(publicAddr) {
			t.Errorf("expected 1.1.1.1 to NOT be local")
		}
	})
}

func TestInboundBoxConfigRouteRules(t *testing.T) {
	// 验证 rebuildInboundBoxLocked 中的 route 与 outbounds 配置能被 sing-box 正常解析
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
		"dns": dnsConfig,
		"outbounds": []any{
			map[string]any{
				"type": "direct",
				"tag":  "direct",
			},
			map[string]any{
				"type": soutDynamicOutboundType,
				"tag":  soutDynamicOutboundType,
			},
		},
		"route": map[string]any{
			"rules": []any{
				map[string]any{
					"ip_cidr": []any{"255.255.255.255/32", "224.0.0.0/4", "ff00::/8"},
					"action":  "reject",
					"method":  "drop",
				},
				map[string]any{
					"domain":   []any{"localhost"},
					"outbound": "direct",
				},
				map[string]any{
					"ip_is_private": true,
					"outbound":      "direct",
				},
				map[string]any{
					"inbound":  []any{"cf-tunnel-in"},
					"outbound": "direct",
				},
			},
			"final": soutDynamicOutboundType,
		},
	}

	engine, err := newEmbeddedEngine("127.0.0.1")
	if err != nil {
		t.Fatalf("newEmbeddedEngine failed: %v", err)
	}

	opt, err := decodeSingBoxOptions[option.Options](engine.inboundCtx, boxConfig)
	if err != nil {
		t.Fatalf("sing-box options decode failed: %v", err)
	}

	if opt.Route == nil {
		t.Fatalf("expected Route config to be parsed")
	}
	if len(opt.Route.Rules) != 4 {
		t.Fatalf("expected 4 route rules, got %d", len(opt.Route.Rules))
	}
	if opt.Route.Rules[0].DefaultOptions.Action != constant.RuleActionTypeReject || opt.Route.Rules[0].DefaultOptions.RejectOptions.Method != constant.RuleActionRejectMethodDrop {
		t.Fatalf("expected first inboundBox rule to reject with drop, got action=%q method=%q", opt.Route.Rules[0].DefaultOptions.Action, opt.Route.Rules[0].DefaultOptions.RejectOptions.Method)
	}
}

func TestOutboundBoxRouteRulesDropEndpointIngressAndBroadcast(t *testing.T) {
	engine, err := newEmbeddedEngine("127.0.0.1")
	if err != nil {
		t.Fatalf("newEmbeddedEngine failed: %v", err)
	}

	rulesRaw := buildOutboundBoxRouteRules(
		[]any{"soutopenvpn1", "soutwireguard2"},
		[]any{
			map[string]any{"inbound": []any{"soutsocks1"}, "outbound": "direct"},
		},
	)
	boxConfig := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": true},
		"outbounds": []any{
			map[string]any{"type": "direct", "tag": "direct"},
		},
		"route": map[string]any{
			"rules": rulesRaw,
			"final": "direct",
		},
	}

	opt, err := decodeSingBoxOptions[option.Options](engine.outboundCtx, boxConfig)
	if err != nil {
		t.Fatalf("outboundBox options decode failed: %v", err)
	}
	if len(opt.Route.Rules) != 3 {
		t.Fatalf("expected 3 outboundBox route rules, got %d", len(opt.Route.Rules))
	}
	r0 := opt.Route.Rules[0].DefaultOptions
	if r0.Action != constant.RuleActionTypeReject || r0.RejectOptions.Method != constant.RuleActionRejectMethodDrop {
		t.Fatalf("expected rule[0] to drop endpoint ingress, got action=%q method=%q", r0.Action, r0.RejectOptions.Method)
	}
	if len(r0.Inbound) != 2 || r0.Inbound[0] != "soutopenvpn1" || r0.Inbound[1] != "soutwireguard2" {
		t.Fatalf("unexpected rule[0] inbound tags: %v", r0.Inbound)
	}
	r1 := opt.Route.Rules[1].DefaultOptions
	if r1.Action != constant.RuleActionTypeReject || r1.RejectOptions.Method != constant.RuleActionRejectMethodDrop || len(r1.IPCIDR) != 3 {
		t.Fatalf("expected rule[1] to drop broadcast/multicast CIDRs, got %+v", r1)
	}

	instance, err := sbox.New(sbox.Options{
		Context: engine.outboundCtx,
		Options: opt,
	})
	if err != nil {
		t.Fatalf("sbox.New for outboundBox failed: %v", err)
	}
	_ = instance.Close()
}

func TestDynamicOutboundBlocksEndpointIngressAndBroadcast(t *testing.T) {
	engine, err := newEmbeddedEngine("127.0.0.1")
	if err != nil {
		t.Fatalf("newEmbeddedEngine failed: %v", err)
	}
	dyn := &soutDynamicOutbound{engine: engine}

	withInbound := func(tag string) context.Context {
		return adapter.WithContext(context.Background(), &adapter.InboundContext{Inbound: tag})
	}

	// 1. 隧道 Endpoint 反向入站（哪怕目标是 127.0.0.1 或公网）必须一律拦截，绝不能返回 SystemDialer
	for _, epTag := range []string{"soutopenvpn1", "soutwireguard2"} {
		for _, dstStr := range []string{"255.255.255.255:68", "127.0.0.1:8081", "1.1.1.1:53"} {
			if d, _, err := dyn.endpointFor(withInbound(epTag), M.ParseSocksaddr(dstStr)); err == nil || d != nil {
				t.Fatalf("expected endpointFor(%s -> %s) to reject, got dialer=%v err=%v", epTag, dstStr, d, err)
			}
		}
	}

	// 2. 任何入站发往受限广播或多播地址均必须拦截
	for _, dstStr := range []string{"255.255.255.255:68", "224.0.0.251:5353", "[ff02::fb]:5353"} {
		if d, _, err := dyn.endpointFor(withInbound("vless-in"), M.ParseSocksaddr(dstStr)); err == nil || d != nil {
			t.Fatalf("expected broadcast/multicast destination %s to be rejected, got dialer=%v err=%v", dstStr, d, err)
		}
	}

	// 3. Cloudflare 反向隧道回源本地与正常直连入站不受影响
	if d, _, err := dyn.endpointFor(withInbound("cf-tunnel-in"), M.ParseSocksaddr("127.0.0.1:8081")); err != nil || d != N.SystemDialer {
		t.Fatalf("expected cf-tunnel-in -> 127.0.0.1:8081 to use SystemDialer, got dialer=%v err=%v", d, err)
	}
	if d, _, err := dyn.endpointFor(withInbound("vless-in"), M.ParseSocksaddr("1.1.1.1:443")); err != nil || d != N.SystemDialer {
		t.Fatalf("expected unrouted vless-in -> 1.1.1.1:443 to use SystemDialer, got dialer=%v err=%v", d, err)
	}
}

func TestSingBoxAndSUIRouteGuardsIdempotencyAndOrder(t *testing.T) {
	engine, err := newEmbeddedEngine("127.0.0.1")
	if err != nil {
		t.Fatalf("newEmbeddedEngine failed: %v", err)
	}

	// 模拟升级前旧配置：localhost 和 ip_is_private 排在最前面，缺少广播与 Endpoint 丢弃规则
	legacyRules := []any{
		map[string]any{"domain": []any{"localhost"}, "outbound": "direct"},
		map[string]any{"ip_is_private": true, "outbound": "direct"},
		map[string]any{"inbound": []any{"vless-in"}, "action": "reject", "method": "drop"},
		map[string]any{"inbound": []any{"vless-in"}, "domain": []any{"ads.example.com"}, "action": "reject", "method": "drop"},
		map[string]any{"auth_user": []any{"soutu1000_s1"}, "outbound": "soutvpn1"},
	}

	// 1. 验证纯 sout 单机模式路由写入逻辑 (ensureSingBoxRouteRules)
	sbRules := ensureSingBoxRouteRules(legacyRules, []string{"soutopenvpn1"}, []string{"vless-in"})
	if jsonRulesEqual(legacyRules, sbRules) {
		t.Fatalf("expected legacy rules to be detected as changed after ensureSingBoxRouteRules")
	}
	// 再次执行必须严格幂等
	sbRules2 := ensureSingBoxRouteRules(sbRules, []string{"soutopenvpn1"}, []string{"vless-in"})
	if !jsonRulesEqual(sbRules, sbRules2) {
		t.Fatalf("ensureSingBoxRouteRules must be strictly idempotent")
	}
	// 顺序校验：[0]=endpoint drop, [1]=broadcast drop, [2]=localhost, [3]=ip_is_private, [4]=用户入站封禁, [5]=用户广告封禁, [6]=用户分流
	if len(sbRules) != 7 {
		t.Fatalf("expected 7 sing-box rules, got %d", len(sbRules))
	}
	if sbRules[0].(map[string]any)["action"] != "reject" || toSUITagSlice(sbRules[0].(map[string]any)["inbound"])[0] != "soutopenvpn1" {
		t.Fatalf("expected rule[0] to be endpoint drop BEFORE ip_is_private, got %+v", sbRules[0])
	}
	if sbRules[1].(map[string]any)["action"] != "reject" {
		t.Fatalf("expected rule[1] to be broadcast/multicast drop, got %+v", sbRules[1])
	}
	if _, priv := sbRules[3].(map[string]any)["ip_is_private"]; !priv {
		t.Fatalf("expected rule[3] to be ip_is_private AFTER drop guards, got %+v", sbRules[3])
	}

	// 2. 验证 sout + s-ui 联动模式路由写入逻辑 (ensureSUIRouteRules)
	suiRules := ensureSUIRouteRules(legacyRules, []string{"wg-ep-1", "warp-ep"}, []string{"vless-in"})
	if len(suiRules) != 7 {
		t.Fatalf("expected 7 s-ui rules, got %d", len(suiRules))
	}
	// s-ui 规则必须显式带 action: "route" / "reject"
	if suiRules[2].(map[string]any)["action"] != "route" || suiRules[3].(map[string]any)["action"] != "route" {
		t.Fatalf("expected s-ui direct rules to carry explicit action=route, got %+v / %+v", suiRules[2], suiRules[3])
	}
	// 当 s-ui 中删除所有 endpoints 时，受管 endpoint drop 规则应自动移除且保留用户自定义 vless-in reject 规则
	suiNoEp := ensureSUIRouteRules(suiRules, nil, []string{"vless-in"})
	if len(suiNoEp) != 6 {
		t.Fatalf("expected 6 s-ui rules after removing endpoints, got %d", len(suiNoEp))
	}
	if !jsonRulesEqual(suiNoEp, ensureSUIRouteRules(suiNoEp, nil, []string{"vless-in"})) {
		t.Fatalf("ensureSUIRouteRules without endpoints must be strictly idempotent")
	}

	// 3. 将两套规则分别喂给真实 sing-box 解析器验证 100% 合法兼容
	for name, candidate := range map[string][]any{"singbox": sbRules, "sui": suiRules} {
		cfg := map[string]any{
			"outbounds": []any{
				map[string]any{"type": "direct", "tag": "direct"},
				map[string]any{"type": "direct", "tag": "soutvpn1"},
			},
			"route": map[string]any{
				"rules": candidate,
				"final": "direct",
			},
		}
		if _, err := decodeSingBoxOptions[option.Options](engine.outboundCtx, cfg); err != nil {
			t.Fatalf("%s generated route rules failed sing-box schema decode: %v", name, err)
		}
	}

	// 4. 验证 s-ui 持久化分流客户端路由自愈 (reconcileSUIBranchRoutes)
	bindings := []branchBinding{
		{TemplateID: 1, Slot: 1, Host: "warp-node"},
		{TemplateID: 1, Slot: 2, Host: "vpn-new-host"},
	}
	tunnels := []*Tunnel{
		{Slot: 1, Status: "up", Node: Node{HostName: "warp-node"}},
		{Slot: 2, Status: "up", Node: Node{HostName: "vpn-new-host"}},
	}
	existingUsers := map[string]bool{
		"soutu1_s1": true,
		"soutu1_s2": true,
	}
	baseRules := []any{
		map[string]any{"action": "sniff"},
		map[string]any{"action": "route", "auth_user": []string{"soutu1_s2"}, "outbound": "soutvpnoldhost"},
	}
	reconciled := reconcileSUIBranchRoutes(baseRules, bindings, tunnels, existingUsers)
	finalSUI := ensureSUIRouteRules(reconciled, nil, []string{"vless-in"})
	if !jsonRulesEqual(finalSUI, ensureSUIRouteRules(reconcileSUIBranchRoutes(finalSUI, bindings, tunnels, existingUsers), nil, []string{"vless-in"})) {
		t.Fatalf("reconcileSUIBranchRoutes + ensureSUIRouteRules must be strictly idempotent")
	}
	foundS1, foundS2 := false, false
	for _, item := range finalSUI {
		m := item.(map[string]any)
		users := toSUITagSlice(m["auth_user"])
		if len(users) == 1 && users[0] == "soutu1_s1" && m["outbound"] == "soutwarpnode" {
			foundS1 = true
		}
		if len(users) == 1 && users[0] == "soutu1_s2" && m["outbound"] == "soutvpnnewhost" {
			foundS2 = true
		}
	}
	if !foundS1 || !foundS2 {
		t.Fatalf("expected reconciled s-ui rules to heal soutu1_s1 and soutu1_s2 routes, got %+v", finalSUI)
	}
}

func TestValidateInboundTLSFiles(t *testing.T) {
	// 1. cf-tunnel-in 无条件放行
	cfIn := map[string]any{
		"type": "cloudflared",
		"tag":  "cf-tunnel-in",
	}
	if !validateInboundTLSFiles(cfIn, "cf-tunnel-in") {
		t.Fatalf("cf-tunnel-in should always pass validation")
	}

	// 2. Reality 模式无证书放行
	realityIn := map[string]any{
		"type": "vless",
		"tag":  "vless-reality",
		"tls": map[string]any{
			"enabled": true,
			"reality": map[string]any{
				"enabled": true,
			},
		},
	}
	if !validateInboundTLSFiles(realityIn, "vless-reality") {
		t.Fatalf("reality inbound should pass validation without cert files")
	}

	// 3. 证书文件不存在的 TUIC / Hy2 必须被拦截跳过
	tuicInvalid := map[string]any{
		"type": "tuic",
		"tag":  "tuic-ww4f",
		"tls": map[string]any{
			"enabled":          true,
			"certificate_path": "/path/to/non_existent_fullchain.pem",
			"key_path":         "/path/to/non_existent_privkey.pem",
		},
	}
	if validateInboundTLSFiles(tuicInvalid, "tuic-ww4f") {
		t.Fatalf("inbound with missing cert file must be rejected to protect tunnel")
	}
}
