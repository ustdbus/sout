#!/usr/bin/env bash
# ================================================================
# sout 4合1 Cloudflare 隧道统一反向代理管理模块 (Caddy + Cloudflared)
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
      python3 - "$CUR_DOMAIN" "$SUI_DB" <<'PYEOF'
import sqlite3, re, sys
domain = sys.argv[1]
db = sys.argv[2]
pat = re.compile(r"[a-zA-Z0-9-]+\.trycloudflare\.com")

def fix_blob(b):
    if b is None:
        return b
    s = b.decode("utf-8", "ignore") if isinstance(b, bytes) else str(b)
    ns = pat.sub(domain, s)
    return ns.encode("utf-8") if ns != s else b

try:
    con = sqlite3.connect(db)
    cur = con.cursor()

    # settings: webURI / subURI 等含域名字段
    cur.execute("SELECT key, value FROM settings WHERE value LIKE '%trycloudflare.com%'")
    for k, v in cur.fetchall():
        nv = pat.sub(domain, v)
        if nv != v:
            cur.execute("UPDATE settings SET value=? WHERE key=?", (nv, k))

    # inbounds: addrs / options / out_json 全字段替换
    cur.execute("SELECT id, addrs, options, out_json FROM inbounds")
    for inb_id, addrs, opts, outj in cur.fetchall():
        cur.execute("UPDATE inbounds SET addrs=?, options=?, out_json=? WHERE id=?",
                    (fix_blob(addrs), fix_blob(opts), fix_blob(outj), inb_id))

    # clients: links / config 全字段替换
    cur.execute("SELECT id, links, config FROM clients")
    for cid, links, cfg in cur.fetchall():
        cur.execute("UPDATE clients SET links=?, config=? WHERE id=?",
                    (fix_blob(links), fix_blob(cfg), cid))

    con.commit()
    con.close()
except Exception:
    pass
PYEOF
      systemctl restart s-ui 2>/dev/null || true
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
ExecStart=/usr/local/bin/cloudflared tunnel --protocol http2 --no-autoupdate run --token ${token}
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
    echo "  正在配置 Cloudflare 隧道 4合1 统一反代 (${domain})..."
  fi
  echo "================================================================"

  install_caddy || return 1
  install_cloudflared || return 1

  mkdir -p "$CADDY_DIR"
  mkdir -p "$WORK_DIR"

  local sout_p sui_p sub_p ws_p sout_port sui_port sub_port node_port
  sout_p=$(rand_path "sout")
  sui_p=$(rand_path "sui")
  sub_p=$(rand_path "sub")
  ws_p=$(rand_path "vlws")

  sout_port=$(rand_port)
  sui_port=$(rand_port)
  sub_port=$(rand_port)
  node_port=$(rand_port)

  # 生成通配监听的 Caddyfile
  cat > "$CADDY_FILE" <<EOF
{
    admin off
    auto_https off
}

:${tunnel_port} {
    redir /${sout_p} /${sout_p}/ 308
    redir /${sui_p} /${sui_p}/ 308
    redir /${sub_p} /${sub_p}/ 308

    # 1. sout 动态家宽管理面板
    handle /${sout_p}* {
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 2. s-ui 节点管理面板
    handle /${sui_p}* {
        reverse_proxy 127.0.0.1:${sui_port}
    }

    # 3. s-ui 订阅端口
    handle /${sub_p}* {
        reverse_proxy 127.0.0.1:${sub_port}
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

  # 配置 s-ui 面板
  local sui_u="admin"
  if [[ -f "$SUI_DB" ]]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      sui_u=$(sqlite3 "$SUI_DB" "SELECT username FROM users LIMIT 1;" 2>/dev/null || echo "admin")
      sqlite3 "$SUI_DB" "UPDATE settings SET value='127.0.0.1' WHERE key='webListen';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='${sui_port}' WHERE key='webPort';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='/${sui_p}/' WHERE key='webPath';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='https://${domain}/${sui_p}/' WHERE key='webURI';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='127.0.0.1' WHERE key='subListen';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='${sub_port}' WHERE key='subPort';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='/${sub_p}/' WHERE key='subPath';" 2>/dev/null || true
      sqlite3 "$SUI_DB" "UPDATE settings SET value='https://${domain}/${sub_p}/' WHERE key='subURI';" 2>/dev/null || true
    fi
    systemctl restart s-ui 2>/dev/null || true
  fi
  [[ -z "$sui_u" ]] && sui_u="admin"

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
    echo "  🎉 Cloudflare 官方免费临时隧道 4合1 统一反代已成功开启！"
  else
    echo "  🎉 Cloudflare 隧道 4合1 统一反代已成功开启！"
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
  echo "  [2] s-ui 节点与分流管理面板"
  echo "      访问地址:  https://${domain}/${sui_p}/"
  echo "      管理账号:  ${sui_u}"
  echo "      管理密码:  [由您在 s-ui 中设置，已安全加密]"
  echo
  echo "  [3] s-ui 客户端订阅地址:  https://${domain}/${sub_p}/"
  echo "  [4] VLESS+WS+CDN 节点:    wss://${domain}:443/${ws_p}"
  echo "================================================================"
  echo
}

remove_caddy_proxy() {
  echo "  [-] 正在关闭 Cloudflare 隧道 4合1 统一反代..."
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

  # 恢复 s-ui 为公网监听
  if [[ -f "$SUI_DB" ]] && command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$SUI_DB" "UPDATE settings SET value='' WHERE key='webListen';" 2>/dev/null || true
    sqlite3 "$SUI_DB" "UPDATE settings SET value='8443' WHERE key='webPort';" 2>/dev/null || true
    sqlite3 "$SUI_DB" "UPDATE settings SET value='/app/' WHERE key='webPath';" 2>/dev/null || true
    systemctl restart s-ui 2>/dev/null || true
  fi

  echo "  [✓] 已恢复为独立端口直连模式。"
}
