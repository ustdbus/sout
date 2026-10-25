package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

// CFDomainEntry 记录已通过 Cloudflare 申请证书的域名配置元数据
type CFDomainEntry struct {
	Token     string `json:"token"`
	AppliedAt int64  `json:"applied_at"`
	NotAfter  int64  `json:"not_after"`
	CertDir   string `json:"cert_dir"`
}

// loadOrGenerateAccountKey 获取或持久化 ACME 账户私钥
func loadOrGenerateAccountKey(keyPath string) (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(data)
		if block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := x509.MarshalECPrivateKey(key)
	if err == nil {
		var buf bytes.Buffer
		_ = pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
		_ = os.MkdirAll(filepath.Dir(keyPath), 0700)
		_ = os.WriteFile(keyPath, buf.Bytes(), 0600)
	}
	return key, nil
}

// getCFZoneID 动态解析并匹配域名对应的 Cloudflare 最佳托管区域 Zone ID
func getCFZoneID(cfToken, domain string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", "https://api.cloudflare.com/client/v4/zones?per_page=50&status=active", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var res struct {
		Success bool `json:"success"`
		Result  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if !res.Success || len(res.Result) == 0 {
		msg := "未检索到活跃的 Cloudflare 托管区域，请检查 API Token 权限是否包含 Zone.Zone:Read 和 Zone.DNS:Edit"
		if len(res.Errors) > 0 {
			msg += " (" + res.Errors[0].Message + ")"
		}
		return "", fmt.Errorf("%s", msg)
	}

	bestZoneID := ""
	bestMatchLen := 0
	domLower := strings.ToLower(domain)
	for _, z := range res.Result {
		zName := strings.ToLower(z.Name)
		if domLower == zName || strings.HasSuffix(domLower, "."+zName) {
			if len(zName) > bestMatchLen {
				bestMatchLen = len(zName)
				bestZoneID = z.ID
			}
		}
	}
	if bestZoneID == "" {
		return "", fmt.Errorf("域名 [%s] 未在其 Cloudflare 账户托管的域名列表中", domain)
	}
	return bestZoneID, nil
}

// createCFTXTRecord 创建 Cloudflare DNS TXT 记录
func createCFTXTRecord(cfToken, zoneID, name, content string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	bodyData, _ := json.Marshal(map[string]any{
		"type":    "TXT",
		"name":    name,
		"content": content,
		"ttl":     60,
	})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var res struct {
		Success bool `json:"success"`
		Result  struct {
			ID string `json:"id"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if !res.Success {
		msg := "添加 DNS TXT 记录失败"
		if len(res.Errors) > 0 {
			msg += ": " + res.Errors[0].Message
		}
		return "", fmt.Errorf("%s", msg)
	}
	return res.Result.ID, nil
}

// deleteCFTXTRecord 删除 Cloudflare DNS TXT 记录 (清理)
func deleteCFTXTRecord(cfToken, zoneID, recordID string) {
	if recordID == "" || zoneID == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfToken)
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

// IssueCFCert 纯 Go 原生向 Let's Encrypt 通过 Cloudflare DNS-01 验证申请 ECC SSL 证书
func IssueCFCert(domain, cfToken, certDir, workDir string, force bool) error {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	if idx := strings.Index(domain, "/"); idx != -1 {
		domain = domain[:idx]
	}
	cfToken = strings.TrimSpace(cfToken)
	if domain == "" || cfToken == "" {
		return fmt.Errorf("域名与 Cloudflare API Token 不能为空")
	}

	fullchainFile := filepath.Join(certDir, "fullchain.pem")
	privkeyFile := filepath.Join(certDir, "privkey.pem")

	// 1. 若本地证书仍有效且非强制模式，直接复用
	if !force && isCertValidAndNotExpiring(fullchainFile, 30) {
		fmt.Printf("  [✓] 本地已存在域名 [%s] 的有效证书（剩余有效期 > 30 天），直接复用。\n", domain)
		return nil
	}

	fmt.Printf("  [1/4] 正在解析 Cloudflare 托管区域 (Zone ID)...\n")
	zoneID, err := getCFZoneID(cfToken, domain)
	if err != nil {
		return fmt.Errorf("解析 Cloudflare Zone 失败: %w", err)
	}

	fmt.Printf("  [2/4] 正在初始化纯 Go 原生 ACME 客户端...\n")
	accountKeyFile := filepath.Join(workDir, "acme_account.key")
	accountKey, err := loadOrGenerateAccountKey(accountKeyFile)
	if err != nil {
		return fmt.Errorf("初始化 ACME 账户私钥失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &acme.Client{
		DirectoryURL: acme.LetsEncryptURL,
		Key:          accountKey,
	}

	// 注册 ACME 账户 (若已存在则忽略错误)
	account := &acme.Account{Contact: []string{"mailto:admin@" + domain}}
	_, _ = client.Register(ctx, account, acme.AcceptTOS)

	fmt.Printf("  [3/4] 正在向 Let's Encrypt 发起 DNS-01 订单并配置验证记录...\n")
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return fmt.Errorf("创建 ACME 订单失败: %w", err)
	}

	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return fmt.Errorf("获取授权信息失败: %w", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}

		var chal *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == "dns-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return fmt.Errorf("ACME 服务端未提供 dns-01 验证项")
		}

		txtVal, err := client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return fmt.Errorf("计算 DNS-01 验证记录失败: %w", err)
		}

		recName := "_acme-challenge." + authz.Identifier.Value
		fmt.Printf("  [+] 正在向 Cloudflare 添加 DNS TXT 记录 [%s]...\n", recName)
		recID, err := createCFTXTRecord(cfToken, zoneID, recName, txtVal)
		if err != nil {
			return fmt.Errorf("写入 Cloudflare DNS TXT 记录失败: %w", err)
		}
		defer deleteCFTXTRecord(cfToken, zoneID, recID)

		fmt.Printf("  [+] DNS 记录写入成功，等待全球 DNS 节点同步生效 (约 15 秒)...\n")
		time.Sleep(15 * time.Second)

		fmt.Printf("  [+] 正在通知 Let's Encrypt 进行所有权验证...\n")
		if _, err := client.Accept(ctx, chal); err != nil {
			return fmt.Errorf("提交 ACME 验证请求失败: %w", err)
		}

		if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
			return fmt.Errorf("Let's Encrypt DNS-01 校验未通过: %w", err)
		}
	}

	fmt.Printf("  [4/4] 验证通过！正在签发证书并生成 ECC 密钥对...\n")
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return fmt.Errorf("等待订单就绪失败: %w", err)
	}

	// 生成域名 ECC 私钥 (ECDSA P-256)
	domainKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 ECC 私钥失败: %w", err)
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, domainKey)
	if err != nil {
		return fmt.Errorf("创建证书请求 CSR 失败: %w", err)
	}

	derCerts, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return fmt.Errorf("Let's Encrypt 签发证书失败: %w", err)
	}

	var certPEM bytes.Buffer
	for _, b := range derCerts {
		_ = pem.Encode(&certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: b})
	}

	keyBytes, err := x509.MarshalECPrivateKey(domainKey)
	if err != nil {
		return fmt.Errorf("格式化 ECC 私钥失败: %w", err)
	}
	var keyPEM bytes.Buffer
	_ = pem.Encode(&keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := os.MkdirAll(certDir, 0700); err != nil {
		return fmt.Errorf("创建证书存放目录失败: %w", err)
	}

	// 同时写入全链证书与兼容文件 (cert.crt / private.key)
	_ = os.WriteFile(fullchainFile, certPEM.Bytes(), 0600)
	_ = os.WriteFile(privkeyFile, keyPEM.Bytes(), 0600)
	_ = os.WriteFile(filepath.Join(certDir, "cert.crt"), certPEM.Bytes(), 0600)
	_ = os.WriteFile(filepath.Join(certDir, "private.key"), keyPEM.Bytes(), 0600)

	// 读取解析 NotAfter
	var notAfter int64
	if len(derCerts) > 0 {
		if c, err := x509.ParseCertificate(derCerts[0]); err == nil {
			notAfter = c.NotAfter.Unix()
		}
	}

	// 更新持久化元数据
	metaFile := filepath.Join(workDir, "cf_ssl_domains.json")
	saveCFDomainMeta(metaFile, domain, cfToken, certDir, notAfter)

	fmt.Printf("  [✓] 证书已成功签发并保存至: %s\n", certDir)
	return nil
}

func saveCFDomainMeta(metaPath, domain, token, certDir string, notAfter int64) {
	var d map[string]CFDomainEntry
	if data, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(data, &d)
	}
	if d == nil {
		d = make(map[string]CFDomainEntry)
	}
	d[domain] = CFDomainEntry{
		Token:     token,
		AppliedAt: time.Now().Unix(),
		NotAfter:  notAfter,
		CertDir:   certDir,
	}
	if out, err := json.MarshalIndent(d, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(metaPath), 0700)
		_ = atomicWriteFile(metaPath, out, 0600)
	}
}

// atomicWriteFile 先写入同目录临时文件再原子重命名，防止断电或 OOM 截断配置文件。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// isValidCertDomain 校验域名合法性，防止路径穿越（如 ../../etc）导致任意目录被写入或删除。
func isValidCertDomain(domain string) bool {
	domain = strings.TrimSpace(domain)
	if domain == "" || len(domain) > 253 {
		return false
	}
	if strings.Contains(domain, "..") || strings.ContainsAny(domain, `/\`) {
		return false
	}
	if domain[0] == '.' || domain[0] == '-' || domain[len(domain)-1] == '.' || domain[len(domain)-1] == '-' {
		return false
	}
	hasDot := false
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if c == '.' {
			hasDot = true
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return hasDot && filepath.Base(domain) == domain
}

// isCertValidAndNotExpiring 检查证书是否有效且剩余天数大于指定天数
func isCertValidAndNotExpiring(certPath string, minDaysLeft int) bool {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	daysLeft := int(time.Until(cert.NotAfter).Hours() / 24)
	return daysLeft > minDaysLeft
}

// ParseCertInfo 纯 Go 解析证书详细信息，无需操作系统 openssl 命令行
func ParseCertInfo(certPath string) (map[string]any, error) {
	if fi, err := os.Stat(certPath); err == nil && fi.IsDir() {
		for _, name := range []string{"fullchain.pem", "cert.crt", "cert.pem"} {
			candidate := filepath.Join(certPath, name)
			if cfi, err := os.Stat(candidate); err == nil && !cfi.IsDir() {
				certPath = candidate
				break
			}
		}
	}
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("无效的 PEM 证书数据")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	issuer := cert.Issuer.CommonName
	if issuer == "" && len(cert.Issuer.Organization) > 0 {
		issuer = cert.Issuer.Organization[0]
	}
	if issuer == "" {
		issuer = "Let's Encrypt"
	}

	daysLeft := int(time.Until(cert.NotAfter).Hours() / 24)
	isValid := daysLeft > 0

	return map[string]any{
		"domain":     cert.Subject.CommonName,
		"dns_names":  cert.DNSNames,
		"issuer":     issuer,
		"not_before": cert.NotBefore.Format("2006-01-02 15:04:05 MST"),
		"not_after":  cert.NotAfter.Format("2006-01-02 15:04:05 MST"),
		"days_left":  daysLeft,
		"is_valid":   isValid,
	}, nil
}

// RenewAllCerts 遍历检查所有通过 Cloudflare 托管的域名证书并在到期前 30 天内自动静默续期
func RenewAllCerts(workDir string) error {
	metaFile := filepath.Join(workDir, "cf_ssl_domains.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return nil
	}
	var domains map[string]CFDomainEntry
	if err := json.Unmarshal(data, &domains); err != nil {
		return nil
	}

	renewed := 0
	for dom, entry := range domains {
		certPath := filepath.Join(entry.CertDir, "fullchain.pem")
		if !isCertValidAndNotExpiring(certPath, 30) {
			log.Printf("[SSL自动续期] 域名 [%s] 证书已过期或即将到期（<= 30天），正在后台静默自动续期...", dom)
			if err := IssueCFCert(dom, entry.Token, entry.CertDir, workDir, true); err != nil {
				log.Printf("[SSL自动续期] 域名 [%s] 续期失败: %v", dom, err)
			} else {
				log.Printf("[SSL自动续期] 域名 [%s] 证书自动续期成功！", dom)
				renewed++
			}
		}
	}

	// 若有证书续期成功，触发重载相关服务
	if renewed > 0 {
		log.Printf("[SSL自动续期] 共成功续期 %d 个域名证书，正在触发服务热重载...", renewed)
		triggerServiceReload()
	}
	return nil
}

// triggerServiceReload 证书更新后自动热重载或平滑重启服务
func triggerServiceReload() {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.Command("systemctl", "restart", "sout").Run()
		_ = exec.Command("systemctl", "restart", "s-ui").Run()
	} else if _, err := exec.LookPath("rc-service"); err == nil {
		_ = exec.Command("rc-service", "sout", "restart").Run()
		_ = exec.Command("rc-service", "s-ui", "restart").Run()
	}
}

// startCertRenewalDaemon 启动常驻后台自动巡检协程：启动后即刻自检一次，随后每 12 小时静默轮询一次
func startCertRenewalDaemon(workDir string) {
	time.Sleep(30 * time.Second)
	_ = RenewAllCerts(workDir)

	ticker := time.NewTicker(12 * time.Hour)
	for range ticker.C {
		_ = RenewAllCerts(workDir)
	}
}

// CertViewItem 证书视图项
type CertViewItem struct {
	Domain    string   `json:"domain"`
	Type      string   `json:"type"` // "acme" 或 "self_signed"
	CertFile  string   `json:"cert_file"`
	KeyFile   string   `json:"key_file"`
	CertPath  string   `json:"cert_path"`
	KeyPath   string   `json:"key_path"`
	CertDir   string   `json:"cert_dir"`
	Issuer    string   `json:"issuer"`
	DNSNames  []string `json:"dns_names"`
	NotBefore string   `json:"not_before"`
	NotAfter  string   `json:"not_after"`
	DaysLeft  int      `json:"days_left"`
	IsValid   bool     `json:"is_valid"`
}

// GetAllCerts 收集 /home/acme 与 /home/ssc 下的所有证书
func GetAllCerts() []CertViewItem {
	var res []CertViewItem
	seen := make(map[string]bool)

	// 1. 扫描 /home/acme (Let's Encrypt / Cloudflare DNS 证书)
	acmeDir := "/home/acme"
	if entries, err := os.ReadDir(acmeDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dom := entry.Name()
			dPath := filepath.Join(acmeDir, dom)
			certFile := ""
			for _, name := range []string{"fullchain.pem", "cert.crt", "cert.pem", dom + ".cer"} {
				cand := filepath.Join(dPath, name)
				if fi, err := os.Stat(cand); err == nil && fi.Size() > 0 {
					certFile = cand
					break
				}
			}
			keyFile := ""
			for _, name := range []string{"privkey.pem", "private.key", "key.pem", dom + ".key"} {
				cand := filepath.Join(dPath, name)
				if fi, err := os.Stat(cand); err == nil && fi.Size() > 0 {
					keyFile = cand
					break
				}
			}

			if certFile != "" && keyFile != "" {
				item := CertViewItem{
					Domain:   dom,
					Type:     "acme",
					CertFile: certFile,
					KeyFile:  keyFile,
					CertPath: certFile,
					KeyPath:  keyFile,
					CertDir:  dPath,
					Issuer:   "Let's Encrypt",
				}
				if info, err := ParseCertInfo(certFile); err == nil {
					if iss, ok := info["issuer"].(string); ok && iss != "" {
						item.Issuer = iss
					}
					if dns, ok := info["dns_names"].([]string); ok {
						item.DNSNames = dns
					}
					if nb, ok := info["not_before"].(string); ok {
						item.NotBefore = nb
					}
					if na, ok := info["not_after"].(string); ok {
						item.NotAfter = na
					}
					if dl, ok := info["days_left"].(int); ok {
						item.DaysLeft = dl
					}
					if iv, ok := info["is_valid"].(bool); ok {
						item.IsValid = iv
					}
				}
				seen[dom] = true
				res = append(res, item)
			}
		}
	}

	// 2. 扫描 /home/ssc (自签证书)
	sscDir := "/home/ssc"
	if entries, err := os.ReadDir(sscDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dom := entry.Name()
			dPath := filepath.Join(sscDir, dom)
			certFile := filepath.Join(dPath, "fullchain.pem")
			keyFile := filepath.Join(dPath, "privkey.pem")

			if fi, err := os.Stat(certFile); err == nil && fi.Size() > 0 {
				if fi2, err2 := os.Stat(keyFile); err2 == nil && fi2.Size() > 0 {
					item := CertViewItem{
						Domain:   dom,
						Type:     "self_signed",
						CertFile: certFile,
						KeyFile:  keyFile,
						CertPath: certFile,
						KeyPath:  keyFile,
						CertDir:  dPath,
						Issuer:   "自签证书 (Self-Signed)",
					}
					if info, err := ParseCertInfo(certFile); err == nil {
						if nb, ok := info["not_before"].(string); ok {
							item.NotBefore = nb
						}
						if na, ok := info["not_after"].(string); ok {
							item.NotAfter = na
						}
						if dl, ok := info["days_left"].(int); ok {
							item.DaysLeft = dl
						}
						if iv, ok := info["is_valid"].(bool); ok {
							item.IsValid = iv
						}
					}
					res = append(res, item)
				}
			}
		}
	}

	return res
}

func apiCertsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET 请求"})
		return
	}
	certs := GetAllCerts()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"certs": certs,
	})
}

func apiCertApplyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST 请求"})
		return
	}

	var req struct {
		Mode   string `json:"mode"` // "cloudflare" 或 "self_signed"
		Domain string `json:"domain"`
		Token  string `json:"token"`
		Force  bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数解析失败: " + err.Error()})
		return
	}

	domain := strings.TrimSpace(req.Domain)
	if domain == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "域名不能为空"})
		return
	}
	if !isValidCertDomain(domain) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "域名格式不合法"})
		return
	}

	workDir := currentBasePathDir()

	switch req.Mode {
	case "cloudflare":
		token := strings.TrimSpace(req.Token)
		if token == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Cloudflare API Token 不能为空"})
			return
		}
		certDir := filepath.Join("/home/acme", domain)
		if err := IssueCFCert(domain, token, certDir, workDir, req.Force); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "申请证书失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"message":  fmt.Sprintf("域名 %s 的 Cloudflare SSL 证书已成功签发！", domain),
			"cert_dir": certDir,
		})

	case "self_signed":
		certFile, keyFile, err := createSelfSignedCert(domain)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成自签证书失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"message":   fmt.Sprintf("域名 %s 的自签证书已生成成功！", domain),
			"cert_file": certFile,
			"key_file":  keyFile,
		})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知申请模式: " + req.Mode})
	}
}

func apiCertDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST 请求"})
		return
	}

	var req struct {
		Domain string `json:"domain"`
		Type   string `json:"type"` // "acme" 或 "self_signed"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数解析失败: " + err.Error()})
		return
	}

	domain := strings.TrimSpace(req.Domain)
	if domain == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "域名不能为空"})
		return
	}
	if !isValidCertDomain(domain) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "域名格式不合法"})
		return
	}

	targetDir := ""
	if req.Type == "self_signed" {
		targetDir = filepath.Join("/home/ssc", filepath.Base(domain))
	} else {
		targetDir = filepath.Join("/home/acme", filepath.Base(domain))
	}

	if targetDir != "" {
		_ = os.RemoveAll(targetDir)
	}

	// 同步从 s-ui / sout 中删除和清理悬空的 TLS 模板与证书引用
	cleanupDeletedCertTLS(domain, targetDir)

	// 从 cf_ssl_domains.json 移除
	workDir := currentBasePathDir()
	metaFile := filepath.Join(workDir, "cf_ssl_domains.json")
	if data, err := os.ReadFile(metaFile); err == nil {
		var domains map[string]CFDomainEntry
		if json.Unmarshal(data, &domains) == nil {
			delete(domains, domain)
			out, _ := json.MarshalIndent(domains, "", "  ")
			_ = atomicWriteFile(metaFile, out, 0600)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("域名 %s 的证书已成功删除，关联 TLS 配置已同步清除", domain),
	})
}

// cleanupDeletedCertTLS 当证书被删除时，同步清理 s-ui 与 sing-box 中的关联 TLS 模板与证书引用
func cleanupDeletedCertTLS(domain, targetDir string) {
	workDir := currentBasePathDir()
	domLower := strings.ToLower(strings.TrimSpace(domain))
	if domLower == "" {
		return
	}

	// 1. 若为 s-ui 模式，清理 s-ui 中的 TLS 模板记录
	if sui, err := DetectSUI(workDir); err == nil && sui != nil {
		tlsResp, _ := suiAPICall(sui.APIRoot(), sui.APIToken(), "GET", "tls", nil)
		if tlsResp != nil {
			if obj, ok := tlsResp["obj"].(map[string]any); ok {
				if list, ok := obj["tls"].([]any); ok {
					for _, item := range list {
						m, ok := item.(map[string]any)
						if !ok {
							continue
						}
						name, _ := m["name"].(string)
						tlsID := m["id"]
						match := false
						if strings.EqualFold(name, domLower) {
							match = true
						}
						if !match {
							if srv, ok := m["server"].(map[string]any); ok {
								sn, _ := srv["server_name"].(string)
								cp, _ := srv["certificate_path"].(string)
								if strings.EqualFold(sn, domLower) || (targetDir != "" && strings.Contains(cp, targetDir)) {
									match = true
								}
							}
						}

						if match && tlsID != nil {
							// 检查并解绑关联该 tls_id 的入站，避免 s-ui 报错
							inbResp, _ := suiAPICall(sui.APIRoot(), sui.APIToken(), "GET", "inbounds", nil)
							if inbResp != nil {
								if inbObj, ok := inbResp["obj"].(map[string]any); ok {
									if inbList, ok := inbObj["inbounds"].([]any); ok {
										for _, inb := range inbList {
											if inbMap, ok := inb.(map[string]any); ok {
												if inbMap["tls_id"] == tlsID {
													inbMap["tls_id"] = 0
													b, _ := json.Marshal(inbMap)
													_, _ = suiAPICall(sui.APIRoot(), sui.APIToken(), "POST", "save", url.Values{
														"object": []string{"inbounds"},
														"action": []string{"edit"},
														"data":   []string{string(b)},
													})
												}
											}
										}
									}
								}
							}

							// 调用 s-ui 原生 API 删除该 TLS 模板
							idBytes, _ := json.Marshal(tlsID)
							_, _ = suiAPICall(sui.APIRoot(), sui.APIToken(), "POST", "save", url.Values{
								"object": []string{"tls"},
								"action": []string{"del"},
								"data":   []string{string(idBytes)},
							})
							log.Printf("[证书删除] 已从 s-ui 中同步删除 TLS 模板 [ID: %v, Name: %s]", tlsID, name)
						}
					}
				}
			}
		}
	}

	// 2. 若为 sing-box 独立轻量模式，检查并清理 config.json 与 singbox_inbound_addrs.json
	if sb, err := DetectSingBox(workDir); err == nil && sb != nil {
		if cfg, err := sb.loadConfig(); err == nil && cfg != nil {
			changed := false
			if inbounds, ok := cfg["inbounds"].([]any); ok {
				for _, raw := range inbounds {
					if ib, ok := raw.(map[string]any); ok {
						if tlsMap, ok := ib["tls"].(map[string]any); ok && tlsMap != nil {
							sn, _ := tlsMap["server_name"].(string)
							cp, _ := tlsMap["certificate_path"].(string)
							if strings.EqualFold(sn, domLower) || (targetDir != "" && strings.Contains(cp, targetDir)) {
								delete(tlsMap, "certificate_path")
								delete(tlsMap, "key_path")
								tlsMap["enabled"] = false
								ib["tls"] = tlsMap
								changed = true
							}
						}
					}
				}
			}
			if changed {
				_ = sb.saveConfig(cfg)
				_ = sb.syncToEmbedded(cfg)
				log.Printf("[证书删除] 已从 sing-box 独立配置中清理关联的 TLS 证书引用: %s", domain)
			}
		}

		// 清理 singbox_inbound_addrs.json 里的客户端 TLS
		addrsPath := filepath.Join(workDir, "singbox_inbound_addrs.json")
		if data, err := os.ReadFile(addrsPath); err == nil {
			var addrsData map[string]any
			if json.Unmarshal(data, &addrsData) == nil {
				addrChanged := false
				for tag, listRaw := range addrsData {
					if list, ok := listRaw.([]any); ok {
						for _, itemRaw := range list {
							if item, ok := itemRaw.(map[string]any); ok {
								if tlsObj, ok := item["tls"].(map[string]any); ok {
									sn, _ := tlsObj["server_name"].(string)
									if strings.EqualFold(sn, domLower) {
										tlsObj["enabled"] = false
										tlsObj["insecure"] = false
										addrChanged = true
									}
								}
							}
						}
					}
					_ = tag
				}
				if addrChanged {
					if out, err := json.MarshalIndent(addrsData, "", "  "); err == nil {
						_ = os.WriteFile(addrsPath, out, 0644)
					}
				}
			}
		}
	}
}
