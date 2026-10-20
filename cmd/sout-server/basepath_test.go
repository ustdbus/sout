package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadBasePath_InheritsFromGatewayMeta(t *testing.T) {
	tmpDir := t.TempDir()

	// 模拟已存在 gateway_meta.json
	meta := map[string]any{
		"enabled":   true,
		"sout_path": "myTunnelPath",
	}
	blob, _ := json.Marshal(meta)
	_ = os.WriteFile(filepath.Join(tmpDir, "gateway_meta.json"), blob, 0600)

	// 调用 LoadBasePath，应优先继承 gateway_meta.json 里的 sout_path 并写入 basepath
	bp, created, err := LoadBasePath(tmpDir)
	if err != nil {
		t.Fatalf("LoadBasePath failed: %v", err)
	}
	if bp != "/myTunnelPath" {
		t.Fatalf("expected /myTunnelPath, got %s", bp)
	}
	if created {
		t.Fatalf("expected created to be false when inherited from gateway_meta")
	}

	// 验证 basepath 文件是否已同步落盘
	savedBP, _ := os.ReadFile(filepath.Join(tmpDir, "basepath"))
	if string(savedBP) != "myTunnelPath\n" {
		t.Fatalf("expected basepath file to contain 'myTunnelPath\\n', got %q", string(savedBP))
	}
}

func TestSyncGatewayBasePath_GatewayMeta(t *testing.T) {
	tmpDir := t.TempDir()

	// 模拟已存在 gateway_meta.json 以及废弃残留的 caddy_meta.json
	m := map[string]any{
		"enabled":   true,
		"sout_path": "oldPath",
	}
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(tmpDir, "gateway_meta.json"), b, 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "caddy_meta.json"), b, 0600)

	// 执行同步更新路径到 newPath
	syncGatewayBasePath(tmpDir, "oldPath", "newPath")

	// 验证 gateway_meta.json 已更新
	data, err := os.ReadFile(filepath.Join(tmpDir, "gateway_meta.json"))
	if err != nil {
		t.Fatalf("failed to read gateway_meta.json: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(data, &res)
	if res["sout_path"] != "newPath" {
		t.Fatalf("expected gateway_meta.json sout_path to be newPath, got %v", res["sout_path"])
	}

	// 验证废弃的旧 caddy_meta.json 已被自动清理
	if _, err := os.Stat(filepath.Join(tmpDir, "caddy_meta.json")); !os.IsNotExist(err) {
		t.Fatalf("expected legacy caddy_meta.json to be deleted")
	}
}
