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
		_ = os.WriteFile(metaPath, out, 0600)
	}
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
