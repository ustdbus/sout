package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildLinksForUser_VlessArgo_TLSComplete(t *testing.T) {
	sb := &SingBox{}

	ibMap := map[string]any{
		"tag":    "vless-argo-test",
		"type":   "vless",
		"listen": "127.0.0.1",
		"transport": map[string]any{
			"type": "ws",
			"path": "/vlws12345678",
			"headers": map[string]any{
				"Host": "tunnel.example.com",
			},
			"early_data_header_name": "Sec-WebSocket-Protocol",
			"max_early_data":        2560,
		},
	}
	uMap := map[string]any{
		"name": "admin",
		"uuid": "11111111-2222-3333-4444-555555555555",
	}

	// 1. 无额外 addrs 时的测试
	links := sb.buildLinksForUser("vless", "vless-argo-test", 30000, ibMap, uMap, "127.0.0.1", nil)
	if len(links) == 0 {
		t.Fatalf("预期生成链接，实际为空")
	}
	rawLink := links[0]
	u, err := url.Parse(rawLink)
	if err != nil {
		t.Fatalf("解析生成的链接失败: %v, raw: %s", err, rawLink)
	}
	if u.Scheme != "vless" {
		t.Errorf("Scheme 错误: got %s, want vless", u.Scheme)
	}
	if u.Port() != "443" {
		t.Errorf("端口应为 443，实际: %s", u.Port())
	}
	q := u.Query()
	if q.Get("security") != "tls" {
		t.Errorf("security 参数必须为 tls，实际: %s", q.Get("security"))
	}
	if q.Get("sni") != "tunnel.example.com" {
		t.Errorf("sni 必须为 tunnel.example.com，实际: %s", q.Get("sni"))
	}
	if q.Get("host") != "tunnel.example.com" {
		t.Errorf("host 必须为 tunnel.example.com，实际: %s", q.Get("host"))
	}
	if q.Get("fp") != "chrome" {
		t.Errorf("fp 必须为 chrome，实际: %s", q.Get("fp"))
	}
	if !strings.Contains(q.Get("path"), "ed=2560") {
		t.Errorf("path 应包含 early_data 参数 ed=2560，实际: %s", q.Get("path"))
	}

	// 2. 有优选 IP 列表（包含端口为 0 或自定义优选端口 17554）时的测试
	prefAddrs := []NodeAddrItem{
		{
			Server:     "154.64.251.220",
			ServerPort: 17554,
			Remark:     "优选1",
		},
		{
			Server:     "104.16.1.1",
			ServerPort: 0,
			Remark:     "优选2",
		},
		{
			Server:     "2606:4700::1",
			ServerPort: 443,
			Remark:     "IPv6优选",
		},
	}
	linksPref := sb.buildLinksForUser("vless", "vless-argo-test", 30000, ibMap, uMap, "127.0.0.1", prefAddrs)
	if len(linksPref) != 3 {
		t.Fatalf("预期生成 3 条优选链接，实际: %d", len(linksPref))
	}

	// 检查第 1 条（17554 优选端口，无 item.TLS）
	u1, _ := url.Parse(linksPref[0])
	q1 := u1.Query()
	if q1.Get("security") != "tls" {
		t.Errorf("优选节点 1 的 security 必须为 tls，实际: %s", q1.Get("security"))
	}
	if q1.Get("sni") != "tunnel.example.com" {
		t.Errorf("优选节点 1 的 sni 必须为 tunnel.example.com，实际: %s", q1.Get("sni"))
	}
	if q1.Get("host") != "tunnel.example.com" {
		t.Errorf("优选节点 1 的 host 必须为 tunnel.example.com，实际: %s", q1.Get("host"))
	}
	if q1.Get("fp") != "chrome" {
		t.Errorf("优选节点 1 的 fp 必须为 chrome，实际: %s", q1.Get("fp"))
	}

	// 检查第 2 条（ServerPort <= 0 时必须自动回退至 443 端口）
	u2, _ := url.Parse(linksPref[1])
	if u2.Port() != "443" {
		t.Errorf("优选节点 2 的端口应自动兜底为 443，实际: %s", u2.Port())
	}
	if u2.Query().Get("security") != "tls" {
		t.Errorf("优选节点 2 的 security 必须为 tls，实际: %s", u2.Query().Get("security"))
	}

	// 检查第 3 条（IPv6 地址包裹 []）
	if !strings.Contains(linksPref[2], "@[2606:4700::1]:443") {
		t.Errorf("优选节点 3 的 IPv6 地址应包含 RFC 3986 方括号包裹，实际链接: %s", linksPref[2])
	}
}

func TestBuildLinksFromInbound_SUI_TLSComplete(t *testing.T) {
	s := &SUI{
		Host: "1.2.3.4",
	}

	outJSON := []byte(`{
		"type": "vless",
		"tag": "vless-argo-sui",
		"listen": "127.0.0.1",
		"listen_port": 31390,
		"transport": {
			"type": "ws",
			"path": "/vlws9999",
			"headers": {
				"Host": "sui-tunnel.test.com"
			},
			"early_data_header_name": "Sec-WebSocket-Protocol",
			"max_early_data": 2560
		},
		"users": [
			{"name": "admin", "uuid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
		]
	}`)

	addrsJSON := []byte(`[
		{"server": "154.64.251.220", "server_port": 17554}
	]`)

	links := s.buildLinksFromInbound(outJSON, addrsJSON, nil, "1.2.3.4", "vless-argo-sui")
	if len(links) == 0 {
		t.Fatalf("s-ui buildLinksFromInbound 未生成链接")
	}

	rawLink := links[0]
	u, err := url.Parse(rawLink)
	if err != nil {
		t.Fatalf("解析生成的链接失败: %v", err)
	}
	q := u.Query()
	if q.Get("security") != "tls" {
		t.Errorf("s-ui 模式下 vless-argo 必须具有 security=tls，实际: %s, raw: %s", q.Get("security"), rawLink)
	}
	if q.Get("sni") != "sui-tunnel.test.com" {
		t.Errorf("s-ui 模式下 vless-argo sni 必须为 sui-tunnel.test.com，实际: %s", q.Get("sni"))
	}
	if q.Get("host") != "sui-tunnel.test.com" {
		t.Errorf("s-ui 模式下 vless-argo host 必须为 sui-tunnel.test.com，实际: %s", q.Get("host"))
	}
	if q.Get("fp") != "chrome" {
		t.Errorf("s-ui 模式下 vless-argo fp 必须为 chrome，实际: %s", q.Get("fp"))
	}
}

func TestFormatNodeURI_EncryptionNone(t *testing.T) {
	// 1. 测试原始缺少 encryption=none 的 vless 链接
	raw := "vless://610fa06f-36a3-4779-a60c-61e2ddb2907f@154.64.251.220:17554?type=ws&path=%2Fvlws019e2ec5%3Fed%3D2560&host=dgn.20023.bond&security=tls&fp=chrome&sni=dgn.20023.bond#vless-argo-37kp"
	formatted := formatNodeURI(raw, "vless-argo-37kp")
	u, err := url.Parse(formatted)
	if err != nil {
		t.Fatalf("url.Parse failed: %v", err)
	}
	if u.Query().Get("encryption") != "none" {
		t.Fatalf("expected encryption=none, got %s in %s", u.Query().Get("encryption"), formatted)
	}

	// 2. 测试已有 encryption=none 的 vless 链接不被破坏
	raw2 := "vless://610fa06f-36a3-4779-a60c-61e2ddb2907f@154.64.251.220:17554?encryption=none&type=ws&security=tls#node"
	formatted2 := formatNodeURI(raw2, "node")
	u2, err := url.Parse(formatted2)
	if err != nil {
		t.Fatalf("url.Parse failed: %v", err)
	}
	if u2.Query().Get("encryption") != "none" {
		t.Fatalf("expected encryption=none, got %s", u2.Query().Get("encryption"))
	}
}

func TestAuthWrap_SubscriptionBypass(t *testing.T) {
	tmpDir := t.TempDir()
	auth, _, err := NewAuth(tmpDir)
	if err != nil {
		t.Fatalf("NewAuth failed: %v", err)
	}
	pw := auth.currentPassword()

	hit := false
	handler := auth.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	// 测试 /sub=PASSWORD 放行
	req := httptest.NewRequest(http.MethodGet, "/sub="+pw, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !hit || rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /sub=password, got %d, hit=%v", rec.Code, hit)
	}

	// 测试 /sub=PASSWORD/clash 带子路径放行
	hit = false
	req = httptest.NewRequest(http.MethodGet, "/sub="+pw+"/clash", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !hit || rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /sub=password/clash, got %d, hit=%v", rec.Code, hit)
	}

	// 测试 /sub?token=PASSWORD 查询参数放行
	hit = false
	req = httptest.NewRequest(http.MethodGet, "/sub?token="+pw, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !hit || rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /sub?token=password, got %d, hit=%v", rec.Code, hit)
	}

	// 测试错误密码应返回 401
	hit = false
	req = httptest.NewRequest(http.MethodGet, "/sub=wrongpw", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if hit || rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", rec.Code)
	}
}
