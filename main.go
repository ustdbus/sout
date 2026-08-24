package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version 由构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	var (
		webPort  = flag.Int("web", 8899, "Web 管理端口")
		maxSlots = flag.Int("max", 20, "最多同时运行的隧道数")
		workDir  = flag.String("dir", "/var/lib/fanout", "工作目录")
	)
	panelMode := flag.String("panel", "", "节点链接后端: 留空自动探测, s-ui, 3x-ui")
	publicIP := flag.String("ip", "", "母机公网 IPv4，用于分享链接/SOCKS5 地址；留空则自动探测")
	showVersion := flag.Bool("version", false, "显示版本后退出")
	flag.Parse()

	if *publicIP == "" {
		*publicIP = os.Getenv("FANOUT_PUBLIC_IP")
	}

	if *showVersion {
		fmt.Println("sout", version)
		return
	}

	if os.Geteuid() != 0 {
		log.Fatal("需要 root 权限（要创建 netns 和改 iptables）")
	}
	if err := os.MkdirAll(*workDir, 0700); err != nil {
		log.Fatalf("创建工作目录失败: %v", err)
	}
	setPublicIPOverride(*publicIP)
	go hostPublicIP() // 预热探测，别让首个请求阻塞
	if err := prepareHost(); err != nil {
		log.Fatal(err)
	}

	configurePanel(*workDir, *panelMode)
	if p, err := openPanel(); err != nil {
		log.Printf("节点链接后端暂不可用（可在 Web 界面查看原因）: %v", err)
	} else {
		log.Printf("节点链接后端: %s", p.Describe())
	}

	mgr := NewManager(*maxSlots, *workDir)
	log.Printf("正在拉取节点列表...")
	if n, err := mgr.RefreshNodes(); err != nil {
		log.Printf("拉取失败（可在 Web 界面重试）: %v", err)
	} else {
		log.Printf("已获取 %d 个节点", n)
	}

	if n, err := mgr.restoreState(); err != nil {
		log.Printf("恢复上次状态失败: %v", err)
	} else if n > 0 {
		log.Printf("正在恢复上次的 %d 条隧道", n)
		go mgr.ReconcileOutbounds()
	}

	go mgr.WatchHealth()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("正在清理所有隧道...")
		mgr.Shutdown()
		closePanel()
		os.Exit(0)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/nodes", apiNodes(mgr))
	mux.HandleFunc("/api/tunnels", apiTunnels(mgr))
	mux.HandleFunc("/api/start", apiStart(mgr))
	mux.HandleFunc("/api/stop", apiStop(mgr))
	mux.HandleFunc("/api/swap", apiSwap(mgr))
	mux.HandleFunc("/api/cred", apiCred(mgr))
	mux.HandleFunc("/api/refresh", apiRefresh(mgr))
	mux.HandleFunc("/api/regions", apiRegions(mgr))
	mux.HandleFunc("/api/provision", apiProvision(mgr))
	mux.HandleFunc("/api/node/add_branch", apiAddNodeBranch(mgr))
	mux.HandleFunc("/api/jobs", apiJobs(mgr))
	mux.HandleFunc("/api/jobs/dismiss", apiJobDismiss(mgr))
	mux.HandleFunc("/api/exits", apiExits(mgr))
	mux.HandleFunc("/api/xui", apiXUIStatus)
	mux.HandleFunc("/api/xui/inbounds", apiXUIInbounds(mgr))
	mux.HandleFunc("/api/xui/bind", apiXUIBind(mgr))
	mux.HandleFunc("/api/xui/clone", apiXUIClone(mgr))
	mux.HandleFunc("/api/xui/detail", apiXUIDetail)
	mux.HandleFunc("/api/xui/links", apiXUILinks)
	mux.HandleFunc("/api/xui/delete", apiXUIDelete(mgr))
	mux.HandleFunc("/api/panel/inbound/new", apiInboundCreate(mgr))
	mux.HandleFunc("/api/panel/inbound/update", apiInboundUpdate(mgr))
	mux.HandleFunc("/api/panel/client/add", apiClientAdd(mgr))
	mux.HandleFunc("/api/panel/client/del", apiClientDelete(mgr))
	mux.HandleFunc("/api/panel/client/reset", apiClientReset(mgr))
	initCustomStore(*workDir)

	mux.HandleFunc("/api/custom/socks/add", apiCustomSocksAdd(mgr))
	mux.HandleFunc("/api/custom/socks/test", apiCustomSocksTest)
	mux.HandleFunc("/api/custom/source/add", apiCustomSourceAdd)
	mux.HandleFunc("/api/custom/source/list", apiCustomSourceList(mgr))
	mux.HandleFunc("/api/custom/source/delete", apiCustomSourceDelete)
	mux.HandleFunc("/api/custom/source/toggle", apiCustomSourceToggle)
	mux.HandleFunc("/api/custom/source/refresh", apiCustomSourceRefresh(mgr))
	mux.HandleFunc("/api/custom/source/import", apiCustomSourceImport(mgr))

	mux.HandleFunc("/sub", handleSub(mgr))

	auth, created, err := NewAuth(*workDir)
	if err != nil {
		log.Fatalf("初始化访问口令失败: %v", err)
	}
	if created {
		log.Printf("已生成访问口令，见 %s", filepath.Join(*workDir, "password"))
	}

	bpCreated, err := initBasePath(*workDir)
	if err != nil {
		log.Fatalf("初始化访问路径失败: %v", err)
	}
	if bpCreated {
		log.Printf("已生成访问路径，见 %s", filepath.Join(*workDir, "basepath"))
	}

	portExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "web" {
			portExplicit = true
		}
	})
	webCfg, err := loadWebSettings(*workDir, *webPort, portExplicit)
	if err != nil {
		log.Fatalf("加载 Web 设置失败: %v", err)
	}

	srv := newWebServer(StripBasePath(auth.Wrap(mux)))
	mux.HandleFunc("/api/settings", apiSettings(auth, srv))
	mux.HandleFunc("/api/update/check", apiUpdateCheck)
	mux.HandleFunc("/api/update/apply", apiUpdateApply)

	log.Printf("管理界面: http://<本机IP>%s%s/", webCfg.listenAddrString(), currentBasePath())
	log.Printf("SOCKS5 端口在 %d-%d 之间随机分配", randPortMin, randPortMax)
	if err := srv.serve(); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiNodes(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes, fetched := m.Nodes()
		if len(nodes) > 200 {
			nodes = nodes[:200]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"nodes":   nodes,
			"fetched": fetched,
		})
	}
}

func apiTunnels(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Tunnels())
	}
}

func apiStart(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		if host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 host 参数"})
			return
		}
		nodes, _ := m.Nodes()
		for _, n := range nodes {
			if n.HostName == host {
				t, err := m.Start(n)
				if err != nil {
					writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, t)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在，可能列表已过期"})
	}
}

func apiStop(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slot, err := strconv.Atoi(r.URL.Query().Get("slot"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slot 参数无效"})
			return
		}
		if err := m.Stop(slot); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "已停止"})
	}
}

func apiRefresh(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := m.RefreshNodes()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"count": n})
	}
}

func apiSwap(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slot, err := strconv.Atoi(r.URL.Query().Get("slot"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slot 参数无效"})
			return
		}
		if err := m.Swap(slot); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "正在换节点"})
	}
}

func apiRegions(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		poolType := r.URL.Query().Get("type")
		writeJSON(w, http.StatusOK, m.Regions(poolType))
	}
}

func apiCred(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var slot int
		var user, pass string
		var port int

		if r.Method == http.MethodPost {
			var body struct {
				Slot int    `json:"slot"`
				User string `json:"user"`
				Pass string `json:"pass"`
				Port int    `json:"port"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				_ = r.ParseForm()
				slot, _ = strconv.Atoi(r.FormValue("slot"))
				user = r.FormValue("user")
				pass = r.FormValue("pass")
				port, _ = strconv.Atoi(r.FormValue("port"))
			} else {
				slot = body.Slot
				user = body.User
				pass = body.Pass
				port = body.Port
			}
		} else {
			q := r.URL.Query()
			var err error
			slot, err = strconv.Atoi(q.Get("slot"))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slot 参数无效"})
				return
			}
			user = q.Get("user")
			pass = q.Get("pass")
			port, _ = strconv.Atoi(q.Get("port"))
		}

		if slot <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slot 参数无效"})
			return
		}

		cred, finalPort, err := m.UpdateTunnelConfig(slot, SocksCred{
			User: strings.TrimSpace(user),
			Pass: strings.TrimSpace(pass),
		}, port)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   "已保存",
			"user": cred.User,
			"pass": cred.Pass,
			"port": finalPort,
		})
	}
}

func apiSettings(auth *Auth, srv *webServer) http.HandlerFunc {
	type settingsReq struct {
		Password   *string `json:"password"`
		BasePath   *string `json:"base_path"`
		Port       *int    `json:"port"`
		ListenAddr *string `json:"listen_addr"`
		PanelURL   *string `json:"panel_url"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var in settingsReq
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
				return
			}
			if in.Password != nil && *in.Password != "" {
				if err := auth.SetPassword(*in.Password); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
					return
				}
			}
			if in.BasePath != nil {
				if _, err := setBasePath(*in.BasePath); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
					return
				}
			}
			if in.PanelURL != nil {
				u := strings.TrimSpace(*in.PanelURL)
				u = strings.TrimRight(u, "/")
				next := getWebSettings()
				next.PanelURL = u
				webSettingsMu.Lock()
				webSettingsCur = next
				webSettingsMu.Unlock()
				_ = saveWebSettings()
			}
			if in.Port != nil || in.ListenAddr != nil {
				next := getWebSettings()
				if in.Port != nil {
					next.Port = *in.Port
				}
				if in.ListenAddr != nil {
					next.ListenAddr = *in.ListenAddr
				}
				if err := srv.applyWebSettings(next); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
					return
				}
			}
		}

		cfg := getWebSettings()
		listen := cfg.ListenAddr
		if listen == "" {
			listen = "0.0.0.0"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"base_path":    currentBasePath(),
			"port":         cfg.Port,
			"listen_addr":  listen,
			"panel_url":    cfg.PanelURL,
			"has_password": true,
			"version":      version,
		})
	}
}

func apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	st, err := checkUpdate()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "检查更新失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "用 POST"})
		return
	}
	st, err := checkUpdate()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "检查更新失败: " + err.Error()})
		return
	}
	if !st.HasUpdate {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": false, "message": "已经是最新版"})
		return
	}
	if err := applyUpdate(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true, "latest": st.Latest})
}

func apiExits(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.ExitsOf())
	}
}

func apiProvision(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		count, err := strconv.Atoi(q.Get("count"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "count 参数无效"})
			return
		}
		tpl := 0
		if s := q.Get("template"); s != "" {
			if tpl, err = strconv.Atoi(s); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "template 参数无效"})
				return
			}
		}
		poolType := q.Get("type")
		if poolType == "" {
			poolType = "all"
		}
		job, err := m.Provision(ProvisionRequest{
			Region: q.Get("region"), Count: count, TemplateID: tpl, PoolType: poolType,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"job": job.ID()})
	}
}

// apiAddNodeBranch 为指定基础节点添加一个出站分支（从已有隧道或新建地区）
func apiAddNodeBranch(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tplID, err := strconv.Atoi(q.Get("template_id"))
		if err != nil || tplID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "template_id 参数无效"})
			return
		}
		host := strings.TrimSpace(q.Get("host"))
		region := strings.TrimSpace(q.Get("region"))

		if host != "" {
			tunnels := m.Tunnels()
			p, err := openPanel()
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			ports, err := p.CloneToTunnels(tplID, []string{host}, tunnels)
			invalidateInbounds()
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "created": ports})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": ports})
			return
		}

		if region != "" {
			job, err := m.Provision(ProvisionRequest{
				Region: region, Count: 1, TemplateID: tplID,
			})
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"job": job.ID()})
			return
		}

		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 host 或 region 参数"})
	}
}

func apiJobs(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.jobs.Views())
	}
}

func apiJobDismiss(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.jobs.Dismiss(r.URL.Query().Get("id"))
		writeJSON(w, http.StatusOK, map[string]string{"ok": "已关闭"})
	}
}

func apiXUIStatus(w http.ResponseWriter, r *http.Request) {
	p, err := openPanel()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    err.Error(),
		})
		return
	}
	resp := map[string]any{
		"available":  true,
		"kind":       p.Kind(),
		"describe":   p.Describe(),
		"can_create": true,
	}
	if x, ok := p.(*XUI); ok {
		resp["port"] = x.Port
		resp["base_path"] = x.BasePath
		resp["scheme"] = x.Scheme
		resp["host"] = x.Host
	}
	writeJSON(w, http.StatusOK, resp)
}

func apiPanelMode(workDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var in struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
				return
			}
			p, err := switchPanelMode(in.Mode)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			invalidateInbounds()
			writeJSON(w, http.StatusOK, map[string]any{
				"mode":     currentPanelMode(),
				"kind":     p.Kind(),
				"describe": p.Describe(),
			})
			return
		}
		resp := map[string]any{
			"mode":  currentPanelMode(),
			"modes": availablePanelModes(workDir),
		}
		if p, err := openPanel(); err == nil {
			resp["kind"] = p.Kind()
			resp["describe"] = p.Describe()
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func apiXUIInbounds(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := cachedInbounds(liveHosts(m))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

func liveHosts(m *Manager) map[string]bool {
	live := map[string]bool{}
	for _, t := range m.Tunnels() {
		if t.Status == "up" {
			live[sanitizeTag(t.Node.HostName)] = true
		}
	}
	return live
}

func apiXUIBind(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Query().Get("tag")
		if tag == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 tag 参数"})
			return
		}
		host := r.URL.Query().Get("host")
		x, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if err := x.Bind(tag, host, m.Tunnels()); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		invalidateInbounds()
		writeJSON(w, http.StatusOK, map[string]string{"ok": "已更新"})
	}
}

func apiXUIClone(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 参数无效"})
			return
		}

		tunnels := m.Tunnels()
		var hosts []string
		if raw := r.URL.Query().Get("hosts"); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if h := strings.TrimSpace(part); h != "" {
					hosts = append(hosts, h)
				}
			}
		} else {
			for _, t := range tunnels {
				if t.Status == "up" {
					hosts = append(hosts, t.Node.HostName)
				}
			}
		}
		if len(hosts) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有可用的隧道"})
			return
		}

		x, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		ports, err := x.CloneToTunnels(id, hosts, tunnels)
		invalidateInbounds()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "created": ports})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": ports})
	}
}

func apiXUIDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 参数无效"})
		return
	}
	x, err := openPanel()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	host := r.URL.Query().Get("host")
	if host == "" {
		host = publicHost(r)
	}
	detail, err := x.InboundDetail(id, host)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func publicHost(r *http.Request) string {
	if ip := hostPublicIP(); ip != "" {
		return ip
	}
	host := r.Host
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	if host == "" || host == "127.0.0.1" || host == "localhost" {
		return "<服务器IP>"
	}
	return host
}

func apiXUILinks(w http.ResponseWriter, r *http.Request) {
	x, err := openPanel()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	var ids []int
	if raw := r.URL.Query().Get("ids"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				ids = append(ids, n)
			}
		}
	} else {
		list, err := x.Inbounds(nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		for _, ib := range list {
			ids = append(ids, ib.ID)
		}
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有可导出的入站"})
		return
	}

	host := r.URL.Query().Get("host")
	if host == "" {
		host = publicHost(r)
	}
	links, err := x.InboundLinks(ids, host)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "links": links})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": links})
}

func apiXUIDelete(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ids []int
		for _, part := range strings.Split(r.URL.Query().Get("ids"), ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				ids = append(ids, n)
			}
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有指定要删除的入站"})
			return
		}
		x, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		err = x.DeleteInbounds(ids, m.Tunnels())
		invalidateInbounds()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": len(ids)})
	}
}

func apiInboundUpdate(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		q := r.URL.Query()
		id, err := strconv.Atoi(q.Get("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 参数无效"})
			return
		}

		var patch InboundPatch
		if v := q.Get("port"); v != "" {
			port, err := strconv.Atoi(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "端口无效"})
				return
			}
			patch.Port = &port
		}
		if q.Has("remark") {
			remark := q.Get("remark")
			patch.Remark = &remark
		}
		if v := q.Get("enable"); v != "" {
			enable := v == "1"
			patch.Enable = &enable
		}

		err = p.UpdateInbound(id, patch, m.Tunnels())
		invalidateInbounds()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "已保存"})
	}
}

func clientAction(m *Manager, what string,
	do func(p Panel, id int, email string, tunnels []*Tunnel) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 参数无效"})
			return
		}
		err = do(p, id, r.URL.Query().Get("email"), m.Tunnels())
		invalidateInbounds()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": what})
	}
}

func apiClientAdd(m *Manager) http.HandlerFunc {
	return clientAction(m, "已添加", func(p Panel, id int, email string, t []*Tunnel) error {
		return p.AddClient(id, email, t)
	})
}

func apiClientDelete(m *Manager) http.HandlerFunc {
	return clientAction(m, "已删除", func(p Panel, id int, email string, t []*Tunnel) error {
		return p.DeleteClient(id, email, t)
	})
}

func apiClientReset(m *Manager) http.HandlerFunc {
	return clientAction(m, "已重置", func(p Panel, id int, email string, t []*Tunnel) error {
		return p.ResetClient(id, email, t)
	})
}

func apiInboundCreate(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}

		q := r.URL.Query()
		port, _ := strconv.Atoi(q.Get("port"))
		ib, err := p.CreateInbound(NewInboundSpec{
			Protocol: q.Get("protocol"),
			Network:  q.Get("network"),
			Port:     port,
			Remark:   q.Get("remark"),
			Path:     q.Get("path"),
			Host:     q.Get("host"),
			Security: q.Get("security"),
			Vision:   q.Get("vision") == "1",

			ServerName: q.Get("sni"),
			CertFile:   q.Get("cert"),
			KeyFile:    q.Get("key"),

			Dest:        q.Get("dest"),
			ServerNames: q.Get("server_names"),
			ShortID:     q.Get("sid"),
			Fingerprint: q.Get("fp"),
		}, m.Tunnels())
		invalidateInbounds()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       ib.ID,
			"port":     ib.Port,
			"protocol": ib.Protocol,
			"remark":   ib.Remark,
			"network":  ib.Network,
			"security": ib.Security,
		})
	}
}

// handleSub 返回包含所有节点（直连与家宽分流）的完整订阅
func handleSub(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view := m.ExitsOf()

		var allLinks []string
		for _, n := range view.Nodes {
			for _, b := range n.Branches {
				for _, l := range b.Links {
					l = strings.TrimSpace(l)
					if l != "" {
						allLinks = append(allLinks, l)
					}
				}
			}
		}

		rawText := strings.Join(allLinks, "\n")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Profile-Update-Interval", "1")
		w.Header().Set("Subscription-Userinfo", "upload=0; download=0; total=1073741824000; expire=0")

		accept := r.Header.Get("Accept")
		isBrowser := strings.Contains(accept, "text/html")
		isRaw := r.URL.Query().Get("raw") == "1" || r.URL.Query().Get("b64") == "0"

		if isRaw || (isBrowser && r.URL.Query().Get("b64") != "1") {
			_, _ = w.Write([]byte(rawText + "\n"))
			return
		}

		b64 := base64.StdEncoding.EncodeToString([]byte(rawText))
		_, _ = w.Write([]byte(b64))
	}
}

func apiCustomSocksTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	var req struct {
		RawURL string `json:"raw_url"`
		Host   string `json:"host"`
		Port   int    `json:"port"`
		User   string `json:"user"`
		Pass   string `json:"pass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	h, p, u, pwd := req.Host, req.Port, req.User, req.Pass
	if req.RawURL != "" {
		parsedH, parsedP, parsedU, parsedPwd, _, err := ParseSocksURL(req.RawURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h, p, u, pwd = parsedH, parsedP, parsedU, parsedPwd
	}
	if h == "" || p <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主机地址与端口不能为空"})
		return
	}
	remoteAddr := fmt.Sprintf("%s:%d", h, p)
	exitIP, ping, ipType, isp, err := ProbeCustomSocks(remoteAddr, u, pwd, 8*time.Second)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("连通性测试失败: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"exit_ip": exitIP,
		"ping":    ping,
		"ip_type": ipType,
		"isp":     isp,
		"host":    h,
		"port":    p,
		"user":    u,
		"pass":    pwd,
	})
}

func apiCustomSocksAdd(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return
		}
		var req struct {
			RawURL      string `json:"raw_url"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			User        string `json:"user"`
			Pass        string `json:"pass"`
			Remark      string `json:"remark"`
			Country     string `json:"country"`
			CountryCode string `json:"country_code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h, p, u, pwd, remark := req.Host, req.Port, req.User, req.Pass, req.Remark
		if req.RawURL != "" {
			parsedH, parsedP, parsedU, parsedPwd, parsedRemark, err := ParseSocksURL(req.RawURL)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			h, p, u, pwd = parsedH, parsedP, parsedU, parsedPwd
			if remark == "" {
				remark = parsedRemark
			}
		}
		if h == "" || p <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主机地址与端口不能为空"})
			return
		}
		if remark == "" {
			remark = h
		}
		remoteAddr := fmt.Sprintf("%s:%d", h, p)
		exitIP, ping, ipType, isp, err := ProbeCustomSocks(remoteAddr, u, pwd, 10*time.Second)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("连接 SOCKS5 代理失败: %v", err)})
			return
		}

		country := req.Country
		if country == "" {
			country = "自定义"
		}
		countryCode := req.CountryCode
		if countryCode == "" {
			countryCode = "CUSTOM"
		}

		nodeID := fmt.Sprintf("custom-%s-%d", h, p)
		node := CustomNode{
			ID:          nodeID,
			HostName:    nodeID,
			Host:        h,
			Port:        p,
			User:        u,
			Pass:        pwd,
			Country:     country,
			CountryCode: countryCode,
			Remark:      remark,
			Ping:        ping,
			SpeedMbps:   100.0,
			ExitIP:      exitIP,
			IPType:      ipType,
			ISP:         isp,
		}

		t, err := m.AddCustomExit(node)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"slot":    t.Slot,
			"port":    t.Port,
			"exit_ip": exitIP,
			"ip_type": ipType,
			"isp":     isp,
		})
	}
}

func apiCustomSourceAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "源名称和 URL 不能为空"})
		return
	}

	nodes, err := FetchSourceNodes(req.URL, 12*time.Second)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("拉取源失败: %v", err)})
		return
	}

	resCount := 0
	dchCount := 0
	for _, n := range nodes {
		if n.IPType == "datacenter" {
			dchCount++
		} else {
			resCount++
		}
	}

	id := fmt.Sprintf("src-%d", time.Now().Unix())
	src := &CustomSource{
		ID:               id,
		Name:             req.Name,
		URL:              req.URL,
		Count:            len(nodes),
		Enabled:          true, // 默认添加时启用
		ResidentialCount: resCount,
		DatacenterCount:  dchCount,
		UpdatedAt:        time.Now(),
	}

	if globalCustomStore != nil {
		globalCustomStore.mu.Lock()
		globalCustomStore.Sources[id] = src
		for _, n := range nodes {
			n.SourceID = id
			globalCustomStore.Nodes[n.ID] = &n
		}
		globalCustomStore.mu.Unlock()
		_ = globalCustomStore.save()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"id":                id,
		"count":             len(nodes),
		"residential_count": resCount,
		"datacenter_count":  dchCount,
	})
}

func apiCustomSourceToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || globalCustomStore == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
		return
	}
	globalCustomStore.mu.Lock()
	src, ok := globalCustomStore.Sources[id]
	if !ok {
		globalCustomStore.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到该源"})
		return
	}
	src.Enabled = !src.Enabled
	enabled := src.Enabled
	globalCustomStore.mu.Unlock()
	_ = globalCustomStore.save()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
}

func apiCustomSourceList(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type SourceItem struct {
			ID               string    `json:"id"`
			Name             string    `json:"name"`
			URL              string    `json:"url"`
			Count            int       `json:"count"`
			Enabled          bool      `json:"enabled"`
			ResidentialCount int       `json:"residential_count"`
			DatacenterCount  int       `json:"datacenter_count"`
			UpdatedAt        time.Time `json:"updated_at"`
			IsBuiltin        bool      `json:"is_builtin"`
			Type             string    `json:"type"`
		}

		nodes, fetched := m.Nodes()
		list := []SourceItem{
			{
				ID:               "builtin-vpngate",
				Name:             "VPN Gate 官方全球家宽源",
				URL:              "https://www.vpngate.net/api/iphone/",
				Count:            len(nodes),
				Enabled:          true,
				ResidentialCount: len(nodes),
				DatacenterCount:  0,
				UpdatedAt:        fetched,
				IsBuiltin:        true,
				Type:             "vpngate",
			},
		}

		if globalCustomStore != nil {
			globalCustomStore.mu.RLock()
			for _, s := range globalCustomStore.Sources {
				resCount := 0
				dchCount := 0
				for _, n := range globalCustomStore.Nodes {
					if n.SourceID == s.ID {
						if n.IPType == "datacenter" {
							dchCount++
						} else {
							resCount++
						}
					}
				}
				s.ResidentialCount = resCount
				s.DatacenterCount = dchCount
				list = append(list, SourceItem{
					ID:               s.ID,
					Name:             s.Name,
					URL:              s.URL,
					Count:            s.Count,
					Enabled:          s.Enabled,
					ResidentialCount: resCount,
					DatacenterCount:  dchCount,
					UpdatedAt:        s.UpdatedAt,
					IsBuiltin:        false,
					Type:             "socks5",
				})
			}
			globalCustomStore.mu.RUnlock()
		}
		writeJSON(w, http.StatusOK, list)
	}
}

func apiCustomSourceDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
		return
	}
	if id == "builtin-vpngate" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "系统内置源不允许删除"})
		return
	}
	if globalCustomStore != nil {
		globalCustomStore.mu.Lock()
		delete(globalCustomStore.Sources, id)
		for k, n := range globalCustomStore.Nodes {
			if n.SourceID == id {
				delete(globalCustomStore.Nodes, k)
			}
		}
		globalCustomStore.mu.Unlock()
		_ = globalCustomStore.save()
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "已删除"})
}

func apiCustomSourceRefresh(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
			return
		}
		if id == "builtin-vpngate" {
			count, err := m.RefreshNodes()
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("刷新官方源失败: %v", err)})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": count})
			return
		}
		if globalCustomStore == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "存储未初始化"})
			return
		}
		globalCustomStore.mu.RLock()
		src, ok := globalCustomStore.Sources[id]
		globalCustomStore.mu.RUnlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到该源"})
			return
		}

		nodes, err := FetchSourceNodes(src.URL, 12*time.Second)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("拉取源失败: %v", err)})
			return
		}

		globalCustomStore.mu.Lock()
		src.Count = len(nodes)
		src.UpdatedAt = time.Now()
		for k, n := range globalCustomStore.Nodes {
			if n.SourceID == id {
				delete(globalCustomStore.Nodes, k)
			}
		}
		for _, n := range nodes {
			n.SourceID = id
			globalCustomStore.Nodes[n.ID] = &n
		}
		globalCustomStore.mu.Unlock()
		_ = globalCustomStore.save()

		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(nodes)})
	}
}

func apiCustomSourceImport(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" || globalCustomStore == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
			return
		}
		globalCustomStore.mu.RLock()
		var candidateNodes []CustomNode
		for _, n := range globalCustomStore.Nodes {
			if n.SourceID == id {
				candidateNodes = append(candidateNodes, *n)
			}
		}
		globalCustomStore.mu.RUnlock()

		if len(candidateNodes) == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "该源暂无节点"})
			return
		}

		type probeResult struct {
			node   CustomNode
			exitIP string
			ping   int
		}

		resCh := make(chan probeResult, 1)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		maxCandidates := 25
		if len(candidateNodes) < maxCandidates {
			maxCandidates = len(candidateNodes)
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		found := make(chan struct{})

		for i := 0; i < maxCandidates; i++ {
			node := candidateNodes[i]
			wg.Add(1)
			go func(n CustomNode) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				case <-found:
					return
				}
				defer func() { <-sem }()

				remoteAddr := fmt.Sprintf("%s:%d", n.Host, n.Port)
				ip, ping, err := ProbeCustomSocks(remoteAddr, n.User, n.Pass, 3500*time.Millisecond)
				if err == nil && ip != "" {
					select {
					case resCh <- probeResult{node: n, exitIP: ip, ping: ping}:
						close(found)
					default:
					}
				}
			}(node)
		}

		go func() {
			wg.Wait()
			close(resCh)
		}()

		res, ok := <-resCh
		if !ok || res.exitIP == "" {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "该源前 25 个候选节点均超时或离线，请尝试刷新源"})
			return
		}

		res.node.ExitIP = res.exitIP
		res.node.Ping = res.ping
		t, err := m.AddCustomExit(res.node)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"slot":    t.Slot,
			"port":    t.Port,
			"exit_ip": res.exitIP,
			"remark":  res.node.Remark,
		})
	}
}

