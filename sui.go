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

func (s *SUI) sqliteJSONQuery(query string) ([]byte, error) {
	out, err := exec.Command("sqlite3", "-json", s.dbPath, query).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
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
			s.cleanLegacyClonedInbounds()
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
				s.cleanLegacyClonedInbounds()
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
			s.cleanLegacyClonedInbounds()
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
	s.cleanLegacyClonedInbounds()
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

func generateUUID() string {
	b := make([]byte, 16)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	_, _ = r.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
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

// cleanLegacyClonedInbounds 自动清理早期版本克隆的多端口入站与遗留规则
func (s *SUI) cleanLegacyClonedInbounds() {
	rawJSON, err := s.sqliteJSONQuery("SELECT id, tag FROM inbounds WHERE tag LIKE '%家宽%';")
	if err != nil || len(rawJSON) == 0 {
		return
	}
	var inbs []struct {
		ID  int    `json:"id"`
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(rawJSON, &inbs); err != nil {
		return
	}
	for _, inb := range inbs {
		tagBytes, _ := json.Marshal(inb.Tag)
		form := url.Values{
			"object": {"inbounds"},
			"action": {"del"},
			"data":   {string(tagBytes)},
		}
		_, _ = s.callAPI(http.MethodPost, "save", form)
		_ = s.sqliteQuery(fmt.Sprintf("DELETE FROM inbounds WHERE id=%d;", inb.ID))
	}
}

type suiDBClient struct {
	ID       int             `json:"id"`
	Name     string          `json:"name"`
	Remark   string          `json:"remark"`
	Enable   any             `json:"enable"`
	Inbounds json.RawMessage `json:"inbounds"`
	Config   json.RawMessage `json:"config"`
}

func (s *SUI) getDBInboundIDs(inboundsRaw json.RawMessage) []int {
	var ids []int
	if len(inboundsRaw) == 0 {
		return ids
	}
	var str string
	if err := json.Unmarshal(inboundsRaw, &str); err == nil {
		_ = json.Unmarshal([]byte(str), &ids)
		return ids
	}
	_ = json.Unmarshal(inboundsRaw, &ids)
	return ids
}

// getBoundUserRoutes 读取 sing-box 路由规则中按用户 (auth_user) 分流的映射
func (s *SUI) getBoundUserRoutes() (map[string]string, error) {
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
		users := toSUITagSlice(rule["auth_user"])
		if len(users) == 0 {
			users = toSUITagSlice(rule["user"])
		}
		for _, u := range users {
			if u != "" {
				bound[u] = host
			}
		}
	}
	return bound, nil
}

// Inbounds 获取 s-ui 中的所有原生入站并按 Client 用户关联分流分支
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

	clientRowsJSON, _ := s.sqliteJSONQuery("SELECT id, name, remark, enable, inbounds, config FROM clients;")
	var allClients []suiDBClient
	_ = json.Unmarshal(clientRowsJSON, &allClients)

	boundUserMap, err := s.getBoundUserRoutes()
	if err != nil {
		boundUserMap = make(map[string]string)
	}

	var list []Inbound
	for _, item := range rawWrap.Inbounds {
		if isResidentialBranch(item.Tag) {
			continue // 忽略旧版克隆入站
		}

		var matchedClients []suiDBClient
		for _, c := range allClients {
			ids := s.getDBInboundIDs(c.Inbounds)
			for _, idVal := range ids {
				if idVal == item.ID {
					matchedClients = append(matchedClients, c)
					break
				}
			}
		}

		if len(matchedClients) == 0 {
			// 没有找到 client 时，默认生成基础直连项
			list = append(list, Inbound{
				ID:       item.ID,
				Port:     item.ListenPort,
				Protocol: item.Type,
				Remark:   item.Tag,
				Enable:   true,
				Tag:      item.Tag,
				BoundTo:  "",
				IsBase:   true,
			})
			continue
		}

		for _, c := range matchedClients {
			boundHost := boundUserMap[c.Name]
			isBase := boundHost == ""
			branchTag := item.Tag
			if !isBase {
				if c.Remark != "" {
					branchTag = fmt.Sprintf("%s (%s)", item.Tag, c.Remark)
				} else {
					branchTag = fmt.Sprintf("%s (%s)", item.Tag, c.Name)
				}
			}

			list = append(list, Inbound{
				ID:       item.ID,
				ClientID: c.ID,
				Port:     item.ListenPort,
				Protocol: item.Type,
				Remark:   c.Name,
				Enable:   true,
				Tag:      branchTag,
				BoundTo:  boundHost,
				BoundUp:  live[boundHost],
				IsBase:   isBase,
			})
		}
	}
	return list, nil
}

// BindUserRoute 在 sing-box 路由规则中为指定 Client 用户绑定出口隧道
func (s *SUI) BindUserRoute(userName string, hostname string, tunnels []*Tunnel) error {
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

	newRules := make([]any, 0, len(rules)+1)
	for _, r := range rules {
		ruleMap, ok := r.(map[string]any)
		if !ok {
			newRules = append(newRules, r)
			continue
		}
		outbound, _ := ruleMap["outbound"].(string)
		if strings.HasPrefix(outbound, suiTagPrefix) {
			users := toSUITagSlice(ruleMap["auth_user"])
			if len(users) == 0 {
				users = toSUITagSlice(ruleMap["user"])
			}
			var remainUsers []string
			for _, item := range users {
				if item != userName {
					remainUsers = append(remainUsers, item)
				}
			}
			if len(remainUsers) > 0 {
				ruleMap["auth_user"] = remainUsers
				delete(ruleMap, "user")
				ruleMap["action"] = "route"
				newRules = append(newRules, ruleMap)
			}
			continue
		}
		newRules = append(newRules, r)
	}

	if hostname != "" {
		newRules = append(newRules, map[string]any{
			"action":    "route",
			"auth_user": []string{userName},
			"outbound":  suiTagPrefix + sanitizeTag(hostname),
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

func (s *SUI) Bind(inboundTag string, hostname string, tunnels []*Tunnel) error {
	return s.BindUserRoute(inboundTag, hostname, tunnels)
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

// CloneToTunnels 采用单端口多用户 (Client) 架构：在同一个母入站下创建分流 Client，并配置 sing-box 用户路由
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
	listenPort := 0
	if pVal, ok := tpl["listen_port"].(float64); ok {
		listenPort = int(pVal)
	}

	baseFlow := "xtls-rprx-vision"
	baseClientCfgJSON := s.sqliteQuery(fmt.Sprintf(
		"SELECT config FROM clients WHERE enable=1 AND %d IN (SELECT json_each.value FROM json_each(clients.inbounds)) LIMIT 1;",
		templateID,
	))
	if baseClientCfgJSON != "" {
		var baseCfg map[string]map[string]any
		if err := json.Unmarshal([]byte(baseClientCfgJSON), &baseCfg); err == nil {
			if vlessMap, ok := baseCfg["vless"]; ok {
				if f, ok := vlessMap["flow"].(string); ok && f != "" {
					baseFlow = f
				}
			}
		}
	}

	var createdPorts []int
	for _, host := range hosts {
		var targetTunnel *Tunnel
		for _, t := range tunnels {
			if t.Node.HostName == host {
				targetTunnel = t
				break
			}
		}

		cName := "海外"
		if targetTunnel != nil {
			cName = countryNameCN(targetTunnel.Node.CountryCode, targetTunnel.Node.Country)
		}

		clientName := fmt.Sprintf("fanout-u-%d-%s", templateID, sanitizeTag(host))
		clientRemark := fmt.Sprintf("%s家宽", cName)

		existingClientID := s.sqliteQuery(fmt.Sprintf("SELECT id FROM clients WHERE name='%s' LIMIT 1;", clientName))
		if existingClientID == "" {
			newUUID := generateUUID()
			newPass := generateRandomToken(10)

			clientCfgObj := map[string]map[string]any{
				"vless": {
					"name": clientName,
					"uuid": newUUID,
					"flow": baseFlow,
				},
				"tuic": {
					"name":     clientName,
					"uuid":     newUUID,
					"password": newPass,
				},
				"vmess": {
					"name":    clientName,
					"uuid":    newUUID,
					"alterId": 0,
				},
				"trojan": {
					"name":     clientName,
					"password": newPass,
				},
				"shadowsocks": {
					"name":     clientName,
					"password": newPass,
				},
				"hysteria2": {
					"name":     clientName,
					"password": newPass,
				},
			}
			cfgBytes, _ := json.Marshal(clientCfgObj)
			inboundsJSON := fmt.Sprintf("[%d]", templateID)

			insertSQL := fmt.Sprintf(
				"INSERT INTO clients (enable, name, remark, config, inbounds, created_at) VALUES (1, '%s', '%s', CAST('%s' AS BLOB), CAST('%s' AS BLOB), %d);",
				clientName, clientRemark, strings.ReplaceAll(string(cfgBytes), "'", "''"), inboundsJSON, time.Now().Unix(),
			)
			_ = s.sqliteQuery(insertSQL)
		}

		if err := s.BindUserRoute(clientName, host, tunnels); err != nil {
			return createdPorts, fmt.Errorf("绑定分流路由 (%s) 失败: %w", clientName, err)
		}

		createdPorts = append(createdPorts, listenPort)
	}

	s.syncSUIDatabaseLinks(hostPublicIP())
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

	_ = s.sqliteQuery(fmt.Sprintf(
		"UPDATE inbounds SET out_json = replace(out_json, '\"server\": \"127.0.0.1\"', '\"server\": \"%s\"') WHERE out_json LIKE '%%\"server\": \"127.0.0.1\"%%';",
		publicHost,
	))

	clientRowsJSON, _ := s.sqliteJSONQuery("SELECT id, name, remark, enable, inbounds, config FROM clients WHERE enable=1;")
	var clients []suiDBClient
	_ = json.Unmarshal(clientRowsJSON, &clients)

	type SUIClientLink struct {
		Remark string `json:"remark"`
		Type   string `json:"type"`
		URI    string `json:"uri"`
	}

	for _, client := range clients {
		inboundIDs := s.getDBInboundIDs(client.Inbounds)
		var clientLinks []SUIClientLink
		for _, inbID := range inboundIDs {
			outJSON := s.sqliteQuery(fmt.Sprintf("SELECT out_json FROM inbounds WHERE id = %d;", inbID))
			addrsJSON := s.sqliteQuery(fmt.Sprintf("SELECT addrs FROM inbounds WHERE id = %d;", inbID))
			tag := s.sqliteQuery(fmt.Sprintf("SELECT tag FROM inbounds WHERE id = %d;", inbID))
			if outJSON == "" {
				continue
			}
			branchRemark := tag
			if client.Remark != "" {
				branchRemark = fmt.Sprintf("%s (%s)", tag, client.Remark)
			}
			links := s.buildLinksFromInbound([]byte(outJSON), []byte(addrsJSON), client.Config, publicHost, branchRemark)
			for _, linkURI := range links {
				if linkURI != "" {
					clientLinks = append(clientLinks, SUIClientLink{
						Remark: branchRemark,
						Type:   "local",
						URI:    linkURI,
					})
				}
			}
		}
		if len(clientLinks) > 0 {
			linksJSON, err := json.MarshalIndent(clientLinks, "", "  ")
			if err == nil {
				escapedJSON := strings.ReplaceAll(string(linksJSON), "'", "''")
				_ = s.sqliteQuery(fmt.Sprintf("UPDATE clients SET links = CAST('%s' AS BLOB) WHERE id = %d;", escapedJSON, client.ID))
			}
		}
	}
}

func (s *SUI) Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error {
	boundMap, err := s.getBoundUserRoutes()
	if err != nil {
		return err
	}
	oldHostTag := sanitizeTag(oldHost)
	for userName, host := range boundMap {
		if host == oldHost || host == oldHostTag {
			if err := s.BindUserRoute(userName, target.Node.HostName, tunnels); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *SUI) ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error {
	return s.syncOutbounds(tunnels)
}

// DeleteInbounds 删除分流分支。若传入 Client ID，删除该 Client 并移除路由规则；若删光分流，恢复纯净直连。
func (s *SUI) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	for _, id := range ids {
		clientName := s.sqliteQuery(fmt.Sprintf("SELECT name FROM clients WHERE id = %d AND name LIKE 'fanout-u-%%';", id))
		if clientName != "" {
			_ = s.sqliteQuery(fmt.Sprintf("DELETE FROM clients WHERE id = %d;", id))
			_ = s.BindUserRoute(clientName, "", tunnels)
			continue
		}

		inbTag := s.sqliteQuery(fmt.Sprintf("SELECT tag FROM inbounds WHERE id = %d;", id))
		if inbTag != "" && isResidentialBranch(inbTag) {
			tagBytes, _ := json.Marshal(inbTag)
			form := url.Values{
				"object": {"inbounds"},
				"action": {"del"},
				"data":   {string(tagBytes)},
			}
			_, _ = s.callAPI(http.MethodPost, "save", form)
			_ = s.sqliteQuery(fmt.Sprintf("DELETE FROM inbounds WHERE id = %d;", id))
			_ = s.BindUserRoute(inbTag, "", tunnels)
		}
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
					Tag:      inb.Tag,
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

func (s *SUI) buildLinksFromInbound(outJsonBytes, addrsBytes, clientConfigBytes []byte, publicHost string, tag string) []string {
	if len(outJsonBytes) == 0 {
		return nil
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
		return nil
	}

	type AddrItem struct {
		Server     string `json:"server"`
		ServerPort int    `json:"server_port"`
		Remark     string `json:"remark"`
	}
	var addrs []AddrItem
	if len(addrsBytes) > 0 {
		_ = json.Unmarshal(addrsBytes, &addrs)
	}

	if len(addrs) == 0 {
		serverHost := publicHost
		if serverHost == "" {
			serverHost = out.Server
		}
		if serverHost == "127.0.0.1" || serverHost == "localhost" || serverHost == "" {
			if publicHost != "" {
				serverHost = publicHost
			} else {
				serverHost = s.Host
			}
		}
		port := out.ServerPort
		addrs = append(addrs, AddrItem{
			Server:     serverHost,
			ServerPort: port,
			Remark:     tag,
		})
	}

	var clientCfg map[string]map[string]any
	if err := json.Unmarshal(clientConfigBytes, &clientCfg); err != nil {
		var str string
		if err2 := json.Unmarshal(clientConfigBytes, &str); err2 == nil {
			_ = json.Unmarshal([]byte(str), &clientCfg)
		}
	}

	baseRemark := tag
	if baseRemark == "" {
		baseRemark = out.Tag
	}

	var links []string
	for _, addr := range addrs {
		host := addr.Server
		if host == "" {
			host = publicHost
		}
		if host == "127.0.0.1" || host == "localhost" || host == "" {
			if publicHost != "" {
				host = publicHost
			} else {
				host = s.Host
			}
		}
		port := addr.ServerPort
		if port <= 0 {
			port = out.ServerPort
		}

		remark := baseRemark
		if addr.Remark != "" && addr.Remark != baseRemark && !strings.Contains(baseRemark, addr.Remark) {
			remark = baseRemark + "-" + addr.Remark
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
			links = append(links, fmt.Sprintf("vless://%s@%s:%d?%s#%s", uuidStr, host, port, v.Encode(), url.PathEscape(remark)))

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
			links = append(links, fmt.Sprintf("tuic://%s:%s@%s:%d?%s#%s", uuidStr, passStr, host, port, v.Encode(), url.PathEscape(remark)))

		case "trojan":
			passStr, _ := clientCfg["trojan"]["password"].(string)
			v := url.Values{}
			if out.TLS.Enabled {
				v.Set("security", "tls")
				if out.TLS.ServerName != "" {
					v.Set("sni", out.TLS.ServerName)
				}
			}
			links = append(links, fmt.Sprintf("trojan://%s@%s:%d?%s#%s", passStr, host, port, v.Encode(), url.PathEscape(remark)))
		}
	}

	return links
}

func (s *SUI) InboundBranchLinks(inboundID int, clientID int, branchTag string, publicHost string) []string {
	outJSON := s.sqliteQuery(fmt.Sprintf("SELECT out_json FROM inbounds WHERE id = %d;", inboundID))
	addrsJSON := s.sqliteQuery(fmt.Sprintf("SELECT addrs FROM inbounds WHERE id = %d;", inboundID))
	if outJSON == "" {
		return nil
	}

	var clientConfigBytes []byte
	if clientID > 0 {
		raw := s.sqliteQuery(fmt.Sprintf("SELECT config FROM clients WHERE id = %d LIMIT 1;", clientID))
		clientConfigBytes = []byte(raw)
	} else {
		raw := s.sqliteQuery(fmt.Sprintf("SELECT config FROM clients WHERE enable=1 AND %d IN (SELECT json_each.value FROM json_each(clients.inbounds)) LIMIT 1;", inboundID))
		clientConfigBytes = []byte(raw)
	}

	return s.buildLinksFromInbound([]byte(outJSON), []byte(addrsJSON), clientConfigBytes, publicHost, branchTag)
}

func (s *SUI) InboundLinks(ids []int, publicHost string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var allLinks []string
	for _, id := range ids {
		links := s.InboundBranchLinks(id, 0, "", publicHost)
		allLinks = append(allLinks, links...)
	}
	return allLinks, nil
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
