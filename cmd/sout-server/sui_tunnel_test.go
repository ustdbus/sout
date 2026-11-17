package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newMockSUIForTunnelTest(t *testing.T, inbound map[string]any, logLines []string, ipv4, ipv6 []string, onSave func(act string, saved map[string]any)) *SUI {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/apiv2/inbounds", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("id")
		if idStr != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": map[string]any{
					"inbounds": []map[string]any{inbound},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"obj": map[string]any{
				"inbounds": []map[string]any{
					{"id": 1, "tag": "vless-in", "type": "vless"},
					{"id": inbound["id"], "tag": inbound["tag"], "type": inbound["type"]},
				},
			},
		})
	})
	mux.HandleFunc("/app/apiv2/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"obj": map[string]any{
				"sys": map[string]any{
					"ipv4": ipv4,
					"ipv6": ipv6,
				},
			},
		})
	})
	mux.HandleFunc("/app/apiv2/logs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"obj":     logLines,
		})
	})
	mux.HandleFunc("/app/apiv2/save", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var dataMap map[string]any
		_ = json.Unmarshal([]byte(r.FormValue("data")), &dataMap)
		if onSave != nil {
			onSave(r.FormValue("action"), dataMap)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"obj":     true,
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	return &SUI{
		Host:     u.Hostname(),
		Port:     port,
		BasePath: "/app/",
		Scheme:   "http",
		token:    "test-token",
		client:   srv.Client(),
	}
}

func TestSUIGetTunnelOverview_ParsesLogsAndHostIPv6(t *testing.T) {
	inbound := map[string]any{
		"id":              float64(6),
		"tag":             "cf-tunnel-in",
		"type":            "cloudflared",
		"protocol":        "quic",
		"edge_ip_version": float64(0),
		"token":           "secret-token",
	}
	// 倒序日志（最新在前），包含一次重试后全 4 路连接上 cdg12/cdg15
	logs := []string{
		"2026/11/17 04:30:27 INFO - inbound/cloudflared[cf-tunnel-in]connected to cdg12 (connection 23e91506-7401-4c68-ab8d-cd07e8ece55d)",
		"2026/11/17 04:30:27 INFO - inbound/cloudflared[cf-tunnel-in]connecting to edge via QUIC (connection 3)",
		"2026/11/17 04:30:27 INFO - inbound/cloudflared[cf-tunnel-in]connected to cdg12 (connection 98356bd3-be77-4d58-975e-390e42640f94)",
		"2026/11/17 04:30:26 INFO - inbound/cloudflared[cf-tunnel-in]connecting to edge via QUIC (connection 2)",
		"2026/11/17 04:30:26 INFO - inbound/cloudflared[cf-tunnel-in]connected to cdg15 (connection d1083417-7342-4609-8ccb-aebbeef668ec)",
		"2026/11/17 04:30:26 INFO - inbound/cloudflared[cf-tunnel-in]connecting to edge via QUIC (connection 1)",
		"2026/11/17 04:30:26 INFO - inbound/cloudflared[cf-tunnel-in]connected to cdg12 (connection 4f1a2d3b-132a-43a1-a9f6-f9d02ec2978a)",
		"2026/11/17 04:30:25 INFO - inbound/cloudflared[cf-tunnel-in]connecting to edge via QUIC (connection 0)",
		"2026/11/17 04:30:25 INFO - inbound/cloudflared[cf-tunnel-in]starting Cloudflare Tunnel with 4 HA connections",
		"2026/11/17 04:20:00 INFO - inbound/cloudflared[cf-tunnel-in]connected to oldpop (connection 00000000-0000-0000-0000-000000000000)",
	}

	s := newMockSUIForTunnelTest(t, inbound, logs, nil, []string{"2a06:9801:700::1/64"}, nil)
	ov, err := s.GetTunnelOverview(true)
	if err != nil {
		t.Fatalf("GetTunnelOverview failed: %v", err)
	}
	if !ov.HasInbound || ov.InboundID != 6 {
		t.Fatalf("expected inbound #6, got %+v", ov)
	}
	if len(ov.Slots) != 4 {
		t.Fatalf("expected 4 slots, got %d", len(ov.Slots))
	}
	expectedAddrs := []string{"cdg12 · 4f1a2d3b", "cdg15 · d1083417", "cdg12 · 98356bd3", "cdg12 · 23e91506"}
	for i, sl := range ov.Slots {
		if !sl.Connected {
			t.Fatalf("slot %d should be connected", i)
		}
		if sl.IPVersion != 6 {
			t.Fatalf("slot %d expected IPv6 on ipv6-only host, got %d", i, sl.IPVersion)
		}
		if sl.EdgeAddr != expectedAddrs[i] {
			t.Fatalf("slot %d expected edge %q, got %q", i, expectedAddrs[i], sl.EdgeAddr)
		}
		if sl.IsPrimary {
			t.Fatalf("slot %d should not claim IsPrimary in s-ui shared mode", i)
		}
	}
}

func TestSUIUpdateCloudflaredInbound_PreservesFieldsAndSaves(t *testing.T) {
	inbound := map[string]any{
		"id":              float64(6),
		"tag":             "cf-tunnel-in",
		"type":            "cloudflared",
		"protocol":        "quic",
		"edge_ip_version": float64(4),
		"token":           "keep-my-secret-token",
	}

	var savedAct string
	var savedBody map[string]any
	s := newMockSUIForTunnelTest(t, inbound, nil, []string{"1.2.3.4"}, []string{"::1"}, func(act string, saved map[string]any) {
		savedAct = act
		savedBody = saved
	})

	if err := s.UpdateCloudflaredInbound("http2", 6); err != nil {
		t.Fatalf("UpdateCloudflaredInbound failed: %v", err)
	}
	if savedAct != "edit" {
		t.Fatalf("expected action=edit, got %q", savedAct)
	}
	if strings.TrimSpace(savedBody["token"].(string)) != "keep-my-secret-token" {
		t.Fatalf("token was not preserved: %#v", savedBody)
	}
	if savedBody["protocol"] != "http2" || int(savedBody["edge_ip_version"].(float64)) != 6 {
		t.Fatalf("unexpected updated fields: %#v", savedBody)
	}
}

func TestGetGatewayStatus_SUISharedMode(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "panel_mode"), []byte("s-ui\n"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "network_mode"), []byte("ipv6_only\n"), 0600)
	meta := GatewayMeta{
		Enabled:      true,
		Mode:         "tunnel",
		Domain:       "neo.example.com",
		TunnelPort:   8081,
		TunnelEngine: "sing-box",
		Protocol:     "quic",
	}
	raw, _ := json.Marshal(meta)
	_ = os.WriteFile(filepath.Join(dir, "gateway_meta.json"), raw, 0600)

	origFetch := fetchSUITunnelOverview
	defer func() { fetchSUITunnelOverview = origFetch }()

	fetchSUITunnelOverview = func(workDir string, cfRunning bool) *SUITunnelOverview {
		return &SUITunnelOverview{
			HasInbound:    true,
			InboundID:     6,
			Protocol:      "http2",
			EdgeIPVersion: 6,
			Slots: []TunnelSlotInfo{
				{Index: 0, Connected: true, Protocol: "http2", CfgProtocol: "http2", IPVersion: 6, CfgIPVersion: 6, EdgeAddr: "cdg12 · 11112222"},
				{Index: 1, Connected: true, Protocol: "http2", CfgProtocol: "http2", IPVersion: 6, CfgIPVersion: 6, EdgeAddr: "cdg15 · 33334444"},
				{Index: 2, Connected: true, Protocol: "http2", CfgProtocol: "http2", IPVersion: 6, CfgIPVersion: 6, EdgeAddr: "cdg12 · 55556666"},
				{Index: 3, Connected: true, Protocol: "http2", CfgProtocol: "http2", IPVersion: 6, CfgIPVersion: 6, EdgeAddr: "cdg12 · 77778888"},
			},
		}
	}

	st := GetGatewayStatus(dir)
	if st["slot_mode"] != "sui_shared" {
		t.Fatalf("expected slot_mode=sui_shared, got %v", st["slot_mode"])
	}
	if st["primary_slot"] != -1 {
		t.Fatalf("expected primary_slot=-1, got %v", st["primary_slot"])
	}
	if st["protocol"] != "http2" || st["edge_ip_version"] != 6 {
		t.Fatalf("expected protocol=http2 & edge_ip_version=6, got %v / %v", st["protocol"], st["edge_ip_version"])
	}
	slots, ok := st["tunnel_slots"].([]TunnelSlotInfo)
	if !ok || len(slots) != 4 || slots[1].EdgeAddr != "cdg15 · 33334444" {
		t.Fatalf("unexpected tunnel_slots: %+v", st["tunnel_slots"])
	}
}
