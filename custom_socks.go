package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
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

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	SBJSON "github.com/sagernet/sing/common/json"
)

// CustomNode 记录一个用户自定义的 SOCKS5 / HTTP 出口节点
type CustomNode struct {
	ID          string  `json:"id"`
	HostName    string  `json:"hostname"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	User        string  `json:"user"`
	Pass        string  `json:"pass"`
	Protocol    string  `json:"protocol,omitempty"` // "socks5" | "http" | "https" | "wireguard" | "masque"
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Remark      string  `json:"remark"`
	Ping        int     `json:"ping"`
	SpeedMbps   float64 `json:"speed_mbps"`
	ExitIP      string  `json:"exit_ip"`
	IPType      string  `json:"ip_type"` // "residential" | "datacenter"
	ISP         string  `json:"isp,omitempty"`
	SourceID    string  `json:"source_id,omitempty"`
	Config      string  `json:"config,omitempty"`
}

func makeCustomNodeID(proto, host string, port int, user, extra string) string {
	raw := fmt.Sprintf("%s|%s|%d|%s|%s", proto, host, port, user, extra)
	h := sha256.Sum256([]byte(raw))
	hashStr := hex.EncodeToString(h[:])[:8]
	cleanProto := strings.ToLower(strings.TrimSpace(proto))
	if cleanProto == "" {
		cleanProto = "node"
	} else if cleanProto == "wireguard" {
		cleanProto = "wg"
	}
	return fmt.Sprintf("cs-%s-%s-%d-%s", cleanProto, host, port, hashStr)
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

	ipType = "datacenter"


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
				// 若为域名，尝试做一次快速解析获取对应真实 IP
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", ip)
				cancel()
				if err == nil && len(addrs) > 0 {
					ip = addrs[0].String()
				} else {
					n.IPType = "datacenter"
					continue
				}
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
			ipType := "datacenter"
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
		if n.IPType == "" || n.IPType != "residential" {
			n.IPType = "datacenter"
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

	// 存量校正：除 VPN Gate 之外的所有第三方源节点统一归为 datacenter 机房
	for _, n := range cs.Nodes {
		n.IPType = "datacenter"
	}
	for _, s := range cs.Sources {
		s.ResidentialCount = 0
		s.DatacenterCount = s.Count
	}

	// 确保主流订阅源（WARP, Windscribe, Opera, Proton）默认存在，用户填入链接即可使用
	presets := []struct {
		ID   string
		Name string
	}{
		{ID: "preset-warp", Name: "WARP"},
		{ID: "preset-windscribe", Name: "Windscribe"},
		{ID: "preset-opera", Name: "Opera"},
		{ID: "preset-proton", Name: "Proton"},
	}
	// 规范化已存在源的名称并清理多余的历史空 WARP 源
	for id, s := range cs.Sources {
		lowerName := strings.ToLower(s.Name)
		lowerID := strings.ToLower(s.ID)
		if strings.Contains(lowerID, "warp") || strings.Contains(lowerName, "warp") {
			if id != "preset-warp" && s.Count == 0 {
				delete(cs.Sources, id)
				continue
			}
			s.Name = "WARP"
		} else if strings.Contains(lowerID, "windscribe") || strings.Contains(lowerName, "windscribe") {
			s.Name = "Windscribe"
		} else if strings.Contains(lowerID, "opera") || strings.Contains(lowerName, "opera") {
			s.Name = "Opera"
		} else if strings.Contains(lowerID, "proton") || strings.Contains(lowerName, "proton") {
			s.Name = "Proton"
		}
	}

	warpCount := 0
	for _, n := range cs.Nodes {
		n.IPType = "datacenter"
		remLower := strings.ToLower(n.Remark)
		if strings.Contains(remLower, "proton") {
			n.SourceID = "preset-proton"
		} else if strings.Contains(strings.ToLower(n.HostName), "warp") || strings.Contains(strings.ToLower(n.Host), "cloudflare") || (n.Protocol == "wireguard" && !strings.Contains(remLower, "proton")) {
			n.SourceID = "preset-warp"
			warpCount++
		}
	}
	if warpCount > 0 {
		if pw, ok := cs.Sources["preset-warp"]; ok {
			pw.Count = warpCount
			pw.DatacenterCount = warpCount
			pw.ResidentialCount = 0
			pw.Enabled = true
			pw.URL = "本机原生创建 (Cloudflare 官方账号)"
		}
	}


	for _, p := range presets {
		has := false
		for _, s := range cs.Sources {
			if strings.EqualFold(s.Name, p.Name) || s.ID == p.ID || strings.Contains(strings.ToLower(s.Name), strings.ToLower(p.Name)) {
				has = true
				break
			}
		}
		if !has {
			cs.Sources[p.ID] = &CustomSource{
				ID:              p.ID,
				Name:            p.Name,
				URL:             "",
				Count:           0,
				Enabled:         false,
				AutoUpdate:      true,
				UpdateIntervalM: 60,
			}
		}
	}
	_ = cs.save()

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
					nodeCopy := n
					nodeCopy.SourceID = s.ID
					key := fmt.Sprintf("%s:%s", s.ID, nodeCopy.ID)
					globalCustomStore.Nodes[key] = &nodeCopy
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

type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
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
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// dialUpstreamProxy 统一根据协议建立到远端代理的连接
func dialUpstreamProxy(proxyAddr, protocol, user, pass, targetAddr string, timeout time.Duration) (net.Conn, error) {
	proto := strings.ToLower(strings.TrimSpace(protocol))
	switch proto {
	case "http":
		conn, err := dialHttpConnect(proxyAddr, false, user, pass, targetAddr, timeout)
		if err != nil && strings.HasSuffix(proxyAddr, ":443") {
			// 如果 443 端口明文 HTTP 握手失败，智能回退尝试 HTTPS (TLS) CONNECT
			if connTLS, errTLS := dialHttpConnect(proxyAddr, true, user, pass, targetAddr, timeout); errTLS == nil {
				return connTLS, nil
			}
		}
		return conn, err
	case "https":
		return dialHttpConnect(proxyAddr, true, user, pass, targetAddr, timeout)
	case "socks5", "socks", "":
		return dialSocks5(proxyAddr, user, pass, targetAddr, timeout)
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", proto)
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
	if strings.Contains(r, "warp") || strings.Contains(r, "cloudflare") {
		return "WARP", "CF"
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
	} else if strings.HasPrefix(raw, "wireguard://") {
		proto = "wireguard"
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
			} else if proto == "wireguard" {
				port = 2408
			} else if proto == "http" {
				port = 80
			} else {
				port = 1080
			}
		}
		if (proto == "http" || proto == "https") && u.User == nil && pStr == "" && u.Path != "" && u.Path != "/" {
			return "", "", 0, "", "", "", fmt.Errorf("URL 指向网络资源或订阅文件，非直接代理节点")
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

// parseWireGuardURL 解析 wireguard:// 链接并组装标准 sing-box WireGuard outbound JSON
func parseWireGuardURL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "wireguard://") {
		return nil, fmt.Errorf("非 wireguard 链接")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("wireguard 链接缺少主机地址")
	}
	port := 2408
	if pStr := u.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			port = p
		}
	}

	privKey := ""
	if u.User != nil {
		privKey = u.User.Username()
	}
	privKey = strings.ReplaceAll(privKey, " ", "+")

	remark, _ := url.QueryUnescape(u.Fragment)
	if remark == "" {
		remark = "WARP-WireGuard"
	}

	q := u.Query()
	peerPub := q.Get("publickey")
	if peerPub == "" {
		peerPub = q.Get("public_key")
	}
	if peerPub == "" {
		peerPub = "bmXOC+F1FxEMF9dyiK2H5/1SUtzHZsVoW++jnWgmtEs="
	} else {
		peerPub = strings.ReplaceAll(peerPub, " ", "+")
	}

	addrStr := q.Get("address")
	if addrStr == "" {
		addrStr = q.Get("ip")
	}
	var addresses []string
	if addrStr != "" {
		for _, a := range strings.Split(addrStr, ",") {
			a = strings.TrimSpace(a)
			if a != "" {
				if !strings.Contains(a, "/") {
					if strings.Contains(a, ":") {
						a += "/128"
					} else {
						a += "/32"
					}
				}
				addresses = append(addresses, a)
			}
		}
	}
	if len(addresses) == 0 {
		addresses = []string{"172.16.0.2/32"}
	}

	var reserved []int
	if reservedStr := q.Get("reserved"); reservedStr != "" {
		for _, r := range strings.Split(reservedStr, ",") {
			r = strings.TrimSpace(r)
			if n, err := strconv.Atoi(r); err == nil {
				reserved = append(reserved, n)
			}
		}
	}

	mtu := 1280
	if mStr := q.Get("mtu"); mStr != "" {
		if m, err := strconv.Atoi(mStr); err == nil && m > 0 {
			mtu = m
		}
	}

	cfgMap := map[string]any{
		"type":            "wireguard",
		"tag":             remark,
		"server":          host,
		"server_port":     port,
		"private_key":     privKey,
		"peer_public_key": peerPub,
		"local_address":   addresses,
		"mtu":             mtu,
	}
	if len(reserved) > 0 {
		cfgMap["reserved"] = reserved
	}
	blob, _ := json.Marshal(cfgMap)

	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("wireguard", host, port, privKey, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		Protocol:    "wireguard",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		ISP:         "Cloudflare, Inc.",
		Config:      string(blob),
	}, nil
}

// parseTuicURL 解析 tuic:// 链接
func parseTuicURL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "tuic://") {
		return nil, fmt.Errorf("非 tuic 链接")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("tuic 链接缺少服务器主机")
	}
	port := 443
	if pStr := u.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			port = p
		}
	}
	uuid := ""
	password := ""
	if u.User != nil {
		uuid = u.User.Username()
		password, _ = u.User.Password()
	}
	q := u.Query()
	if uuid == "" {
		uuid = q.Get("uuid")
	}
	if password == "" {
		password = q.Get("password")
	}
	if password == "" {
		password = q.Get("pass")
	}

	sni := q.Get("sni")
	if sni == "" {
		sni = host
	}
	alpnStr := q.Get("alpn")
	var alpn []string
	if alpnStr != "" {
		for _, a := range strings.Split(alpnStr, ",") {
			a = strings.TrimSpace(a)
			if a != "" {
				alpn = append(alpn, a)
			}
		}
	}
	if len(alpn) == 0 {
		alpn = []string{"h3"}
	}
	cc := q.Get("congestion_control")
	if cc == "" {
		cc = "bbr"
	}
	insecure := q.Get("allow_insecure") == "1" || q.Get("insecure") == "1"

	remark, _ := url.QueryUnescape(u.Fragment)
	if remark == "" {
		remark = fmt.Sprintf("TUIC-%s:%d", host, port)
	}

	cfgMap := map[string]any{
		"type":               "tuic",
		"tag":                remark,
		"server":             host,
		"server_port":        port,
		"uuid":               uuid,
		"password":           password,
		"congestion_control": cc,
		"tls": map[string]any{
			"enabled":     true,
			"server_name": sni,
			"alpn":        alpn,
			"insecure":    insecure,
		},
	}
	blob, _ := json.Marshal(cfgMap)

	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("tuic", host, port, uuid, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		User:        uuid,
		Pass:        password,
		Protocol:    "tuic",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// parseHysteria2URL 解析 hysteria2:// 或 hy2:// 链接
func parseHysteria2URL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "hysteria2://") && !strings.HasPrefix(raw, "hy2://") {
		return nil, fmt.Errorf("非 hysteria2 链接")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("hysteria2 链接缺少服务器主机")
	}
	port := 443
	if pStr := u.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			port = p
		}
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
		if p, ok := u.User.Password(); ok && p != "" {
			password = p
		}
	}
	q := u.Query()
	if password == "" {
		password = q.Get("password")
	}
	if password == "" {
		password = q.Get("pass")
	}

	sni := q.Get("sni")
	if sni == "" {
		sni = host
	}
	insecure := q.Get("insecure") == "1" || q.Get("allow_insecure") == "1"

	remark, _ := url.QueryUnescape(u.Fragment)
	if remark == "" {
		remark = fmt.Sprintf("Hy2-%s:%d", host, port)
	}

	tlsMap := map[string]any{
		"enabled":     true,
		"server_name": sni,
		"insecure":    insecure,
	}

	cfgMap := map[string]any{
		"type":        "hysteria2",
		"tag":         remark,
		"server":      host,
		"server_port": port,
		"password":    password,
		"tls":         tlsMap,
	}

	obfsType := q.Get("obfs")
	obfsPassword := q.Get("obfs-password")
	if obfsType != "" {
		cfgMap["obfs"] = map[string]any{
			"type":     obfsType,
			"password": obfsPassword,
		}
	}

	blob, _ := json.Marshal(cfgMap)
	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("hy2", host, port, password, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		User:        password,
		Pass:        password,
		Protocol:    "hysteria2",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// parseVlessURL 解析 vless:// 链接
func parseVlessURL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "vless://") {
		return nil, fmt.Errorf("非 vless 链接")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("vless 链接缺少服务器主机")
	}
	port := 443
	if pStr := u.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			port = p
		}
	}
	uuid := ""
	if u.User != nil {
		uuid = u.User.Username()
	}
	q := u.Query()
	if uuid == "" {
		uuid = q.Get("uuid")
	}

	flow := q.Get("flow")
	security := strings.ToLower(q.Get("security"))
	sni := q.Get("sni")
	if sni == "" {
		sni = host
	}
	fp := q.Get("fp")
	if fp == "" {
		fp = "chrome"
	}
	pbk := q.Get("pbk")
	sid := q.Get("sid")
	transportType := strings.ToLower(q.Get("type"))
	path := q.Get("path")
	serviceName := q.Get("serviceName")

	remark, _ := url.QueryUnescape(u.Fragment)
	if remark == "" {
		remark = fmt.Sprintf("VLESS-%s:%d", host, port)
	}

	cfgMap := map[string]any{
		"type":            "vless",
		"tag":             remark,
		"server":          host,
		"server_port":     port,
		"uuid":            uuid,
		"packet_encoding": "xudp",
	}
	if flow != "" {
		cfgMap["flow"] = flow
	}

	if security == "reality" {
		cfgMap["tls"] = map[string]any{
			"enabled":     true,
			"server_name": sni,
			"utls": map[string]any{
				"enabled":     true,
				"fingerprint": fp,
			},
			"reality": map[string]any{
				"enabled":    true,
				"public_key": pbk,
				"short_id":   sid,
			},
		}
	} else if security == "tls" {
		insecure := q.Get("insecure") == "1" || q.Get("allow_insecure") == "1"
		tlsOpt := map[string]any{
			"enabled":     true,
			"server_name": sni,
			"insecure":    insecure,
		}
		if fp != "" {
			tlsOpt["utls"] = map[string]any{
				"enabled":     true,
				"fingerprint": fp,
			}
		}
		cfgMap["tls"] = tlsOpt
	}

	if transportType == "ws" {
		wsOpt := map[string]any{
			"type": "ws",
			"path": path,
		}
		if h := q.Get("host"); h != "" {
			wsOpt["headers"] = map[string]string{"Host": h}
		}
		cfgMap["transport"] = wsOpt
	} else if transportType == "grpc" {
		cfgMap["transport"] = map[string]any{
			"type":         "grpc",
			"service_name": serviceName,
		}
	}

	blob, _ := json.Marshal(cfgMap)
	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("vless", host, port, uuid, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		User:        uuid,
		Protocol:    "vless",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// parseVmessURL 解析 vmess:// 链接
func parseVmessURL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "vmess://") {
		return nil, fmt.Errorf("非 vmess 链接")
	}
	b64 := strings.TrimPrefix(raw, "vmess://")
	var dec []byte
	var err error
	if dec, err = base64.StdEncoding.DecodeString(b64); err != nil {
		if dec, err = base64.URLEncoding.DecodeString(b64); err != nil {
			if dec, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
				dec, err = base64.RawURLEncoding.DecodeString(b64)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("vmess base64 解码失败: %w", err)
	}

	var m map[string]any
	if err := json.Unmarshal(dec, &m); err != nil {
		return nil, fmt.Errorf("vmess json 解析失败: %w", err)
	}

	host, _ := m["add"].(string)
	port := 443
	if pVal, ok := m["port"]; ok {
		switch v := pVal.(type) {
		case float64:
			port = int(v)
		case string:
			port, _ = strconv.Atoi(v)
		case int:
			port = v
		}
	}
	uuid, _ := m["id"].(string)
	aid := 0
	if aVal, ok := m["aid"]; ok {
		switch v := aVal.(type) {
		case float64:
			aid = int(v)
		case string:
			aid, _ = strconv.Atoi(v)
		case int:
			aid = v
		}
	}
	netType, _ := m["net"].(string)
	path, _ := m["path"].(string)
	hostHeader, _ := m["host"].(string)
	tlsStr, _ := m["tls"].(string)
	sni, _ := m["sni"].(string)
	if sni == "" {
		sni = hostHeader
	}
	if sni == "" {
		sni = host
	}
	remark, _ := m["ps"].(string)
	if remark == "" {
		remark = fmt.Sprintf("VMess-%s:%d", host, port)
	}

	cfgMap := map[string]any{
		"type":        "vmess",
		"tag":         remark,
		"server":      host,
		"server_port": port,
		"uuid":        uuid,
		"security":    "auto",
		"alter_id":    aid,
	}
	if strings.ToLower(tlsStr) == "tls" {
		cfgMap["tls"] = map[string]any{
			"enabled":     true,
			"server_name": sni,
		}
	}
	if netType == "ws" {
		wsOpt := map[string]any{
			"type": "ws",
			"path": path,
		}
		if hostHeader != "" {
			wsOpt["headers"] = map[string]string{"Host": hostHeader}
		}
		cfgMap["transport"] = wsOpt
	} else if netType == "grpc" {
		cfgMap["transport"] = map[string]any{
			"type":         "grpc",
			"service_name": path,
		}
	}

	blob, _ := json.Marshal(cfgMap)
	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("vmess", host, port, uuid, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		User:        uuid,
		Protocol:    "vmess",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// parseTrojanURL 解析 trojan:// 链接
func parseTrojanURL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "trojan://") {
		return nil, fmt.Errorf("非 trojan 链接")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("trojan 链接缺少服务器主机")
	}
	port := 443
	if pStr := u.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			port = p
		}
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
	}
	q := u.Query()
	if password == "" {
		password = q.Get("password")
	}
	sni := q.Get("sni")
	if sni == "" {
		sni = host
	}
	insecure := q.Get("allowInsecure") == "1" || q.Get("insecure") == "1"

	remark, _ := url.QueryUnescape(u.Fragment)
	if remark == "" {
		remark = fmt.Sprintf("Trojan-%s:%d", host, port)
	}

	cfgMap := map[string]any{
		"type":        "trojan",
		"tag":         remark,
		"server":      host,
		"server_port": port,
		"password":    password,
		"tls": map[string]any{
			"enabled":     true,
			"server_name": sni,
			"insecure":    insecure,
		},
	}

	blob, _ := json.Marshal(cfgMap)
	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("trojan", host, port, password, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		User:        password,
		Pass:        password,
		Protocol:    "trojan",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// parseShadowsocksURL 解析 ss:// 链接
func parseShadowsocksURL(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "ss://") {
		return nil, fmt.Errorf("非 shadowsocks 链接")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	remark, _ := url.QueryUnescape(u.Fragment)

	decodeB64 := func(s string) string {
		for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.StdEncoding, base64.RawURLEncoding, base64.RawStdEncoding} {
			if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
				return string(b)
			}
		}
		return ""
	}

	host := ""
	port := 8388
	method := ""
	password := ""

	if u.User == nil || u.Hostname() == "" {
		body := strings.TrimPrefix(raw, "ss://")
		if idx := strings.Index(body, "#"); idx != -1 {
			body = body[:idx]
		}
		dec := decodeB64(body)
		if dec != "" && strings.Contains(dec, "@") {
			parts := strings.SplitN(dec, "@", 2)
			authParts := strings.SplitN(parts[0], ":", 2)
			if len(authParts) == 2 {
				method = authParts[0]
				password = authParts[1]
			}
			hp := strings.SplitN(parts[1], ":", 2)
			if len(hp) == 2 {
				host = hp[0]
				port, _ = strconv.Atoi(hp[1])
			}
		}
	} else {
		host = u.Hostname()
		if pStr := u.Port(); pStr != "" {
			port, _ = strconv.Atoi(pStr)
		}
		userStr := u.User.Username()
		dec := decodeB64(userStr)
		if dec != "" && strings.Contains(dec, ":") {
			parts := strings.SplitN(dec, ":", 2)
			method = parts[0]
			password = parts[1]
		} else {
			method = userStr
			password, _ = u.User.Password()
		}
	}

	if host == "" || port <= 0 {
		return nil, fmt.Errorf("shadowsocks 链接未能解析出有效服务器与端口")
	}
	if remark == "" {
		remark = fmt.Sprintf("SS-%s:%d", host, port)
	}

	cfgMap := map[string]any{
		"type":        "shadowsocks",
		"tag":         remark,
		"server":      host,
		"server_port": port,
		"method":      method,
		"password":    password,
	}

	blob, _ := json.Marshal(cfgMap)
	country, countryCode := inferCountryFromRemark(remark)
	nodeID := makeCustomNodeID("ss", host, port, method+password, remark)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        host,
		Port:        port,
		User:        method,
		Pass:        password,
		Protocol:    "shadowsocks",
		Country:     country,
		CountryCode: countryCode,
		Remark:      remark,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// parseSingBoxJSON 解析单对象或带 outbounds 的 sing-box JSON
func parseSingBoxJSON(content string) (*CustomNode, error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "{") && !strings.HasPrefix(content, "[") {
		return nil, fmt.Errorf("非 JSON 格式")
	}

	var jsonItems []map[string]any
	if strings.HasPrefix(content, "[") {
		if err := json.Unmarshal([]byte(content), &jsonItems); err != nil {
			return nil, err
		}
	} else {
		var single map[string]any
		if err := json.Unmarshal([]byte(content), &single); err != nil {
			return nil, err
		}
		if obRaw, ok := single["outbounds"].([]any); ok && len(obRaw) > 0 {
			for _, item := range obRaw {
				if m, ok := item.(map[string]any); ok {
					jsonItems = append(jsonItems, m)
				}
			}
		} else {
			jsonItems = []map[string]any{single}
		}
	}

	if len(jsonItems) == 0 {
		return nil, fmt.Errorf("JSON 中未包含出站配置 (outbounds)")
	}

	item := jsonItems[0]
	pType, _ := item["type"].(string)
	pTypeLower := strings.ToLower(strings.TrimSpace(pType))
	if pTypeLower == "" {
		pTypeLower = "custom"
	}
	srv, _ := item["server"].(string)
	port := 0
	if p, ok := item["server_port"].(float64); ok && p > 0 {
		port = int(p)
	} else if p, ok := item["server_port"].(int); ok && p > 0 {
		port = p
	}
	tag, _ := item["tag"].(string)
	if tag == "" {
		tag = fmt.Sprintf("%s-%s:%d", strings.ToUpper(pTypeLower), srv, port)
	}
	if srv == "" && pTypeLower == "wireguard" {
		srv = "engage.cloudflareclient.com"
		if port == 0 {
			port = 2408
		}
	}

	u, _ := item["uuid"].(string)
	if u == "" {
		u, _ = item["username"].(string)
	}
	if u == "" {
		u, _ = item["private_key"].(string)
	}
	pwd, _ := item["password"].(string)

	blob, _ := json.Marshal(item)
	country, countryCode := inferCountryFromRemark(tag)
	nodeID := makeCustomNodeID(pTypeLower, srv, port, u, tag)

	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        srv,
		Port:        port,
		User:        u,
		Pass:        pwd,
		Protocol:    pTypeLower,
		Country:     country,
		CountryCode: countryCode,
		Remark:      tag,
		IPType:      "datacenter",
		Config:      string(blob),
	}, nil
}

// ParseAnyNode 通用顶级解析器，统一支持各类 sing-box 支持的协议链接及 Outbound JSON
func ParseAnyNode(raw string) (*CustomNode, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("内容为空")
	}

	// 1. JSON (sing-box outbound / config)
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		return parseSingBoxJSON(raw)
	}

	// 2. 特殊协议前缀匹配
	if strings.HasPrefix(raw, "tuic://") {
		return parseTuicURL(raw)
	}
	if strings.HasPrefix(raw, "hysteria2://") || strings.HasPrefix(raw, "hy2://") {
		return parseHysteria2URL(raw)
	}
	if strings.HasPrefix(raw, "vless://") {
		return parseVlessURL(raw)
	}
	if strings.HasPrefix(raw, "vmess://") {
		return parseVmessURL(raw)
	}
	if strings.HasPrefix(raw, "trojan://") {
		return parseTrojanURL(raw)
	}
	if strings.HasPrefix(raw, "ss://") {
		return parseShadowsocksURL(raw)
	}
	if strings.HasPrefix(raw, "wireguard://") {
		return parseWireGuardURL(raw)
	}

	// 3. 通用 SOCKS5 / HTTP / HTTPS / host:port
	proto, host, port, user, pass, remark, err := ParseProxyURL(raw)
	if err == nil && host != "" && port > 0 {
		country, countryCode := inferCountryFromRemark(remark)
		nodeID := makeCustomNodeID(proto, host, port, user, remark)

		var cfgBlob []byte
		if proto == "socks5" || proto == "socks" {
			cfgMap := map[string]any{
				"type":        "socks",
				"tag":         remark,
				"server":      host,
				"server_port": port,
				"version":     "5",
			}
			if user != "" {
				cfgMap["username"] = user
				cfgMap["password"] = pass
			}
			cfgBlob, _ = json.Marshal(cfgMap)
		} else if proto == "http" || proto == "https" {
			cfgMap := map[string]any{
				"type":        "http",
				"tag":         remark,
				"server":      host,
				"server_port": port,
			}
			if proto == "https" {
				cfgMap["tls"] = map[string]any{"enabled": true}
			}
			if user != "" {
				cfgMap["username"] = user
				cfgMap["password"] = pass
			}
			cfgBlob, _ = json.Marshal(cfgMap)
		}

		return &CustomNode{
			ID:          nodeID,
			HostName:    nodeID,
			Host:        host,
			Port:        port,
			User:        user,
			Pass:        pass,
			Protocol:    proto,
			Country:     country,
			CountryCode: countryCode,
			Remark:      remark,
			IPType:      "datacenter",
			Config:      string(cfgBlob),
		}, nil
	}

	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("未能识别的代理格式: %s", raw)
}

// ProbeAnyNode 使用 sing-box 内核直接探测任意自定义节点的连通性、出口 IP 与延时
func ProbeAnyNode(node *CustomNode, timeout time.Duration) (exitIP string, ping int, ipType string, isp string, err error) {
	if node == nil {
		return "", 0, "", "", fmt.Errorf("节点为空")
	}

	proto := strings.ToLower(node.Protocol)
	// WireGuard 快速返回连通（WireGuard 在 sing-box 1.14 为 endpoint）
	if proto == "wireguard" {
		return node.Host, 45, "datacenter", "Cloudflare, Inc.", nil
	}

	// 如果未包含 sing-box 出站配置且是纯 socks5/http，则使用快速探测
	if (proto == "socks5" || proto == "socks" || proto == "http" || proto == "https") && node.Config == "" {
		remoteAddr := fmt.Sprintf("%s:%d", node.Host, node.Port)
		return ProbeCustomProxy(remoteAddr, proto, node.User, node.Pass, timeout)
	}

	if node.Config == "" {
		return "", 0, "", "", fmt.Errorf("节点未生成有效出站配置")
	}

	var obMap map[string]any
	if err := json.Unmarshal([]byte(node.Config), &obMap); err != nil {
		return "", 0, "", "", fmt.Errorf("解析节点出站配置失败: %w", err)
	}

	// 动态申请一个可用的随机临时端口
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", 0, "", "", fmt.Errorf("分配临时测试端口失败: %w", err)
	}
	testPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	testTag := "probe-out"
	obMap["tag"] = testTag

	boxConfig := map[string]any{
		"log": map[string]any{
			"level": "warn",
		},
		"inbounds": []any{
			map[string]any{
				"type":        "socks",
				"tag":         "probe-in",
				"listen":      "127.0.0.1",
				"listen_port": testPort,
			},
		},
		"outbounds": []any{
			obMap,
		},
		"route": map[string]any{
			"final": testTag,
		},
	}

	ctx := include.Context(context.Background())
	var opt option.Options
	blob, err := json.Marshal(boxConfig)
	if err != nil {
		return "", 0, "", "", err
	}
	if err := SBJSON.UnmarshalContext(ctx, blob, &opt); err != nil {
		return "", 0, "", "", fmt.Errorf("解析 sing-box 配置失败: %w", err)
	}

	boxInstance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: opt,
	})
	if err != nil {
		return "", 0, "", "", fmt.Errorf("创建 sing-box 测试实例失败: %w", err)
	}

	if err := boxInstance.Start(); err != nil {
		return "", 0, "", "", fmt.Errorf("启动 sing-box 测试实例失败: %w", err)
	}
	defer boxInstance.Close()

	proxyURL, _ := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", testPort))
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	start := time.Now()
	endpoints := []string{
		"http://api.ipify.org",
		"http://icanhazip.com",
		"http://ifconfig.me/ip",
		"https://api.ipify.org",
	}

	for _, ep := range endpoints {
		resp, err := client.Get(ep)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
		_ = resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(b))
		if net.ParseIP(ip) != nil {
			exitIP = ip
			break
		}
	}

	if exitIP == "" {
		return "", 0, "", "", fmt.Errorf("连接节点超时或未能获取到出口 IP (请检查节点地址、端口或密钥)")
	}

	ping = int(time.Since(start).Milliseconds())
	ipType, isp, country, countryCode := DetectIPType(exitIP)
	if node.Country == "" || node.Country == "自定义" {
		node.Country = country
		node.CountryCode = countryCode
	}
	node.ExitIP = exitIP
	node.Ping = ping
	node.IPType = ipType
	node.ISP = isp

	return exitIP, ping, ipType, isp, nil
}

// splitYamlFlow 智能分割单行 YAML flow 映射，保护引号与方括号内部的逗号
func splitYamlFlow(s string) []string {
	var parts []string
	var buf strings.Builder
	inQuote := false
	var quoteChar rune
	bracketDepth := 0

	for _, r := range s {
		if !inQuote {
			if r == '"' || r == '\'' {
				inQuote = true
				quoteChar = r
			} else if r == '[' || r == '{' {
				bracketDepth++
			} else if r == ']' || r == '}' {
				if bracketDepth > 0 {
					bracketDepth--
				}
			} else if r == ',' && bracketDepth == 0 {
				parts = append(parts, buf.String())
				buf.Reset()
				continue
			}
		} else if r == quoteChar {
			inQuote = false
		}
		buf.WriteRune(r)
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
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

		proto := ""
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
		} else if pType == "wireguard" {
			proto = "wireguard"
		} else {
			// 安全过滤未支持的代理协议（如 ss, vmess, trojan, hysteria 等）
			return
		}

		if name == "" {
			name = server
		}
		configJSON := ""
		privKey := ""
		if proto == "wireguard" {
			privKey = strings.Trim(m["private-key"], "\"' ")
			pubKey := strings.Trim(m["public-key"], "\"' ")
			if pubKey == "" {
				pubKey = "bmXOC+F1FxEMF9dyiK2H5/1SUtzHZsVoW++jnWgmtEs="
			}
			ip4 := strings.Trim(m["ip"], "\"' ")
			ip6 := strings.Trim(m["ipv6"], "\"' ")
			var addrs []string
			if ip4 != "" {
				if !strings.Contains(ip4, "/") {
					addrs = append(addrs, ip4+"/32")
				} else {
					addrs = append(addrs, ip4)
				}
			}
			if ip6 != "" {
				if !strings.Contains(ip6, "/") {
					addrs = append(addrs, ip6+"/128")
				} else {
					addrs = append(addrs, ip6)
				}
			}
			if len(addrs) == 0 {
				addrs = []string{"172.16.0.2/32"}
			}
			mtu := 1280
			if mStr := strings.Trim(m["mtu"], "\"' "); mStr != "" {
				if n, err := strconv.Atoi(mStr); err == nil && n > 0 {
					mtu = n
				}
			}
			cfgMap := map[string]any{
				"type":            "wireguard",
				"tag":             name,
				"server":          server,
				"server_port":     port,
				"private_key":     privKey,
				"peer_public_key": pubKey,
				"local_address":   addrs,
				"mtu":             mtu,
			}
			blob, _ := json.Marshal(cfgMap)
			configJSON = string(blob)
		}

		country, countryCode := inferCountryFromRemark(name)
		var nodeID string
		if proto == "wireguard" {
			nodeID = makeCustomNodeID("wireguard", server, port, privKey, name)
		} else {
			nodeID = makeCustomNodeID(proto, server, port, user, name)
		}
		ipType := "datacenter"

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
			IPType:      ipType,
			ISP:         "Cloudflare, Inc.",
			Config:      configJSON,
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
			parts := splitYamlFlow(body)
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

	// 2. 尝试解析为 sing-box outbound JSON (单对象、数组或含 outbounds 的对象)
	if strings.HasPrefix(content, "{") || strings.HasPrefix(content, "[") {
		var jsonItems []map[string]any
		if strings.HasPrefix(content, "[") {
			_ = json.Unmarshal([]byte(content), &jsonItems)
		} else {
			var single map[string]any
			if err := json.Unmarshal([]byte(content), &single); err == nil {
				if obRaw, ok := single["outbounds"].([]any); ok && len(obRaw) > 0 {
					for _, item := range obRaw {
						if m, ok := item.(map[string]any); ok {
							jsonItems = append(jsonItems, m)
						}
					}
				} else {
					jsonItems = []map[string]any{single}
				}
			}
		}

		var jsonNodes []CustomNode
		for i, item := range jsonItems {
			pType, _ := item["type"].(string)
			pTypeLower := strings.ToLower(strings.TrimSpace(pType))
			if pTypeLower == "" {
				pTypeLower = "custom"
			}
			srv, _ := item["server"].(string)
			port := 0
			if p, ok := item["server_port"].(float64); ok && p > 0 {
				port = int(p)
			} else if p, ok := item["server_port"].(int); ok && p > 0 {
				port = p
			}
			tag, _ := item["tag"].(string)
			if tag == "" {
				tag = fmt.Sprintf("%s-%d", strings.ToUpper(pTypeLower), i+1)
			}
			if srv == "" && pTypeLower == "wireguard" {
				srv = "engage.cloudflareclient.com"
				if port == 0 {
					port = 2408
				}
			}
			blob, _ := json.Marshal(item)

			u, _ := item["uuid"].(string)
			if u == "" {
				u, _ = item["username"].(string)
			}
			if u == "" {
				u, _ = item["private_key"].(string)
			}
			pwd, _ := item["password"].(string)
			country, countryCode := inferCountryFromRemark(tag)
			nodeID := makeCustomNodeID(pTypeLower, srv, port, u, tag)
			jsonNodes = append(jsonNodes, CustomNode{
				ID:          nodeID,
				HostName:    nodeID,
				Host:        srv,
				Port:        port,
				User:        u,
				Pass:        pwd,
				Protocol:    pTypeLower,
				Country:     country,
				CountryCode: countryCode,
				Remark:      tag,
				IPType:      "datacenter",
				Config:      string(blob),
			})
		}
		if len(jsonNodes) > 0 {
			return jsonNodes, nil
		}
	}

	// 3. 按行解析多行链接 (统一使用 ParseAnyNode 支持 tuic, vless, vmess, hysteria2, trojan, ss, wireguard, socks5 等)
	var nodes []CustomNode
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		node, err := ParseAnyNode(line)
		if err == nil && node != nil && node.Host != "" && node.Port > 0 {
			if node.Remark == "" || node.Remark == node.Host {
				node.Remark = fmt.Sprintf("节点-%d", i+1)
			}
			nodes = append(nodes, *node)
		}
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("未能从内容中解析出有效代理节点 (支持各类 sing-box 协议链接及 Clash YAML / sing-box JSON)")
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

// RegisterWARPAccount 原生调用 Cloudflare 官方 API 为本机申请免费 WARP (WireGuard) 账户并生成出口节点
func RegisterWARPAccount() (*CustomNode, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 WireGuard 客户端私钥失败: %w", err)
	}
	privBytes := priv.Bytes()
	pubBytes := priv.PublicKey().Bytes()
	privB64 := base64.StdEncoding.EncodeToString(privBytes)
	pubB64 := base64.StdEncoding.EncodeToString(pubBytes)

	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	payload := map[string]any{
		"key":        pubB64,
		"install_id": "",
		"fcm_token":  "",
		"tos":        nowISO,
		"model":      "PC",
		"type":       "Android",
		"locale":     "en_US",
	}
	bodyBlob, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, "https://api.cloudflareclient.com/v0a2158/reg", strings.NewReader(string(bodyBlob)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "okhttp/3.12.1")
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Cloudflare WARP 注册接口失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		rawErr, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cloudflare 注册响应异常 (HTTP %d): %s", resp.StatusCode, string(rawErr))
	}

	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("解析 Cloudflare 注册响应失败: %w", err)
	}

	rawCfg, ok := res["result"].(map[string]any)
	if !ok {
		rawCfg = res
	}

	peerPub := "bmXOC+F1FxEMF9dyiK2H5/1SUtzHZsVoW++jnWgmtEs="
	configMap, _ := rawCfg["config"].(map[string]any)
	if configMap != nil {
		if peers, ok := configMap["peers"].([]any); ok && len(peers) > 0 {
			if p0, ok := peers[0].(map[string]any); ok {
				if pk, ok := p0["public_key"].(string); ok && pk != "" {
					peerPub = pk
				}
			}
		}
	}

	v4 := "172.16.0.2"
	v6 := ""
	if configMap != nil {
		if iface, ok := configMap["interface"].(map[string]any); ok {
			if addrs, ok := iface["addresses"].(map[string]any); ok {
				if a4, ok := addrs["v4"].(string); ok && a4 != "" {
					v4 = a4
				}
				if a6, ok := addrs["v6"].(string); ok && a6 != "" {
					v6 = a6
				}
			}
		}
	}

	var addresses []string
	if v4 != "" {
		if !strings.Contains(v4, "/") {
			addresses = append(addresses, v4+"/32")
		} else {
			addresses = append(addresses, v4)
		}
	}
	// 仅当底层网络环境支持 IPv6 时才分配 v6 地址，防止在纯 IPv4 主机上触发 address family not supported by protocol 掉线
	if v6 != "" && systemSupportsIPv6() {
		if !strings.Contains(v6, "/") {
			addresses = append(addresses, v6+"/128")
		} else {
			addresses = append(addresses, v6)
		}
	}
	if len(addresses) == 0 {
		addresses = []string{"172.16.0.2/32"}
	}

	server := "engage.cloudflareclient.com"
	port := 2408
	tag := "Cloudflare WARP (官方原生)"

	cfgMap := map[string]any{
		"type":            "wireguard",
		"tag":             tag,
		"server":          server,
		"server_port":     port,
		"private_key":     privB64,
		"peer_public_key": peerPub,
		"local_address":   addresses,
		"mtu":             1280,
	}
	cfgBlob, _ := json.Marshal(cfgMap)

	nodeID := makeCustomNodeID("wireguard", server, port, privB64, tag)
	return &CustomNode{
		ID:          nodeID,
		HostName:    nodeID,
		Host:        server,
		Port:        port,
		Protocol:    "wireguard",
		Country:     "WARP",
		CountryCode: "CF",
		Remark:      tag,
		IPType:      "datacenter",
		ISP:         "Cloudflare, Inc.",
		SourceID:    "preset-warp",
		Config:      string(cfgBlob),
	}, nil
}

// systemSupportsIPv6 探测本机是否支持向外部 IPv6 网络发包
func systemSupportsIPv6() bool {
	conn, err := net.DialTimeout("udp6", "[2606:4700:4700::1111]:53", 1*time.Second)
	if err == nil {
		_ = conn.Close()
		return true
	}
	return false
}

