package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// ExitInbound 是挂在某个出口上的一个入站。
type ExitInbound struct {
	ID       int    `json:"id"`
	Port     int    `json:"port"`
	Remark   string `json:"remark"`
	Protocol string `json:"protocol"`
	Enable   bool   `json:"enable"`
	Tag      string `json:"tag"`
}

// Exit 是界面上的一个 VPN 隧道出口。
type Exit struct {
	Slot      int           `json:"slot"`
	Port      int           `json:"port"` // SOCKS5 端口
	Host      string        `json:"host"`
	Region    string        `json:"region"`
	Country   string        `json:"country"`
	Ping      int           `json:"ping"`
	SpeedMbps float64       `json:"speed_mbps"`
	ExitIP    string        `json:"exit_ip"`
	Status    string        `json:"status"`
	Err       string        `json:"err,omitempty"`
	Since     time.Time     `json:"since"`
	SocksUser string        `json:"socks_user"`
	SocksPass string        `json:"socks_pass"`
	Inbounds  []ExitInbound `json:"inbounds"`
	Kind       string        `json:"kind,omitempty"` // "vpngate" | "custom"
	IPType     string        `json:"ip_type,omitempty"` // "residential" | "datacenter"
	ISP        string        `json:"isp,omitempty"`
	SourceName string        `json:"source_name,omitempty"`
	Protocol   string        `json:"protocol,omitempty"` // "wireguard" | "https" | "http" | "openvpn" | "socks5"
}

// NodeBranch 是某个节点下的一个分流分支（如直连分支、日本家宽分支等）
type NodeBranch struct {
	ID         int      `json:"id"`
	Tag        string   `json:"tag"`
	Remark     string   `json:"remark"`
	Protocol   string   `json:"protocol"`
	Port       int      `json:"port"`
	BoundTo    string   `json:"bound_to"`
	BoundLabel string   `json:"bound_label"`
	IsBase     bool     `json:"is_base"`
	Enabled    bool     `json:"enabled"`
	Links      []string `json:"links"`
}

// GroupedNode 是以 s-ui 原生节点为主体的卡片数据结构
type GroupedNode struct {
	BaseID   int          `json:"base_id"`
	Name     string       `json:"name"`
	Protocol string       `json:"protocol"`
	Port     int          `json:"port"`
	Branches []NodeBranch `json:"branches"`
}

// ExitsView 是主界面需要的全部数据。
type ExitsView struct {
	Nodes         []GroupedNode   `json:"nodes"`
	Exits         []Exit          `json:"exits"`
	Direct        []ExitInbound   `json:"direct"`
	CustomSources []*CustomSource `json:"custom_sources,omitempty"`
	Panel         string          `json:"panel,omitempty"`
	Backend       string          `json:"backend"`
	PanelInfo     string          `json:"panel_info"`
	PublicIP      string          `json:"public_ip"`
}

type inboundCache struct {
	mu   sync.Mutex
	at   time.Time
	list []Inbound
	err  error
}

const inboundCacheTTL = 2500 * time.Millisecond

var ibCache inboundCache

func cachedInbounds(live map[string]bool) ([]Inbound, error) {
	ibCache.mu.Lock()
	defer ibCache.mu.Unlock()
	if time.Since(ibCache.at) < inboundCacheTTL {
		return ibCache.list, ibCache.err
	}

	var list []Inbound
	x, err := openPanel()
	if err == nil {
		list, err = x.Inbounds(live)
	}
	ibCache.at, ibCache.list, ibCache.err = time.Now(), list, err
	return list, err
}

func invalidateInbounds() {
	ibCache.mu.Lock()
	ibCache.at = time.Time{}
	ibCache.mu.Unlock()
}

func isResidentialBranch(tag string) bool {
	return strings.Contains(tag, " (") && strings.HasSuffix(tag, ")")
}

func getBaseTag(tag string) string {
	if isResidentialBranch(tag) {
		idx := strings.LastIndex(tag, " (")
		if idx != -1 {
			return tag[:idx]
		}
	}
	return tag
}

// ExitsOf 把隧道和入站组织成以「s-ui 节点」和「出口出站」双重视图
func (m *Manager) ExitsOf() ExitsView {
	tunnels := m.Tunnels()
	publicHost := hostPublicIP()
	view := ExitsView{
		Nodes:    make([]GroupedNode, 0),
		Exits:    make([]Exit, 0, len(tunnels)),
		PublicIP: publicHost,
	}

	var p Panel
	if panel, err := openPanel(); err == nil {
		p = panel
		view.Backend = panel.Kind()
		view.PanelInfo = panel.Describe()
	}

	live := map[string]bool{}
	for _, t := range tunnels {
		if t.Status == "up" {
			addLive := func(h string) {
				h = strings.TrimSpace(h)
				if h != "" {
					live[h] = true
					live[sanitizeTag(h)] = true
				}
			}
			addLive(t.Node.HostName)
			addLive(t.Node.IP)
			addLive(t.CustomHost)
			addLive(t.ExitIP)
			for _, part := range strings.Split(t.Node.HostName, "-") {
				if net.ParseIP(part) != nil {
					addLive(part)
				}
			}
		}
	}

	byHost := map[string]int{}
	hostToTunnel := map[string]*Tunnel{}
	for i, t := range tunnels {
		addMap := func(h string) {
			h = strings.TrimSpace(h)
			if h != "" {
				byHost[h] = i
				byHost[sanitizeTag(h)] = i
				hostToTunnel[h] = t
				hostToTunnel[sanitizeTag(h)] = t
			}
		}
		addMap(t.Node.HostName)
		addMap(t.Node.IP)
		addMap(t.CustomHost)
		addMap(t.ExitIP)
		for _, part := range strings.Split(t.Node.HostName, "-") {
			if net.ParseIP(part) != nil {
				addMap(part)
			}
		}
		cred := t.credential()
		hostLower := strings.ToLower(t.Node.HostName)
		rmkLower := strings.ToLower(t.Node.Remark)
		kind := t.Kind
		if kind == "" {
			if t.CustomProto != "" || t.CustomHost != "" || (t.Node.Protocol != "" && t.Node.Protocol != "openvpn") ||
				strings.Contains(hostLower, "totallyacdn.com") || strings.Contains(hostLower, "cloudflareclient.com") ||
				strings.Contains(hostLower, "opera") || strings.HasPrefix(hostLower, "cs-") {
				kind = "custom"
			} else {
				kind = "vpngate"
			}
		}
		ipType := t.IPType
		if kind == "custom" {
			ipType = "datacenter"
			t.IPType = "datacenter"
		} else if ipType == "" || kind == "vpngate" {
			ipType = "residential"
			t.IPType = "residential"
		}


		proto := "openvpn"
		if kind == "custom" {
			if t.CustomProto != "" {
				proto = t.CustomProto
			} else if t.Node.Protocol != "" {
				proto = t.Node.Protocol
			} else if strings.Contains(hostLower, "cloudflareclient.com") || strings.HasPrefix(hostLower, "cs-wg-") {
				proto = "wireguard"
			} else if strings.Contains(hostLower, "totallyacdn.com") || strings.HasSuffix(hostLower, "-443") {
				proto = "https"
			} else if strings.Contains(hostLower, "opera") {
				proto = "http"
			} else {
				proto = "socks5"
			}
		} else {
			proto = "openvpn"
		}

		sourceName := "VPN Gate"
		if kind == "custom" {
			sourceName = "自定义订阅"
			srcID := t.Node.SourceID
			if srcID == "" {
				srcID = t.TargetSourceID
			}
			if srcID != "" && globalCustomStore != nil {
				globalCustomStore.mu.RLock()
				if s, ok := globalCustomStore.Sources[srcID]; ok && s.Name != "" {
					sourceName = s.Name
				} else if s, ok := globalCustomStore.Sources["preset-"+srcID]; ok && s.Name != "" {
					sourceName = s.Name
				} else if idx := strings.Index(srcID, ":"); idx > 0 {
					prefix := srcID[:idx]
					if s, ok := globalCustomStore.Sources["preset-"+prefix]; ok && s.Name != "" {
						sourceName = s.Name
					} else if s, ok := globalCustomStore.Sources[prefix]; ok && s.Name != "" {
						sourceName = s.Name
					}
				}
				globalCustomStore.mu.RUnlock()
			}
			if sourceName == "自定义订阅" {
				if strings.Contains(rmkLower, "windscribe") || strings.Contains(hostLower, "windscribe") || strings.Contains(hostLower, "totallyacdn.com") {
					sourceName = "Windscribe"
				} else if strings.Contains(rmkLower, "warp") || strings.Contains(rmkLower, "cloudflare") || strings.Contains(hostLower, "warp") || strings.Contains(hostLower, "cloudflareclient.com") {
					sourceName = "WARP"
				} else if strings.Contains(rmkLower, "proton") || strings.Contains(hostLower, "proton") || strings.Contains(strings.ToLower(t.TargetSourceID), "proton") {
					sourceName = "Proton"
				} else if strings.Contains(rmkLower, "opera") || strings.Contains(hostLower, "opera") {
					sourceName = "Opera"
				} else if t.Node.Remark != "" && t.Node.Remark != t.Node.IP {
					sourceName = t.Node.Remark
				}
			}
		}
		view.Exits = append(view.Exits, Exit{
			Slot: t.Slot, Port: t.Port, Host: t.Node.HostName,
			Region: t.Node.CountryCode, Country: t.Node.Country,
			Ping: t.Node.Ping, SpeedMbps: t.Node.SpeedMbps,
			ExitIP: t.ExitIP, Status: t.Status, Err: t.Err, Since: t.Since,
			SocksUser: cred.User, SocksPass: cred.Pass,
			Kind:       kind,
			IPType:     ipType,
			ISP:        t.ISP,
			SourceName: sourceName,
			Protocol:   proto,
		})
	}

	list, err := cachedInbounds(live)
	if err != nil {
		view.Panel = err.Error()
		return view
	}

	// 统一优先校准所有分支的 ib.Tag，去除机房后缀，并透传自定义 Remark 与精确源名称
	for idx := range list {
		ib := &list[idx]
		if ib.BoundTo != "" {
			if t, ok := hostToTunnel[ib.BoundTo]; ok {
				exitName := ""
				if t.Kind == "custom" {
					if t.Node.Remark != "" && t.Node.Remark != t.Node.IP {
						exitName = t.Node.Remark
					} else if globalCustomStore != nil {
						globalCustomStore.mu.RLock()
						if cn, ok := globalCustomStore.Nodes[t.Node.HostName]; ok && cn.Remark != "" {
							exitName = cn.Remark
						}
						globalCustomStore.mu.RUnlock()
					}
				}
				if exitName == "" {
					if t.Node.Remark != "" && !strings.Contains(t.Node.Remark, "://") && !strings.Contains(t.Node.Remark, "@") && t.Node.Remark != t.Node.IP {
						exitName = t.Node.Remark
					} else {
						exitName = formatExitRemark(t.TargetRegion, t.IPType, t.Node.HostName)
						if exitName == "" || exitName == "出站出口" || exitName == "出口分流" {
							if t.Node.Country != "" && t.Node.Country != "自定义" {
								exitName = t.Node.Country
							} else {
								exitName = "出口分流"
							}
						}
					}
				}
				if !ib.IsBase && exitName != "" {
					baseTag := getBaseTag(ib.Tag)
					ib.Tag = fmt.Sprintf("%s (%s)", baseTag, exitName)
				}
			}
		}
	}

	for _, ib := range list {
		row := ExitInbound{
			ID: ib.ID, Port: ib.Port, Remark: ib.Remark,
			Protocol: ib.Protocol, Enable: ib.Enable, Tag: ib.Tag,
		}
		if i, ok := byHost[ib.BoundTo]; ib.BoundTo != "" && ok {
			view.Exits[i].Inbounds = append(view.Exits[i].Inbounds, row)
			continue
		}
		view.Direct = append(view.Direct, row)
	}

	baseNodeMap := map[string]*GroupedNode{}
	var baseOrder []string

	// 识别基础原生入站并去重保序 (仅基础原生母节点生成独立卡片)
	for _, ib := range list {
		if !ib.IsBase {
			continue
		}
		baseTag := getBaseTag(ib.Tag)
		if _, exists := baseNodeMap[baseTag]; !exists {
			node := &GroupedNode{
				BaseID:   ib.ID,
				Name:     baseTag,
				Protocol: strings.ToUpper(ib.Protocol),
				Port:     ib.Port,
				Branches: make([]NodeBranch, 0),
			}
			baseNodeMap[baseTag] = node
			baseOrder = append(baseOrder, baseTag)
		}
	}

	// 第三轮：挂载分支
	for _, ib := range list {
		targetID := ib.ID
		if ib.ClientID > 0 {
			targetID = ib.ClientID
		}
		boundLabel := "直连"
		if ib.BoundTo != "" {
			if t, ok := hostToTunnel[ib.BoundTo]; ok {
				exitIP := t.ExitIP
				if exitIP == "" {
					exitIP = "连接中"
				}
				exitName := ""
				if t.Kind == "custom" {
					if t.Node.Remark != "" && t.Node.Remark != t.Node.IP {
						exitName = t.Node.Remark
					} else if globalCustomStore != nil {
						globalCustomStore.mu.RLock()
						if cn, ok := globalCustomStore.Nodes[t.Node.HostName]; ok && cn.Remark != "" {
							exitName = cn.Remark
						}
						globalCustomStore.mu.RUnlock()
					}
				}
				if exitName == "" {
					if t.Node.Remark != "" && !strings.Contains(t.Node.Remark, "://") && !strings.Contains(t.Node.Remark, "@") && t.Node.Remark != t.Node.IP {
						exitName = t.Node.Remark
					} else {
						exitName = formatExitRemark(t.TargetRegion, t.IPType, t.Node.HostName)
						if exitName == "" || exitName == "出站出口" || exitName == "出口分流" {
							if t.Node.Country != "" && t.Node.Country != "自定义" {
								exitName = t.Node.Country
							} else {
								exitName = "出口分流"
							}
						}
					}
				}
				boundLabel = fmt.Sprintf("%s (%s · SOCKS5:%d)", exitName, exitIP, t.Port)
			} else {
				if !ib.IsBase {
					// 绑定的出口隧道已在隧道池中彻底删除，跳过该失效孤儿分支，避免前端残留
					continue
				}
				boundLabel = fmt.Sprintf("出口 (%s)", ib.BoundTo)
			}
		}

		var links []string
		if sui, ok := p.(*SUI); ok {
			links = sui.InboundBranchLinks(ib.ID, ib.ClientID, ib.Tag, publicHost)
		} else if sb, ok := p.(*SingBox); ok {
			links = sb.InboundBranchLinks(ib.ID, ib.ClientID, ib.Remark, publicHost)
		} else if p != nil {
			if l, err := p.InboundLinks([]int{targetID}, publicHost); err == nil {
				links = l
			}
		}

		isBase := ib.IsBase
		if !isBase && isResidentialBranch(ib.Tag) {
			isBase = false
		}
		enabled := isBranchEnabled(ib.Tag, ib.Port)
		if !enabled {
			links = nil
		}
		if links == nil {
			links = []string{}
		}
		branch := NodeBranch{
			ID:         targetID,
			Tag:        ib.Tag,
			Remark:     ib.Remark,
			Protocol:   strings.ToUpper(ib.Protocol),
			Port:       ib.Port,
			BoundTo:    ib.BoundTo,
			BoundLabel: boundLabel,
			IsBase:     isBase,
			Enabled:    enabled,
			Links:      links,
		}

		targetBaseName := getBaseTag(ib.Tag)
		if node, ok := baseNodeMap[targetBaseName]; ok {
			node.Branches = append(node.Branches, branch)
		} else {
			// 兜底方案：根据 ib.ID 精确匹配母节点的 BaseID
			for _, node := range baseNodeMap {
				if node.BaseID == ib.ID {
					node.Branches = append(node.Branches, branch)
					break
				}
			}
		}
	}

	for _, name := range baseOrder {
		if node, ok := baseNodeMap[name]; ok {
			view.Nodes = append(view.Nodes, *node)
		}
	}

	return view
}
