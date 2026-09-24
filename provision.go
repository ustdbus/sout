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

// GetAllCandidateNodes 获取指定池下的全部候选可用节点 (VPN Gate + 已启用的自定义订阅源)
func (m *Manager) GetAllCandidateNodes(poolType string) []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getAllCandidateNodesLocked(poolType)
}

func (m *Manager) getAllCandidateNodesLocked(poolType string) []Node {
	var nodes []Node

	// 1. VPN Gate 官方节点（全部属于家宽池）
	if isVPNGateEnabled() && (poolType == "" || poolType == "all" || poolType == "residential") {
		m.ensureNodesLoadedLocked()
		for _, n := range m.nodes {
			n.Kind = "vpngate"
			n.IPType = "residential"
			nodes = append(nodes, n)
		}
	}


	// 2. 自定义订阅源中已启用的节点
	if globalCustomStore != nil {
		globalCustomStore.mu.RLock()
		for _, node := range globalCustomStore.Nodes {
			if node.SourceID != "" {
				src, ok := globalCustomStore.Sources[node.SourceID]
				if !ok || !src.Enabled {
					continue
				}
			}
			ipType := "datacenter"
			if node.IPType != "" {
				ipType = node.IPType
			}
			if ipType != "datacenter" {
				ipType = "datacenter"
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
					Protocol:    node.Protocol,
					Remark:      node.Remark,
					SourceID:    node.SourceID,
					Config:      node.Config,
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
				Protocol:    node.Protocol,
				Country:     node.Country,
				CountryCode: node.CountryCode,
				Remark:      node.Remark,
				Ping:        node.Ping,
				SpeedMbps:   node.SpeedMbps,
				IPType:      node.IPType,
				ISP:         node.ISP,
				SourceID:    node.SourceID,
				Config:      node.Config,
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

func classifyNodeCategory(n Node) string {
	remLower := strings.ToLower(n.Remark)
	hostLower := strings.ToLower(n.IP)
	protoLower := strings.ToLower(n.Protocol)
	if n.Kind == "vpngate" || strings.Contains(n.SourceID, "vpngate") {
		return "vpngate"
	}
	if strings.Contains(remLower, "proton") || strings.Contains(hostLower, "proton") || strings.Contains(n.SourceID, "proton") {
		return "proton"
	}
	if strings.Contains(remLower, "ws-") || strings.Contains(remLower, "windscribe") || strings.Contains(hostLower, "totallyacdn") || strings.Contains(n.SourceID, "windscribe") {
		return "windscribe"
	}
	if strings.Contains(remLower, "opera") || strings.Contains(hostLower, "opera") || strings.Contains(n.SourceID, "opera") {
		return "opera"
	}
	if strings.Contains(remLower, "warp") || strings.Contains(hostLower, "cloudflare") || (protoLower == "wireguard" && !strings.Contains(remLower, "proton")) {
		return "warp"
	}
	return "custom"
}

func matchNodeSubRegion(n Node, sub string) bool {
	if sub == "" || strings.EqualFold(sub, "ALL") {
		return true
	}
	subLower := strings.ToLower(sub)
	remLower := strings.ToLower(n.Remark)
	cLower := strings.ToLower(n.Country)
	ccLower := strings.ToLower(n.CountryCode)
	hostLower := strings.ToLower(n.HostName + " " + n.IP)

	// 精确城市别名与映射识别
	switch subLower {
	case "洛杉矶", "los angeles":
		return strings.Contains(remLower, "洛杉矶") || strings.Contains(remLower, "los angeles") ||
			strings.Contains(hostLower, "us-west-094") || strings.Contains(hostLower, "us-west-095")
	case "西雅图", "seattle":
		return strings.Contains(remLower, "西雅图") || strings.Contains(remLower, "seattle") ||
			strings.Contains(hostLower, "us-west-084")
	case "纽约", "new york":
		return strings.Contains(remLower, "纽约") || strings.Contains(remLower, "new york") ||
			strings.Contains(hostLower, "us-east")
	case "芝加哥/亚特兰大", "chicago", "atlanta", "美国中部":
		return strings.Contains(remLower, "美国中部") || strings.Contains(remLower, "central") ||
			strings.Contains(hostLower, "us-central")
	case "温哥华", "vancouver":
		return strings.Contains(remLower, "温哥华") || strings.Contains(remLower, "vancouver") ||
			strings.Contains(hostLower, "ca-west")
	case "多伦多", "toronto":
		return strings.Contains(remLower, "多伦多") || strings.Contains(remLower, "toronto") ||
			strings.Contains(hostLower, "ca-038") || strings.Contains(hostLower, "ca-058") || strings.Contains(hostLower, "ca-059")
	case "蒙特利尔", "montreal":
		return strings.Contains(remLower, "蒙特利尔") || strings.Contains(remLower, "montreal") ||
			strings.Contains(hostLower, "ca-065") || strings.Contains(hostLower, "ca-067")
	case "伦敦", "london":
		return strings.Contains(remLower, "伦敦") || strings.Contains(remLower, "london") ||
			strings.Contains(remLower, "英国") || strings.Contains(hostLower, "uk-")
	case "法兰克福", "frankfurt":
		return strings.Contains(remLower, "法兰克福") || strings.Contains(remLower, "frankfurt") ||
			strings.Contains(remLower, "德国") || strings.Contains(hostLower, "de-")
	case "巴黎", "paris":
		return strings.Contains(remLower, "巴黎") || strings.Contains(remLower, "paris") ||
			strings.Contains(remLower, "法国") || strings.Contains(hostLower, "fr-")
	case "阿姆斯特丹", "amsterdam":
		return strings.Contains(remLower, "阿姆斯特丹") || strings.Contains(remLower, "amsterdam") ||
			strings.Contains(remLower, "荷兰") || strings.Contains(hostLower, "nl-")
	case "苏黎世", "zurich":
		return strings.Contains(remLower, "苏黎世") || strings.Contains(remLower, "zurich") ||
			strings.Contains(remLower, "瑞士") || strings.Contains(hostLower, "ch-")
	case "奥斯陆", "oslo":
		return strings.Contains(remLower, "奥斯陆") || strings.Contains(remLower, "oslo") ||
			strings.Contains(remLower, "挪威") || strings.Contains(hostLower, "no-")
	case "布加勒斯特", "bucharest":
		return strings.Contains(remLower, "布加勒斯特") || strings.Contains(remLower, "bucharest") ||
			strings.Contains(remLower, "罗马尼亚") || strings.Contains(hostLower, "ro-")
	case "香港", "hong kong":
		return strings.Contains(remLower, "香港") || strings.Contains(remLower, "hong kong") ||
			strings.Contains(hostLower, "hk-")
	case "美国西部":
		return strings.Contains(remLower, "美国西部") || strings.Contains(hostLower, "us-west")
	case "美国东部":
		return strings.Contains(remLower, "美国东部") || strings.Contains(hostLower, "us-east")
	case "加拿大":
		return strings.Contains(remLower, "加拿大") || strings.Contains(hostLower, "ca-")
	}

	return strings.Contains(remLower, subLower) || strings.Contains(cLower, subLower) || strings.EqualFold(ccLower, subLower) || strings.Contains(hostLower, subLower)
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
		if strings.HasPrefix(region, "SRC:") {
			parts := strings.Split(region, ":")
			targetCat := parts[1]
			subFilter := ""
			if len(parts) >= 3 {
				subFilter = parts[2]
			}
			nodeCat := classifyNodeCategory(n)
			if !strings.EqualFold(targetCat, nodeCat) && n.SourceID != targetCat {
				continue
			}
			if !matchNodeSubRegion(n, subFilter) {
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
	Category  string  `json:"category"` // "vpngate" | "warp" | "windscribe" | "opera" | "proton" | "custom"
}

// Regions 返回当前指定节点池下所有可用地区的聚合统计，按 5 大专属源分类输出
func (m *Manager) Regions(poolType string) []RegionStat {
	candidateNodes := m.GetAllCandidateNodes(poolType)

	m.mu.RLock()
	used := map[string]bool{}
	for _, t := range m.tunnels {
		used[t.Node.HostName] = true
	}
	m.mu.RUnlock()

	var result []RegionStat

	// --- 1. VPN Gate 分类 ---
	vpngateNodes := make([]Node, 0)
	if isVPNGateEnabled() {
		for _, n := range candidateNodes {
			if !used[n.HostName] && classifyNodeCategory(n) == "vpngate" {
				vpngateNodes = append(vpngateNodes, n)
			}
		}
	}
	if len(vpngateNodes) > 0 {
		bestSpeed := 0.0
		for _, n := range vpngateNodes {
			if n.SpeedMbps > bestSpeed {
				bestSpeed = n.SpeedMbps
			}
		}
		result = append(result, RegionStat{
			Code:      "SRC:vpngate:ALL",
			Name:      "全球最高速 (VPN Gate)",
			Available: len(vpngateNodes),
			BestSpeed: bestSpeed,
			Category:  "vpngate",
		})
		vgByCode := map[string]*RegionStat{}
		for _, n := range vpngateNodes {
			if n.CountryCode == "" {
				continue
			}
			s := vgByCode[n.CountryCode]
			if s == nil {
				s = &RegionStat{
					Code:      "SRC:vpngate:" + n.CountryCode,
					Name:      fmt.Sprintf("%s %s", n.CountryCode, n.Country),
					BestPing:  n.Ping,
					Category:  "vpngate",
				}
				vgByCode[n.CountryCode] = s
			}
			s.Available++
			if n.SpeedMbps > s.BestSpeed {
				s.BestSpeed = n.SpeedMbps
			}
			if n.Ping > 0 && (s.BestPing == 0 || n.Ping < s.BestPing) {
				s.BestPing = n.Ping
			}
		}
		var vgList []RegionStat
		for _, s := range vgByCode {
			vgList = append(vgList, *s)
		}
		sort.Slice(vgList, func(i, j int) bool { return vgList[i].Available > vgList[j].Available })
		result = append(result, vgList...)
	}

	// --- 2. WARP 分类 ---
	warpNodes := make([]Node, 0)
	warpInUse := 0
	for _, n := range candidateNodes {
		if classifyNodeCategory(n) == "warp" {
			if !used[n.HostName] {
				warpNodes = append(warpNodes, n)
			} else {
				warpInUse++
			}
		}
	}
	if len(warpNodes) == 0 && warpInUse == 0 {
		m.mu.RLock()
		for _, t := range m.tunnels {
			if t.Status == "up" && (strings.Contains(strings.ToLower(t.Node.HostName), "warp") || strings.Contains(strings.ToLower(t.CustomHost), "cloudflare") || t.CustomProto == "wireguard") {
				warpInUse++
				break
			}
		}
		m.mu.RUnlock()
	}

	if len(warpNodes) > 0 {
		bestSpeed := 0.0
		for _, n := range warpNodes {
			if n.SpeedMbps > bestSpeed {
				bestSpeed = n.SpeedMbps
			}
		}
		result = append(result, RegionStat{
			Code:      "SRC:warp:ALL",
			Name:      "⚡ 全球就近出站 (Anycast)",
			Available: len(warpNodes),
			BestSpeed: bestSpeed,
			Category:  "warp",
		})
	} else if warpInUse > 0 {
		result = append(result, RegionStat{
			Code:      "SRC:warp:ALL",
			Name:      "⚡ 全球就近出站 (WARP 账号已就绪 · 出口运行中)",
			Available: 1,
			Category:  "warp",
		})
	}


	// --- 3. Windscribe 分类 (支持城市/大区精确专属页面) ---
	wsNodes := make([]Node, 0)
	for _, n := range candidateNodes {
		if !used[n.HostName] && classifyNodeCategory(n) == "windscribe" {
			wsNodes = append(wsNodes, n)
		}
	}
	if len(wsNodes) > 0 {
		result = append(result, RegionStat{
			Code:      "SRC:windscribe:ALL",
			Name:      "🌐 全部 Windscribe 节点",
			Available: len(wsNodes),
			Category:  "windscribe",
		})
		wsSubs := []struct {
			Filter string
			Name   string
		}{
			{Filter: "洛杉矶", Name: "🇺🇸 洛杉矶 (Los Angeles)"},
			{Filter: "西雅图", Name: "🇺🇸 西雅图 (Seattle)"},
			{Filter: "纽约", Name: "🇺🇸 纽约 (New York)"},
			{Filter: "芝加哥/亚特兰大", Name: "🇺🇸 芝加哥/亚特兰大 (Central)"},
			{Filter: "温哥华", Name: "🇨🇦 温哥华 (Vancouver)"},
			{Filter: "多伦多", Name: "🇨🇦 多伦多 (Toronto)"},
			{Filter: "蒙特利尔", Name: "🇨🇦 蒙特利尔 (Montreal)"},
			{Filter: "伦敦", Name: "🇬🇧 伦敦 (London)"},
			{Filter: "法兰克福", Name: "🇩🇪 法兰克福 (Frankfurt)"},
			{Filter: "巴黎", Name: "🇫🇷 巴黎 (Paris)"},
			{Filter: "阿姆斯特丹", Name: "🇳🇱 阿姆斯特丹 (Amsterdam)"},
			{Filter: "苏黎世", Name: "🇨🇭 苏黎世 (Zurich)"},
			{Filter: "奥斯陆", Name: "🇳🇴 奥斯陆 (Oslo)"},
			{Filter: "布加勒斯特", Name: "🇷🇴 布加勒斯特 (Bucharest)"},
			{Filter: "香港", Name: "🇭🇰 香港 (Victoria)"},
			{Filter: "美国西部", Name: "🇺🇸 全部美国西部"},
			{Filter: "美国东部", Name: "🇺🇸 全部美国东部"},
			{Filter: "加拿大", Name: "🇨🇦 全部加拿大"},
		}
		for _, sub := range wsSubs {
			cnt := 0
			bestSpd := 0.0
			for _, n := range wsNodes {
				if matchNodeSubRegion(n, sub.Filter) {
					cnt++
					if n.SpeedMbps > bestSpd {
						bestSpd = n.SpeedMbps
					}
				}
			}
			if cnt > 0 {
				result = append(result, RegionStat{
					Code:      "SRC:windscribe:" + sub.Filter,
					Name:      sub.Name,
					Available: cnt,
					BestSpeed: bestSpd,
					Category:  "windscribe",
				})
			}
		}
	}

	// --- 4. Opera 分类 (支持美洲/欧洲/亚洲等大区城市) ---
	operaNodes := make([]Node, 0)
	for _, n := range candidateNodes {
		if !used[n.HostName] && classifyNodeCategory(n) == "opera" {
			operaNodes = append(operaNodes, n)
		}
	}
	if len(operaNodes) > 0 {
		result = append(result, RegionStat{
			Code:      "SRC:opera:ALL",
			Name:      "🌐 全部 Opera 节点",
			Available: len(operaNodes),
			Category:  "opera",
		})
		operaSubs := []struct {
			Filter string
			Name   string
		}{
			{Filter: "美洲", Name: "🌎 美洲大区 (Americas)"},
			{Filter: "欧洲", Name: "🌍 欧洲大区 (Europe)"},
			{Filter: "亚洲", Name: "🌏 亚洲大区 (Asia)"},
		}
		for _, sub := range operaSubs {
			cnt := 0
			bestSpd := 0.0
			for _, n := range operaNodes {
				if matchNodeSubRegion(n, sub.Filter) {
					cnt++
					if n.SpeedMbps > bestSpd {
						bestSpd = n.SpeedMbps
					}
				}
			}
			if cnt > 0 {
				result = append(result, RegionStat{
					Code:      "SRC:opera:" + sub.Filter,
					Name:      sub.Name,
					Available: cnt,
					BestSpeed: bestSpd,
					Category:  "opera",
				})
			}
		}
	}

	// --- 5. Proton 分类 (支持日本、新加坡、美国等国家专属出口) ---
	protonNodes := make([]Node, 0)
	for _, n := range candidateNodes {
		if !used[n.HostName] && classifyNodeCategory(n) == "proton" {
			protonNodes = append(protonNodes, n)
		}
	}
	if len(protonNodes) > 0 {
		result = append(result, RegionStat{
			Code:      "SRC:proton:ALL",
			Name:      "🌐 全部 Proton 节点",
			Available: len(protonNodes),
			Category:  "proton",
		})
		protonSubs := []struct {
			Filter string
			Name   string
		}{
			{Filter: "日本", Name: "🇯🇵 日本 (Japan)"},
			{Filter: "新加坡", Name: "🇸🇬 新加坡 (Singapore)"},
			{Filter: "美国", Name: "🇺🇸 美国 (United States)"},
			{Filter: "荷兰", Name: "🇳🇱 荷兰 (Netherlands)"},
			{Filter: "瑞士", Name: "🇨🇭 瑞士 (Switzerland)"},
			{Filter: "加拿大", Name: "🇨🇦 加拿大 (Canada)"},
			{Filter: "挪威", Name: "🇳🇴 挪威 (Norway)"},
			{Filter: "波兰", Name: "🇵🇱 波兰 (Poland)"},
			{Filter: "罗马尼亚", Name: "🇷🇴 罗马尼亚 (Romania)"},
			{Filter: "墨西哥", Name: "🇲🇽 墨西哥 (Mexico)"},
		}
		for _, sub := range protonSubs {
			cnt := 0
			bestSpd := 0.0
			for _, n := range protonNodes {
				if matchNodeSubRegion(n, sub.Filter) {
					cnt++
					if n.SpeedMbps > bestSpd {
						bestSpd = n.SpeedMbps
					}
				}
			}
			if cnt > 0 {
				result = append(result, RegionStat{
					Code:      "SRC:proton:" + sub.Filter,
					Name:      sub.Name,
					Available: cnt,
					BestSpeed: bestSpd,
					Category:  "proton",
				})
			}
		}
	}
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
