package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// formatURLHost 保证在 URI（如 vless:// 等）中 host 部分符合 RFC 3986 规范（IPv6 地址自动包裹方括号）
func formatURLHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

// sanitizeNodeAddrItem 规范化处理 NodeAddrItem，确保 IPv6 地址干净且端口正确（自动剔除因重复追加造成的 :port:port 污染）
func sanitizeNodeAddrItem(item NodeAddrItem, defaultPort int) NodeAddrItem {
	server := strings.TrimSpace(item.Server)
	port := item.ServerPort
	if port <= 0 {
		port = defaultPort
	}
	remark := strings.TrimSpace(item.Remark)

	// 1. 若带有标准方括号形式: [2401:b60::2]:39192 或 [2401:b60::2]
	if strings.HasPrefix(server, "[") {
		if end := strings.Index(server, "]"); end != -1 {
			rawHost := server[1:end]
			after := strings.TrimSpace(server[end+1:])
			if strings.HasPrefix(after, ":") {
				if p, err := strconv.Atoi(strings.TrimSpace(after[1:])); err == nil && p > 0 && p <= 65535 {
					port = p
				}
			}
			server = rawHost
		}
	} else if ip := net.ParseIP(server); ip != nil {
		// 2. 如果本身就是合法的纯 IP（IPv4 或 IPv6），直接使用，不剥离末段
		server = ip.String()
	} else if strings.Count(server, ":") > 1 {
		// 3. 含有多个冒号且不是合法 IP，说明末尾可能附带了一个或多个被错误追加的端口（如 2401:...:39192 或 2401:...:39192:39192）
		temp := server
		for {
			lastColon := strings.LastIndex(temp, ":")
			if lastColon == -1 {
				break
			}
			tail := strings.TrimSpace(temp[lastColon+1:])
			left := strings.TrimSpace(temp[:lastColon])
			if p, err := strconv.Atoi(tail); err == nil && p > 0 && p <= 65535 {
				port = p
				temp = left
				if ip := net.ParseIP(temp); ip != nil {
					temp = ip.String()
					break
				}
				continue
			}
			break
		}
		server = temp
	} else if strings.Count(server, ":") == 1 {
		// 4. 只有一个冒号（标准 IPv4:port 或 domain:port）
		lastColon := strings.LastIndex(server, ":")
		tail := strings.TrimSpace(server[lastColon+1:])
		left := strings.TrimSpace(server[:lastColon])
		if p, err := strconv.Atoi(tail); err == nil && p > 0 && p <= 65535 {
			server = left
			port = p
		}
	}

	server = strings.Trim(server, "[] \t")
	return NodeAddrItem{
		Server:     server,
		ServerPort: port,
		Remark:     remark,
		TLS:        item.TLS,
	}
}

type NodeAddrItem struct {
	Server     string         `json:"server"`
	ServerPort int            `json:"server_port"`
	Remark     string         `json:"remark,omitempty"`
	TLS        map[string]any `json:"tls,omitempty"`
}

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

type Inbound struct {
	ID       int    `json:"id"`
	ClientID int    `json:"client_id,omitempty"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Remark   string `json:"remark"`
	Enable   bool   `json:"enable"`
	Tag      string `json:"tag"`
	BoundTo  string `json:"route_to,omitempty"`
	BoundUp  bool   `json:"bound_up,omitempty"`
	IsBase   bool   `json:"is_base,omitempty"`
}

type InboundDetail struct {
	Inbound
	Clients []ClientInfo `json:"clients"`
	Links   []string     `json:"links"`
	Listen  string       `json:"listen"`
	Network string       `json:"network"`
	TLS     string       `json:"tls"`
}

type ClientInfo struct {
	Email  string `json:"email"`
	ID     string `json:"id"`
	Enable bool   `json:"enable"`
}

func sanitizeTag(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "node"
	}
	return strings.ToLower(b.String())
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
	case "sing-box":
		sb, err := DetectSingBox(panelState.workDir)
		if err != nil {
			return nil, fmt.Errorf("指定了 sing-box 模式但探测失败: %w", err)
		}
		panelState.current = sb
		return sb, nil
	}

	// 自动探测：先看是否存在 s-ui，再看 sing-box
	if s, err := DetectSUI(panelState.workDir); err == nil {
		panelState.current = s
		_ = savePanelMode(panelState.workDir, "s-ui")
		return s, nil
	}
	if sb, err := DetectSingBox(panelState.workDir); err == nil {
		panelState.current = sb
		_ = savePanelMode(panelState.workDir, "sing-box")
		return sb, nil
	}

	return nil, fmt.Errorf("未检测到支持的后端（请先安装 sing-box 内核或 s-ui 面板）")
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

	sbOK, sbReason := true, ""
	if _, err := DetectSingBox(workDir); err != nil {
		sbOK, sbReason = false, err.Error()
	}
	modes = append(modes, map[string]any{"mode": "sing-box", "label": "sing-box 原生内核", "available": sbOK, "reason": sbReason})

	suiOK, suiReason := true, ""
	if _, err := DetectSUI(workDir); err != nil {
		suiOK, suiReason = false, err.Error()
	}
	modes = append(modes, map[string]any{"mode": "s-ui", "label": "s-ui 面板 (sing-box)", "available": suiOK, "reason": suiReason})

	return modes
}

func switchPanelMode(mode string) (Panel, error) {
	switch mode {
	case "", "sing-box", "s-ui":
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
