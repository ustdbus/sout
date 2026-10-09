package main

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// publicIPSources 是几个只回一行纯 IPv4 的接口，任意一个先返回就用它。
var publicIPSources = []string{
	"https://checkip.amazonaws.com",
	"http://checkip.amazonaws.com",
	"https://ipv4.icanhazip.com",
	"https://ifconfig.me/ip",
	"https://api.ipify.org",
}

var (
	publicIPMu         sync.Mutex
	publicIPOverride   string    // 由 -ip / FANOUT_PUBLIC_IP 显式指定，优先级最高
	publicIPCache      string    // 上一次探测成功的结果
	publicIPAt         time.Time // 上次探测时间，用于 TTL
	publicIPNegativeAt time.Time // 上次探测失败时间，用于负缓存 TTL
)

const (
	publicIPTTL         = 30 * time.Minute
	publicIPNegativeTTL = 5 * time.Minute
)

// hasIPv4Route 探测本机是否存在到公网 IPv4 的有效路由（Linux 内核 FIB 毫秒级返回）
func hasIPv4Route() bool {
	conn, err := net.DialTimeout("udp4", "8.8.8.8:53", 50*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// hasIPv6Route 探测本机是否存在到公网 IPv6 的有效路由
func hasIPv6Route() bool {
	conn, err := net.DialTimeout("udp6", "[2001:4860:4860::8888]:53", 50*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// detectNetworkMode 识别母机的网络栈架构：
// "ipv6_only" (纯 IPv6) | "dual_stack" (双栈) | "ipv4_only" (纯 IPv4)
func detectNetworkMode() string {
	for _, p := range []string{"/var/lib/sout/network_mode", "/var/lib/sout/network_stack"} {
		if data, err := os.ReadFile(p); err == nil {
			m := strings.TrimSpace(string(data))
			if m == "ipv6_only" || m == "dual_stack" || m == "ipv4_only" {
				return m
			}
		}
	}
	v4 := hasIPv4Route()
	v6 := hasIPv6Route()
	if !v4 && v6 {
		return "ipv6_only"
	}
	if v4 && v6 {
		return "dual_stack"
	}
	return "ipv4_only"
}

// setPublicIPOverride 记录用户显式指定的母机公网地址，空值表示不覆盖。
func setPublicIPOverride(ip string) {
	publicIPMu.Lock()
	publicIPOverride = strings.TrimSpace(ip)
	publicIPMu.Unlock()
}

// hostPublicIP 返回跑 fanout 这台母机的公网 IPv4。
// 优先用显式覆盖值；否则用缓存（未过期）；再否则对外探测一次。
// 探测不到就返回空串，由调用方决定兜底。
func hostPublicIP() string {
	// 纯 IPv6 独立隔离：立即返回空串，坚决不进行任何耗时的外部 IPv4 探测
	if detectNetworkMode() == "ipv6_only" {
		return ""
	}

	publicIPMu.Lock()
	if publicIPOverride != "" {
		ip := publicIPOverride
		publicIPMu.Unlock()
		return ip
	}
	if publicIPCache != "" && time.Since(publicIPAt) < publicIPTTL {
		ip := publicIPCache
		publicIPMu.Unlock()
		return ip
	}
	if publicIPCache == "" && time.Since(publicIPNegativeAt) < publicIPNegativeTTL {
		publicIPMu.Unlock()
		return ""
	}
	publicIPMu.Unlock()

	// 优先从安装时记录的持久化文件读取
	for _, p := range []string{"/var/lib/sout/host_ipv4", "/var/lib/sout/host_ip"} {
		if data, err := os.ReadFile(p); err == nil {
			val := strings.TrimSpace(string(data))
			if parsed := net.ParseIP(val); parsed != nil && parsed.To4() != nil {
				publicIPMu.Lock()
				publicIPCache = parsed.String()
				publicIPAt = time.Now()
				publicIPMu.Unlock()
				return parsed.String()
			}
		}
	}

	// 毫秒级网络栈预检：无 IPv4 路由直接判定为无 IPv4，绝不触发外部 curl -4 耗时超时
	if !hasIPv4Route() {
		publicIPMu.Lock()
		publicIPNegativeAt = time.Now()
		publicIPMu.Unlock()
		return ""
	}

	ip := probePublicIP()
	publicIPMu.Lock()
	if ip == "" {
		publicIPNegativeAt = time.Now()
		ip = publicIPCache
	} else {
		publicIPCache = ip
		publicIPAt = time.Now()
	}
	publicIPMu.Unlock()
	return ip
}

// probePublicIP 逐个问外部接口，拿到第一个合法的 IPv4 就返回。
func probePublicIP() string {
	for _, url := range publicIPSources {
		out, err := exec.Command("curl", "-4", "-s", "--max-time", "2", url).Output()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(out))
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return ip
		}
	}
	return ""
}

// publicIPv6Sources 是只回一行纯 IPv6 的接口。
var publicIPv6Sources = []string{
	"https://ipv6.icanhazip.com",
	"https://api64.ipify.org",
	"https://v6.ident.me",
	"https://ifconfig.co/ip",
}

var (
	publicIPv6Mu         sync.Mutex
	publicIPv6Override   string
	publicIPv6Cache      string
	publicIPv6At         time.Time
	publicIPv6NegativeAt time.Time
)

// setPublicIPv6Override 记录用户显式指定的母机公网 IPv6，空值表示不覆盖。
func setPublicIPv6Override(ip string) {
	publicIPv6Mu.Lock()
	publicIPv6Override = strings.TrimSpace(ip)
	publicIPv6Mu.Unlock()
}

// hostPublicIPv6 返回本机公网 IPv6；没有 IPv6 出口时返回空串（调用方据此决定是否回显）。
func hostPublicIPv6() string {
	publicIPv6Mu.Lock()
	if publicIPv6Override != "" {
		ip := publicIPv6Override
		publicIPv6Mu.Unlock()
		return ip
	}
	if publicIPv6Cache != "" && time.Since(publicIPv6At) < publicIPTTL {
		cached := publicIPv6Cache
		publicIPv6Mu.Unlock()
		return cached
	}
	if publicIPv6Cache == "" && time.Since(publicIPv6NegativeAt) < publicIPNegativeTTL {
		publicIPv6Mu.Unlock()
		return ""
	}
	publicIPv6Mu.Unlock()

	// 优先从安装时记录的持久化文件读取
	for _, p := range []string{"/var/lib/sout/host_ipv6", "/var/lib/sout/host_ip"} {
		if data, err := os.ReadFile(p); err == nil {
			val := strings.TrimSpace(string(data))
			if parsed := net.ParseIP(val); parsed != nil && parsed.To4() == nil && parsed.To16() != nil {
				publicIPv6Mu.Lock()
				publicIPv6Cache = parsed.String()
				publicIPv6At = time.Now()
				publicIPv6Mu.Unlock()
				return parsed.String()
			}
		}
	}

	if !hasIPv6Route() {
		publicIPv6Mu.Lock()
		publicIPv6NegativeAt = time.Now()
		publicIPv6Mu.Unlock()
		return ""
	}

	ip := probePublicIPv6()
	publicIPv6Mu.Lock()
	if ip == "" {
		publicIPv6NegativeAt = time.Now()
		ip = publicIPv6Cache
	} else {
		publicIPv6Cache = ip
		publicIPv6At = time.Now()
	}
	publicIPv6Mu.Unlock()
	return ip
}

// hostConnectIP 返回母机公网直连 IP。
// 双栈或纯 IPv4 环境返回 IPv4，纯 IPv6 环境返回 IPv6。
func hostConnectIP() string {
	mode := detectNetworkMode()
	if mode == "ipv6_only" {
		if v6 := hostPublicIPv6(); v6 != "" {
			return v6
		}
		for _, p := range []string{"/var/lib/sout/host_ipv6", "/var/lib/sout/host_ip"} {
			if data, err := os.ReadFile(p); err == nil {
				val := strings.TrimSpace(string(data))
				if val != "" && val != "127.0.0.1" {
					return val
				}
			}
		}
		return ""
	}

	// 优先直接利用持久化文件中记录的地址（已校验存在），避免页面打开时做网络探测
	for _, p := range []string{"/var/lib/sout/host_ipv4", "/var/lib/sout/host_ip", "/var/lib/sout/host_ipv6"} {
		if data, err := os.ReadFile(p); err == nil {
			val := strings.TrimSpace(string(data))
			if parsed := net.ParseIP(val); parsed != nil && !parsed.IsLoopback() {
				if parsed.To4() != nil {
					return parsed.String()
				}
			}
		}
	}

	if ip := hostPublicIP(); ip != "" && ip != "127.0.0.1" {
		return ip
	}
	if v6 := hostPublicIPv6(); v6 != "" {
		return v6
	}
	// 尝试从持久化文件兜底（包括 IPv6）
	for _, p := range []string{"/var/lib/sout/host_ip", "/var/lib/sout/host_ipv6"} {
		if data, err := os.ReadFile(p); err == nil {
			val := strings.TrimSpace(string(data))
			if val != "" && val != "127.0.0.1" {
				return val
			}
		}
	}
	return ""
}

// probePublicIPv6 逐个问外部接口，拿到第一个合法的 IPv6 就返回。
// 无 IPv6 出口时 curl 会超时/报错，这里直接跳过，不影响 IPv4 流程。
func probePublicIPv6() string {
	for _, url := range publicIPv6Sources {
		out, err := exec.Command("curl", "-6", "-s", "--max-time", "2", url).Output()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(out))
		parsed := net.ParseIP(ip)
		if parsed != nil && parsed.To4() == nil && parsed.To16() != nil {
			return parsed.String()
		}
	}
	return ""
}
