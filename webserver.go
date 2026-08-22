package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type webServer struct {
	handler http.Handler

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	addr string
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

	ln, err := net.Listen("tcp", addr)
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

	srv := &http.Server{Handler: s.handler}
	s.srv = srv
	s.ln = ln
	s.addr = addr
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP 服务 %s 退出: %v", addr, err)
		}
	}()

	log.Printf("已切换监听地址至 %s", addr)
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

	cur := getWebSettings()
	curNorm, _ := normalizeListenAddr(cur.ListenAddr)
	cur.ListenAddr = curNorm

	// 端口和监听地址均未变化时，直接跳过 reload，避免误触发 bind error
	if next.Port == cur.Port && next.ListenAddr == cur.ListenAddr {
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
