package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProvisionRequest 是"拉取 N 条出口"的请求参数
type ProvisionRequest struct {
	Region     string // 地区代码，留空或 "ALL" 表示不限地区
	Count      int
	TemplateID int    // 模板入站 ID；0 表示仅建出站
	PoolType   string // "all" | "residential" | "datacenter"
}

// GetAllCandidateNodes 获取指定池下的全部候选可用节点 (VPN Gate + 已启用的 SOCKS5 订阅源)
func (m *Manager) GetAllCandidateNodes(poolType string) []Node {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var nodes []Node

	// 1. VPN Gate 官方节点（全部属于家宽池）
	if poolType == "" || poolType == "all" || poolType == "residential" {
		for _, n := range m.nodes {
			n.Kind = "vpngate"
			n.IPType = "residential"
			nodes = append(nodes, n)
		}
	}

	// 2. 自定义 SOCKS5 订阅源中已启用的节点
	if globalCustomStore != nil {
		globalCustomStore.mu.RLock()
		for _, node := range globalCustomStore.Nodes {
			if node.SourceID != "" {
				src, ok := globalCustomStore.Sources[node.SourceID]
				if !ok || !src.Enabled {
					continue
				}
			}
			ipType := node.IPType
			if ipType == "" {
				ipType = "residential"
			}
			if poolType == "" || poolType == "all" || poolType == ipType {
				cCode := node.CountryCode
				if cCode == "" {
					cCode = "CUSTOM"
				}
				cName := node.Country
				if cName == "" {
					cName = "自定义S5"
				}
				nodes = append(nodes, Node{
					HostName:    node.HostName,
					IP:          node.Host,
					Country:     cName,
					CountryCode: cCode,
					Ping:        node.Ping,
					SpeedMbps:   node.SpeedMbps,
					IPType:      ipType,
					ISP:         node.ISP,
					Kind:        "custom",
					Port:        node.Port,
					User:        node.User,
					Pass:        node.Pass,
					Remark:      node.Remark,
					SourceID:    node.SourceID,
				})
			}
		}
		globalCustomStore.mu.RUnlock()
	}

	return nodes
}

// Provision 异步执行一组出站拉取，返回 Job 供前端查询进度
func (m *Manager) Provision(req ProvisionRequest) (*Job, error) {
	if req.Count < 1 {
		return nil, fmt.Errorf("拉取数量至少为 1")
	}
	poolType := req.PoolType
	if poolType == "" {
		poolType = "all"
	}
	picks, err := m.pickNodes(req.Region, poolType, req.Count)
	if err != nil {
		return nil, err
	}

	labels := make([]string, 0, len(picks)+1)
	for _, n := range picks {
		labels = append(labels, regionLabel(n)+" 出口")
	}
	if req.TemplateID > 0 {
		labels = append(labels, "绑定节点")
	}

	where := req.Region
	if where == "" || strings.EqualFold(where, "ALL") {
		where = "不限地区"
	} else if where == "SRC:builtin-vpngate" {
		where = "VPN Gate 官方源"
	} else if strings.HasPrefix(where, "SRC:") {
		srcID := strings.TrimPrefix(where, "SRC:")
		if globalCustomStore != nil {
			globalCustomStore.mu.RLock()
			if s, ok := globalCustomStore.Sources[srcID]; ok {
				where = s.Name
			}
			globalCustomStore.mu.RUnlock()
		}
	}
	poolLabel := ""
	if poolType == "residential" {
		poolLabel = " (家宽池)"
	} else if poolType == "datacenter" {
		poolLabel = " (机房池)"
	}
	job := m.jobs.New(fmt.Sprintf("拉取 %d 条 %s 出口%s", len(picks), where, poolLabel), labels)

	go m.runProvision(job, picks, req.Region, poolType, req.TemplateID)
	return job, nil
}

func (m *Manager) runProvision(job *Job, picks []Node, region, poolType string, templateID int) {
	defer job.Finish()

	var wg sync.WaitGroup
	started := make([]*Tunnel, len(picks))

	for i, node := range picks {
		var t *Tunnel
		var err error
		if node.Kind == "custom" {
			cNode := CustomNode{
				HostName:    node.HostName,
				Host:        node.IP,
				Port:        node.Port,
				User:        node.User,
				Pass:        node.Pass,
				Country:     node.Country,
				CountryCode: node.CountryCode,
				Remark:      node.Remark,
				Ping:        node.Ping,
				SpeedMbps:   node.SpeedMbps,
				IPType:      node.IPType,
				ISP:         node.ISP,
				SourceID:    node.SourceID,
			}
			t, err = m.AddCustomExit(cNode)
		} else {
			t, err = m.Start(node)
		}

		if err != nil {
			job.Set(i, "failed", err.Error())
			continue
		}
		t.TargetPoolType = poolType
		t.TargetRegion = region
		if strings.HasPrefix(region, "SRC:") {
			t.TargetSourceID = strings.TrimPrefix(region, "SRC:")
		}
		started[i] = t
		job.Set(i, "running", "连接 "+node.HostName)

		wg.Add(1)
		go func(i int, t *Tunnel) {
			defer wg.Done()
			m.waitUp(t)
			if t.Status == "up" {
				job.Set(i, "ok", t.ExitIP)
				return
			}
			job.Set(i, "failed", firstLine(t.Err))
		}(i, t)
	}
	wg.Wait()

	if templateID <= 0 {
		return
	}

	step := len(picks)
	var hosts []string
	for _, t := range started {
		if t != nil && t.Status == "up" {
			hosts = append(hosts, t.Node.HostName)
		}
	}
	if len(hosts) == 0 {
		job.Set(step, "failed", "没有连通的出口，已跳过绑定")
		return
	}

	x, err := openPanel()
	if err != nil {
		job.Set(step, "failed", err.Error())
		return
	}
	ports, err := x.CloneToTunnels(templateID, hosts, m.Tunnels())
	invalidateInbounds()
	if err != nil {
		job.Set(step, "failed", firstLine(err.Error()))
		return
	}
	job.Set(step, "ok", fmt.Sprintf("已创建 %d 个分流入站", len(ports)))
}

func (m *Manager) waitUp(t *Tunnel) {
	const maxWait = 5 * time.Minute
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if t.Status == "up" || t.Status == "failed" || t.Status == "stopped" {
			return
		}
		time.Sleep(time.Second)
	}
}

// pickNodes 挑选 count 个未被占用的节点，按速度与质量降序选取
func (m *Manager) pickNodes(region, poolType string, count int) ([]Node, error) {
	candidateNodes := m.GetAllCandidateNodes(poolType)

	m.mu.RLock()
	used := map[string]bool{}
	for _, t := range m.tunnels {
		used[t.Node.HostName] = true
	}
	m.mu.RUnlock()

	var out []Node
	for _, n := range candidateNodes {
		if len(out) >= count {
			break
		}
		if used[n.HostName] {
			continue
		}
		if region == "SRC:builtin-vpngate" {
			if n.Kind == "custom" || (n.SourceID != "" && n.SourceID != "builtin-vpngate") {
				continue
			}
		} else if strings.HasPrefix(region, "SRC:") {
			srcID := strings.TrimPrefix(region, "SRC:")
			if n.SourceID != srcID {
				continue
			}
		} else if region != "" && !strings.EqualFold(region, "ALL") && !strings.EqualFold(n.CountryCode, region) {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		if region != "" && !strings.EqualFold(region, "ALL") {
			return nil, fmt.Errorf("%s 没有可用的空闲节点", region)
		}
		return nil, fmt.Errorf("没有可用的空闲节点，请稍后刷新列表")
	}
	return out, nil
}

// RegionStat 每个目标地区的可用节点概况
type RegionStat struct {
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	Available int     `json:"available"`
	BestPing  int     `json:"best_ping"`
	BestSpeed float64 `json:"best_speed_mbps"`
}

// Regions 返回当前指定节点池下所有可用地区的聚合统计，按可用数降序
func (m *Manager) Regions(poolType string) []RegionStat {
	candidateNodes := m.GetAllCandidateNodes(poolType)

	m.mu.RLock()
	used := map[string]bool{}
	for _, t := range m.tunnels {
		used[t.Node.HostName] = true
	}
	m.mu.RUnlock()

	// 1. 按具体国家/地区聚合 (忽略占位的 CUSTOM)
	byCode := map[string]*RegionStat{}
	for _, n := range candidateNodes {
		if used[n.HostName] || n.CountryCode == "" || n.CountryCode == "CUSTOM" {
			continue
		}
		s := byCode[n.CountryCode]
		if s == nil {
			s = &RegionStat{Code: n.CountryCode, Name: n.Country, BestPing: n.Ping}
			byCode[n.CountryCode] = s
		}
		s.Available++
		if n.SpeedMbps > s.BestSpeed {
			s.BestSpeed = n.SpeedMbps
		}
		if n.Ping > 0 && (s.BestPing == 0 || n.Ping < s.BestPing) {
			s.BestPing = n.Ping
		}
	}

	out := make([]RegionStat, 0, len(byCode))
	for _, s := range byCode {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Available != out[j].Available {
			return out[i].Available > out[j].Available
		}
		return out[i].Code < out[j].Code
	})

	var sourceStats []RegionStat

	// 2. 按用户添加的自定义源生成专属选项卡片（如 hookzof、proxifly 等）
	if globalCustomStore != nil {
		globalCustomStore.mu.RLock()
		for _, src := range globalCustomStore.Sources {
			if !src.Enabled {
				continue
			}
			srcAvail := 0
			bestPing := 0
			bestSpeed := 0.0
			for _, n := range candidateNodes {
				if used[n.HostName] || n.SourceID != src.ID {
					continue
				}
				srcAvail++
				if n.SpeedMbps > bestSpeed {
					bestSpeed = n.SpeedMbps
				}
				if n.Ping > 0 && (bestPing == 0 || n.Ping < bestPing) {
					bestPing = n.Ping
				}
			}
			if srcAvail > 0 {
				srcStat := RegionStat{
					Code:      "SRC:" + src.ID,
					Name:      src.Name,
					Available: srcAvail,
					BestPing:  bestPing,
					BestSpeed: bestSpeed,
				}
				sourceStats = append(sourceStats, srcStat)
			}
		}
		globalCustomStore.mu.RUnlock()
	}

	// 3. 官方内置源：VPN Gate 官方全球家宽源
	vpngateAvail := 0
	vpngateBestPing := 0
	vpngateBestSpeed := 0.0
	for _, n := range candidateNodes {
		if used[n.HostName] || n.Kind == "custom" || (n.SourceID != "" && n.SourceID != "builtin-vpngate") {
			continue
		}
		vpngateAvail++
		if n.SpeedMbps > vpngateBestSpeed {
			vpngateBestSpeed = n.SpeedMbps
		}
		if n.Ping > 0 && (vpngateBestPing == 0 || n.Ping < vpngateBestPing) {
			vpngateBestPing = n.Ping
		}
	}
	if vpngateAvail > 0 {
		srcStat := RegionStat{
			Code:      "SRC:builtin-vpngate",
			Name:      "VPN Gate (官方源)",
			Available: vpngateAvail,
			BestPing:  vpngateBestPing,
			BestSpeed: vpngateBestSpeed,
		}
		sourceStats = append([]RegionStat{srcStat}, sourceStats...)
	}

	// 4. 计算全局「不限地区 (ALL)」真实可用总数与最高速度
	allAvail := 0
	allBestPing := 0
	allBestSpeed := 0.0
	for _, n := range candidateNodes {
		if used[n.HostName] {
			continue
		}
		allAvail++
		if n.SpeedMbps > allBestSpeed {
			allBestSpeed = n.SpeedMbps
		}
		if n.Ping > 0 && (allBestPing == 0 || n.Ping < allBestPing) {
			allBestPing = n.Ping
		}
	}
	allStat := RegionStat{
		Code:          "ALL",
		Name:          "不限地区",
		Available:     allAvail,
		BestPing:      allBestPing,
		BestSpeed:     allBestSpeed,
	}

	var result []RegionStat
	if allAvail > 0 {
		result = append(result, allStat)
	}
	result = append(result, sourceStats...)
	result = append(result, out...)
	return result
}

func regionLabel(n Node) string {
	if n.CountryCode != "" {
		return n.CountryCode
	}
	return n.HostName
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
