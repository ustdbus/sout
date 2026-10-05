package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const updateRepo = "ustdbus/sout"

// releaseInfo 是 GitHub Releases API 里我们关心的字段。
type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// UpdateStatus 回给界面：当前版本、最新版本、有没有新版、更新内容。
type UpdateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"has_update"`
	Notes     string `json:"notes"`
	URL       string `json:"url"`
}

// goarch 把 runtime.GOARCH 映射成 release 资产用的名字。
func assetArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// fetchLatestRelease 拉取最新 release 元数据。
func fetchLatestRelease() (*releaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", updateRepo)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sout-updater")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// checkUpdate 比对当前版本与最新 release。
func checkUpdate() (*UpdateStatus, error) {
	rel, err := fetchLatestRelease()
	if err != nil {
		return nil, err
	}
	cur := strings.TrimSpace(version)
	latest := strings.TrimSpace(rel.TagName)
	st := &UpdateStatus{
		Current:   cur,
		Latest:    latest,
		Notes:     strings.TrimSpace(rel.Body),
		URL:       rel.HTMLURL,
		HasUpdate: versionLess(cur, latest),
	}
	return st, nil
}

// versionLess 判断 cur 是否比 latest 旧。解析 vX.Y.Z 做数值比较；
// dev 或无法解析时保守认为"有更新"（让用户能装上正式版）。
func versionLess(cur, latest string) bool {
	if latest == "" {
		return false
	}
	if cur == "" || cur == "dev" {
		return true
	}
	cn, cok := parseSemver(cur)
	ln, lok := parseSemver(latest)
	if !cok || !lok {
		return cur != latest
	}
	for i := 0; i < 3; i++ {
		if cn[i] != ln[i] {
			return cn[i] < ln[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// 去掉预发布/构建后缀
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i := 0; i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// UpdateProgress 实时汇报更新进度与状态
type UpdateProgress struct {
	Running       bool   `json:"running"`
	Status        string `json:"status"` // idle, starting, checking, downloading, verifying, extracting, installing, restarting, done, error
	Progress      int    `json:"progress"` // 0-100
	Speed         string `json:"speed"`
	Message       string `json:"message"`
	Error         string `json:"error,omitempty"`
	TargetVersion string `json:"target_version,omitempty"`
}

var (
	updateMu      sync.Mutex
	currentUpdate = UpdateProgress{Status: "idle", Message: "就绪"}
)

// GetUpdateProgress 获取当前更新状态快照
func GetUpdateProgress() UpdateProgress {
	updateMu.Lock()
	defer updateMu.Unlock()
	return currentUpdate
}

func setUpdateProgress(fn func(*UpdateProgress)) {
	updateMu.Lock()
	defer updateMu.Unlock()
	fn(&currentUpdate)
}

// StartAsyncUpdate 启动异步后台更新任务，立即返回避免前端请求挂起超时
func StartAsyncUpdate() (started bool, msg string) {
	updateMu.Lock()
	if currentUpdate.Running {
		updateMu.Unlock()
		return false, "更新任务已在进行中，请勿重复点击"
	}
	currentUpdate = UpdateProgress{
		Running:  true,
		Status:   "starting",
		Progress: 0,
		Message:  "正在连接 GitHub 获取最新版本信息...",
	}
	updateMu.Unlock()

	go runAsyncUpdate()
	return true, "更新任务已启动"
}

func runAsyncUpdate() {
	defer func() {
		if r := recover(); r != nil {
			setUpdateProgress(func(p *UpdateProgress) {
				p.Running = false
				p.Status = "error"
				p.Error = fmt.Sprintf("panic: %v", r)
				p.Message = "更新异常中断"
			})
		}
	}()

	err := doApplyUpdate()
	if err != nil {
		setUpdateProgress(func(p *UpdateProgress) {
			p.Running = false
			p.Status = "error"
			p.Error = err.Error()
			p.Message = "更新失败: " + err.Error()
		})
		return
	}

	setUpdateProgress(func(p *UpdateProgress) {
		p.Running = false
		p.Status = "restarting"
		p.Progress = 100
		p.Message = "更新完成，服务正在平滑重启..."
	})

	time.Sleep(1200 * time.Millisecond)
	restartSelf()
}

// applyUpdate 同步调用更新（兼容原有入口）
func applyUpdate() error {
	return doApplyUpdate()
}

func doApplyUpdate() error {
	runtime.GC()
	debug.FreeOSMemory()

	setUpdateProgress(func(p *UpdateProgress) {
		p.Status = "checking"
		p.Message = "正在检查最新 Release 元数据..."
	})

	rel, err := fetchLatestRelease()
	if err != nil {
		return err
	}

	setUpdateProgress(func(p *UpdateProgress) {
		p.TargetVersion = rel.TagName
	})

	arch := assetArch()
	assetName := fmt.Sprintf("sout-linux-%s.tar.gz", arch)
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			assetURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return fmt.Errorf("最新版里找不到适配 %s 的包", arch)
	}

	tmp, err := os.MkdirTemp("", "fanout-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	tarPath := filepath.Join(tmp, assetName)
	setUpdateProgress(func(p *UpdateProgress) {
		p.Status = "downloading"
		p.Message = "正在下载新版本安装包..."
	})

	if err := downloadFileWithProgress(assetURL, tarPath); err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}

	if sumsURL != "" {
		setUpdateProgress(func(p *UpdateProgress) {
			p.Status = "verifying"
			p.Message = "正在校验安装包完整性..."
		})
		if err := verifyChecksum(tarPath, assetName, sumsURL); err != nil {
			return err
		}
	}

	setUpdateProgress(func(p *UpdateProgress) {
		p.Status = "extracting"
		p.Message = "正在解包新版本..."
	})

	newBin := filepath.Join(tmp, "sout-server")
	if err := extractBinary(tarPath, "sout-server", newBin); err != nil {
		if err2 := extractBinary(tarPath, "sout", newBin); err2 != nil {
			if err3 := extractBinary(tarPath, "fanout", newBin); err3 != nil {
				return fmt.Errorf("解包失败: %w", err)
			}
		}
	}
	// 一并更新终端管理脚本。此前更新流程只替换二进制，
	// 导致 /usr/local/bin/sout 永远停留在安装时的版本（脚本层修复无法下发）。
	newFsh := filepath.Join(tmp, "f.sh")
	hasNewFsh := extractBinary(tarPath, "f.sh", newFsh) == nil
	_ = os.Remove(tarPath)

	setUpdateProgress(func(p *UpdateProgress) {
		p.Status = "installing"
		p.Message = "正在原子替换二进制程序..."
	})

	// 终端管理脚本(/usr/local/bin/sout)同步替换；失败不影响二进制主流程
	if hasNewFsh {
		if err := copyFileMode(newFsh, "/usr/local/bin/sout", 0755); err != nil {
			setUpdateProgress(func(p *UpdateProgress) {
				p.Message = "管理脚本更新失败（二进制仍会更新）: " + err.Error()
			})
		}
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前程序失败: %w", err)
	}
	self, _ = filepath.EvalSymlinks(self)

	staged := self + ".new"
	if err := copyFileMode(newBin, staged, 0755); err != nil {
		return fmt.Errorf("写入新版本失败: %w", err)
	}
	if err := os.Rename(staged, self); err != nil {
		os.Remove(staged)
		return fmt.Errorf("替换二进制失败: %w", err)
	}

	return nil
}

func downloadFile(url, dst string) error {
	return downloadFileWithProgress(url, dst)
}

func downloadFileWithProgress(rawURL, dst string) error {
	candidateURLs := []string{rawURL}
	if strings.Contains(rawURL, "github.com") {
		candidateURLs = append(candidateURLs,
			"https://ghproxy.net/"+rawURL,
			"https://mirror.ghproxy.com/"+rawURL,
		)
	}

	var lastErr error
	for _, targetURL := range candidateURLs {
		err := tryDownloadWithProgress(targetURL, dst)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func tryDownloadWithProgress(url, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "fanout-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	totalSize := resp.ContentLength

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 32*1024)
	var downloaded int64
	lastReport := time.Now()
	var lastBytes int64

	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := f.Write(buf[:n]); wErr != nil {
				return wErr
			}
			downloaded += int64(n)

			now := time.Now()
			if now.Sub(lastReport) >= 300*time.Millisecond || rErr == io.EOF {
				elapsedSec := now.Sub(lastReport).Seconds()
				var speedStr string
				if elapsedSec > 0 {
					speedBps := float64(downloaded-lastBytes) / elapsedSec
					if speedBps > 1024*1024 {
						speedStr = fmt.Sprintf("%.1f MB/s", speedBps/(1024*1024))
					} else {
						speedStr = fmt.Sprintf("%.0f KB/s", speedBps/1024)
					}
				}
				lastReport = now
				lastBytes = downloaded

				progress := 0
				if totalSize > 0 {
					progress = int(downloaded * 100 / totalSize)
					if progress > 99 && rErr != io.EOF {
						progress = 99
					}
				}

				setUpdateProgress(func(p *UpdateProgress) {
					p.Progress = progress
					p.Speed = speedStr
					if totalSize > 0 {
						p.Message = fmt.Sprintf("正在下载: %.1f MB / %.1f MB (%d%%)", float64(downloaded)/(1024*1024), float64(totalSize)/(1024*1024), progress)
					} else {
						p.Message = fmt.Sprintf("正在下载: %.1f MB", float64(downloaded)/(1024*1024))
					}
				})
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return rErr
		}
	}
	return nil
}


func verifyChecksum(path, name, sumsURL string) error {
	sums := filepath.Join(filepath.Dir(path), "checksums.txt")
	if err := downloadFile(sumsURL, sums); err != nil {
		return fmt.Errorf("下载校验和失败: %w", err)
	}
	want, err := sha256FromList(sums, name)
	if err != nil {
		return err
	}
	got, err := sha256File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("校验和不匹配，包可能损坏")
	}
	return nil
}

func sha256FromList(listPath, name string) (string, error) {
	blob, err := os.ReadFile(listPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(blob), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("校验和列表里没有 %s", name)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary 从 tar.gz 里取出指定文件名的成员写到 dst。
func extractBinary(tarGz, member, dst string) error {
	f, err := os.Open(tarGz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("包里没有 %s", member)
		}
		if err != nil {
			return err
		}
		base := filepath.Base(hd.Name)
		if base != member && !(member == "fanout" && base == "sout") && !(member == "sout" && base == "fanout") {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, tr); err != nil {
			return err
		}
		return nil
	}
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// restartSelf 通过 init 系统重启 sout 服务，拉起刚替换的新二进制。
// systemd / openrc 各一套；都不可用时退回直接自我 exec。
func restartSelf() {
	if hasCmd("systemctl") && dirExists("/run/systemd/system") {
		if exec.Command("systemctl", "is-active", "sout").Run() == nil {
			_ = exec.Command("systemctl", "restart", "sout").Start()
			return
		}
		_ = exec.Command("systemctl", "restart", "fanout").Start()
		return
	}
	if hasCmd("rc-service") {
		svc := "sout"
		if exec.Command("rc-service", "sout", "status").Run() != nil && exec.Command("rc-service", "fanout", "status").Run() == nil {
			svc = "fanout"
		}
		// 必须通过独立后台子进程延迟重启，确保旧进程彻底退出后再拉起新版本
		_ = exec.Command("sh", "-c", fmt.Sprintf("sleep 1 && (rc-service %s restart || (rc-service %s stop; sleep 1; killall -9 sout-server 2>/dev/null; rc-service %s zap && rc-service %s start))", svc, svc, svc, svc)).Start()
		return
	}
	// 没有 init 系统托管：直接退出，让外部守护（若有）拉起；
	// 没有守护就只能等下次手动启动。日志留个痕。
	fmt.Println("sout: 已替换二进制，但未检测到 systemd/openrc，请手动重启服务")
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
