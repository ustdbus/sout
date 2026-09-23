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

func TestParseProxyURLEdgeCases(t *testing.T) {
	// 1. 空字符串
	if _, _, _, _, _, _, err := ParseProxyURL(""); err == nil {
		t.Errorf("expected error on empty string")
	}

	// 2. 特殊字符与 URL 编码
	proto, h, p, u, pwd, remark, err := ParseProxyURL("socks5://user%40test.com:pass%23123@1.2.3.4:1080#%E6%B5%8B%E8%AF%95")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "user@test.com" || pwd != "pass#123" || remark != "测试" {
		t.Errorf("unexpected decoded user/pass/remark: user=%s pass=%s remark=%s", u, pwd, remark)
	}
	if proto != "socks5" || h != "1.2.3.4" || p != 1080 {
		t.Errorf("unexpected proto/host/port: %s %s %d", proto, h, p)
	}

	// 3. 默认端口推导 (https=443, http=80, socks5=1080)
	p1, _, port1, _, _, _, _ := ParseProxyURL("https://example.com#test")
	if p1 != "https" || port1 != 443 {
		t.Errorf("expected https:443, got %s:%d", p1, port1)
	}
	p2, _, port2, _, _, _, _ := ParseProxyURL("http://example.com#test")
	if p2 != "http" || port2 != 80 {
		t.Errorf("expected http:80, got %s:%d", p2, port2)
	}
	p3, _, port3, _, _, _, _ := ParseProxyURL("socks5://example.com#test")
	if p3 != "socks5" || port3 != 1080 {
		t.Errorf("expected socks5:1080, got %s:%d", p3, port3)
	}

	// 4. 无效格式
	if _, _, _, _, _, _, err := ParseProxyURL("invalid-format-string"); err == nil {
		t.Errorf("expected error on invalid string format")
	}
}

func TestParseSubscriptionContentEdgeCases(t *testing.T) {
	// 1. 空内容
	if _, err := ParseSubscriptionContent(""); err == nil {
		t.Errorf("expected error on empty content")
	}

	// 2. 只有注释和空行
	if _, err := ParseSubscriptionContent("# comment\n\n# another comment\n"); err == nil {
		t.Errorf("expected error when no valid nodes found")
	}

	// 3. Base64 编码的链接列表
	rawLinks := "http://u:p@1.1.1.1:8080#WS-日本1\nsocks5://2.2.2.2:1080#测试节点\n"
	b64 := "aHR0cDovL3U6cEAxLjEuMS4xOjgwODAjV1Mt5pel5pysMQpzb2NrczU6Ly8yLjIuMi4yOjEwODAj5rWL6K+V6IqC54K5Cg=="
	b64Nodes, err := ParseSubscriptionContent(b64)
	if err != nil {
		t.Fatalf("Base64 subscription parse error: %v", err)
	}
	if len(b64Nodes) != 2 {
		t.Fatalf("expected 2 nodes from Base64, got %d", len(b64Nodes))
	}
	if b64Nodes[0].Country != "日本" || b64Nodes[0].CountryCode != "JP" {
		t.Errorf("expected JP/日本, got %s/%s", b64Nodes[0].CountryCode, b64Nodes[0].Country)
	}

	// 4. 包含部分损坏行但有有效行的情况
	mixedSample := "# Header comment\ngarbage line here\nhttp://u:p@1.1.1.1:443#WS-英国1\nanother bad line\n"
	mixedNodes, err := ParseSubscriptionContent(mixedSample)
	if err != nil {
		t.Fatalf("unexpected error parsing mixed sample: %v", err)
	}
	if len(mixedNodes) != 1 || mixedNodes[0].Country != "英国" {
		t.Errorf("expected 1 valid UK node, got %v", mixedNodes)
	}
}

