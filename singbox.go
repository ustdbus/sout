package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	singboxConfigDefault = "/etc/sing-box/config.json"
	singboxBinaryDefault = "/usr/local/bin/sing-box"
)

// SingBox 结构体，直接管理并接管本机 sing-box 原生内核配置文件
type SingBox struct {
	configPath string
	workDir    string
	mu         sync.Mutex
}

var _ Panel = (*SingBox)(nil)

func (sb *SingBox) Kind() string { return "sing-box" }

func (sb *SingBox) Describe() string {
	return fmt.Sprintf("接管本机 sing-box 原生内核 (%s)", sb.configPath)
}

// DetectSingBox 探测本机是否安装 sing-box 或存在配置文件
func DetectSingBox(workDir string) (*SingBox, error) {
	// 检查常用路径
	candidates := []string{
		singboxConfigDefault,
		"/usr/local/etc/sing-box/config.json",
		filepath.Join(workDir, "sing-box", "config.json"),
		"/etc/sing-box/config.json",
	}

	var foundPath string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			foundPath = p
			break
		}
	}

	// 如果配置文件未找到，但系统已安装 sing-box 命令，则初始化默认配置文件
	if foundPath == "" {
		if hasCmd("sing-box") || fileExists(singboxBinaryDefault) || fileExists("/usr/bin/sing-box") {
			foundPath = singboxConfigDefault
			if err := initDefaultSingBoxConfig(foundPath); err != nil {
				return nil, fmt.Errorf("初始化 sing-box 配置文件失败: %w", err)
			}
		}
	}

	if foundPath == "" {
		return nil, fmt.Errorf("未检测到 sing-box 内核或配置文件")
	}

	return &SingBox{
		configPath: foundPath,
		workDir:    workDir,
	}, nil
}

func initDefaultSingBoxConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	defaultCfg := map[string]any{
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"inbounds": []any{},
		"outbounds": []any{
			map[string]any{
				"type": "direct",
				"tag":  "direct",
			},
			map[string]any{
				"type": "block",
				"tag":  "block",
			},
		},
		"route": map[string]any{
			"rules":                 []any{},
			"final":                 "direct",
			"auto_detect_interface": true,
		},
	}
	data, err := json.MarshalIndent(defaultCfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (sb *SingBox) loadConfig() (map[string]any, error) {
	data, err := os.ReadFile(sb.configPath)
	if err != nil {
		return nil, fmt.Errorf("读取 sing-box 配置失败: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 sing-box 配置 JSON 失败: %w", err)
	}
	return cfg, nil
}

func (sb *SingBox) saveConfig(cfg map[string]any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	// 临时写入以验证
	tmpFile := sb.configPath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}

	// 若系统安装了 sing-box，则进行语法检查
	if hasCmd("sing-box") {
		out, checkErr := exec.Command("sing-box", "check", "-c", tmpFile).CombinedOutput()
		if checkErr != nil {
			_ = os.Remove(tmpFile)
			return fmt.Errorf("sing-box 配置校验未通过: %s (%w)", strings.TrimSpace(string(out)), checkErr)
		}
	}

	if err := os.Rename(tmpFile, sb.configPath); err != nil {
		return fmt.Errorf("替换主配置文件失败: %w", err)
	}

	sb.restartService()
	return nil
}

func (sb *SingBox) restartService() {
	if hasCmd("systemctl") && dirExists("/run/systemd/system") {
		_ = exec.Command("systemctl", "restart", "sing-box").Run()
	} else if hasCmd("rc-service") {
		_ = exec.Command("rc-service", "sing-box", "restart").Run()
	} else if hasCmd("service") {
		_ = exec.Command("service", "sing-box", "restart").Run()
	}
}

// Inbounds 列出当前 sing-box 中的所有入站节点及其派生的分流分支
func (sb *SingBox) Inbounds(live map[string]bool) ([]Inbound, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return nil, err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	routeRaw, _ := cfg["route"].(map[string]any)
	var rulesRaw []any
	if routeRaw != nil {
		rulesRaw, _ = routeRaw["rules"].([]any)
	}

	// 构建 user -> outbound 路由映射表
	userRouteMap := make(map[string]string)
	for _, rAny := range rulesRaw {
		if rMap, ok := rAny.(map[string]any); ok {
			outbound, _ := rMap["outbound"].(string)
			if authUsers, ok := rMap["auth_user"].([]any); ok {
				for _, u := range authUsers {
					if uStr, ok := u.(string); ok {
						userRouteMap[uStr] = outbound
					}
				}
			}
		}
	}

	var result []Inbound
	for idx, ibRaw := range inboundsRaw {
		ibMap, ok := ibRaw.(map[string]any)
		if !ok {
			continue
		}

		baseID := (idx + 1) * 1000
		proto, _ := ibMap["type"].(string)
		tag, _ := ibMap["tag"].(string)
		port := int(getFloat(ibMap["listen_port"]))
		remark := tag
		if remark == "" {
			remark = fmt.Sprintf("%s-%d", proto, port)
		}

		// 主节点 (母节点)
		result = append(result, Inbound{
			ID:       baseID,
			Port:     port,
			Protocol: proto,
			Remark:   remark,
			Enable:   true,
			Tag:      tag,
			IsBase:   true,
		})

		// 扫描派生的分流用户 (Client 分支)
		usersRaw, _ := ibMap["users"].([]any)
		for cIdx, uRaw := range usersRaw {
			uMap, ok := uRaw.(map[string]any)
			if !ok {
				continue
			}
			userName, _ := uMap["name"].(string)
			if strings.HasPrefix(userName, "soutu") {
				outboundTag := userRouteMap[userName]
				boundHost := strings.TrimPrefix(outboundTag, "sout")
				boundUp := false
				if live != nil && boundHost != "" {
					boundUp = live[sanitizeTag(boundHost)]
				}
				branchRemark := fmt.Sprintf("%s [出口: %s]", remark, boundHost)

				result = append(result, Inbound{
					ID:       baseID,
					ClientID: cIdx + 1,
					Port:     port,
					Protocol: proto,
					Remark:   branchRemark,
					Enable:   true,
					Tag:      userName,
					BoundTo:  boundHost,
					BoundUp:  boundUp,
					IsBase:   false,
				})
			}
		}
	}

	return result, nil
}

// InboundDetail 获取指定入站的完整详情与分享链接
func (sb *SingBox) InboundDetail(id int, publicHost string) (*InboundDetail, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return nil, err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	idx := (id / 1000) - 1
	if idx < 0 || idx >= len(inboundsRaw) {
		return nil, fmt.Errorf("找不到对应的节点 (ID: %d)", id)
	}

	ibMap, ok := inboundsRaw[idx].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("节点数据解析失败")
	}

	proto, _ := ibMap["type"].(string)
	tag, _ := ibMap["tag"].(string)
	port := int(getFloat(ibMap["listen_port"]))
	listen, _ := ibMap["listen"].(string)

	var clients []ClientInfo
	usersRaw, _ := ibMap["users"].([]any)
	for _, uRaw := range usersRaw {
		if uMap, ok := uRaw.(map[string]any); ok {
			email, _ := uMap["name"].(string)
			uuidStr, _ := uMap["uuid"].(string)
			if uuidStr == "" {
				uuidStr, _ = uMap["password"].(string)
			}
			clients = append(clients, ClientInfo{
				Email:  email,
				ID:     uuidStr,
				Enable: true,
			})
		}
	}

	tlsStr := "none"
	if tlsMap, ok := ibMap["tls"].(map[string]any); ok {
		if en, _ := tlsMap["enabled"].(bool); en {
			tlsStr = "tls"
		}
	}

	networkStr := "tcp"
	if transportMap, ok := ibMap["transport"].(map[string]any); ok {
		if tp, ok := transportMap["type"].(string); ok {
			networkStr = tp
		}
	} else if proto == "tuic" || proto == "hysteria2" {
		networkStr = "udp"
	}

	links := sb.buildLinksForInbound(ibMap, publicHost)

	detail := &InboundDetail{
		Inbound: Inbound{
			ID:       id,
			Port:     port,
			Protocol: proto,
			Remark:   tag,
			Enable:   true,
			Tag:      tag,
			IsBase:   true,
		},
		Clients: clients,
		Links:   links,
		Listen:  listen,
		Network: networkStr,
		TLS:     tlsStr,
	}

	return detail, nil
}

// InboundLinks 生成指定入站/分支节点的客户端分享链接
func (sb *SingBox) InboundLinks(ids []int, publicHost string) ([]string, error) {
	var allLinks []string
	for _, id := range ids {
		detail, err := sb.InboundDetail(id, publicHost)
		if err == nil && detail != nil {
			allLinks = append(allLinks, detail.Links...)
		}
	}
	return allLinks, nil
}

func (sb *SingBox) buildLinksForInbound(ibMap map[string]any, publicHost string) []string {
	proto, _ := ibMap["type"].(string)
	tag, _ := ibMap["tag"].(string)
	port := int(getFloat(ibMap["listen_port"]))
	if port == 0 {
		return nil
	}

	host := publicHost
	if host == "" {
		host = "127.0.0.1"
	}

	sni := host
	allowInsecure := "0"
	if tlsMap, ok := ibMap["tls"].(map[string]any); ok {
		if sn, ok := tlsMap["server_name"].(string); ok && sn != "" {
			sni = sn
		}
		if insec, _ := tlsMap["insecure"].(bool); insec {
			allowInsecure = "1"
		}
	}

	usersRaw, _ := ibMap["users"].([]any)
	if len(usersRaw) == 0 {
		return nil
	}

	var links []string
	for _, uRaw := range usersRaw {
		uMap, ok := uRaw.(map[string]any)
		if !ok {
			continue
		}
		uName, _ := uMap["name"].(string)
		uuidStr, _ := uMap["uuid"].(string)
		passStr, _ := uMap["password"].(string)

		nodeRemark := tag
		if strings.HasPrefix(uName, "soutu") {
			nodeRemark = fmt.Sprintf("%s-%s", tag, uName)
		}

		switch proto {
		case "tuic":
			authPart := uuidStr
			if passStr != "" {
				authPart += ":" + passStr
			}
			link := fmt.Sprintf("tuic://%s@%s:%d?congestion_control=bbr&alpn=h3&sni=%s&allow_insecure=%s#%s",
				authPart, host, port, url.QueryEscape(sni), allowInsecure, url.QueryEscape(nodeRemark))
			links = append(links, link)

		case "hysteria2":
			authPart := passStr
			if authPart == "" {
				authPart = uuidStr
			}
			link := fmt.Sprintf("hysteria2://%s@%s:%d?sni=%s&insecure=%s#%s",
				authPart, host, port, url.QueryEscape(sni), allowInsecure, url.QueryEscape(nodeRemark))
			links = append(links, link)

		case "vless":
			security := "none"
			if tlsMap, ok := ibMap["tls"].(map[string]any); ok {
				if en, _ := tlsMap["enabled"].(bool); en {
					security = "tls"
				}
			}
			transportType := "tcp"
			wsPath := ""
			wsHost := sni
			if trMap, ok := ibMap["transport"].(map[string]any); ok {
				if tp, ok := trMap["type"].(string); ok {
					transportType = tp
				}
				if pth, ok := trMap["path"].(string); ok {
					wsPath = pth
				}
				if h, ok := trMap["host"].(string); ok && h != "" {
					wsHost = h
				}
			}
			link := fmt.Sprintf("vless://%s@%s:%d?encryption=none&security=%s&sni=%s&type=%s",
				uuidStr, host, port, security, url.QueryEscape(sni), transportType)
			if transportType == "ws" && wsPath != "" {
				link += fmt.Sprintf("&path=%s&host=%s", url.QueryEscape(wsPath), url.QueryEscape(wsHost))
			}
			link += "#" + url.QueryEscape(nodeRemark)
			links = append(links, link)

		case "shadowsocks":
			method, _ := ibMap["method"].(string)
			auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", method, passStr)))
			link := fmt.Sprintf("ss://%s@%s:%d#%s", auth, host, port, url.QueryEscape(nodeRemark))
			links = append(links, link)
		}
	}

	return links
}

// CloneToTunnels 派生分流节点至目标隧道
func (sb *SingBox) CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return nil, err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	idx := (templateID / 1000) - 1
	if idx < 0 || idx >= len(inboundsRaw) {
		return nil, fmt.Errorf("指定的模板节点不存在 (ID: %d)", templateID)
	}

	ibMap, ok := inboundsRaw[idx].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("节点数据无效")
	}

	usersRaw, _ := ibMap["users"].([]any)

	routeRaw, ok := cfg["route"].(map[string]any)
	if !ok {
		routeRaw = map[string]any{"final": "direct", "rules": []any{}}
		cfg["route"] = routeRaw
	}
	rulesRaw, _ := routeRaw["rules"].([]any)

	createdPorts := []int{}

	for _, host := range hosts {
		hTag := sanitizeTag(host)
		clientName := fmt.Sprintf("soutu%d%s", templateID, hTag)

		// 检查是否已存在该客户端
		exists := false
		for _, u := range usersRaw {
			if uMap, ok := u.(map[string]any); ok {
				if uMap["name"] == clientName {
					exists = true
					break
				}
			}
		}

		if !exists {
			newClient := map[string]any{
				"name":     clientName,
				"uuid":     generateUUID(),
				"password": generateRandomPassword(16),
			}
			usersRaw = append(usersRaw, newClient)
		}

		// 添加或更新 route.rules 规则
		outboundTag := "sout" + hTag
		ruleExists := false
		for _, r := range rulesRaw {
			if rMap, ok := r.(map[string]any); ok {
				if rMap["outbound"] == outboundTag {
					ruleExists = true
					break
				}
			}
		}
		if !ruleExists {
			newRule := map[string]any{
				"auth_user": []any{clientName},
				"outbound":  outboundTag,
			}
			rulesRaw = append([]any{newRule}, rulesRaw...)
		}

		createdPorts = append(createdPorts, int(getFloat(ibMap["listen_port"])))
	}

	ibMap["users"] = usersRaw
	routeRaw["rules"] = rulesRaw

	// 同步当前隧道的 SOCKS5 出站
	sb.syncOutboundsInternal(cfg, tunnels)

	if err := sb.saveConfig(cfg); err != nil {
		return nil, err
	}

	invalidateInbounds()
	return createdPorts, nil
}

// Bind 绑定入站 tag 到指定 host
func (sb *SingBox) Bind(inboundTag string, hostname string, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	routeRaw, ok := cfg["route"].(map[string]any)
	if !ok {
		return fmt.Errorf("路由配置缺失")
	}
	rulesRaw, _ := routeRaw["rules"].([]any)

	outboundTag := "sout" + sanitizeTag(hostname)
	found := false
	for _, r := range rulesRaw {
		if rMap, ok := r.(map[string]any); ok {
			if authUsers, ok := rMap["auth_user"].([]any); ok {
				for _, u := range authUsers {
					if u == inboundTag {
						rMap["outbound"] = outboundTag
						found = true
						break
					}
				}
			}
		}
	}

	if !found {
		newRule := map[string]any{
			"auth_user": []any{inboundTag},
			"outbound":  outboundTag,
		}
		rulesRaw = append([]any{newRule}, rulesRaw...)
		routeRaw["rules"] = rulesRaw
	}

	sb.syncOutboundsInternal(cfg, tunnels)
	return sb.saveConfig(cfg)
}

// Rebind 重绑定旧主机到新隧道
func (sb *SingBox) Rebind(oldHost string, target *Tunnel, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	oldTag := "sout" + sanitizeTag(oldHost)
	newTag := "sout" + sanitizeTag(target.Node.HostName)

	routeRaw, _ := cfg["route"].(map[string]any)
	if routeRaw != nil {
		rulesRaw, _ := routeRaw["rules"].([]any)
		for _, r := range rulesRaw {
			if rMap, ok := r.(map[string]any); ok {
				if rMap["outbound"] == oldTag {
					rMap["outbound"] = newTag
				}
			}
		}
	}

	sb.syncOutboundsInternal(cfg, tunnels)
	return sb.saveConfig(cfg)
}

// ResyncOutbound 重新同步特定隧道出站
func (sb *SingBox) ResyncOutbound(t *Tunnel, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	sb.syncOutboundsInternal(cfg, tunnels)
	return sb.saveConfig(cfg)
}

func (sb *SingBox) syncOutboundsInternal(cfg map[string]any, tunnels []*Tunnel) {
	outboundsRaw, _ := cfg["outbounds"].([]any)
	var newOutbounds []any

	// 保留非 sout 相关的基础出站 (如 direct, block, dns 等)
	for _, o := range outboundsRaw {
		if oMap, ok := o.(map[string]any); ok {
			tag, _ := oMap["tag"].(string)
			if !strings.HasPrefix(tag, "sout") {
				newOutbounds = append(newOutbounds, o)
			}
		}
	}

	// 针对每一个处于 up 状态的隧道增加 SOCKS5 出站
	for _, t := range tunnels {
		if t.Status != "up" {
			continue
		}
		cred := t.credential()
		obTag := "sout" + sanitizeTag(t.Node.HostName)
		ob := map[string]any{
			"type":        "socks",
			"tag":         obTag,
			"server":      "127.0.0.1",
			"server_port": t.Port,
			"version":     "5",
		}
		if cred.User != "" && cred.Pass != "" {
			ob["username"] = cred.User
			ob["password"] = cred.Pass
		}
		newOutbounds = append(newOutbounds, ob)
	}

	cfg["outbounds"] = newOutbounds
}

// DeleteInbounds 删除指定入站或其派生的 Client
func (sb *SingBox) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	routeRaw, _ := cfg["route"].(map[string]any)

	idSet := make(map[int]bool)
	for _, id := range ids {
		idSet[id] = true
	}

	var remainingInbounds []any
	for idx, ib := range inboundsRaw {
		baseID := (idx + 1) * 1000
		if idSet[baseID] {
			// 删除该整条入站及其规则
			continue
		}
		remainingInbounds = append(remainingInbounds, ib)
	}
	cfg["inbounds"] = remainingInbounds

	// 清理 route.rules 中悬空的 sout 出站规则
	if routeRaw != nil {
		rulesRaw, _ := routeRaw["rules"].([]any)
		var validRules []any
		for _, r := range rulesRaw {
			if rMap, ok := r.(map[string]any); ok {
				outbound, _ := rMap["outbound"].(string)
				if strings.HasPrefix(outbound, "sout") {
					authUsers, _ := rMap["auth_user"].([]any)
					if len(authUsers) > 0 {
						validRules = append(validRules, r)
					}
				} else {
					validRules = append(validRules, r)
				}
			}
		}
		routeRaw["rules"] = validRules
	}

	sb.syncOutboundsInternal(cfg, tunnels)
	return sb.saveConfig(cfg)
}

// DeleteBranchesByHost 删除绑定到指定主机的分流分支
func (sb *SingBox) DeleteBranchesByHost(host string, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	targetOutbound := "sout" + sanitizeTag(host)

	// 1. 查找所有指向该 outbound 的 auth_user
	targetUsers := make(map[string]bool)
	routeRaw, _ := cfg["route"].(map[string]any)
	if routeRaw != nil {
		rulesRaw, _ := routeRaw["rules"].([]any)
		var keptRules []any
		for _, r := range rulesRaw {
			if rMap, ok := r.(map[string]any); ok {
				if rMap["outbound"] == targetOutbound {
					if authUsers, ok := rMap["auth_user"].([]any); ok {
						for _, u := range authUsers {
							if uStr, ok := u.(string); ok {
								targetUsers[uStr] = true
							}
						}
					}
				} else {
					keptRules = append(keptRules, r)
				}
			}
		}
		routeRaw["rules"] = keptRules
	}

	// 2. 从所有 inbounds 的 users 列表中移除这些分流用户
	inboundsRaw, _ := cfg["inbounds"].([]any)
	for _, ib := range inboundsRaw {
		if ibMap, ok := ib.(map[string]any); ok {
			usersRaw, _ := ibMap["users"].([]any)
			var keptUsers []any
			for _, u := range usersRaw {
				if uMap, ok := u.(map[string]any); ok {
					uName, _ := uMap["name"].(string)
					if !targetUsers[uName] {
						keptUsers = append(keptUsers, u)
					}
				}
			}
			ibMap["users"] = keptUsers
		}
	}

	sb.syncOutboundsInternal(cfg, tunnels)
	return sb.saveConfig(cfg)
}

// CreateInbound 创建新的入站节点
func (sb *SingBox) CreateInbound(spec NewInboundSpec, tunnels []*Tunnel) (*CreatedInbound, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return nil, err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)

	port := spec.Port
	if port == 0 {
		port = 10000 + len(inboundsRaw) + 1
	}

	tag := spec.Remark
	if tag == "" {
		tag = fmt.Sprintf("%s-%d", spec.Protocol, port)
	}

	newIb := map[string]any{
		"type":        spec.Protocol,
		"tag":         tag,
		"listen":      "::",
		"listen_port": port,
	}

	defaultUser := map[string]any{
		"name":     "default",
		"uuid":     generateUUID(),
		"password": generateRandomPassword(16),
	}
	newIb["users"] = []any{defaultUser}

	if spec.Security == "tls" || spec.Security == "reality" {
		tlsMap := map[string]any{
			"enabled": true,
		}
		if spec.ServerName != "" {
			tlsMap["server_name"] = spec.ServerName
		}
		if spec.CertFile != "" {
			tlsMap["certificate_path"] = spec.CertFile
		}
		if spec.KeyFile != "" {
			tlsMap["key_path"] = spec.KeyFile
		}
		newIb["tls"] = tlsMap
	}

	if spec.Network != "" && spec.Network != "tcp" {
		newIb["transport"] = map[string]any{
			"type": spec.Network,
			"path": spec.Path,
		}
	}

	cfg["inbounds"] = append(inboundsRaw, newIb)
	if err := sb.saveConfig(cfg); err != nil {
		return nil, err
	}

	newID := len(inboundsRaw) * 1000
	return &CreatedInbound{
		ID:       newID,
		Port:     port,
		Protocol: spec.Protocol,
		Remark:   tag,
		Network:  spec.Network,
		Security: spec.Security,
	}, nil
}

func (sb *SingBox) UpdateInbound(id int, patch InboundPatch, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	idx := (id / 1000) - 1
	if idx < 0 || idx >= len(inboundsRaw) {
		return fmt.Errorf("节点不存在")
	}

	ibMap, ok := inboundsRaw[idx].(map[string]any)
	if !ok {
		return fmt.Errorf("节点配置无效")
	}

	if patch.Port != nil && *patch.Port > 0 {
		ibMap["listen_port"] = *patch.Port
	}
	if patch.Remark != nil && *patch.Remark != "" {
		ibMap["tag"] = *patch.Remark
	}

	return sb.saveConfig(cfg)
}

func (sb *SingBox) NodeDetail(id int) (*NodeDetailInfo, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return nil, err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	idx := (id / 1000) - 1
	if idx < 0 || idx >= len(inboundsRaw) {
		return nil, fmt.Errorf("节点不存在")
	}

	ibMap, _ := inboundsRaw[idx].(map[string]any)
	proto, _ := ibMap["type"].(string)
	tag, _ := ibMap["tag"].(string)
	port := int(getFloat(ibMap["listen_port"]))
	listen, _ := ibMap["listen"].(string)

	tlsEnabled := false
	sni := ""
	if tlsMap, ok := ibMap["tls"].(map[string]any); ok {
		tlsEnabled, _ = tlsMap["enabled"].(bool)
		sni, _ = tlsMap["server_name"].(string)
	}

	return &NodeDetailInfo{
		ID:           id,
		Name:         tag,
		Protocol:     proto,
		Listen:       listen,
		ListenPort:   port,
		TLSEnabled:   tlsEnabled,
		SNI:          sni,
		ServerHasTLS: tlsEnabled,
		Addrs:        []NodeAddrItem{},
	}, nil
}

func (sb *SingBox) UpdateNodeConfig(id int, listen string, listenPort int, addrs []NodeAddrItem, tlsEnabled bool, sni string, tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	inboundsRaw, _ := cfg["inbounds"].([]any)
	idx := (id / 1000) - 1
	if idx < 0 || idx >= len(inboundsRaw) {
		return fmt.Errorf("节点不存在")
	}

	ibMap, _ := inboundsRaw[idx].(map[string]any)
	if listen != "" {
		ibMap["listen"] = listen
	}
	if listenPort > 0 {
		ibMap["listen_port"] = listenPort
	}
	if tlsEnabled {
		tlsMap, ok := ibMap["tls"].(map[string]any)
		if !ok {
			tlsMap = map[string]any{}
		}
		tlsMap["enabled"] = true
		if sni != "" {
			tlsMap["server_name"] = sni
		}
		ibMap["tls"] = tlsMap
	}

	return sb.saveConfig(cfg)
}

func (sb *SingBox) AddClient(id int, email string, tunnels []*Tunnel) error   { return nil }
func (sb *SingBox) DeleteClient(id int, email string, tunnels []*Tunnel) error { return nil }
func (sb *SingBox) ResetClient(id int, email string, tunnels []*Tunnel) error  { return nil }

func (sb *SingBox) OnTunnelsChanged(tunnels []*Tunnel) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}
	sb.syncOutboundsInternal(cfg, tunnels)
	return sb.saveConfig(cfg)
}

func (sb *SingBox) Close() {}

func generateRandomPassword(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

func getFloat(val any) float64 {
	switch v := val.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	default:
		return 0
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
