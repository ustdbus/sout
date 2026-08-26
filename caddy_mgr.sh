#!/usr/bin/env bash
# ==============================================================================
# sout - Cloudflare 隧道 4合1 统一反代独立管理脚本
# ==============================================================================

set -e

R='\033[0;31m'
G='\033[0;32m'
Y='\033[0;33m'
B='\033[0;34m'
C='\033[0;36m'
D='\033[0;90m'
N='\033[0m'

WORK_DIR="/usr/local/sout"
CADDY_META="${WORK_DIR}/caddy_meta.json"

pause() {
  echo
  read -rp "  按回车键继续..." _
}

get_caddy_arch() {
  local a
  a=$(uname -m)
  case "$a" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv7l|armv7) echo "armv7" ;;
    *) echo "amd64" ;;
  esac
}

install_caddy_bin() {
  if command -v caddy >/dev/null 2>&1; then
    return 0
  fi
  local arch
  arch=$(get_caddy_arch)
  echo -e "  ${B}[+] 正在获取标准 Caddy 反代服务 (${arch})...${N}"
  local tmp
  tmp=$(mktemp -d)
  local url="https://caddyserver.com/api/download?os=linux&arch=${arch}"
  
  if ! curl -fsSL "$url" -o "$tmp/caddy"; then
    echo -e "  ${Y}[!] 官方 API 下载稍慢，正在重试...${N}"
    if ! curl -fsSL "$url" -o "$tmp/caddy"; then
      echo -e "  ${R}[✗] Caddy 二进制下载失败，请检查网络连接${N}" >&2
      rm -rf "$tmp"
      return 1
    fi
  fi
  
  install -m 755 "$tmp/caddy" /usr/local/bin/caddy
  rm -rf "$tmp"
  
  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy
  chmod 755 /etc/caddy /var/log/caddy
  
  cat > /etc/systemd/system/caddy.service <<'EOF'
[Unit]
Description=Caddy Web Server
Documentation=https://caddyserver.com/docs/
After=network.target network-online.target
Requires=network-online.target

[Service]
Type=notify
User=root
Group=root
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable caddy >/dev/null 2>&1 || true
  echo -e "  ${G}[✓] Caddy 服务已就绪${N}"
  return 0
}

install_cloudflared_bin() {
  if command -v cloudflared >/dev/null 2>&1; then
    return 0
  fi
  local arch
  arch=$(get_caddy_arch)
  echo -e "  ${B}[+] 正在获取 Cloudflare 官方隧道客户端 cloudflared (${arch})...${N}"
  local url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${arch}"
  
  if ! curl -fL --retry 3 --connect-timeout 10 -o /usr/local/bin/cloudflared "$url"; then
    echo -e "  ${R}[✗] cloudflared 二进制下载失败，请检查网络连接${N}" >&2
    return 1
  fi
  chmod +x /usr/local/bin/cloudflared
  echo -e "  ${G}[✓] cloudflared 已成功安装至 /usr/local/bin/cloudflared${N}"
  return 0
}

setup_cloudflared_service() {
  local token="$1"
  cat > /etc/systemd/system/cloudflared.service <<EOF
[Unit]
Description=Cloudflare Tunnel Agent
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=notify
TimeoutStartSec=0
ExecStart=/usr/local/bin/cloudflared tunnel --no-autoupdate run --token ${token}
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable cloudflared >/dev/null 2>&1 || true
  systemctl restart cloudflared
}

rand_local_port() {
  local p
  while true; do
    p=$(( 30000 + RANDOM % 15000 ))
    if ! ss -tulpn | grep -q ":${p}[[:space:]]"; then
      echo "$p"
      return
    fi
  done
}

rand_safe_path() {
  local prefix="$1"
  local r
  r=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
  echo "${prefix}${r}"
}

caddy_status() {
  systemctl is-active caddy 2>/dev/null || echo "inactive"
}

is_caddy_enabled() {
  if [[ -f "$CADDY_META" ]]; then
    if grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META"; then
      echo "true"
      return
    fi
  fi
  echo "false"
}

setup_caddy_proxy() {
  local domain="$1"
  local tunnel_token="$2"
  local tunnel_port="${3:-8080}"

  echo
  echo -e "${B}================================================================${N}"
  echo -e "${B}  正在配置 Cloudflare 隧道 4合1 统一反代...${N}"
  echo -e "${B}================================================================${N}"

  # 1. 确保 Caddy 与 cloudflared 安装
  install_caddy_bin || { echo -e "  ${R}安装 Caddy 失败${N}"; return 1; }
  install_cloudflared_bin || { echo -e "  ${R}安装 cloudflared 失败${N}"; return 1; }

  # 2. 分配 4 个本地端口和 4 个安全路径
  local sout_port sui_port sub_port node_port
  local sout_path sui_path sub_path ws_path

  sout_port=$(rand_local_port)
  sui_port=$(rand_local_port)
  sub_port=$(rand_local_port)
  node_port=$(rand_local_port)

  sout_path=$(rand_safe_path "sout")
  sui_path=$(rand_safe_path "sui")
  sub_path=$(rand_safe_path "sub")
  ws_path=$(rand_safe_path "vlws")

  # 3. 写入纯净本地 Caddyfile (监听 127.0.0.1:${tunnel_port}，无公网端口与证书负担)
  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy
  cat > /etc/caddy/Caddyfile <<EOF
{
    admin off
    auto_https off
}

http://127.0.0.1:${tunnel_port} {
    redir /${sout_path} /${sout_path}/ 308
    redir /${sui_path} /${sui_path}/ 308
    redir /${sub_path} /${sub_path}/ 308

    # 1. sout 动态家宽管理面板
    handle /${sout_path}* {
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 2. s-ui 节点管理面板
    handle /${sui_path}* {
        reverse_proxy 127.0.0.1:${sui_port}
    }

    # 3. s-ui 订阅端口
    handle /${sub_path}* {
        reverse_proxy 127.0.0.1:${sub_port}
    }

    # 4. VLESS + WebSocket 节点 (实时零缓冲透传)
    handle /${ws_path}* {
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

  echo -e "  [+] 正在启动本地 Caddy 分流服务与 Cloudflare 隧道..."
  systemctl restart caddy
  setup_cloudflared_service "$tunnel_token"
  sleep 2

  # 4. 自动化配置 s-ui
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_admin_user="admin"
  local sui_admin_pass=""
  if [[ -f "${WORK_DIR}/sui_pass" ]]; then
    sui_admin_pass=$(cat "${WORK_DIR}/sui_pass" 2>/dev/null || true)
  fi
  if [[ -z "$sui_admin_pass" ]]; then
    sui_admin_pass=$(head -c 8 /dev/urandom | xxd -p | head -c 10)
    echo "$sui_admin_pass" > "${WORK_DIR}/sui_pass"
    chmod 600 "${WORK_DIR}/sui_pass"
  fi
  if [[ -f "${WORK_DIR}/sui_user" ]]; then
    sui_admin_user=$(cat "${WORK_DIR}/sui_user" 2>/dev/null || echo "admin")
  else
    echo "$sui_admin_user" > "${WORK_DIR}/sui_user"
    chmod 600 "${WORK_DIR}/sui_user"
  fi

  if [[ -x /usr/local/s-ui/sui ]]; then
    echo -e "  [+] 正在自动配置 s-ui 管理员账号 (${sui_admin_user})..."
    /usr/local/s-ui/sui admin -username "${sui_admin_user}" -password "${sui_admin_pass}" >/dev/null 2>&1 || true
  fi

  if [[ -f "$sui_db" ]]; then
    echo -e "  [+] 正在自动配置 s-ui 数据库 (路径分流与 VLESS-WS 节点)..."
    python3 << PYEOF
import sqlite3, json, os, uuid, urllib.parse

db = '$sui_db'
domain = '$domain'
sui_port = $sui_port
sui_path = '/${sui_path}/'
sub_port = $sub_port
sub_path = '/${sub_path}/'
node_port = $node_port
ws_path = '/${ws_path}'

con = sqlite3.connect(db)
cur = con.cursor()

# 1. 更新 settings (Caddy统一终结TLS，s-ui内部使用HTTP代理)
settings_map = {
    'webPort': str(sui_port),
    'webListen': '127.0.0.1',
    'webPath': sui_path,
    'webURI': f'https://{domain}{sui_path}',
    'subPort': str(sub_port),
    'subListen': '127.0.0.1',
    'subPath': sub_path,
    'subURI': f'https://{domain}{sub_path}',
    'webCertFile': '',
    'webKeyFile': '',
    'subCertFile': '',
    'subKeyFile': ''
}

for k, v in settings_map.items():
    cur.execute('SELECT id FROM settings WHERE key = ?', (k,))
    if cur.fetchone():
        cur.execute('UPDATE settings SET value = ? WHERE key = ?', (v, k))
    else:
        cur.execute('INSERT INTO settings (key, value) VALUES (?, ?)', (k, v))

# 2. 自动收敛所有原本监听 443 的入站节点至本地端口
cur.execute('SELECT id, tag, options, addrs FROM inbounds')
for inb_id, inb_tag, inb_opts, inb_addrs in cur.fetchall():
    try:
        opts_s = inb_opts.decode('utf-8') if isinstance(inb_opts, bytes) else str(inb_opts)
        opts_j = json.loads(opts_s)
        if opts_j.get('listen_port') == 443 or opts_j.get('listen') in ('::', '0.0.0.0', '*'):
            if 'transport' in opts_j and opts_j['transport'].get('type') == 'ws':
                opts_j['listen'] = '127.0.0.1'
                opts_j['listen_port'] = 43641
                b_opts = json.dumps(opts_j, indent=2).encode('utf-8')
                b_addrs = inb_addrs if isinstance(inb_addrs, bytes) else (str(inb_addrs).encode('utf-8') if inb_addrs else b'[]')
                cur.execute('UPDATE inbounds SET tls_id = 0, options = ?, addrs = ? WHERE id = ?',
                            (b_opts, b_addrs, inb_id))
    except:
        pass

# 3. 配置/更新 VLESS-WS CDN 节点
node_tag = 'vless-ws-cdn'
client_uuid = str(uuid.uuid4())

addrs_data = [
    {
        'server': '45.89.235.139',
        'server_port': 443,
        'tls': {
            'disable_sni': False,
            'enabled': True,
            'insecure': False,
            'server_name': domain
        }
    },
    {
        'server': domain,
        'server_port': 443,
        'tls': {
            'disable_sni': False,
            'enabled': True,
            'insecure': False,
            'server_name': domain
        }
    }
]
addrs_blob = json.dumps(addrs_data, indent=2).encode('utf-8')

options_dict = {
    'listen': '127.0.0.1',
    'listen_port': node_port,
    'sniff': True,
    'sniff_override_destination': True,
    'users': [
        {
            'flow': '',
            'name': 'admin',
            'uuid': client_uuid
        }
    ],
    'transport': {
        'early_data_header_name': 'Sec-WebSocket-Protocol',
        'headers': {
            'Host': domain
        },
        'path': ws_path,
        'type': 'ws'
    }
}
options_blob = json.dumps(options_dict, indent=2).encode('utf-8')

cur.execute('SELECT id FROM inbounds WHERE tag = ?', (node_tag,))
inb_row = cur.fetchone()
if inb_row:
    cur.execute('UPDATE inbounds SET type = ?, tls_id = 0, addrs = ?, options = ? WHERE id = ?',
                ('vless', addrs_blob, options_blob, inb_row[0]))
else:
    cur.execute('INSERT INTO inbounds (type, tag, tls_id, addrs, options) VALUES (?, ?, 0, ?, ?)',
                ('vless', node_tag, addrs_blob, options_blob))
    inb_id = cur.lastrowid

node_uri = f'vless://{client_uuid}@{domain}:443?type=ws&path={urllib.parse.quote(ws_path)}&host={domain}&security=tls&sni={domain}#{urllib.parse.quote(node_tag)}'
links_data = [
    {
        'remark': node_tag,
        'type': 'local',
        'uri': node_uri
    }
]
links_blob = json.dumps(links_data, indent=2).encode('utf-8')

client_cfg = json.dumps({'vless': {'name': 'admin', 'uuid': client_uuid, 'flow': ''}}).encode('utf-8')
inbounds_blob = json.dumps([inb_id]).encode('utf-8')
cur.execute('SELECT id FROM clients WHERE name = ?', ('admin',))
c_row = cur.fetchone()
if c_row:
    cur.execute('UPDATE clients SET enable = 1, config = ?, inbounds = ?, links = ? WHERE id = ?',
                (client_cfg, inbounds_blob, links_blob, c_row[0]))
else:
    cur.execute("INSERT INTO clients (enable, name, remark, config, inbounds, links, created_at) VALUES (1, ?, ?, ?, ?, ?, strftime('%s', 'now'))",
                ('admin', '默认用户', client_cfg, inbounds_blob, links_blob))

cur.execute("UPDATE clients SET links = CAST('[]' AS BLOB) WHERE links IS NULL OR length(links) = 0")

con.commit()
con.close()
PYEOF
    systemctl restart s-ui 2>/dev/null || true
  fi

  # 5. 自动化配置 sout
  echo -e "  [+] 正在自动配置 sout 插件 (绑定 127.0.0.1:${sout_port} 并挂载路径 /${sout_path}/)..."
  mkdir -p "$WORK_DIR"
  echo "$sout_path" > "${WORK_DIR}/basepath"
  chmod 600 "${WORK_DIR}/basepath"

  python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    try:
        with open(path) as f: data = json.load(f)
    except: pass
data['port'] = $sout_port
data['listen_addr'] = '127.0.0.1'
data['panel_url'] = 'https://$domain'
data['ssl_enabled'] = False
with open(path, 'w') as f:
    json.dump(data, f, indent=2)
" 2>/dev/null || true

  # 6. 保存反代元数据
  cat > "$CADDY_META" <<METAEOF
{
  "enabled": true,
  "mode": "tunnel",
  "domain": "${domain}",
  "tunnel_token": "${tunnel_token}",
  "tunnel_port": ${tunnel_port},
  "sout_port": ${sout_port},
  "sout_path": "${sout_path}",
  "sui_port": ${sui_port},
  "sui_path": "${sui_path}",
  "sui_user": "${sui_admin_user}",
  "sui_pass": "${sui_admin_pass}",
  "sub_port": ${sub_port},
  "sub_path": "${sub_path}",
  "node_port": ${node_port},
  "ws_path": "${ws_path}"
}
METAEOF
  chmod 600 "$CADDY_META"

  systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true

  echo
  echo -e "${G}================================================================${N}"
  echo -e "${G}  🎉 Cloudflare 隧道 4合1 统一反代已成功开启！${N}"
  echo -e "${G}================================================================${N}"
  echo -e "  访问域名:      ${B}https://${domain}${N}"
  echo -e "  隧道服务:      ${G}cloudflared (运行中 / active)${N}"
  echo -e "  本地回源端口:  ${Y}127.0.0.1:${tunnel_port}${N}"
  echo -e "  ----------------------------------------------------------------"
  echo -e "  [1] sout 家宽动态出口插件面板"
  echo -e "      访问地址:  ${B}https://${domain}/${sout_path}/${N}"
  echo -e "      访问口令:  ${Y}$(cat "${WORK_DIR}/password" 2>/dev/null || echo "未设置")${N}"
  echo
  echo -e "  [2] s-ui 节点与分流管理面板"
  echo -e "      访问地址:  ${B}https://${domain}/${sui_path}/${N}"
  echo -e "      管理账号:  ${Y}${sui_admin_user}${N}"
  echo -e "      管理密码:  ${Y}${sui_admin_pass}${N}"
  echo
  echo -e "  [3] s-ui 客户端订阅地址:  ${B}https://${domain}/${sub_path}/${N}"
  echo -e "  [4] VLESS+WS+CDN 节点:    ${B}wss://${domain}:443/${ws_path}${N}"
  echo -e "${G}================================================================${N}"
  echo
}

disable_caddy_proxy() {
  echo
  read -rp "  确定关闭 Cloudflare 隧道反代并恢复默认独立端口模式吗？[y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  echo "  [+] 正在停止 Caddy 与 cloudflared 服务..."
  systemctl stop caddy 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || true
  systemctl stop cloudflared 2>/dev/null || true
  systemctl disable cloudflared 2>/dev/null || true

  if [[ -f "$CADDY_META" ]]; then
    rm -f "$CADDY_META"
  fi

  # 恢复 sout 为默认端口 8899 和 0.0.0.0 监听
  python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    try:
        with open(path) as f: data = json.load(f)
    except: pass
data['port'] = 8899
data['listen_addr'] = '0.0.0.0'
data.pop('panel_url', None)
with open(path, 'w') as f:
    json.dump(data, f, indent=2)
" 2>/dev/null || true

  # 恢复 s-ui 监听为 8443 / 8444
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  if [[ -f "$sui_db" ]]; then
    python3 -c "
import sqlite3
con = sqlite3.connect('$sui_db')
con.execute('UPDATE settings SET value = "8443" WHERE key = "webPort"')
con.execute('UPDATE settings SET value = "" WHERE key = "webListen"')
con.execute('UPDATE settings SET value = "/app/" WHERE key = "webPath"')
con.execute('UPDATE settings SET value = "8444" WHERE key = "subPort"')
con.execute('UPDATE settings SET value = "" WHERE key = "subListen"')
con.execute('UPDATE settings SET value = "/sub/" WHERE key = "subPath"')
con.commit()
con.close()
" 2>/dev/null || true
    systemctl restart s-ui 2>/dev/null || true
  fi

  systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true
  echo -e "  ${G}[✓] 已成功关闭隧道反代，sout 与 s-ui 已恢复独立端口访问模式。${N}"
}

caddy_interactive_setup() {
  echo
  echo -e "${B}================================================================${N}"
  echo -e "${B}  🚀 Cloudflare 隧道 4合1 一键全自动反代配置 (免开端口/杜绝525)${N}"
  echo -e "${B}================================================================${N}"
  echo -e "  特点：无需公网端口、无视NAT网络、免申请SSL证书、杜绝525握手错误"
  echo -e "${D}----------------------------------------------------------------${N}"
  
  local domain tunnel_token tunnel_port
  read -rp "  1. 请输入您的访问域名 (如 djj.20023.bond): " domain
  domain=$(echo "$domain" | tr -d ' \r\n')
  [[ -z "$domain" ]] && { echo -e "  ${R}域名不能为空！${N}"; return 1; }

  echo -e "  ${D}💡 提示：前往 Cloudflare Zero Trust -> Networks -> Tunnels 创建隧道并复制 Token${N}"
  read -rp "  2. 请输入 Cloudflare 隧道 Token (eyJh...): " tunnel_token
  tunnel_token=$(echo "$tunnel_token" | tr -d ' \r\n')
  [[ -z "$tunnel_token" ]] && { echo -e "  ${R}隧道 Token 不能为空！${N}"; return 1; }

  echo
  echo -e "  ${D}💡 本地回源端口用于 cloudflared 将流量转发至本地 Caddy，默认 8080 即可${N}"
  read -rp "  3. 请输入本地回源端口 [默认 8080]: " tunnel_port
  tunnel_port=$(echo "$tunnel_port" | tr -d ' \r\n')
  tunnel_port="${tunnel_port:-8080}"

  setup_caddy_proxy "$domain" "$tunnel_token" "$tunnel_port"
}

caddy_menu() {
  while true; do
    echo
    echo -e "${B}========================================${N}"
    echo -e "${B}  Cloudflare 隧道 4合1 反代管理${N}"
    echo -e "${B}========================================${N}"
    local en dom st cf_st
    en=$(is_caddy_enabled)
    st=$(caddy_status)
    cf_st=$(systemctl is-active cloudflared 2>/dev/null || echo "inactive")
    
    if [[ "$en" == "true" ]]; then
      dom=$(grep -oE '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
      local tun_p
      tun_p=$(grep -oE '"tunnel_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
      [[ -z "$tun_p" ]] && tun_p="8080"

      echo -e "  反代状态:      ${G}已开启 (Cloudflare 隧道模式)${N}"
      echo -e "  隧道服务:      $([[ "$cf_st" == "active" ]] && echo -e "${G}运行中${N}" || echo -e "${R}已停止(${cf_st})${N}")"
      echo -e "  Caddy 服务:    $([[ "$st" == "active" ]] && echo -e "${G}运行中${N}" || echo -e "${R}已停止(${st})${N}")"
      echo -e "  托管域名:      ${B}${dom}${N}"
      echo -e "  本地回源:      ${Y}127.0.0.1:${tun_p}${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 查看反代访问清单与节点地址"
      echo "  2) 重新配置隧道与域名 (修改 Token/域名/端口)"
      echo "  3) 查看 cloudflared 隧道运行日志"
      echo "  4) 重启隧道与 Caddy 服务"
      echo "  5) 关闭隧道反代 (恢复独立端口模式)"
      echo "  0) 返回上级菜单"
      echo
      read -rp "  请选择 [0-5]: " opt
      case "$opt" in
        1)
          if [[ -f "$CADDY_META" ]]; then
            local sout_p sui_p sub_p ws_p
            sout_p=$(grep -oE '"sout_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            ws_p=$(grep -oE '"ws_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            echo
            echo -e "  [1] sout 管理面板:  ${B}https://${dom}/${sout_p}/${N}"
            echo -e "  [2] s-ui 管理面板:  ${B}https://${dom}/${sui_p}/${N}"
            echo -e "  [3] s-ui 订阅地址:  ${B}https://${dom}/${sub_p}/${N}"
            echo -e "  [4] VLESS-WS 节点:  ${B}wss://${dom}:443/${ws_p}${N}"
            echo -e "  访问口令:          ${Y}$(cat "${WORK_DIR}/password" 2>/dev/null || echo "未设置")${N}"
          fi
          pause ;;
        2) caddy_interactive_setup; pause ;;
        3) echo; journalctl -u cloudflared -n 40 --no-pager; pause ;;
        4)
          systemctl restart cloudflared 2>/dev/null || true
          systemctl restart caddy && echo -e "  ${G}服务已重启${N}"
          pause ;;
        5) disable_caddy_proxy; pause; break ;;
        0) break ;;
        *) ;;
      esac
    else
      echo -e "  反代状态:      ${D}未开启 (当前为独立多端口模式)${N}"
      echo -e "  💡 提示:       ${Y}强烈推荐开启 Cloudflare 隧道 4合1 反代 (免开端口/杜绝525)${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 一键开启 Cloudflare 隧道 4合1 反代"
      echo "  0) 返回上级菜单"
      echo
      read -rp "  请选择 [0-1]: " opt
      case "$opt" in
        1) caddy_interactive_setup; pause ;;
        0) break ;;
        *) ;;
      esac
    fi
  done
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  caddy_menu
fi
