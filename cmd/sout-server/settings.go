package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// WebSettings 是面板的可持久化配置（端口、监听地址、自定义面板 URL、SSL/HTTPS 等）
type WebSettings struct {
	Port       int    `json:"port"`
	ListenAddr string `json:"listen_addr"`
	PanelURL   string `json:"panel_url,omitempty"`
	SSLEnabled bool   `json:"ssl_enabled"`
	SSLDomain  string `json:"ssl_domain,omitempty"`
	SSLCert    string `json:"ssl_cert,omitempty"`
	SSLKey     string `json:"ssl_key,omitempty"`
}

var (
	webSettingsMu   sync.RWMutex
	webSettingsCur  WebSettings
	webSettingsPath string
)

func webSettingsFilePath(dir string) string { return filepath.Join(dir, "settings.json") }

func loadWebSettings(dir string, defaultPort int, portExplicit bool) (WebSettings, error) {
	webSettingsPath = webSettingsFilePath(dir)

	s := WebSettings{Port: defaultPort, ListenAddr: ""}
	blob, err := os.ReadFile(webSettingsPath)
	switch {
	case os.IsNotExist(err):
		webSettingsMu.Lock()
		webSettingsCur = s
		webSettingsMu.Unlock()
		return s, saveWebSettings()
	case err != nil:
		return s, err
	}
	if err := json.Unmarshal(blob, &s); err != nil {
		return s, err
	}
	if s.Port == 0 {
		s.Port = defaultPort
	}
	s.ListenAddr, _ = normalizeListenAddr(s.ListenAddr)
	changed := false
	if portExplicit && s.Port != defaultPort {
		s.Port = defaultPort
		changed = true
	}
	webSettingsMu.Lock()
	webSettingsCur = s
	webSettingsMu.Unlock()
	if changed {
		return s, saveWebSettings()
	}
	return s, nil
}

func getWebSettings() WebSettings {
	webSettingsMu.RLock()
	defer webSettingsMu.RUnlock()
	return webSettingsCur
}

func saveWebSettings() error {
	webSettingsMu.RLock()
	blob, err := json.MarshalIndent(webSettingsCur, "", "  ")
	webSettingsMu.RUnlock()
	if err != nil {
		return err
	}
	tmp := webSettingsPath + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, webSettingsPath)
}

func normalizeListenAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" || addr == "0.0.0.0" || strings.EqualFold(addr, "all") {
		return "", nil
	}
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String(), nil
	}
	return "", fmt.Errorf("地址不是合法 IP，留空或 0.0.0.0 表示公网监听")
}

func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间")
	}
	return nil
}

func (s WebSettings) listenAddrString() string {
	return net.JoinHostPort(s.ListenAddr, strconv.Itoa(s.Port))
}

func (s WebSettings) schemeString() string {
	if s.SSLEnabled {
		return "https"
	}
	return "http"
}

func validateSSL(certPath, keyPath string) error {
	certPath = strings.TrimSpace(certPath)
	keyPath = strings.TrimSpace(keyPath)
	if certPath == "" || keyPath == "" {
		return fmt.Errorf("开启 SSL 必须提供证书 (cert) 和私钥 (key) 文件路径")
	}
	if _, err := os.Stat(certPath); err != nil {
		return fmt.Errorf("证书文件不存在或无法读取: %w", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("私钥文件不存在或无法读取: %w", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("证书或私钥格式解析失败: %w", err)
	}
	return nil
}
