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
}

// CaddyMeta 保持向下兼容
type CaddyMeta = GatewayMeta

type GatewayManager struct {
	mu      sync.Mutex
	workDir string
	server  *http.Server
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
	if globalGateway.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = globalGateway.server.Shutdown(ctx)
		globalGateway.server = nil
		globalGateway.port = 0
		log.Println("[Gateway] 内置轻量反代网关已关闭")
	}
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
	if globalGateway.server != nil && globalGateway.port == meta.TunnelPort {
		globalGateway.server.Handler = handler
		log.Printf("[Gateway] 内置轻量反代网关路由已热更新 (127.0.0.1:%d)", meta.TunnelPort)
		return
	}

	stopGatewayLocked()

	listenAddr := fmt.Sprintf("127.0.0.1:%d", meta.TunnelPort)
	srv := &http.Server{
		Addr:    listenAddr,
		Handler: handler,
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Printf("[Gateway] 监听回源端口失败 (%s): %v", listenAddr, err)
		return
	}

	globalGateway.server = srv
	globalGateway.port = meta.TunnelPort

	go func() {
		log.Printf("[Gateway] 内置轻量反代网关已成功接管 127.0.0.1:%d", meta.TunnelPort)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[Gateway] 服务运行异常: %v", err)
		}
	}()
}

func stopGatewayLocked() {
	if globalGateway.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = globalGateway.server.Shutdown(ctx)
		globalGateway.server = nil
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
