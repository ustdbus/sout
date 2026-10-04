package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GatewayMeta 代表轻量网关分流元数据结构
type GatewayMeta struct {
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"`
	Domain      string `json:"domain"`
	TunnelToken string `json:"tunnel_token"`
	TunnelPort  int    `json:"tunnel_port"`
	SoutPort    int    `json:"sout_port"`
	SoutPath    string `json:"sout_path"`
	SuiPort     int    `json:"sui_port"`
	SuiPath     string `json:"sui_path"`
	SuiUser     string `json:"sui_user"`
	SubPort     int    `json:"sub_port"`
	SubPath     string `json:"sub_path"`
	NodePort    int    `json:"node_port"`
	WsPath      string `json:"ws_path"`
	Protocol    string `json:"protocol"`
}

// CaddyMeta 保持向下兼容
type CaddyMeta = GatewayMeta

type GatewayManager struct {
	mu      sync.Mutex
	workDir string
	servers []*http.Server
	port    int
	meta    *GatewayMeta
}

var globalGateway = &GatewayManager{}

// InitGateway 在主程序启动时初始化并拉起轻量反代网关
func InitGateway(workDir string) {
	globalGateway.mu.Lock()
	globalGateway.workDir = workDir
	globalGateway.mu.Unlock()

	ReloadGateway(workDir)
}

// ShutdownGateway 优雅关闭网关
func ShutdownGateway() {
	globalGateway.mu.Lock()
	defer globalGateway.mu.Unlock()
	stopGatewayLocked()
	log.Println("[Gateway] 内置轻量反代网关已关闭")
}

// ReloadGateway 根据 gateway_meta.json / caddy_meta.json 动态重载或重启网关
func ReloadGateway(workDir string) {
	globalGateway.mu.Lock()
	defer globalGateway.mu.Unlock()

	if workDir == "" {
		workDir = "/var/lib/sout"
	}
	globalGateway.workDir = workDir

	metaPath := filepath.Join(workDir, "gateway_meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		metaPath = filepath.Join(workDir, "caddy_meta.json")
		data, err = os.ReadFile(metaPath)
	}
	if err != nil {
		stopGatewayLocked()
		return
	}

	var meta GatewayMeta
	if err := json.Unmarshal(data, &meta); err != nil || !meta.Enabled || meta.TunnelPort <= 0 {
		stopGatewayLocked()
		return
	}

	// 规范化路径，移除多余的前导和末尾斜杠
	meta.SoutPath = strings.Trim(meta.SoutPath, "/")
	meta.SuiPath = strings.Trim(meta.SuiPath, "/")
	meta.SubPath = strings.Trim(meta.SubPath, "/")
	meta.WsPath = strings.Trim(meta.WsPath, "/")

	globalGateway.meta = &meta

	// 如果已经在运行且端口一致，只需更新路由处理器；若端口变化则重启监听
	handler := buildGatewayHandler(&meta, workDir)
	if len(globalGateway.servers) > 0 && globalGateway.port == meta.TunnelPort {
		for _, s := range globalGateway.servers {
			s.Handler = handler
		}
		log.Printf("[Gateway] 内置轻量反代网关路由已热更新 (Port: %d)", meta.TunnelPort)
		return
	}

	stopGatewayLocked()

	// 同时监听 IPv4 (127.0.0.1) 与 IPv6 ([::1])，防止 cloudflared 解析 localhost 时命中 ::1 报 502
	var listeners []net.Listener
	if ln4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", meta.TunnelPort)); err == nil {
		listeners = append(listeners, ln4)
	} else {
		log.Printf("[Gateway] 监听 IPv4 回源端口失败 (127.0.0.1:%d): %v", meta.TunnelPort, err)
	}

	if ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", meta.TunnelPort)); err == nil {
		listeners = append(listeners, ln6)
	}

	if len(listeners) == 0 {
		log.Printf("[Gateway] 无法监听任何本地回源端口 (%d)", meta.TunnelPort)
		return
	}

	globalGateway.port = meta.TunnelPort
	for _, ln := range listeners {
		srv := &http.Server{
			Handler: handler,
		}
		globalGateway.servers = append(globalGateway.servers, srv)
		go func(l net.Listener, s *http.Server) {
			log.Printf("[Gateway] 内置轻量反代网关已成功接管 %s", l.Addr().String())
			if err := s.Serve(l); err != nil && err != http.ErrServerClosed {
				log.Printf("[Gateway] 服务运行异常 (%s): %v", l.Addr().String(), err)
			}
		}(ln, srv)
	}
}

func stopGatewayLocked() {
	if len(globalGateway.servers) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, s := range globalGateway.servers {
			_ = s.Shutdown(ctx)
		}
		globalGateway.servers = nil
		globalGateway.port = 0
	}
}

// buildGatewayHandler 构建网关分流路由
func buildGatewayHandler(meta *CaddyMeta, workDir string) http.Handler {
	// 准备反代代理池
	var (
		nodeProxy *httputil.ReverseProxy
		soutProxy *httputil.ReverseProxy
		suiProxy  *httputil.ReverseProxy
	)

	if meta.NodePort > 0 {
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", meta.NodePort))
		nodeProxy = httputil.NewSingleHostReverseProxy(target)
		// 零缓冲透传：实时支持 WebSocket 与 EarlyData 极速通信
		nodeProxy.FlushInterval = -1
	}

	if meta.SoutPort > 0 {
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", meta.SoutPort))
		soutProxy = httputil.NewSingleHostReverseProxy(target)
	}

	if meta.SuiPort > 0 {
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", meta.SuiPort))
		suiProxy = httputil.NewSingleHostReverseProxy(target)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		// 1. WebSocket 节点路由 (零缓冲透传，Go 原生支持 101 Switching Protocols)
		if meta.WsPath != "" && (p == "/"+meta.WsPath || strings.HasPrefix(p, "/"+meta.WsPath+"/")) {
			if nodeProxy != nil {
				nodeProxy.ServeHTTP(w, r)
				return
			}
			http.Error(w, "Node proxy unavailable", http.StatusBadGateway)
			return
		}

		// 2. 免密节点订阅接口
		if meta.SubPath != "" && (p == "/"+meta.SubPath || strings.HasPrefix(p, "/"+meta.SubPath+"/")) {
			if soutProxy != nil {
				// 获取 sout 密码以拼接免密订阅链接
				var pw string
				for _, pf := range []string{filepath.Join(workDir, "password"), "/etc/sout/password"} {
					if b, err := os.ReadFile(pf); err == nil {
						pw = strings.TrimSpace(string(b))
						if pw != "" {
							break
						}
					}
				}
				targetPath := "/" + meta.SoutPath + "/sub"
				if pw != "" {
					targetPath = "/" + meta.SoutPath + "/sub=" + pw
				}
				// 保留子路径后缀（若有）
				subSuffix := strings.TrimPrefix(p, "/"+meta.SubPath)
				if subSuffix != "" && subSuffix != "/" {
					targetPath += subSuffix
				}
				r.URL.Path = targetPath
				soutProxy.ServeHTTP(w, r)
				return
			}
			http.Error(w, "Sout proxy unavailable", http.StatusBadGateway)
			return
		}

		// 3. s-ui 管理面板
		if meta.SuiPath != "" && meta.SuiPort > 0 {
			if p == "/"+meta.SuiPath {
				http.Redirect(w, r, "/"+meta.SuiPath+"/", http.StatusPermanentRedirect)
				return
			}
			if strings.HasPrefix(p, "/"+meta.SuiPath+"/") {
				if suiProxy != nil {
					suiProxy.ServeHTTP(w, r)
					return
				}
				http.Error(w, "s-ui proxy unavailable", http.StatusBadGateway)
				return
			}
		}

		// 4. sout 动态家宽管理面板
		if meta.SoutPath != "" && meta.SoutPort > 0 {
			if p == "/"+meta.SoutPath {
				http.Redirect(w, r, "/"+meta.SoutPath+"/", http.StatusPermanentRedirect)
				return
			}
			if strings.HasPrefix(p, "/"+meta.SoutPath+"/") {
				if soutProxy != nil {
					soutProxy.ServeHTTP(w, r)
					return
				}
				http.Error(w, "sout proxy unavailable", http.StatusBadGateway)
				return
			}
		}

		// 5. 伪装根路径与兜底响应
		if p == "/" || p == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Service Ready"))
			return
		}

		// 其他未匹配路径一律返回 Service Ready
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Service Ready"))
	})
}

// GetGatewayStatus 获取当前轻量网关与隧道的完整配置及运行状态
func GetGatewayStatus(workDir string) map[string]any {
	globalGateway.mu.Lock()
	defer globalGateway.mu.Unlock()

	if workDir == "" {
		workDir = globalGateway.workDir
	}
	if workDir == "" {
		workDir = "/var/lib/sout"
	}

	st := map[string]any{
		"gateway_running":     len(globalGateway.servers) > 0,
		"gateway_port":        globalGateway.port,
		"enabled":             false,
		"domain":              "",
		"tunnel_port":         8081,
		"sout_path":           "",
		"sub_path":            "",
		"sui_path":            "",
		"sui_user":            "",
		"ws_path":             "",
		"node_port":           0,
		"mode":                "",
		"token_masked":        "",
		"protocol":            "quic",
		"cloudflared_running": false,
		"password":            "",
		"panel_mode":          "sing-box",
	}

	metaPath := filepath.Join(workDir, "gateway_meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		metaPath = filepath.Join(workDir, "caddy_meta.json")
		data, err = os.ReadFile(metaPath)
	}

	if err == nil {
		var meta GatewayMeta
		if json.Unmarshal(data, &meta) == nil {
			st["enabled"] = meta.Enabled
			st["domain"] = meta.Domain
			st["mode"] = meta.Mode
			st["tunnel_port"] = meta.TunnelPort
			st["sout_port"] = meta.SoutPort
			st["sout_path"] = meta.SoutPath
			st["sui_port"] = meta.SuiPort
			st["sui_path"] = meta.SuiPath
			st["sui_user"] = meta.SuiUser
			st["sub_port"] = meta.SubPort
			st["sub_path"] = meta.SubPath
			st["ws_path"] = meta.WsPath
			st["node_port"] = meta.NodePort
			if meta.Protocol != "" {
				st["protocol"] = meta.Protocol
			}
			if len(meta.TunnelToken) > 16 {
				st["token_masked"] = meta.TunnelToken[:6] + "..." + meta.TunnelToken[len(meta.TunnelToken)-6:]
			} else if meta.TunnelToken != "" {
				st["token_masked"] = "******"
			}
		}
	}

	// 检查 cloudflared 进程运行状态
	cfRunning := false
	if exec.Command("pgrep", "-f", "cloudflared").Run() == nil {
		cfRunning = true
	} else if data, err := os.ReadFile("/run/cloudflared.pid"); err == nil {
		pidStr := strings.TrimSpace(string(data))
		if pidStr != "" && exec.Command("kill", "-0", pidStr).Run() == nil {
			cfRunning = true
		}
	}
	st["cloudflared_running"] = cfRunning

	// 若配置已启用隧道但进程意外掉线，自动触发后台拉起自愈，防止出现 1033 错误
	if enabled, _ := st["enabled"].(bool); enabled && !cfRunning {
		go func() {
			if _, err := exec.LookPath("systemctl"); err == nil {
				_ = exec.Command("systemctl", "start", "cloudflared").Run()
			} else if _, err := exec.LookPath("rc-service"); err == nil {
				_ = exec.Command("rc-service", "cloudflared", "start").Run()
			}
		}()
	}

	// 读取当前面板口令
	pwPath := filepath.Join(workDir, "password")
	if pw, err := os.ReadFile(pwPath); err == nil {
		st["password"] = strings.TrimSpace(string(pw))
	}

	// 读取当前面板运行模式
	pmPath := filepath.Join(workDir, "panel_mode")
	if pm, err := os.ReadFile(pmPath); err == nil {
		st["panel_mode"] = strings.TrimSpace(string(pm))
	}

	return st
}

