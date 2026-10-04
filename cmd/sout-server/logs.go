package main

import (
	"bytes"
	"encoding/json"
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

func readLastLinesFromFile(filePath string, lines int) string {
	if fi, err := os.Stat(filePath); err != nil || fi.Size() == 0 {
		return ""
	}
	if hasCmd("tail") {
		out, err := exec.Command("tail", "-n", strconv.Itoa(lines), filePath).CombinedOutput()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return string(out)
		}
	}
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()
	readSize := int64(256 * 1024)
	offset := int64(0)
	if size > readSize {
		offset = size - readSize
	}
	buf := make([]byte, size-offset)
	_, err = f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return ""
	}
	allLines := strings.Split(string(buf), "\n")
	if len(allLines) > lines {
		allLines = allLines[len(allLines)-lines:]
	}
	return strings.Join(allLines, "\n")
}

// fetchLogs 获取最近日志，优先从 systemd journal 获取，回退到常见日志文件或内存日志缓冲区
func fetchLogs(source string, lines int) string {
	if lines <= 0 {
		lines = 200
	}
	if lines > 1000 {
		lines = 1000
	}

	switch source {
	case "tunnel", "cloudflared":
		if hasCmd("journalctl") {
			cmd := exec.Command("journalctl", "-u", "cloudflared", "-n", strconv.Itoa(lines), "--no-pager")
			out, err := cmd.CombinedOutput()
			if err == nil && len(bytes.TrimSpace(out)) > 0 {
				return string(out)
			}
		}
		candidates := []string{
			"/var/log/cloudflared.err",
			"/var/log/cloudflared.log",
			"/var/log/cloudflared/cloudflared.log",
			"/var/log/cloudflared/cloudflared.err",
		}
		for _, f := range candidates {
			if s := readLastLinesFromFile(f, lines); s != "" {
				return s
			}
		}
		return "暂无 Cloudflare 隧道日志（若未启用隧道或刚启动，请稍候查看）"

	case "sing-box", "sui", "core":
		if hasCmd("journalctl") {
			cmd := exec.Command("journalctl", "-u", "sing-box", "-n", strconv.Itoa(lines), "--no-pager")
			out, err := cmd.CombinedOutput()
			if err == nil && len(bytes.TrimSpace(out)) > 0 {
				return string(out)
			}
			cmdSui := exec.Command("journalctl", "-u", "s-ui", "-n", strconv.Itoa(lines), "--no-pager")
			outSui, errSui := cmdSui.CombinedOutput()
			if errSui == nil && len(bytes.TrimSpace(outSui)) > 0 {
				return string(outSui)
			}
		}
		candidates := []string{
			"/var/log/sing-box.err",
			"/var/log/sing-box.log",
			"/var/log/sing-box/sing-box.log",
			"/var/log/sing-box/sing-box.err",
			"/var/log/s-ui.err",
			"/var/log/s-ui.log",
		}
		for _, f := range candidates {
			if s := readLastLinesFromFile(f, lines); s != "" {
				return s
			}
		}
		return "未找到 sing-box / s-ui 的服务日志或独立日志文件"

	default: // "sout"
		if hasCmd("journalctl") {
			cmd := exec.Command("journalctl", "-u", "sout", "-n", strconv.Itoa(lines), "--no-pager")
			out, err := cmd.CombinedOutput()
			if err == nil && len(bytes.TrimSpace(out)) > 0 {
				return string(out)
			}
		}
		candidates := []string{
			"/var/log/sout.err",
			"/var/log/sout.log",
		}
		for _, f := range candidates {
			if s := readLastLinesFromFile(f, lines); s != "" {
				return s
			}
		}
		return globalLogBuffer.Get(lines)
	}
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
