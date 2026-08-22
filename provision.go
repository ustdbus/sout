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
	TemplateID int // 模板入站 ID；0 表示仅建出站
}

// Provision 异步执行一组出站拉取，返回 Job 供前端查询进度
func (m *Manager) Provision(req ProvisionRequest) (*Job, error) {
	if req.Count < 1 {
		return nil, fmt.Errorf("拉取数量至少为 1")
	}
	picks, err := m.pickNodes(req.Region, req.Count)
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
		where = "全球最优"
	}
	job := m.jobs.New(fmt.Sprintf("拉取 %d 条 %s 出口", len(picks), where), labels)

	go m.runProvision(job, picks, req.TemplateID)
	return job, nil
}

func (m *Manager) runProvision(job *Job, picks []Node, templateID int) {
	defer job.Finish()

	var wg sync.WaitGroup
	started := make([]*Tunnel, len(picks))

	for i, node := range picks {
		t, err := m.Start(node)
		if err != nil {
			job.Set(i, "failed", err.Error())
			continue
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

// pickNodes 挑选 count 个未被占用的节点，按速度降序选取
func (m *Manager) pickNodes(region string, count int) ([]Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	used := map[string]bool{}
	for _, t := range m.tunnels {
		used[t.Node.HostName] = true
	}

	var out []Node
	for _, n := range m.nodes {
		if len(out) >= count {
			break
		}
		if used[n.HostName] {
			continue
		}
		if region != "" && !strings.EqualFold(region, "ALL") && !strings.EqualFold(n.CountryCode, region) {
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

// Regions 返回当前所有可用地区的聚合统计，按可用数降序
func (m *Manager) Regions() []RegionStat {
	m.mu.RLock()
	defer m.mu.RUnlock()

	used := map[string]bool{}
	for _, t := range m.tunnels {
		used[t.Node.HostName] = true
	}

	byCode := map[string]*RegionStat{}
	for _, n := range m.nodes {
		if used[n.HostName] || n.CountryCode == "" {
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
	return out
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
