package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Panel 是 fanout 管理节点链接的后端。
type Panel interface {
	// Kind 返回 "s-ui"
	Kind() string
	// Describe 给出一行人能读的后端说明。
	Describe() string

	Inbounds(live map[string]bool) ([]Inbound, error)
	InboundDetail(id int, publicHost string) (*InboundDetail, error)
	InboundLinks(ids []int, publicHost string) ([]string, error)

	Bind(inboundTag string, hostname string, tunnels []*Tunnel) error
	Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error
	ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error

	CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]int, error)
	DeleteInbounds(ids []int, tunnels []*Tunnel) error
	DeleteBranchesByHost(host string, tunnels []*Tunnel) error

	CreateInbound(spec NewInboundSpec, tunnels []*Tunnel) (*CreatedInbound, error)
	UpdateInbound(id int, patch InboundPatch, tunnels []*Tunnel) error
	NodeDetail(id int) (*NodeDetailInfo, error)
	UpdateNodeConfig(id int, listen string, listenPort int, addrs []NodeAddrItem, tlsEnabled bool, sni string, tunnels []*Tunnel) error

	AddClient(id int, email string, tunnels []*Tunnel) error
	DeleteClient(id int, email string, tunnels []*Tunnel) error
	ResetClient(id int, email string, tunnels []*Tunnel) error

	OnTunnelsChanged(tunnels []*Tunnel) error
	Close()
}

type NodeAddrItem struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	Remark     string `json:"remark,omitempty"`
}

type NodeDetailInfo struct {
	ID           int            `json:"id"`
	Name         string         `json:"name"`
	Protocol     string         `json:"protocol"`
	Listen       string         `json:"listen"`
	ListenPort   int            `json:"listen_port"`
	TLSEnabled   bool           `json:"tls_enabled"`
	SNI          string         `json:"sni"`
	ServerHasTLS bool           `json:"server_has_tls"`
	Addrs        []NodeAddrItem `json:"addrs"`
}

type InboundPatch struct {
	Port   *int
	Remark *string
	Enable *bool
}

type CreatedInbound struct {
	ID       int    `json:"id"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Remark   string `json:"remark"`
	Network  string `json:"network"`
	Security string `json:"security"`
}

func closePanel() {
	panelState.mu.Lock()
	p := panelState.current
	panelState.mu.Unlock()
	if p != nil {
		p.Close()
	}
}

var panelState struct {
	mu      sync.Mutex
	current Panel
	workDir string
	forced  string
}

func panelModeFile(dir string) string { return filepath.Join(dir, "panel_mode") }

func configurePanel(workDir, mode string) {
	panelState.mu.Lock()
	defer panelState.mu.Unlock()
	panelState.workDir = workDir
	if mode == "" {
		blob, err := os.ReadFile(panelModeFile(workDir))
		if err == nil {
			mode = strings.TrimSpace(string(blob))
		}
	}
	panelState.forced = mode
	panelState.current = nil
}

func savePanelMode(dir, mode string) error {
	if dir == "" {
		return nil
	}
	path := panelModeFile(dir)
	if mode == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(path, []byte(mode), 0600)
}

func openPanel() (Panel, error) {
	panelState.mu.Lock()
	defer panelState.mu.Unlock()

	if panelState.current != nil {
		return panelState.current, nil
	}

	switch panelState.forced {
	case "s-ui":
		s, err := DetectSUI(panelState.workDir)
		if err != nil {
			return nil, fmt.Errorf("指定了 s-ui 模式但探测失败: %w", err)
		}
		panelState.current = s
		return s, nil
	}

	// 自动探测 s-ui
	if s, err := DetectSUI(panelState.workDir); err == nil {
		panelState.current = s
		return s, nil
	}

	return nil, fmt.Errorf("未检测到支持的面板（请先安装 s-ui 面板）")
}

func currentPanelMode() string {
	panelState.mu.Lock()
	defer panelState.mu.Unlock()
	if panelState.forced != "" {
		return panelState.forced
	}
	if panelState.current != nil {
		return panelState.current.Kind()
	}
	return ""
}

func availablePanelModes(workDir string) []map[string]any {
	modes := []map[string]any{}

	suiOK, suiReason := true, ""
	if _, err := DetectSUI(workDir); err != nil {
		suiOK, suiReason = false, err.Error()
	}
	modes = append(modes, map[string]any{"mode": "s-ui", "label": "s-ui 面板 (sing-box)", "available": suiOK, "reason": suiReason})

	return modes
}

func switchPanelMode(mode string) (Panel, error) {
	switch mode {
	case "", "s-ui":
	default:
		return nil, fmt.Errorf("未知后端模式 %q", mode)
	}

	panelState.mu.Lock()
	old := panelState.current
	workDir := panelState.workDir
	panelState.mu.Unlock()
	if old != nil {
		old.Close()
	}

	panelState.mu.Lock()
	panelState.forced = mode
	panelState.current = nil
	panelState.mu.Unlock()

	p, err := openPanel()
	if err != nil {
		panelState.mu.Lock()
		panelState.forced = ""
		panelState.current = nil
		panelState.mu.Unlock()
		return nil, err
	}
	if err := savePanelMode(workDir, mode); err != nil {
		log.Printf("记录后端模式失败: %v", err)
	}
	return p, nil
}
