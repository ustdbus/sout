package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// NewInboundSpec 是新建入站的通用参数。
type NewInboundSpec struct {
	Protocol string `json:"protocol"`
	Network  string `json:"network"`
	Port     int    `json:"port"`
	Remark   string `json:"remark"`
	Path     string `json:"path"`
	Host     string `json:"host"`
	Security string `json:"security"`
	Vision   bool   `json:"vision"`

	ServerName string `json:"server_name"`
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`

	Dest        string `json:"dest"`
	ServerNames string `json:"server_names"`
	ShortID     string `json:"short_id"`
	Fingerprint string `json:"fingerprint"`
}

var (
	nativeProtocols  = map[string]bool{"vless": true, "vmess": true, "trojan": true, "shadowsocks": true}
	nativeNetworks   = map[string]bool{"tcp": true, "ws": true, "grpc": true, "httpupgrade": true, "xhttp": true}
	nativeSecurities = map[string]bool{"none": true, "tls": true, "reality": true}
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func visionCapable(proto, network, security string) bool {
	return proto == "vless" && network == "tcp" && (security == "tls" || security == "reality")
}

// normalizedSpec 是 NewInboundSpec 经过校验后的结构
type normalizedSpec struct {
	Protocol string
	Network  string
	Security string
	Port     int
	Path     string
	Host     string
	Remark   string
	Flow     string
}

// normalizeInboundSpec 校验协议组合并补上默认值。
func normalizeInboundSpec(spec NewInboundSpec, used map[int]bool) (*normalizedSpec, error) {
	proto := strings.ToLower(strings.TrimSpace(spec.Protocol))
	if proto == "" {
		proto = "vless"
	}
	if !nativeProtocols[proto] {
		return nil, fmt.Errorf("不支持的协议 %q", spec.Protocol)
	}
	network := strings.ToLower(strings.TrimSpace(spec.Network))
	if network == "" {
		network = "tcp"
	}
	if !nativeNetworks[network] {
		return nil, fmt.Errorf("不支持的传输方式 %q", spec.Network)
	}
	security := strings.ToLower(strings.TrimSpace(spec.Security))
	if security == "" {
		security = "none"
	}
	if !nativeSecurities[security] {
		return nil, fmt.Errorf("不支持的安全层 %q", spec.Security)
	}
	if security == "reality" && network != "tcp" && network != "xhttp" && network != "grpc" {
		return nil, fmt.Errorf("REALITY 不支持 %s 传输", network)
	}

	port := spec.Port
	if port == 0 {
		p, err := freeRandomPort(used)
		if err != nil {
			return nil, err
		}
		port = p
	} else if used[port] {
		return nil, fmt.Errorf("端口 %d 已被别的入站占用", port)
	}

	path := strings.TrimSpace(spec.Path)
	if path == "" {
		switch network {
		case "ws", "httpupgrade", "xhttp":
			path = "/" + randomHex(6)
		case "grpc":
			path = randomHex(6)
		}
	}

	remark := strings.TrimSpace(spec.Remark)
	if remark == "" {
		remark = fmt.Sprintf("%s-%d", proto, port)
	}

	flow := ""
	if spec.Vision {
		if !visionCapable(proto, network, security) {
			return nil, fmt.Errorf("xtls-rprx-vision 只能用于 VLESS + TCP + TLS/REALITY")
		}
		flow = "xtls-rprx-vision"
	}

	return &normalizedSpec{
		Protocol: proto,
		Network:  network,
		Security: security,
		Port:     port,
		Path:     path,
		Host:     strings.TrimSpace(spec.Host),
		Remark:   remark,
		Flow:     flow,
	}, nil
}
