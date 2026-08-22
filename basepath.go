package main

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const basePathAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

var (
	basePathMu  sync.RWMutex
	basePathCur string
	basePathDir string
)

func initBasePath(dir string) (bool, error) {
	bp, created, err := LoadBasePath(dir)
	if err != nil {
		return false, err
	}
	basePathMu.Lock()
	basePathCur = bp
	basePathDir = dir
	basePathMu.Unlock()
	return created, nil
}

func currentBasePath() string {
	basePathMu.RLock()
	defer basePathMu.RUnlock()
	return basePathCur
}

func setBasePath(raw string) (string, error) {
	bp := normalizeBasePath(raw)
	if bp != "" {
		for _, c := range strings.TrimPrefix(bp, "/") {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-' || c == '_'
			if !ok {
				return "", fmt.Errorf("访问路径只能包含字母、数字、- 和 _")
			}
		}
	}
	basePathMu.RLock()
	dir := basePathDir
	basePathMu.RUnlock()
	if err := os.WriteFile(filepath.Join(dir, "basepath"), []byte(strings.TrimPrefix(bp, "/")+"\n"), 0600); err != nil {
		return "", fmt.Errorf("写入访问路径失败: %w", err)
	}
	basePathMu.Lock()
	basePathCur = bp
	basePathMu.Unlock()
	return bp, nil
}

func LoadBasePath(dir string) (string, bool, error) {
	path := filepath.Join(dir, "basepath")

	blob, err := os.ReadFile(path)
	if err == nil {
		if bp := strings.TrimSpace(string(blob)); bp != "" {
			return normalizeBasePath(bp), false, nil
		}
	} else if !os.IsNotExist(err) {
		return "", false, err
	}

	bp, err := randomBasePath(10)
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(bp+"\n"), 0600); err != nil {
		return "", false, fmt.Errorf("写入访问路径失败: %w", err)
	}
	return normalizeBasePath(bp), true, nil
}

func randomBasePath(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = basePathAlphabet[int(v)%len(basePathAlphabet)]
	}
	return string(out), nil
}

func normalizeBasePath(bp string) string {
	bp = strings.Trim(bp, "/")
	if bp == "" {
		return ""
	}
	return "/" + bp
}

func StripBasePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := currentBasePath()
		if base == "" {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case r.URL.Path == base:
			http.Redirect(w, r, base+"/", http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, base+"/"):
			r.URL.Path = strings.TrimPrefix(r.URL.Path, base)
			next.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
