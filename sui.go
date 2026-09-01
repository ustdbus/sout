package main

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
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
	suiTagPrefix     = "sout"
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
	if hasCmd("systemctl") && dirExists("/run/systemd/system") {
		if exec.Command("systemctl", "is-active", "--quiet", "s-ui").Run() == nil {
			return true
		}
	}
	if hasCmd("rc-service") {
		if exec.Command("rc-service", "s-ui", "status").Run() == nil {
			return true
		}
	}
	if exec.Command("pgrep", "-x", "sui").Run() == nil {
		return true
	}
	return false
}

func restartSUI() {
	if hasCmd("systemctl") && dirExists("/run/systemd/system") {
		_ = exec.Command("systemctl", "restart", "s-ui").Run()
	} else if hasCmd("rc-service") {
		_ = exec.Command("rc-service", "s-ui", "restart").Run()
	} else {
		_ = exec.Command("systemctl", "restart", "s-ui").Run()
	}
}

func runSQLite(dbPath string, query string) (string, error) {
	if hasCmd("sqlite3") {
		out, err := exec.Command("sqlite3", dbPath, query).Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	if hasCmd("python3") {
		pyCode := fmt.Sprintf("import sqlite3\ncon = sqlite3.connect(%q)\ncur = con.cursor()\ncur.execute(%q)\nif cur.description:\n    rows = cur.fetchall()\n    for r in rows:\n        print('|'.join(str(x) if x is not None else '' for x in r))\nelse:\n    con.commit()\n", dbPath, query)
		out, err := exec.Command("python3", "-c", pyCode).Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("未找到 sqlite3 或 python3 工具")
}

func runSQLiteJSON(dbPath string, query string) ([]byte, error) {
	if hasCmd("sqlite3") {
		out, err := exec.Command("sqlite3", "-json", dbPath, query).Output()
		if err == nil {
			return out, nil
		}
	}
	if hasCmd("python3") {
		pyCode := fmt.Sprintf("import sqlite3, json\ncon = sqlite3.connect(%q)\ncur = con.cursor()\ncur.execute(%q)\ncols = [d[0] for d in cur.description] if cur.description else []\nrows = []\nfor r in cur.fetchall():\n    row_dict = {}\n    for k, v in zip(cols, r):\n        if isinstance(v, (bytes, bytearray)):\n            v = v.decode('utf-8', errors='ignore')\n        row_dict[k] = v\n    rows.append(row_dict)\nprint(json.dumps(rows))\n", dbPath, query)
		out, err := exec.Command("python3", "-c", pyCode).Output()
		if err == nil {
			return out, nil
		}
	}
	return nil, fmt.Errorf("未找到 sqlite3 或 python3 工具")
}

func (s *SUI) sqliteQuery(query string) string {
	res, _ := runSQLite(s.dbPath, query)
	return res
}

func (s *SUI) sqliteJSONQuery(query string) ([]byte, error) {
	return runSQLiteJSON(s.dbPath, query)
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
		res, _ := runSQLite(dbPath, query)
		return res
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
			_ = s.cleanStaleRoutesAndClients(nil)
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

	existingToken := sqliteExec("SELECT token FROM tokens WHERE (desc='sout' OR desc='fanout') AND (expiry=0 OR expiry > " + strconv.FormatInt(time.Now().Unix(), 10) + ") LIMIT 1;")
	if existingToken != "" {
		s := newSUI(existingToken)
		if s.tokenValid() {
			cachedSUIToken = existingToken
			if workDir != "" {
				saveTokenFile(workDir, suiTokenFile, existingToken)
			}
			s.cleanLegacyClonedInbounds()
			_ = s.cleanStaleRoutesAndClients(nil)
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
	_ = exec.Command("sqlite3", dbPath, fmt.Sprintf("INSERT INTO tokens (desc, token, expiry, user_id) VALUES ('sout', '%s', 0, %s);", newToken, adminID)).Run()

	restartSUI()
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
	rawJSON, err := s.sqliteJSONQuery("SELECT id, tag FROM inbounds WHERE tag LIKE '%家宽%' OR tag LIKE '%机房%';")
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
	Links    json.RawMessage `json:"links"`
	Config   json.RawMessage `json:"config"`
}

// apiClient 是 s-ui API 返回的客户端模型（GET /apiv2/clients）
type apiClient struct {
	ID       int             `json:"id"`
	Enable   bool            `json:"enable"`
	Name     string          `json:"name"`
	Config   json.RawMessage `json:"config"`
	Inbounds json.RawMessage `json:"inbounds"`
	Links    json.RawMessage `json:"links"`
	Remark   string          `json:"remark"`
	Desc     string          `json:"desc"`
	Group    string          `json:"group"`
	Volume   int64           `json:"volume"`
	Expiry   int64           `json:"expiry"`
	Up       int64           `json:"up"`
	Down     int64           `json:"down"`
	DelayStart bool          `json:"delayStart"`
	AutoReset bool           `json:"autoReset"`
	ResetDays int            `json:"resetDays"`
	NextReset int64          `json:"nextReset"`
	TotalUp   int64          `json:"totalUp"`
	TotalDown int64          `json:"totalDown"`
	CreatedAt int64          `json:"createdAt"`
	OnlineAt  int64          `json:"onlineAt"`
}

// apiSaveClient 调用 s-ui 自身的客户端保存接口，由 s-ui 生成/更新订阅链接。
func (s *SUI) apiSaveClient(act string, client map[string]any) error {
	dataBytes, err := json.Marshal(client)
	if err != nil {
		return err
	}
	form := url.Values{
		"object": {"clients"},
		"action": {act},
		"data":   {string(dataBytes)},
	}
	_, err = s.callAPI(http.MethodPost, "save", form)
	return err
}

// apiDeleteClient 调用 s-ui API 删除客户端，避免直接操作 SQLite。
func (s *SUI) apiDeleteClient(id int) error {
	dataBytes, _ := json.Marshal(id)
	form := url.Values{
		"object": {"clients"},
		"action": {"del"},
		"data":   {string(dataBytes)},
	}
	_, err := s.callAPI(http.MethodPost, "save", form)
	return err
}

// apiClients 通过 s-ui API 获取客户端列表；id>0 时返回单个客户端的完整信息。
func (s *SUI) apiClients(id int) ([]map[string]any, error) {
	endpoint := "clients"
	if id > 0 {
		endpoint = fmt.Sprintf("clients?id=%d", id)
	}
	obj, err := s.callAPI(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal(obj, &raw); err != nil {
		return nil, err
	}
	return raw.Clients, nil
}

// apiClientByName 在 s-ui 客户端列表中按名称查找。
func (s *SUI) apiClientByName(name string) (map[string]any, bool, error) {
	clients, err := s.apiClients(0)
	if err != nil {
		return nil, false, err
	}
	for _, c := range clients {
		if n, _ := c["name"].(string); n == name {
			return c, true, nil
		}
	}
	return nil, false, nil
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

func isSUIOutboundTag(outbound string) bool {
	return strings.HasPrefix(outbound, "sout") || strings.HasPrefix(outbound, "fanout")
}

func extractSUIHost(outbound string) string {
	if strings.HasPrefix(outbound, "sout-") {
		return strings.TrimPrefix(outbound, "sout-")
	}
	if strings.HasPrefix(outbound, "sout") {
		return strings.TrimPrefix(outbound, "sout")
	}
	if strings.HasPrefix(outbound, "fanout-") {
		return strings.TrimPrefix(outbound, "fanout-")
	}
	if strings.HasPrefix(outbound, "fanout") {
		return strings.TrimPrefix(outbound, "fanout")
	}
	return outbound
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
		if !isSUIOutboundTag(outbound) {
			continue
		}
		host := extractSUIHost(outbound)
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

	type inbItem struct {
		ID         int             `json:"id"`
		Type       string          `json:"type"`
		Tag        string          `json:"tag"`
		Listen     string          `json:"listen"`
		ListenPort int             `json:"listen_port"`
		OutJson    json.RawMessage `json:"out_json"`
	}
	var inboundsList []inbItem
	var rawWrap struct {
		Inbounds []inbItem `json:"inbounds"`
	}
	if err := json.Unmarshal(obj, &rawWrap); err == nil && len(rawWrap.Inbounds) > 0 {
		inboundsList = rawWrap.Inbounds
	} else {
		_ = json.Unmarshal(obj, &inboundsList)
	}

	clientRowsJSON, _ := s.sqliteJSONQuery("SELECT id, name, remark, enable, inbounds, links, config FROM clients;")
	var allClients []suiDBClient
	_ = json.Unmarshal(clientRowsJSON, &allClients)

	boundUserMap, err := s.getBoundUserRoutes()
	if err != nil {
		boundUserMap = make(map[string]string)
	}

	var list []Inbound
	for _, item := range inboundsList {
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
			isFanoutClient := strings.HasPrefix(c.Name, "soutu") || strings.HasPrefix(c.Name, "sout-u-") || strings.HasPrefix(c.Name, "fanoutu") || strings.HasPrefix(c.Name, "fanout-u-")
			isBase := !isFanoutClient && boundHost == ""
			branchTag := item.Tag
			if !isBase {
				if c.Remark != "" {
					branchTag = fmt.Sprintf("%s (%s)", item.Tag, c.Remark)
				} else if boundHost != "" {
					branchTag = fmt.Sprintf("%s (%s)", item.Tag, boundHost)
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

	validOutboundTags := make(map[string]bool)
	validOutboundTags["direct"] = true
	validOutboundTags["block"] = true
	for _, t := range tunnels {
		if t.Node.HostName != "" {
			validOutboundTags[suiTagPrefix+sanitizeTag(t.Node.HostName)] = true
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
		action, _ := ruleMap["action"].(string)
		if action != "route" {
			newRules = append(newRules, r)
			continue
		}
		if isSUIOutboundTag(outbound) {
			// 如果该出站已经在当前活跃隧道中失效/不存在，直接丢弃该悬空规则
			if !validOutboundTags[outbound] && outbound != (suiTagPrefix+sanitizeTag(hostname)) {
				continue
			}
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

	type outboundItem struct {
		ID   int    `json:"id"`
		Tag  string `json:"tag"`
		Type string `json:"type"`
	}

	var list []outboundItem
	var rawWrap struct {
		Outbounds []outboundItem `json:"outbounds"`
	}
	if err := json.Unmarshal(outboundsObj, &rawWrap); err == nil && len(rawWrap.Outbounds) > 0 {
		list = rawWrap.Outbounds
	} else {
		_ = json.Unmarshal(outboundsObj, &list)
	}

	existingFanoutTags := make(map[string]int)
	for _, ob := range list {
		if isSUIOutboundTag(ob.Tag) {
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

type branchBinding struct {
	TemplateID int    `json:"template_id"`
	Slot       int    `json:"slot,omitempty"`
	Host       string `json:"host,omitempty"`
	Region     string `json:"region,omitempty"`
	PoolType   string `json:"pool_type,omitempty"`
}

func branchBindingsPath(workDir string) string {
	if workDir == "" {
		workDir = "/var/lib/sout"
	}
	return filepath.Join(workDir, "branch_bindings.json")
}

func loadBranchBindings(workDir string) []branchBinding {
	blob, err := os.ReadFile(branchBindingsPath(workDir))
	if err != nil {
		return nil
	}
	var list []branchBinding
	_ = json.Unmarshal(blob, &list)
	return list
}

func saveBranchBinding(workDir string, binding branchBinding) {
	list := loadBranchBindings(workDir)
	var updated []branchBinding
	exists := false
	for _, b := range list {
		if b.TemplateID == binding.TemplateID && (b.Host == binding.Host || (b.Slot > 0 && b.Slot == binding.Slot)) {
			updated = append(updated, binding)
			exists = true
		} else {
			updated = append(updated, b)
		}
	}
	if !exists {
		updated = append(updated, binding)
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err == nil {
		_ = os.WriteFile(branchBindingsPath(workDir), data, 0600)
	}
}

func removeBranchBinding(workDir string, templateID int, host string, slot int) {
	list := loadBranchBindings(workDir)
	var updated []branchBinding
	for _, b := range list {
		match := false
		if templateID > 0 && b.TemplateID == templateID {
			if host != "" && (b.Host == host || sanitizeTag(b.Host) == sanitizeTag(host)) {
				match = true
			}
			if slot > 0 && b.Slot == slot {
				match = true
			}
		} else if templateID <= 0 {
			if host != "" && (b.Host == host || sanitizeTag(b.Host) == sanitizeTag(host)) {
				match = true
			}
			if slot > 0 && b.Slot == slot {
				match = true
			}
		}
		if !match {
			updated = append(updated, b)
		}
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err == nil {
		_ = os.WriteFile(branchBindingsPath(workDir), data, 0600)
	}
}

func ensureSUIClientProtocolConfig(clientCfgObj map[string]map[string]any, tplType string, clientName, uuidStr, passStr, flowStr string) {
	if tplType == "" {
		tplType = "vless"
	}
	if clientCfgObj[tplType] == nil {
		clientCfgObj[tplType] = make(map[string]any)
	}
	targetCfg := clientCfgObj[tplType]
	switch tplType {
	case "vless":
		if _, ok := targetCfg["name"]; !ok {
			targetCfg["name"] = clientName
		}
		if _, ok := targetCfg["uuid"]; !ok {
			targetCfg["uuid"] = uuidStr
		}
		if flowStr != "" {
			targetCfg["flow"] = flowStr
		}
	case "vmess":
		if _, ok := targetCfg["name"]; !ok {
			targetCfg["name"] = clientName
		}
		if _, ok := targetCfg["uuid"]; !ok {
			targetCfg["uuid"] = uuidStr
		}
		if _, ok := targetCfg["alterId"]; !ok {
			targetCfg["alterId"] = 0
		}
	case "tuic":
		if _, ok := targetCfg["name"]; !ok {
			targetCfg["name"] = clientName
		}
		if _, ok := targetCfg["uuid"]; !ok {
			targetCfg["uuid"] = uuidStr
		}
		if _, ok := targetCfg["password"]; !ok {
			targetCfg["password"] = passStr
		}
	case "trojan", "hysteria2", "hy2":
		if _, ok := targetCfg["name"]; !ok {
			targetCfg["name"] = clientName
		}
		if _, ok := targetCfg["password"]; !ok {
			targetCfg["password"] = passStr
		}
	case "shadowsocks", "ss":
		if _, ok := targetCfg["name"]; !ok {
			targetCfg["name"] = clientName
		}
		if _, ok := targetCfg["password"]; !ok {
			targetCfg["password"] = passStr
		}
		if _, ok := targetCfg["method"]; !ok {
			targetCfg["method"] = "2022-blake3-aes-128-gcm"
		}
	case "socks", "socks5", "http", "mixed":
		if _, ok := targetCfg["username"]; !ok {
			targetCfg["username"] = clientName
		}
		if _, ok := targetCfg["password"]; !ok {
			targetCfg["password"] = passStr
		}
	}
	clientCfgObj[tplType] = targetCfg
}

// CloneToTunnels 采用单端口多用户 (Client) 架构：在同一个母入站下创建分流 Client，并配置 sing-box 用户路由
func (s *SUI) CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]int, error) {
	inboundsObj, err := s.callAPI(http.MethodGet, fmt.Sprintf("inbounds?id=%d", templateID), nil)
	if err != nil {
		return nil, err
	}
	var inboundsList []map[string]any
	var rawWrap struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(inboundsObj, &rawWrap); err == nil && len(rawWrap.Inbounds) > 0 {
		inboundsList = rawWrap.Inbounds
	} else {
		_ = json.Unmarshal(inboundsObj, &inboundsList)
	}
	if len(inboundsList) == 0 {
		return nil, fmt.Errorf("未找到模板入站 %d", templateID)
	}
	tpl := inboundsList[0]
	tplType, _ := tpl["type"].(string)
	tplType = strings.ToLower(strings.TrimSpace(tplType))
	if tplType == "" {
		tplType = "vless"
	}
	origTag, _ := tpl["tag"].(string)
	if origTag == "" {
		origTag = fmt.Sprintf("inbound-%d", templateID)
	}
	listenPort := 0
	if pVal, ok := tpl["listen_port"].(float64); ok {
		listenPort = int(pVal)
	}

	baseFlow := ""
	baseClientCfgJSON := s.sqliteQuery(fmt.Sprintf(
		"SELECT config FROM clients WHERE enable=1 AND %d IN (SELECT json_each.value FROM json_each(clients.inbounds)) LIMIT 1;",
		templateID,
	))
	if baseClientCfgJSON != "" {
		var baseCfg map[string]map[string]any
		if err := json.Unmarshal([]byte(baseClientCfgJSON), &baseCfg); err == nil {
			if vlessMap, ok := baseCfg["vless"]; ok {
				if f, ok := vlessMap["flow"].(string); ok {
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
		poolName := "家宽"
		slot := 0
		region := ""
		poolType := ""
		if targetTunnel != nil {
			cName = countryNameCN(targetTunnel.Node.CountryCode, targetTunnel.Node.Country)
			if targetTunnel.IPType == "datacenter" {
				poolName = "机房"
			}
			slot = targetTunnel.Slot
			region = targetTunnel.TargetRegion
			poolType = targetTunnel.TargetPoolType
		}

		// 持久化保存分流绑定意图，确保重启或节点轮换后自动恢复
		saveBranchBinding(s.workDir, branchBinding{
			TemplateID: templateID,
			Slot:       slot,
			Host:       host,
			Region:     region,
			PoolType:   poolType,
		})

		clientName := fmt.Sprintf("soutu%d%s", templateID, sanitizeTag(host))
		clientRemark := fmt.Sprintf("%s%s", cName, poolName)

		existing, existingOK, err := s.apiClientByName(clientName)
		if err != nil {
			return createdPorts, fmt.Errorf("查询分流客户端失败: %w", err)
		}
		clientID := 0
		var existingFull map[string]any
		if existingOK {
			if idVal, ok := existing["id"].(float64); ok {
				clientID = int(idVal)
			}
			full, ferr := s.apiClients(clientID)
			if ferr == nil && len(full) > 0 {
				existingFull = full[0]
			}
		}
		preserveCred := false
		newUUID := generateUUID()
		newPass := generateRandomToken(10)

		// 精准深拷贝原生主客户端模版的 config，仅替换实际存在的协议字段
		clientCfgObj := make(map[string]map[string]any)
		if existingFull != nil {
			if rawCfg, ok := existingFull["config"]; ok {
				rawBytes, _ := json.Marshal(rawCfg)
				_ = json.Unmarshal(rawBytes, &clientCfgObj)
				preserveCred = true
			}
		}
		tmplJSON, _ := s.sqliteJSONQuery(fmt.Sprintf("SELECT config FROM clients WHERE enable=1 AND %d IN (SELECT json_each.value FROM json_each(clients.inbounds)) LIMIT 1;", templateID))
		if string(tmplJSON) == "[]" || len(tmplJSON) == 0 {
			tmplJSON, _ = s.sqliteJSONQuery("SELECT config FROM clients WHERE enable=1 AND name NOT LIKE 'soutu%' AND name NOT LIKE 'sout-u-%' AND name NOT LIKE 'fanoutu%' AND name NOT LIKE 'fanout-u-%' LIMIT 1;")
		}
		var tmplClients []suiDBClient
		_ = json.Unmarshal(tmplJSON, &tmplClients)
		if len(tmplClients) > 0 && len(clientCfgObj) == 0 {
			clientCfgObj = parseSUIClientConfig(tmplClients[0].Config)
		}
		if len(clientCfgObj) == 0 {
			clientCfgObj = map[string]map[string]any{
				"vless": {
					"name": clientName,
					"uuid": newUUID,
					"flow": baseFlow,
				},
			}
		} else {
			// 仅遍历模板中真实存在的协议并替换凭据，绝不塞入未使用的冗余协议
			for proto, vals := range clientCfgObj {
				if vals == nil {
					vals = make(map[string]any)
				}
				if _, hasName := vals["name"]; hasName {
					vals["name"] = clientName
				}
				if _, hasUser := vals["username"]; hasUser {
					vals["username"] = clientName
				}
				if _, hasUUID := vals["uuid"]; hasUUID {
					if !preserveCred {
						vals["uuid"] = newUUID
					}
				}
				if _, hasPass := vals["password"]; hasPass {
					if !preserveCred {
						vals["password"] = newPass
					}
				}
				if _, hasAuth := vals["auth_str"]; hasAuth {
					if !preserveCred {
						vals["auth_str"] = newPass
					}
				}
				if proto == "vless" {
					vals["flow"] = baseFlow
				}
				clientCfgObj[proto] = vals
			}
		}

		ensureSUIClientProtocolConfig(clientCfgObj, tplType, clientName, newUUID, newPass, baseFlow)

		// 通过 s-ui 自身 API 保存/更新分流客户端，由 s-ui 自动生成订阅链接（含 ed/fp 等参数）
		act := "new"
		clientPayload := map[string]any{
			"id":         0,
			"enable":     true,
			"name":       clientName,
			"remark":     clientRemark,
			"config":     clientCfgObj,
			"inbounds":   []int{templateID},
			"links":      []any{},
			"volume":     0,
			"expiry":     0,
			"down":       0,
			"up":         0,
			"desc":       "",
			"group":      "",
			"delayStart": false,
			"autoReset":  false,
			"resetDays":  0,
			"nextReset":  0,
			"totalUp":    0,
			"totalDown":  0,
			"createdAt":  0,
			"onlineAt":   0,
		}
		if existingOK {
			clientPayload["id"] = clientID
			act = "edit"
		}
		if err := s.apiSaveClient(act, clientPayload); err != nil {
			return createdPorts, fmt.Errorf("保存分流客户端 (%s) 失败: %w", clientName, err)
		}

		if err := s.BindUserRoute(clientName, host, tunnels); err != nil {
			return createdPorts, fmt.Errorf("绑定分流路由 (%s) 失败: %w", clientName, err)
		}

		createdPorts = append(createdPorts, listenPort)
	}

	s.restartSingBox()
	s.syncSUIDatabaseLinks(hostPublicIP())
	return createdPorts, nil
}

// syncSUIDatabaseLinks 通过 s-ui API 重新保存分流客户端，让 s-ui 自己生成权威订阅链接
func (s *SUI) syncSUIDatabaseLinks(publicHost string) {
	// s-ui 自身会在 /apiv2/save object=clients 时自动生成 clients.links（含 ed/fp 等参数）。
	// 这里只对旧的/直连 SQLite 插入过的分流客户端做一次 API 重保存迁移，让 s-ui 重新生成权威链接。
	allClients, err := s.apiClients(0)
	if err != nil {
		return
	}
	for _, raw := range allClients {
		name, _ := raw["name"].(string)
		if !isSplitUser(name) {
			continue
		}
		idVal, _ := raw["id"].(float64)
		id := int(idVal)
		if id <= 0 {
			continue
		}
		full, err := s.apiClients(id)
		if err != nil || len(full) == 0 {
			continue
		}
		client := full[0]
		// 保留非 local 链接（外部/订阅链接），local 部分交给 s-ui 重新生成
		if links, ok := client["links"].([]any); ok {
			var preserved []any
			for _, item := range links {
				if m, ok := item.(map[string]any); ok {
					if typ, _ := m["type"].(string); typ != "local" {
						preserved = append(preserved, item)
					}
				}
			}
			client["links"] = preserved
		} else {
			client["links"] = []any{}
		}
		if err := s.apiSaveClient("edit", client); err != nil {
			log.Printf("重新生成分流客户端 %s 的 s-ui 链接失败: %v", name, err)
		}
	}
}

func (s *SUI) Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error {
	boundMap, err := s.getBoundUserRoutes()
	if err != nil {
		return err
	}
	oldHostTag := sanitizeTag(oldHost)

	cName := countryNameCN(target.Node.CountryCode, target.Node.Country)
	poolName := "家宽"
	if target.IPType == "datacenter" {
		poolName = "机房"
	}
	newRemark := fmt.Sprintf("%s%s", cName, poolName)

	for userName, host := range boundMap {
		if host == oldHost || host == oldHostTag {
			if err := s.BindUserRoute(userName, target.Node.HostName, tunnels); err != nil {
				return err
			}
			// 同步更新 SQLite 中 client 的 remark，使 tag 变成新的「国家+机房/家宽」
			_ = s.sqliteQuery(fmt.Sprintf("UPDATE clients SET remark = '%s' WHERE name = '%s';", newRemark, userName))
		}
	}
	s.syncSUIDatabaseLinks(hostPublicIP())
	invalidateInbounds()
	return nil
}

// DeleteBranchesByHost 当出口隧道被删除/停止时，级联删除绑定到该出口上的所有分流分支客户端及路由规则
func (s *SUI) DeleteBranchesByHost(host string, tunnels []*Tunnel) error {
	if host == "" {
		return nil
	}
	boundMap, err := s.getBoundUserRoutes()
	if err != nil {
		return err
	}
	oldHostTag := sanitizeTag(host)

	deleteByName := func(userName string) {
		if client, ok, err := s.apiClientByName(userName); err != nil {
			log.Printf("查询待删除分流客户端 %s 失败: %v", userName, err)
			return
		} else if ok {
			if idVal, ok := client["id"].(float64); ok {
				_ = s.apiDeleteClient(int(idVal))
			}
		}
		_ = s.BindUserRoute(userName, "", tunnels)
	}

	removeBranchBinding(s.workDir, 0, host, 0)
	for userName, boundHost := range boundMap {
		if boundHost == host || boundHost == oldHostTag {
			deleteByName(userName)
		}
	}

	// 双重保障：通过 s-ui API 清理以该 hostTag 结尾的所有 sout/fanout client
	clients, err := s.apiClients(0)
	if err == nil {
		for _, c := range clients {
			name, _ := c["name"].(string)
			if isSplitUser(name) && strings.HasSuffix(name, oldHostTag) {
				if idVal, ok := c["id"].(float64); ok {
					_ = s.apiDeleteClient(int(idVal))
				}
			}
		}
	}
	invalidateInbounds()
	return nil
}

func (s *SUI) ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error {
	return s.syncOutbounds(tunnels)
}

// DeleteInbounds 删除分流分支。若传入 Client ID，删除该 Client 并移除路由规则；若删光分流，恢复纯净直连。
func (s *SUI) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	for _, id := range ids {
		clientName := s.sqliteQuery(fmt.Sprintf("SELECT name FROM clients WHERE id = %d AND (name LIKE 'soutu%%' OR name LIKE 'sout-u-%%' OR name LIKE 'fanoutu%%' OR name LIKE 'fanout-u-%%');", id))
		if clientName != "" {
			_ = s.apiDeleteClient(id)
			_ = s.BindUserRoute(clientName, "", tunnels)
			removeBranchBinding(s.workDir, 0, clientName, 0)
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
			_ = s.BindUserRoute(inbTag, "", tunnels)
			removeBranchBinding(s.workDir, 0, inbTag, 0)
		}
	}

	s.syncSUIDatabaseLinks(hostPublicIP())
	invalidateInbounds()
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
			MaxEarlyData        int               `json:"max_early_data"`
		} `json:"transport"`
		Users []struct {
			Name string `json:"name"`
			UUID string `json:"uuid"`
			Flow string `json:"flow"`
		} `json:"users"`
		CongestionControl string `json:"congestion_control"`
	}
	if err := json.Unmarshal(outJsonBytes, &out); err != nil {
		return nil
	}

	if out.Type == "" {
		out.Type = "vless"
	}
	if !out.TLS.Enabled && (out.ServerPort == 443 || strings.Contains(tag, "tls") || strings.Contains(tag, "cdn")) {
		out.TLS.Enabled = true
		if out.TLS.ServerName == "" {
			out.TLS.ServerName = publicHost
		}
	}

	type AddrItem struct {
		Server     string `json:"server"`
		ServerPort int    `json:"server_port"`
		Remark     string `json:"remark"`
		TLS        struct {
			Enabled    bool   `json:"enabled"`
			ServerName string `json:"server_name"`
			Insecure   bool   `json:"insecure"`
			DisableSNI bool   `json:"disable_sni"`
			UTLS       struct {
				Enabled     bool   `json:"enabled"`
				Fingerprint string `json:"fingerprint"`
			} `json:"utls"`
		} `json:"tls"`
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
			if uuidStr == "" && len(out.Users) > 0 && out.Users[0].UUID != "" {
				uuidStr = out.Users[0].UUID
			}
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
				path := out.Transport.Path
				if out.Transport.MaxEarlyData > 0 && out.Transport.EarlyDataHeaderName == "Sec-WebSocket-Protocol" {
					sep := "?"
					if strings.Contains(path, "?") {
						sep = "&"
					}
					path = fmt.Sprintf("%s%sed=%d", path, sep, out.Transport.MaxEarlyData)
				}
				v.Set("path", path)
			}
			if out.Transport.Headers != nil && out.Transport.Headers["Host"] != "" {
				v.Set("host", out.Transport.Headers["Host"])
			}
			if out.Transport.ServiceName != "" {
				v.Set("serviceName", out.Transport.ServiceName)
			}
			tlsEnabled := out.TLS.Enabled || addr.TLS.Enabled || port == 443
			if tlsEnabled {
				sniToUse := addr.TLS.ServerName
				if sniToUse == "" {
					sniToUse = out.TLS.ServerName
				}
				if sniToUse == "" {
					sniToUse = publicHost
				}

				if out.TLS.Reality.Enabled {
					v.Set("security", "reality")
					v.Set("pbk", out.TLS.Reality.PublicKey)
					v.Set("sid", out.TLS.Reality.ShortID)
				} else {
					v.Set("security", "tls")
					if out.TLS.Insecure || addr.TLS.Insecure {
						v.Set("allowInsecure", "1")
					}
				}
				if sniToUse != "" && !addr.TLS.DisableSNI {
					v.Set("sni", sniToUse)
				}
				fp := out.TLS.UTLS.Fingerprint
				if fp == "" {
					fp = addr.TLS.UTLS.Fingerprint
				}
				if fp != "" {
					v.Set("fp", fp)
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

		case "socks", "socks5":
			user, _ := clientCfg["socks"]["username"].(string)
			pass, _ := clientCfg["socks"]["password"].(string)
			if user != "" || pass != "" {
				links = append(links, fmt.Sprintf("socks5://%s:%s@%s:%d#%s", url.QueryEscape(user), url.QueryEscape(pass), host, port, url.PathEscape(remark)))
			} else {
				links = append(links, fmt.Sprintf("socks5://%s:%d#%s", host, port, url.PathEscape(remark)))
			}

		case "http", "mixed":
			user, _ := clientCfg["http"]["username"].(string)
			pass, _ := clientCfg["http"]["password"].(string)
			if user == "" && pass == "" {
				user, _ = clientCfg["mixed"]["username"].(string)
				pass, _ = clientCfg["mixed"]["password"].(string)
			}
			if user != "" || pass != "" {
				links = append(links, fmt.Sprintf("http://%s:%s@%s:%d#%s", url.QueryEscape(user), url.QueryEscape(pass), host, port, url.PathEscape(remark)))
			} else {
				links = append(links, fmt.Sprintf("http://%s:%d#%s", host, port, url.PathEscape(remark)))
			}

		case "shadowsocks", "ss":
			passStr, _ := clientCfg["shadowsocks"]["password"].(string)
			methodStr, _ := clientCfg["shadowsocks"]["method"].(string)
			if methodStr == "" {
				methodStr = "2022-blake3-aes-128-gcm"
			}
			auth := base64.URLEncoding.EncodeToString([]byte(methodStr + ":" + passStr))
			links = append(links, fmt.Sprintf("ss://%s@%s:%d#%s", auth, host, port, url.PathEscape(remark)))

		case "hysteria2", "hy2":
			passStr, _ := clientCfg["hysteria2"]["password"].(string)
			v := url.Values{}
			if out.TLS.ServerName != "" {
				v.Set("sni", out.TLS.ServerName)
			}
			if out.TLS.Insecure {
				v.Set("insecure", "1")
			}
			links = append(links, fmt.Sprintf("hysteria2://%s@%s:%d?%s#%s", passStr, host, port, v.Encode(), url.PathEscape(remark)))

		case "vmess":
			uuidStr, _ := clientCfg["vmess"]["uuid"].(string)
			alterId, _ := clientCfg["vmess"]["alterId"].(float64)
			tlsStr := ""
			if out.TLS.Enabled {
				tlsStr = "tls"
			}
			tp := out.Transport.Type
			if tp == "" {
				tp = "tcp"
			}
			vmessObj := map[string]any{
				"v":    "2",
				"ps":   remark,
				"add":  host,
				"port": port,
				"id":   uuidStr,
				"aid":  int(alterId),
				"net":  tp,
				"type": "none",
				"host": out.Transport.Host,
				"path": out.Transport.Path,
				"tls":  tlsStr,
				"sni":  out.TLS.ServerName,
			}
			b, _ := json.Marshal(vmessObj)
			links = append(links, "vmess://"+base64.StdEncoding.EncodeToString(b))
		}
	}

	return links
}

func getCfgVal(cfg map[string]map[string]any, section, key string) string {
	if cfg == nil {
		return ""
	}
	if sec, ok := cfg[section]; ok {
		if v, ok := sec[key].(string); ok {
			return v
		}
	}
	for _, sec := range cfg {
		if v, ok := sec[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func replaceLinkCredential(uri string, proto string, oldClientCfg, newClientCfg map[string]map[string]any) string {
	proto = strings.ToLower(proto)
	if strings.HasPrefix(uri, "vmess://") || proto == "vmess" {
		newUUID := getCfgVal(newClientCfg, "vmess", "uuid")
		if newUUID == "" {
			newUUID = getCfgVal(newClientCfg, "vless", "uuid")
		}
		if newUUID != "" {
			b64Part := strings.TrimPrefix(uri, "vmess://")
			if idx := strings.Index(b64Part, "#"); idx != -1 {
				b64Part = b64Part[:idx]
			}
			var jsonBytes []byte
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
				b, err := enc.DecodeString(b64Part)
				if err == nil && len(b) > 0 {
					jsonBytes = b
					break
				}
			}
			if len(jsonBytes) > 0 {
				var vmessMap map[string]any
				if err := json.Unmarshal(jsonBytes, &vmessMap); err == nil {
					vmessMap["id"] = newUUID
					if alterId, ok := newClientCfg["vmess"]["alterId"].(float64); ok {
						vmessMap["aid"] = int(alterId)
					}
					newJSON, _ := json.Marshal(vmessMap)
					return "vmess://" + base64.StdEncoding.EncodeToString(newJSON)
				}
			}
		}
		return uri
	}

	switch proto {
	case "vless":
		oldUUID := getCfgVal(oldClientCfg, proto, "uuid")
		newUUID := getCfgVal(newClientCfg, proto, "uuid")
		if oldUUID != "" && newUUID != "" && strings.Contains(uri, oldUUID) {
			return strings.ReplaceAll(uri, oldUUID, newUUID)
		}
	case "tuic":
		oldUUID := getCfgVal(oldClientCfg, "tuic", "uuid")
		newUUID := getCfgVal(newClientCfg, "tuic", "uuid")
		oldPass := getCfgVal(oldClientCfg, "tuic", "password")
		newPass := getCfgVal(newClientCfg, "tuic", "password")
		res := uri
		if oldUUID != "" && newUUID != "" && strings.Contains(res, oldUUID) {
			res = strings.ReplaceAll(res, oldUUID, newUUID)
		}
		if oldPass != "" && newPass != "" && strings.Contains(res, oldPass) {
			res = strings.ReplaceAll(res, oldPass, newPass)
		}
		return res
	case "trojan", "hysteria2", "hy2":
		oldPass := getCfgVal(oldClientCfg, proto, "password")
		newPass := getCfgVal(newClientCfg, proto, "password")
		if oldPass != "" && newPass != "" && strings.Contains(uri, oldPass) {
			return strings.ReplaceAll(uri, oldPass, newPass)
		}
	case "hysteria", "hy":
		oldAuth := getCfgVal(oldClientCfg, "hysteria", "auth_str")
		newAuth := getCfgVal(newClientCfg, "hysteria", "auth_str")
		if oldAuth != "" && newAuth != "" && strings.Contains(uri, oldAuth) {
			return strings.ReplaceAll(uri, oldAuth, newAuth)
		}
	case "socks", "socks5", "http", "mixed":
		oldUser := getCfgVal(oldClientCfg, proto, "username")
		oldPass := getCfgVal(oldClientCfg, proto, "password")
		newUser := getCfgVal(newClientCfg, proto, "username")
		newPass := getCfgVal(newClientCfg, proto, "password")
		if oldUser != "" && newUser != "" {
			oldAuth := fmt.Sprintf("%s:%s@", oldUser, oldPass)
			newAuth := fmt.Sprintf("%s:%s@", newUser, newPass)
			if strings.Contains(uri, oldAuth) {
				return strings.ReplaceAll(uri, oldAuth, newAuth)
			}
		}
	case "shadowsocks", "ss":
		oldPass := getCfgVal(oldClientCfg, "shadowsocks", "password")
		oldMethod := getCfgVal(oldClientCfg, "shadowsocks", "method")
		newPass := getCfgVal(newClientCfg, "shadowsocks", "password")
		newMethod := getCfgVal(newClientCfg, "shadowsocks", "method")
		if newMethod == "" {
			newMethod = oldMethod
		}
		if newMethod == "" {
			newMethod = "2022-blake3-aes-128-gcm"
		}
		if oldPass != "" && newPass != "" {
			oldAuth := base64.URLEncoding.EncodeToString([]byte(oldMethod + ":" + oldPass))
			newAuth := base64.URLEncoding.EncodeToString([]byte(newMethod + ":" + newPass))
			if strings.Contains(uri, oldAuth) {
				return strings.ReplaceAll(uri, oldAuth, newAuth)
			}
		}
	}
	return uri
}

func formatNodeURI(uri string, tagToUse string) string {
	if strings.HasPrefix(uri, "vmess://") {
		b64Part := strings.TrimPrefix(uri, "vmess://")
		if idx := strings.Index(b64Part, "#"); idx != -1 {
			b64Part = b64Part[:idx]
		}
		var jsonBytes []byte
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
			b, err := enc.DecodeString(b64Part)
			if err == nil && len(b) > 0 {
				jsonBytes = b
				break
			}
		}
		if len(jsonBytes) > 0 {
			var vmessMap map[string]any
			if err := json.Unmarshal(jsonBytes, &vmessMap); err == nil {
				if tagToUse != "" {
					vmessMap["ps"] = tagToUse
				}
				newJSON, _ := json.Marshal(vmessMap)
				return "vmess://" + base64.StdEncoding.EncodeToString(newJSON)
			}
		}
		return "vmess://" + b64Part
	}

	basePart := uri
	if idx := strings.Index(uri, "#"); idx != -1 {
		basePart = uri[:idx]
	}
	if tagToUse == "" {
		return basePart
	}
	return fmt.Sprintf("%s#%s", basePart, url.PathEscape(tagToUse))
}

type SUIClientLink struct {
	Remark string `json:"remark"`
	Type   string `json:"type"`
	URI    string `json:"uri"`
}

func parseSUIClientLinks(linksRaw json.RawMessage) []SUIClientLink {
	var links []SUIClientLink
	if len(linksRaw) == 0 {
		return links
	}
	var str string
	if err := json.Unmarshal(linksRaw, &str); err == nil {
		_ = json.Unmarshal([]byte(str), &links)
		return links
	}
	_ = json.Unmarshal(linksRaw, &links)
	return links
}

func parseSUIClientConfig(configRaw json.RawMessage) map[string]map[string]any {
	var cfg map[string]map[string]any
	if len(configRaw) == 0 {
		return cfg
	}
	var str string
	if err := json.Unmarshal(configRaw, &str); err == nil {
		_ = json.Unmarshal([]byte(str), &cfg)
		return cfg
	}
	_ = json.Unmarshal(configRaw, &cfg)
	return cfg
}

func (s *SUI) InboundBranchLinks(inboundID int, clientID int, branchTag string, publicHost string) []string {
	inbTag := s.sqliteQuery(fmt.Sprintf("SELECT tag FROM inbounds WHERE id = %d;", inboundID))
	inbType := strings.ToLower(s.sqliteQuery(fmt.Sprintf("SELECT type FROM inbounds WHERE id = %d;", inboundID)))
	baseInbTag := getBaseTag(inbTag)

	var matchedURIs []string

	// 1. 如果指定了 clientID，优先查询当前 client
	var client suiDBClient
	if clientID > 0 {
		clientJSON, _ := s.sqliteJSONQuery(fmt.Sprintf("SELECT id, name, remark, enable, inbounds, links, config FROM clients WHERE id = %d LIMIT 1;", clientID))
		var arr []suiDBClient
		_ = json.Unmarshal(clientJSON, &arr)
		if len(arr) > 0 {
			client = arr[0]
		}
	} else {
		clientJSON, _ := s.sqliteJSONQuery(fmt.Sprintf("SELECT id, name, remark, enable, inbounds, links, config FROM clients WHERE enable=1 AND %d IN (SELECT json_each.value FROM json_each(clients.inbounds)) LIMIT 1;", inboundID))
		var arr []suiDBClient
		_ = json.Unmarshal(clientJSON, &arr)
		if len(arr) > 0 {
			client = arr[0]
		}
	}


	// 2. 如果客户端自身已由 s-ui 生成有效 links（分流客户端现在也由 s-ui 生成），直接取用
	if len(client.Links) > 0 {
		linksArr := parseSUIClientLinks(client.Links)
		for _, item := range linksArr {
			if item.Remark == inbTag || item.Remark == baseInbTag || getBaseTag(item.Remark) == baseInbTag {
				if item.URI != "" {
					matchedURIs = append(matchedURIs, item.URI)
				}
			}
		}
	}

	// 3. 如果是 split client 或者当前 client 尚未生成 links，寻找该入站的主原生 client 作为模版派生
	if len(matchedURIs) == 0 {
		query := fmt.Sprintf("SELECT id, name, remark, enable, inbounds, links, config FROM clients WHERE id != %d AND (name NOT LIKE 'soutu%%' AND name NOT LIKE 'sout-u-%%' AND name NOT LIKE 'fanoutu%%' AND name NOT LIKE 'fanout-u-%%') AND links IS NOT NULL AND links != '' LIMIT 1;", client.ID)
		if client.ID == 0 {
			query = "SELECT id, name, remark, enable, inbounds, links, config FROM clients WHERE (name NOT LIKE 'soutu%%' AND name NOT LIKE 'sout-u-%%' AND name NOT LIKE 'fanoutu%%' AND name NOT LIKE 'fanout-u-%%') AND links IS NOT NULL AND links != '' LIMIT 1;"
		}
		templateJSON, _ := s.sqliteJSONQuery(query)
		var templates []suiDBClient
		_ = json.Unmarshal(templateJSON, &templates)
		if len(templates) > 0 {
			tmpl := templates[0]
			tmplLinks := parseSUIClientLinks(tmpl.Links)
			tmplCfg := parseSUIClientConfig(tmpl.Config)
			clientCfg := parseSUIClientConfig(client.Config)

			for _, item := range tmplLinks {
				if item.Remark == inbTag || item.Remark == baseInbTag || getBaseTag(item.Remark) == baseInbTag {
					if item.URI != "" {
						derivedURI := replaceLinkCredential(item.URI, inbType, tmplCfg, clientCfg)
						matchedURIs = append(matchedURIs, derivedURI)
					}
				}
			}
		}
	}

	// 4. 格式化链接与备注（vmess 使用 ps 字段且无 #hash，其他协议使用 #remark 片段）
	if len(matchedURIs) > 0 {
		var finalLinks []string
		tagToUse := branchTag
		if tagToUse == "" {
			tagToUse = inbTag
		}
		for _, uri := range matchedURIs {
			finalLinks = append(finalLinks, formatNodeURI(uri, tagToUse))
		}
		return finalLinks
	}

	// 5. 兜底方案：从 inbounds 表构建
	outJSON := s.sqliteQuery(fmt.Sprintf("SELECT out_json FROM inbounds WHERE id = %d;", inboundID))
	if outJSON == "" {
		outJSON = s.sqliteQuery(fmt.Sprintf("SELECT options FROM inbounds WHERE id = %d;", inboundID))
	}
	addrsJSON := s.sqliteQuery(fmt.Sprintf("SELECT addrs FROM inbounds WHERE id = %d;", inboundID))
	if outJSON == "" {
		return nil
	}
	return s.buildLinksFromInbound([]byte(outJSON), []byte(addrsJSON), client.Config, publicHost, branchTag)
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

func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (s *SUI) NodeDetail(id int) (*NodeDetailInfo, error) {
	inboundsObj, err := s.callAPI(http.MethodGet, fmt.Sprintf("inbounds?id=%d", id), nil)
	var rawWrap struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err == nil {
		_ = json.Unmarshal(inboundsObj, &rawWrap)
	}

	if len(rawWrap.Inbounds) == 0 {
		// 从 sqlite 兜底
		rowJSON, qErr := s.sqliteJSONQuery(fmt.Sprintf("SELECT id, type, tag, options, addrs FROM inbounds WHERE id=%d LIMIT 1;", id))
		if qErr != nil || len(rowJSON) == 0 {
			return nil, fmt.Errorf("未找到节点 %d", id)
		}
		var rows []struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Tag     string          `json:"tag"`
			Options json.RawMessage `json:"options"`
			Addrs   json.RawMessage `json:"addrs"`
		}
		if err := json.Unmarshal(rowJSON, &rows); err != nil || len(rows) == 0 {
			return nil, fmt.Errorf("未找到节点 %d", id)
		}
		row := rows[0]
		listen := "::"
		listenPort := 0
		if len(row.Options) > 0 {
			var opt map[string]any
			if json.Unmarshal(row.Options, &opt) == nil {
				if l, ok := opt["listen"].(string); ok && l != "" {
					listen = l
				}
				if p, ok := opt["listen_port"].(float64); ok {
					listenPort = int(p)
				}
			}
		}
		var addrs []NodeAddrItem
		if len(row.Addrs) > 0 {
			_ = json.Unmarshal(row.Addrs, &addrs)
		}
		return &NodeDetailInfo{
			ID:         row.ID,
			Name:       row.Tag,
			Protocol:   strings.ToUpper(row.Type),
			Listen:     listen,
			ListenPort: listenPort,
			Addrs:      addrs,
		}, nil
	}

	inb := rawWrap.Inbounds[0]
	tag, _ := inb["tag"].(string)
	typ, _ := inb["type"].(string)
	listen := "::"
	listenPort := 0
	if l, ok := inb["listen"].(string); ok && l != "" {
		listen = l
	}
	if p, ok := inb["listen_port"].(float64); ok {
		listenPort = int(p)
	}

	// 检查 options 里的 listen / listen_port
	if optMap, ok := inb["options"].(map[string]any); ok {
		if l, ok := optMap["listen"].(string); ok && l != "" {
			listen = l
		}
		if p, ok := optMap["listen_port"].(float64); ok {
			listenPort = int(p)
		}
	}

	var addrs []NodeAddrItem
	var rawAddrsList []map[string]any
	rawAddrsHex := s.sqliteQuery(fmt.Sprintf("SELECT hex(addrs) FROM inbounds WHERE id = %d;", id))
	if rawAddrsHex != "" {
		if b, err := hex.DecodeString(rawAddrsHex); err == nil {
			_ = json.Unmarshal(b, &rawAddrsList)
			_ = json.Unmarshal(b, &addrs)
		}
	}
	if len(addrs) == 0 {
		if addrsRaw, ok := inb["addrs"]; ok {
			b, _ := json.Marshal(addrsRaw)
			_ = json.Unmarshal(b, &rawAddrsList)
			_ = json.Unmarshal(b, &addrs)
		}
	}

	var tlsEnabled bool
	var sni string

	var outJSON map[string]any
	rawOutHex := s.sqliteQuery(fmt.Sprintf("SELECT hex(out_json) FROM inbounds WHERE id = %d;", id))
	if rawOutHex != "" {
		if b, err := hex.DecodeString(rawOutHex); err == nil {
			_ = json.Unmarshal(b, &outJSON)
		}
	}
	if outJSON != nil {
		if tlsMap, ok := outJSON["tls"].(map[string]any); ok {
			if en, ok := tlsMap["enabled"].(bool); ok {
				tlsEnabled = en
			}
			if s, ok := tlsMap["server_name"].(string); ok {
				sni = s
			}
		}
		if sni == "" {
			if tr, ok := outJSON["transport"].(map[string]any); ok {
				if hdrs, ok := tr["headers"].(map[string]any); ok {
					if h, ok := hdrs["Host"].(string); ok {
						sni = h
					}
				}
			}
		}
	}

	// 若 out_json 未开启，但 addrs 中某项开启了 TLS，亦识别为开启
	if !tlsEnabled && len(rawAddrsList) > 0 {
		for _, rawItem := range rawAddrsList {
			if tlsMap, ok := rawItem["tls"].(map[string]any); ok {
				if en, ok := tlsMap["enabled"].(bool); ok && en {
					tlsEnabled = true
					if sni == "" {
						if s, ok := tlsMap["server_name"].(string); ok {
							sni = s
						}
					}
					break
				}
			}
		}
	}

	var allAddrs []NodeAddrItem
	if len(addrs) > 0 {
		allAddrs = addrs
	} else if outJSON != nil {
		if srv, ok := outJSON["server"].(string); ok && srv != "" {
			p := 0
			if pFloat, ok := outJSON["server_port"].(float64); ok {
				p = int(pFloat)
			}
			rem := ""
			if r, ok := outJSON["remark"].(string); ok {
				rem = r
			}
			allAddrs = append(allAddrs, NodeAddrItem{
				Server:     srv,
				ServerPort: p,
				Remark:     rem,
			})
		}
	}

	return &NodeDetailInfo{
		ID:         id,
		Name:       tag,
		Protocol:   strings.ToUpper(typ),
		Listen:     listen,
		ListenPort: listenPort,
		TLSEnabled: tlsEnabled,
		SNI:        sni,
		Addrs:      allAddrs,
	}, nil
}

func (s *SUI) UpdateNodeConfig(id int, listen string, listenPort int, addrs []NodeAddrItem, tlsEnabled bool, sni string, tunnels []*Tunnel) error {
	// 1. 读取原有的 out_json
	var origOutJSON map[string]any
	rawOutJSONHex := s.sqliteQuery(fmt.Sprintf("SELECT hex(out_json) FROM inbounds WHERE id = %d;", id))
	if rawOutJSONHex != "" {
		if b, err := hex.DecodeString(rawOutJSONHex); err == nil {
			_ = json.Unmarshal(b, &origOutJSON)
		}
	}
	if origOutJSON == nil {
		origOutJSON = make(map[string]any)
	}

	serverName := strings.TrimSpace(sni)
	var tlsMap map[string]any
	if tlsEnabled {
		tlsMap = map[string]any{
			"enabled":     true,
			"server_name": serverName,
			"utls": map[string]any{
				"enabled":     true,
				"fingerprint": "chrome",
			},
		}
		origOutJSON["tls"] = tlsMap
		if serverName != "" {
			if tr, ok := origOutJSON["transport"].(map[string]any); ok {
				if hdrs, ok := tr["headers"].(map[string]any); ok {
					hdrs["Host"] = serverName
				}
			}
		}
	} else {
		delete(origOutJSON, "tls")
	}

	// 2. 组装 addrs 列表（包含所有多域名条目，并附加 tls 配置）
	addrsList := make([]map[string]any, 0, len(addrs))
	for i, a := range addrs {
		srv := strings.TrimSpace(a.Server)
		if srv == "" {
			continue
		}
		item := map[string]any{
			"server":      srv,
			"server_port": a.ServerPort,
		}
		if a.Remark != "" {
			item["remark"] = a.Remark
		}
		if tlsEnabled && tlsMap != nil {
			item["tls"] = tlsMap
		}
		if i == 0 {
			origOutJSON["server"] = srv
			origOutJSON["server_port"] = a.ServerPort
			if a.Remark != "" {
				origOutJSON["remark"] = a.Remark
			} else {
				delete(origOutJSON, "remark")
			}
		}
		addrsList = append(addrsList, item)
	}

	// 3. 更新 options
	optMap := make(map[string]any)
	rawOptHex := s.sqliteQuery(fmt.Sprintf("SELECT hex(options) FROM inbounds WHERE id = %d;", id))
	if rawOptHex != "" {
		if b, err := hex.DecodeString(rawOptHex); err == nil {
			_ = json.Unmarshal(b, &optMap)
		}
	}
	if listen != "" {
		optMap["listen"] = listen
	}
	if listenPort > 0 {
		optMap["listen_port"] = listenPort
	}

	newOptJSON, _ := json.Marshal(optMap)
	addrsJSON, _ := json.Marshal(addrsList)
	newOutJSON, _ := json.Marshal(origOutJSON)

	// 4. 持久化到 SQLite 并重启 sing-box
	_ = s.sqliteQuery(fmt.Sprintf(
		"UPDATE inbounds SET options=X'%s', addrs=X'%s', out_json=X'%s', listen_port=%d WHERE id=%d;",
		hex.EncodeToString(newOptJSON),
		hex.EncodeToString(addrsJSON),
		hex.EncodeToString(newOutJSON),
		listenPort,
		id,
	))

	s.restartSingBox()
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

// reconcileBranchBindings 自动将持久化保存的分流绑定恢复到活跃隧道
func (s *SUI) reconcileBranchBindings(tunnels []*Tunnel) {
	bindings := loadBranchBindings(s.workDir)
	if len(bindings) == 0 {
		return
	}
	for _, b := range bindings {
		if b.TemplateID <= 0 {
			continue
		}
		var targetTunnel *Tunnel
		for _, t := range tunnels {
			if t.Status == "up" {
				if b.Host != "" && (t.Node.HostName == b.Host || sanitizeTag(t.Node.HostName) == sanitizeTag(b.Host)) {
					targetTunnel = t
					break
				}
				if b.Slot > 0 && t.Slot == b.Slot {
					targetTunnel = t
					break
				}
			}
		}
		if targetTunnel == nil {
			continue
		}
		clientName := fmt.Sprintf("soutu%d%s", b.TemplateID, sanitizeTag(targetTunnel.Node.HostName))
		if _, ok, err := s.apiClientByName(clientName); err == nil && !ok {
			log.Printf("自动自愈并恢复家宽分流绑定: 模板入站 %d -> 节点 %s (%s)", b.TemplateID, targetTunnel.Node.HostName, targetTunnel.Node.Country)
			_, _ = s.CloneToTunnels(b.TemplateID, []string{targetTunnel.Node.HostName}, tunnels)
		}
	}
}

func isSplitUser(u string) bool {
	return strings.HasPrefix(u, "soutu") || strings.HasPrefix(u, "sout-u-") || strings.HasPrefix(u, "fanoutu") || strings.HasPrefix(u, "fanout-u-")
}

// cleanStaleRoutesAndClients 自动清理不存在于活跃隧道及持久化配置中的残余路由规则与分流用户
func (s *SUI) cleanStaleRoutesAndClients(tunnels []*Tunnel) error {
	activeTags := make(map[string]bool)
	configuredHosts := make(map[string]bool)
	for _, t := range tunnels {
		if t.Node.HostName != "" {
			configuredHosts[sanitizeTag(t.Node.HostName)] = true
			if t.Status == "up" {
				activeTags[suiTagPrefix+sanitizeTag(t.Node.HostName)] = true
			}
		}
	}

	// 保护持久化分流配置中的 Host
	bindings := loadBranchBindings(s.workDir)
	for _, b := range bindings {
		if b.Host != "" {
			configuredHosts[sanitizeTag(b.Host)] = true
		}
	}

	// 1. 通过 s-ui API 清理失效分流客户端（仅清理已彻底被删除的无主客户端）
	clients, err := s.apiClients(0)
	if err == nil {
		for _, c := range clients {
			name, _ := c["name"].(string)
			if !isSplitUser(name) {
				continue
			}
			matched := false
			for hostTag := range configuredHosts {
				if strings.HasSuffix(name, hostTag) {
					matched = true
					break
				}
			}
			if !matched {
				if idVal, ok := c["id"].(float64); ok {
					_ = s.apiDeleteClient(int(idVal))
				}
			}
		}
	}

	// 2. 清理 sing-box 路由规则中失效的 sout/fanout 分流项
	configObj, err := s.callAPI(http.MethodGet, "config", nil)
	if err == nil {
		var cfg map[string]any
		if err := json.Unmarshal(configObj, &cfg); err == nil {
			rawConfig, _ := cfg["config"].(map[string]any)
			if rawConfig == nil {
				rawConfig = cfg
			}
			route, _ := rawConfig["route"].(map[string]any)
			if route != nil {
				rules, _ := route["rules"].([]any)
				var cleanRules []any
				changed := false
				for _, r := range rules {
					ruleMap, ok := r.(map[string]any)
					if !ok {
						cleanRules = append(cleanRules, r)
						continue
					}
					outbound, _ := ruleMap["outbound"].(string)
					// 如果该规则指向 sout 出站但当前已无该配置的隧道，清理
					if isSUIOutboundTag(outbound) && !configuredHosts[strings.TrimPrefix(outbound, suiTagPrefix)] {
						changed = true
						continue
					}
					// 检查 auth_user 中是否包含失效的分流用户
					users := toSUITagSlice(ruleMap["auth_user"])
					if len(users) > 0 {
						var validUsers []string
						for _, u := range users {
							if isSplitUser(u) {
								userMatched := false
								for hostTag := range configuredHosts {
									if strings.HasSuffix(u, hostTag) {
										userMatched = true
										break
									}
								}
								if userMatched {
									validUsers = append(validUsers, u)
								}
							} else {
								validUsers = append(validUsers, u)
							}
						}
						if len(validUsers) != len(users) {
							changed = true
							if len(validUsers) == 0 {
								continue
							}
							ruleMap["auth_user"] = validUsers
						}
					}
					cleanRules = append(cleanRules, ruleMap)
				}
				if changed {
					route["rules"] = cleanRules
					rawConfig["route"] = route
					configBytes, _ := json.Marshal(rawConfig)
					form := url.Values{
						"object": {"config"},
						"action": {"edit"},
						"data":   {string(configBytes)},
					}
					_, _ = s.callAPI(http.MethodPost, "save", form)
				}
			}
		}
	}
	return nil
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
