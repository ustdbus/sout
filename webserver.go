package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type webServer struct {
	handler http.Handler

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	addr string
	tls  bool
}

func newWebServer(h http.Handler) *webServer {
	return &webServer{handler: h}
}

func (s *webServer) serve() error {
	cfg := getWebSettings()
	if err := s.reload(cfg); err != nil {
		return err
	}
	select {}
}

func (s *webServer) reload(cfg WebSettings) error {
	addr := cfg.listenAddrString()

	var tlsConfig *tls.Config
	if cfg.SSLEnabled {
		if err := validateSSL(cfg.SSLCert, cfg.SSLKey); err != nil {
			return err
		}
		cert, err := tls.LoadX509KeyPair(cfg.SSLCert, cfg.SSLKey)
		if err != nil {
			return fmt.Errorf("加载 SSL 证书失败: %w", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	s.mu.Lock()
	oldSrv := s.srv
	oldLn := s.ln
	if oldLn != nil {
		_ = oldLn.Close()
	}
	if oldSrv != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = oldSrv.Shutdown(ctx)
		}()
	}

	rawLn, err := net.Listen("tcp", addr)
	if err != nil {
		if oldLn != nil && s.addr != "" && s.addr != addr {
			if rln, rerr := net.Listen("tcp", s.addr); rerr == nil {
				s.ln = rln
				srv := &http.Server{Handler: s.handler}
				s.srv = srv
				go srv.Serve(rln)
			}
		}
		s.mu.Unlock()
		return fmt.Errorf("无法监听 %s: %w", addr, err)
	}

	var ln net.Listener = rawLn
	if tlsConfig != nil {
		ln = tls.NewListener(rawLn, tlsConfig)
	}

	srv := &http.Server{Handler: s.handler}
	s.srv = srv
	s.ln = ln
	s.addr = addr
	s.tls = cfg.SSLEnabled
	s.mu.Unlock()

	scheme := "http"
	if cfg.SSLEnabled {
		scheme = "https"
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("%s 服务 %s 退出: %v", strings.ToUpper(scheme), addr, err)
		}
	}()

	log.Printf("已切换监听地址至 %s://%s", scheme, addr)
	return nil
}

func (s *webServer) applyWebSettings(next WebSettings) error {
	if err := validatePort(next.Port); err != nil {
		return err
	}
	norm, err := normalizeListenAddr(next.ListenAddr)
	if err != nil {
		return err
	}
	next.ListenAddr = norm

	if next.SSLEnabled {
		if err := validateSSL(next.SSLCert, next.SSLKey); err != nil {
			return err
		}
	}

	cur := getWebSettings()
	curNorm, _ := normalizeListenAddr(cur.ListenAddr)
	cur.ListenAddr = curNorm

	// 配置完全无变化时跳过 reload，避免误报端口占用
	if next.Port == cur.Port && next.ListenAddr == cur.ListenAddr &&
		next.SSLEnabled == cur.SSLEnabled && next.SSLCert == cur.SSLCert &&
		next.SSLKey == cur.SSLKey && next.SSLDomain == cur.SSLDomain {
		return nil
	}

	if err := s.reload(next); err != nil {
		return err
	}

	webSettingsMu.Lock()
	webSettingsCur = next
	webSettingsMu.Unlock()
	if err := saveWebSettings(); err != nil {
		log.Printf("保存 Web 设置失败: %v", err)
		return err
	}
	return nil
}
