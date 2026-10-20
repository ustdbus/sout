package main

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// genX25519Keys 生成 Reality 所需的 X25519 私钥与公钥 (base64 raw url 编码)
func genX25519Keys() (string, string) {
	priv, err := ecdh.X25519().GenerateKey(crand.Reader)
	if err != nil {
		return "", ""
	}
	privStr := base64.RawURLEncoding.EncodeToString(priv.Bytes())
	pubStr := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	return privStr, pubStr
}

// scanLocalACMECerts 扫描 /home/acme/ 下已有的有效证书
func scanLocalACMECerts() []map[string]string {
	var list []map[string]string
	acmeDir := "/home/acme"
	entries, err := os.ReadDir(acmeDir)
	if err != nil {
		return list
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		domName := entry.Name()
		dirPath := filepath.Join(acmeDir, domName)

		var certPath, keyPath string
		certCandidates := []string{
			filepath.Join(dirPath, "fullchain.pem"),
			filepath.Join(dirPath, "cert.crt"),
			filepath.Join(dirPath, domName+".cer"),
		}
		for _, c := range certCandidates {
			if fi, err := os.Stat(c); err == nil && fi.Size() > 0 {
				certPath = c
				break
			}
		}

		keyCandidates := []string{
			filepath.Join(dirPath, "privkey.pem"),
			filepath.Join(dirPath, "private.key"),
			filepath.Join(dirPath, domName+".key"),
		}
		for _, k := range keyCandidates {
			if fi, err := os.Stat(k); err == nil && fi.Size() > 0 {
				keyPath = k
				break
			}
		}

		if certPath != "" && keyPath != "" {
			list = append(list, map[string]string{
				"domain": domName,
				"cert":   certPath,
				"key":    keyPath,
			})
		}
	}
	return list
}

// createSelfSignedCert 生成 10 年期自签 ECC 证书
func createSelfSignedCert(domain string) (string, string, error) {
	if domain == "" {
		domain = "apple.com"
	}
	outDir := filepath.Join("/home/ssc", domain)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", "", fmt.Errorf("创建证书目录失败: %w", err)
	}
	certPath := filepath.Join(outDir, "fullchain.pem")
	keyPath := filepath.Join(outDir, "privkey.pem")

	// 如果已有有效证书，直接复用
	if f1, err := os.Stat(certPath); err == nil && f1.Size() > 0 {
		if f2, err2 := os.Stat(keyPath); err2 == nil && f2.Size() > 0 {
			return certPath, keyPath, nil
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("生成 ECC 私钥失败: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := crand.Int(crand.Reader, serialNumberLimit)
	if err != nil {
		serialNumber = big.NewInt(time.Now().UnixNano())
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   domain,
			Organization: []string{"Cloudflare Managed AutoCert"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain, "*." + domain},
	}

	certDER, err := x509.CreateCertificate(crand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return "", "", fmt.Errorf("生成自签证书失败: %w", err)
	}

	certOut, err := os.OpenFile(certPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return "", "", err
	}
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	_ = certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	keyOut, err := os.OpenFile(keyPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", "", err
	}
	_ = pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	_ = keyOut.Close()

	return certPath, keyPath, nil
}

// findFreePort 寻找一个可用端口
func findFreePort(preferred int) int {
	if preferred > 0 && preferred <= 65535 {
		if l, err := net.Listen("tcp", fmt.Sprintf(":%d", preferred)); err == nil {
			_ = l.Close()
			return preferred
		}
	}
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		r, _ := crand.Int(crand.Reader, big.NewInt(40000))
		return int(r.Int64()) + 20000
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// getActivePanelMode 获取当前真实运行的 panel 模式 ("sing-box" 或 "s-ui")
func getActivePanelMode(workDir string) string {
	mode := "sing-box"
	pmPath := filepath.Join(workDir, "panel_mode")
	if data, err := os.ReadFile(pmPath); err == nil {
		m := strings.TrimSpace(string(data))
		if m == "s-ui" || m == "sing-box" {
			return m
		}
	}
	if fileExists("/usr/local/s-ui/db/s-ui.db") {
		return "s-ui"
	}
	return mode
}

// apiNodePresetInfoHandler 返回当前节点预设信息与可用证书
func apiNodePresetInfoHandler(w http.ResponseWriter, r *http.Request) {
	workDir := currentBasePathDir()
	mode := getActivePanelMode(workDir)

	gwStatus := GetGatewayStatus(workDir)
	domain := ""
	nodePort := 2082
	wsPath := "/wsargo"
	hasTunnel := false

	if gwStatus != nil {
		if d, ok := gwStatus["domain"].(string); ok && d != "" {
			domain = d
			hasTunnel = true
		}
		if p, ok := gwStatus["node_port"].(float64); ok && p > 0 {
			nodePort = int(p)
		} else if p2, ok := gwStatus["node_port"].(int); ok && p2 > 0 {
			nodePort = p2
		}
		if wp, ok := gwStatus["ws_path"].(string); ok && wp != "" {
			wsPath = "/" + strings.Trim(wp, "/")
		}
	}

	pubIP := hostPublicIP()
	pubIPv6 := hostPublicIPv6()
	certs := scanLocalACMECerts()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"panel_mode":  mode,
		"has_tunnel":  hasTunnel,
		"domain":      domain,
		"node_port":   nodePort,
		"ws_path":     wsPath,
		"public_ip":   pubIP,
		"public_ipv6": pubIPv6,
		"acme_certs":  certs,
	})
}

// NodeCreatePresetReq 节点预设创建请求
type NodeCreatePresetReq struct {
	Preset   string `json:"preset"` // "vless_argo", "vless_reality", "tuic_hy2"
	Domain   string `json:"domain,omitempty"`
	Port     int    `json:"port,omitempty"`
	SNI      string `json:"sni,omitempty"`
	CertType string `json:"cert_type,omitempty"` // "self_signed", "local_acme", "custom"
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
}

// apiNodeCreatePresetHandler 处理预设节点创建
func apiNodeCreatePresetHandler(mgr *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST 请求"})
			return
		}

		var req NodeCreatePresetReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "解析请求参数失败: " + err.Error()})
			return
		}

		workDir := currentBasePathDir()
		panelMode := getActivePanelMode(workDir)
		gwStatus := GetGatewayStatus(workDir)

		switch req.Preset {
		case "vless_argo":
			domain := strings.TrimSpace(req.Domain)
			if domain == "" && gwStatus != nil {
				if d, ok := gwStatus["domain"].(string); ok {
					domain = d
				}
			}
			if domain == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "未检测到隧道域名，请先在上方配置 Cloudflare 隧道或手动输入域名！",
				})
				return
			}

			nodePort := req.Port
			if nodePort <= 0 && gwStatus != nil {
				if p, ok := gwStatus["node_port"].(float64); ok && p > 0 {
					nodePort = int(p)
				} else if p2, ok := gwStatus["node_port"].(int); ok && p2 > 0 {
					nodePort = p2
				}
			}
			if nodePort <= 0 {
				nodePort = 2082
			}

			wsPath := "/wsargo"
			if gwStatus != nil {
				if wp, ok := gwStatus["ws_path"].(string); ok && wp != "" {
					wsPath = "/" + strings.Trim(wp, "/")
				}
			}

			if panelMode == "s-ui" {
				err := createSUIArgoNode(workDir, domain, nodePort, wsPath)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "s-ui 模式下创建 VLESS-Argo 失败: " + err.Error()})
					return
				}
			} else {
				err := createSingBoxArgoNode(workDir, domain, nodePort, wsPath)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sing-box 独立模式下创建 VLESS-Argo 失败: " + err.Error()})
					return
				}
			}

			invalidateInbounds()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"message": fmt.Sprintf("VLESS-Argo 节点已成功创建 (回源 127.0.0.1:%d, 路径 %s, 域名 %s)", nodePort, wsPath, domain),
			})

		case "vless_reality":
			sni := strings.TrimSpace(req.SNI)
			if sni == "" {
				sni = "apple.com"
			}
			realityPort := req.Port
			if realityPort <= 0 {
				realityPort = findFreePort(2083)
			}

			if panelMode == "s-ui" {
				err := createSUIRealityNode(workDir, realityPort, sni)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "s-ui 模式下创建 VLESS-Reality 失败: " + err.Error()})
					return
				}
			} else {
				err := createSingBoxRealityNode(workDir, realityPort, sni)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sing-box 独立模式下创建 VLESS-Reality 失败: " + err.Error()})
					return
				}
			}

			invalidateInbounds()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"message": fmt.Sprintf("VLESS-Reality 节点已成功创建 (公网直连端口 %d, 伪装 %s, 免证书)", realityPort, sni),
			})

		case "tuic_hy2":
			certDomain := strings.TrimSpace(req.Domain)
			if certDomain == "" {
				certDomain = "apple.com"
			}
			certFile := strings.TrimSpace(req.CertFile)
			keyFile := strings.TrimSpace(req.KeyFile)
			isInsecure := req.Insecure

			if req.CertType == "self_signed" || (certFile == "" || keyFile == "") {
				cf, kf, err := createSelfSignedCert(certDomain)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "自动生成自签证书失败: " + err.Error()})
					return
				}
				certFile = cf
				keyFile = kf
				isInsecure = true
			}

			if panelMode == "s-ui" {
				err := createSUITuicHy2Nodes(workDir, certDomain, certFile, keyFile, isInsecure)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "s-ui 模式下创建 TUIC/Hysteria2 失败: " + err.Error()})
					return
				}
			} else {
				err := createSingBoxTuicHy2Nodes(workDir, certDomain, certFile, keyFile, isInsecure)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sing-box 独立模式下创建 TUIC/Hysteria2 失败: " + err.Error()})
					return
				}
			}

			invalidateInbounds()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"message": fmt.Sprintf("TUIC 与 Hysteria2 高速节点已成功创建并上线 (域名 %s, 自签/免证书: %v)", certDomain, isInsecure),
			})

		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知节点类型: " + req.Preset})
		}
	}
}

// -----------------------------------------------------------------------------
// s-ui 外挂模式具体实现
// -----------------------------------------------------------------------------

func createSUIArgoNode(workDir, domain string, nodePort int, wsPath string) error {
	sui, err := DetectSUI(workDir)
	if err != nil {
		return err
	}

	cleanWS := "/" + strings.Trim(wsPath, "/")
	argoTag := fmt.Sprintf("vless-argo-%s", genRandSuffix())

	argoPayload := map[string]any{
		"id":          0,
		"type":        "vless",
		"tag":         argoTag,
		"tls_id":      0,
		"listen":      "127.0.0.1",
		"listen_port": nodePort,
		"transport": map[string]any{
			"type":                   "ws",
			"path":                   cleanWS,
			"headers":                map[string]any{"Host": domain},
			"early_data_header_name": "Sec-WebSocket-Protocol",
			"max_early_data":         2560,
		},
		"addrs": []any{
			map[string]any{
				"server":      domain,
				"server_port": 443,
				"tls": map[string]any{
					"enabled":     true,
					"insecure":    false,
					"server_name": domain,
					"utls": map[string]any{
						"enabled":     true,
						"fingerprint": "chrome",
					},
				},
			},
		},
	}
	payBytes, _ := json.Marshal(argoPayload)
	_, err = suiAPICall(sui.APIRoot(), sui.APIToken(), "POST", "save", url.Values{
		"object": {"inbounds"},
		"action": {"new"},
		"data":   {string(payBytes)},
	})
	if err != nil {
		return err
	}

	bindSUIDefaultClient(sui)
	return nil
}

func createSUIRealityNode(workDir string, realityPort int, sni string) error {
	sui, err := DetectSUI(workDir)
	if err != nil {
		return err
	}

	// 1. 查询现有 reality TLS
	tlsResp, _ := suiAPICall(sui.APIRoot(), sui.APIToken(), "GET", "tls", nil)
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

	privStr, pubStr := genX25519Keys()
	sid := genRandHex(8)

	if realityTID == nil || realityTID == 0 {
		realityTLSPayload := map[string]any{
			"id":   0,
			"name": "reality",
			"server": map[string]any{
				"enabled":     true,
				"server_name": sni,
				"reality": map[string]any{
					"enabled": true,
					"handshake": map[string]any{
						"server_port": 443,
						"server":      sni,
					},
					"short_id":    []string{sid},
					"private_key": privStr,
				},
			},
			"client": map[string]any{
				"reality": map[string]any{
					"public_key": pubStr,
					"short_id":   sid,
				},
				"utls": map[string]any{
					"enabled":     true,
					"fingerprint": "chrome",
				},
			},
		}
		payT, _ := json.Marshal(realityTLSPayload)
		respT, err := suiAPICall(sui.APIRoot(), sui.APIToken(), "POST", "save", url.Values{
			"object": {"tls"},
			"action": {"new"},
			"data":   {string(payT)},
		})
		if err == nil && respT != nil {
			if obj, ok := respT["obj"].(map[string]any); ok {
				realityTID = obj["id"]
			}
		}
		time.Sleep(150 * time.Millisecond)
	}

	var realityAddrs []any
	pubIP := hostPublicIP()
	if pubIP != "" && pubIP != "127.0.0.1" {
		realityAddrs = append(realityAddrs, map[string]any{
			"server":      pubIP,
			"server_port": realityPort,
		})
	}
	pubV6 := hostPublicIPv6()
	if pubV6 != "" && pubV6 != "127.0.0.1" {
		realityAddrs = append(realityAddrs, map[string]any{
			"server":      pubV6,
			"server_port": realityPort,
		})
	}
	if len(realityAddrs) == 0 {
		realityAddrs = append(realityAddrs, map[string]any{
			"server":      "127.0.0.1",
			"server_port": realityPort,
		})
	}

	realityTag := fmt.Sprintf("vless-reality-%s", genRandSuffix())
	realityPayload := map[string]any{
		"id":          0,
		"type":        "vless",
		"tag":         realityTag,
		"tls_id":      realityTID,
		"listen":      "::",
		"listen_port": realityPort,
		"addrs":       realityAddrs,
	}
	payBytes, _ := json.Marshal(realityPayload)
	_, err = suiAPICall(sui.APIRoot(), sui.APIToken(), "POST", "save", url.Values{
		"object": {"inbounds"},
		"action": {"new"},
		"data":   {string(payBytes)},
	})
	if err != nil {
		return err
	}

	bindSUIDefaultClient(sui)
	return nil
}

func createSUITuicHy2Nodes(workDir, certDomain, certFile, keyFile string, isInsecure bool) error {
	sui, err := DetectSUI(workDir)
	if err != nil {
		return err
	}

	tuicPort := findFreePort(0)
	hy2Port := findFreePort(0)
	for hy2Port == tuicPort {
		hy2Port = findFreePort(0)
	}

	insecStr := "false"
	if isInsecure {
		insecStr = "true"
	}

	// 复用 toolSUIApplyTuicHy2
	os.Setenv("SUI_API", sui.APIRoot())
	os.Setenv("SUI_TOKEN", sui.APIToken())
	os.Setenv("CERT_DOMAIN", certDomain)
	os.Setenv("CERT_FILE", certFile)
	os.Setenv("KEY_FILE", keyFile)
	os.Setenv("IS_INSECURE", insecStr)
	os.Setenv("ACTION_MODE", "create_both")
	os.Setenv("TUIC_PORT", strconv.Itoa(tuicPort))
	os.Setenv("HY2_PORT", strconv.Itoa(hy2Port))
	os.Setenv("PUBLIC_IP", hostPublicIP())
	os.Setenv("CONGESTION_CONTROL", "bbr")

	toolSUIApplyTuicHy2(nil)
	return nil
}

func bindSUIDefaultClient(sui *SUI) {
	// 关联新入站至管理员 Client
	inbRespLatest, _ := suiAPICall(sui.APIRoot(), sui.APIToken(), "GET", "inbounds", nil)
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

	cliResp, _ := suiAPICall(sui.APIRoot(), sui.APIToken(), "GET", "clients", nil)
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
	if len(clients) > 0 {
		targetClient = clients[0]
	}

	userName := "admin"
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
	_, _ = suiAPICall(sui.APIRoot(), sui.APIToken(), "POST", "save", url.Values{
		"object": {"clients"},
		"action": {cliAct},
		"data":   {string(cliBytes)},
	})
}

// -----------------------------------------------------------------------------
// sing-box 独立轻量模式具体实现
// -----------------------------------------------------------------------------

func getSingboxConfigFile(workDir string) string {
	candidates := []string{
		singboxConfigDefault,
		filepath.Join(workDir, "singbox_config.json"),
		"/usr/local/etc/sing-box/config.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return singboxConfigDefault
}

func createSingBoxArgoNode(workDir, domain string, nodePort int, wsPath string) error {
	sb, err := DetectSingBox(workDir)
	if err != nil {
		return err
	}

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	cleanWS := "/" + strings.Trim(wsPath, "/")
	argoTag := fmt.Sprintf("vless-argo-%s", genRandSuffix())
	cUUID := genUUID()

	inbounds, _ := cfg["inbounds"].([]any)
	newIb := map[string]any{
		"type":        "vless",
		"tag":         argoTag,
		"listen":      "127.0.0.1",
		"listen_port": nodePort,
		"users": []any{
			map[string]any{"name": "admin", "uuid": cUUID},
		},
		"transport": map[string]any{
			"type":                   "ws",
			"path":                   cleanWS,
			"headers":                map[string]any{"Host": domain},
			"early_data_header_name": "Sec-WebSocket-Protocol",
			"max_early_data":         2560,
		},
	}
	cfg["inbounds"] = append(inbounds, newIb)

	if err := sb.saveConfig(cfg); err != nil {
		return err
	}

	// 记录连接地址
	addrsMap := sb.loadRawInboundAddrs()
	addrsMap[argoTag] = []map[string]any{
		{
			"server":      domain,
			"server_port": 443,
			"tls": map[string]any{
				"enabled":     true,
				"insecure":    false,
				"server_name": domain,
				"utls": map[string]any{
					"enabled":     true,
					"fingerprint": "chrome",
				},
			},
		},
	}
	sb.saveRawInboundAddrs(addrsMap)

	_ = sb.syncToEmbedded(cfg)
	return nil
}

func createSingBoxRealityNode(workDir string, realityPort int, sni string) error {
	sb, err := DetectSingBox(workDir)
	if err != nil {
		return err
	}

	cfg, err := sb.loadConfig()
	if err != nil {
		return err
	}

	privStr, pubStr := genX25519Keys()
	sid := genRandHex(8)
	realityTag := fmt.Sprintf("vless-reality-%s", genRandSuffix())
	cUUID := genUUID()

	inbounds, _ := cfg["inbounds"].([]any)
	newIb := map[string]any{
		"type":        "vless",
		"tag":         realityTag,
		"listen":      "::",
		"listen_port": realityPort,
		"users": []any{
			map[string]any{"name": "admin", "uuid": cUUID, "flow": "xtls-rprx-vision"},
		},
		"tls": map[string]any{
			"enabled":     true,
			"server_name": sni,
			"reality": map[string]any{
				"enabled":   true,
				"handshake": map[string]any{"server": sni, "server_port": 443},
				"private_key": privStr,
				"short_id":    []string{sid},
			},
		},
	}
	cfg["inbounds"] = append(inbounds, newIb)

	if err := sb.saveConfig(cfg); err != nil {
		return err
	}

	// 记录连接地址
	pubIP := hostPublicIP()
	if pubIP == "" {
		pubIP = "127.0.0.1"
	}
	pubV6 := hostPublicIPv6()

	var addrsList []map[string]any
	addrsList = append(addrsList, map[string]any{
		"server":      pubIP,
		"server_port": realityPort,
		"tls": map[string]any{
			"enabled":     true,
			"server_name": sni,
			"utls": map[string]any{
				"enabled":     true,
				"fingerprint": "chrome",
			},
			"reality": map[string]any{
				"public_key": pubStr,
				"short_id":   sid,
			},
		},
	})
	if pubV6 != "" && pubV6 != "127.0.0.1" && pubV6 != pubIP {
		addrsList = append(addrsList, map[string]any{
			"server":      pubV6,
			"server_port": realityPort,
			"tls": map[string]any{
				"enabled":     true,
				"server_name": sni,
				"utls": map[string]any{
					"enabled":     true,
					"fingerprint": "chrome",
				},
				"reality": map[string]any{
					"public_key": pubStr,
					"short_id":   sid,
				},
			},
		})
	}

	addrsMap := sb.loadRawInboundAddrs()
	addrsMap[realityTag] = addrsList
	sb.saveRawInboundAddrs(addrsMap)

	_ = sb.syncToEmbedded(cfg)
	return nil
}

func createSingBoxTuicHy2Nodes(workDir, certDomain, certFile, keyFile string, isInsecure bool) error {
	sb, err := DetectSingBox(workDir)
	if err != nil {
		return err
	}

	sbConf := getSingboxConfigFile(workDir)
	tuicPort := findFreePort(0)
	hy2Port := findFreePort(0)
	for hy2Port == tuicPort {
		hy2Port = findFreePort(0)
	}

	insecStr := "false"
	if isInsecure {
		insecStr = "true"
	}

	pubIP := hostPublicIP()
	if pubIP == "" {
		pubIP = certDomain
	}

	os.Setenv("SB_CONF", sbConf)
	os.Setenv("CERT_DOM", certDomain)
	os.Setenv("CERT_FILE", certFile)
	os.Setenv("KEY_FILE", keyFile)
	os.Setenv("TUIC_PORT", strconv.Itoa(tuicPort))
	os.Setenv("HY2_PORT", strconv.Itoa(hy2Port))
	os.Setenv("TUIC_UUID", genUUID())
	os.Setenv("TUIC_PASS", genRandHex(8))
	os.Setenv("HY2_PASS", genRandHex(8))
	os.Setenv("NODE_SERVER", pubIP)
	os.Setenv("IS_INSECURE", insecStr)
	os.Setenv("CONGESTION_CTRL", "bbr")

	toolSingboxApplyTuicHy2(nil)

	if cfg, err := sb.loadConfig(); err == nil {
		_ = sb.syncToEmbedded(cfg)
	}
	return nil
}
