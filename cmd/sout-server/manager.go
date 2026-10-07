package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	vpngateMu       sync.RWMutex
	vpngateDisabled bool
	vpngatePath     string
)

func initVPNGateToggle(dir string) {
	vpngateMu.Lock()
	defer vpngateMu.Unlock()
	vpngatePath = filepath.Join(dir, "vpngate_disabled")
	if _, err := os.Stat(vpngatePath); err == nil {
		vpngateDisabled = true
	} else {
		vpngateDisabled = false
	}
}

func isVPNGateEnabled() bool {
	vpngateMu.RLock()
	defer vpngateMu.RUnlock()
	return !vpngateDisabled
}

func setVPNGateEnabled(enabled bool) error {
	vpngateMu.Lock()
	defer vpngateMu.Unlock()
	vpngateDisabled = !enabled
	if vpngatePath == "" {
		return nil
	}
	if !enabled {
		return os.WriteFile(vpngatePath, []byte("disabled\n"), 0644)
	}
	_ = os.Remove(vpngatePath)
	return nil
}

// Manager 维护隧道槽位与状态
type Manager struct {
	mu       sync.RWMutex
	tunnels  map[int]*Tunnel
	nodes    []Node
	fetched  time.Time
	workDir  string
	maxSlots int
	jobs     JobStore
	engine            *embeddedEngine
	chainUpstreamSlot int

	syncMu       sync.Mutex
	syncedExitIP map[int]string // 记录已同步过 outbounds 的出口状态（slot -> exitIP/status）
}

func NewManager(maxSlots int, workDir string) *Manager {
	initVPNGateToggle(workDir)
	engine, err := newEmbeddedEngine("127.0.0.1")
	if err != nil {
		log.Printf("初始化内嵌 sing-box 引擎警告: %v", err)
	}

	// 优先预载本地持久化节点缓存，杜绝服务启动/刷新期间出现「源不存在/暂无节点」的空窗期
	var initNodes []Node
	var initFetched time.Time
	cachePath := filepath.Join(workDir, "vpngate_cache.json")
	if blob, rerr := os.ReadFile(cachePath); rerr == nil {
		var cached []Node
		if jerr := json.Unmarshal(blob, &cached); jerr == nil && len(cached) > 0 {
			initNodes = cached
			initFetched = time.Now()
			log.Printf("VPN Gate 官方源启动即载入本地缓存 %d 个节点 (0毫秒就绪)", len(cached))
		}
	}

	return &Manager{
		tunnels:      map[int]*Tunnel{},
		nodes:        initNodes,
		fetched:      initFetched,
		workDir:      workDir,
		maxSlots:     maxSlots,
		engine:       engine,
		syncedExitIP: map[int]string{},
	}
}


// RefreshNodes 获取节点列表并同步更新已有隧道的元数据
func (m *Manager) RefreshNodes() (int, error) {
	cachePath := filepath.Join(m.workDir, "vpngate_cache.json")
	nodes, err := fetchNodes(60 * time.Second)
	if err != nil {
		// 网络拉取失败，尝试使用本地持久化缓存
		if blob, rerr := os.ReadFile(cachePath); rerr == nil {
			var cached []Node
			if jerr := json.Unmarshal(blob, &cached); jerr == nil && len(cached) > 0 {
				m.mu.Lock()
				m.nodes = cached
				m.fetched = time.Now()
				m.mu.Unlock()
				log.Printf("VPN Gate 官方源从本地缓存恢复 %d 个节点 (网络拉取重试中: %v)", len(cached), err)
				return len(cached), nil
			}
		}
		return 0, err
	}

	// 拉取成功，异步写入本地缓存以备无网/超时恢复
	if blob, merr := json.Marshal(nodes); merr == nil {
		_ = os.WriteFile(cachePath, blob, 0644)
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

func (m *Manager) ensureNodesLoadedLocked() {
	if len(m.nodes) > 0 {
		return
	}
	cachePath := filepath.Join(m.workDir, "vpngate_cache.json")
	if blob, rerr := os.ReadFile(cachePath); rerr == nil {
		var cached []Node
		if jerr := json.Unmarshal(blob, &cached); jerr == nil && len(cached) > 0 {
			m.nodes = cached
			m.fetched = time.Now()
			log.Printf("VPN Gate 官方源自动从本地缓存应急载入 %d 个节点", len(cached))
		}
	}
}

func (m *Manager) Nodes() ([]Node, time.Time) {
	m.mu.Lock()
	m.ensureNodesLoadedLocked()
	out := make([]Node, len(m.nodes))
	copy(out, m.nodes)
	fetched := m.fetched
	m.mu.Unlock()
	return out, fetched
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
		Slot:           slot,
		Port:           port,
		Node:           node,
		Kind:           node.Kind,
		IPType:         node.IPType,
		TargetPoolType: node.IPType,
		TargetRegion:   node.CountryCode,
		TargetSourceID: node.SourceID,
		ISP:            node.ISP,
		Status:         "starting",
		Since:          time.Now(),
		Cred:           cred,
	}
	if node.Kind == "custom" || (node.Protocol != "" && node.Protocol != "openvpn") {
		t.Kind = "custom"
		t.CustomHost = node.IP
		if t.CustomHost == "" {
			t.CustomHost = node.HostName
		}
		t.CustomPort = node.Port
		t.CustomUser = node.User
		t.CustomPass = node.Pass
		t.CustomProto = node.Protocol
	}
	if t.Kind == "" {
		t.Kind = "vpngate"
	}
	if t.Kind == "custom" {
		t.IPType = "datacenter"
	} else if t.IPType == "" {
		t.IPType = "residential"
	}

	if t.TargetPoolType == "" {
		t.TargetPoolType = t.IPType
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
	candidates := m.candidatesFor(t)
	for i, node := range candidates {
		if !m.tunnelActive(t) {
			return false
		}
		if i > 0 && m.nodeInUse(node.HostName, t.Slot) {
			continue
		}
		oldHost := t.Node.HostName
		t.Node = node
		if node.Kind != "" {
			t.Kind = node.Kind
		}
		if t.Kind == "" {
			if strings.HasPrefix(node.HostName, "cs-") || node.Protocol != "" {
				t.Kind = "custom"
			} else {
				t.Kind = "vpngate"
			}
		}
		if t.Kind == "custom" {
			t.CustomHost = node.IP
			t.CustomPort = node.Port
			t.CustomUser = node.User
			t.CustomPass = node.Pass
			t.CustomProto = node.Protocol
		}
		t.Status = "starting"
		if i > 0 {
			t.Err = fmt.Sprintf("正在尝试第 %d/%d 个候选节点 (%s, %s)...", i+1, len(candidates), node.IP, node.Country)
		}

		var err error
		maxRetries := 1
		if len(candidates) == 1 {
			maxRetries = 2
		}
		for try := 0; try < maxRetries; try++ {
			if !m.tunnelActive(t) {
				return false
			}
			if try > 0 {
				time.Sleep(2 * time.Second)
			}
			err = m.tryNode(t)
			if err == nil {
				break
			}
		}

		if err == nil {
			// 若出口仍在后台等待 OpenVPN 握手就绪，保持 starting，
			// 由就绪监控接手升级为 up 并同步出站；否则立即置 up。
			t.mu.Lock()
			if t.Status != "starting" {
				t.Status = "up"
			}
			t.Err = ""
			t.mu.Unlock()
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
		// 记录失败节点到历史中，避免死循环选中
		t.recordHost(node.HostName)
		log.Printf("槽位 %d 尝试候选节点 %d/%d: %s (%s, %s) 失败: %v", t.Slot, i+1, len(candidates), node.HostName, node.IP, node.CountryCode, err)
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
	t.setEngine(m.engine)
	if t.Kind == "custom" && t.Node.Protocol != "wireguard" && t.CustomProto != "wireguard" {
		t.CustomHost = t.Node.IP
		t.CustomPort = t.Node.Port
		t.CustomUser = t.Node.User
		t.CustomPass = t.Node.Pass
		t.CustomProto = t.Node.Protocol
		return t.startCustom()
	}
	if t.Kind == "custom" && (t.Node.Protocol == "wireguard" || t.CustomProto == "wireguard") {
		t.CustomProto = "wireguard"
	}
	return t.start(m.workDir)
}

func (m *Manager) candidatesFor(t *Tunnel) []Node {
	const maxTries = 30
	m.mu.RLock()
	defer m.mu.RUnlock()

	first := t.Node
	used := map[string]bool{first.HostName: true}
	for _, other := range m.tunnels {
		used[other.Node.HostName] = true
	}
	// 将历史失败节点也计入 used，优先挑选未尝试过的健康节点
	for _, h := range t.HistoryHosts {
		used[h] = true
	}

	poolType := t.TargetPoolType
	if poolType == "" {
		poolType = first.IPType
	}
	if poolType == "" {
		poolType = "all"
	}
	allNodes := m.getAllCandidateNodesLocked(poolType)
	out := []Node{first}

	region := t.TargetRegion
	if region == "" {
		if t.TargetSourceID != "" {
			region = "SRC:" + t.TargetSourceID
		} else {
			region = first.CountryCode
		}
	}

	expectedKind := t.Kind
	if expectedKind == "" {
		expectedKind = first.Kind
	}
	if expectedKind == "" {
		if strings.HasPrefix(first.HostName, "cs-") || first.Protocol != "" {
			expectedKind = "custom"
		} else {
			expectedKind = "vpngate"
		}
	}

	matchesFilter := func(n Node) bool {
		candidateKind := n.Kind
		if candidateKind == "" {
			if strings.HasPrefix(n.HostName, "cs-") || n.Protocol != "" {
				candidateKind = "custom"
			} else {
				candidateKind = "vpngate"
			}
		}
		if candidateKind != expectedKind {
			return false
		}

		if poolType != "all" && n.IPType != "" && n.IPType != poolType {
			return false
		}

		if strings.HasPrefix(region, "SRC:") {
			parts := strings.Split(region, ":")
			targetCat := parts[1]
			subFilter := ""
			if len(parts) >= 3 {
				subFilter = parts[2]
			}
			nodeCat := classifyNodeCategory(n)
			if !strings.EqualFold(targetCat, nodeCat) && n.SourceID != targetCat {
				return false
			}
			if !matchNodeSubRegion(n, subFilter) {
				return false
			}
			return true
		}

		if region != "" && !strings.EqualFold(region, "ALL") && !strings.EqualFold(region, "CUSTOM") {
			return strings.EqualFold(n.CountryCode, region)
		}

		return true
	}

	// 1. 从未尝试过的同区域/同源节点中挑选优质候选
	for _, n := range allNodes {
		if len(out) >= maxTries {
			break
		}
		if used[n.HostName] {
			continue
		}
		if !matchesFilter(n) {
			continue
		}
		out = append(out, n)
	}

	// 2. 如果候选不足（由于大量节点被 HistoryHosts 排除），放宽 HistoryHosts 限制重新补充候选
	if len(out) < 10 {
		for _, n := range allNodes {
			if len(out) >= maxTries {
				break
			}
			if n.HostName == first.HostName {
				continue
			}
			// 其他活跃槽位占用的节点绝对不能跨槽位抢占
			occupied := false
			for _, other := range m.tunnels {
				if other.Slot != t.Slot && other.Node.HostName == n.HostName {
					occupied = true
					break
				}
			}
			if occupied {
				continue
			}

			already := false
			for _, o := range out {
				if o.HostName == n.HostName {
					already = true
					break
				}
			}
			if already {
				continue
			}

			if !matchesFilter(n) {
				continue
			}
			out = append(out, n)
		}
	}

	return out
}

// ToggleChainUpstream 切换指定槽位出口为全局前置链式出站节点（全局单选互斥）
func (m *Manager) ToggleChainUpstream(slot int) (bool, error) {
	m.mu.Lock()
	t, ok := m.tunnels[slot]
	if !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("槽位 %d 的出口隧道不存在", slot)
	}

	var newUpstreamTag string
	var newState bool
	if t.IsChainUpstream {
		t.IsChainUpstream = false
		m.chainUpstreamSlot = 0
		newState = false
		newUpstreamTag = ""
	} else {
		for s, other := range m.tunnels {
			if s != slot {
				other.IsChainUpstream = false
			}
		}
		t.IsChainUpstream = true
		m.chainUpstreamSlot = slot
		newState = true
		isWG := t.CustomProto == "wireguard" || t.Node.Protocol == "wireguard"
		if isWG {
			newUpstreamTag = fmt.Sprintf("soutwireguard%d", slot)
		} else {
			newUpstreamTag = fmt.Sprintf("soutopenvpn%d", slot)
		}
	}
	m.mu.Unlock()

	// 通知内嵌引擎更新全局前置链标签并热重载受影响的隧道
	if m.engine != nil {
		m.engine.setChainUpstream(newUpstreamTag)
	}

	// 保存状态
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}

	// 关键补齐：通知主面板 (sing-box / s-ui) 同步出站并使基础直连路由 (route.final) 即时生效
	m.notifyPanel()

	return newState, nil
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
	if t.IsChainUpstream || m.chainUpstreamSlot == slot {
		m.chainUpstreamSlot = 0
		if m.engine != nil {
			m.engine.setChainUpstream("")
		}
	}
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	m.notifyPanel()

	// 级联清理：提取该隧道当前节点及所有历史绑定过的节点（包括主机名、IP、自定义Host、出口IP等别名），全量清理分流管理中的分支与路由规则
	hostSet := make(map[string]bool)
	addHost := func(h string) {
		h = strings.TrimSpace(h)
		if h != "" {
			hostSet[h] = true
		}
	}
	addHost(t.Node.HostName)
	addHost(t.Node.IP)
	addHost(t.CustomHost)
	addHost(t.ExitIP)
	for _, part := range strings.Split(t.Node.HostName, "-") {
		if net.ParseIP(part) != nil {
			addHost(part)
		}
	}
	for _, hh := range t.HistoryHosts {
		addHost(hh)
	}
	var hosts []string
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	go m.cleanupBoundBranches(hosts...)

	return nil
}

func (m *Manager) Swap(slot int) error {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有在运行", slot)
	}
	poolType := t.TargetPoolType
	if poolType == "" {
		poolType = t.IPType
	}
	if poolType == "" {
		poolType = "all"
	}
	region := t.TargetRegion
	if region == "" {
		if t.TargetSourceID != "" {
			region = "SRC:" + t.TargetSourceID
		} else {
			region = t.Node.CountryCode
		}
	}
	picks, err := m.pickNodes(region, poolType, 1)
	if err != nil {
		return err
	}
	oldHost := t.Node.HostName
	t.recordHost(oldHost)
	t.Node = picks[0]
	if t.Kind == "custom" {
		t.CustomHost = picks[0].IP
		t.CustomPort = picks[0].Port
		t.CustomUser = picks[0].User
		t.CustomPass = picks[0].Pass
		t.CustomProto = picks[0].Protocol
	}
	m.reconnect(t, oldHost)
	return nil
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
	ipType := "datacenter"

	t := &Tunnel{
		Slot:           slot,
		Port:           port,
		Kind:           "custom",
		IPType:         ipType,
		TargetPoolType: ipType,
		TargetRegion:   node.CountryCode,
		TargetSourceID: node.SourceID,
		ISP:            node.ISP,
		CustomHost:     node.Host,
		CustomPort:     node.Port,
		CustomUser:     node.User,
		CustomPass:     node.Pass,
		CustomProto:    node.Protocol,
		Status:         "starting",
		Since:          time.Now(),
		Cred:           cred,
		ExitIP:         node.ExitIP,
		Node: Node{
			HostName:    node.HostName,
			IP:          node.Host,
			Country:     node.Country,
			CountryCode: node.CountryCode,
			Ping:        node.Ping,
			SpeedMbps:   node.SpeedMbps,
			Config:      node.Config,
			IPType:      ipType,
			ISP:         node.ISP,
			Kind:        "custom",
			Port:        node.Port,
			User:        node.User,
			Pass:        node.Pass,
			Protocol:    node.Protocol,
			Remark:      node.Remark,
			SourceID:    node.SourceID,
		},
	}
	t.setEngine(m.engine)
	m.tunnels[slot] = t
	m.mu.Unlock()

	go m.bringUp(t, true)
	return t, nil
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
	if m.engine != nil {
		_ = m.engine.close()
	}
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
	p, err := openPanel()
	if err != nil {
		return
	}
	for _, h := range hosts {
		if h != "" {
			_ = p.DeleteBranchesByHost(h, m.Tunnels())
		}
	}
	_ = p.OnTunnelsChanged(m.Tunnels())
	invalidateInbounds()
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

// WatchExitReady 观察后台等待就绪的出口：一旦从 starting 升为 up，
// 立即把出站同步进面板配置，避免"出口已就绪但分流规则仍指向空出站"。
func (m *Manager) WatchExitReady() {
	for range time.Tick(5 * time.Second) {
		for _, t := range m.Tunnels() {
			if t.Status != "up" {
				continue
			}
			t.mu.Lock()
			key := t.ExitIP
			if key == "" {
				key = "up"
			}
			t.mu.Unlock()

			m.syncMu.Lock()
			prev, seen := m.syncedExitIP[t.Slot]
			m.syncMu.Unlock()
			if seen && prev == key {
				continue
			}

			if err := m.resync(t); err != nil {
				log.Printf("出口槽位 %d 就绪后同步出站失败: %v", t.Slot, err)
				continue
			}
			m.syncMu.Lock()
			m.syncedExitIP[t.Slot] = key
			m.syncMu.Unlock()
			if err := m.saveState(); err != nil {
				log.Printf("保存状态失败: %v", err)
			}
			m.notifyPanel()
			log.Printf("出口槽位 %d (%s) 已就绪 (出口 IP %s)，分流出站已同步", t.Slot, t.Node.HostName, t.ExitIP)
		}
	}
}
