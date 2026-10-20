package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCertInfoAndGetAllCerts(t *testing.T) {
	// 1. 测试 GetAllCerts() 不崩溃
	certs := GetAllCerts()
	if certs == nil {
		t.Logf("GetAllCerts() returned nil or empty slice, which is expected in test env without /home/acme")
	}

	// 2. 生成一个临时证书测试 ParseCertInfo
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}

	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := time.Now().Add(30 * 24 * time.Hour)

	template := x509.Certificate{
		SerialNumber: big.NewInt(2026),
		Subject: pkix.Name{
			CommonName:   "test.example.com",
			Organization: []string{"Test Org"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"test.example.com", "*.test.example.com"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("创建证书失败: %v", err)
	}

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "fullchain.pem")
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("创建临时文件失败: %v", err)
	}
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	certOut.Close()

	info, err := ParseCertInfo(certPath)
	if err != nil || info == nil {
		t.Fatalf("ParseCertInfo 失败: %v", err)
	}
	if !info["is_valid"].(bool) {
		t.Errorf("证书应该有效")
	}
	if info["days_left"].(int) < 28 || info["days_left"].(int) > 31 {
		t.Errorf("days_left 异常: %v", info["days_left"])
	}
	dnsList, ok := info["dns_names"].([]string)
	if !ok || len(dnsList) != 2 {
		t.Errorf("DNSNames 异常: %v", info["dns_names"])
	}
}

func TestApiCertsHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/certs", nil)
	w := httptest.NewRecorder()
	apiCertsHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("期望状态码 200, 实际得到: %d", resp.StatusCode)
	}
}
