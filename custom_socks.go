package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// CustomNode 记录一个用户自定义的 SOCKS5 / HTTP 出口节点
type CustomNode struct {
	ID          string  `json:"id"`
	HostName    string  `json:"hostname"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	User        string  `json:"user"`
	Pass        string  `json:"pass"`
	Protocol    string  `json:"protocol,omitempty"` // "socks5" | "http" | "https" | "masque"
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

// StartAutoUpdateWorker 启动后台自动定时更新订阅源的 Worker（包括 VPN Gate 和自定义订阅源）
func StartAutoUpdateWorker(m *Manager) {
	go func() {
		lastVpngateUpdate := time.Now()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			// 1. VPN Gate 官方源自动定时拉取（默认每 60 分钟自动更新一次）
			if m != nil && time.Since(lastVpngateUpdate) >= 60*time.Minute {
				if n, err := m.RefreshNodes(); err == nil {
					lastVpngateUpdate = time.Now()
					log.Printf("VPN Gate 官方全球源已完成自动定时更新，获取到 %d 个节点", n)
					m.ReviveFailedTunnels()
				}
			}

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

			customUpdated := false
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
				customUpdated = true
			}

			if customUpdated && m != nil {
				m.ReviveFailedTunnels()
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

// dialHttpConnect 通过 HTTP / HTTPS 代理建立 CONNECT 隧道
func dialHttpConnect(proxyAddr string, isTLS bool, user, pass, targetAddr string, timeout time.Duration) (net.Conn, error) {
	var conn net.Conn
	var err error
	if isTLS {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", proxyAddr, &tls.Config{
			InsecureSkipVerify: true,
		})
	} else {
		conn, err = net.DialTimeout("tcp", proxyAddr, timeout)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n", targetAddr, targetAddr)
	if user != "" || pass != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
	}
	req += "\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取 HTTP 代理握手响应失败: %w", err)
	}

	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		_ = conn.Close()
		return nil, fmt.Errorf("无效的 HTTP 代理响应: %s", strings.TrimSpace(statusLine))
	}
	statusCode, _ := strconv.Atoi(parts[1])
	if statusCode != 200 {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTP 代理 CONNECT 失败 (状态码: %d %s)", statusCode, strings.TrimSpace(statusLine))
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialUpstreamProxy 统一根据协议建立到远端代理的连接
func dialUpstreamProxy(proxyAddr, protocol, user, pass, targetAddr string, timeout time.Duration) (net.Conn, error) {
	proto := strings.ToLower(strings.TrimSpace(protocol))
	switch proto {
	case "http":
		return dialHttpConnect(proxyAddr, false, user, pass, targetAddr, timeout)
	case "https":
		return dialHttpConnect(proxyAddr, true, user, pass, targetAddr, timeout)
	default:
		return dialSocks5(proxyAddr, user, pass, targetAddr, timeout)
	}
}

// ProbeCustomProxy 探测自定义代理（SOCKS5 / HTTP / HTTPS）的真实出口 IP、延迟及家宽/机房属性
func ProbeCustomProxy(proxyAddr, protocol, user, pass string, timeout time.Duration) (exitIP string, ping int, ipType string, isp string, err error) {
	start := time.Now()
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialUpstreamProxy(proxyAddr, protocol, user, pass, addr, timeout)
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
		return "", 0, "", "", fmt.Errorf("连接代理超时或未获取到出口 IP")
	}

	ping = int(time.Since(start).Milliseconds())

	// 查询 IP 属性
	ipType, isp, _, _ = DetectIPType(exitIP)
	return exitIP, ping, ipType, isp, nil
}

// ProbeCustomSocks 保持向后兼容调用 ProbeCustomProxy
func ProbeCustomSocks(proxyAddr, user, pass string, timeout time.Duration) (exitIP string, ping int, ipType string, isp string, err error) {
	return ProbeCustomProxy(proxyAddr, "socks5", user, pass, timeout)
}

// inferCountryFromRemark 根据节点备注名称智能推断国家与代码
func inferCountryFromRemark(remark string) (string, string) {
	r := strings.ToLower(remark)
	if strings.Contains(r, "香港") || strings.Contains(r, "hk") || strings.Contains(r, "hongkong") {
		return "香港", "HK"
	}
	if strings.Contains(r, "日本") || strings.Contains(r, "jp") || strings.Contains(r, "japan") || strings.Contains(r, "tokyo") {
		return "日本", "JP"
	}
	if strings.Contains(r, "美国") || strings.Contains(r, "us") || strings.Contains(r, "united states") {
		return "美国", "US"
	}
	if strings.Contains(r, "新加坡") || strings.Contains(r, "sg") || strings.Contains(r, "singapore") {
		return "新加坡", "SG"
	}
	if strings.Contains(r, "加拿大") || strings.Contains(r, "ca") || strings.Contains(r, "canada") {
		return "加拿大", "CA"
	}
	if strings.Contains(r, "英国") || strings.Contains(r, "gb") || strings.Contains(r, "uk") || strings.Contains(r, "london") {
		return "英国", "GB"
	}
	if strings.Contains(r, "德国") || strings.Contains(r, "de") || strings.Contains(r, "germany") {
		return "德国", "DE"
	}
	if strings.Contains(r, "法国") || strings.Contains(r, "fr") || strings.Contains(r, "france") {
		return "法国", "FR"
	}
	if strings.Contains(r, "荷兰") || strings.Contains(r, "nl") || strings.Contains(r, "netherlands") {
		return "荷兰", "NL"
	}
	if strings.Contains(r, "瑞士") || strings.Contains(r, "ch") || strings.Contains(r, "switzerland") {
		return "瑞士", "CH"
	}
	if strings.Contains(r, "罗马尼亚") || strings.Contains(r, "ro") || strings.Contains(r, "romania") {
		return "罗马尼亚", "RO"
	}
	if strings.Contains(r, "挪威") || strings.Contains(r, "no") || strings.Contains(r, "norway") {
		return "挪威", "NO"
	}
	if strings.Contains(r, "亚洲") || strings.Contains(r, "asia") {
		return "亚洲", "AS"
	}
	if strings.Contains(r, "欧洲") || strings.Contains(r, "europe") {
		return "欧洲", "EU"
	}
	if strings.Contains(r, "美洲") || strings.Contains(r, "america") {
		return "美洲", "AM"
	}
	return "自定义", "CUSTOM"
}

// ParseProxyURL 解析代理链接，兼容 socks5://, socks://, http://, https://, masque:// 以及 host:port[:user:pass]
func ParseProxyURL(raw string) (proto, host string, port int, user, pass, remark string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, "", "", "", fmt.Errorf("链接为空")
	}

	proto = "socks5"
	if strings.HasPrefix(raw, "socks5://") || strings.HasPrefix(raw, "socks://") {
		proto = "socks5"
	} else if strings.HasPrefix(raw, "https://") {
		proto = "https"
	} else if strings.HasPrefix(raw, "http://") {
		proto = "http"
	} else if strings.HasPrefix(raw, "masque://") {
		proto = "masque"
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", 0, "", "", "", err
		}
		host = u.Hostname()
		pStr := u.Port()
		if pStr != "" {
			port, _ = strconv.Atoi(pStr)
		} else {
			if proto == "https" || proto == "masque" {
				port = 443
			} else if proto == "http" {
				port = 80
			} else {
				port = 1080
			}
		}
		if u.User != nil {
			user, _ = url.QueryUnescape(u.User.Username())
			pwd, hasPwd := u.User.Password()
			if hasPwd {
				pass, _ = url.QueryUnescape(pwd)
			}
		}
		remark, _ = url.QueryUnescape(u.Fragment)
		if remark == "" {
			remark = host
		}
		return proto, host, port, user, pass, remark, nil
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
		if port == 443 {
			proto = "https"
		} else if port == 80 || port == 8080 {
			proto = "http"
		}
		return proto, host, port, user, pass, host, nil
	}
	return "", "", 0, "", "", "", fmt.Errorf("无法解析的代理格式: %s", raw)
}

// ParseSocksURL 兼容原有接口
func ParseSocksURL(raw string) (host string, port int, user, pass, remark string, err error) {
	_, host, port, user, pass, remark, err = ParseProxyURL(raw)
	return host, port, user, pass, remark, err
}

// parseClashYamlNodes 解析 Clash / Mihomo 订阅中的 proxies 节点列表
func parseClashYamlNodes(content string) []CustomNode {
	var nodes []CustomNode
	lines := strings.Split(content, "\n")
	inProxies := false

	parseMap := func(m map[string]string) {
		server := strings.Trim(m["server"], "\"' ")
		portStr := strings.Trim(m["port"], "\"' ")
		name := strings.Trim(m["name"], "\"' ")
		pType := strings.ToLower(strings.Trim(m["type"], "\"' "))
		user := strings.Trim(m["username"], "\"' ")
		pass := strings.Trim(m["password"], "\"' ")
		tlsStr := strings.ToLower(strings.Trim(m["tls"], "\"' "))

		if server == "" || portStr == "" {
			return
		}
		port, _ := strconv.Atoi(portStr)
		if port <= 0 {
			return
		}

		proto := "socks5"
		if pType == "http" {
			if tlsStr == "true" {
				proto = "https"
			} else {
				proto = "http"
			}
		} else if pType == "socks5" || pType == "socks" {
			proto = "socks5"
		} else if pType == "masque" {
			proto = "masque"
		}

		if name == "" {
			name = server
		}
		country, countryCode := inferCountryFromRemark(name)
		nodeID := fmt.Sprintf("cs-%s-%d", server, port)
		nodes = append(nodes, CustomNode{
			ID:          nodeID,
			HostName:    nodeID,
			Host:        server,
			Port:        port,
			User:        user,
			Pass:        pass,
			Protocol:    proto,
			Country:     country,
			CountryCode: countryCode,
			Remark:      name,
			IPType:      "residential",
		})
	}

	var curProxy map[string]string
	commit := func() {
		if len(curProxy) > 0 {
			parseMap(curProxy)
			curProxy = nil
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "proxies:") {
			inProxies = true
			continue
		}
		if inProxies && (strings.HasPrefix(trimmed, "proxy-groups:") || strings.HasPrefix(trimmed, "rules:") || strings.HasPrefix(trimmed, "rule-providers:")) {
			commit()
			break
		}
		if !inProxies {
			continue
		}

		// 单行 flow 格式: - {name: "xxx", type: http, ...}
		if strings.HasPrefix(trimmed, "- {") && strings.HasSuffix(trimmed, "}") {
			commit()
			body := trimmed[3 : len(trimmed)-1]
			m := make(map[string]string)
			parts := strings.Split(body, ",")
			for _, part := range parts {
				kv := strings.SplitN(part, ":", 2)
				if len(kv) == 2 {
					m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
				}
			}
			parseMap(m)
			continue
		}

		// 多行 block 格式: - name: "xxx"
		if strings.HasPrefix(trimmed, "- ") {
			commit()
			curProxy = make(map[string]string)
			rest := strings.TrimPrefix(trimmed, "- ")
			kv := strings.SplitN(rest, ":", 2)
			if len(kv) == 2 {
				curProxy[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			}
		} else if curProxy != nil && strings.Contains(trimmed, ":") {
			kv := strings.SplitN(trimmed, ":", 2)
			if len(kv) == 2 {
				curProxy[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			}
		}
	}
	commit()
	return nodes
}

// ParseSubscriptionContent 能够识别并解析多行链接、Base64 订阅以及 Clash/Mihomo YAML
func ParseSubscriptionContent(content string) ([]CustomNode, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("订阅内容为空")
	}

	// 尝试 Base64 解码
	if dec, err := base64.StdEncoding.DecodeString(content); err == nil && len(dec) > 0 {
		content = string(dec)
	} else if dec, err := base64.URLEncoding.DecodeString(content); err == nil && len(dec) > 0 {
		content = string(dec)
	}

	// 1. 如果包含 proxies: 说明是 Clash / Mihomo 配置文件
	if strings.Contains(content, "proxies:") {
		nodes := parseClashYamlNodes(content)
		if len(nodes) > 0 {
			ptrs := make([]*CustomNode, len(nodes))
			for i := range nodes {
				ptrs[i] = &nodes[i]
			}
			BatchDetectIPInfo(ptrs)
			return nodes, nil
		}
	}

	// 2. 按行解析多行链接
	var nodes []CustomNode
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proto, h, p, u, pwd, remark, err := ParseProxyURL(line)
		if err != nil || h == "" || p <= 0 {
			continue
		}
		if remark == "" {
			remark = fmt.Sprintf("节点-%d", i+1)
		}
		country, countryCode := inferCountryFromRemark(remark)
		nodeID := fmt.Sprintf("cs-%s-%d", h, p)
		nodes = append(nodes, CustomNode{
			ID:          nodeID,
			HostName:    nodeID,
			Host:        h,
			Port:        p,
			User:        u,
			Pass:        pwd,
			Protocol:    proto,
			Country:     country,
			CountryCode: countryCode,
			Remark:      remark,
			IPType:      "residential",
		})
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("未能从内容中解析出有效代理节点 (支持 SOCKS5 / HTTP / HTTPS 链接及 Clash YAML)")
	}

	ptrs := make([]*CustomNode, len(nodes))
	for i := range nodes {
		ptrs[i] = &nodes[i]
	}
	BatchDetectIPInfo(ptrs)
	return nodes, nil
}

// FetchSourceNodes 拉取并解析外部订阅源（支持链接列表、Base64 及 Clash/Mihomo YAML）
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

	return ParseSubscriptionContent(string(raw))
}

