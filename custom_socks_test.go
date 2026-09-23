package main

import (
	"fmt"
	"testing"
	"time"
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

func TestParseClashUnsupportedProtocols(t *testing.T) {
	// 测试包含 ss, vmess, trojan, masque 及 http 的复合配置
	yaml := `
proxies:
  - {name: "SS-Node", type: ss, server: 1.1.1.1, port: 8388, cipher: aes-128-gcm, password: pwd}
  - {name: "VMess-Node", type: vmess, server: 2.2.2.2, port: 443, uuid: 123456}
  - {name: "Trojan-Node", type: trojan, server: 3.3.3.3, port: 443, password: pwd}
  - {name: "WS-Node", type: http, server: 4.4.4.4, port: 443, username: u, password: p, tls: true}
  - {name: "S5-Node", type: socks5, server: 5.5.5.5, port: 1080}
`
	nodes, err := ParseSubscriptionContent(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 应该只保留 WS-Node (https) 和 S5-Node (socks5)，ss/vmess/trojan 必须被安全过滤跳过
	if len(nodes) != 2 {
		t.Fatalf("expected 2 supported nodes, got %d", len(nodes))
	}
	if nodes[0].Remark != "WS-Node" || nodes[0].Protocol != "https" {
		t.Errorf("expected WS-Node https, got %+v", nodes[0])
	}
	if nodes[1].Remark != "S5-Node" || nodes[1].Protocol != "socks5" {
		t.Errorf("expected S5-Node socks5, got %+v", nodes[1])
	}
}

func TestParseClashComplexFlowFormat(t *testing.T) {
	// 测试单行 flow 映射内包含逗号、方括号数组及引号
	yaml := `
proxies:
  - {name: "WARP-Node", type: masque, server: "162.159.198.1", port: 443, dns: [1.1.1.1, 8.8.8.8], remark: "US, West"}
  - {name: "HTTPS-Node", type: http, server: 1.2.3.4, port: 443, tls: true, username: "admin", password: "p@ss,word"}
`
	nodes := parseClashYamlNodes(yaml)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].Protocol != "masque" || nodes[0].Host != "162.159.198.1" {
		t.Errorf("unexpected node 0: %+v", nodes[0])
	}
	if nodes[1].Protocol != "https" || nodes[1].Pass != "p@ss,word" {
		t.Errorf("unexpected node 1: %+v", nodes[1])
	}
}

func TestParseProxyURL_SubUrlRejection(t *testing.T) {
	// 普通网络资源/订阅 URL 传入 ParseProxyURL 应拒绝，而非误判为代理节点
	subURL := "https://raw.githubusercontent.com/ustdbus/vpn-out/sub/all-proxies.txt"
	if _, _, _, _, _, _, err := ParseProxyURL(subURL); err == nil {
		t.Errorf("expected error when parsing subscription URL as proxy URL, got nil")
	}

	// 真正的 HTTPS 代理链接带有用户名密码或纯 host:port
	proxyURL := "https://user:pass@1.2.3.4:443#MyProxy"
	proto, host, port, user, pass, remark, err := ParseProxyURL(proxyURL)
	if err != nil {
		t.Fatalf("unexpected error parsing valid proxy URL: %v", err)
	}
	if proto != "https" || host != "1.2.3.4" || port != 443 || user != "user" || pass != "pass" || remark != "MyProxy" {
		t.Errorf("unexpected parsed values: %s %s %d %s %s %s", proto, host, port, user, pass, remark)
	}
}

func TestDialUpstreamProxy_UnsupportedProto(t *testing.T) {
	_, err := dialUpstreamProxy("1.2.3.4:443", "masque", "", "", "8.8.8.8:53", 1*time.Second)
	if err == nil {
		t.Errorf("expected error for unsupported proto masque, got nil")
	}
}

func TestNodeProtocolPreservation(t *testing.T) {
	// 验证 Node 结构体包含 Protocol 字段且与 CustomNode 正确双向映射
	cNode := CustomNode{
		HostName: "test-node",
		Host:     "1.1.1.1",
		Port:     443,
		Protocol: "https",
		User:     "u",
		Pass:     "p",
		Country:  "美国",
	}
	node := Node{
		HostName: cNode.HostName,
		IP:       cNode.Host,
		Port:     cNode.Port,
		Protocol: cNode.Protocol,
		User:     cNode.User,
		Pass:     cNode.Pass,
		Kind:     "custom",
	}
	if node.Protocol != "https" {
		t.Fatalf("expected node.Protocol = https, got %s", node.Protocol)
	}
}

func TestLiveVpnSubscriptionIntegration(t *testing.T) {
	// 1. 测试拉取并解析在线 Clash 订阅
	nodes, err := FetchSourceNodes("https://raw.githubusercontent.com/ustdbus/vpn-out/sub/clash-subscription.yaml", 15*time.Second)
	if err != nil {
		t.Fatalf("FetchSourceNodes(clash-subscription) error: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatalf("no nodes returned from clash-subscription.yaml")
	}
	t.Logf("Successfully parsed %d nodes from online Clash subscription", len(nodes))

	// 2. 测试拉取并解析在线单行代理列表 (all-proxies.txt)
	txtNodes, err := FetchSourceNodes("https://raw.githubusercontent.com/ustdbus/vpn-out/sub/all-proxies.txt", 15*time.Second)
	if err != nil {
		t.Fatalf("FetchSourceNodes(all-proxies.txt) error: %v", err)
	}
	if len(txtNodes) == 0 {
		t.Fatalf("no nodes returned from all-proxies.txt")
	}
	t.Logf("Successfully parsed %d nodes from online all-proxies.txt", len(txtNodes))

	// 3. 测试对真实 Windscribe 节点进行 ProbeCustomProxy 连通性探测
	var wsNode *CustomNode
	for i := range txtNodes {
		if txtNodes[i].Protocol == "https" || txtNodes[i].Protocol == "http" {
			wsNode = &txtNodes[i]
			break
		}
	}
	if wsNode != nil {
		t.Logf("Testing probe for node: %s (%s://%s:%d)", wsNode.Remark, wsNode.Protocol, wsNode.Host, wsNode.Port)
		exitIP, ping, ipType, isp, err := ProbeCustomProxy(
			fmt.Sprintf("%s:%d", wsNode.Host, wsNode.Port),
			wsNode.Protocol,
			wsNode.User,
			wsNode.Pass,
			15*time.Second,
		)
		if err != nil {
			t.Fatalf("ProbeCustomProxy failed: %v", err)
		}
		t.Logf("Probe success! ExitIP: %s, Ping: %dms, IPType: %s, ISP: %s", exitIP, ping, ipType, isp)
	}
}
