package main

import (
	"testing"
)

func TestParseProxyURL(t *testing.T) {
	cases := []struct {
		raw      string
		wantP    string
		wantH    string
		wantPort int
		wantUser string
		wantPass string
		wantRem  string
	}{
		{
			raw:      "socks5://alice:secret123@1.2.3.4:1080#测试S5",
			wantP:    "socks5",
			wantH:    "1.2.3.4",
			wantPort: 1080,
			wantUser: "alice",
			wantPass: "secret123",
			wantRem:  "测试S5",
		},
		{
			raw:      "http://user:pass@hk-01.windscribe.com:443#WS-香港1",
			wantP:    "http",
			wantH:    "hk-01.windscribe.com",
			wantPort: 443,
			wantUser: "user",
			wantPass: "pass",
			wantRem:  "WS-香港1",
		},
		{
			raw:      "https://operadev:tok123@77.111.246.41:443#Opera-亚洲1",
			wantP:    "https",
			wantH:    "77.111.246.41",
			wantPort: 443,
			wantUser: "operadev",
			wantPass: "tok123",
			wantRem:  "Opera-亚洲1",
		},
		{
			raw:      "192.168.1.100:1080:bob:pwd456",
			wantP:    "socks5",
			wantH:    "192.168.1.100",
			wantPort: 1080,
			wantUser: "bob",
			wantPass: "pwd456",
			wantRem:  "192.168.1.100",
		},
	}

	for _, tc := range cases {
		proto, h, p, u, pwd, remark, err := ParseProxyURL(tc.raw)
		if err != nil {
			t.Fatalf("ParseProxyURL(%q) unexpected error: %v", tc.raw, err)
		}
		if proto != tc.wantP || h != tc.wantH || p != tc.wantPort || u != tc.wantUser || pwd != tc.wantPass || remark != tc.wantRem {
			t.Errorf("ParseProxyURL(%q) = (%q, %q, %d, %q, %q, %q); want (%q, %q, %d, %q, %q, %q)",
				tc.raw, proto, h, p, u, pwd, remark, tc.wantP, tc.wantH, tc.wantPort, tc.wantUser, tc.wantPass, tc.wantRem)
		}
	}
}

func TestParseSubscriptionContent(t *testing.T) {
	// 测试 Clash YAML 内容
	yamlSample := `
proxies:
  - {name: "WS-香港1", type: http, server: 10.0.0.1, port: 443, username: "u1", password: "p1", tls: true}
  - name: "WS-美国1"
    type: http
    server: 10.0.0.2
    port: 443
    username: "u2"
    password: "p2"
  - name: "SocksNode"
    type: socks5
    server: 10.0.0.3
    port: 1080
`
	nodes, err := ParseSubscriptionContent(yamlSample)
	if err != nil {
		t.Fatalf("ParseSubscriptionContent(yaml) error: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(nodes))
	}
	if nodes[0].Protocol != "https" || nodes[0].Country != "香港" {
		t.Errorf("nodes[0] mismatch: %+v", nodes[0])
	}
	if nodes[1].Protocol != "http" || nodes[1].Country != "美国" {
		t.Errorf("nodes[1] mismatch: %+v", nodes[1])
	}
	if nodes[2].Protocol != "socks5" {
		t.Errorf("nodes[2] mismatch: %+v", nodes[2])
	}

	// 测试多行链接
	linksSample := "http://u:p@1.1.1.1:443#WS-香港\nhttps://op:token@2.2.2.2:443#Opera-亚洲1\nsocks5://3.3.3.3:1080#测试S5\n"
	linkNodes, err := ParseSubscriptionContent(linksSample)
	if err != nil {
		t.Fatalf("ParseSubscriptionContent(links) error: %v", err)
	}
	if len(linkNodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(linkNodes))
	}
	if linkNodes[0].Country != "香港" || linkNodes[0].Protocol != "http" {
		t.Errorf("linkNodes[0] mismatch: %+v", linkNodes[0])
	}
	if linkNodes[1].Country != "亚洲" || linkNodes[1].Protocol != "https" {
		t.Errorf("linkNodes[1] mismatch: %+v", linkNodes[1])
	}
}
