package main

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// runTool 分发 sout-server tool <subcmd> [args...] 子命令
func runTool(args []string) {
	if len(args) == 0 {
		printToolHelp()
		return
	}
	subcmd := args[0]
	subArgs := args[1:]

	switch subcmd {
	case "json-get":
		toolJSONGet(subArgs)
	case "json-set":
		toolJSONSet(subArgs)
	case "ver-cmp":
		toolVerCmp(subArgs)
	case "sui-query":
		toolSUIQuery(subArgs)
	case "settings-get":
		toolSettingsGet(subArgs)
	case "settings-set":
		toolSettingsSet(subArgs)
	case "warp-proxy":
		toolWarpProxy(subArgs)
	case "sui-cleanup":
		toolSUICleanup(subArgs)
	case "singbox-clean-cf":
		toolSingboxCleanCF(subArgs)
	case "sui-backup":
		toolSUIBackup(subArgs)
	case "sui-restore":
		toolSUIRestore(subArgs)
	case "cf-dns-clean":
		toolCFDNSClean(subArgs)
	case "sui-clean-cf":
		toolSUICleanCF(subArgs)
	case "sui-sync-listen":
		toolSUISyncListen(subArgs)
	case "gateway-sout-sync":
		toolGatewaySoutSync(subArgs)
	case "singbox-parse-ports":
		toolSingboxParsePorts(subArgs)
	case "singbox-parse-tuic-hy2":
		toolSingboxParseTuicHy2(subArgs)
	case "singbox-apply-tuic-hy2":
		toolSingboxApplyTuicHy2(subArgs)
	case "sui-find-local-inbound":
		toolSUIFindLocalInbound(subArgs)
	case "sui-check-tuic-hy2":
		toolSUICheckTuicHy2(subArgs)
	case "sui-create-local-inbound":
		toolSUICreateLocalInbound(subArgs)
	case "sui-apply-tuic-hy2":
		toolSUIApplyTuicHy2(subArgs)
	case "sui-init-base-nodes":
		toolSUIInitBaseNodes(subArgs)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", subcmd)
		os.Exit(1)
	}
}

func printToolHelp() {
	fmt.Println("sout-server tool - 原生纯 Go 运维工具箱")
	fmt.Println("用法:")
	fmt.Println("  sout-server tool json-get <file> <key> [default]")
	fmt.Println("  sout-server tool json-set <file> <key> <val> [--type string|int|bool]")
	fmt.Println("  sout-server tool ver-cmp <v1> <v2>")
	fmt.Println("  sout-server tool sui-query <db_path> <username|has-cf|token>")
	fmt.Println("  sout-server tool settings-get <dir> <key> [default]")
	fmt.Println("  sout-server tool settings-set <dir> <key> <val>")
}

func toolJSONGet(args []string) {
	if len(args) < 2 {
		fmt.Println()
		return
	}
	file := args[0]
	key := args[1]
	defaultVal := ""
	if len(args) > 2 {
		defaultVal = args[2]
	}

	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println(defaultVal)
		return
	}

	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		fmt.Println(defaultVal)
		return
	}

	val, found := getNestedValue(root, key)
	if !found || val == nil {
		fmt.Println(defaultVal)
		return
	}

	switch v := val.(type) {
	case string:
		fmt.Println(v)
	case bool:
		if v {
			fmt.Println("true")
		} else {
			fmt.Println("false")
		}
	case float64:
		if v == float64(int64(v)) {
			fmt.Println(int64(v))
		} else {
			fmt.Println(strconv.FormatFloat(v, 'f', -1, 64))
		}
	case int:
		fmt.Println(v)
	case int64:
		fmt.Println(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			fmt.Println(defaultVal)
		} else {
			fmt.Println(string(b))
		}
	}
}

func getNestedValue(root any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	curr := root
	for _, part := range parts {
		m, ok := curr.(map[string]any)
		if !ok {
			return nil, false
		}
		val, exists := m[part]
		if !exists {
			return nil, false
		}
		curr = val
	}
	return curr, true
}

func toolJSONSet(args []string) {
	if len(args) < 3 {
		return
	}
	file := args[0]
	key := args[1]
	valStr := args[2]

	var typeHint string
	for i := 3; i < len(args); i++ {
		if args[i] == "--type" && i+1 < len(args) {
			typeHint = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--type=") {
			typeHint = strings.TrimPrefix(args[i], "--type=")
		}
	}

	var val any
	switch strings.ToLower(typeHint) {
	case "int", "integer":
		if n, err := strconv.ParseInt(valStr, 10, 64); err == nil {
			val = n
		} else {
			val = valStr
		}
	case "bool", "boolean":
		val = (valStr == "true" || valStr == "1")
	case "string":
		val = valStr
	default:
		if valStr == "true" {
			val = true
		} else if valStr == "false" {
			val = false
		} else if n, err := strconv.ParseInt(valStr, 10, 64); err == nil && (!strings.HasPrefix(valStr, "0") || valStr == "0") {
			val = n
		} else {
			val = valStr
		}
	}

	var root map[string]any
	data, err := os.ReadFile(file)
	if err == nil {
		_ = json.Unmarshal(data, &root)
	}
	if root == nil {
		root = make(map[string]any)
	}

	setNestedValue(root, key, val)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return
	}
	out = append(out, '\n')

	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, ".tmp-json-*")
	if err != nil {
		_ = os.WriteFile(file, out, 0600)
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Chmod(0600)
	_, _ = tmp.Write(out)
	_ = tmp.Close()

	_ = os.Rename(tmpPath, file)
	_ = os.Chmod(file, 0600)
}

func setNestedValue(root map[string]any, key string, val any) {
	parts := strings.Split(key, ".")
	curr := root
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		next, ok := curr[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			curr[part] = next
		}
		curr = next
	}
	curr[parts[len(parts)-1]] = val
}

func toolVerCmp(args []string) {
	if len(args) < 2 {
		fmt.Println("=")
		return
	}
	res := compareVersions(args[0], args[1])
	fmt.Println(res)
}

var verRegex = regexp.MustCompile(`\d+`)

func compareVersions(v1, v2 string) string {
	v1 = strings.TrimPrefix(strings.TrimSpace(v1), "v")
	v1 = strings.TrimPrefix(v1, "V")
	v2 = strings.TrimPrefix(strings.TrimSpace(v2), "v")
	v2 = strings.TrimPrefix(v2, "V")

	s1 := verRegex.FindAllString(v1, -1)
	s2 := verRegex.FindAllString(v2, -1)

	maxLen := len(s1)
	if len(s2) > maxLen {
		maxLen = len(s2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int64
		if i < len(s1) {
			n1, _ = strconv.ParseInt(s1[i], 10, 64)
		}
		if i < len(s2) {
			n2, _ = strconv.ParseInt(s2[i], 10, 64)
		}
		if n1 < n2 {
			return "<"
		}
		if n1 > n2 {
			return ">"
		}
	}
	return "="
}

func toolSettingsGet(args []string) {
	if len(args) < 2 {
		fmt.Println()
		return
	}
	dir := args[0]
	key := args[1]
	file := filepath.Join(dir, "settings.json")
	toolJSONGet(append([]string{file, key}, args[2:]...))
}

func toolSettingsSet(args []string) {
	if len(args) < 3 {
		return
	}
	dir := args[0]
	key := args[1]
	val := args[2]
	file := filepath.Join(dir, "settings.json")
	toolJSONSet(append([]string{file, key, val}, args[3:]...))
}

func toolSUIQuery(args []string) {
	if len(args) < 2 {
		fmt.Println()
		return
	}
	dbPath := args[0]
	qType := args[1]

	switch qType {
	case "username":
		fmt.Println(querySUIUsername(dbPath))
	case "has-cf":
		if querySUIHasCF(dbPath) {
			fmt.Println("true")
		} else {
			fmt.Println("false")
		}
	case "token":
		fmt.Println(querySUIToken(dbPath))
	default:
		fmt.Println()
	}
}

func querySUIUsername(dbPath string) string {
	if hasCmd("sqlite3") {
		out, err := exec.Command("sqlite3", dbPath, "SELECT username FROM users LIMIT 1;").Output()
		if err == nil {
			val := strings.TrimSpace(string(out))
			if val != "" {
				return val
			}
		}
	}
	res, err := runPureGoSQLite(dbPath, "SELECT username FROM users LIMIT 1;")
	if err == nil && res != "" {
		return res
	}
	return "admin"
}

func querySUIHasCF(dbPath string) bool {
	if hasCmd("sqlite3") {
		out, err := exec.Command("sqlite3", dbPath, "SELECT 1 FROM inbounds WHERE type='cloudflared' OR tag='cf-tunnel-in' OR tag LIKE 'cf-tunnel%' LIMIT 1;").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return true
		}
	}
	res, err := runPureGoSQLite(dbPath, "SELECT 1 FROM inbounds WHERE type='cloudflared' OR tag='cf-tunnel-in' LIMIT 1;")
	if err == nil && res == "1" {
		return true
	}
	if data, err := os.ReadFile(dbPath); err == nil {
		if bytes.Contains(data, []byte("cloudflared")) || bytes.Contains(data, []byte("cf-tunnel-in")) {
			return true
		}
	}
	return false
}

func querySUIToken(dbPath string) string {
	nowSec := strconv.FormatInt(time.Now().Unix(), 10)
	query := "SELECT token FROM tokens WHERE (desc='sout' OR desc='fanout') AND (expiry=0 OR expiry > " + nowSec + ") ORDER BY id DESC LIMIT 1;"
	if hasCmd("sqlite3") {
		out, err := exec.Command("sqlite3", dbPath, query).Output()
		if err == nil {
			tok := strings.TrimSpace(string(out))
			if tok != "" {
				return tok
			}
		}
	}
	res, err := runPureGoSQLite(dbPath, query)
	if err == nil && res != "" {
		return res
	}
	workDir := filepath.Dir(dbPath)
	if tokenBytes, err := os.ReadFile(filepath.Join(workDir, "sui-token")); err == nil {
		tok := strings.TrimSpace(string(tokenBytes))
		if tok != "" {
			return tok
		}
	}
	return ""
}

func toolWarpProxy(args []string) {
	if len(args) == 0 {
		return
	}
	stFile := args[0]
	data, err := os.ReadFile(stFile)
	if err != nil {
		return
	}
	var st struct {
		Tunnels []struct {
			IsChainUpstream bool   `json:"is_chain_upstream"`
			Status          string `json:"status"`
			Port            int    `json:"port"`
			SocksUser       string `json:"socks_user"`
			SocksPass       string `json:"socks_pass"`
			Country         string `json:"country"`
			Remark          string `json:"remark"`
			HostName        string `json:"hostname"`
		} `json:"tunnels"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}

	formatProxy := func(user, pass string, port int) string {
		if user != "" {
			return fmt.Sprintf("%s:%s@127.0.0.1:%d", user, pass, port)
		}
		return fmt.Sprintf("127.0.0.1:%d", port)
	}

	for _, t := range st.Tunnels {
		if t.IsChainUpstream && t.Status == "up" && t.Port > 0 {
			fmt.Println(formatProxy(t.SocksUser, t.SocksPass, t.Port))
			return
		}
	}
	for _, t := range st.Tunnels {
		c := strings.ToUpper(t.Country)
		rem := strings.ToUpper(t.Remark)
		h := strings.ToLower(t.HostName)
		if (strings.Contains(c, "WARP") || strings.Contains(rem, "WARP") || strings.Contains(h, "cloudflare") || strings.Contains(h, "cs-wg")) && t.Status == "up" && t.Port > 0 {
			fmt.Println(formatProxy(t.SocksUser, t.SocksPass, t.Port))
			return
		}
	}
	for _, t := range st.Tunnels {
		if t.Status == "up" && t.Port > 0 {
			fmt.Println(formatProxy(t.SocksUser, t.SocksPass, t.Port))
			return
		}
	}
}

func toolSUICleanup(args []string) {
	dbPath := "/usr/local/s-ui/db/s-ui.db"
	if len(args) > 0 {
		dbPath = args[0]
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return
	}
	if hasCmd("sqlite3") {
		_ = exec.Command("sqlite3", dbPath, "DELETE FROM outbounds WHERE tag LIKE 'sout-%' OR tag LIKE 'fanout-%';").Run()
		_ = exec.Command("sqlite3", dbPath, "DELETE FROM clients WHERE name LIKE 'sout-%' OR name LIKE 'fanout-%';").Run()
		_ = exec.Command("sqlite3", dbPath, "DELETE FROM tokens WHERE desc='sout' OR desc='fanout';").Run()
		_ = exec.Command("sqlite3", dbPath, "DELETE FROM inbounds WHERE tag LIKE '%家宽%' OR tag LIKE '%fanout%' OR tag LIKE '%sout%';").Run()
	}
}

func toolSingboxCleanCF(args []string) {
	cfgPath := "/etc/sing-box/config.json"
	if len(args) > 0 {
		cfgPath = args[0]
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var conf map[string]any
	if err := json.Unmarshal(data, &conf); err != nil {
		return
	}

	changed := false
	if inbs, ok := conf["inbounds"].([]any); ok {
		var newInbs []any
		for _, raw := range inbs {
			ib, ok := raw.(map[string]any)
			if !ok {
				newInbs = append(newInbs, raw)
				continue
			}
			tag, _ := ib["tag"].(string)
			tp, _ := ib["type"].(string)
			if tag == "cf-tunnel-in" || tp == "cloudflared" {
				changed = true
				continue
			}
			newInbs = append(newInbs, raw)
		}
		if changed {
			conf["inbounds"] = newInbs
		}
	}

	if route, ok := conf["route"].(map[string]any); ok {
		if rules, ok := route["rules"].([]any); ok {
			var newRules []any
			for _, raw := range rules {
				r, ok := raw.(map[string]any)
				if !ok {
					newRules = append(newRules, raw)
					continue
				}
				inbList := toSUITagSlice(r["inbound"])
				hasCF := false
				for _, inb := range inbList {
					if inb == "cf-tunnel-in" {
						hasCF = true
						break
					}
				}
				if hasCF {
					changed = true
					continue
				}
				newRules = append(newRules, raw)
			}
			if changed {
				route["rules"] = newRules
			}
		}
	}

	if changed {
		if out, err := json.MarshalIndent(conf, "", "  "); err == nil {
			_ = os.WriteFile(cfgPath, out, 0644)
		}
	}
}

func toolSUICleanCF(args []string) {
	dbPath := "/usr/local/s-ui/db/s-ui.db"
	if len(args) > 0 {
		dbPath = args[0]
	}
	if hasCmd("sqlite3") {
		_ = exec.Command("sqlite3", dbPath, "DELETE FROM inbounds WHERE tag='cf-tunnel-in' OR type='cloudflared';").Run()
	}
}

func toolSUIBackup(args []string) {
	if len(args) < 2 {
		return
	}
	dbPath := args[0]
	backupFile := args[1]
	keys := []string{
		"webPort", "webListen", "webPath", "webDomain", "webCertFile", "webKeyFile", "webURI",
		"subPort", "subListen", "subPath", "subDomain", "subCertFile", "subKeyFile", "subURI", "subUpdates",
		"subEncode", "subShowInfo", "subClashExt", "subJsonExt", "subClashSprtAll", "subClashNoDefGrp",
	}
	backup := make(map[string]string)
	for _, k := range keys {
		val, _ := runSQLite(dbPath, fmt.Sprintf("SELECT value FROM settings WHERE key='%s';", k))
		if val != "" {
			backup[k] = val
		}
	}
	if len(backup) > 0 {
		if out, err := json.MarshalIndent(backup, "", "  "); err == nil {
			_ = os.WriteFile(backupFile, out, 0600)
		}
	}
}

func toolSUIRestore(args []string) {
	if len(args) < 3 {
		fmt.Println("8443|/app/|8444|/sub/|http|http||")
		return
	}
	dbPath := args[0]
	backupFile := args[1]
	pubIP := args[2]

	isPortFree := func(port int) bool {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = ln.Close()
			return true
		}
		return false
	}

	getFreePort := func(start int) int {
		if isPortFree(start) {
			return start
		}
		for p := start + 1; p < 65000; p++ {
			if isPortFree(p) {
				return p
			}
		}
		return start
	}

	var b map[string]string
	if data, err := os.ReadFile(backupFile); err == nil {
		_ = json.Unmarshal(data, &b)
	}
	if b == nil {
		b = make(map[string]string)
	}

	origWP, _ := strconv.Atoi(b["webPort"])
	if origWP <= 0 {
		origWP = 8443
	}
	finalWP := origWP
	if !isPortFree(origWP) {
		finalWP = getFreePort(8443)
	}

	origWPath := b["webPath"]
	if origWPath == "" {
		origWPath = "/app/"
	}
	if !strings.HasPrefix(origWPath, "/") {
		origWPath = "/" + origWPath
	}
	if !strings.HasSuffix(origWPath, "/") {
		origWPath += "/"
	}

	origSP, _ := strconv.Atoi(b["subPort"])
	if origSP <= 0 {
		origSP = 8444
	}
	finalSP := origSP
	if !isPortFree(origSP) {
		finalSP = getFreePort(8444)
	}

	origSPath := b["subPath"]
	if origSPath == "" {
		origSPath = "/sub/"
	}
	if !strings.HasPrefix(origSPath, "/") {
		origSPath = "/" + origSPath
	}
	if !strings.HasSuffix(origSPath, "/") {
		origSPath += "/"
	}

	origWCert := b["webCertFile"]
	origWKey := b["webKeyFile"]
	origSCert := b["subCertFile"]
	origSKey := b["subKeyFile"]
	origWDom := b["webDomain"]
	origSDom := b["subDomain"]

	proto := "http"
	if origWCert != "" && origWKey != "" {
		proto = "https"
	}
	subProto := "http"
	if origSCert != "" && origSKey != "" {
		subProto = "https"
	}

	hostWeb := origWDom
	if hostWeb == "" {
		hostWeb = fmt.Sprintf("%s:%d", pubIP, finalWP)
	}
	hostSub := origSDom
	if hostSub == "" {
		hostSub = fmt.Sprintf("%s:%d", pubIP, finalSP)
	}

	if hasCmd("sqlite3") {
		execSQL := func(query string) {
			_ = exec.Command("sqlite3", dbPath, query).Run()
		}
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='webCertFile';", origWCert))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='webKeyFile';", origWKey))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='subCertFile';", origSCert))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='subKeyFile';", origSKey))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='webDomain';", origWDom))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='subDomain';", origSDom))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%d' WHERE key='webPort';", finalWP))
		execSQL("UPDATE settings SET value='' WHERE key='webListen';")
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='webPath';", origWPath))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s://%s%s' WHERE key='webURI';", proto, hostWeb, origWPath))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%d' WHERE key='subPort';", finalSP))
		execSQL("UPDATE settings SET value='' WHERE key='subListen';")
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='subPath';", origSPath))
		execSQL(fmt.Sprintf("UPDATE settings SET value='%s://%s%s' WHERE key='subURI';", subProto, hostSub, origSPath))
	}

	fmt.Printf("%d|%s|%d|%s|%s|%s|%s|%s\n", finalWP, origWPath, finalSP, origSPath, proto, subProto, origWDom, origSDom)
}

func toolCFDNSClean(args []string) {
	if len(args) < 4 {
		return
	}
	metaPath := args[0]
	domain := args[1]
	cfToken := args[2]
	certDir := args[3]

	client := &http.Client{Timeout: 10 * time.Second}
	parts := strings.Split(domain, ".")
	if len(parts) >= 2 {
		rootZone := strings.Join(parts[len(parts)-2:], ".")
		req, _ := http.NewRequest("GET", "https://api.cloudflare.com/client/v4/zones?name="+rootZone, nil)
		req.Header.Set("Authorization", "Bearer "+cfToken)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err == nil {
			defer resp.Body.Close()
			var zData struct {
				Result []struct {
					ID string `json:"id"`
				} `json:"result"`
			}
			if json.NewDecoder(resp.Body).Decode(&zData) == nil && len(zData.Result) > 0 {
				zID := zData.Result[0].ID
				recordURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=_acme-challenge.%s", zID, domain)
				req2, _ := http.NewRequest("GET", recordURL, nil)
				req2.Header.Set("Authorization", "Bearer "+cfToken)
				if resp2, err := client.Do(req2); err == nil {
					defer resp2.Body.Close()
					var rData struct {
						Result []struct {
							ID string `json:"id"`
						} `json:"result"`
					}
					if json.NewDecoder(resp2.Body).Decode(&rData) == nil {
						for _, rec := range rData.Result {
							delURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zID, rec.ID)
							reqDel, _ := http.NewRequest("DELETE", delURL, nil)
							reqDel.Header.Set("Authorization", "Bearer "+cfToken)
							_, _ = client.Do(reqDel)
						}
					}
				}
			}
		}
	}

	var d map[string]any
	if data, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(data, &d)
	}
	if d == nil {
		d = make(map[string]any)
	}
	d[domain] = map[string]any{
		"token":      cfToken,
		"applied_at": time.Now().Unix(),
		"cert_dir":   certDir,
	}
	if out, err := json.MarshalIndent(d, "", "  "); err == nil {
		_ = os.WriteFile(metaPath, out, 0600)
	}
}

func toolGatewaySoutSync(args []string) {
	if len(args) < 2 {
		fmt.Println("8899|False")
		return
	}
	settingsPath := args[0]
	domain := args[1]
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("8899|False")
		return
	}
	var d map[string]any
	if err := json.Unmarshal(data, &d); err != nil {
		fmt.Println("8899|False")
		return
	}

	p := 8899
	if portVal, ok := d["port"].(float64); ok && portVal > 0 {
		p = int(portVal)
	}
	la, _ := d["listen_addr"].(string)
	purl, _ := d["panel_url"].(string)
	targetURL := "https://" + domain
	changed := false
	needsRestart := false
	if la != "127.0.0.1" {
		d["listen_addr"] = "127.0.0.1"
		changed = true
		needsRestart = true
	}
	if purl != targetURL {
		d["panel_url"] = targetURL
		changed = true
	}
	if changed {
		if out, err := json.MarshalIndent(d, "", "  "); err == nil {
			_ = os.WriteFile(settingsPath, out, 0600)
		}
	}
	if needsRestart {
		fmt.Printf("%d|True\n", p)
	} else {
		fmt.Printf("%d|False\n", p)
	}
}

func toolSUISyncListen(args []string) {
	if len(args) < 3 {
		return
	}
	dbPath := args[0]
	domain := args[1]
	subPath := args[2]
	if !hasCmd("sqlite3") {
		return
	}
	out, _ := exec.Command("sqlite3", dbPath, "SELECT value FROM settings WHERE key='webPort';").Output()
	port := strings.TrimSpace(string(out))
	if port == "" {
		port = "2096"
	}
	out, _ = exec.Command("sqlite3", dbPath, "SELECT value FROM settings WHERE key='webPath';").Output()
	path := strings.TrimSpace(string(out))
	if path == "" {
		path = "sui"
	}
	out, _ = exec.Command("sqlite3", dbPath, "SELECT value FROM settings WHERE key='subPort';").Output()
	sp := strings.TrimSpace(string(out))
	if sp == "" {
		sp = "2097"
	}

	targetWebURI := fmt.Sprintf("https://%s/%s/", domain, strings.Trim(path, "/"))
	targetSubURI := fmt.Sprintf("https://%s/%s/", domain, strings.Trim(subPath, "/"))

	_ = exec.Command("sqlite3", dbPath, "UPDATE settings SET value='127.0.0.1' WHERE key='webListen';").Run()
	_ = exec.Command("sqlite3", dbPath, "UPDATE settings SET value='127.0.0.1' WHERE key='subListen';").Run()
	_ = exec.Command("sqlite3", dbPath, fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='webURI';", targetWebURI)).Run()
	_ = exec.Command("sqlite3", dbPath, fmt.Sprintf("UPDATE settings SET value='%s' WHERE key='subURI';", targetSubURI)).Run()

	fmt.Printf("%s|%s|True|%s\n", port, strings.Trim(path, "/"), sp)
}

// 纯 Go SQLite 原生解析器
type sqliteCellRecord struct {
	RowID  int64
	Values []SQLiteValue
}

type SQLiteValue struct {
	Type    int // 0: null, 1: int, 2: float, 3: string, 4: blob
	IntVal  int64
	StrVal  string
	BlobVal []byte
}

func readVarint(buf []byte, offset int) (uint64, int) {
	var res uint64
	for i := 0; i < 8; i++ {
		if offset+i >= len(buf) {
			return res, i
		}
		b := buf[offset+i]
		res = (res << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			return res, i + 1
		}
	}
	if offset+8 < len(buf) {
		res = (res << 8) | uint64(buf[offset+8])
		return res, 9
	}
	return res, 8
}

func parseRecord(payload []byte) []SQLiteValue {
	if len(payload) == 0 {
		return nil
	}
	hdrLen, n := readVarint(payload, 0)
	if hdrLen == 0 || int(hdrLen) > len(payload) {
		return nil
	}

	var serialTypes []uint64
	off := n
	for off < int(hdrLen) {
		st, sn := readVarint(payload, off)
		serialTypes = append(serialTypes, st)
		off += sn
	}

	values := make([]SQLiteValue, len(serialTypes))
	bodyOff := int(hdrLen)
	for i, st := range serialTypes {
		switch {
		case st == 0:
			values[i] = SQLiteValue{Type: 0}
		case st == 1:
			if bodyOff < len(payload) {
				values[i] = SQLiteValue{Type: 1, IntVal: int64(int8(payload[bodyOff]))}
				bodyOff++
			}
		case st == 2:
			if bodyOff+2 <= len(payload) {
				values[i] = SQLiteValue{Type: 1, IntVal: int64(int16(binary.BigEndian.Uint16(payload[bodyOff:])))}
				bodyOff += 2
			}
		case st == 3:
			if bodyOff+3 <= len(payload) {
				val := int64(payload[bodyOff])<<16 | int64(payload[bodyOff+1])<<8 | int64(payload[bodyOff+2])
				if val&0x800000 != 0 {
					val |= ^int64(0xffffff)
				}
				values[i] = SQLiteValue{Type: 1, IntVal: val}
				bodyOff += 3
			}
		case st == 4:
			if bodyOff+4 <= len(payload) {
				values[i] = SQLiteValue{Type: 1, IntVal: int64(int32(binary.BigEndian.Uint32(payload[bodyOff:])))}
				bodyOff += 4
			}
		case st == 5:
			if bodyOff+6 <= len(payload) {
				val := int64(binary.BigEndian.Uint32(payload[bodyOff:]))<<16 | int64(binary.BigEndian.Uint16(payload[bodyOff+4:]))
				values[i] = SQLiteValue{Type: 1, IntVal: val}
				bodyOff += 6
			}
		case st == 6:
			if bodyOff+8 <= len(payload) {
				values[i] = SQLiteValue{Type: 1, IntVal: int64(binary.BigEndian.Uint64(payload[bodyOff:]))}
				bodyOff += 8
			}
		case st == 8:
			values[i] = SQLiteValue{Type: 1, IntVal: 0}
		case st == 9:
			values[i] = SQLiteValue{Type: 1, IntVal: 1}
		case st >= 12 && st%2 == 0:
			blen := int((st - 12) / 2)
			if bodyOff+blen <= len(payload) {
				values[i] = SQLiteValue{Type: 4, BlobVal: payload[bodyOff : bodyOff+blen]}
				bodyOff += blen
			}
		case st >= 13 && st%2 == 1:
			slen := int((st - 13) / 2)
			if bodyOff+slen <= len(payload) {
				values[i] = SQLiteValue{Type: 3, StrVal: string(payload[bodyOff : bodyOff+slen])}
				bodyOff += slen
			}
		}
	}
	return values
}

func parseSQLitePages(dbData []byte) (map[string]int, [][]sqliteCellRecord) {
	if len(dbData) < 100 || !bytes.HasPrefix(dbData, []byte("SQLite format 3\000")) {
		return nil, nil
	}
	pageSize := int(binary.BigEndian.Uint16(dbData[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize == 0 || len(dbData) < pageSize {
		return nil, nil
	}

	totalPages := len(dbData) / pageSize
	allPageCells := make([][]sqliteCellRecord, totalPages+1)

	for p := 1; p <= totalPages; p++ {
		start := (p - 1) * pageSize
		end := start + pageSize
		if end > len(dbData) {
			break
		}
		page := dbData[start:end]
		hdrOff := 0
		if p == 1 {
			hdrOff = 100
		}
		if hdrOff >= len(page) {
			continue
		}

		pageType := page[hdrOff]
		if pageType != 0x0D {
			continue
		}

		numCells := int(binary.BigEndian.Uint16(page[hdrOff+3 : hdrOff+5]))
		cellPtrsOff := hdrOff + 8
		var cells []sqliteCellRecord

		for c := 0; c < numCells; c++ {
			if cellPtrsOff+2 > len(page) {
				break
			}
			cellPtr := int(binary.BigEndian.Uint16(page[cellPtrsOff : cellPtrsOff+2]))
			cellPtrsOff += 2
			if cellPtr >= len(page) {
				continue
			}

			payloadSize, n1 := readVarint(page, cellPtr)
			rowID, n2 := readVarint(page, cellPtr+n1)
			dataStart := cellPtr + n1 + n2
			dataEnd := dataStart + int(payloadSize)
			if dataEnd > len(page) {
				dataEnd = len(page)
			}
			if dataStart < len(page) && dataStart < dataEnd {
				payload := page[dataStart:dataEnd]
				rec := parseRecord(payload)
				cells = append(cells, sqliteCellRecord{RowID: int64(rowID), Values: rec})
			}
		}
		allPageCells[p] = cells
	}

	tableRoots := make(map[string]int)
	if len(allPageCells) > 1 {
		for _, cell := range allPageCells[1] {
			if len(cell.Values) >= 4 {
				tblType := cell.Values[0].StrVal
				tblName := cell.Values[1].StrVal
				rootPage := int(cell.Values[3].IntVal)
				if tblType == "table" && tblName != "" && rootPage > 0 {
					tableRoots[tblName] = rootPage
				}
			}
		}
	}

	return tableRoots, allPageCells
}

func runPureGoSQLite(dbPath string, query string) (string, error) {
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return "", err
	}
	tableRoots, allCells := parseSQLitePages(data)
	if tableRoots == nil {
		return "", fmt.Errorf("无法解析 SQLite 数据库")
	}

	q := strings.TrimSpace(query)
	qUpper := strings.ToUpper(q)

	if strings.Contains(qUpper, "FROM SETTINGS") && strings.Contains(qUpper, "KEY=") {
		re := regexp.MustCompile(`(?i)key\s*=\s*['"]([^'"]+)['"]`)
		m := re.FindStringSubmatch(q)
		if len(m) > 1 {
			targetKey := m[1]
			rootPage := tableRoots["settings"]
			if rootPage > 0 && rootPage < len(allCells) {
				for _, cell := range allCells[rootPage] {
					if len(cell.Values) >= 2 {
						k := cell.Values[0].StrVal
						v := cell.Values[1].StrVal
						if k == targetKey {
							return v, nil
						}
					}
				}
			}
		}
		return "", nil
	}

	if strings.Contains(qUpper, "FROM USERS") {
		rootPage := tableRoots["users"]
		if rootPage > 0 && rootPage < len(allCells) {
			for _, cell := range allCells[rootPage] {
				if strings.Contains(qUpper, "USERNAME") {
					if len(cell.Values) >= 2 && cell.Values[1].StrVal != "" {
						return cell.Values[1].StrVal, nil
					}
				} else if strings.Contains(qUpper, "ID") {
					if len(cell.Values) >= 1 {
						return strconv.FormatInt(cell.Values[0].IntVal, 10), nil
					}
				}
			}
		}
		if strings.Contains(qUpper, "USERNAME") {
			return "admin", nil
		}
		if strings.Contains(qUpper, "ID") {
			return "1", nil
		}
	}

	if strings.Contains(qUpper, "FROM INBOUNDS") {
		rootPage := tableRoots["inbounds"]
		if rootPage > 0 && rootPage < len(allCells) {
			for _, cell := range allCells[rootPage] {
				for _, v := range cell.Values {
					if v.StrVal == "cloudflared" || v.StrVal == "cf-tunnel-in" || strings.HasPrefix(v.StrVal, "cf-tunnel") {
						return "1", nil
					}
				}
			}
		}
		return "", nil
	}

	if strings.Contains(qUpper, "FROM TOKENS") {
		rootPage := tableRoots["tokens"]
		if rootPage > 0 && rootPage < len(allCells) {
			cells := allCells[rootPage]
			now := time.Now().Unix()
			for i := len(cells) - 1; i >= 0; i-- {
				cell := cells[i]
				if len(cell.Values) >= 4 {
					var desc, token string
					var expiry int64
					for _, v := range cell.Values {
						if v.StrVal == "sout" || v.StrVal == "fanout" {
							desc = v.StrVal
						} else if len(v.StrVal) >= 16 {
							token = v.StrVal
						} else if v.IntVal > 0 {
							expiry = v.IntVal
						}
					}
					if desc != "" && token != "" {
						if expiry == 0 || expiry > now {
							return token, nil
						}
					}
				}
			}
		}
		return "", nil
	}

	return "", nil
}

func runPureGoSQLiteJSON(dbPath string, query string) ([]byte, error) {
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, err
	}
	tableRoots, allCells := parseSQLitePages(data)
	if tableRoots == nil {
		return nil, fmt.Errorf("无法解析 SQLite 数据库")
	}

	qUpper := strings.ToUpper(query)

	if strings.Contains(qUpper, "FROM TOKENS") {
		rootPage := tableRoots["tokens"]
		var rows []map[string]any
		now := time.Now().Unix()
		if rootPage > 0 && rootPage < len(allCells) {
			cells := allCells[rootPage]
			for i := len(cells) - 1; i >= 0; i-- {
				cell := cells[i]
				var desc, token string
				var id int64
				var expiry int64
				if cell.RowID > 0 {
					id = cell.RowID
				}
				for _, v := range cell.Values {
					if v.StrVal == "sout" || v.StrVal == "fanout" {
						desc = v.StrVal
					} else if len(v.StrVal) >= 16 {
						token = v.StrVal
					} else if v.IntVal > 0 {
						if id == 0 {
							id = v.IntVal
						} else {
							expiry = v.IntVal
						}
					}
				}
				if desc != "" && token != "" && (expiry == 0 || expiry > now) {
					rows = append(rows, map[string]any{
						"id":    id,
						"token": token,
					})
				}
			}
		}
		return json.Marshal(rows)
	}

	if strings.Contains(qUpper, "FROM CLIENTS") {
		rootPage := tableRoots["clients"]
		var rows []map[string]any
		if rootPage > 0 && rootPage < len(allCells) {
			for _, cell := range allCells[rootPage] {
				item := make(map[string]any)
				if cell.RowID > 0 {
					item["id"] = cell.RowID
				}
				for _, v := range cell.Values {
					if v.Type == 3 {
						s := v.StrVal
						if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
							item["config"] = json.RawMessage(s)
						} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
							if strings.Contains(s, "http") || strings.Contains(s, "vmess") || strings.Contains(s, "vless") {
								item["links"] = json.RawMessage(s)
							} else {
								item["inbounds"] = json.RawMessage(s)
							}
						} else if item["name"] == nil && len(s) > 0 && !strings.Contains(s, " ") {
							item["name"] = s
						} else if item["remark"] == nil && len(s) > 0 {
							item["remark"] = s
						}
					} else if v.Type == 1 {
						if item["id"] == nil {
							item["id"] = v.IntVal
						} else if item["enable"] == nil {
							item["enable"] = (v.IntVal == 1)
						}
					}
				}
				if item["id"] != nil {
					rows = append(rows, item)
				}
			}
		}
		return json.Marshal(rows)
	}

	return []byte("[]"), nil
}

func toolSingboxParsePorts(args []string) {
	cfgPath := "/etc/sing-box/config.json"
	if len(args) > 0 {
		cfgPath = args[0]
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var conf map[string]any
	if err := json.Unmarshal(data, &conf); err != nil {
		return
	}
	inbounds, _ := conf["inbounds"].([]any)
	for _, raw := range inbounds {
		ib, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := ib["tag"].(string)
		tp, _ := ib["type"].(string)
		lp, _ := ib["listen_port"].(float64)
		tr, _ := ib["transport"].(map[string]any)
		tlsMap, _ := ib["tls"].(map[string]any)

		if (strings.Contains(t, "argo") || (tp == "vless" || tp == "vmess") && tr != nil && tr["type"] == "ws") && lp > 0 {
			fmt.Printf("old_node_port=%d\n", int(lp))
			if tr != nil {
				trP, _ := tr["path"].(string)
				if trP != "" {
					fmt.Printf("old_ws_path=%s\n", strings.Trim(trP, "/"))
				}
			}
		} else if (strings.Contains(t, "reality") || (tlsMap != nil && tlsMap["reality"] != nil)) && lp > 0 {
			fmt.Printf("old_reality_port=%d\n", int(lp))
		}
	}
}

func toolSingboxParseTuicHy2(args []string) {
	cfgPath := "/etc/sing-box/config.json"
	if len(args) > 0 && args[0] != "" {
		cfgPath = args[0]
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Println("||||")
		return
	}
	var conf map[string]any
	if err := json.Unmarshal(data, &conf); err != nil {
		fmt.Println("||||")
		return
	}
	inbounds, _ := conf["inbounds"].([]any)
	var tP, hP, tUID, tPass, hPass string
	for _, raw := range inbounds {
		ib, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := ib["type"].(string)
		lp, _ := ib["listen_port"].(float64)
		users, _ := ib["users"].([]any)
		if t == "tuic" && tP == "" {
			if lp > 0 {
				tP = strconv.Itoa(int(lp))
			}
			if len(users) > 0 {
				if uMap, ok := users[0].(map[string]any); ok {
					if uid, ok := uMap["uuid"].(string); ok {
						tUID = uid
					}
					if pwd, ok := uMap["password"].(string); ok {
						tPass = pwd
					}
				}
			}
		} else if t == "hysteria2" && hP == "" {
			if lp > 0 {
				hP = strconv.Itoa(int(lp))
			}
			if len(users) > 0 {
				if uMap, ok := users[0].(map[string]any); ok {
					if pwd, ok := uMap["password"].(string); ok {
						hPass = pwd
					}
				}
			}
		}
	}
	fmt.Printf("%s|%s|%s|%s|%s\n", tP, hP, tUID, tPass, hPass)
}

func genRandSuffix() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = charset[time.Now().UnixNano()%int64(len(charset))]
		time.Sleep(time.Nanosecond * 50)
	}
	return string(b)
}

func toolSingboxApplyTuicHy2(args []string) {
	cfgPath := os.Getenv("SB_CONF")
	if cfgPath == "" && len(args) > 0 {
		cfgPath = args[0]
	}
	if cfgPath == "" {
		cfgPath = "/etc/sing-box/config.json"
	}

	tuicP, _ := strconv.Atoi(os.Getenv("TUIC_PORT"))
	hy2P, _ := strconv.Atoi(os.Getenv("HY2_PORT"))
	tuicUUID := os.Getenv("TUIC_UUID")
	tuicPass := os.Getenv("TUIC_PASS")
	hy2Pass := os.Getenv("HY2_PASS")
	certDom := os.Getenv("CERT_DOM")
	certFile := os.Getenv("CERT_FILE")
	keyFile := os.Getenv("KEY_FILE")
	nodeServer := os.Getenv("NODE_SERVER")
	isInsecure := (os.Getenv("IS_INSECURE") == "true")
	cc := os.Getenv("CONGESTION_CTRL")
	if cc == "" {
		cc = "cubic"
	}

	var conf map[string]any
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(data, &conf)
	}
	if conf == nil {
		conf = make(map[string]any)
	}

	inbounds, _ := conf["inbounds"].([]any)
	tuicTag := ""
	hy2Tag := ""
	for _, raw := range inbounds {
		ib, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := ib["type"].(string)
		tg, _ := ib["tag"].(string)
		lp, _ := ib["listen_port"].(float64)
		users, _ := ib["users"].([]any)
		if t == "tuic" && tuicTag == "" {
			tuicTag = tg
			if lp > 0 && tuicP <= 0 {
				tuicP = int(lp)
			}
			if len(users) > 0 {
				if uMap, ok := users[0].(map[string]any); ok {
					if uid, ok := uMap["uuid"].(string); ok && tuicUUID == "" {
						tuicUUID = uid
					}
					if pwd, ok := uMap["password"].(string); ok && tuicPass == "" {
						tuicPass = pwd
					}
				}
			}
		} else if t == "hysteria2" && hy2Tag == "" {
			hy2Tag = tg
			if lp > 0 && hy2P <= 0 {
				hy2P = int(lp)
			}
			if len(users) > 0 {
				if uMap, ok := users[0].(map[string]any); ok {
					if pwd, ok := uMap["password"].(string); ok && hy2Pass == "" {
						hy2Pass = pwd
					}
				}
			}
		}
	}

	if tuicTag == "" || tuicTag == "tuic-in" {
		tuicTag = "tuic-" + genRandSuffix()
	}
	if hy2Tag == "" || hy2Tag == "hy2-in" {
		hy2Tag = "hysteria2-" + genRandSuffix()
	}

	var cleaned []any
	for _, raw := range inbounds {
		ib, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := ib["type"].(string)
		lp, _ := ib["listen_port"].(float64)
		tg, _ := ib["tag"].(string)
		if t == "tuic" || t == "hysteria2" || int(lp) == tuicP || int(lp) == hy2P ||
			strings.HasPrefix(tg, "tuic-") || strings.HasPrefix(tg, "hysteria2-") || tg == "tuic-in" || tg == "hy2-in" {
			continue
		}
		cleaned = append(cleaned, ib)
	}

	cleaned = append(cleaned, map[string]any{
		"type":        "tuic",
		"tag":         tuicTag,
		"listen":      "::",
		"listen_port": tuicP,
		"users": []any{
			map[string]any{
				"name":     "admin",
				"uuid":     tuicUUID,
				"password": tuicPass,
			},
		},
		"congestion_control": cc,
		"tls": map[string]any{
			"enabled":          true,
			"server_name":      certDom,
			"alpn":             []string{"h3"},
			"certificate_path": certFile,
			"key_path":         keyFile,
		},
	})

	cleaned = append(cleaned, map[string]any{
		"type":                    "hysteria2",
		"tag":                     hy2Tag,
		"listen":                  "::",
		"listen_port":             hy2P,
		"ignore_client_bandwidth": true,
		"users": []any{
			map[string]any{
				"name":     "admin",
				"password": hy2Pass,
			},
		},
		"tls": map[string]any{
			"enabled":          true,
			"server_name":      certDom,
			"alpn":             []string{"h3"},
			"certificate_path": certFile,
			"key_path":         keyFile,
		},
	})

	conf["inbounds"] = cleaned
	if out, err := json.MarshalIndent(conf, "", "  "); err == nil {
		_ = os.WriteFile(cfgPath, out, 0644)
	}

	addrsPath := "/var/lib/sout/singbox_inbound_addrs.json"
	var addrsData map[string]any
	if data, err := os.ReadFile(addrsPath); err == nil {
		_ = json.Unmarshal(data, &addrsData)
	}
	if addrsData == nil {
		addrsData = make(map[string]any)
	}

	srvHost := nodeServer
	if srvHost == "" {
		srvHost = certDom
	}
	v4Host := os.Getenv("PUBLIC_IP")
	v6Host := os.Getenv("HOST_PUBLIC_IPV6")
	if v6Host == "" {
		v6Host = os.Getenv("PUBLIC_IPV6")
	}
	if v6Host == "" {
		if d, err := os.ReadFile("/var/lib/sout/host_ipv6"); err == nil {
			v6Host = strings.TrimSpace(string(d))
		}
	}
	if v4Host == "" {
		if d, err := os.ReadFile("/var/lib/sout/host_ipv4"); err == nil {
			v4Host = strings.TrimSpace(string(d))
		}
	}

	var baseDirectHosts []string
	if srvHost != "" && srvHost != v4Host && srvHost != v6Host {
		baseDirectHosts = append(baseDirectHosts, srvHost)
	}
	if v4Host != "" && v4Host != "127.0.0.1" {
		found := false
		for _, h := range baseDirectHosts {
			if h == v4Host {
				found = true
				break
			}
		}
		if !found {
			baseDirectHosts = append(baseDirectHosts, v4Host)
		}
	}
	if v6Host != "" && v6Host != "127.0.0.1" {
		found := false
		for _, h := range baseDirectHosts {
			if h == v6Host {
				found = true
				break
			}
		}
		if !found {
			baseDirectHosts = append(baseDirectHosts, v6Host)
		}
	}
	if len(baseDirectHosts) == 0 {
		h := srvHost
		if h == "" {
			h = "127.0.0.1"
		}
		baseDirectHosts = []string{h}
	}

	tuicList := make([]any, 0, len(baseDirectHosts))
	for _, h := range baseDirectHosts {
		tuicList = append(tuicList, map[string]any{
			"server":      h,
			"server_port": tuicP,
			"tls": map[string]any{
				"enabled":     true,
				"insecure":    isInsecure,
				"server_name": certDom,
			},
		})
	}
	addrsData[tuicTag] = tuicList

	hy2List := make([]any, 0, len(baseDirectHosts))
	for _, h := range baseDirectHosts {
		hy2List = append(hy2List, map[string]any{
			"server":      h,
			"server_port": hy2P,
			"tls": map[string]any{
				"enabled":     true,
				"insecure":    isInsecure,
				"server_name": certDom,
			},
		})
	}
	addrsData[hy2Tag] = hy2List

	_ = os.MkdirAll(filepath.Dir(addrsPath), 0755)
	if out, err := json.MarshalIndent(addrsData, "", "  "); err == nil {
		_ = os.WriteFile(addrsPath, out, 0644)
	}

	varsPath := "/var/lib/sout/.tuic_hy2_vars"
	content := fmt.Sprintf("tuic_uuid='%s'\ntuic_pass='%s'\nhy2_pass='%s'\ntuic_port=%d\nhy2_port=%d\n",
		tuicUUID, tuicPass, hy2Pass, tuicP, hy2P)
	_ = os.WriteFile(varsPath, []byte(content), 0600)
}

func toolSUIFindLocalInbound(args []string) {
	dbPath := "/usr/local/s-ui/db/s-ui.db"
	if len(args) > 0 && args[0] != "" {
		dbPath = args[0]
	}
	if !hasCmd("sqlite3") && !fileExists(dbPath) {
		fmt.Println("CREATE")
		return
	}

	if hasCmd("sqlite3") {
		out, errCmd := exec.Command("sqlite3", dbPath, "SELECT options FROM inbounds;").Output()
		if errCmd == nil {
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var opt map[string]any
				if json.Unmarshal([]byte(line), &opt) == nil {
					listen, _ := opt["listen"].(string)
					lp, _ := opt["listen_port"].(float64)
					if listen == "127.0.0.1" && lp > 0 {
						p := strconv.Itoa(int(lp))
						path := "vlws"
						if tr, ok := opt["transport"].(map[string]any); ok {
							if tp, ok := tr["path"].(string); ok && tp != "" {
								path = strings.Trim(tp, "/")
							}
						}
						fmt.Printf("FOUND|%s|%s\n", p, path)
						return
					}
				}
			}
			fmt.Println("CREATE")
			return
		}
	}

	jsonRows, err := runPureGoSQLiteJSON(dbPath, "inbounds")
	if err == nil && len(jsonRows) > 0 {
		var inbounds []map[string]any
		if json.Unmarshal(jsonRows, &inbounds) == nil {
			for _, ib := range inbounds {
				optRaw, ok := ib["options"]
				if !ok {
					continue
				}
				var opt map[string]any
				switch v := optRaw.(type) {
				case map[string]any:
					opt = v
				case string:
					_ = json.Unmarshal([]byte(v), &opt)
				case json.RawMessage:
					_ = json.Unmarshal(v, &opt)
				}
				if opt != nil {
					listen, _ := opt["listen"].(string)
					lp, _ := opt["listen_port"].(float64)
					if listen == "127.0.0.1" && lp > 0 {
						p := strconv.Itoa(int(lp))
						path := "vlws"
						if tr, ok := opt["transport"].(map[string]any); ok {
							if tp, ok := tr["path"].(string); ok && tp != "" {
								path = strings.Trim(tp, "/")
							}
						}
						fmt.Printf("FOUND|%s|%s\n", p, path)
						return
					}
				}
			}
		}
	}
	fmt.Println("CREATE")
}

func toolSUICheckTuicHy2(args []string) {
	apiBase := os.Getenv("SUI_API")
	token := os.Getenv("SUI_TOKEN")
	if len(args) > 0 && args[0] != "" {
		apiBase = args[0]
	}
	if len(args) > 1 && args[1] != "" {
		token = args[1]
	}
	if apiBase == "" {
		return
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", strings.TrimRight(apiBase, "/")+"/inbounds", nil)
	if err != nil {
		return
	}
	req.Header.Set("Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var data struct {
		Obj struct {
			Inbounds []struct {
				Type       string `json:"type"`
				Tag        string `json:"tag"`
				ListenPort int    `json:"listen_port"`
				TLSID      int    `json:"tls_id"`
			} `json:"inbounds"`
		} `json:"obj"`
	}
	if json.NewDecoder(resp.Body).Decode(&data) != nil {
		return
	}

	var parts []string
	for _, ib := range data.Obj.Inbounds {
		if ib.Type == "tuic" {
			parts = append(parts, fmt.Sprintf("TUIC|%s|%d|%d", ib.Tag, ib.ListenPort, ib.TLSID))
		} else if ib.Type == "hysteria2" {
			parts = append(parts, fmt.Sprintf("Hysteria2|%s|%d|%d", ib.Tag, ib.ListenPort, ib.TLSID))
		}
	}
	fmt.Println(strings.Join(parts, ";"))
}

func suiAPICall(baseURL, token, method, endpoint string, form url.Values) (map[string]any, error) {
	fullURL := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	var req *http.Request
	var err error
	if form != nil && len(form) > 0 {
		req, err = http.NewRequest(method, fullURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequest(method, fullURL, nil)
		if err != nil {
			return nil, err
		}
	}
	req.Header.Set("Token", token)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var res map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
				return res, nil
			}
			return nil, fmt.Errorf("json decode error")
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}
	return nil, lastErr
}

func toolSUICreateLocalInbound(args []string) {
	baseURL := os.Getenv("SUI_API")
	token := os.Getenv("SUI_TOKEN")
	nodePort, _ := strconv.Atoi(os.Getenv("NODE_PORT"))
	wsPath := os.Getenv("WS_PATH")
	adminUser := os.Getenv("SUI_ADMIN_USER")
	if adminUser == "" {
		adminUser = "admin"
	}
	domain := os.Getenv("DOMAIN")
	if baseURL == "" || nodePort <= 0 {
		return
	}

	inbResp, _ := suiAPICall(baseURL, token, "GET", "inbounds", nil)
	var inbRows []map[string]any
	if inbResp != nil {
		if obj, ok := inbResp["obj"].(map[string]any); ok {
			if list, ok := obj["inbounds"].([]any); ok {
				for _, r := range list {
					if m, ok := r.(map[string]any); ok {
						inbRows = append(inbRows, m)
					}
				}
			}
		}
	}

	targetTag := fmt.Sprintf("vlws-%s", genRandSuffix())
	var targetID any
	for _, row := range inbRows {
		t, _ := row["tag"].(string)
		if strings.HasPrefix(t, "vlws-") && len(t) == 9 {
			targetTag = t
			targetID = row["id"]
			break
		}
	}

	cleanWSPath := "/" + strings.Trim(wsPath, "/")
	payload := map[string]any{
		"id":          0,
		"type":        "vless",
		"tag":         targetTag,
		"tls_id":      0,
		"listen":      "127.0.0.1",
		"listen_port": nodePort,
		"transport": map[string]any{
			"type": "ws",
			"path": cleanWSPath,
		},
		"addrs": []any{
			map[string]any{
				"server":      domain,
				"server_port": 443,
				"tls": map[string]any{
					"enabled":     true,
					"server_name": domain,
				},
			},
		},
	}
	action := "new"
	if targetID != nil {
		payload["id"] = targetID
		action = "edit"
	}

	payBytes, _ := json.Marshal(payload)
	_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
		"object": {"inbounds"},
		"action": {action},
		"data":   {string(payBytes)},
	})
	time.Sleep(300 * time.Millisecond)

	inbResp2, _ := suiAPICall(baseURL, token, "GET", "inbounds", nil)
	var createdID any
	if inbResp2 != nil {
		if obj, ok := inbResp2["obj"].(map[string]any); ok {
			if list, ok := obj["inbounds"].([]any); ok {
				for _, r := range list {
					if m, ok := r.(map[string]any); ok {
						if m["tag"] == targetTag {
							createdID = m["id"]
							break
						}
					}
				}
			}
		}
	}

	cliResp, _ := suiAPICall(baseURL, token, "GET", "clients", nil)
	var clients []map[string]any
	if cliResp != nil {
		if obj, ok := cliResp["obj"].(map[string]any); ok {
			if list, ok := obj["clients"].([]any); ok {
				for _, r := range list {
					if m, ok := r.(map[string]any); ok {
						clients = append(clients, m)
					}
				}
			}
		}
	}

	var targetClient map[string]any
	for _, c := range clients {
		if c["name"] == adminUser {
			targetClient = c
			break
		}
	}
	if targetClient == nil && len(clients) > 0 {
		targetClient = clients[0]
	}

	if targetClient != nil && createdID != nil {
		inbs, _ := targetClient["inbounds"].([]any)
		hasID := false
		for _, idVal := range inbs {
			if idVal == createdID {
				hasID = true
				break
			}
		}
		if !hasID {
			targetClient["inbounds"] = append(inbs, createdID)
			cliBytes, _ := json.Marshal(targetClient)
			_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
				"object": {"clients"},
				"action": {"edit"},
				"data":   {string(cliBytes)},
			})
		}
	}
	fmt.Printf("  [✓] 成功创建并上线本地 127.0.0.1 隧道节点: %s (端口: %d, 路径: %s)\n", targetTag, nodePort, cleanWSPath)
}

func genRandHex(n int) string {
	b := make([]byte, n)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

func genUUID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func toolSUIApplyTuicHy2(args []string) {
	baseURL := os.Getenv("SUI_API")
	token := os.Getenv("SUI_TOKEN")
	certDomain := os.Getenv("CERT_DOMAIN")
	certFile := os.Getenv("CERT_FILE")
	keyFile := os.Getenv("KEY_FILE")
	isInsecure := (strings.ToLower(os.Getenv("IS_INSECURE")) == "true")
	actionMode := os.Getenv("ACTION_MODE")
	if actionMode == "" {
		actionMode = "create_both"
	}
	adminName := os.Getenv("SUI_ADMIN_USER")
	if adminName == "" {
		adminName = "admin"
	}
	pubIP := os.Getenv("PUBLIC_IP")
	if pubIP == "" {
		pubIP = certDomain
	}
	cc := os.Getenv("CONGESTION_CONTROL")
	if cc == "" {
		cc = "bbr"
	}
	tuicPort, _ := strconv.Atoi(os.Getenv("TUIC_PORT"))
	hy2Port, _ := strconv.Atoi(os.Getenv("HY2_PORT"))

	if baseURL == "" || certDomain == "" {
		return
	}

	tlsResp, _ := suiAPICall(baseURL, token, "GET", "tls", nil)
	var tlsList []map[string]any
	if tlsResp != nil {
		if obj, ok := tlsResp["obj"].(map[string]any); ok {
			if list, ok := obj["tls"].([]any); ok {
				for _, t := range list {
					if m, ok := t.(map[string]any); ok {
						tlsList = append(tlsList, m)
					}
				}
			}
		}
	}

	var matchTLS map[string]any
	for _, t := range tlsList {
		if t["name"] == certDomain {
			matchTLS = t
			break
		}
		if srv, ok := t["server"].(map[string]any); ok {
			if srv["server_name"] == certDomain {
				matchTLS = t
				break
			}
		}
	}

	var newTLSID any
	if matchTLS != nil {
		newTLSID = matchTLS["id"]
		tlsPayload := map[string]any{
			"id":   newTLSID,
			"name": certDomain,
			"server": map[string]any{
				"enabled":          true,
				"server_name":      certDomain,
				"alpn":             []string{"h3"},
				"certificate_path": certFile,
				"key_path":         keyFile,
			},
		}
		payBytes, _ := json.Marshal(tlsPayload)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"tls"},
			"action": {"edit"},
			"data":   {string(payBytes)},
		})
	} else {
		tlsPayload := map[string]any{
			"id":   0,
			"name": certDomain,
			"server": map[string]any{
				"enabled":          true,
				"server_name":      certDomain,
				"alpn":             []string{"h3"},
				"certificate_path": certFile,
				"key_path":         keyFile,
			},
		}
		payBytes, _ := json.Marshal(tlsPayload)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"tls"},
			"action": {"new"},
			"data":   {string(payBytes)},
		})
		time.Sleep(300 * time.Millisecond)

		tlsResp2, _ := suiAPICall(baseURL, token, "GET", "tls", nil)
		if tlsResp2 != nil {
			if obj, ok := tlsResp2["obj"].(map[string]any); ok {
				if list, ok := obj["tls"].([]any); ok {
					for _, t := range list {
						if m, ok := t.(map[string]any); ok {
							if m["name"] == certDomain {
								newTLSID = m["id"]
								break
							}
						}
					}
				}
			}
		}
	}

	inbResp, _ := suiAPICall(baseURL, token, "GET", "inbounds", nil)
	var inbRows []map[string]any
	if inbResp != nil {
		if obj, ok := inbResp["obj"].(map[string]any); ok {
			if list, ok := obj["inbounds"].([]any); ok {
				for _, r := range list {
					if m, ok := r.(map[string]any); ok {
						inbRows = append(inbRows, m)
					}
				}
			}
		}
	}

	var existingTuic, existingHy2 map[string]any
	for _, r := range inbRows {
		tp, _ := r["type"].(string)
		if tp == "tuic" && existingTuic == nil {
			existingTuic = r
		} else if tp == "hysteria2" && existingHy2 == nil {
			existingHy2 = r
		}
	}

	tuicAddrs := []any{
		map[string]any{
			"server":      pubIP,
			"server_port": tuicPort,
			"tls": map[string]any{
				"enabled":     true,
				"insecure":    isInsecure,
				"server_name": certDomain,
			},
		},
	}
	if existingTuic != nil {
		if lp, ok := existingTuic["listen_port"].(float64); ok && lp > 0 {
			tuicPort = int(lp)
			tuicAddrs[0].(map[string]any)["server_port"] = tuicPort
		}
		if actionMode == "change_existing_only" || actionMode == "change_and_fill" {
			tuicT, _ := existingTuic["tag"].(string)
			if !strings.HasPrefix(tuicT, "tuic-") || len(tuicT) != 9 {
				tuicT = fmt.Sprintf("tuic-%s", genRandSuffix())
			}
			tuicPayload := map[string]any{
				"id":                 existingTuic["id"],
				"type":               "tuic",
				"tag":                tuicT,
				"tls_id":             newTLSID,
				"listen":             "::",
				"listen_port":        tuicPort,
				"congestion_control": cc,
				"addrs":              tuicAddrs,
			}
			payBytes, _ := json.Marshal(tuicPayload)
			_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
				"object": {"inbounds"},
				"action": {"edit"},
				"data":   {string(payBytes)},
			})
			fmt.Printf("  [✓] TUIC 节点 [%s] 已成功更新 (TLS_ID: %v, 拥塞控制: %s, 多域名: %s:%d)\n", tuicT, newTLSID, cc, pubIP, tuicPort)
		}
	} else if actionMode == "create_both" || actionMode == "change_and_fill" {
		tuicTag := fmt.Sprintf("tuic-%s", genRandSuffix())
		tuicPayload := map[string]any{
			"id":                 0,
			"type":               "tuic",
			"tag":                tuicTag,
			"tls_id":             newTLSID,
			"listen":             "::",
			"listen_port":        tuicPort,
			"congestion_control": cc,
			"addrs":              tuicAddrs,
		}
		payBytes, _ := json.Marshal(tuicPayload)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"inbounds"},
			"action": {"new"},
			"data":   {string(payBytes)},
		})
		fmt.Printf("  [✓] 已成功创建并上线 TUIC 节点: tag=%s, 端口=%d, 拥塞控制=%s, 多域名=%s:%d\n", tuicTag, tuicPort, cc, pubIP, tuicPort)
	}
	time.Sleep(300 * time.Millisecond)

	hy2Addrs := []any{
		map[string]any{
			"server":      pubIP,
			"server_port": hy2Port,
			"tls": map[string]any{
				"enabled":     true,
				"insecure":    isInsecure,
				"server_name": certDomain,
			},
		},
	}
	if existingHy2 != nil {
		if lp, ok := existingHy2["listen_port"].(float64); ok && lp > 0 {
			hy2Port = int(lp)
			hy2Addrs[0].(map[string]any)["server_port"] = hy2Port
		}
		if actionMode == "change_existing_only" || actionMode == "change_and_fill" {
			hy2T, _ := existingHy2["tag"].(string)
			if !strings.HasPrefix(hy2T, "hysteria2-") || len(hy2T) != 14 {
				hy2T = fmt.Sprintf("hysteria2-%s", genRandSuffix())
			}
			hy2Payload := map[string]any{
				"id":                      existingHy2["id"],
				"type":                    "hysteria2",
				"tag":                     hy2T,
				"tls_id":                  newTLSID,
				"listen":                  "::",
				"listen_port":             hy2Port,
				"ignore_client_bandwidth": true,
				"addrs":                   hy2Addrs,
			}
			payBytes, _ := json.Marshal(hy2Payload)
			_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
				"object": {"inbounds"},
				"action": {"edit"},
				"data":   {string(payBytes)},
			})
			fmt.Printf("  [✓] Hysteria2 节点 [%s] 已成功更新 (TLS_ID: %v, 忽略客户端带宽: 开启, 多域名: %s:%d)\n", hy2T, newTLSID, pubIP, hy2Port)
		}
	} else if actionMode == "create_both" || actionMode == "change_and_fill" {
		hy2Tag := fmt.Sprintf("hysteria2-%s", genRandSuffix())
		hy2Payload := map[string]any{
			"id":                      0,
			"type":                    "hysteria2",
			"tag":                     hy2Tag,
			"tls_id":                  newTLSID,
			"listen":                  "::",
			"listen_port":             hy2Port,
			"ignore_client_bandwidth": true,
			"addrs":                   hy2Addrs,
		}
		payBytes, _ := json.Marshal(hy2Payload)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"inbounds"},
			"action": {"new"},
			"data":   {string(payBytes)},
		})
		fmt.Printf("  [✓] 已成功创建并上线 Hysteria2 节点: tag=%s, 端口=%d, 忽略客户端带宽=开启, 多域名=%s:%d\n", hy2Tag, hy2Port, pubIP, hy2Port)
	}
	time.Sleep(300 * time.Millisecond)

	inbRespLatest, _ := suiAPICall(baseURL, token, "GET", "inbounds", nil)
	var allInbIDs []any
	if inbRespLatest != nil {
		if obj, ok := inbRespLatest["obj"].(map[string]any); ok {
			if list, ok := obj["inbounds"].([]any); ok {
				for _, r := range list {
					if m, ok := r.(map[string]any); ok {
						if idVal, ok := m["id"]; ok && idVal != nil {
							allInbIDs = append(allInbIDs, idVal)
						}
					}
				}
			}
		}
	}

	cliResp, _ := suiAPICall(baseURL, token, "GET", "clients", nil)
	var clients []map[string]any
	if cliResp != nil {
		if obj, ok := cliResp["obj"].(map[string]any); ok {
			if list, ok := obj["clients"].([]any); ok {
				for _, r := range list {
					if m, ok := r.(map[string]any); ok {
						clients = append(clients, m)
					}
				}
			}
		}
	}

	var targetClient map[string]any
	for _, c := range clients {
		if c["id"] == float64(1) || c["id"] == int64(1) || c["id"] == 1 {
			targetClient = c
			break
		}
	}
	if targetClient == nil {
		for _, c := range clients {
			if c["name"] == adminName {
				targetClient = c
				break
			}
		}
	}
	if targetClient == nil && len(clients) > 0 {
		targetClient = clients[0]
	}

	userName := adminName
	var userID any = 0
	isNewUser := true
	if targetClient != nil {
		if n, ok := targetClient["name"].(string); ok && n != "" {
			userName = n
		}
		userID = targetClient["id"]
		isNewUser = false
	}

	var existingUserInbs []any
	if targetClient != nil {
		existingUserInbs, _ = targetClient["inbounds"].([]any)
	}
	inbIDMap := make(map[any]bool)
	for _, idVal := range existingUserInbs {
		inbIDMap[idVal] = true
	}
	for _, idVal := range allInbIDs {
		inbIDMap[idVal] = true
	}
	var mergedInbs []any
	for idVal := range inbIDMap {
		mergedInbs = append(mergedInbs, idVal)
	}

	clientCfg := make(map[string]any)
	if targetClient != nil {
		if m, ok := targetClient["config"].(map[string]any); ok {
			clientCfg = m
		}
	}
	clientPass := genRandHex(8)
	clientUUID := genUUID()

	if _, ok := clientCfg["vless"]; !ok {
		clientCfg["vless"] = map[string]any{"name": userName, "uuid": clientUUID, "flow": "xtls-rprx-vision"}
	}
	if _, ok := clientCfg["vmess"]; !ok {
		clientCfg["vmess"] = map[string]any{"name": userName, "uuid": clientUUID}
	}
	if _, ok := clientCfg["tuic"]; !ok {
		clientCfg["tuic"] = map[string]any{"name": userName, "uuid": genUUID(), "password": clientPass}
	}
	if _, ok := clientCfg["hysteria2"]; !ok {
		clientCfg["hysteria2"] = map[string]any{"name": userName, "password": clientPass}
	}

	clientPayload := map[string]any{
		"id":         userID,
		"enable":     true,
		"name":       userName,
		"remark":     "默认用户",
		"config":     clientCfg,
		"inbounds":   mergedInbs,
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
	if targetClient != nil {
		for _, k := range []string{"remark", "links", "volume", "expiry", "down", "up", "desc", "group", "delayStart", "autoReset", "resetDays", "nextReset", "totalUp", "totalDown", "createdAt", "onlineAt"} {
			if v, ok := targetClient[k]; ok {
				clientPayload[k] = v
			}
		}
	}

	cliBytes, _ := json.Marshal(clientPayload)
	cliAct := "edit"
	if isNewUser {
		cliAct = "new"
	}
	_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
		"object": {"clients"},
		"action": {cliAct},
		"data":   {string(cliBytes)},
	})
	fmt.Printf("  [✓] 主用户 [%s] 已成功关联绑定全部入站节点。\n", userName)
}

func toolSUIInitBaseNodes(args []string) {
	hasSUI := (os.Getenv("HAS_SUI") == "true") && os.Getenv("SUI_TOKEN") != ""
	domain := os.Getenv("DOMAIN")
	publicIP := os.Getenv("PUBLIC_IP")
	nodePort, _ := strconv.Atoi(os.Getenv("NODE_PORT"))
	realityPort, _ := strconv.Atoi(os.Getenv("REALITY_PORT"))
	wsPath := os.Getenv("WS_PATH")
	adminUser := os.Getenv("SUI_ADMIN_USER")
	if adminUser == "" {
		adminUser = "admin"
	}
	tunnelToken := strings.TrimSpace(os.Getenv("TUNNEL_TOKEN"))

	if nodePort <= 0 {
		nodePort = 2082
	}
	if realityPort <= 0 {
		realityPort = 2083
	}
	cleanWS := "/" + strings.Trim(wsPath, "/")

	if hasSUI {
		baseURL := os.Getenv("SUI_API")
		token := os.Getenv("SUI_TOKEN")

		// 1. 设置 settings 避免监听限制
		settingsData := map[string]any{
			"webListen":   "127.0.0.1",
			"subListen":   "127.0.0.1",
			"webCertFile": "",
			"webKeyFile":  "",
			"subCertFile": "",
			"subKeyFile":  "",
		}
		sBytes, _ := json.Marshal(settingsData)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"settings"},
			"action": {"set"},
			"data":   {string(sBytes)},
		})
		time.Sleep(200 * time.Millisecond)

		// 2. 检查现有 inbounds
		inbResp, _ := suiAPICall(baseURL, token, "GET", "inbounds", nil)
		var inbRows []map[string]any
		if inbResp != nil {
			if obj, ok := inbResp["obj"].(map[string]any); ok {
				if list, ok := obj["inbounds"].([]any); ok {
					for _, r := range list {
						if m, ok := r.(map[string]any); ok {
							inbRows = append(inbRows, m)
						}
					}
				}
			}
		}

		findTag := func(prefix string) (string, any) {
			for _, r := range inbRows {
				t, _ := r["tag"].(string)
				if strings.HasPrefix(t, prefix+"-") && len(t) == len(prefix)+5 {
					return t, r["id"]
				}
			}
			return fmt.Sprintf("%s-%s", prefix, genRandSuffix()), nil
		}

		// 创建/编辑 vless-argo
		vlwsTag, vlwsID := findTag("vlws")
		vlwsPayload := map[string]any{
			"id":          0,
			"type":        "vless",
			"tag":         vlwsTag,
			"tls_id":      0,
			"listen":      "127.0.0.1",
			"listen_port": nodePort,
			"transport": map[string]any{
				"type": "ws",
				"path": cleanWS,
			},
			"addrs": []any{
				map[string]any{
					"server":      domain,
					"server_port": 443,
					"tls": map[string]any{
						"enabled":     true,
						"server_name": domain,
					},
				},
			},
		}
		vlwsAct := "new"
		if vlwsID != nil {
			vlwsPayload["id"] = vlwsID
			vlwsAct = "edit"
		}
		pay1, _ := json.Marshal(vlwsPayload)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"inbounds"},
			"action": {vlwsAct},
			"data":   {string(pay1)},
		})

		// 创建/编辑 reality
		tlsResp, _ := suiAPICall(baseURL, token, "GET", "tls", nil)
		var realityTID any = 0
		if tlsResp != nil {
			if obj, ok := tlsResp["obj"].(map[string]any); ok {
				if list, ok := obj["tls"].([]any); ok {
					for _, t := range list {
						if m, ok := t.(map[string]any); ok {
							if m["name"] == "reality" {
								realityTID = m["id"]
								break
							}
						}
					}
				}
			}
		}

		privK := genRandHex(16)
		pubK := genRandHex(16)
		sid := genRandHex(4)
		if realityTID == nil || realityTID == 0 {
			realityTLSPayload := map[string]any{
				"id":   0,
				"name": "reality",
				"server": map[string]any{
					"enabled":     true,
					"server_name": "apple.com",
					"reality": map[string]any{
						"enabled": true,
						"handshake": map[string]any{
							"server_port": 443,
							"server":      "apple.com",
						},
						"short_id":    []string{sid},
						"private_key": privK,
					},
				},
				"client": map[string]any{
					"reality": map[string]any{
						"public_key": pubK,
						"short_id":   sid,
					},
					"utls": map[string]any{
						"enabled":     true,
						"fingerprint": "chrome",
					},
				},
			}
			payT, _ := json.Marshal(realityTLSPayload)
			_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
				"object": {"tls"},
				"action": {"new"},
				"data":   {string(payT)},
			})
			time.Sleep(200 * time.Millisecond)
		}

		vlreTag, vlreID := findTag("vlre")
		realityAddrs := []any{
			map[string]any{
				"server":      publicIP,
				"server_port": realityPort,
			},
		}
		vlrePayload := map[string]any{
			"id":          0,
			"type":        "vless",
			"tag":         vlreTag,
			"tls_id":      realityTID,
			"listen":      "::",
			"listen_port": realityPort,
			"addrs":       realityAddrs,
		}
		vlreAct := "new"
		if vlreID != nil {
			vlrePayload["id"] = vlreID
			vlreAct = "edit"
		}
		pay2, _ := json.Marshal(vlrePayload)
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"inbounds"},
			"action": {vlreAct},
			"data":   {string(pay2)},
		})

		// cf-tunnel-in 原生入站
		if tunnelToken != "" {
			cfPayload := map[string]any{
				"id":     0,
				"type":   "cloudflared",
				"tag":    "cf-tunnel-in",
				"token":  tunnelToken,
				"listen": "127.0.0.1",
			}
			payCF, _ := json.Marshal(cfPayload)
			_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
				"object": {"inbounds"},
				"action": {"new"},
				"data":   {string(payCF)},
			})
		}

		// 关联主用户
		inbRespLatest, _ := suiAPICall(baseURL, token, "GET", "inbounds", nil)
		var allInbIDs []any
		if inbRespLatest != nil {
			if obj, ok := inbRespLatest["obj"].(map[string]any); ok {
				if list, ok := obj["inbounds"].([]any); ok {
					for _, r := range list {
						if m, ok := r.(map[string]any); ok {
							if idVal, ok := m["id"]; ok && idVal != nil {
								allInbIDs = append(allInbIDs, idVal)
							}
						}
					}
				}
			}
		}

		cliResp, _ := suiAPICall(baseURL, token, "GET", "clients", nil)
		var clients []map[string]any
		if cliResp != nil {
			if obj, ok := cliResp["obj"].(map[string]any); ok {
				if list, ok := obj["clients"].([]any); ok {
					for _, r := range list {
						if m, ok := r.(map[string]any); ok {
							clients = append(clients, m)
						}
					}
				}
			}
		}

		var targetClient map[string]any
		for _, c := range clients {
			if c["name"] == adminUser {
				targetClient = c
				break
			}
		}
		if targetClient == nil && len(clients) > 0 {
			targetClient = clients[0]
		}

		userName := adminUser
		var userID any = 0
		isNewUser := true
		if targetClient != nil {
			if n, ok := targetClient["name"].(string); ok && n != "" {
				userName = n
			}
			userID = targetClient["id"]
			isNewUser = false
		}

		clientCfg := make(map[string]any)
		if targetClient != nil {
			if m, ok := targetClient["config"].(map[string]any); ok {
				clientCfg = m
			}
		}
		cUUID := genUUID()
		cPass := genRandHex(8)
		if _, ok := clientCfg["vless"]; !ok {
			clientCfg["vless"] = map[string]any{"name": userName, "uuid": cUUID, "flow": "xtls-rprx-vision"}
		}
		if _, ok := clientCfg["vmess"]; !ok {
			clientCfg["vmess"] = map[string]any{"name": userName, "uuid": cUUID}
		}
		if _, ok := clientCfg["tuic"]; !ok {
			clientCfg["tuic"] = map[string]any{"name": userName, "uuid": genUUID(), "password": cPass}
		}
		if _, ok := clientCfg["hysteria2"]; !ok {
			clientCfg["hysteria2"] = map[string]any{"name": userName, "password": cPass}
		}

		clientPayload := map[string]any{
			"id":       userID,
			"enable":   true,
			"name":     userName,
			"remark":   "默认用户",
			"config":   clientCfg,
			"inbounds": allInbIDs,
		}
		cliBytes, _ := json.Marshal(clientPayload)
		cliAct := "edit"
		if isNewUser {
			cliAct = "new"
		}
		_, _ = suiAPICall(baseURL, token, "POST", "save", url.Values{
			"object": {"clients"},
			"action": {cliAct},
			"data":   {string(cliBytes)},
		})
	} else {
		// 纯 sing-box 内核模式
		sbConf := "/etc/sing-box/config.json"
		var conf map[string]any
		if data, err := os.ReadFile(sbConf); err == nil {
			_ = json.Unmarshal(data, &conf)
		}
		if conf == nil {
			conf = make(map[string]any)
		}
		inbounds, _ := conf["inbounds"].([]any)
		cUUID := genUUID()
		newInbs := []any{}
		for _, raw := range inbounds {
			if m, ok := raw.(map[string]any); ok {
				t, _ := m["tag"].(string)
				if strings.HasPrefix(t, "vlws-") || strings.HasPrefix(t, "vlre-") || t == "cf-tunnel-in" {
					continue
				}
				newInbs = append(newInbs, m)
			}
		}
		newInbs = append(newInbs, map[string]any{
			"type":        "vless",
			"tag":         fmt.Sprintf("vlws-%s", genRandSuffix()),
			"listen":      "127.0.0.1",
			"listen_port": nodePort,
			"users": []any{
				map[string]any{"name": adminUser, "uuid": cUUID},
			},
			"transport": map[string]any{"type": "ws", "path": cleanWS},
		})
		newInbs = append(newInbs, map[string]any{
			"type":        "vless",
			"tag":         fmt.Sprintf("vlre-%s", genRandSuffix()),
			"listen":      "::",
			"listen_port": realityPort,
			"users": []any{
				map[string]any{"name": adminUser, "uuid": cUUID, "flow": "xtls-rprx-vision"},
			},
			"tls": map[string]any{
				"enabled":     true,
				"server_name": "apple.com",
				"reality": map[string]any{
					"enabled":   true,
					"handshake": map[string]any{"server": "apple.com", "server_port": 443},
				},
			},
		})
		if tunnelToken != "" {
			newInbs = append(newInbs, map[string]any{
				"type":   "cloudflared",
				"tag":    "cf-tunnel-in",
				"token":  tunnelToken,
				"listen": "127.0.0.1",
			})
		}
		conf["inbounds"] = newInbs
		if out, err := json.MarshalIndent(conf, "", "  "); err == nil {
			_ = os.WriteFile(sbConf, out, 0644)
		}
	}

	metaVars := fmt.Sprintf("node_port=%d\nws_path=%s\nreality_port=%d\n", nodePort, strings.Trim(wsPath, "/"), realityPort)
	_ = os.WriteFile("/var/lib/sout/.caddy_meta_vars", []byte(metaVars), 0600)
	fmt.Println("  [✓] 基础节点与轻量分流已配置就绪！")
}
