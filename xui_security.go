package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type tlsConfig struct {
	ServerName string
	CertFile   string
	KeyFile    string
	SelfSigned bool
	CertSha256 string
}

type realityConfig struct {
	Dest        string
	ServerNames []string
	PrivateKey  string
	PublicKey   string
	ShortIDs    []string
	Fingerprint string
}

type nativeInbound struct {
	Port     int
	Protocol string
	Network  string
	Path     string
	Host     string
	Security string
	Remark   string
	Enable   bool
	TLS      *tlsConfig
	Reality  *realityConfig
}

func (ib *nativeInbound) netOrTCP() string {
	if ib.Network == "" {
		return "tcp"
	}
	return ib.Network
}

func (ib *nativeInbound) securityOrNone() string {
	if ib.Security == "" {
		return "none"
	}
	return ib.Security
}

func randomShortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func certFingerprint(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", fmt.Errorf("无法解析证书 PEM: %s", certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

func selfSignedCert(dir, serverName string) (certFile, keyFile string, err error) {
	certsDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certsDir, 0700); err != nil {
		return "", "", err
	}
	certFile = filepath.Join(certsDir, "self.crt")
	keyFile = filepath.Join(certsDir, "self.key")
	if _, err := os.Stat(certFile); err == nil {
		if _, err := os.Stat(keyFile); err == nil {
			return certFile, keyFile, nil
		}
	}
	priv, err := exec.Command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", keyFile,
		"-out", certFile, "-days", "3650", "-nodes", "-subj", "/CN="+serverName).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("生成自签证书失败: %s (%w)", string(priv), err)
	}
	return certFile, keyFile, nil
}

func checkRealityDest(dest, serverName string) error {
	conn, err := net.DialTimeout("tcp", dest, 8*time.Second)
	if err != nil {
		return fmt.Errorf("连不上 %s: %w", dest, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	c := tls.Client(conn, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	})
	if err := c.Handshake(); err != nil {
		return fmt.Errorf("%s 的 TLS1.3 握手失败: %w", dest, err)
	}
	return nil
}

func realityKeys(bin string) (priv, pub string, err error) {
	out, err := exec.Command(bin, "x25519").Output()
	if err != nil {
		return "", "", fmt.Errorf("生成 REALITY 密钥失败: %w", err)
	}
	text := string(out)

	rePriv := regexp.MustCompile(`(?i)private\s*key:\s*(\S+)`)
	rePub := regexp.MustCompile(`(?i)(?:password\s*\(publickey\)|public\s*key):\s*(\S+)`)

	mp := rePriv.FindStringSubmatch(text)
	mb := rePub.FindStringSubmatch(text)
	if mp == nil || mb == nil {
		return "", "", fmt.Errorf("无法解析 xray x25519 输出: %s", strings.TrimSpace(text))
	}
	return mp[1], mb[1], nil
}

func buildTLS(dir string, spec NewInboundSpec) (*tlsConfig, error) {
	name := strings.TrimSpace(spec.ServerName)
	if name == "" {
		name = "localhost"
	}
	conf := &tlsConfig{ServerName: name}

	cert, key := strings.TrimSpace(spec.CertFile), strings.TrimSpace(spec.KeyFile)
	if (cert == "") != (key == "") {
		return nil, fmt.Errorf("证书和私钥要成对填写，或者都留空用自签证书")
	}
	if cert != "" && key != "" {
		if _, err := os.Stat(cert); err != nil {
			return nil, fmt.Errorf("证书文件不可读: %w", err)
		}
		if _, err := os.Stat(key); err != nil {
			return nil, fmt.Errorf("私钥文件不可读: %w", err)
		}
		conf.CertFile, conf.KeyFile = cert, key
		return conf, nil
	}

	c, k, err := selfSignedCert(dir, name)
	if err != nil {
		return nil, err
	}
	conf.CertFile, conf.KeyFile, conf.SelfSigned = c, k, true
	fp, err := certFingerprint(c)
	if err != nil {
		return nil, err
	}
	conf.CertSha256 = fp
	return conf, nil
}

func buildReality(xrayBin string, spec NewInboundSpec) (*realityConfig, error) {
	dest := strings.TrimSpace(spec.Dest)
	if dest == "" {
		dest = "www.tesla.com:443"
	}
	if !strings.Contains(dest, ":") {
		dest += ":443"
	}

	var names []string
	for _, s := range strings.Split(spec.ServerNames, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		names = []string{strings.SplitN(dest, ":", 2)[0]}
	}

	priv, pub, err := realityKeys(xrayBin)
	if err != nil {
		return nil, err
	}
	if err := checkRealityDest(dest, names[0]); err != nil {
		return nil, fmt.Errorf("REALITY 目标站点不可用，换一个 dest: %w", err)
	}

	short := strings.TrimSpace(spec.ShortID)
	if short == "" {
		short = randomShortID()
	}
	fp := strings.TrimSpace(spec.Fingerprint)
	if fp == "" {
		fp = "chrome"
	}

	return &realityConfig{
		Dest:        dest,
		ServerNames: names,
		PrivateKey:  priv,
		PublicKey:   pub,
		ShortIDs:    []string{short},
		Fingerprint: fp,
	}, nil
}

func streamSettingsJSON(ib *nativeInbound) map[string]any {
	net := ib.netOrTCP()
	sec := ib.securityOrNone()

	stream := map[string]any{
		"network":  net,
		"security": sec,
	}

	switch net {
	case "ws":
		ws := map[string]any{"path": ib.Path}
		if ib.Host != "" {
			ws["headers"] = map[string]string{"Host": ib.Host}
		}
		stream["wsSettings"] = ws
	case "httpupgrade":
		hu := map[string]any{"path": ib.Path}
		if ib.Host != "" {
			hu["host"] = ib.Host
		}
		stream["httpupgradeSettings"] = hu
	case "xhttp":
		xh := map[string]any{"path": ib.Path}
		if ib.Host != "" {
			xh["host"] = ib.Host
		}
		stream["xhttpSettings"] = xh
	case "grpc":
		stream["grpcSettings"] = map[string]any{
			"serviceName": strings.TrimPrefix(ib.Path, "/"),
			"multiMode":   true,
		}
	}

	switch sec {
	case "tls":
		if ib.TLS != nil {
			t := map[string]any{
				"serverName":   ib.TLS.ServerName,
				"certificates": []any{map[string]any{"certificateFile": ib.TLS.CertFile, "keyFile": ib.TLS.KeyFile}},
			}
			stream["tlsSettings"] = t
		}
	case "reality":
		if ib.Reality != nil {
			r := map[string]any{
				"dest":        ib.Reality.Dest,
				"serverNames": ib.Reality.ServerNames,
				"privateKey":  ib.Reality.PrivateKey,
				"shortIds":    ib.Reality.ShortIDs,
				"fingerprint": ib.Reality.Fingerprint,
			}
			stream["realitySettings"] = r
		}
	}

	return stream
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func findXray(workDir string) (string, error) {
	for _, bin := range []string{
		"/usr/local/bin/xray",
		"/usr/bin/xray",
		"/usr/local/x-ui/bin/xray-linux-amd64",
		"/usr/local/x-ui/bin/xray-linux-arm64",
		filepath.Join(workDir, "bin", "xray"),
	} {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	if p, err := exec.LookPath("xray"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 xray 可执行文件")
}
