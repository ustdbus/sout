package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SocksCred 是一条隧道的 SOCKS5 访问凭据。
type SocksCred struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

// Tunnel 是一条运行中的隧道。
// 在用户态架构下，VPN Gate 出口由内嵌的 sing-box 1.14 (gVisor) 提供免 TUN/免 root 的 OpenVPN endpoint 与本地认证 SOCKS5 监听。
type Tunnel struct {
	Slot           int       `json:"slot"`
	Port           int       `json:"port"`
	Node           Node      `json:"node"`
	Status         string    `json:"status"` // starting | up | failed | stopped
	ExitIP         string    `json:"exit_ip"`
	Err            string    `json:"err,omitempty"`
	Since          time.Time `json:"since"`
	Cred           SocksCred `json:"cred"`
	Kind           string    `json:"kind,omitempty"`    // "vpngate" | "custom"
	IPType         string    `json:"ip_type,omitempty"` // "residential" | "datacenter"
	ISP            string    `json:"isp,omitempty"`
	CustomHost     string    `json:"custom_host,omitempty"`
	CustomPort     int       `json:"custom_port,omitempty"`
	CustomUser     string    `json:"custom_user,omitempty"`
	CustomPass     string    `json:"custom_pass,omitempty"`
	TargetPoolType string    `json:"target_pool_type,omitempty"` // "residential" | "datacenter" | "all"
	TargetRegion   string    `json:"target_region,omitempty"`    // 国家代码或源 (如 "US", "JP", "ALL")
	TargetSourceID string    `json:"target_source_id,omitempty"` // 源 ID
	HistoryHosts   []string  `json:"history_hosts,omitempty"`

	engine   *embeddedEngine
	listener net.Listener
	mu       sync.Mutex
}

func (t *Tunnel) setEngine(engine *embeddedEngine) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.engine = engine
}

func (t *Tunnel) credential() SocksCred {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Cred
}

func (t *Tunnel) setCredential(c SocksCred) error {
	t.mu.Lock()
	t.Cred = c
	engine := t.engine
	kind := t.Kind
	t.mu.Unlock()

	if kind != "custom" && engine != nil {
		return engine.addTunnel(t)
	}
	return nil
}

func (t *Tunnel) switchPort(newPort int) error {
	t.mu.Lock()
	if t.Port == newPort {
		t.mu.Unlock()
		return nil
	}
	t.Port = newPort
	engine := t.engine
	kind := t.Kind
	t.mu.Unlock()

	if kind == "custom" {
		t.stop()
		return t.startCustom()
	}
	if engine != nil {
		return engine.addTunnel(t)
	}
	return nil
}

func (t *Tunnel) recordHost(oldHost string) {
	if oldHost == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range t.HistoryHosts {
		if h == oldHost {
			return
		}
	}
	t.HistoryHosts = append(t.HistoryHosts, oldHost)
}

func (t *Tunnel) start(dir string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.Status = "starting"
	t.Err = ""

	if t.Kind == "custom" {
		return t.startCustom()
	}

	// VPN Gate 用户态 OpenVPN 隧道
	if t.engine == nil {
		t.Status = "failed"
		t.Err = "内嵌 sing-box 引擎未初始化"
		return fmt.Errorf("%s", t.Err)
	}

	if err := t.engine.addTunnel(t); err != nil {
		t.Status = "failed"
		t.Err = fmt.Sprintf("启动用户态出口失败: %v", err)
		return err
	}

	// 探测出口真实 IP
	exitIP, err := t.probeExitIP()
	if err != nil {
		t.engine.removeTunnel(t)
		t.Status = "failed"
		t.Err = fmt.Sprintf("探测出口 IP 失败: %v", err)
		return err
	}

	t.ExitIP = exitIP
	t.Status = "up"
	t.Since = time.Now()
	t.Err = ""
	return nil
}

func (t *Tunnel) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.Kind == "custom" {
		if t.listener != nil {
			_ = t.listener.Close()
			t.listener = nil
		}
	} else {
		if t.engine != nil {
			t.engine.removeTunnel(t)
		}
	}

	t.Status = "stopped"
	t.ExitIP = ""
}

func (t *Tunnel) probeExitIP() (string, error) {
	if t.Kind == "custom" {
		return t.probeCustomExitIP()
	}

	// VPN Gate 出口通过用户态 sing-box 拨号探测
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	// 尝试多个 IP 查询接口
	for _, target := range []string{"api.ipify.org:80", "ifconfig.me:80", "icanhazip.com:80"} {
		conn, err := t.engine.dialTunnel(ctx, t, "tcp", target)
		if err != nil {
			continue
		}

		host := strings.Split(target, ":")[0]
		req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUser-Agent: curl/7.88.1\r\nAccept: */*\r\nConnection: close\r\n\r\n", host)
		_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			continue
		}

		resp, err := io.ReadAll(conn)
		conn.Close()
		if err != nil {
			continue
		}

		lines := strings.Split(string(resp), "\r\n\r\n")
		if len(lines) >= 2 {
			body := strings.TrimSpace(lines[1])
			if ip := net.ParseIP(body); ip != nil && ip.To4() != nil {
				return ip.String(), nil
			}
		}
	}

	return "", fmt.Errorf("所有出口 IP 探测源均无有效 IPv4 响应")
}

func (t *Tunnel) probeCustomExitIP() (string, error) {
	// 自定义 SOCKS5 代理通过本地监听端口建立 HTTP 客户端探测
	proxyURL, err := url.Parse(fmt.Sprintf("socks5://%s:%s@127.0.0.1:%d", t.Cred.User, t.Cred.Pass, t.Port))
	if err != nil {
		return "", err
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   6 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	for _, u := range []string{"http://api.ipify.org", "http://ifconfig.me", "http://icanhazip.com"} {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		ipStr := strings.TrimSpace(string(body))
		if ip := net.ParseIP(ipStr); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("自定义 SOCKS5 探测出口 IP 失败")
}

func (t *Tunnel) startCustom() error {
	addr := fmt.Sprintf("127.0.0.1:%d", t.Port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Status = "failed"
		t.Err = fmt.Sprintf("监听端口 %d 失败: %v", t.Port, err)
		return err
	}
	t.listener = l

	targetAddr := fmt.Sprintf("%s:%d", t.CustomHost, t.CustomPort)
	go func() {
		for {
			clientConn, err := l.Accept()
			if err != nil {
				return
			}
			go t.handleCustomForward(clientConn, targetAddr)
		}
	}()

	exitIP, err := t.probeExitIP()
	if err != nil {
		_ = l.Close()
		t.listener = nil
		t.Status = "failed"
		t.Err = fmt.Sprintf("探测出口 IP 失败: %v", err)
		return err
	}

	t.ExitIP = exitIP
	t.Status = "up"
	t.Since = time.Now()
	t.Err = ""
	return nil
}

func (t *Tunnel) handleCustomForward(clientConn net.Conn, targetAddr string) {
	defer clientConn.Close()

	// 1. 本地 SOCKS5 认证握手
	buf := make([]byte, 256)
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil || buf[0] != 0x05 {
		return
	}
	nmethods := int(buf[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(clientConn, methods); err != nil {
		return
	}

	// 检查是否需要用户名密码认证
	if t.Cred.User != "" && t.Cred.Pass != "" {
		clientConn.Write([]byte{0x05, 0x02}) // USER/PASS

		authBuf := make([]byte, 512)
		if _, err := io.ReadFull(clientConn, authBuf[:2]); err != nil || authBuf[0] != 0x01 {
			return
		}
		ulen := int(authBuf[1])
		if _, err := io.ReadFull(clientConn, authBuf[2:2+ulen]); err != nil {
			return
		}
		user := string(authBuf[2 : 2+ulen])

		plenOffset := 2 + ulen
		if _, err := io.ReadFull(clientConn, authBuf[plenOffset:plenOffset+1]); err != nil {
			return
		}
		plen := int(authBuf[plenOffset])
		passOffset := plenOffset + 1
		if _, err := io.ReadFull(clientConn, authBuf[passOffset:passOffset+plen]); err != nil {
			return
		}
		pass := string(authBuf[passOffset : passOffset+plen])

		if user != t.Cred.User || pass != t.Cred.Pass {
			clientConn.Write([]byte{0x01, 0x01}) // 认证失败
			return
		}
		clientConn.Write([]byte{0x01, 0x00}) // 认证成功
	} else {
		clientConn.Write([]byte{0x05, 0x00}) // NO AUTH
	}

	// 2. 读取客户端请求
	reqBuf := make([]byte, 4)
	if _, err := io.ReadFull(clientConn, reqBuf); err != nil || reqBuf[0] != 0x05 || reqBuf[1] != 0x01 {
		return // 仅支持 CONNECT
	}

	var dstHost string
	switch reqBuf[3] {
	case 0x01: // IPv4
		ipBuf := make([]byte, 4)
		if _, err := io.ReadFull(clientConn, ipBuf); err != nil {
			return
		}
		dstHost = net.IP(ipBuf).String()
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(clientConn, lenBuf); err != nil {
			return
		}
		dBuf := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(clientConn, dBuf); err != nil {
			return
		}
		dstHost = string(dBuf)
	case 0x04: // IPv6
		ipBuf := make([]byte, 16)
		if _, err := io.ReadFull(clientConn, ipBuf); err != nil {
			return
		}
		dstHost = net.IP(ipBuf).String()
	default:
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, portBuf); err != nil {
		return
	}
	dstPort := int(portBuf[0])<<8 | int(portBuf[1])

	// 3. 连接远端上游 SOCKS5 并完成转发
	targetConn, err := net.DialTimeout("tcp", targetAddr, 8*time.Second)
	if err != nil {
		clientConn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // Host unreachable
		return
	}
	defer targetConn.Close()

	if err := socks5ClientHandshake(targetConn, t.CustomUser, t.CustomPass, dstHost, dstPort); err != nil {
		clientConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// 响应客户端 CONNECT 成功
	clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	// 4. 双向流量转发
	go io.Copy(targetConn, clientConn)
	io.Copy(clientConn, targetConn)
}
