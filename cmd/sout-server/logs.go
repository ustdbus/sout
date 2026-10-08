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
	"path/filepath"
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

// 单个日志文件上限 1MB，保留 1 份备份 => 单个文件最多占用约 2MB，防止容器磁盘 I/O 阻塞。
const logRotateMaxBytes = 1 * 1024 * 1024

// rotatingLogWriter 追加写入日志文件，超过阈值时把当前文件改名为 .1 后重新开始。
// 目的是避免无 systemd 环境下 stdout/stderr 被 OpenRC 重定向到文件后无限增长。
type rotatingLogWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

func newRotatingLogWriter(path string, maxBytes int64) (*rotatingLogWriter, error) {
	w := &rotatingLogWriter{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingLogWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = fi.Size()
	return nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size+int64(len(p)) > w.maxBytes {
		_ = w.file.Close()
		w.file = nil
		_ = os.Rename(w.path, w.path+".1")
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// logFileWriter 为一个已被重定向的流创建轮转写入器；未被重定向到文件则返回 nil，
// 以免在这里凭空创建重复的日志文件。
func logFileWriter(stream *os.File) io.Writer {
	if stream == nil {
		return nil
	}
	fi, err := stream.Stat()
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeCharDevice != 0 || fi.Mode()&os.ModeNamedPipe != 0 {
		// 终端或管道（如 systemd journal），不需要内部轮转
		return nil
	}
	path := stream.Name()
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	w, err := newRotatingLogWriter(path, logRotateMaxBytes)
	if err != nil {
		return nil
	}
	return w
}

// initLogCapture 同时接管 stdout / stderr 与内存环形缓冲。
// 无 systemd 时 OpenRC 会把这两个流重定向到文件，这里为其加上大小上限。
func initLogCapture() {
	writers := []io.Writer{os.Stdout, os.Stderr, globalLogBuffer}
	if fw := logFileWriter(os.Stderr); fw != nil {
		// 已被重定向到文件：仅由轮转写入器与内存缓冲接管，避免同一份日志向同一文件写入两次
		writers = []io.Writer{fw, globalLogBuffer}
	}
	log.SetOutput(io.MultiWriter(writers...))
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

// fetchServiceLogs 通用取日志：先试 systemd journal，再回退到服务自己的日志文件。
// journal 不可用（OpenRC 等）时文件是唯一来源，所以这里对两类后端都带上候选文件。
func fetchServiceLogs(unit string, lines int, files ...string) string {
	if hasCmd("journalctl") {
		cmd := exec.Command("journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager")
		if out, err := cmd.CombinedOutput(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			return string(out)
		}
	}
	for _, f := range files {
		if s := readLastLinesFromFile(f, lines); s != "" {
			return s
		}
	}
	// 文件也拿不到时回退内存缓冲，保证面板不至于空白
	return globalLogBuffer.Get(lines)
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
		isSingboxTunnel := false
		if b, err := os.ReadFile("/etc/sing-box/config.json"); err == nil && (strings.Contains(string(b), "\"cloudflared\"") || strings.Contains(string(b), "\"cf-tunnel-in\"")) {
			isSingboxTunnel = true
		} else if b, err := os.ReadFile(filepath.Join(currentBasePathDir(), "caddy_meta.json")); err == nil && strings.Contains(string(b), "\"tunnel_engine\": \"sing-box\"") {
			isSingboxTunnel = true
		}

		if isSingboxTunnel {
			var sbLogs string
			if hasCmd("journalctl") {
				cmd := exec.Command("journalctl", "-u", "sing-box", "-n", strconv.Itoa(lines*2), "--no-pager")
				if out, err := cmd.CombinedOutput(); err == nil && len(bytes.TrimSpace(out)) > 0 {
					sbLogs = string(out)
				}
			}
			if sbLogs == "" {
				for _, f := range []string{"/var/log/sing-box.log", "/var/log/sing-box/sing-box.log", "/var/log/sing-box.err"} {
					if s := readLastLinesFromFile(f, lines*2); s != "" {
						sbLogs = s
						break
					}
				}
			}
			if sbLogs != "" {
				var tunnelLines []string
				for _, line := range strings.Split(sbLogs, "\n") {
					lower := strings.ToLower(line)
					if strings.Contains(lower, "cloudflared") || strings.Contains(lower, "cf-tunnel") || strings.Contains(lower, "tunnel") || strings.Contains(lower, "quic") || strings.Contains(lower, "ingress") {
						tunnelLines = append(tunnelLines, line)
					}
				}
				if len(tunnelLines) > 0 {
					if len(tunnelLines) > lines {
						tunnelLines = tunnelLines[len(tunnelLines)-lines:]
					}
					return strings.Join(tunnelLines, "\n")
				}
				allLines := strings.Split(sbLogs, "\n")
				if len(allLines) > lines {
					allLines = allLines[len(allLines)-lines:]
				}
				return strings.Join(allLines, "\n")
			}
		}

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

	case "sui", "s-ui":
		// s-ui 模式下内核由 s-ui 进程托管，连接日志都记在 s-ui 名下
		return fetchServiceLogs("s-ui", lines,
			"/var/log/s-ui.err",
			"/var/log/s-ui.log",
			"/usr/local/s-ui/logs/s-ui.log",
		)

	case "sing-box", "core":
		// 纯内核模式：可直接查 sing-box 服务，或读它自己落盘的日志
		return fetchServiceLogs("sing-box", lines,
			"/var/log/sing-box/sing-box.log",
			"/var/log/sing-box/sing-box.err",
			"/var/log/sing-box.err",
			"/var/log/sing-box.log",
		)

	default: // "sout"
		return fetchServiceLogs("sout", lines,
			"/var/log/sout.err",
			"/var/log/sout.log",
		)
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
