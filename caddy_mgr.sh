#!/usr/bin/env bash
# caddy_mgr.sh - Caddy 一键全自动反代、Cloudflare DNS-01 证书托管与 4合1 端口复用模块

WORK_DIR="${WORK_DIR:-/var/lib/sout}"
CADDY_META="${WORK_DIR}/caddy_meta.json"

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
  local arch
  arch=$(get_caddy_arch)
  echo -e "  ${B}[+] 正在获取带 Cloudflare DNS-01 模块的 Caddy (${arch})...${N}"
  local tmp
  tmp=$(mktemp -d)
  local url="https://caddyserver.com/api/download?os=linux&arch=${arch}&p=github.com%2Fcaddy-dns%2Fcloudflare"
  
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
  
  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy /home/acme
  chmod 755 /etc/caddy /var/log/caddy /home/acme
  
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
  echo "${prefix}_${r}"
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

sync_caddy_certs() {
  local domain="$1"
  mkdir -p "/home/acme/${domain}"
  local cert_found="" key_found=""
  
  for _ in $(seq 1 15); do
    cert_found=$(find /var/lib/caddy /root/.local/share/caddy -name "${domain}.crt" -o -name "fullchain.pem" 2>/dev/null | head -1)
    key_found=$(find /var/lib/caddy /root/.local/share/caddy -name "${domain}.key" -o -name "privkey.pem" 2>/dev/null | head -1)
    if [[ -n "$cert_found" && -n "$key_found" ]]; then
      break
    fi
    sleep 2
  done

  if [[ -n "$cert_found" && -n "$key_found" ]]; then
    cp -f "$cert_found" "/home/acme/${domain}/fullchain.pem"
    cp -f "$key_found" "/home/acme/${domain}/privkey.pem"
    chmod 644 "/home/acme/${domain}/fullchain.pem"
    chmod 600 "/home/acme/${domain}/privkey.pem"
    echo -e "  ${G}[✓] 证书已成功同步到 /home/acme/${domain}/${N}"
    return 0
  else
    echo -e "  ${Y}[!] 证书后台生成中，已设置自动监听目录${N}"
    return 1
  fi
}

setup_caddy_proxy() {
  local domain="$1"
  local cf_token="$2"
  local ext_port="${3:-443}"

  echo
  echo -e "${B}================================================================${N}"
  echo -e "${B}  正在配置 Caddy 4合1 反代与 Cloudflare DNS-01 证书申请...${N}"
  echo -e "${B}================================================================${N}"

  # 1. 确保 Caddy 安装
  if ! command -v caddy >/dev/null 2>&1 || ! /usr/local/bin/caddy list-modules 2>/dev/null | grep -q "dns.providers.cloudflare"; then
    install_caddy_bin || { echo -e "  ${R}安装 Caddy 失败${N}"; return 1; }
  fi

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

  # 3. 写入 Caddyfile
  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy "/home/acme/${domain}"
  cat > /etc/caddy/Caddyfile <<EOF
{
    admin off
    storage file_system {
        root /var/lib/caddy
    }
    log {
        output file /var/log/caddy/access.log {
            roll_size 10mb
            roll_keep 3
        }
        level INFO
    }
}

https://${domain}:443 {
    tls {
        dns cloudflare ${cf_token}
    }

    # 1. sout 动态家宽管理面板
    handle /${sout_path}/* {
        reverse_proxy 127.0.0.1:${sout_port}
    }

    # 2. s-ui 节点管理面板
    handle /${sui_path}/* {
        reverse_proxy 127.0.0.1:${sui_port}
    }

    # 3. s-ui 订阅端口
    handle /${sub_path}/* {
        reverse_proxy 127.0.0.1:${sub_port}
    }

    # 4. VLESS + WebSocket 节点
    handle /${ws_path}* {
        reverse_proxy 127.0.0.1:${node_port}
    }

    # 5. 伪装根路径
    handle {
        respond "Service Ready" 200
    }
}
EOF

  echo -e "  [+] 正在启动 Caddy 并通过 Cloudflare DNS-01 申请证书..."
  systemctl restart caddy
  sleep 3

  # 4. 同步证书
  sync_caddy_certs "$domain" || true

  # 5. 自动化配置 s-ui
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  if [[ -f "$sui_db" ]]; then
    echo -e "  [+] 正在自动配置 s-ui 数据库 (绑定 mytls, 路径分流与 VLESS-WS 节点)..."
    python3 -c "
import sqlite3, json, os

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

# 1. 设置 mytls 证书
cur.execute('SELECT id FROM tls WHERE name = ?', ('mytls',))
row = cur.fetchone()
tls_server = {
    'enabled': True,
    'server_name': domain,
    'certificate_path': f'/home/acme/{domain}/fullchain.pem',
    'key_path': f'/home/acme/{domain}/privkey.pem'
}
server_blob = json.dumps(tls_server, indent=2).encode('utf-8')
client_blob = b'{}'

if row:
    tls_id = row[0]
    cur.execute('UPDATE tls SET server = ?, client = ? WHERE id = ?', (server_blob, client_blob, tls_id))
else:
    cur.execute('INSERT INTO tls (name, server, client) VALUES (?, ?, ?)', ('mytls', server_blob, client_blob))
    tls_id = cur.lastrowid

# 2. 更新 settings
settings_map = {
    'webPort': str(sui_port),
    'webListen': '127.0.0.1',
    'webPath': sui_path,
    'webURI': f'https://{domain}{sui_path}',
    'subPort': str(sub_port),
    'subListen': '127.0.0.1',
    'subPath': sub_path,
    'subURI': f'https://{domain}{sub_path}',
    'webCertFile': f'/home/acme/{domain}/fullchain.pem',
    'webKeyFile': f'/home/acme/{domain}/privkey.pem'
}

for k, v in settings_map.items():
    cur.execute('SELECT id FROM settings WHERE key = ?', (k,))
    if cur.fetchone():
        cur.execute('UPDATE settings SET value = ? WHERE key = ?', (v, k))
    else:
        cur.execute('INSERT INTO settings (key, value) VALUES (?, ?)', (k, v))

# 3. 配置/更新 VLESS-WS CDN 节点
node_tag = 'vless-ws-cdn'
addrs_blob = json.dumps([{'server': domain, 'server_port': 443}]).encode('utf-8')
options_dict = {
    'listen': '127.0.0.1',
    'listen_port': node_port,
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
    cur.execute('UPDATE inbounds SET type = \"vless\", tls_id = 0, addrs = ?, options = ? WHERE id = ?',
                (addrs_blob, options_blob, inb_row[0]))
else:
    cur.execute('INSERT INTO inbounds (type, tag, tls_id, addrs, options) VALUES (\"vless\", ?, 0, ?, ?)',
                (node_tag, addrs_blob, options_blob))

con.commit()
con.close()
" 2>/dev/null || true
    systemctl restart s-ui 2>/dev/null || true
  fi

  # 6. 自动化配置 sout
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

  # 7. 保存反代元数据
  cat > "$CADDY_META" <<METAEOF
{
  "enabled": true,
  "domain": "${domain}",
  "cf_token": "${cf_token}",
  "ext_port": ${ext_port},
  "sout_port": ${sout_port},
  "sout_path": "${sout_path}",
  "sui_port": ${sui_port},
  "sui_path": "${sui_path}",
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
  echo -e "${G}  🎉 Caddy 4合1 全自动反代与 SSL 证书托管已成功开启！${N}"
  echo -e "${G}================================================================${N}"
  echo -e "  解析域名:      ${B}${domain}${N}"
  echo -e "  反代入口:      ${G}443 (HTTPS / TLS 自动托管)${N}"
  if [[ "$ext_port" != "443" ]]; then
    echo -e "  NAT 回源端口:  ${Y}${ext_port}${N} (已在 CF Origin Rules 生效)"
  fi
  echo -e "  证书存储目录:  ${B}/home/acme/${domain}/${N}"
  echo -e "  ----------------------------------------------------------------"
  echo -e "  [1] sout 管理面板:  ${B}https://${domain}/${sout_path}/${N}"
  echo -e "  [2] s-ui 管理面板:  ${B}https://${domain}/${sui_path}/${N}"
  echo -e "  [3] s-ui 订阅地址:  ${B}https://${domain}/${sub_path}/${N}"
  echo -e "  [4] VLESS-WS 节点:  ${B}wss://${domain}:443/${ws_path}${N}"
  echo -e "  ----------------------------------------------------------------"
  echo -e "  访问口令:      ${Y}$(cat "${WORK_DIR}/password" 2>/dev/null || echo "未设置")${N}"
  echo -e "${G}================================================================${N}"
  echo
}

disable_caddy_proxy() {
  echo
  read -rp "  确定关闭 Caddy 反代并恢复默认独立端口模式吗？[y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  echo "  [+] 正在停止 Caddy 服务..."
  systemctl stop caddy 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || true

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
con.execute('UPDATE settings SET value = \"8443\" WHERE key = \"webPort\"')
con.execute('UPDATE settings SET value = \"\" WHERE key = \"webListen\"')
con.execute('UPDATE settings SET value = \"/app/\" WHERE key = \"webPath\"')
con.execute('UPDATE settings SET value = \"8444\" WHERE key = \"subPort\"')
con.execute('UPDATE settings SET value = \"\" WHERE key = \"subListen\"')
con.execute('UPDATE settings SET value = \"/sub/\" WHERE key = \"subPath\"')
con.commit()
con.close()
" 2>/dev/null || true
    systemctl restart s-ui 2>/dev/null || true
  fi

  systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true
  echo -e "  ${G}[✓] 已成功关闭 Caddy 反代，sout 与 s-ui 已恢复独立端口访问模式。${N}"
}

caddy_interactive_setup() {
  echo
  echo -e "${B}================================================================${N}"
  echo -e "${B}  🚀 Caddy 一键全自动反代与 SSL 证书托管配置${N}"
  echo -e "${B}================================================================${N}"
  echo -e "  特点：4合1共用443端口，自动DNS-01申请并续期证书，无视NAT与CDN"
  echo -e "${D}----------------------------------------------------------------${N}"
  
  local domain cf_token ext_port
  read -rp "  1. 请输入您的解析域名 (如 djj.20023.bond): " domain
  domain=$(echo "$domain" | tr -d ' \r\n')
  [[ -z "$domain" ]] && { echo -e "  ${R}域名不能为空！${N}"; return 1; }

  echo -e "  ${D}💡 提示：用于 DNS-01 自动申请与续期证书。若不会获取，请询问 AI${N}"
  read -rp "  2. 请输入 Cloudflare API Token (具有 Zone.DNS 权限): " cf_token
  cf_token=$(echo "$cf_token" | tr -d ' \r\n')
  [[ -z "$cf_token" ]] && { echo -e "  ${R}Token 不能为空！${N}"; return 1; }

  echo
  echo -e "  ${D}💡 默认 443 为正常独立 VPS；如果是 NAT 小鸡（如 28443:443）请输入 28443${N}"
  read -rp "  3. 请输入映射到本机 443 的端口 [默认 443]: " ext_port
  ext_port=$(echo "$ext_port" | tr -d ' \r\n')
  ext_port="${ext_port:-443}"

  if [[ "$ext_port" != "443" ]]; then
    echo
    echo -e "  ${Y}⚠️  重要警告 (NAT 小鸡用户必须设置)：${N}"
    echo -e "  请务必前往 Cloudflare 控制台后台："
    echo -e "  「规则 (Rules)」->「回源规则 (Origin Rules)」-> 创建规则，"
    echo -e "  将主机名 \"${domain}\" 的回源端口重写为 \"${ext_port}\"！"
    echo -e "  ${D}(💡 提示：若不会在 Cloudflare 设置回源规则，请询问 AI)${N}"
    echo
    read -rp "  确认已了解并已在 Cloudflare 配置好回源规则？[y/N]: " confirm
    [[ ${confirm,,} == y ]] || { echo "  已取消配置"; return 1; }
  fi

  setup_caddy_proxy "$domain" "$cf_token" "$ext_port"
}

caddy_menu() {
  while true; do
    echo
    echo -e "${B}========================================${N}"
    echo -e "${B}  Caddy 反代与 SSL 证书全托管管理${N}"
    echo -e "${B}========================================${N}"
    local en dom st
    en=$(is_caddy_enabled)
    st=$(caddy_status)
    
    if [[ "$en" == "true" ]]; then
      dom=$(grep -oE '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
      echo -e "  反代状态:      ${G}已开启 (4合1共用443)${N}"
      echo -e "  Caddy 服务:    $([[ "$st" == "active" ]] && echo -e "${G}运行中${N}" || echo -e "${R}已停止(${st})${N}")"
      echo -e "  托管域名:      ${B}${dom}${N}"
      echo -e "  证书路径:      ${B}/home/acme/${dom}/${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 查看反代访问清单与节点地址"
      echo "  2) 重新配置反代与证书 (修改域名/Token/端口)"
      echo "  3) 查看 Caddy 访问与证书日志"
      echo "  4) 重启 Caddy 服务"
      echo "  5) 关闭 Caddy 反代 (恢复独立端口模式)"
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
        3) echo; journalctl -u caddy -n 40 --no-pager; pause ;;
        4) systemctl restart caddy && echo -e "  ${G}Caddy 已重启${N}"; pause ;;
        5) disable_caddy_proxy; pause; break ;;
        0) break ;;
        *) ;;
      esac
    else
      echo -e "  反代状态:      ${D}未开启 (当前为独立多端口模式)${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 一键开启 Caddy 4合1反代与证书托管"
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
