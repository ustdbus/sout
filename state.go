package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// persistedTunnel 是落盘在 state.json 中的持久化隧道结构
type persistedTunnel struct {
	Slot        int     `json:"slot"`
	Port        int     `json:"port"`
	HostName    string  `json:"hostname"`
	CountryCode string  `json:"country_code"`
	Country     string  `json:"country"`
	Ping        int     `json:"ping,omitempty"`
	SpeedMbps   float64 `json:"speed_mbps,omitempty"`
	Config      string  `json:"config"`
	// SOCKS5 凭据
	SocksUser  string `json:"socks_user,omitempty"`
	SocksPass  string `json:"socks_pass,omitempty"`
	Kind       string `json:"kind,omitempty"`
	CustomHost string `json:"custom_host,omitempty"`
	CustomPort int    `json:"custom_port,omitempty"`
	CustomUser string `json:"custom_user,omitempty"`
	CustomPass string `json:"custom_pass,omitempty"`
}

type persistedState struct {
	Tunnels []persistedTunnel `json:"tunnels"`
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// saveState 把当前运行的隧道全部持久化到磁盘
func (m *Manager) saveState() error {
	var st persistedState
	for _, t := range m.Tunnels() {
		if t.Status == "stopped" {
			continue
		}
		st.Tunnels = append(st.Tunnels, persistedTunnel{
			Slot:        t.Slot,
			Port:        t.Port,
			HostName:    t.Node.HostName,
			CountryCode: t.Node.CountryCode,
			Country:     t.Node.Country,
			Ping:        t.Node.Ping,
			SpeedMbps:   t.Node.SpeedMbps,
			Config:      t.Node.Config,
			SocksUser:   t.Cred.User,
			SocksPass:   t.Cred.Pass,
			Kind:        t.Kind,
			CustomHost:  t.CustomHost,
			CustomPort:  t.CustomPort,
			CustomUser:  t.CustomUser,
			CustomPass:  t.CustomPass,
		})
	}

	blob, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(m.workDir) + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(m.workDir))
}

// restoreState 恢复上次保存的隧道列表
func (m *Manager) restoreState() (int, error) {
	blob, err := os.ReadFile(statePath(m.workDir))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var st persistedState
	if err := json.Unmarshal(blob, &st); err != nil {
		return 0, fmt.Errorf("解析状态文件失败: %w", err)
	}

	known := map[string]Node{}
	for _, n := range m.nodes {
		known[n.HostName] = n
	}

	for _, p := range st.Tunnels {
		node, ok := known[p.HostName]
		if !ok {
			node = Node{
				HostName:    p.HostName,
				CountryCode: p.CountryCode,
				Country:     p.Country,
				Ping:        p.Ping,
				SpeedMbps:   p.SpeedMbps,
			}
		} else {
			if node.Ping == 0 && p.Ping > 0 {
				node.Ping = p.Ping
			}
			if node.SpeedMbps == 0 && p.SpeedMbps > 0 {
				node.SpeedMbps = p.SpeedMbps
			}
		}
		node.Config = p.Config
		cred := SocksCred{User: p.SocksUser, Pass: p.SocksPass}
		if cred.User == "" || cred.Pass == "" {
			gen, err := newSocksCred()
			if err != nil {
				return 0, fmt.Errorf("生成 SOCKS5 凭据失败: %w", err)
			}
			cred = gen
		}
		t := &Tunnel{
			Slot:       p.Slot,
			Port:       p.Port,
			Node:       node,
			Status:     "starting",
			Cred:       cred,
			Kind:       p.Kind,
			CustomHost: p.CustomHost,
			CustomPort: p.CustomPort,
			CustomUser: p.CustomUser,
			CustomPass: p.CustomPass,
		}
		m.mu.Lock()
		m.tunnels[p.Slot] = t
		m.mu.Unlock()
		go m.bringUpPersist(t, true, true)
	}
	return len(st.Tunnels), nil
}
