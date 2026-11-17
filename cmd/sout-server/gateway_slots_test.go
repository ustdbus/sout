package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTunnelSlotPersistenceAndUpdateAPI(t *testing.T) {
	dir := t.TempDir()

	basePathMu.Lock()
	origDir := basePathDir
	basePathDir = dir
	basePathMu.Unlock()
	defer func() {
		basePathMu.Lock()
		basePathDir = origDir
		basePathMu.Unlock()
	}()

	meta := GatewayMeta{
		Enabled:    true,
		Mode:       "named_tunnel",
		Domain:     "edge.example.com",
		TunnelPort: 8081,
		Protocol:   "quic",
		SoutPath:   "sout",
		SubPath:    "sub",
	}
	raw, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "gateway_meta.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Default status returns 4 slots with alternating IPv4/IPv6 and slot 0 primary
	st := GetGatewayStatus(dir)
	slots, ok := st["tunnel_slots"].([]TunnelSlotInfo)
	if !ok || len(slots) != 4 {
		t.Fatalf("expected 4 tunnel slots, got %T %v", st["tunnel_slots"], st["tunnel_slots"])
	}
	if !slots[0].IsPrimary || slots[0].IPVersion != 4 || slots[1].IPVersion != 6 {
		t.Fatalf("unexpected default slots: %+v", slots)
	}

	// 2. Update slot #2 (index 1) via API handler to http2 + IPv4 + primary
	payload := `{"action":"update_slot","slot_index":1,"protocol":"http2","ip_version":4,"set_primary":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/tunnel", bytes.NewBufferString(payload))
	rec := httptest.NewRecorder()
	apiTunnelHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify persisted gateway_meta.json
	updatedRaw, err := os.ReadFile(filepath.Join(dir, "gateway_meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updated GatewayMeta
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.PrimarySlot != 1 {
		t.Fatalf("expected PrimarySlot=1, got %d", updated.PrimarySlot)
	}
	if len(updated.TunnelSlots) != 4 || updated.TunnelSlots[1].Protocol != "http2" || updated.TunnelSlots[1].IPVersion != 4 {
		t.Fatalf("unexpected persisted slots: %+v", updated.TunnelSlots)
	}
}
