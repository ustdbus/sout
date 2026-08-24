package main

import (
	"fmt"
	"log"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// Manager 维护隧道槽位与状态
type Manager struct {
	mu       sync.RWMutex
	tunnels  map[int]*Tunnel
	nodes    []Node
	fetched  time.Time
	workDir  string
	maxSlots int
	jobs     JobStore
}

func NewManager(maxSlots int, workDir string) *Manager {
	return &Manager{
		tunnels:  map[int]*Tunnel{},
		workDir:  workDir,
		maxSlots: maxSlots,
	}
}

// RefreshNodes 获取节点列表并同步更新已有隧道的元数据
func (m *Manager) RefreshNodes() (int, error) {
	nodes, err := fetchNodes(60 * time.Second)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.nodes = nodes
	m.fetched = time.Now()

	// 自动更新已有隧道的 Ping 和 Speed 等元数据
	nodeMap := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		nodeMap[n.HostName] = n
	}
	for _, t := range m.tunnels {
		if match, ok := nodeMap[t.Node.HostName]; ok {
			if match.Ping > 0 {
				t.Node.Ping = match.Ping
			}
			if match.SpeedMbps > 0 {
				t.Node.SpeedMbps = match.SpeedMbps
			}
		}
	}
	m.mu.Unlock()

	return len(nodes), nil
}

func (m *Manager) Nodes() ([]Node, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Node, len(m.nodes))
	copy(out, m.nodes)
	return out, m.fetched
}

func (m *Manager) Tunnels() []*Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// freeSlot 找一个未占用的槽位
func (m *Manager) freeSlot() (int, error) {
	for i := 1; i <= m.maxSlots; i++ {
		if _, used := m.tunnels[i]; !used {
			return i, nil
		}
	}
	return 0, fmt.Errorf("槽位已满 (最多 %d 条)", m.maxSlots)
}

// Start 为指定节点开启一条隧道
func (m *Manager) Start(node Node) (*Tunnel, error) {
	m.mu.Lock()
	slot, err := m.freeSlot()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	taken := map[int]bool{}
	for _, other := range m.tunnels {
		taken[other.Port] = true
	}
	port, err := freeRandomPort(taken)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	cred, err := newSocksCred()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	t := &Tunnel{
		Slot:   slot,
		Port:   port,
		Node:   node,
		Kind:   node.Kind,
		IPType: node.IPType,
		ISP:    node.ISP,
		Status: "starting",
		Since:  time.Now(),
		Cred:   cred,
	}
	if t.Kind == "" {
		t.Kind = "vpngate"
	}
	if t.IPType == "" {
		t.IPType = "residential"
	}
	m.tunnels[slot] = t
	m.mu.Unlock()

	go m.bringUp(t, true)
	return t, nil
}

func (m *Manager) bringUp(t *Tunnel, notify bool) {
	m.bringUpPersist(t, notify, false)
}

const (
	reconnectBackoffMin = 5 * time.Second
	reconnectBackoffMax = 60 * time.Second
)

func (m *Manager) bringUpPersist(t *Tunnel, notify bool, persist bool) {
	backoff := reconnectBackoffMin
	for {
		if m.tryCandidates(t, notify) {
			return
		}
		if !persist || !m.tunnelActive(t) {
			if persist {
				return
			}
			t.Status = "failed"
			if serr := m.saveState(); serr != nil {
				log.Printf("保存状态失败: %v", serr)
			}
			return
		}

		t.Status = "starting"
		t.Err = fmt.Sprintf("暂无可用节点，%.0f 秒后重试", backoff.Seconds())
		log.Printf("槽位 %d 候选节点尝试失败，%.0f 秒后刷新重试", t.Slot, backoff.Seconds())
		time.Sleep(backoff)
		if !m.tunnelActive(t) {
			return
		}
		if _, err := m.RefreshNodes(); err != nil {
			log.Printf("刷新节点列表失败: %v", err)
		}
		if backoff < reconnectBackoffMax {
			backoff *= 2
			if backoff > reconnectBackoffMax {
				backoff = reconnectBackoffMax
			}
		}
	}
}

func (m *Manager) tryCandidates(t *Tunnel, notify bool) bool {
	candidates := m.candidatesFor(t.Node)
	for i, node := range candidates {
		if !m.tunnelActive(t) {
			return false
		}
		if i > 0 && m.nodeInUse(node.HostName, t.Slot) {
			continue
		}
		oldHost := t.Node.HostName
		t.Node = node
		t.Kind = node.Kind
		if t.Kind == "custom" {
			t.CustomHost = node.IP
			t.CustomPort = node.Port
			t.CustomUser = node.User
			t.CustomPass = node.Pass
		}
		t.Status = "starting"
		if i > 0 {
			t.Err = fmt.Sprintf("正在尝试第 %d/%d 个候选节点 (%s)...", i+1, len(candidates), node.IP)
		}

		err := m.tryNode(t)
		if err == nil {
			t.Status = "up"
			t.Err = ""
			if t.Node.HostName != oldHost {
				t.recordHost(oldHost)
				_ = m.rebind(oldHost, t)
			}
			if serr := m.saveState(); serr != nil {
				log.Printf("保存状态失败: %v", serr)
			}
			if notify {
				m.notifyPanel()
			}
			return true
		}
		if t.Kind != "custom" {
			t.teardownNetns()
		}
	}
	return false
}

func (m *Manager) tunnelActive(t *Tunnel) bool {
	if t.Status == "stopped" {
		return false
	}
	m.mu.RLock()
	cur, ok := m.tunnels[t.Slot]
	m.mu.RUnlock()
	return ok && cur == t
}

func (m *Manager) tryNode(t *Tunnel) error {
	if t.Kind == "custom" {
		t.CustomHost = t.Node.IP
		t.CustomPort = t.Node.Port
		t.CustomUser = t.Node.User
		t.CustomPass = t.Node.Pass
		return t.startCustom()
	}
	if err := t.setupNetns(); err != nil {
		return err
	}
	if err := t.startOpenVPN(m.workDir); err != nil {
		return err
	}
	if t.listener == nil {
		if err := t.serve(); err != nil {
			return err
		}
	}
	ip, err := t.probeExitIP()
	if err != nil {
		return err
	}
	t.ExitIP = ip
	return nil
}

func (m *Manager) candidatesFor(first Node) []Node {
	const maxTries = 30
	m.mu.RLock()
	defer m.mu.RUnlock()

	used := map[string]bool{first.HostName: true}
	for _, t := range m.tunnels {
		used[t.Node.HostName] = true
	}

	allNodes := m.GetAllCandidateNodes("all")
	out := []Node{first}

	// 1. 同源 / 同地区 候选查找
	for _, n := range allNodes {
		if len(out) >= maxTries {
			break
		}
		if used[n.HostName] {
			continue
		}
		// 如果指定了自定义源，严格限定在同一个源内轮询
		if first.SourceID != "" {
			if n.SourceID != first.SourceID {
				continue
			}
		} else if first.CountryCode != "" && !strings.EqualFold(first.CountryCode, "ALL") && !strings.EqualFold(first.CountryCode, "CUSTOM") {
			// 如果指定了具体国家，在相同国家内轮询
			if !strings.EqualFold(n.CountryCode, first.CountryCode) {
				continue
			}
		}
		out = append(out, n)
	}

	// 2. 如果是不限地区/全部池，并且同源没填满，可继续将其他可用节点加入候选列表
	if (first.CountryCode == "" || strings.EqualFold(first.CountryCode, "ALL") || strings.EqualFold(first.CountryCode, "CUSTOM")) && first.SourceID == "" {
		for _, n := range allNodes {
			if len(out) >= maxTries {
				break
			}
			if used[n.HostName] {
				continue
			}
			alreadyIn := false
			for _, o := range out {
				if o.HostName == n.HostName {
					alreadyIn = true
					break
				}
			}
			if !alreadyIn {
				out = append(out, n)
			}
		}
	}

	return out
}

func (m *Manager) Stop(slot int) error {
	m.mu.Lock()
	t, ok := m.tunnels[slot]
	if ok {
		delete(m.tunnels, slot)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有在运行", slot)
	}
	t.stop()
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	// 级联清理绑定在此出口上的所有分流分支（包括当前节点与所有历史更换过的节点）
	allHosts := append([]string{t.Node.HostName}, t.HistoryHosts...)
	m.cleanupBoundBranches(allHosts...)
	invalidateInbounds()
	m.notifyPanel()
	return nil
}

func (m *Manager) Swap(slot int) error {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有在运行", slot)
	}
	poolType := t.IPType
	if poolType == "" {
		poolType = "residential"
	}
	var region string
	if t.Node.SourceID != "" {
		region = "SRC:" + t.Node.SourceID
	} else {
		region = t.Node.CountryCode
	}
	picks, err := m.pickNodes(region, poolType, 1)
	if err != nil {
		picks, err = m.pickNodes(t.Node.CountryCode, poolType, 1)
		if err != nil {
			return err
		}
	}
	oldHost := t.Node.HostName
	t.recordHost(oldHost)
	t.Node = picks[0]
	if t.Kind == "custom" {
		t.CustomHost = picks[0].IP
		t.CustomPort = picks[0].Port
		t.CustomUser = picks[0].User
		t.CustomPass = picks[0].Pass
	}
	m.reconnect(t, oldHost)
	return nil
}

func (m *Manager) StopAll() {
	for _, t := range m.Tunnels() {
		_ = m.Stop(t.Slot)
	}
}

func (m *Manager) SetCred(slot int, cred SocksCred) (SocksCred, error) {
	c, _, err := m.UpdateTunnelConfig(slot, cred, 0)
	return c, err
}

func (m *Manager) UpdateTunnelConfig(slot int, cred SocksCred, newPort int) (SocksCred, int, error) {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return SocksCred{}, 0, fmt.Errorf("槽位 %d 没有在运行", slot)
	}

	if cred.User == "" && cred.Pass == "" {
		gen, err := newSocksCred()
		if err != nil {
			return SocksCred{}, 0, err
		}
		cred = gen
	}
	if err := validateCred(cred); err != nil {
		return SocksCred{}, 0, err
	}

	if newPort > 0 && newPort != t.Port {
		if newPort < 1 || newPort > 65535 {
			return SocksCred{}, 0, fmt.Errorf("端口 %d 无效", newPort)
		}
		m.mu.RLock()
		for s, ot := range m.tunnels {
			if s != slot && ot.Port == newPort {
				m.mu.RUnlock()
				return SocksCred{}, 0, fmt.Errorf("端口 %d 已被槽位 %d 占用", newPort, s)
			}
		}
		m.mu.RUnlock()

		if err := t.switchPort(newPort); err != nil {
			return SocksCred{}, 0, err
		}
	}

	t.setCredential(cred)
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	m.syncCred(t)
	return cred, t.Port, nil
}

// AddCustomExit 为自定义 SOCKS5 节点创建并启动出口隧道
func (m *Manager) AddCustomExit(node CustomNode) (*Tunnel, error) {
	m.mu.Lock()
	slot, err := m.freeSlot()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	taken := map[int]bool{}
	for _, other := range m.tunnels {
		taken[other.Port] = true
	}
	port, err := freeRandomPort(taken)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	cred, err := newSocksCred()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if node.HostName == "" {
		node.HostName = fmt.Sprintf("custom-%s-%d", node.Host, node.Port)
	}
	if node.Country == "" {
		node.Country = "自定义"
	}
	if node.CountryCode == "" {
		node.CountryCode = "CUSTOM"
	}
	ipType := node.IPType
	if ipType == "" {
		ipType = "residential"
	}
	t := &Tunnel{
		Slot:       slot,
		Port:       port,
		Kind:       "custom",
		IPType:     ipType,
		ISP:        node.ISP,
		CustomHost: node.Host,
		CustomPort: node.Port,
		CustomUser: node.User,
		CustomPass: node.Pass,
		Status:     "starting",
		Since:      time.Now(),
		Cred:       cred,
		ExitIP:     node.ExitIP,
		Node: Node{
			HostName:    node.HostName,
			IP:          node.Host,
			Country:     node.Country,
			CountryCode: node.CountryCode,
			Ping:        node.Ping,
			SpeedMbps:   node.SpeedMbps,
			IPType:      ipType,
			ISP:         node.ISP,
			Kind:        "custom",
			SourceID:    node.SourceID,
		},
	}
	m.tunnels[slot] = t
	m.mu.Unlock()

	go m.bringUp(t, true)
	return t, nil
}

func (m *Manager) ReconcileOutbounds() {
	p, err := openPanel()
	if err != nil || p.Kind() != "3x-ui" {
		return
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		tunnels := m.Tunnels()
		if len(tunnels) == 0 {
			return
		}
		var up *Tunnel
		settled := true
		for _, t := range tunnels {
			if t.Status == "up" && up == nil {
				up = t
			}
			if t.Status == "starting" {
				settled = false
			}
		}
		if (settled || time.Now().After(deadline)) && up != nil {
			if err := m.resync(up); err != nil {
				log.Printf("同步出站失败: %v", err)
			}
			return
		}
		if settled || time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func (m *Manager) syncCred(t *Tunnel) {
	if err := m.resync(t); err != nil {
		log.Printf("同步 SOCKS5 凭据到节点对接后端失败: %v", err)
	}
}

func (m *Manager) Shutdown() {
	for _, t := range m.Tunnels() {
		t.stop()
	}
}

func prepareHost() error {
	if err := exec.Command("sysctl", "-qw", "net.ipv4.ip_forward=1").Run(); err != nil {
		return fmt.Errorf("设置 ip_forward 失败: %w", err)
	}
	return nil
}

func (m *Manager) nodeInUse(host string, exceptSlot int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for slot, t := range m.tunnels {
		if slot != exceptSlot && t.Node.HostName == host {
			return true
		}
	}
	return false
}

func (m *Manager) rebind(oldHost string, t *Tunnel) error {
	x, err := openPanel()
	if err != nil {
		return nil
	}
	return x.Rebind(oldHost, t, m.Tunnels())
}

func (m *Manager) resync(t *Tunnel) error {
	x, err := openPanel()
	if err != nil {
		return nil
	}
	return x.ResyncOutbound(t, m.Tunnels())
}

func (m *Manager) cleanupBoundBranches(hosts ...string) {
	if len(hosts) == 0 {
		return
	}
	p, err := openPanel()
	if err != nil {
		return
	}
	for _, h := range hosts {
		if h != "" {
			_ = p.DeleteBranchesByHost(h, m.Tunnels())
		}
	}
}

func (m *Manager) notifyPanel() {
	p, err := openPanel()
	if err != nil {
		return
	}
	if err := p.OnTunnelsChanged(m.Tunnels()); err != nil {
		log.Printf("同步节点对接后端失败: %v", err)
	}
}
