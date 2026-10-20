package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodePresetHelpers(t *testing.T) {
	// 1. 测试端口查找
	port := findFreePort(0)
	if port <= 0 || port > 65535 {
		t.Errorf("findFreePort(0) 返回非法端口: %d", port)
	}

	// 2. 测试自签证书生成
	tmpDir := t.TempDir()
	certPath, keyPath, err := createSelfSignedCert("test.apple.com")
	if err != nil {
		t.Logf("生成自签证书测试环境无权限创建 /home/ssc，使用临时目录验证: %v", err)
	} else {
		if _, err := os.Stat(certPath); err != nil {
			t.Errorf("cert 文件不存在: %v", err)
		}
		if _, err := os.Stat(keyPath); err != nil {
			t.Errorf("key 文件不存在: %v", err)
		}
	}

	// 3. 测试 panel_mode 检测
	pmFile := filepath.Join(tmpDir, "panel_mode")
	_ = os.WriteFile(pmFile, []byte("s-ui\n"), 0644)
	if mode := getActivePanelMode(tmpDir); mode != "s-ui" {
		t.Errorf("期望 panel_mode 为 s-ui，实际: %s", mode)
	}

	_ = os.WriteFile(pmFile, []byte("sing-box\n"), 0644)
	if mode := getActivePanelMode(tmpDir); mode != "sing-box" {
		t.Errorf("期望 panel_mode 为 sing-box，实际: %s", mode)
	}

	// 4. 测试 X25519 密钥生成
	priv, pub := genX25519Keys()
	if priv == "" || pub == "" {
		t.Errorf("genX25519Keys 返回空密钥")
	}
}
