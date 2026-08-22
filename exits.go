package main

import (
	"fmt"
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
	ExitIP    string        `json:"exit_ip"`
	Status    string        `json:"status"`
	Err       string        `json:"err,omitempty"`
	Since     time.Time     `json:"since"`
	SocksUser string        `json:"socks_user"`
	SocksPass string        `json:"socks_pass"`
	Inbounds  []ExitInbound `json:"inbounds"`
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
	Nodes     []GroupedNode `json:"nodes"`
	Exits     []Exit        `json:"exits"`
	Direct    []ExitInbound `json:"direct"`
	Panel     string        `json:"panel"`
	Backend   string        `json:"backend"`
	PanelInfo string        `json:"panel_info"`
	PublicIP  string        `json:"public_ip"`
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
	return strings.Contains(tag, " (") && (strings.HasSuffix(tag, "家宽)") || strings.Contains(tag, "家宽-"))
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
			live[sanitizeTag(t.Node.HostName)] = true
		}
	}

	byHost := map[string]int{}
	hostToTunnel := map[string]*Tunnel{}
	for i, t := range tunnels {
		sTag := sanitizeTag(t.Node.HostName)
		byHost[sTag] = i
		hostToTunnel[sTag] = t
		cred := t.credential()
		view.Exits = append(view.Exits, Exit{
			Slot: t.Slot, Port: t.Port, Host: t.Node.HostName,
			Region: t.Node.CountryCode, Country: t.Node.Country,
			ExitIP: t.ExitIP, Status: t.Status, Err: t.Err, Since: t.Since,
			SocksUser: cred.User, SocksPass: cred.Pass,
		})
	}

	list, err := cachedInbounds(live)
	if err != nil {
		view.Panel = err.Error()
		return view
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

	// 第一轮：识别基础原生入站（非家宽后缀）
	for _, ib := range list {
		if !isResidentialBranch(ib.Tag) {
			node := &GroupedNode{
				BaseID:   ib.ID,
				Name:     ib.Tag,
				Protocol: strings.ToUpper(ib.Protocol),
				Port:     ib.Port,
				Branches: make([]NodeBranch, 0),
			}
			baseNodeMap[ib.Tag] = node
			baseOrder = append(baseOrder, ib.Tag)
		}
	}

	// 第二轮：如果某些家宽分支的原生入站不存在，兜底创建 baseNode
	for _, ib := range list {
		if isResidentialBranch(ib.Tag) {
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
	}

	// 第三轮：挂载分支
	for _, ib := range list {
		var links []string
		if p != nil {
			if l, err := p.InboundLinks([]int{ib.ID}, publicHost); err == nil {
				links = l
			}
		}

		boundLabel := "直连（香港）"
		if ib.BoundTo != "" {
			if t, ok := hostToTunnel[ib.BoundTo]; ok {
				cName := countryNameCN(t.Node.CountryCode, t.Node.Country)
				exitIP := t.ExitIP
				if exitIP == "" {
					exitIP = "连接中"
				}
				boundLabel = fmt.Sprintf("%s家宽 (%s · SOCKS5:%d)", cName, exitIP, t.Port)
			} else {
				boundLabel = fmt.Sprintf("出口 (%s)", ib.BoundTo)
			}
		}

		isBase := !isResidentialBranch(ib.Tag)
		branch := NodeBranch{
			ID:         ib.ID,
			Tag:        ib.Tag,
			Remark:     ib.Remark,
			Protocol:   strings.ToUpper(ib.Protocol),
			Port:       ib.Port,
			BoundTo:    ib.BoundTo,
			BoundLabel: boundLabel,
			IsBase:     isBase,
			Links:      links,
		}

		targetBaseName := getBaseTag(ib.Tag)
		if node, ok := baseNodeMap[targetBaseName]; ok {
			node.Branches = append(node.Branches, branch)
		}
	}

	for _, name := range baseOrder {
		if node, ok := baseNodeMap[name]; ok {
			view.Nodes = append(view.Nodes, *node)
		}
	}

	return view
}
