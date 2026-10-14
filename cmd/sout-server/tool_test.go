package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONGetSet(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "test.json")

	// 1. json-set 顶层 string
	toolJSONSet([]string{f, "name", "sout", "--type", "string"})
	// 2. json-set 顶层 int
	toolJSONSet([]string{f, "port", "8899", "--type", "int"})
	// 3. json-set 顶层 bool
	toolJSONSet([]string{f, "ssl", "true", "--type", "bool"})
	// 4. json-set 点分 key 与自动类型推断
	toolJSONSet([]string{f, "gateway.enabled", "true"})
	toolJSONSet([]string{f, "gateway.rate", "100"})

	// 读取并检验 json 结构
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}

	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	vName, ok := getNestedValue(root, "name")
	if !ok || vName != "sout" {
		t.Errorf("期望 name 为 sout，实际获取: %v", vName)
	}

	vPort, ok := getNestedValue(root, "port")
	if !ok || vPort != int64(8899) {
		t.Errorf("期望 port 为 8899，实际获取: %v", vPort)
	}

	vSSL, ok := getNestedValue(root, "ssl")
	if !ok || vSSL != true {
		t.Errorf("期望 ssl 为 true，实际获取: %v", vSSL)
	}

	vGW, ok := getNestedValue(root, "gateway.enabled")
	if !ok || vGW != true {
		t.Errorf("期望 gateway.enabled 为 true，实际获取: %v", vGW)
	}

	vMissing, ok := getNestedValue(root, "nonexistent.key")
	if ok || vMissing != nil {
		t.Errorf("期望不存在 key 返回 nil，实际获取: %v", vMissing)
	}
}

func TestVerCmp(t *testing.T) {
	tests := []struct {
		v1, v2 string
		want   string
	}{
		{"v4.6.0", "v4.7.0", "<"},
		{"v4.7.8", "4.7.8", "="},
		{"4.8.0", "v4.7.8", ">"},
		{"v1.10.0", "v1.9.0", ">"},
		{"1.0", "1.0.0", "="},
		{"v2.0.1", "2.0.2", "<"},
		{"v5.0.0", "v5.0.0", "="},
	}

	for _, tt := range tests {
		got := compareVersions(tt.v1, tt.v2)
		if got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %q, 期望: %q", tt.v1, tt.v2, got, tt.want)
		}
	}
}

func TestSUIQuery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "nonexistent.db")

	user := querySUIUsername(dbPath)
	if user != "admin" {
		t.Errorf("期望默认用户名为 admin，实际获取: %q", user)
	}

	hasCF := querySUIHasCF(dbPath)
	if hasCF != false {
		t.Errorf("期望 hasCF 为 false，实际获取: %v", hasCF)
	}

	tok := querySUIToken(dbPath)
	if tok != "" {
		t.Errorf("期望空 token，实际获取: %q", tok)
	}
}

func TestSettingsGetSet(t *testing.T) {
	tmpDir := t.TempDir()
	toolSettingsSet([]string{tmpDir, "port", "9999", "--type", "int"})
	toolSettingsSet([]string{tmpDir, "listen_addr", "127.0.0.1"})

	settingsFile := filepath.Join(tmpDir, "settings.json")
	if fi, err := os.Stat(settingsFile); err != nil {
		t.Fatalf("settings.json 未成功生成: %v", err)
	} else if fi.Mode().Perm() != 0600 {
		t.Errorf("期望文件权限为 0600，实际权限: %v", fi.Mode().Perm())
	}
}
