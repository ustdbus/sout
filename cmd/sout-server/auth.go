package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Auth struct {
	mu       sync.RWMutex
	dir      string
	password string
	secret   []byte
	sessions map[string]time.Time
	fails    map[string]*loginFails
}

type loginFails struct {
	count   int
	last    time.Time
	blocked time.Time
}

const sessionTTL = 30 * 24 * time.Hour

const (
	loginMaxFails  = 8
	loginBlockFor  = 2 * time.Minute
	loginFailReset = 10 * time.Minute
)

func NewAuth(dir string) (*Auth, bool, error) {
	path := filepath.Join(dir, "password")
	created := false

	blob, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		pw, gerr := randomToken(9)
		if gerr != nil {
			return nil, false, gerr
		}
		if werr := os.WriteFile(path, []byte(pw+"\n"), 0600); werr != nil {
			return nil, false, fmt.Errorf("写入密码文件失败: %w", werr)
		}
		blob = []byte(pw)
		created = true
	} else if err != nil {
		return nil, false, err
	}

	secretPath := filepath.Join(dir, "auth_secret")
	secret, err := os.ReadFile(secretPath)
	if os.IsNotExist(err) || len(secret) < 16 {
		sec := make([]byte, 32)
		if _, rerr := rand.Read(sec); rerr == nil {
			secret = sec
			_ = os.WriteFile(secretPath, secret, 0600)
		}
	}

	return &Auth{
		dir:      dir,
		password: strings.TrimSpace(string(blob)),
		secret:   secret,
		sessions: map[string]time.Time{},
		fails:    map[string]*loginFails{},
	}, created, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (a *Auth) currentPassword() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.password
}

func (a *Auth) check(pw string) bool {
	a.mu.RLock()
	cur := a.password
	a.mu.RUnlock()
	want := sha256.Sum256([]byte(cur))
	got := sha256.Sum256([]byte(pw))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func (a *Auth) SetPassword(pw string) error {
	pw = strings.TrimSpace(pw)
	if pw == "" {
		return fmt.Errorf("密码不能为空")
	}
	if len(pw) < 4 {
		return fmt.Errorf("密码至少 4 位")
	}
	path := filepath.Join(a.dir, "password")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(pw+"\n"), 0600); err != nil {
		return fmt.Errorf("写入密码文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("重命名密码文件失败: %w", err)
	}
	a.mu.Lock()
	a.password = pw
	a.sessions = map[string]time.Time{} // 修改密码后清空旧内存会话
	a.mu.Unlock()
	return nil
}

func (a *Auth) signPayload(payload string) string {
	a.mu.RLock()
	pw := a.password
	sec := a.secret
	a.mu.RUnlock()

	h := hmac.New(sha256.New, sec)
	h.Write([]byte(payload))
	h.Write([]byte("|"))
	pwHash := sha256.Sum256([]byte(pw))
	h.Write(pwHash[:])
	return hex.EncodeToString(h.Sum(nil))
}

func (a *Auth) issue() (string, error) {
	exp := time.Now().Add(sessionTTL)
	expHex := strconv.FormatInt(exp.Unix(), 16)
	nonce, err := randomToken(8)
	if err != nil {
		return "", err
	}
	payload := expHex + "." + nonce
	sig := a.signPayload(payload)
	tok := payload + "." + sig

	a.mu.Lock()
	a.sessions[tok] = exp
	for k, e := range a.sessions {
		if time.Now().After(e) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	return tok, nil
}

func (a *Auth) valid(tok string) bool {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return false
	}

	// 1. 先查活跃内存缓存
	a.mu.RLock()
	exp, ok := a.sessions[tok]
	a.mu.RUnlock()
	if ok && time.Now().Before(exp) {
		return true
	}

	// 2. 内存未命中（如升级、重启），进行 HMAC-SHA256 签名与有效期无感自验证
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[0], 16, 64)
	if err != nil {
		return false
	}
	expTime := time.Unix(expUnix, 0)
	if time.Now().After(expTime) {
		return false
	}

	payload := parts[0] + "." + parts[1]
	expectedSig := a.signPayload(payload)
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(parts[2])) != 1 {
		return false
	}

	// 验证签名成功，密码未变且在 30 天有效期内，自动无感恢复入内存
	a.mu.Lock()
	a.sessions[tok] = expTime
	a.mu.Unlock()
	return true
}

const sessionCookie = "sout_session"

// Wrap 包装 handler。未登录时 API 返回 401，页面跳登录，订阅 /sub 免登录放行
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			a.handleLogin(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/sub=") {
			pw := strings.TrimPrefix(r.URL.Path, "/sub=")
			subPath := ""
			if idx := strings.Index(pw, "/"); idx != -1 {
				subPath = pw[idx:]
				pw = pw[:idx]
			}
			if a.check(pw) {
				r.URL.Path = "/sub" + subPath
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "访问口令不正确"})
			return
		}
		if (r.URL.Path == "/sub" || r.URL.Path == "/sub/") && (r.URL.Query().Get("token") != "" || r.URL.Query().Get("pw") != "" || r.URL.Query().Get("password") != "") {
			token := r.URL.Query().Get("token")
			if token == "" {
				token = r.URL.Query().Get("pw")
			}
			if token == "" {
				token = r.URL.Query().Get("password")
			}
			if a.check(token) {
				r.URL.Path = "/sub"
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "访问口令不正确"})
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil && a.valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if pw := r.Header.Get("X-Sout-Password"); pw != "" && a.check(pw) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(loginHTML))
	})
}

func (a *Auth) blocked(ip string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	f, ok := a.fails[ip]
	return ok && time.Now().Before(f.blocked)
}

func (a *Auth) recordFail(ip string) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.fails[ip]
	if !ok || (f.blocked.IsZero() && now.Sub(f.last) > loginFailReset) {
		f = &loginFails{}
		a.fails[ip] = f
	}
	f.count++
	f.last = now
	if f.count >= loginMaxFails {
		f.blocked = now.Add(loginBlockFor)
		f.count = 0
	}
	for k, v := range a.fails {
		if now.Sub(v.last) > loginFailReset && now.After(v.blocked) {
			delete(a.fails, k)
		}
	}
}

func (a *Auth) clearFails(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(loginHTML))
		return
	}
	ip := clientIP(r)
	if a.blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "登录失败次数过多，请稍后再试"})
		return
	}
	if !a.check(r.FormValue("password")) {
		a.recordFail(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "访问口令不正确"})
		return
	}
	a.clearFails(ip)
	tok, err := a.issue()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]string{"ok": "已登录"})
}

const loginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sout</title>
<style>
body{margin:0;height:100vh;display:flex;flex-direction:column;gap:16px;
  align-items:center;justify-content:center;
  background:#12151a;color:#dde3ec;
  font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.links{display:flex;gap:16px}
.links a{color:#8b95a5;text-decoration:none;font-size:12px}
.links a:hover{color:#4a9eda}
form{background:#181c23;border:1px solid #262c36;border-radius:6px;
  padding:22px 24px;width:300px}
h1{font-size:13px;font-weight:600;margin:0 0 16px}
label{display:block;color:#8b95a5;font-size:11px;margin-bottom:6px}
input{width:100%;box-sizing:border-box;background:#0e1116;border:1px solid #262c36;
  color:#dde3ec;border-radius:4px;padding:7px 9px;font:inherit}
input:focus{outline:none;border-color:#4a9eda}
button{width:100%;margin-top:14px;background:#4a9eda;border:0;color:#0b0e12;
  font:inherit;font-weight:600;border-radius:4px;padding:8px;cursor:pointer}
.err{color:#c25450;font-size:11px;margin-top:10px;min-height:14px}
</style>
</head>
<body>
<form id="f">
  <h1>sout</h1>
  <label for="pw">访问口令</label>
  <input type="password" id="pw" autofocus autocomplete="current-password">
  <button type="submit">登录</button>
  <div class="err" id="err"></div>
</form>
<div class="links">
  <a href="https://github.com/ustdbus/sout" target="_blank" rel="noopener">GitHub</a>
</div>
<script>
document.getElementById('f').onsubmit = async e => {
  e.preventDefault();
  const body = new URLSearchParams({password: document.getElementById('pw').value});
  const r = await fetch('login', {method:'POST', body});
  if(r.ok){ location.reload(); return; }
  const d = await r.json().catch(()=>({}));
  document.getElementById('err').textContent = d.error || '登录失败';
};
</script>
</body>
</html>`
