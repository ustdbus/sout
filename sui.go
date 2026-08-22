package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	suiDBPathDefault = "/usr/local/s-ui/db/s-ui.db"
	suiBinaryDefault = "/usr/local/s-ui/sui"
	suiTagPrefix     = "fanout-"
	suiTokenFile     = "sui-token"
)

var (
	cachedSUIToken   string
	cachedSUITokenMu sync.Mutex
)

// SUI 结构体，对接本机 s-ui 面板
type SUI struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	BasePath string `json:"base_path"`
	Scheme   string `json:"scheme"`
	token    string
	dbPath   string
	client   *http.Client
	workDir  string
}

func (s *SUI) Kind() string { return "s-ui" }

func (s *SUI) Describe() string {
	return fmt.Sprintf("接管本机 s-ui 面板（%s:%d）", s.Host, s.Port)
}

func (s *SUI) base() string {
	return fmt.Sprintf("%s://%s:%d%s", s.Scheme, s.Host, s.Port, s.BasePath)
}

func localSUIClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func suiRunning() bool {
	if exec.Command("systemctl", "is-active", "--quiet", "s-ui").Run() == nil {
		return true
	}
	if exec.Command("pgrep", "-x", "sui").Run() == nil {
		return true
	}
	return false
}

func (s *SUI) sqliteQuery(query string) string {
	out, err := exec.Command("sqlite3", s.dbPath, query).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectSUI 自动探测本机 s-ui 安装状态、端口、路径并提取或生成 Token
func DetectSUI(workDir string) (*SUI, error) {
	dbPath := suiDBPathDefault
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		altPath := "/etc/s-ui/s-ui.db"
		if _, err := os.Stat(altPath); err == nil {
			dbPath = altPath
		} else {
			return nil, fmt.Errorf("未检测到 s-ui 数据库文件")
		}
	}

	sqliteExec := func(query string) string {
		out, err := exec.Command("sqlite3", dbPath, query).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	portStr := sqliteExec("SELECT value FROM settings WHERE key='webPort';")
	port := 2095
	if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
		port = p
	}

	basePath := sqliteExec("SELECT value FROM settings WHERE key='webPath';")
	if basePath == "" {
		basePath = "/app/"
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}

	scheme := "http"
	certFile := sqliteExec("SELECT value FROM settings WHERE key='webCertFile';")
	keyFile := sqliteExec("SELECT value FROM settings WHERE key='webKeyFile';")
	if certFile != "" && keyFile != "" {
		scheme = "https"
	}

	newSUI := func(token string) *SUI {
		return &SUI{
			Host:     "127.0.0.1",
			Port:     port,
			BasePath: basePath,
			Scheme:   scheme,
			token:    token,
			dbPath:   dbPath,
			client:   localSUIClient(),
			workDir:  workDir,
		}
	}

	cachedSUITokenMu.Lock()
	defer cachedSUITokenMu.Unlock()
	if cachedSUIToken != "" {
		s := newSUI(cachedSUIToken)
		if s.tokenValid() {
			s.syncSUIDatabaseLinks(hostPublicIP())
			return s, nil
		}
	}

	if workDir != "" {
		saved := readSavedTokenFile(workDir, suiTokenFile)
		if saved != "" {
			s := newSUI(saved)
			if s.tokenValid() {
				cachedSUIToken = saved
				s.syncSUIDatabaseLinks(hostPublicIP())
				return s, nil
			}
		}
	}

	existingToken := sqliteExec("SELECT token FROM tokens WHERE desc='fanout' AND (expiry=0 OR expiry > " + strconv.FormatInt(time.Now().Unix(), 10) + ") LIMIT 1;")
	if existingToken != "" {
		s := newSUI(existingToken)
		if s.tokenValid() {
			cachedSUIToken = existingToken
			if workDir != "" {
				saveTokenFile(workDir, suiTokenFile, existingToken)
			}
			s.syncSUIDatabaseLinks(hostPublicIP())
			return s, nil
		}
	}

	newToken := generateRandomToken(32)
	adminIDStr := sqliteExec("SELECT id FROM users LIMIT 1;")
	adminID := "1"
	if adminIDStr != "" {
		adminID = adminIDStr
	}
	_ = exec.Command("sqlite3", dbPath, fmt.Sprintf("INSERT INTO tokens (desc, token, expiry, user_id) VALUES ('fanout', '%s', 0, %s);", newToken, adminID)).Run()

	_ = exec.Command("systemctl", "restart", "s-ui").Run()
	time.Sleep(2 * time.Second)

	s := newSUI(newToken)
	cachedSUIToken = newToken
	if workDir != "" {
		saveTokenFile(workDir, suiTokenFile, newToken)
	}
	s.syncSUIDatabaseLinks(hostPublicIP())

	return s, nil
}

func readSavedTokenFile(workDir, filename string) string {
	blob, err := os.ReadFile(filepath.Join(workDir, filename))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(blob))
}

func saveTokenFile(workDir, filename, token string) {
	path := filepath.Join(workDir, filename)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		log.Printf("保存 Token 失败: %v", err)
	}
}

func generateRandomToken(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[r.Intn(len(letters))]
	}
	return string(b)
}

func (s *SUI) tokenValid() bool {
	_, err := s.callAPI(http.MethodGet, "inbounds", nil)
	return err == nil
}

func (s *SUI) callAPI(method, endpoint string, form url.Values) ([]byte, error) {
	fullURL := fmt.Sprintf("%s/apiv2/%s", strings.TrimSuffix(s.base(), "/"), strings.TrimPrefix(endpoint, "/"))
	var reqBody io.Reader
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return nil, err
	}

	// 传递真实的母机公网 IP / 域名作为 Host，避免 s-ui 将连接地址误记为 127.0.0.1
	pubIP := hostPublicIP()
	if pubIP != "" {
		req.Host = pubIP
		req.Header.Set("X-Forwarded-Host", pubIP)
	}

	req.Header.Set("Token", s.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 s-ui API (%s) 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var res struct {
		Success bool            `json:"success"`
		Msg     string          `json:"msg"`
		Obj     json.RawMessage `json:"obj"`
	}
	if err := json.Unmarshal(respData, &res); err != nil {
		return nil, fmt.Errorf("解析 s-ui 响应失败: %s", string(respData))
	}
	if !res.Success {
		return nil, fmt.Errorf("s-ui API 报错: %s", res.Msg)
	}

	return res.Obj, nil
}

// Inbounds 获取 s-ui 中的所有入站并关联绑定状态
func (s *SUI) Inbounds(live map[string]bool) ([]Inbound, error) {
	obj, err := s.callAPI(http.MethodGet, "inbounds", nil)
	if err != nil {
		return nil, err
	}

	var rawWrap struct {
		Inbounds []struct {
			ID         int             `json:"id"`
			Type       string          `json:"type"`
			Tag        string          `json:"tag"`
			Listen     string          `json:"listen"`
			ListenPort int             `json:"listen_port"`
			OutJson    json.RawMessage `json:"out_json"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(obj, &rawWrap); err != nil {
		return nil, fmt.Errorf("解析入站列表失败: %w", err)
	}

	boundMap, err := s.getBoundRoutes()
	if err != nil {
		boundMap = make(map[string]string)
	}

	var list []Inbound
	for _, item := range rawWrap.Inbounds {
		boundHost := boundMap[item.Tag]
		list = append(list, Inbound{
			ID:       item.ID,
			Port:     item.ListenPort,
			Protocol: item.Type,
			Remark:   item.Tag,
			Enable:   true,
			Tag:      item.Tag,
			BoundTo:  boundHost,
			BoundUp:  live[boundHost],
		})
	}
	return list, nil
}

func (s *SUI) getBoundRoutes() (map[string]string, error) {
	configObj, err := s.callAPI(http.MethodGet, "config", nil)
	if err != nil {
		return nil, err
	}

	var cfg struct {
		Config struct {
			Route struct {
				Rules []map[string]any `json:"rules"`
			} `json:"route"`
		} `json:"config"`
	}
	if err := json.Unmarshal(configObj, &cfg); err != nil {
		return nil, err
	}

	bound := make(map[string]string)
	for _, rule := range cfg.Config.Route.Rules {
		outbound, _ := rule["outbound"].(string)
		if !strings.HasPrefix(outbound, suiTagPrefix) {
			continue
		}
		host := strings.TrimPrefix(outbound, suiTagPrefix)
		if inbList, ok := rule["inbound"].([]any); ok {
			for _, inb := range inbList {
				if tagStr, ok := inb.(string); ok {
					bound[tagStr] = host
				}
			}
		} else if tagStr, ok := rule["inbound"].(string); ok {
			bound[tagStr] = host
		}
	}
	return bound, nil
}

// Bind 绑定/解绑某个入站到指定隧道。hostname 为空时解除绑定变回直连。
func (s *SUI) Bind(inboundTag string, hostname string, tunnels []*Tunnel) error {
	if err := s.syncOutbounds(tunnels); err != nil {
		return fmt.Errorf("同步 s-ui 出站失败: %w", err)
	}

	configObj, err := s.callAPI(http.MethodGet, "config", nil)
	if err != nil {
		return err
	}

	var cfg map[string]any
	if err := json.Unmarshal(configObj, &cfg); err != nil {
		return err
	}
	rawConfig, _ := cfg["config"].(map[string]any)
	if rawConfig == nil {
		rawConfig = make(map[string]any)
	}
	route, _ := rawConfig["route"].(map[string]any)
	if route == nil {
		route = make(map[string]any)
	}
	rules, _ := route["rules"].([]any)

	validTags := make(map[string]bool)
	if allInb, err := s.Inbounds(nil); err == nil {
		for _, inb := range allInb {
			validTags[inb.Tag] = true
		}
	}

	newRules := make([]any, 0, len(rules)+1)
	for _, r := range rules {
		ruleMap, ok := r.(map[string]any)
		if !ok {
			newRules = append(newRules, r)
			continue
		}
		outbound, _ := ruleMap["outbound"].(string)
		if strings.HasPrefix(outbound, suiTagPrefix) {
			inbs := toSUITagSlice(ruleMap["inbound"])
			var remainInbs []string
			for _, item := range inbs {
				if item != inboundTag && validTags[item] {
					remainInbs = append(remainInbs, item)
				}
			}
			if len(remainInbs) > 0 {
				ruleMap["inbound"] = remainInbs
				newRules = append(newRules, ruleMap)
			}
			continue
		}
		newRules = append(newRules, r)
	}

	if hostname != "" {
		newRules = append(newRules, map[string]any{
			"inbound":  []string{inboundTag},
			"outbound": suiTagPrefix + sanitizeTag(hostname),
		})
	}

	route["rules"] = newRules
	rawConfig["route"] = route

	configBytes, _ := json.Marshal(rawConfig)
	form := url.Values{
		"object": {"config"},
		"action": {"edit"},
		"data":   {string(configBytes)},
	}
	if _, err := s.callAPI(http.MethodPost, "save", form); err != nil {
		return fmt.Errorf("保存路由配置失败: %w", err)
	}

	s.restartSingBox()

	return nil
}

func (s *SUI) restartSingBox() {
	_, _ = s.callAPI(http.MethodPost, "restartSb", nil)
}

func (s *SUI) syncOutbounds(tunnels []*Tunnel) error {
	outboundsObj, err := s.callAPI(http.MethodGet, "outbounds", nil)
	if err != nil {
		return err
	}

	var rawWrap struct {
		Outbounds []struct {
			ID   int    `json:"id"`
			Tag  string `json:"tag"`
			Type string `json:"type"`
		} `json:"outbounds"`
	}
	_ = json.Unmarshal(outboundsObj, &rawWrap)

	existingFanoutTags := make(map[string]int)
	for _, ob := range rawWrap.Outbounds {
		if strings.HasPrefix(ob.Tag, suiTagPrefix) {
			existingFanoutTags[ob.Tag] = ob.ID
		}
	}

	activeTags := make(map[string]bool)
	for _, t := range tunnels {
		if t.Status != "up" {
			continue
		}
		tag := suiTagPrefix + sanitizeTag(t.Node.HostName)
		activeTags[tag] = true

		cred := t.credential()
		outboundPayload := map[string]any{
			"type":        "socks",
			"tag":         tag,
			"server":      "127.0.0.1",
			"server_port": t.Port,
			"version":     "5",
		}
		if cred.User != "" {
			outboundPayload["username"] = cred.User
			outboundPayload["password"] = cred.Pass
		}

		action := "new"
		if id, exists := existingFanoutTags[tag]; exists {
			action = "edit"
			outboundPayload["id"] = id
		}

		payloadBytes, _ := json.Marshal(outboundPayload)
		form := url.Values{
			"object": {"outbounds"},
			"action": {action},
			"data":   {string(payloadBytes)},
		}
		_, _ = s.callAPI(http.MethodPost, "save", form)
	}

	for tag := range existingFanoutTags {
		if !activeTags[tag] {
			tagBytes, _ := json.Marshal(tag)
			form := url.Values{
				"object": {"outbounds"},
				"action": {"del"},
				"data":   {string(tagBytes)},
			}
			_, _ = s.callAPI(http.MethodPost, "save", form)
		}
	}

	return nil
}

// countryNameCN 把国家代码和英文名转换为中文国家名称
func countryNameCN(code, name string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "JP":
		return "日本"
	case "US":
		return "美国"
	case "KR":
		return "韩国"
	case "HK":
		return "香港"
	case "TW":
		return "台湾"
	case "SG":
		return "新加坡"
	case "DE":
		return "德国"
	case "GB", "UK":
		return "英国"
	case "CA":
		return "加拿大"
	case "AU":
		return "澳大利亚"
	case "FR":
		return "法国"
	case "RU":
		return "俄罗斯"
	case "TH":
		return "泰国"
	case "VN":
		return "越南"
	case "MY":
		return "马来西亚"
	case "PH":
		return "菲律宾"
	case "IN":
		return "印度"
	case "BR":
		return "巴西"
	case "NL":
		return "荷兰"
	case "IT":
		return "意大利"
	case "ES":
		return "西班牙"
	case "SE":
		return "瑞典"
	case "CH":
		return "瑞士"
	case "UA":
		return "乌克兰"
	case "ID":
		return "印尼"
	case "TR":
		return "土耳其"
	default:
		if name != "" {
			return name
		}
		if code != "" {
			return code
		}
		return "海外"
	}
}

// CloneToTunnels 为每个隧道克隆一个新入站，保留原模板节点为直连，新节点命名为：原名称 (地区+家宽)
func (s *SUI) CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]int, error) {
	inboundsObj, err := s.callAPI(http.MethodGet, fmt.Sprintf("inbounds?id=%d", templateID), nil)
	if err != nil {
		return nil, err
	}
	var rawWrap struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(inboundsObj, &rawWrap); err != nil || len(rawWrap.Inbounds) == 0 {
		return nil, fmt.Errorf("未找到模板入站 %d", templateID)
	}
	tpl := rawWrap.Inbounds[0]
	origTag, _ := tpl["tag"].(string)
	if origTag == "" {
		origTag = fmt.Sprintf("inbound-%d", templateID)
	}

	// 获取母模板配置中的 server 地址（如域名或公网 IP）
	serverHost := hostPublicIP()
	if tplOutJSONStr, ok := tpl["out_json"].(string); ok && tplOutJSONStr != "" {
		var tplOut struct {
			Server string `json:"server"`
		}
		if json.Unmarshal([]byte(tplOutJSONStr), &tplOut) == nil && tplOut.Server != "" && tplOut.Server != "127.0.0.1" && tplOut.Server != "localhost" {
			serverHost = tplOut.Server
		}
	}
	if serverHost == "" {
		serverHost = s.Host
	}

	usedPorts := make(map[int]bool)
	allInbounds, _ := s.Inbounds(nil)
	for _, inb := range allInbounds {
		usedPorts[inb.Port] = true
	}

	byHost := make(map[string]*Tunnel)
	for _, t := range tunnels {
		byHost[t.Node.HostName] = t
	}

	clientIDs := s.sqliteQuery("SELECT GROUP_CONCAT(id) FROM clients WHERE enable = 1;")
	if clientIDs == "" {
		clientIDs = s.sqliteQuery("SELECT GROUP_CONCAT(id) FROM clients;")
	}

	var createdPorts []int
	for _, host := range hosts {
		t := byHost[host]
		if t == nil || t.Status != "up" {
			continue
		}

		port, err := freeRandomPort(usedPorts)
		if err != nil {
			return createdPorts, err
		}
		usedPorts[port] = true

		cName := countryNameCN(t.Node.CountryCode, t.Node.Country)
		newTag := fmt.Sprintf("%s (%s家宽)", origTag, cName)
		for _, inb := range allInbounds {
			if inb.Tag == newTag {
				newTag = fmt.Sprintf("%s (%s家宽-%d)", origTag, cName, port)
				break
			}
		}

		clone := make(map[string]any)
		for k, v := range tpl {
			clone[k] = v
		}
		delete(clone, "id")
		clone["tag"] = newTag
		clone["listen_port"] = port

		// 明确指定 addrs，确保 s-ui 生成客户端链接与 out_json 时写入正确的公网 IP / 域名和端口
		clone["addrs"] = []map[string]any{
			{
				"server":      serverHost,
				"server_port": port,
				"remark":      newTag,
			},
		}

		cloneBytes, _ := json.Marshal(clone)
		form := url.Values{
			"object":    {"inbounds"},
			"action":    {"new"},
			"data":      {string(cloneBytes)},
			"initUsers": {clientIDs},
		}
		_, err = s.callAPI(http.MethodPost, "save", form)
		if err != nil {
			return createdPorts, fmt.Errorf("创建入站 (%s) 失败: %w", newTag, err)
		}

		if err := s.Bind(newTag, t.Node.HostName, tunnels); err != nil {
			return createdPorts, fmt.Errorf("绑定入站 %s 失败: %w", newTag, err)
		}

		// 同步修复 s-ui 数据库中 inbounds.out_json 和 clients.links，确保两边完全一致
		s.syncSUIDatabaseLinks(serverHost)

		createdPorts = append(createdPorts, port)
	}

	return createdPorts, nil
}

// syncSUIDatabaseLinks 深度同步 s-ui 数据库中的 inbounds.out_json 与 clients.links
func (s *SUI) syncSUIDatabaseLinks(publicHost string) {
	if publicHost == "" {
		publicHost = hostPublicIP()
	}
	if publicHost == "" {
		return
	}

	// 1. 将 inbounds 表中 server 为 127.0.0.1 的记录替换为 publicHost
	_ = s.sqliteQuery(fmt.Sprintf(
		"UPDATE inbounds SET out_json = replace(out_json, '\"server\": \"127.0.0.1\"', '\"server\": \"%s\"') WHERE out_json LIKE '%%\"server\": \"127.0.0.1\"%%';",
		publicHost,
	))

	// 2. 重新构建并刷入 clients.links
	clientCfgJSON := s.sqliteQuery("SELECT config FROM clients WHERE enable = 1 LIMIT 1;")
	if clientCfgJSON == "" {
		clientCfgJSON = s.sqliteQuery("SELECT config FROM clients LIMIT 1;")
	}
	if clientCfgJSON == "" {
		return
	}

	allInbounds, err := s.Inbounds(nil)
	if err != nil {
		return
	}

	type SUIClientLink struct {
		Remark string `json:"remark"`
		Type   string `json:"type"`
		URI    string `json:"uri"`
	}

	var fixedLinks []SUIClientLink
	for _, inb := range allInbounds {
		outJSON := s.sqliteQuery(fmt.Sprintf("SELECT out_json FROM inbounds WHERE id = %d;", inb.ID))
		if outJSON == "" {
			continue
		}
		linkURI := s.buildLinkFromOutJson([]byte(outJSON), []byte(clientCfgJSON), publicHost, inb.Tag)
		if linkURI != "" {
			fixedLinks = append(fixedLinks, SUIClientLink{
				Remark: inb.Tag,
				Type:   "local",
				URI:    linkURI,
			})
		}
	}

	if len(fixedLinks) > 0 {
		linksJSON, err := json.MarshalIndent(fixedLinks, "", "  ")
		if err == nil {
			escapedJSON := strings.ReplaceAll(string(linksJSON), "'", "''")
			_ = s.sqliteQuery(fmt.Sprintf("UPDATE clients SET links = CAST('%s' AS BLOB);", escapedJSON))
		}
	}
}

func (s *SUI) Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error {
	boundMap, err := s.getBoundRoutes()
	if err != nil {
		return err
	}
	oldHostTag := sanitizeTag(oldHost)
	for inbTag, host := range boundMap {
		if host == oldHost || host == oldHostTag {
			if err := s.Bind(inbTag, target.Node.HostName, tunnels); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *SUI) ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error {
	return s.syncOutbounds(tunnels)
}

// DeleteInbounds 删除入站。通过 tag 删除 s-ui 中的入站并清理相关路由规则
func (s *SUI) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	inbounds, err := s.Inbounds(nil)
	if err != nil {
		return err
	}
	tagMap := make(map[int]string)
	for _, inb := range inbounds {
		tagMap[inb.ID] = inb.Tag
	}

	for _, id := range ids {
		tag, ok := tagMap[id]
		if !ok {
			continue
		}
		tagBytes, _ := json.Marshal(tag)
		form := url.Values{
			"object": {"inbounds"},
			"action": {"del"},
			"data":   {string(tagBytes)},
		}
		_, _ = s.callAPI(http.MethodPost, "save", form)

		// 清理该 tag 的分流规则
		_ = s.Bind(tag, "", tunnels)
	}

	s.syncSUIDatabaseLinks(hostPublicIP())

	return nil
}

func (s *SUI) inboundClients(inboundID int) []string {
	query := fmt.Sprintf("SELECT clients.name FROM clients, json_each(clients.inbounds) as je WHERE je.value = %d", inboundID)
	out := s.sqliteQuery(query)
	if out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	var names []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}

func (s *SUI) InboundDetail(id int, publicHost string) (*InboundDetail, error) {
	inbounds, err := s.Inbounds(nil)
	if err != nil {
		return nil, err
	}
	for _, inb := range inbounds {
		if inb.ID == id {
			detail := &InboundDetail{
				Inbound: Inbound{
					ID:       inb.ID,
					Port:     inb.Port,
					Protocol: inb.Protocol,
					Remark:   inb.Remark,
					Enable:   inb.Enable,
					Tag:      inb.Tag, // 必须完整赋值 Tag 供前端绑定使用
					BoundTo:  inb.BoundTo,
					BoundUp:  inb.BoundUp,
				},
				Listen: "0.0.0.0",
			}
			links, _ := s.InboundLinks([]int{id}, publicHost)
			detail.Links = links

			clients := s.inboundClients(id)
			for _, c := range clients {
				detail.Clients = append(detail.Clients, ClientInfo{
					Email:  c,
					ID:     c,
					Enable: true,
				})
			}
			return detail, nil
		}
	}
	return nil, fmt.Errorf("入站 %d 不存在", id)
}

func (s *SUI) buildLinkFromOutJson(outJsonBytes []byte, clientConfigBytes []byte, publicHost string, tag string) string {
	if len(outJsonBytes) == 0 {
		return ""
	}
	var out struct {
		Type       string `json:"type"`
		Server     string `json:"server"`
		ServerPort int    `json:"server_port"`
		Tag        string `json:"tag"`
		TLS        struct {
			Enabled    bool   `json:"enabled"`
			ServerName string `json:"server_name"`
			Reality    struct {
				Enabled   bool   `json:"enabled"`
				PublicKey string `json:"public_key"`
				ShortID   string `json:"short_id"`
			} `json:"reality"`
			UTLS struct {
				Enabled     bool   `json:"enabled"`
				Fingerprint string `json:"fingerprint"`
			} `json:"utls"`
			Insecure bool `json:"insecure"`
		} `json:"tls"`
		Transport struct {
			Type                string            `json:"type"`
			Path                string            `json:"path"`
			Host                string            `json:"host"`
			Headers             map[string]string `json:"headers"`
			ServiceName         string            `json:"service_name"`
			EarlyDataHeaderName string            `json:"early_data_header_name"`
		} `json:"transport"`
		CongestionControl string `json:"congestion_control"`
	}
	if err := json.Unmarshal(outJsonBytes, &out); err != nil {
		return ""
	}

	var clientCfg map[string]map[string]any
	_ = json.Unmarshal(clientConfigBytes, &clientCfg)

	host := publicHost
	if host == "" {
		host = out.Server
	}
	if host == "127.0.0.1" || host == "localhost" || host == "" {
		if publicHost != "" {
			host = publicHost
		} else {
			host = s.Host
		}
	}

	port := out.ServerPort
	remark := tag
	if remark == "" {
		remark = out.Tag
	}

	switch strings.ToLower(out.Type) {
	case "vless":
		u := clientCfg["vless"]["uuid"]
		uuidStr, _ := u.(string)
		if uuidStr == "" {
			uuidStr = "auto"
		}
		v := url.Values{}
		tp := out.Transport.Type
		if tp == "" {
			tp = "tcp"
		}
		v.Set("type", tp)
		if out.Transport.Path != "" {
			v.Set("path", out.Transport.Path)
		}
		if out.Transport.Headers != nil && out.Transport.Headers["Host"] != "" {
			v.Set("host", out.Transport.Headers["Host"])
		}
		if out.Transport.ServiceName != "" {
			v.Set("serviceName", out.Transport.ServiceName)
		}
		if out.TLS.Enabled {
			if out.TLS.Reality.Enabled {
				v.Set("security", "reality")
				v.Set("pbk", out.TLS.Reality.PublicKey)
				v.Set("sid", out.TLS.Reality.ShortID)
			} else {
				v.Set("security", "tls")
				if out.TLS.Insecure {
					v.Set("allowInsecure", "1")
				}
			}
			if out.TLS.ServerName != "" {
				v.Set("sni", out.TLS.ServerName)
			}
			if out.TLS.UTLS.Fingerprint != "" {
				v.Set("fp", out.TLS.UTLS.Fingerprint)
			}
			if flow, ok := clientCfg["vless"]["flow"].(string); ok && flow != "" && tp == "tcp" {
				v.Set("flow", flow)
			}
		}
		return fmt.Sprintf("vless://%s@%s:%d?%s#%s", uuidStr, host, port, v.Encode(), url.PathEscape(remark))

	case "tuic":
		uuidStr, _ := clientCfg["tuic"]["uuid"].(string)
		passStr, _ := clientCfg["tuic"]["password"].(string)
		v := url.Values{}
		if out.TLS.Enabled {
			v.Set("security", "tls")
			if out.TLS.ServerName != "" {
				v.Set("sni", out.TLS.ServerName)
			}
		}
		if out.CongestionControl != "" {
			v.Set("congestion_control", out.CongestionControl)
		}
		return fmt.Sprintf("tuic://%s:%s@%s:%d?%s#%s", uuidStr, passStr, host, port, v.Encode(), url.PathEscape(remark))

	case "trojan":
		passStr, _ := clientCfg["trojan"]["password"].(string)
		v := url.Values{}
		if out.TLS.Enabled {
			v.Set("security", "tls")
			if out.TLS.ServerName != "" {
				v.Set("sni", out.TLS.ServerName)
			}
		}
		return fmt.Sprintf("trojan://%s@%s:%d?%s#%s", passStr, host, port, v.Encode(), url.PathEscape(remark))
	}

	return ""
}

func (s *SUI) InboundLinks(ids []int, publicHost string) ([]string, error) {
	inbounds, err := s.Inbounds(nil)
	if err != nil {
		return nil, err
	}
	idMap := make(map[int]Inbound)
	for _, inb := range inbounds {
		idMap[inb.ID] = inb
	}

	clientCfgJSON := s.sqliteQuery("SELECT config FROM clients WHERE enable = 1 LIMIT 1;")
	if clientCfgJSON == "" {
		clientCfgJSON = s.sqliteQuery("SELECT config FROM clients LIMIT 1;")
	}

	var links []string
	for _, id := range ids {
		inb, ok := idMap[id]
		if !ok {
			continue
		}
		outJSON := s.sqliteQuery(fmt.Sprintf("SELECT out_json FROM inbounds WHERE id = %d;", id))
		if outJSON != "" {
			l := s.buildLinkFromOutJson([]byte(outJSON), []byte(clientCfgJSON), publicHost, inb.Tag)
			if l != "" {
				links = append(links, l)
				continue
			}
		}
	}
	return links, nil
}

func (s *SUI) CreateInbound(spec NewInboundSpec, tunnels []*Tunnel) (*CreatedInbound, error) {
	return nil, fmt.Errorf("s-ui 模式下请在 s-ui 面板中创建模板入站，或使用新建出口向导")
}

func (s *SUI) UpdateInbound(id int, patch InboundPatch, tunnels []*Tunnel) error {
	inboundsObj, err := s.callAPI(http.MethodGet, fmt.Sprintf("inbounds?id=%d", id), nil)
	if err != nil {
		return err
	}
	var rawWrap struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(inboundsObj, &rawWrap); err != nil || len(rawWrap.Inbounds) == 0 {
		return fmt.Errorf("未找到入站 %d", id)
	}
	inb := rawWrap.Inbounds[0]
	oldTag, _ := inb["tag"].(string)

	if patch.Port != nil {
		inb["listen_port"] = *patch.Port
	}
	if patch.Remark != nil {
		inb["tag"] = *patch.Remark
	}

	dataBytes, _ := json.Marshal(inb)
	form := url.Values{
		"object": {"inbounds"},
		"action": {"edit"},
		"data":   {string(dataBytes)},
	}
	if _, err := s.callAPI(http.MethodPost, "save", form); err != nil {
		return err
	}

	newTag, _ := inb["tag"].(string)
	if patch.Remark != nil && oldTag != "" && oldTag != newTag {
		boundMap, _ := s.getBoundRoutes()
		if host, ok := boundMap[oldTag]; ok {
			_ = s.Bind(oldTag, "", tunnels)
			_ = s.Bind(newTag, host, tunnels)
		}
	}

	return nil
}

func (s *SUI) AddClient(id int, email string, tunnels []*Tunnel) error {
	return nil
}

func (s *SUI) DeleteClient(id int, email string, tunnels []*Tunnel) error {
	return nil
}

func (s *SUI) ResetClient(id int, email string, tunnels []*Tunnel) error {
	return nil
}

func (s *SUI) OnTunnelsChanged(tunnels []*Tunnel) error {
	return s.syncOutbounds(tunnels)
}

func (s *SUI) Close() {}

func toSUITagSlice(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
	if arr, ok := v.([]any); ok {
		var res []string
		for _, item := range arr {
			if s, ok := item.(string); ok {
				res = append(res, s)
			}
		}
		return res
	}
	return nil
}
