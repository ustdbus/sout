package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SocksCred 是一条隧道的 SOCKS5 访问凭据。
//
// 每条隧道一套独立凭据：泄露一条不会连累其他出口，
// 换节点时也能只重置这一条而不影响已分发的其他配置。
type SocksCred struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

// Tunnel 是一条运行中的隧道：一个 netns + 一个 openvpn 进程 + 一个本地 SOCKS5 端口。
type Tunnel struct {
	Slot       int       `json:"slot"`
	Port       int       `json:"port"`
	Node       Node      `json:"node"`
	Status     string    `json:"status"` // starting | up | failed | stopped
	ExitIP     string    `json:"exit_ip"`
	Err        string    `json:"err,omitempty"`
	Since      time.Time `json:"since"`
	Cred       SocksCred `json:"cred"`
	Kind       string    `json:"kind,omitempty"` // "vpngate" | "custom"
	IPType     string    `json:"ip_type,omitempty"` // "residential" | "datacenter"
	ISP        string    `json:"isp,omitempty"`
	CustomHost string    `json:"custom_host,omitempty"`
	CustomPort int       `json:"custom_port,omitempty"`
	CustomUser   string    `json:"custom_user,omitempty"`
	CustomPass   string    `json:"custom_pass,omitempty"`
	TargetPoolType string    `json:"target_pool_type,omitempty"` // 用户指定的 "residential" | "datacenter" | "all"
	TargetRegion   string    `json:"target_region,omitempty"`    // 用户指定的国家代码或源 (如 "US", "SRC:hookzof", "ALL")
	TargetSourceID string    `json:"target_source_id,omitempty"` // 用户指定的源 ID (如 "builtin-vpngate", "src-xxx")
	HistoryHosts   []string  `json:"history_hosts,omitempty"`

	ns       string
	listener net.Listener
	ovpn     *exec.Cmd
	mu       sync.Mutex
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

func (t *Tunnel) nsName() string { return fmt.Sprintf("fo%d", t.Slot) }
func (t *Tunnel) subnet() string { return fmt.Sprintf("10.99.%d", t.Slot) }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runQuiet 执行清理类命令，忽略"本来就不存在"这类错误。
func runQuiet(name string, args ...string) {
	_ = exec.Command(name, args...).Run()
}

// setupNetns 建立 netns 与 veth 链路，并配好 NAT 与转发放行。
func (t *Tunnel) setupNetns() error {
	ns, sub := t.nsName(), t.subnet()
	veth, peer := fmt.Sprintf("fov%d", t.Slot), fmt.Sprintf("fop%d", t.Slot)

	t.teardownNetns()

	if err := run("ip", "netns", "add", ns); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up"); err != nil {
		return err
	}
	if err := run("ip", "link", "add", veth, "type", "veth", "peer", "name", peer); err != nil {
		return err
	}
	if err := run("ip", "link", "set", peer, "netns", ns); err != nil {
		return err
	}
	if err := run("ip", "addr", "add", sub+".1/30", "dev", veth); err != nil {
		return err
	}
	if err := run("ip", "link", "set", veth, "up"); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "addr", "add", sub+".2/30", "dev", peer); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "link", "set", peer, "up"); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "route", "add", "default", "via", sub+".1"); err != nil {
		return err
	}

	// netns 内的 DNS，仅用于 openvpn 解析远端主机名
	nsDir := filepath.Join("/etc/netns", ns)
	if err := os.MkdirAll(nsDir, 0755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", nsDir, err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, "resolv.conf"), []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
		return fmt.Errorf("写 resolv.conf 失败: %w", err)
	}

	cidr := sub + ".0/30"
	ensureRule("nat", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	ensureRuleInsert("filter", "FORWARD", "-s", cidr, "-j", "ACCEPT")
	ensureRuleInsert("filter", "FORWARD", "-d", cidr, "-j", "ACCEPT")
	return nil
}

// ensureRule 幂等追加一条 iptables 规则。
func ensureRule(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if exec.Command("iptables", check...).Run() == nil {
		return
	}
	add := append([]string{"-w", "5", "-t", table, "-A", chain}, spec...)
	runQuiet("iptables", add...)
}

// ensureRuleInsert 幂等插入规则到链首。
// FORWARD 链末尾常有兜底 REJECT，必须插到最前面才生效。
func ensureRuleInsert(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if exec.Command("iptables", check...).Run() == nil {
		return
	}
	ins := append([]string{"-w", "5", "-t", table, "-I", chain, "1"}, spec...)
	runQuiet("iptables", ins...)
}

func (t *Tunnel) teardownNetns() {
	ns, sub := t.nsName(), t.subnet()
	cidr := sub + ".0/30"
	runQuiet("ip", "netns", "del", ns)
	runQuiet("ip", "link", "del", fmt.Sprintf("fov%d", t.Slot))
	runQuiet("iptables", "-w", "5", "-t", "nat", "-D", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	runQuiet("iptables", "-w", "5", "-D", "FORWARD", "-s", cidr, "-j", "ACCEPT")
	runQuiet("iptables", "-w", "5", "-D", "FORWARD", "-d", cidr, "-j", "ACCEPT")
}

// startOpenVPN 在 netns 内拉起 openvpn，并等待 tun0 拿到地址。
func (t *Tunnel) startOpenVPN(dir string) error {
	ns := t.nsName()
	cfgPath := filepath.Join(dir, ns+".ovpn")
	if err := os.WriteFile(cfgPath, []byte(t.Node.Config), 0600); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}
	authPath := filepath.Join(dir, "auth.txt")
	if err := os.WriteFile(authPath, []byte("vpn\nvpn\n"), 0600); err != nil {
		return fmt.Errorf("写凭据失败: %w", err)
	}

	logPath := filepath.Join(dir, ns+".log")
	cmd := exec.Command("ip", "netns", "exec", ns, "openvpn",
		"--config", cfgPath,
		"--auth-user-pass", authPath,
		"--auth-nocache",
		"--dev", "tun0",
		"--connect-retry-max", "2",
		"--connect-timeout", "20",
		"--data-ciphers", "AES-128-CBC:AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305",
		"--verb", "3",
		"--log", logPath,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 openvpn 失败: %w", err)
	}
	t.ovpn = cmd
	go cmd.Wait() // 回收子进程，避免僵尸

	// openvpn 建好 tun0 前 SOCKS5 无法正常出网，这里等它就绪
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := exec.Command("ip", "netns", "exec", ns, "ip", "-4", "addr", "show", "tun0").Output(); err == nil {
			if strings.Contains(string(out), "inet ") {
				return nil
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("openvpn 提前退出，详见 %s", logPath)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("等待 tun0 就绪超时，详见 %s", logPath)
}

// serve 在母机上监听 SOCKS5 端口，出站连接则在 netns 内建立。
// 监听必须留在母机侧：netns 内的 loopback 与母机彼此独立，
// 监听在 netns 里的话外部根本连不上。
func (t *Tunnel) serve() error {
	// 端口要尽量保持不变，否则用户已经分发出去的客户端配置会失效。
	// 进程刚重启时旧监听可能还在 TIME_WAIT，这里给几秒重试窗口。
	var ln net.Listener
	var err error
	for i := 0; i < 6; i++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", t.Port))
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		// 确实被别的进程长期占用了，才换端口
		port, perr := freeRandomPort(map[int]bool{t.Port: true})
		if perr != nil {
			return fmt.Errorf("监听 %d 失败且无备用端口: %w", t.Port, err)
		}
		ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return fmt.Errorf("监听 %d 失败: %w", port, err)
		}
		t.Port = port
	}
	t.listener = ln
	dial := dialerInNetns(t.nsName())

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 每次连接现取凭据：改口令后不必重建监听，新连接立刻按新凭据校验
			cred := t.credential()
			go serveSocks(conn, &cred, dial)
		}
	}()
	return nil
}

// credential 取一份凭据副本，避免读写并发。
func (t *Tunnel) credential() SocksCred {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Cred
}

// setCredential 换掉这条隧道的 SOCKS5 凭据。已建立的连接不受影响，
// 新连接立即按新凭据校验。
func (t *Tunnel) setCredential(c SocksCred) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Cred = c
}

// startCustom 启动自定义 SOCKS5 转发隧道
func (t *Tunnel) startCustom() error {
	t.mu.Lock()
	if t.listener != nil {
		_ = t.listener.Close()
		t.listener = nil
	}
	t.Status = "starting"
	t.Err = "连接中..."
	t.Since = time.Now()
	t.mu.Unlock()

	var ln net.Listener
	var err error
	for i := 0; i < 6; i++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", t.Port))
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.mu.Lock()
		t.Status = "failed"
		t.Err = fmt.Sprintf("监听端口 %d 失败: %v", t.Port, err)
		t.mu.Unlock()
		return fmt.Errorf("监听本地端口 %d 失败: %w", t.Port, err)
	}
	t.listener = ln

	remoteAddr := fmt.Sprintf("%s:%d", t.CustomHost, t.CustomPort)
	dial := func(network, targetAddr string) (net.Conn, error) {
		return dialSocks5(remoteAddr, t.CustomUser, t.CustomPass, targetAddr, 15*time.Second)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			cred := t.credential()
			go serveSocks(conn, &cred, dial)
		}
	}()

	// 探测节点真实连通性与出口 IP
	exitIP, ping, ipType, isp, err := ProbeCustomSocks(remoteAddr, t.CustomUser, t.CustomPass, 8*time.Second)
	if err != nil {
		t.mu.Lock()
		t.Status = "failed"
		t.Err = fmt.Sprintf("节点连接失败: %v", err)
		t.mu.Unlock()
		return err
	}

	// 严格属性二验：如果用户明确指定了家宽或机房池，但实测结果不符，则判定失败并触发下一个候选节点
	if t.TargetPoolType == "residential" && ipType == "datacenter" {
		t.mu.Lock()
		t.Status = "failed"
		t.Err = "节点实测为机房 IP (与目标家宽池不符)"
		t.mu.Unlock()
		return fmt.Errorf("节点实测为机房 IP (与目标家宽池不符): %s", exitIP)
	}
	if t.TargetPoolType == "datacenter" && ipType == "residential" {
		t.mu.Lock()
		t.Status = "failed"
		t.Err = "节点实测为家宽 IP (与目标机房池不符)"
		t.mu.Unlock()
		return fmt.Errorf("节点实测为家宽 IP (与目标机房池不符): %s", exitIP)
	}

	t.mu.Lock()
	t.Status = "up"
	t.ExitIP = exitIP
	t.Err = ""
	if ping > 0 {
		t.Node.Ping = ping
	}
	if ipType != "" {
		t.IPType = ipType
		t.Node.IPType = ipType
	}
	if isp != "" {
		t.ISP = isp
		t.Node.ISP = isp
	}
	t.mu.Unlock()

	return nil
}

// switchPort 动态切换隧道 SOCKS5 监听端口
func (t *Tunnel) switchPort(newPort int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if newPort == t.Port && t.listener != nil {
		return nil
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", newPort))
	if err != nil {
		return fmt.Errorf("无法监听端口 %d: %w", newPort, err)
	}

	oldLn := t.listener
	t.listener = ln
	t.Port = newPort

	if oldLn != nil {
		_ = oldLn.Close()
	}

	var dial func(network, targetAddr string) (net.Conn, error)
	if t.Kind == "custom" {
		remoteAddr := fmt.Sprintf("%s:%d", t.CustomHost, t.CustomPort)
		dial = func(network, targetAddr string) (net.Conn, error) {
			return dialSocks5(remoteAddr, t.CustomUser, t.CustomPass, targetAddr, 15*time.Second)
		}
	} else {
		dial = dialerInNetns(t.nsName())
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			cred := t.credential()
			go serveSocks(conn, &cred, dial)
		}
	}()

	return nil
}

// probeExitIP 通过隧道查询出口 IP，用于确认这条隧道确实换了 IP。
func (t *Tunnel) probeExitIP() (string, error) {
	if t.Kind == "custom" {
		remoteAddr := fmt.Sprintf("%s:%d", t.CustomHost, t.CustomPort)
		ip, _, _, _, err := ProbeCustomSocks(remoteAddr, t.CustomUser, t.CustomPass, 10*time.Second)
		return ip, err
	}
	out, err := exec.Command("ip", "netns", "exec", t.nsName(),
		"curl", "-s", "--max-time", "15", "http://api.ipify.org").Output()
	if err != nil {
		return "", fmt.Errorf("查询出口 IP 失败: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("出口 IP 返回异常: %q", ip)
	}
	return ip, nil
}

// stop 停止这条隧道并清理它占用的所有资源。
func (t *Tunnel) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.listener != nil {
		t.listener.Close()
		t.listener = nil
	}
	if t.ovpn != nil && t.ovpn.Process != nil {
		_ = t.ovpn.Process.Kill()
		t.ovpn = nil
	}
	if t.Kind != "custom" {
		t.teardownNetns()
	}
	t.Status = "stopped"
}
