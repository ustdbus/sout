package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// RingLogBuffer 内存环形日志缓冲区，保存最近的日志行，确保无 systemd 环境也能查看日志
type RingLogBuffer struct {
	mu       sync.RWMutex
	lines    []string
	maxLines int
}

var globalLogBuffer = &RingLogBuffer{
	maxLines: 2000,
	lines:    make([]string, 0, 2000),
}

func (b *RingLogBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	text := string(p)
	rawLines := strings.Split(text, "\n")
	for _, l := range rawLines {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		if len(b.lines) >= b.maxLines {
			b.lines = b.lines[1:]
		}
		b.lines = append(b.lines, l)
	}
	return len(p), nil
}

func (b *RingLogBuffer) Get(max int) string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.lines) == 0 {
		return "暂无内存日志记录"
	}
	if max <= 0 || max > len(b.lines) {
		max = len(b.lines)
	}
	start := len(b.lines) - max
	if start < 0 {
		start = 0
	}
	return strings.Join(b.lines[start:], "\n")
}

func initLogCapture() {
	mw := io.MultiWriter(os.Stderr, globalLogBuffer)
	log.SetOutput(mw)
}

// fetchLogs 获取最近日志，优先从 systemd journal 获取，回退到内存日志缓冲区
func fetchLogs(source string, lines int) string {
	if lines <= 0 {
		lines = 200
	}
	if lines > 1000 {
		lines = 1000
	}

	if source == "sing-box" {
		// 尝试从 journalctl 读取 sing-box 服务日志
		if hasCmd("journalctl") {
			cmd := exec.Command("journalctl", "-u", "sing-box", "-n", strconv.Itoa(lines), "--no-pager")
			out, err := cmd.CombinedOutput()
			if err == nil && len(bytes.TrimSpace(out)) > 0 {
				return string(out)
			}
		}
		// 尝试从常见日志文件读取
		candidates := []string{"/var/log/sing-box.log", "/var/log/sing-box/sing-box.log"}
		for _, f := range candidates {
			if data, err := os.ReadFile(f); err == nil && len(data) > 0 {
				allLines := strings.Split(string(data), "\n")
				if len(allLines) > lines {
					allLines = allLines[len(allLines)-lines:]
				}
				return strings.Join(allLines, "\n")
			}
		}
		return "未找到 sing-box 的 systemd 服务日志或独立日志文件"
	}

	// 默认获取 sout 自身日志
	if hasCmd("journalctl") {
		cmd := exec.Command("journalctl", "-u", "sout", "-n", strconv.Itoa(lines), "--no-pager")
		out, err := cmd.CombinedOutput()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return string(out)
		}
	}

	return globalLogBuffer.Get(lines)
}

func apiLogsHandler(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "sout"
	}
	linesStr := r.URL.Query().Get("lines")
	lines := 200
	if n, err := strconv.Atoi(linesStr); err == nil && n > 0 {
		lines = n
	}

	content := fetchLogs(source, lines)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"source": source,
		"lines":  lines,
		"logs":   content,
	})
}
