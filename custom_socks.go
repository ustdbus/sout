package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CustomNode 记录一个用户自定义的 SOCKS5 出口节点
type CustomNode struct {
	ID          string  `json:"id"`
	HostName    string  `json:"hostname"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	User        string  `json:"user"`
	Pass        string  `json:"pass"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Remark      string  `json:"remark"`
	Ping        int     `json:"ping"`
	SpeedMbps   float64 `json:"speed_mbps"`
	ExitIP      string  `json:"exit_ip"`
	IPType      string  `json:"ip_type"` // "residential" | "datacenter"
	ISP         string  `json:"isp,omitempty"`
	SourceID    string  `json:"source_id,omitempty"`
}

// CustomSource 记录一个第三方的 SOCKS5 订阅/API 节点源
type CustomSource struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	URL              string    `json:"url"`
	Count            int       `json:"count"`
	Enabled          bool      `json:"enabled"`
	AutoUpdate       bool      `json:"auto_update"`       // 是否开启自动更新
	UpdateIntervalM  int       `json:"update_interval_m"` // 自动更新周期（分钟），默认 60 分钟
	ResidentialCount int       `json:"residential_count"`
	DatacenterCount  int       `json:"datacenter_count"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type IPInfo struct {
	IPType      string    `json:"ip_type"`
	ISP         string    `json:"isp"`
	Country     string    `json:"country"`
	CountryCode string    `json:"country_code"`
	CachedAt    time.Time `json:"cached_at"`
}

var (
	ipCacheMu sync.RWMutex
	ipCache   = make(map[string]IPInfo)
)

// DetectIPType 查询单个 IP 的属性（家宽/机房、运营商、国家）
func DetectIPType(ip string) (ipType, isp, country, countryCode string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "datacenter", "", "", ""
	}
	ipCacheMu.RLock()
	if info, ok := ipCache[ip]; ok && time.Since(info.CachedAt) < 24*time.Hour {
		ipCacheMu.RUnlock()
		return info.IPType, info.ISP, info.Country, info.CountryCode
	}
	ipCacheMu.RUnlock()

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://ip-api.com/json/%s?fields=status,country,countryCode,isp,org,as,hosting,mobile,query", ip))
	if err != nil {
		return "datacenter", "", "", ""
	}
	defer resp.Body.Close()

	var data struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		ISP         string `json:"isp"`
		Hosting     bool   `json:"hosting"`
		Mobile      bool   `json:"mobile"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || data.Status != "success" {
		return "datacenter", "", "", ""
	}

	ipType = "residential"
	if data.Hosting {
		ipType = "datacenter"
	}

	info := IPInfo{
		IPType:      ipType,
		ISP:         data.ISP,
		Country:     data.Country,
		CountryCode: data.CountryCode,
		CachedAt:    time.Now(),
	}

	ipCacheMu.Lock()
	ipCache[ip] = info
	ipCacheMu.Unlock()

	return info.IPType, info.ISP, info.Country, info.CountryCode
}

// BatchDetectIPInfo 批量并发探测节点列表的真实属性（分批处理全部节点）
func BatchDetectIPInfo(nodes []*CustomNode) {
	if len(nodes) == 0 {
		return
	}

	type QueryItem struct {
		Query string `json:"query"`
	}

	for start := 0; start < len(nodes); start += 100 {
		end := start + 100
		if end > len(nodes) {
			end = len(nodes)
		}
		chunk := nodes[start:end]

		var queries []QueryItem
		nodeIdxMap := make(map[string][]*CustomNode)

		for _, n := range chunk {
			ip := strings.TrimSpace(n.Host)
			if net.ParseIP(ip) == nil {
				n.IPType = "datacenter"
				continue
			}
			if _, seen := nodeIdxMap[ip]; !seen {
				queries = append(queries, QueryItem{Query: ip})
			}
			nodeIdxMap[ip] = append(nodeIdxMap[ip], n)
		}

		if len(queries) == 0 {
			continue
		}

		payload, err := json.Marshal(queries)
		if err != nil {
			continue
		}

		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Post("http://ip-api.com/batch?fields=status,country,countryCode,isp,org,as,hosting,mobile,query", "application/json", strings.NewReader(string(payload)))
		if err != nil {
			continue
		}

		var results []struct {
			Status      string `json:"status"`
			Query       string `json:"query"`
			Country     string `json:"country"`
			CountryCode string `json:"countryCode"`
			ISP         string `json:"isp"`
			Hosting     bool   `json:"hosting"`
			Mobile      bool   `json:"mobile"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		for _, item := range results {
			if item.Status != "success" {
				continue
			}
			ipType := "residential"
			if item.Hosting {
				ipType = "datacenter"
			}
			for _, n := range nodeIdxMap[item.Query] {
				n.IPType = ipType
				n.ISP = item.ISP
				if n.Country == "" || n.Country == "自定义" {
					n.Country = item.Country
				}
				if n.CountryCode == "" || n.CountryCode == "CUSTOM" {
					n.CountryCode = item.CountryCode
				}
			}
		}
	}

	for _, n := range nodes {
		if n.IPType == "" {
			n.IPType = "residential"
		}
	}
}

type CustomStore struct {
	mu      sync.RWMutex
	dir     string
	Sources map[string]*CustomSource `json:"sources"`
	Nodes   map[string]*CustomNode   `json:"nodes"`
}

var globalCustomStore *CustomStore

func initCustomStore(dir string) *CustomStore {
	cs := &CustomStore{
		dir:     dir,
		Sources: make(map[string]*CustomSource),
		Nodes:   make(map[string]*CustomNode),
	}
	cs.load()
	globalCustomStore = cs
	return cs
}

func (cs *CustomStore) savePath() string {
	return filepath.Join(cs.dir, "custom_store.json")
}

func (cs *CustomStore) load() {
	blob, err := os.ReadFile(cs.savePath())
	if err != nil {
		return
	}
	var data struct {
		Sources map[string]*CustomSource `json:"sources"`
		Nodes   map[string]*CustomNode   `json:"nodes"`
	}
	if err := json.Unmarshal(blob, &data); err == nil {
		if data.Sources != nil {
			for _, s := range data.Sources {
				if s.UpdateIntervalM == 0 {
					s.AutoUpdate = true
					s.UpdateIntervalM = 60
				}
			}
			cs.Sources = data.Sources
		}
		if data.Nodes != nil {
			cs.Nodes = data.Nodes
		}
	}
}

func (cs *CustomStore) save() error {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	data := struct {
		Sources map[string]*CustomSource `json:"sources"`
		Nodes   map[string]*CustomNode   `json:"nodes"`
	}{
		Sources: cs.Sources,
		Nodes:   cs.Nodes,
	}
	blob, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.savePath(), blob, 0600)
}

// StartAutoUpdateWorker 启动后台自动定时更新订阅源的 Worker
func StartAutoUpdateWorker() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if globalCustomStore == nil {
				continue
			}
			var toUpdate []*CustomSource
			globalCustomStore.mu.RLock()
			for _, s := range globalCustomStore.Sources {
				if s.Enabled && s.AutoUpdate && s.UpdateIntervalM > 0 {
					interval := time.Duration(s.UpdateIntervalM) * time.Minute
					if time.Since(s.UpdatedAt) >= interval {
						toUpdate = append(toUpdate, s)
					}
				}
			}
			globalCustomStore.mu.RUnlock()

			for _, s := range toUpdate {
				nodes, err := FetchSourceNodes(s.URL, 15*time.Second)
				if err != nil {
					continue
				}

				resCount := 0
				dchCount := 0
				for _, n := range nodes {
					if n.IPType == "datacenter" {
						dchCount++
					} else {
						resCount++
					}
				}

				globalCustomStore.mu.Lock()
				s.Count = len(nodes)
				s.ResidentialCount = resCount
				s.DatacenterCount = dchCount
				s.UpdatedAt = time.Now()
				for k, n := range globalCustomStore.Nodes {
					if n.SourceID == s.ID {
						delete(globalCustomStore.Nodes, k)
					}
				}
				for _, n := range nodes {
					n.SourceID = s.ID
					globalCustomStore.Nodes[n.ID] = &n
				}
				globalCustomStore.mu.Unlock()
				_ = globalCustomStore.save()
			}
		}
	}()
}

// dialSocks5 建立到远端上游 SOCKS5 代理的 TCP 隧道 (支持无认证与 RFC1929 用户名密码)
func dialSocks5(proxyAddr, user, pass, targetAddr string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if user != "" || pass != "" {
		_, err = conn.Write([]byte{0x05, 0x01, 0x02})
	} else {
		_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	resp := make([]byte, 2)
	if _, err = io.ReadFull(conn, resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp[0] != 0x05 {
		_ = conn.Close()
		return nil, fmt.Errorf("非 SOCKS5 响应")
	}

	if resp[1] == 0x02 {
		req := []byte{0x01, byte(len(user))}
		req = append(req, []byte(user)...)
		req = append(req, byte(len(pass)))
		req = append(req, []byte(pass)...)
		if _, err = conn.Write(req); err != nil {
			_ = conn.Close()
			return nil, err
		}
		authResp := make([]byte, 2)
		if _, err = io.ReadFull(conn, authResp); err != nil || authResp[1] != 0x00 {
			_ = conn.Close()
			return nil, fmt.Errorf("SOCKS5 用户名或密码认证失败")
		}
	} else if resp[1] != 0x00 {
		_ = conn.Close()
		return nil, fmt.Errorf("SOCKS5 握手认证被拒绝 (方法代码: %d)", resp[1])
	}

	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	port, _ := strconv.Atoi(portStr)

	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = []byte{0x05, 0x01, 0x00, 0x01}
			req = append(req, ip4...)
		} else {
			req = []byte{0x05, 0x01, 0x00, 0x04}
			req = append(req, ip.To16()...)
		}
	} else {
		req = []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))

	if _, err = conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, err
	}

	connResp := make([]byte, 4)
	if _, err = io.ReadFull(conn, connResp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if connResp[1] != 0x00 {
		_ = conn.Close()
		return nil, fmt.Errorf("SOCKS5 CONNECT 失败: %d", connResp[1])
	}

	switch connResp[3] {
	case 0x01:
		_, _ = io.CopyN(io.Discard, conn, 4+2)
	case 0x04:
		_, _ = io.CopyN(io.Discard, conn, 16+2)
	case 0x03:
		l := make([]byte, 1)
		_, _ = io.ReadFull(conn, l)
		_, _ = io.CopyN(io.Discard, conn, int64(l[0])+2)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// ProbeCustomSocks 探测自定义 SOCKS5 代理的真实出口 IP、延迟及家宽/机房属性
func ProbeCustomSocks(proxyAddr, user, pass string, timeout time.Duration) (exitIP string, ping int, ipType string, isp string, err error) {
	start := time.Now()
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialSocks5(proxyAddr, user, pass, addr, timeout)
	}
	tr := &http.Transport{
		DialContext:     dialer,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: timeout}

	endpoints := []string{
		"http://api.ipify.org",
		"http://icanhazip.com",
		"http://ifconfig.me",
		"http://checkip.amazonaws.com",
	}

	for _, ep := range endpoints {
		resp, err := client.Get(ep)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			exitIP = ip
			break
		}
	}

	if exitIP == "" {
		return "", 0, "", "", fmt.Errorf("连接 SOCKS5 代理超时或未获取到出口 IP")
	}

	ping = int(time.Since(start).Milliseconds())

	// 查询 IP 属性
	ipType, isp, _, _ = DetectIPType(exitIP)
	return exitIP, ping, ipType, isp, nil
}

// ParseSocksURL 解析 socks5://user:pass@host:port#remark 格式或 host:port:user:pass 格式
func ParseSocksURL(raw string) (host string, port int, user, pass, remark string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, "", "", "", fmt.Errorf("链接为空")
	}

	if strings.HasPrefix(raw, "socks5://") || strings.HasPrefix(raw, "socks://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", 0, "", "", "", err
		}
		host = u.Hostname()
		p, _ := strconv.Atoi(u.Port())
		port = p
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		remark, _ = url.QueryUnescape(u.Fragment)
		if remark == "" {
			remark = host
		}
		return host, port, user, pass, remark, nil
	}

	// 尝试 host:port[:user:pass] 格式
	parts := strings.Split(raw, ":")
	if len(parts) >= 2 {
		host = parts[0]
		port, _ = strconv.Atoi(parts[1])
		if len(parts) >= 4 {
			user = parts[2]
			pass = parts[3]
		}
		return host, port, user, pass, host, nil
	}
	return "", 0, "", "", "", fmt.Errorf("无法解析的 SOCKS5 格式")
}

// FetchSourceNodes 拉取并解析外部 SOCKS5 订阅源
func FetchSourceNodes(sourceURL string, timeout time.Duration) ([]CustomNode, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; sout/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求源链接失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("源响应状态码 %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	content := strings.TrimSpace(string(raw))
	if dec, err := base64.StdEncoding.DecodeString(content); err == nil {
		content = string(dec)
	} else if dec, err := base64.URLEncoding.DecodeString(content); err == nil {
		content = string(dec)
	}

	var nodes []CustomNode
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		h, p, u, pwd, remark, err := ParseSocksURL(line)
		if err != nil || h == "" || p <= 0 {
			continue
		}
		if remark == "" {
			remark = fmt.Sprintf("节点-%d", i+1)
		}
		nodeID := fmt.Sprintf("cs-%s-%d", h, p)
		nodes = append(nodes, CustomNode{
			ID:          nodeID,
			HostName:    nodeID,
			Host:        h,
			Port:        p,
			User:        u,
			Pass:        pwd,
			Country:     "自定义",
			CountryCode: "CUSTOM",
			Remark:      remark,
			IPType:      "residential", // 初始默认
		})
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("未在该源中解析出有效 SOCKS5 节点")
	}

	// 批量探测 IP 属性与家宽/机房分类
	ptrs := make([]*CustomNode, len(nodes))
	for i := range nodes {
		ptrs[i] = &nodes[i]
	}
	BatchDetectIPInfo(ptrs)

	return nodes, nil
}
