package main

import (
	"context"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
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

	opt, err := decodeSingBoxOptions[option.Options](context.Background(), boxConfig)
	if err != nil {
		t.Fatalf("sing-box options decode failed: %v", err)
	}

	if opt.Route == nil {
		t.Fatalf("expected Route config to be parsed")
	}
	if len(opt.Route.Rules) != 3 {
		t.Fatalf("expected 3 route rules, got %d", len(opt.Route.Rules))
	}
}
