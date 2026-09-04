#!/usr/bin/env bash
# ================================================================
# sout Cloudflare隧道连接和Caddy流量代理管理模块 (Caddy + Cloudflared)
# 支持模式：
# 1. 自定义命名隧道 (带域名 + Token)
# 2. Cloudflare 官方免费临时隧道 (无需域名/无需Token，直接回车即开即用，动态实时同步)
# ================================================================

CADDY_DIR="/etc/caddy"
CADDY_FILE="/etc/caddy/Caddyfile"
WORK_DIR="/var/lib/sout"
CADDY_META="/var/lib/sout/caddy_meta.json"
SUI_DB="/usr/local/s-ui/db/s-ui.db"
DOMAIN_FILE="/var/lib/sout/tunnel_domain"
QUICK_SCRIPT="/usr/local/bin/sout-quick-tunnel"

get_arch() {
  local arch
  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "amd64" ;;
  esac
}

install_caddy() {
  if command -v caddy >/dev/null 2>&1; then
    return 0
  fi
  echo "  [+] 正在获取标准 Caddy 反代服务 ($(get_arch))..."
  local arch
  arch=$(get_arch)
  local caddy_url="https://github.com/caddyserver/caddy/releases/download/v2.11.4/caddy_2.11.4_linux_${arch}.tar.gz"
  
  local tmp_dir
  tmp_dir=$(mktemp -d)
  if curl -sSL -f "$caddy_url" -o "${tmp_dir}/caddy.tar.gz"; then
    tar -zxf "${tmp_dir}/caddy.tar.gz" -C "$tmp_dir"
    install -m 755 "${tmp_dir}/caddy" /usr/local/bin/caddy
    rm -rf "$tmp_dir"
  else
    rm -rf "$tmp_dir"
    echo "  [!] 官方预编译二进制下载受阻，尝试使用包管理器安装 Caddy..."
    if command -v apt-get >/dev/null 2>&1; then
      apt-get update -qq && apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl
      curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>/dev/null || true
      curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null 2>&1 || true
      apt-get update -qq && apt-get install -y -qq caddy
    fi
  fi

  if ! command -v caddy >/dev/null 2>&1; then
    echo "  [✗] Caddy 安装失败，请检查网络连接。"
    return 1
  fi
  echo "  [✓] Caddy 服务已就绪"
}

install_cloudflared() {
  if command -v cloudflared >/dev/null 2>&1; then
    return 0
  fi
  echo "  [+] 正在获取 Cloudflare 官方隧道客户端 cloudflared ($(get_arch))..."
  local arch
  arch=$(get_arch)
  local cf_url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${arch}"
  if curl -sSL -L -f "$cf_url" -o /usr/local/bin/cloudflared; then
    chmod +x /usr/local/bin/cloudflared
    echo "  [✓] cloudflared 已成功安装至 /usr/local/bin/cloudflared"
  else
    echo "  [✗] cloudflared 下载失败，请检查网络连接。"
    return 1
  fi
}

create_quick_tunnel_daemon() {
  cat > "$QUICK_SCRIPT" <<'EOF'
#!/usr/bin/env bash
PORT="${1:-8081}"
LOG_FILE="/var/log/cloudflared_quick.log"
DOMAIN_FILE="/var/lib/sout/tunnel_domain"
META_FILE="/var/lib/sout/caddy_meta.json"
SUI_DB="/usr/local/s-ui/db/s-ui.db"

mkdir -p /var/lib/sout /var/log
> "$LOG_FILE"

/usr/local/bin/cloudflared tunnel --url "http://127.0.0.1:${PORT}" --no-autoupdate 2>&1 | tee -a "$LOG_FILE" &
CF_PID=$!

CUR_DOMAIN=""
while kill -0 "$CF_PID" 2>/dev/null; do
  NEW_DOMAIN=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' "$LOG_FILE" | tail -1 | sed 's|https://||' | tr -d ' 
')
  if [[ -n "$NEW_DOMAIN" && "$NEW_DOMAIN" != "$CUR_DOMAIN" ]]; then
    CUR_DOMAIN="$NEW_DOMAIN"
    echo "$CUR_DOMAIN" > "$DOMAIN_FILE"

    if [[ -f "$META_FILE" ]]; then
      python3 -c "
import json
try:
    with open('$META_FILE') as f: d = json.load(f)
    d['domain'] = '$CUR_DOMAIN'
    with open('$META_FILE', 'w') as f: json.dump(d, f, indent=2)
except Exception: pass
" 2>/dev/null || true
    fi

    if [[ -f /var/lib/sout/settings.json ]]; then
      python3 -c "
import json
try:
    with open('/var/lib/sout/settings.json') as f: d = json.load(f)
    d['panel_url'] = 'https://$CUR_DOMAIN'
    with open('/var/lib/sout/settings.json', 'w') as f: json.dump(d, f, indent=2)
except Exception: pass
" 2>/dev/null || true
      systemctl restart sout 2>/dev/null || true
    fi

      if [[ -f "$SUI_DB" ]]; then
        sui_token=$(cat "/var/lib/sout/sui-token" 2>/dev/null || true)
        if [[ -n "$sui_token" ]]; then
          SUI_TOKEN="$sui_token" SUI_DB="$SUI_DB" CUR_DOMAIN="$CUR_DOMAIN" python3 <<'PY'
import json, os, sqlite3, urllib.request, urllib.parse

TOKEN = os.environ['SUI_TOKEN']
con = sqlite3.connect(os.environ['SUI_DB'])
cur = con.cursor()
cur.execute("SELECT value FROM settings WHERE key='webPath'")
wp = cur.fetchone()
wp_str = wp[0] if wp else '/app/'
cur.execute("SELECT value FROM settings WHERE key='subPath'")
sp = cur.fetchone()
sp_str = sp[0] if sp else '/sub/'
cur.execute("SELECT value FROM settings WHERE key='webPort'")
wport = cur.fetchone()
wport = wport[0] if wport and wport[0] else '8443'
con.close()

if not wp_str.startswith('/'):
    wp_str = '/' + wp_str
if not wp_str.endswith('/'):
    wp_str += '/'
if not sp_str.startswith('/'):
    sp_str = '/' + sp_str
if not sp_str.endswith('/'):
    sp_str += '/'

BASE = f'http://127.0.0.1:{wport}{wp_str}apiv2'
domain = os.environ['CUR_DOMAIN']

def api(method, endpoint, form=None):
    url = BASE.rstrip('/') + '/' + endpoint.lstrip('/')
    data = urllib.parse.urlencode(form).encode() if form else None
    headers = {'Token': TOKEN}
    if data is not None:
        headers['Content-Type'] = 'application/x-www-form-urlencoded'
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=20) as resp:
        return json.loads(resp.read().decode('utf-8'))

# 更新 webURI/subURI
settings_data = {
    'webURI': f'https://{domain}{wp_str}',
    'subURI': f'https://{domain}{sp_str}',
}
api('POST', 'save', {
    'object': 'settings',
    'action': 'set',
    'data': json.dumps(settings_data),
})

# 更新 vmess-argo 入站 addrs（域名变化）
inbounds_resp = api('GET', 'inbounds')
inbound_id = None
for row in inbounds_resp.get('obj', {}).get('inbounds') or []:
    t = row.get('tag', '')
    if t == 'vmess-argo' or t.startswith('vmess-argo-'):
        inbound_id = row.get('id')
        break
if inbound_id:
    addrs_data = [{
        'server': domain,
        'server_port': 443,
        'tls': {
            'disable_sni': False,
            'enabled': True,
            'insecure': False,
            'server_name': domain,
            'utls': {
                'enabled': True,
                'fingerprint': 'chrome'
            }
        }
    }]
    api('POST', 'save', {
        'object': 'inbounds',
        'action': 'edit',
        'data': json.dumps({
            'id': inbound_id,
            'addrs': addrs_data,
        }),
    })
PY
          systemctl restart s-ui 2>/dev/null || true
        fi
      fi
  fi
  sleep 3
done
wait "$CF_PID"
EOF
  chmod +x "$QUICK_SCRIPT"
}

setup_cloudflared_service() {
  local token="$1"
  local tun_p="${2:-8081}"

  if [[ -n "$token" ]]; then
    cat > /etc/systemd/system/cloudflared.service <<EOF
[Unit]
Description=Cloudflare Named Tunnel Agent
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/cloudflared tunnel --protocol quic --no-autoupdate run --token ${token}
Restart=always
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  else
    create_quick_tunnel_daemon
    cat > /etc/systemd/system/cloudflared.service <<EOF
[Unit]
Description=Cloudflare Quick Tunnel Dynamic Daemon
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${QUICK_SCRIPT} ${tun_p}
Restart=always
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  fi

  systemctl daemon-reload
  systemctl enable cloudflared >/dev/null 2>&1 || true
  systemctl restart cloudflared
}

setup_caddy_service() {
  cat > /etc/systemd/system/caddy.service <<EOF
[Unit]
Description=Caddy Web Server
Documentation=https://caddyserver.com/docs/
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=notify
User=root
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable caddy >/dev/null 2>&1 || true
}

rand_path() {
  local prefix="$1"
  local hex
  hex=$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' 
' | cut -c1-8)
  [[ -z "$hex" ]] && hex="$(date +%s%N | cut -c1-8)"
  echo "${prefix}${hex}"
}

rand_port() {
  local p
  while true; do
    p=$(( 30000 + RANDOM % 20000 ))
    if ! ss -tulpn 2>/dev/null | grep -q ":${p} "; then
      echo "$p"
      return
    fi
  done
}

get_quick_tunnel_domain() {
  local max_wait=20
  local d=""
  for ((i=1; i<=max_wait; i++)); do
    if [[ -f "$DOMAIN_FILE" ]]; then
      d=$(cat "$DOMAIN_FILE" | tr -d ' 
')
      [[ -n "$d" ]] && break
    fi
    d=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' /var/log/cloudflared_quick.log 2>/dev/null | tail -1 | sed 's|https://||' | tr -d ' 
')
    [[ -n "$d" ]] && break
    sleep 1
  done
  echo "$d"
}

setup_caddy_proxy() {
  local domain="${1:-}"
  local tunnel_token="${2:-}"
  local tunnel_port="${3:-8081}"

  local is_quick="false"
  if [[ -z "$domain" && -z "$tunnel_token" ]]; then
    is_quick="true"
  fi

  echo
  echo "================================================================"
  if [[ "$is_quick" == "true" ]]; then
    echo "  正在配置 Cloudflare 官方免费临时隧道 (免域名 / 免Token)..."
  else
    echo "  正在配置 Cloudflare隧道连接和Caddy流量代理 (${domain})..."
  fi
  echo "================================================================"

  install_caddy || return 1
  install_cloudflared || return 1

  mkdir -p "$CADDY_DIR"
  mkdir -p "$WORK_DIR"

  local cur_backend
  cur_backend=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
  if [[ -z "$cur_backend" ]]; then
    if [[ -f "$SUI_DB" ]]; then cur_backend="s-ui"
    elif [[ -f /etc/sing-box/config.json ]]; then cur_backend="sing-box"
    fi
  fi

  local sout_p sui_p sub_p ws_p sout_port sui_port sub_port node_port
  sout_p=$(rand_path "sout")
  sui_p=""
  [[ "$cur_backend" != "sing-box" ]] && sui_p=$(rand_path "sui")
  sub_p=$(rand_path "sub")
  ws_p=$(rand_path "vlws")

  sout_port=$(rand_port)
  sui_port=0
  [[ "$cur_backend" != "sing-box" ]] && sui_port=$(rand_port)
  sub_port=$(rand_port)
  node_port=$(rand_port)

  # 构建 Caddyfile 中可选的 s-ui 规则块
  local sui_caddy_block=""
  if [[ -n "$sui_p" && "$sui_port" -gt 0 ]]; then
    sui_caddy_block="    redir /${sui_p} /${sui_p}/ 308

    # 2. s-ui 节点管理面板
    handle /${sui_p}* {
        reverse_proxy 127.0.0.1:${sui_port}
    }"
  fi

  # 生成通配监听的 Caddyfile
  cat > "$CADDY_FILE" <<EOF
{
    admin off
    auto_https off
}

:${tunnel_port} {
    redir /${sout_p} /${sout_p}/ 308
    redir /${sub_p} /${sub_p}/ 308
${sui_caddy_block}

    # 1. sout 动态家宽管理面板
    handle /${sout_p}* {
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 3. sout 订阅接口（重写到 sout 面板的 /sub）
    handle /${sub_p}* {
        rewrite * /${sout_p}/sub{uri}
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 4. WebSocket 节点 (实时零缓冲透传)
    handle /${ws_p}* {
        reverse_proxy 127.0.0.1:${node_port} {
            flush_interval -1
        }
    }

    # 5. 伪装根路径
    handle {
        respond "Service Ready" 200
    }
}
EOF

  setup_caddy_service
  systemctl restart caddy

  echo "  [+] 正在启动本地 Caddy 分流服务与 Cloudflare 隧道..."
  setup_cloudflared_service "$tunnel_token" "$tunnel_port"

  if [[ "$is_quick" == "true" ]]; then
    echo "  [+] 正在等待 Cloudflare 分配免费临时域名..."
    domain=$(get_quick_tunnel_domain)
    if [[ -z "$domain" ]]; then
      echo "  [!] 暂未即时获取到临时域名，稍后可通过 sout 查看。"
      domain="临时隧道连接中.trycloudflare.com"
    else
      echo "  [✓] 成功获取免费临时域名: https://${domain}"
    fi
  fi

  local sui_u="admin"
  if [[ "$cur_backend" == "sing-box" ]]; then
    # sing-box 纯内核模式：直接向 /etc/sing-box/config.json 注入 127.0.0.1 隧道节点
    DOMAIN="$domain" NODE_PORT="$node_port" WS_PATH="/${ws_p}" python3 <<'PYEOF'
import json, os, uuid, random, string
p = '/etc/sing-box/config.json'
try:
    with open(p, 'r') as f:
        conf = json.load(f)
except Exception:
    conf = {}
inbounds = conf.setdefault('inbounds', [])
node_port = int(os.environ['NODE_PORT'])
ws_path = os.environ['WS_PATH']
domain = os.environ['DOMAIN']

rand_suffix = "".join(random.choices(string.ascii_lowercase + string.digits, k=4))
inbounds.append({
    'type': 'vmess',
    'tag': f'vmess-argo-{rand_suffix}',
    'listen': '127.0.0.1',
    'listen_port': node_port,
    'users': [{'name': 'default', 'uuid': str(uuid.uuid4())}],
    'transport': {
        'type': 'ws',
        'path': ws_path,
        'headers': {'Host': domain},
        'max_early_data': 2560,
        'early_data_header_name': 'Sec-WebSocket-Protocol'
    }
})
with open(p, 'w') as f:
    json.dump(conf, f, indent=2)
PYEOF
    systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true
    echo -e "  [✓] 已在 sing-box 中创建 127.0.0.1 隧道回源节点 (端口: ${node_port}, 路径: /${ws_p}/)"
  else
    # 配置 s-ui 面板（通过 s-ui API，避免直接写库）
    if [[ -f "$SUI_DB" ]]; then
      local sui_token
      sui_token=$(cat "/var/lib/sout/sui-token" 2>/dev/null || true)
      if [[ -z "$sui_token" ]]; then
        echo "  [!] 未找到 s-ui API Token，跳过 s-ui 设置更新"
      else
        SUI_API="http://127.0.0.1:${sui_port}/${sui_p}/apiv2" \
        SUI_TOKEN="$sui_token" \
        SUI_DB="$SUI_DB" \
        DOMAIN="$domain" \
        SUI_PORT="$sui_port" \
        SUB_PORT="$sub_port" \
        SUI_PATH="/${sui_p}/" \
        SUB_PATH="/${sub_p}/" \
        python3 <<'PY'
import json, os, sqlite3, urllib.request, urllib.parse
con = sqlite3.connect(os.environ['SUI_DB'])
cur = con.cursor()
cur.execute("SELECT value FROM settings WHERE key='webPort'")
cur_port_row = cur.fetchone()
cur_port = cur_port_row[0] if cur_port_row and cur_port_row[0] else '8443'
cur.execute("SELECT value FROM settings WHERE key='webPath'")
cur_path_row = cur.fetchone()
cur_path = cur_path_row[0] if cur_path_row and cur_path_row[0] else '/app/'
if not cur_path.startswith('/'): cur_path = '/' + cur_path
if not cur_path.endswith('/'): cur_path += '/'
con.close()
BASE = f'http://127.0.0.1:{cur_port}{cur_path}apiv2'
TOKEN = os.environ['SUI_TOKEN']
settings_data = {
    'webPort': str(os.environ.get('SUI_PORT', '')),
    'webListen': '127.0.0.1',
    'webPath': os.environ['SUI_PATH'],
    'webURI': f'https://{os.environ["DOMAIN"]}{os.environ["SUI_PATH"]}',
    'subPort': str(os.environ.get('SUB_PORT', '')),
    'subListen': '127.0.0.1',
    'subPath': os.environ['SUB_PATH'],
    'subURI': f'https://{os.environ["DOMAIN"]}{os.environ["SUB_PATH"]}',
    'webCertFile': '',
    'webKeyFile': '',
    'subCertFile': '',
    'subKeyFile': '',
}
form = urllib.parse.urlencode({
    'object': 'settings',
    'action': 'set',
    'data': json.dumps(settings_data),
}).encode()
req = urllib.request.Request(BASE.rstrip('/') + '/save', data=form, headers={'Token': TOKEN, 'Content-Type': 'application/x-www-form-urlencoded'})
with urllib.request.urlopen(req, timeout=20) as resp:
    resp.read()
PY
        systemctl restart s-ui 2>/dev/null || true
      fi
    fi
    [[ -z "$sui_u" ]] && sui_u="admin"
  fi

  # 配置 sout 服务
  local sout_json="${WORK_DIR}/settings.json"
  cat > "$sout_json" <<EOF
{
  "port": ${sout_port},
  "listen_addr": "127.0.0.1",
  "panel_url": "https://${domain}",
  "ssl_enabled": false
}
EOF
  echo "$sout_p" > "${WORK_DIR}/basepath"
  systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true

  # 保存 meta
  local meta_mode="tunnel"
  [[ "$is_quick" == "true" ]] && meta_mode="quick_tunnel"
  cat > "$CADDY_META" <<EOF
{
  "enabled": true,
  "mode": "${meta_mode}",
  "domain": "${domain}",
  "tunnel_token": "${tunnel_token}",
  "tunnel_port": ${tunnel_port},
  "sout_port": ${sout_port},
  "sout_path": "${sout_p}",
  "sui_port": ${sui_port},
  "sui_path": "${sui_p}",
  "sui_user": "${sui_u}",
  "sub_port": ${sub_port},
  "sub_path": "${sub_p}",
  "node_port": ${node_port},
  "ws_path": "${ws_p}"
}
EOF

  local pw
  pw=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 password 文件")

  echo
  echo "================================================================"
  if [[ "$is_quick" == "true" ]]; then
    echo "  🎉 Cloudflare 官方免费临时隧道连接和Caddy流量代理已成功开启！"
  else
    echo "  🎉 Cloudflare隧道连接和Caddy流量代理已成功开启！"
  fi
  echo "================================================================"
  echo "  访问域名:      https://${domain}"
  echo "  隧道服务:      cloudflared (运行中 / active)"
  echo "  本地回源端口:  127.0.0.1:${tunnel_port}"
  echo "  ----------------------------------------------------------------"
  echo "  [1] sout 家宽动态出口插件面板"
  echo "      访问地址:  https://${domain}/${sout_p}/"
  echo "      访问口令:  ${pw}"
  echo
  if [[ "$cur_backend" == "sing-box" ]]; then
    echo "  [2] sing-box 原生内核"
    echo "      运行后端:  sing-box 原生内核"
    echo "      核心配置:  /etc/sing-box/config.json"
    echo "      运行状态:  $(systemctl is-active sing-box 2>/dev/null || rc-service sing-box status 2>/dev/null || echo 'active')"
  else
    echo "  [2] s-ui 节点与分流管理面板"
    echo "      访问地址:  https://${domain}/${sui_p}/"
    echo "      管理账号:  ${sui_u}"
    echo "      管理密码:  [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
  fi
  echo
  echo "  [3] sout 订阅地址:  https://${domain}/${sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
  echo "================================================================"
  echo
}

reload_caddy_proxy() {
  if [[ ! -f "$CADDY_META" ]] || ! grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META" 2>/dev/null; then
    echo
    echo -e "  ${Y}================================================================${N}"
    echo -e "  ${Y}💡 提示: 检测到您尚未配置 Cloudflare 隧道连接${N}"
    echo -e "  ${Y}请先在菜单中选择配置隧道完成隧道配置，后再使用此功能。${N}"
    echo -e "  ${Y}================================================================${N}"
    return
  fi

  echo
  echo -e "  ${B}[+] 正在扫描并重新识别各组件 (隧道/sout/s-ui/节点) 最新路径与端口...${N}"

  local domain tunnel_port sout_p sui_p sub_p ws_p sout_port sui_port node_port meta_mode
  meta_mode=$(grep -oE '"mode"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  domain=$(grep -oE '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  tunnel_port=$(grep -oE '"tunnel_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')

  # 0. 动态探测 Cloudflare 隧道的实时端口与活跃域名
  if [[ -f /etc/systemd/system/cloudflared.service ]]; then
    local probed_cf_port
    probed_cf_port=$(grep -oE 'http://127\.0\.0\.1:[0-9]+' /etc/systemd/system/cloudflared.service 2>/dev/null | awk -F: '{print $3}' | head -1)
    [[ -n "$probed_cf_port" ]] && tunnel_port="$probed_cf_port"
  fi
  [[ -z "$tunnel_port" ]] && tunnel_port="8081"

  if [[ "$meta_mode" == "quick_tunnel" ]] || [[ "$domain" == *trycloudflare.com* ]]; then
    echo -e "  [+] 正在探测 Cloudflare 临时隧道当前活跃域名..."
    local cur_quick_dom
    cur_quick_dom=$(get_quick_tunnel_domain)
    if [[ -n "$cur_quick_dom" ]]; then
      domain="$cur_quick_dom"
      echo -e "  ${G}[✓] 成功识别到当前活跃域名: https://${domain}${N}"
    else
      echo -e "  ${D}[*] 沿用已有隧道域名: https://${domain}${N}"
    fi
  else
    echo -e "  [+] 识别到固定托管域名: https://${domain} (回源端口: ${tunnel_port})"
  fi

  # 1. 动态探测并自动纠偏 sout 面板配置 (确保监听 127.0.0.1 并更新完整 URL)
  sout_port="8899"
  local sout_needs_restart=0
  if [[ -f "${WORK_DIR}/settings.json" ]]; then
    local sout_info
    sout_info=$(python3 -c "
import json
try:
    with open('${WORK_DIR}/settings.json') as f:
        d = json.load(f)
    p = d.get('port', 8899)
    la = d.get('listen_addr', '')
    purl = d.get('panel_url', '')
    target_url = 'https://${domain}'
    changed = False
    if la != '127.0.0.1':
        d['listen_addr'] = '127.0.0.1'
        changed = True
    if purl != target_url:
        d['panel_url'] = target_url
        changed = True
    if changed:
        with open('${WORK_DIR}/settings.json', 'w') as f:
            json.dump(d, f, indent=2)
    print(f'{p}|{changed}')
except Exception:
    print('8899|False')
" 2>/dev/null || echo "8899|False")
    sout_port=$(echo "$sout_info" | cut -d'|' -f1)
    if [[ "$(echo "$sout_info" | cut -d'|' -f2)" == "True" ]]; then
      sout_needs_restart=1
    fi
  fi
  sout_p=$(grep -oE '"sout_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  [[ -z "$sout_p" ]] && sout_p="sout"
  if [[ "$sout_needs_restart" -eq 1 ]]; then
    echo -e "  [+] 检测到 sout 监听地址/面板URL需要更新，已自动修正并重启服务..."
    systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true
  fi

  # 2. 动态探测并自动纠偏后端配置
  local cur_backend
  cur_backend=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
  if [[ -z "$cur_backend" ]]; then
    if [[ -f "$SUI_DB" ]]; then cur_backend="s-ui"
    elif [[ -f /etc/sing-box/config.json ]]; then cur_backend="sing-box"
    fi
  fi

  sui_port="2096"
  sui_p=""
  [[ "$cur_backend" != "sing-box" ]] && sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  [[ "$cur_backend" != "sing-box" && -z "$sui_p" ]] && sui_p="sui"
  sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  [[ -z "$sub_p" ]] && sub_p="sub"

  local sui_needs_restart=0
  if [[ "$cur_backend" != "sing-box" && -f /usr/local/s-ui/db/s-ui.db ]]; then
    local sui_info
    sui_info=$(python3 -c "
import sqlite3
con = sqlite3.connect('/usr/local/s-ui/db/s-ui.db')
cur = con.cursor()
cur.execute(\"SELECT value FROM settings WHERE key='webPort'\")
r1 = cur.fetchone()
port = r1[0] if r1 and r1[0] else '2096'
cur.execute(\"SELECT value FROM settings WHERE key='webPath'\")
r2 = cur.fetchone()
path = r2[0] if r2 and r2[0] else 'sui'
cur.execute(\"SELECT value FROM settings WHERE key='webListen'\")
r3 = cur.fetchone()
w_listen = r3[0] if r3 and r3[0] else ''
cur.execute(\"SELECT value FROM settings WHERE key='subListen'\")
r4 = cur.fetchone()
s_listen = r4[0] if r4 and r4[0] else ''

target_web_uri = 'https://${domain}/' + path.strip('/') + '/'
target_sub_uri = 'https://${domain}/${sub_p}/'

changed = False
if w_listen != '127.0.0.1':
    cur.execute(\"UPDATE settings SET value='127.0.0.1' WHERE key='webListen'\")
    changed = True
if s_listen != '127.0.0.1':
    cur.execute(\"UPDATE settings SET value='127.0.0.1' WHERE key='subListen'\")
    changed = True

cur.execute(\"UPDATE settings SET value=? WHERE key='webURI'\", (target_web_uri,))
cur.execute(\"UPDATE settings SET value=? WHERE key='subURI'\", (target_sub_uri,))

if changed:
    con.commit()
con.close()
path = path.strip('/')
print(f'{port}|{path}|{changed}')
" 2>/dev/null || true)
    if [[ -n "$sui_info" ]]; then
      local probed_port probed_path probed_changed
      probed_port=$(echo "$sui_info" | cut -d'|' -f1)
      probed_path=$(echo "$sui_info" | cut -d'|' -f2)
      probed_changed=$(echo "$sui_info" | cut -d'|' -f3)
      [[ -n "$probed_port" ]] && sui_port="$probed_port"
      [[ -n "$probed_path" ]] && sui_p="$probed_path"
      if [[ "$probed_changed" == "True" ]]; then
        sui_needs_restart=1
      fi
    fi
  fi
  if [[ "$sui_needs_restart" -eq 1 ]]; then
    echo -e "  [+] 检测到 s-ui 监听地址不是 127.0.0.1，已自动修正为 127.0.0.1 并重启服务..."
    systemctl restart s-ui 2>/dev/null || true
  fi

  # 3. 动态识别/创建隧道节点入站 (核心：识别监听在 127.0.0.1 的隧道节点)
  sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  [[ -z "$sub_p" ]] && sub_p="sub"
  ws_p=$(grep -oE '"ws_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  node_port=$(grep -oE '"node_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
  [[ -z "$node_port" ]] && node_port="2082"

  if [[ "$cur_backend" == "sing-box" ]]; then
    local sb_node_res
    sb_node_res=$(python3 -c "
import json
sb_conf = '/etc/sing-box/config.json'
try:
    with open(sb_conf, 'r') as f:
        d = json.load(f)
except Exception:
    d = {}
found_p, found_w = '', ''
for ib in d.get('inbounds', []):
    if not isinstance(ib, dict): continue
    if ib.get('listen') == '127.0.0.1' and ib.get('listen_port', 0) > 0:
        found_p = str(ib.get('listen_port'))
        tr = ib.get('transport', {})
        found_w = tr.get('path', '').strip('/') if isinstance(tr, dict) else ''
        break
if found_p:
    print(f'FOUND|{found_p}|{found_w or \"vlws\"}')
else:
    print('CREATE')
" 2>/dev/null || echo "CREATE")

    if [[ "$sb_node_res" == FOUND* ]]; then
      local n_port n_path
      n_port=$(echo "$sb_node_res" | cut -d'|' -f2)
      n_path=$(echo "$sb_node_res" | cut -d'|' -f3)
      [[ -n "$n_port" ]] && node_port="$n_port"
      [[ -n "$n_path" ]] && ws_p="$n_path"
      echo -e "  [+] 成功识别到 sing-box 现有 127.0.0.1 隧道节点 (端口: ${node_port}, 路径: /${ws_p}/)"
    else
      echo -e "  [+] sing-box 中未找到 127.0.0.1 隧道节点，正在自动创建..."
      [[ -z "$ws_p" ]] && ws_p=$(rand_safe_path "vlws")
      node_port=$(rand_port)
      DOMAIN="$domain" NODE_PORT="$node_port" WS_PATH="/${ws_p}" python3 <<'PYEOF'
import json, os, uuid, random, string
p = '/etc/sing-box/config.json'
try:
    with open(p, 'r') as f:
        conf = json.load(f)
except Exception:
    conf = {}
inbounds = conf.setdefault('inbounds', [])
node_port = int(os.environ['NODE_PORT'])
ws_path = os.environ['WS_PATH']
domain = os.environ['DOMAIN']

rand_suffix = "".join(random.choices(string.ascii_lowercase + string.digits, k=4))
inbounds.append({
    'type': 'vmess',
    'tag': f'vmess-argo-{rand_suffix}',
    'listen': '127.0.0.1',
    'listen_port': node_port,
    'users': [{'name': 'default', 'uuid': str(uuid.uuid4())}],
    'transport': {
        'type': 'ws',
        'path': f'/{ws_path}',
        'headers': {'Host': domain},
        'max_early_data': 2560,
        'early_data_header_name': 'Sec-WebSocket-Protocol'
    }
})
with open(p, 'w') as f:
    json.dump(conf, f, indent=2)
PYEOF
      systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true
      echo -e "  ${G}[✓] 127.0.0.1 隧道节点已在 sing-box 中创建 (端口: ${node_port}, 路径: /${ws_p}/)${N}"
    fi

  elif [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
    local node_result
    node_result=$(python3 -c "
import sqlite3, json

def to_str(v):
    if isinstance(v, bytes): return v.decode('utf-8', errors='replace')
    return str(v) if v is not None else ''

db_path = '/usr/local/s-ui/db/s-ui.db'
con = sqlite3.connect(db_path)
cur = con.cursor()
cur.execute('SELECT id, type, tag, options, addrs FROM inbounds')
rows = cur.fetchall()
con.close()

found_port = ''
found_path = ''

# 遍历寻找 listen 为 127.0.0.1 的隧道入站节点
for r in rows:
    ib_id = r[0]
    typ = to_str(r[1])
    tag = to_str(r[2])
    opt_raw = to_str(r[3])
    addrs_raw = to_str(r[4])
    
    try:
        opt = json.loads(opt_raw) if opt_raw else {}
    except Exception:
        opt = {}
        
    listen_ip = opt.get('listen', '')
    port = opt.get('listen_port', 0)
    tr = opt.get('transport', {})
    path = tr.get('path', '') if isinstance(tr, dict) else ''
    
    # 只要节点监听在 127.0.0.1 且端口有效
    if listen_ip == '127.0.0.1' and port > 0:
        found_port = str(port)
        if path:
            found_path = path.strip('/')
        else:
            found_path = 'vlws'
        break

if found_port:
    print(f'FOUND|{found_port}|{found_path}')
else:
    print('CREATE')
" 2>/dev/null || echo "CREATE")

    if [[ "$node_result" == FOUND* ]]; then
      local n_port n_path
      n_port=$(echo "$node_result" | cut -d'|' -f2)
      n_path=$(echo "$node_result" | cut -d'|' -f3)
      [[ -n "$n_port" ]] && node_port="$n_port"
      [[ -n "$n_path" ]] && ws_p="$n_path"
      echo -e "  [+] 成功识别到已存在的 127.0.0.1 隧道节点 (端口: ${node_port}, 路径: /${ws_p}/)"
    else
      echo -e "  [+] 未找到监听在 127.0.0.1 的隧道节点，正在自动依据模板创建..."
      local sui_token
      sui_token=$(cat "${WORK_DIR}/sui-token" 2>/dev/null || true)
      local sui_admin_user
      sui_admin_user=$(get_sui_user)
      if [[ -z "$ws_p" ]]; then
        ws_p=$(rand_safe_path "vlws")
      fi
      node_port=$(rand_port)
      
      SUI_API="http://127.0.0.1:${sui_port}/${sui_p}/apiv2" \
      SUI_TOKEN="$sui_token" \
      SUI_DB="/usr/local/s-ui/db/s-ui.db" \
      DOMAIN="$domain" \
      NODE_PORT="$node_port" \
      WS_PATH="/${ws_p}" \
      SUI_ADMIN_USER="$sui_admin_user" \
      python3 <<'PYEOF'
import json, os, uuid, urllib.request, urllib.parse

BASE = os.environ['SUI_API']
TOKEN = os.environ.get('SUI_TOKEN', '')

def api(method, endpoint, form=None):
    url = BASE.rstrip('/') + '/' + endpoint.lstrip('/')
    data = urllib.parse.urlencode(form).encode() if form else None
    headers = {'Token': TOKEN}
    if data is not None:
        headers['Content-Type'] = 'application/x-www-form-urlencoded'
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read().decode('utf-8'))

try:
    inbounds_resp = api('GET', 'inbounds') or {}
    inbounds_obj = inbounds_resp.get('obj') or []
    if isinstance(inbounds_obj, dict):
        inbound_rows = inbounds_obj.get('inbounds') or []
    else:
        inbound_rows = inbounds_obj or []
    inbound_rows = [r for r in inbound_rows if isinstance(r, dict)]

    node_tag = None
    existing_id = None
    for r in inbound_rows:
        t = r.get('tag', '')
        if t == 'vmess-argo' or t.startswith('vmess-argo-'):
            node_tag = t
            existing_id = r.get('id')
            break

    if not node_tag:
        import string, random
        chars = string.ascii_lowercase + string.digits
        rand_suffix = "".join(random.choices(chars, k=4))
        node_tag = f"vmess-argo-{rand_suffix}"

    client_uuid = str(uuid.uuid4())
    addrs_data = [{
        'server': os.environ['DOMAIN'],
        'server_port': 443,
        'tls': {
            'disable_sni': False,
            'enabled': True,
            'insecure': False,
            'server_name': os.environ['DOMAIN'],
            'utls': {'enabled': True, 'fingerprint': 'chrome'}
        }
    }]
    inbound_payload = {
        'id': existing_id or 0,
        'type': 'vmess',
        'tag': node_tag,
        'tls_id': 0,
        'listen': '127.0.0.1',
        'listen_port': int(os.environ['NODE_PORT']),
        'addrs': addrs_data,
        'transport': {
            'early_data_header_name': 'Sec-WebSocket-Protocol',
            'max_early_data': 2560,
            'headers': {'Host': os.environ['DOMAIN']},
            'path': os.environ['WS_PATH'],
            'type': 'ws'
        }
    }
    api('POST', 'save', {
        'object': 'inbounds',
        'action': 'edit' if existing_id else 'new',
        'data': json.dumps(inbound_payload),
    })
except Exception:
    pass
PYEOF
      systemctl restart s-ui 2>/dev/null || true
      echo -e "  ${G}[✓] 127.0.0.1 隧道节点创建完成 (端口: ${node_port}, 路径: /${ws_p}/)${N}"
    fi
  fi

  # 4. 重新生成纯净 Caddyfile
  local sui_caddy_rules=""
  if [[ "$cur_backend" != "sing-box" && -n "$sui_p" && -n "$sui_port" ]]; then
    sui_caddy_rules="    redir /${sui_p} /${sui_p}/ 308

    # 2. s-ui 节点管理面板
    handle /${sui_p}* {
        reverse_proxy 127.0.0.1:${sui_port}
    }"
  fi

  mkdir -p /etc/caddy
  cat > "$CADDY_FILE" <<EOF
{
    admin off
    auto_https off
}

:${tunnel_port} {
    redir /${sout_p} /${sout_p}/ 308
    redir /${sub_p} /${sub_p}/ 308
${sui_caddy_rules}

    # 1. sout 动态家宽管理面板
    handle /${sout_p}* {
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 3. sout 订阅接口（重写到 sout 面板的 /sub）
    handle /${sub_p}* {
        rewrite * /${sout_p}/sub{uri}
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 4. VLESS + WebSocket 节点 (实时零缓冲透传)
    handle /${ws_p}* {
        reverse_proxy 127.0.0.1:${node_port} {
            flush_interval -1
        }
    }

    # 5. 伪装根路径
    handle {
        respond "Service Ready" 200
    }
}
EOF

  # 5. 更新 caddy_meta.json
  python3 -c "
import json
p = '${CADDY_META}'
try:
    with open(p, 'r') as f:
        d = json.load(f)
except:
    d = {}
d['sout_port'] = int('${sout_port}')
d['sout_path'] = '${sout_p}'
d['sui_port'] = int('${sui_port}')
d['sui_path'] = '${sui_p}'
d['sub_path'] = '${sub_p}'
d['ws_path'] = '${ws_p}'
d['node_port'] = int('${node_port}')
with open(p, 'w') as f:
    json.dump(d, f, indent=2)
" 2>/dev/null || true

  # 6. 重启 Caddy 服务
  if systemctl restart caddy 2>/dev/null; then
    systemctl enable caddy 2>/dev/null || true
    echo -e "  ${G}[✓] Caddy 反代服务已重新加载最新分流配置并成功启动！${N}"
  else
    echo -e "  ${R}[×] Caddy 重启失败，请检查 Caddyfile 或端口占用${N}"
  fi
}

remove_caddy_proxy() {
  echo "  [-] 正在关闭 Cloudflare隧道连接和Caddy流量代理..."
  systemctl stop cloudflared 2>/dev/null || true
  systemctl disable cloudflared 2>/dev/null || true
  systemctl stop caddy 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || true
  rm -f "$CADDY_META" "$DOMAIN_FILE" "$QUICK_SCRIPT" /var/log/cloudflared_quick.log 2>/dev/null || true

  # 恢复 sout 为独立公网监听
  if [[ -f "${WORK_DIR}/settings.json" ]]; then
    sed -i 's|"listen_addr": "127.0.0.1"|"listen_addr": "0.0.0.0"|g' "${WORK_DIR}/settings.json"
    sed -i 's|"port": [0-9]*|"port": 8899|g' "${WORK_DIR}/settings.json"
    systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true
  fi

    # 恢复 s-ui 为公网监听（读取当前端口/路径后通过 API 修改，避免直接写库）
    if [[ -f "$SUI_DB" ]]; then
      local sui_token
      sui_token=$(cat "/var/lib/sout/sui-token" 2>/dev/null || true)
      if [[ -n "$sui_token" ]]; then
        SUI_TOKEN="$sui_token" SUI_DB="$SUI_DB" python3 <<'PY'
import json, os, sqlite3, urllib.request, urllib.parse
con = sqlite3.connect(os.environ['SUI_DB'])
cur = con.cursor()
cur.execute("SELECT value FROM settings WHERE key='webPort'")
port_row = cur.fetchone()
port = port_row[0] if port_row and port_row[0] else '8443'
cur.execute("SELECT value FROM settings WHERE key='webPath'")
path_row = cur.fetchone()
path = path_row[0] if path_row and path_row[0] else '/app/'
if not path.startswith('/'):
    path = '/' + path
if not path.endswith('/'):
    path += '/'
con.close()
BASE = f'http://127.0.0.1:{port}{path}apiv2'
TOKEN = os.environ['SUI_TOKEN']
settings_data = {
    'webListen': '',
    'webPort': '8443',
    'webPath': '/app/',
}
form = urllib.parse.urlencode({
    'object': 'settings',
    'action': 'set',
    'data': json.dumps(settings_data),
}).encode()
req = urllib.request.Request(BASE.rstrip('/') + '/save', data=form, headers={'Token': TOKEN, 'Content-Type': 'application/x-www-form-urlencoded'})
with urllib.request.urlopen(req, timeout=20) as resp:
    resp.read()
PY
        systemctl restart s-ui 2>/dev/null || true
      fi
    fi

  echo "  [✓] 已恢复为独立端口直连模式。"
}

case "${1:-}" in
  setup)
    shift
    setup_caddy_proxy "$@"
    ;;
  reload|refresh)
    reload_caddy_proxy
    ;;
  remove|stop|disable)
    remove_caddy_proxy
    ;;
  *)
    ;;
esac
