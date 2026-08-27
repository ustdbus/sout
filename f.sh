#!/usr/bin/env bash
set -e

UNIT="sout.service"
if systemctl is-active fanout.service >/dev/null 2>&1 && ! systemctl is-active sout.service >/dev/null 2>&1; then
  UNIT="fanout.service"
fi
BIN="/usr/local/bin/sout-server"
[[ ! -f "$BIN" && -f "/usr/local/bin/sout" ]] && BIN="/usr/local/bin/sout"
[[ ! -f "$BIN" && -f "/usr/local/bin/fanout" ]] && BIN="/usr/local/bin/fanout"
WORK_DIR="/var/lib/sout"
[[ ! -d "$WORK_DIR" && -d "/var/lib/fanout" ]] && WORK_DIR="/var/lib/fanout"
DEFAULT_PORT=8899

R='\033[31m'; G='\033[32m'; Y='\033[33m'; B='\033[34m'; D='\033[90m'; N='\033[0m'

need_root() {
  if [[ $EUID -ne 0 ]]; then
    echo -e "${R}请使用 root 权限运行此脚本 (sudo sout)${N}"
    exit 1
  fi
}

check_sui() {
  if [[ -f /usr/local/s-ui/db/s-ui.db ]] || [[ -f /usr/local/s-ui/s-ui ]] || command -v sui >/dev/null 2>&1 || [[ -f /usr/local/s-ui/sui ]]; then
    return 0
  fi
  return 1
}

svc_status()     { systemctl is-active "$UNIT" 2>/dev/null || echo inactive; }
svc_is_enabled() { systemctl is-enabled "$UNIT" 2>/dev/null | grep -q 'enabled'; }
svc_start()      { systemctl start "$UNIT"; }
svc_stop()       { systemctl stop "$UNIT"; }
svc_restart()    { systemctl restart "$UNIT"; }
svc_reload()     { systemctl daemon-reload; }
svc_enable()     { systemctl enable "$UNIT"; }
svc_disable()    { systemctl disable "$UNIT"; }
svc_logs()       { journalctl -u "$UNIT" -n "${1:-40}" --no-pager; }
svc_logs_follow(){ journalctl -u "$UNIT" -f; }

web_port() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local p
    p=$(grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' "$WORK_DIR/settings.json" | grep -oE '[0-9]+' | head -1)
    [[ -n "$p" ]] && { echo "$p"; return; }
  fi
  echo "$DEFAULT_PORT"
}

web_listen_addr() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local a
    a=$(grep -oE '"listen_addr"[[:space:]]*:[[:space:]]*"[^"]*"' "$WORK_DIR/settings.json" | cut -d'"' -f4)
    [[ -n "$a" ]] && { echo "$a"; return; }
  fi
  echo "0.0.0.0"
}

web_panel_url() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local u
    u=$(grep -oE '"panel_url"[[:space:]]*:[[:space:]]*"[^"]*"' "$WORK_DIR/settings.json" | cut -d'"' -f4)
    [[ -n "$u" ]] && { echo "$u"; return; }
  fi
  echo ""
}

web_ssl_enabled() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    if grep -q '"ssl_enabled"[[:space:]]*:[[:space:]]*true' "$WORK_DIR/settings.json"; then
      echo "true"
      return
    fi
  fi
  echo "false"
}

web_ssl_domain() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local d
    d=$(grep -oE '"ssl_domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$WORK_DIR/settings.json" | cut -d'"' -f4)
    [[ -n "$d" ]] && { echo "$d"; return; }
  fi
  echo ""
}

web_ssl_cert() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local c
    c=$(grep -oE '"ssl_cert"[[:space:]]*:[[:space:]]*"[^"]*"' "$WORK_DIR/settings.json" | cut -d'"' -f4)
    [[ -n "$c" ]] && { echo "$c"; return; }
  fi
  echo ""
}

web_ssl_key() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local k
    k=$(grep -oE '"ssl_key"[[:space:]]*:[[:space:]]*"[^"]*"' "$WORK_DIR/settings.json" | cut -d'"' -f4)
    [[ -n "$k" ]] && { echo "$k"; return; }
  fi
  echo ""
}

web_password() {
  if [[ -f "$WORK_DIR/password" ]]; then
    cat "$WORK_DIR/password" | tr -d ' \r\n'
  else
    echo "(未设置)"
  fi
}

web_basepath() {
  if [[ -f "$WORK_DIR/basepath" ]]; then
    local bp
    bp=$(cat "$WORK_DIR/basepath" | tr -d ' \r\n')
    [[ -n "$bp" ]] && { echo "/${bp%/}/"; return; }
  fi
  echo "/"
}

public_ip() {
  local ip
  ip=$(curl -s4m 3 https://api.ipify.org || curl -s4m 3 https://ifconfig.me || echo "127.0.0.1")
  echo "$ip"
}

pause() {
  echo
  read -rp "  按回车键继续..." _
}

CADDY_META="${WORK_DIR}/caddy_meta.json"

get_sui_user() {
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local u="admin"
  if [[ -f "$sui_db" ]]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      u=$(sqlite3 "$sui_db" "SELECT username FROM users LIMIT 1;" 2>/dev/null || echo "admin")
    elif command -v python3 >/dev/null 2>&1; then
      u=$(python3 -c "import sqlite3; con=sqlite3.connect('$sui_db'); cur=con.cursor(); r=cur.execute('SELECT username FROM users LIMIT 1').fetchone(); print(r[0] if r else 'admin'); con.close()" 2>/dev/null || echo "admin")
    fi
  fi
  [[ -z "$u" ]] && u="admin"
  echo "$u"
}

show_info() {
  local st la port bp pw pip purl full_url ssl_en ssl_dom scheme c_en c_dom c_sout_p c_sui_p c_sub_p c_mode
  st=$(svc_status)
  la=$(web_listen_addr)
  port=$(web_port)
  bp=$(web_basepath)
  pw=$(web_password)
  pip=$(public_ip)
  purl=$(web_panel_url)
  ssl_en=$(web_ssl_enabled)
  ssl_dom=$(web_ssl_domain)
  c_en=$(is_caddy_enabled)

  scheme="http"
  [[ "$ssl_en" == "true" ]] && scheme="https"

  bp="/${bp#/}"
  [[ "$bp" != */ ]] && bp="${bp}/"
  if [[ -n "$purl" ]]; then
    purl="${purl%/}"
    full_url="${purl}${bp}"
  fi

  local cur_ver="dev"
  if [[ -x "$BIN" ]]; then
    cur_ver=$("$BIN" -version 2>/dev/null | awk '{print $NF}' || echo "dev")
  fi
  [[ -z "$cur_ver" ]] && cur_ver="dev"

  echo
  echo -e "  程序版本:    ${G}${cur_ver}${N}"
  if [[ "$st" == "active" ]]; then
    echo -e "  服务状态:    ${G}运行中 (active)${N}"
  else
    echo -e "  服务状态:    ${R}已停止 (${st})${N}"
  fi
  echo -e "  开机自启:    $(svc_is_enabled && echo -e "${G}已开启${N}" || echo -e "${D}已关闭${N}")"

  if [[ -f /usr/local/s-ui/db/s-ui.db ]] || [[ -f /usr/local/s-ui/s-ui ]] || command -v sui >/dev/null 2>&1; then
    echo -e "  面板对接:    ${G}s-ui (Sing-Box) 已就绪${N}"
  elif command -v /usr/local/x-ui/x-ui >/dev/null 2>&1 || [[ -x /usr/bin/x-ui ]]; then
    echo -e "  面板对接:    ${G}3x-ui 已就绪${N}"
  else
    echo -e "  面板对接:    ${R}未检测到 s-ui 面板${N}"
  fi

  local sui_u
  sui_u=$(get_sui_user)

  if [[ "$c_en" == "true" ]]; then
    c_mode=$(grep -oE '"mode"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4 || echo "tunnel")
    c_dom=$(grep -oE '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4 || echo "")
    c_sout_p=$(grep -oE '"sout_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4 || echo "sout")
    c_sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4 || echo "sui")
    c_sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4 || echo "sub")
    local c_tun_p
    c_tun_p=$(grep -oE '"tunnel_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ' || echo "8081")
    [[ -z "$c_tun_p" ]] && c_tun_p="8081"

    local cf_st="未运行"
    if [[ $(systemctl is-active cloudflared 2>/dev/null || echo "") == "active" ]]; then
      cf_st="${G}运行中 (active)${N}"
    else
      cf_st="${R}未运行${N}"
    fi

    # 如果是临时隧道，且记录域名失效/需要更新时，动态从 journalctl 抓取最新域名
    if [[ "$c_mode" == "quick_tunnel" ]]; then
      local real_d
      real_d=$(journalctl -u cloudflared -n 50 --no-pager 2>/dev/null | grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' | tail -1 | sed 's|https://||' | tr -d ' 
')
      [[ -n "$real_d" ]] && c_dom="$real_d"
      echo -e "  反代模式:    ${G}Cloudflare 官方免费临时隧道 4合1 (已开启)${N}"
    else
      echo -e "  反代模式:    ${G}Cloudflare 隧道 4合1 模式 (已开启)${N}"
    fi

    echo -e "  隧道服务:    ${cf_st} (本地回源: 127.0.0.1:${c_tun_p})"
    echo -e "  管理面板:    ${B}https://${c_dom}/${c_sout_p}/${N}"
    echo -e "  访问口令:    ${Y}${pw}${N}"
    echo -e "  s-ui 面板:   ${B}https://${c_dom}/${c_sui_p}/${N}"
    echo -e "  s-ui 用户名: ${Y}${sui_u}${N}"
    echo -e "  s-ui 密  码: ${D}[由您在 s-ui 中设置，已安全加密]${N}"
    echo -e "  订阅链接:    ${B}https://${c_dom}/${c_sub_p}/${N}"
  else
    if [[ "$ssl_en" == "true" ]]; then
      echo -e "  SSL 加密:    ${G}已开启 (HTTPS)${N}"
    else
      echo -e "  SSL 加密:    ${D}未开启 (HTTP)${N}"
    fi
    
    if [[ "$la" == "127.0.0.1" ]]; then
      echo -e "  监听地址:    ${Y}127.0.0.1 (本地反向代理模式)${N}"
      if [[ -n "$full_url" ]]; then
        echo -e "  本地地址:    ${B}${full_url}${N}"
      else
        echo -e "  本地地址:    ${B}${scheme}://127.0.0.1:${port}${bp}${N}"
        echo -e "  公网访问:    ${D}(仅能通过您配置的反向代理域名访问)${N}"
      fi
    else
      echo -e "  监听地址:    ${G}0.0.0.0 (所有公网网卡)${N}"
      if [[ -n "$full_url" ]]; then
        echo -e "  管理面板:    ${B}${full_url}${N}"
      elif [[ "$ssl_en" == "true" && -n "$ssl_dom" ]]; then
        echo -e "  管理面板:    ${B}https://${ssl_dom}:${port}${bp}${N}"
      else
        echo -e "  管理面板:    ${B}${scheme}://${pip}:${port}${bp}${N}"
      fi
    fi
    echo -e "  访问口令:    ${Y}${pw}${N}"

    local sui_db="/usr/local/s-ui/db/s-ui.db"
    if [[ -f "$sui_db" || -x /usr/local/s-ui/sui ]]; then
      local sui_port="8443"
      local sui_path="/app/"
      if [[ -f "$sui_db" ]]; then
        if command -v sqlite3 >/dev/null 2>&1; then
          local p_val path_val
          p_val=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPort' LIMIT 1;" 2>/dev/null || true)
          path_val=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPath' LIMIT 1;" 2>/dev/null || true)
          [[ -n "$p_val" ]] && sui_port="$p_val"
          [[ -n "$path_val" ]] && sui_path="$path_val"
        fi
      fi
      sui_path="/${sui_path#/}"
      [[ "$sui_path" != */ ]] && sui_path="${sui_path}/"
      echo -e "  s-ui 面板:   ${B}http://${pip}:${sui_port}${sui_path}${N}"
      echo -e "  s-ui 用户名: ${Y}${sui_u}${N}"
      echo -e "  s-ui 密  码: ${D}[由您在 s-ui 中设置，已安全加密]${N}"
    fi
  fi
  echo -e "  💡 提示:     ${D}如遗忘密码，可在终端运行 s-ui 随时重置${N}"
  echo -e "  s-ui 唤起命令: ${C}s-ui${N}"
  echo -e "  sout 唤起命令: ${C}sout${N}"
  echo
}

list_tunnels() {
  local port bp pw
  port=$(web_port)
  bp=$(web_basepath)
  pw=$(web_password)
  echo
  echo -e "  ${B}当前运行中的出口隧道列表：${N}"
  python3 -c "
import urllib.request, urllib.parse, http.cookiejar, json
try:
    cj = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
    login_url = f'http://127.0.0.1:${port}${bp}login'
    login_data = urllib.parse.urlencode({'password': '${pw}'}).encode()
    opener.open(login_url, data=login_data, timeout=3)
    
    req = urllib.request.Request(f'http://127.0.0.1:${port}${bp}api/tunnels')
    res = opener.open(req, timeout=4)
    data = json.loads(res.read().decode())
    if not data:
        print('  (暂无运行中的出口隧道，请在面板中新建)')
    else:
        print('  ' + f'{\"槽位\":<6} {\"状态\":<10} {\"出口 IP\":<18} {\"SOCKS5 端口\":<14} {\"国家/地区\"}')
        print('  ' + '-'*68)
        for t in data:
            slot = t.get('slot', '-')
            st = t.get('status', '-')
            ip = t.get('exit_ip') or '连接中...'
            port = t.get('port', '-')
            node = t.get('node', {})
            c = node.get('country') or node.get('country_code') or '-'
            print('  ' + f'{slot:<6} {st:<10} {ip:<18} 127.0.0.1:{port:<6} {c}')
except Exception as e:
    print('  获取隧道列表失败:', e)
"
  echo
}

change_port() {
  local cur new
  cur=$(web_port)
  echo
  read -rp "  请输入新管理端口 (当前 ${cur}): " new
  [[ -z "$new" ]] && { echo "  未修改"; return; }
  if ! [[ "$new" =~ ^[0-9]+$ ]] || (( new < 1 || new > 65535 )); then
    echo -e "  ${R}端口不合法${N}"; return
  fi
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    sed -i "s/\"port\"[[:space:]]*:[[:space:]]*[0-9]*/\"port\": ${new}/" "$WORK_DIR/settings.json"
  else
    printf '{\n  \"port\": %s,\n  \"listen_addr\": \"\"\n}\n' "$new" > "$WORK_DIR/settings.json"
    chmod 600 "$WORK_DIR/settings.json"
  fi
  svc_restart
  echo -e "  ${G}管理端口已修改为 ${new}${N}"
}

change_listen_addr() {
  local cur
  cur=$(web_listen_addr)
  echo
  echo -e "  当前监听地址: ${B}${cur}${N}"
  echo "  1) 127.0.0.1 (仅本地监听，便于 Nginx / 1Panel / Caddy 等反向代理)"
  echo "  2) 0.0.0.0   (公网 IP 直接访问)"
  echo
  read -rp "  请选择 [1/2]: " opt
  local new_addr=""
  case "$opt" in
    1) new_addr="127.0.0.1" ;;
    2) new_addr="0.0.0.0" ;;
    *) echo "  取消操作"; return ;;
  esac

  local port
  port=$(web_port)
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    if grep -q '"listen_addr"' "$WORK_DIR/settings.json"; then
      sed -i "s/\"listen_addr\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"listen_addr\": \"${new_addr}\"/" "$WORK_DIR/settings.json"
    else
      sed -i "s/\"port\"[[:space:]]*:[[:space:]]*[0-9]*/\"port\": ${port},\n  \"listen_addr\": \"${new_addr}\"/" "$WORK_DIR/settings.json"
    fi
  else
    printf '{\n  \"port\": %s,\n  \"listen_addr\": \"%s\"\n}\n' "$port" "$new_addr" > "$WORK_DIR/settings.json"
    chmod 600 "$WORK_DIR/settings.json"
  fi

  svc_restart
  if [[ "$new_addr" == "127.0.0.1" ]]; then
    echo -e "  ${G}已成功切换为 127.0.0.1 监听（仅允许本地反向代理访问）${N}"
  else
    echo -e "  ${G}已成功切换为 0.0.0.0 监听（公网 IP 可直接访问）${N}"
  fi
}

change_panel_url() {
  local cur new_url
  cur=$(web_panel_url)
  echo
  echo -e "  当前面板 URL: ${B}${cur:-(未设置)}${N}"
  read -rp "  请输入新面板 URL (如 https://example.com 或 https://example.com/，留空清除): " new_url
  new_url=$(echo "$new_url" | tr -d ' \r\n')
  # 去除结尾所有斜杠
  new_url=$(echo "$new_url" | sed -e 's:/*$::')

  python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    try:
        with open(path, 'r') as f:
            data = json.load(f)
    except: pass
if '$new_url':
    data['panel_url'] = '$new_url'
else:
    data.pop('panel_url', None)
with open(path, 'w') as f:
    json.dump(data, f, indent=2)
"
  svc_restart
  if [[ -n "$new_url" ]]; then
    echo -e "  ${G}面板基础 URL 已更新为: ${new_url}${N}"
  else
    echo -e "  ${Y}已清除自定义面板 URL，恢复默认显示${N}"
  fi
}

reset_password() {
  local pw
  echo
  read -rp "  请输入新口令 (留空随机生成): " pw
  if [[ -z "$pw" ]]; then
    pw=$(head -c 9 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
  umask 077
  echo "$pw" > "$WORK_DIR/password"
  svc_restart
  echo -e "  ${G}新访问口令: ${pw}${N}"
}

reset_basepath() {
  local bp
  echo
  read -rp "  请输入新路径 (留空随机): " bp
  if [[ -z "$bp" ]]; then
    rm -f "$WORK_DIR/basepath"
    svc_restart
    sleep 2
    bp=$(cat "$WORK_DIR/basepath" 2>/dev/null)
  else
    bp=${bp#/}; bp=${bp%/}
    umask 077
    echo "$bp" > "$WORK_DIR/basepath"
    svc_restart
  fi
  echo -e "  ${G}新路径: /${bp}/${N}"
}

change_ssl() {
  local cur_en cur_dom cur_cert cur_key
  cur_en=$(web_ssl_enabled)
  cur_dom=$(web_ssl_domain)
  cur_cert=$(web_ssl_cert)
  cur_key=$(web_ssl_key)

  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  sout 原生 SSL / HTTPS 设置${N}"
  echo -e "${B}========================================${N}"
  if [[ "$cur_en" == "true" ]]; then
    echo -e "  当前状态:      ${G}已开启 SSL (HTTPS)${N}"
    echo -e "  域名:          ${B}${cur_dom:-(未设置)}${N}"
    echo -e "  证书 (cert):   ${B}${cur_cert}${N}"
    echo -e "  私钥 (key):    ${B}${cur_key}${N}"
    echo -e "${D}----------------------------------------${N}"
    echo "  1) 关闭 SSL (切换回 HTTP)"
    echo "  2) 修改证书与私钥路径"
    echo "  3) 修改域名"
    echo "  0) 返回主菜单"
    echo
    read -rp "  请选择 [0-3]: " opt
    case "$opt" in
      1)
        python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    with open(path) as f: data = json.load(f)
data['ssl_enabled'] = False
with open(path, 'w') as f: json.dump(data, f, indent=2)
"
        svc_restart
        echo -e "  ${Y}已关闭 SSL，面板已切换回 HTTP 访问${N}"
        ;;
      2)
        echo
        read -rp "  请输入新 SSL 证书 (cert) 绝对路径: " new_cert
        read -rp "  请输入新 SSL 私钥 (key) 绝对路径: " new_key
        [[ -z "$new_cert" || -z "$new_key" ]] && { echo "  路径不能为空，未修改"; return; }
        if [[ ! -f "$new_cert" ]]; then
          echo -e "  ${R}证书文件不存在: ${new_cert}${N}"
          return
        fi
        if [[ ! -f "$new_key" ]]; then
          echo -e "  ${R}私钥文件不存在: ${new_key}${N}"
          return
        fi
        python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    with open(path) as f: data = json.load(f)
data['ssl_cert'] = '$new_cert'
data['ssl_key'] = '$new_key'
with open(path, 'w') as f: json.dump(data, f, indent=2)
"
        svc_restart
        echo -e "  ${G}SSL 证书路径已更新并重启生效${N}"
        ;;
      3)
        echo
        read -rp "  请输入新域名 (如 sout.example.com): " new_dom
        python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    with open(path) as f: data = json.load(f)
data['ssl_domain'] = '$new_dom'
with open(path, 'w') as f: json.dump(data, f, indent=2)
"
        svc_restart
        echo -e "  ${G}域名已更新为: ${new_dom}${N}"
        ;;
      *) ;;
    esac
  else
    echo -e "  当前状态:      ${D}未开启 SSL (当前为 HTTP)${N}"
    echo -e "${D}----------------------------------------${N}"
    echo "  1) 开启 SSL (HTTPS)"
    echo "  0) 返回主菜单"
    echo
    read -rp "  请选择 [0-1]: " opt
    if [[ "$opt" == "1" ]]; then
      echo
      read -rp "  请输入绑定域名 (如 sout.example.com，可选留空): " new_dom
      read -rp "  请输入 SSL 证书 (cert) 绝对路径: " new_cert
      read -rp "  请输入 SSL 私钥 (key) 绝对路径: " new_key
      if [[ -z "$new_cert" || -z "$new_key" ]]; then
        echo -e "  ${R}证书与私钥路径不能为空！${N}"
        return
      fi
      if [[ ! -f "$new_cert" ]]; then
        echo -e "  ${R}证书文件不存在: ${new_cert}${N}"
        return
      fi
      if [[ ! -f "$new_key" ]]; then
        echo -e "  ${R}私钥文件不存在: ${new_key}${N}"
        return
      fi
      python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    with open(path) as f: data = json.load(f)
data['ssl_enabled'] = True
data['ssl_domain'] = '$new_dom'
data['ssl_cert'] = '$new_cert'
data['ssl_key'] = '$new_key'
with open(path, 'w') as f: json.dump(data, f, indent=2)
"
      svc_restart
      echo -e "  ${G}🎉 SSL 已成功开启！面板已切换为 HTTPS 安全加密访问。${N}"
    fi
  fi
}

cleanup_sui() {
  local db="/usr/local/s-ui/db/s-ui.db"
  if [[ ! -f "$db" ]]; then
    return
  fi
  echo -e "  正在清理 s-ui 中由 sout 创建的所有出入站及路由规则..."
  python3 -c "
import sqlite3, json

db = '/usr/local/s-ui/db/s-ui.db'
try:
    con = sqlite3.connect(db)
    cur = con.cursor()
    
    # 1. 查找所有 sout/fanout 创建的入站 ID 和 Tag
    cur.execute('SELECT id, tag FROM inbounds')
    inbounds = cur.fetchall()
    
    sout_inb_ids = []
    sout_inb_tags = []
    for (ib_id, tag) in inbounds:
        tag_str = str(tag)
        if '家宽' in tag_str or 'fanout' in tag_str or 'sout' in tag_str:
            sout_inb_ids.append(ib_id)
            sout_inb_tags.append(tag_str)
    
    if sout_inb_ids:
        for ib_id in sout_inb_ids:
            con.execute('DELETE FROM inbounds WHERE id = ?', (ib_id,))
        print(f'    - 已清理 {len(sout_inb_ids)} 个 sout 分流入站: {sout_inb_tags}')
    
    # 2. 删除所有 sout-* 与 fanout-* 出站
    cur.execute(\"DELETE FROM outbounds WHERE tag LIKE 'sout-%' OR tag LIKE 'fanout-%'\")
    out_deleted = cur.rowcount
    if out_deleted > 0:
        print(f'    - 已清理 {out_deleted} 个 sout 出站隧道')

    # 3. 清理 settings 表中 config 的 route.rules
    cur.execute(\"SELECT value FROM settings WHERE key = 'config'\")
    row = cur.fetchone()
    if row and row[0]:
        try:
            cfg = json.loads(row[0])
            rules = cfg.get('route', {}).get('rules', [])
            new_rules = []
            for r in rules:
                outbound = r.get('outbound', '')
                if not outbound.startswith('sout-') and not outbound.startswith('fanout-'):
                    new_rules.append(r)
            if len(new_rules) != len(rules):
                cfg['route']['rules'] = new_rules
                con.execute(\"UPDATE settings SET value = ? WHERE key = 'config'\", (json.dumps(cfg, indent=2),))
                print(f'    - 已清理 {len(rules) - len(new_rules)} 条 sout 分流路由规则')
        except Exception as e:
            print('    - 清理路由规则警告:', e)

    # 4. 清理 clients
    cur.execute(\"DELETE FROM clients WHERE name LIKE 'sout-%' OR name LIKE 'fanout-%'\")

    con.commit()
    con.close()
    print('  已完全恢复 s-ui 原始数据库与节点配置。')
except Exception as e:
    print('  清理 s-ui 数据时出现异常:', e)
" 2>/dev/null || true

  systemctl restart s-ui 2>/dev/null || true
}

uninstall_sout_only() {
  local yes
  echo
  read -rp "  确定仅卸载 sout 插件服务吗？(保留 s-ui 面板及其节点配置) [y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  echo "  正在停止并卸载 sout 及反代服务..."
  svc_stop >/dev/null 2>&1 || true
  svc_disable >/dev/null 2>&1 || true
  systemctl stop fanout 2>/dev/null || true
  systemctl disable fanout 2>/dev/null || true

  # 彻底清理 Caddy 与 cloudflared 隧道
  systemctl stop caddy 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || true
  systemctl stop cloudflared 2>/dev/null || true
  systemctl disable cloudflared 2>/dev/null || true
  rm -f /etc/systemd/system/caddy.service /etc/systemd/system/cloudflared.service 2>/dev/null || true
  rm -rf /etc/caddy /var/lib/caddy /var/log/caddy /usr/local/bin/caddy /usr/local/bin/cloudflared /usr/local/bin/sout-quick-tunnel /var/log/cloudflared* 2>/dev/null || true

  # 恢复 s-ui 为公网直连监听
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  if [[ -f "$sui_db" ]] && command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$sui_db" "UPDATE settings SET value='' WHERE key='webListen';" 2>/dev/null || true
    sqlite3 "$sui_db" "UPDATE settings SET value='8443' WHERE key='webPort';" 2>/dev/null || true
    sqlite3 "$sui_db" "UPDATE settings SET value='/app/' WHERE key='webPath';" 2>/dev/null || true
    systemctl restart s-ui 2>/dev/null || true
  fi

  for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep -E '^(fo|so)[0-9]'); do
    ip netns del "$ns" 2>/dev/null || true
  done
  for l in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | grep -E '^(fov|sov)[0-9]'); do
    ip link del "$l" 2>/dev/null || true
  done

  rm -f "/etc/systemd/system/sout.service" "/etc/systemd/system/fanout.service" "/etc/init.d/sout" "/etc/init.d/fanout"
  rm -f "$BIN" /usr/local/bin/sout /usr/local/bin/fanout /usr/local/bin/f /usr/local/bin/sout-cli
  rm -rf "$WORK_DIR" /var/lib/sout /var/lib/fanout 2>/dev/null || true

  svc_reload
  echo -e "  ${G}[✓] sout 插件及反代组件已彻底清理完成，s-ui 面板已恢复公网直连模式！${N}"
  exit 0
}

uninstall_sui_only() {
  local yes
  echo
  read -rp "  确定仅卸载 s-ui 面板吗？(保留 sout 服务) [y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  echo "  正在停止并卸载 s-ui 面板..."
  systemctl stop s-ui 2>/dev/null || true
  systemctl disable s-ui 2>/dev/null || true
  rm -f /etc/systemd/system/s-ui.service /etc/init.d/s-ui 2>/dev/null || true
  systemctl daemon-reload 2>/dev/null || true
  systemctl reset-failed 2>/dev/null || true
  rm -rf /etc/s-ui /usr/local/s-ui 2>/dev/null || true
  rm -f /usr/bin/s-ui /usr/local/bin/s-ui /usr/bin/sui /usr/local/bin/sui 2>/dev/null || true
  echo -e "  ${G}[✓] s-ui 面板已卸载完成，sout 服务已保留。${N}"
}

uninstall_all() {
  local yes
  echo
  read -rp "  ⚠️ 确定彻底卸载 sout 和 s-ui 吗？所有节点与服务将被完全清理！[y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  echo "  正在停止并彻底清理所有服务与组件..."
  svc_stop >/dev/null 2>&1 || true
  svc_disable >/dev/null 2>&1 || true
  systemctl stop fanout 2>/dev/null || true
  systemctl disable fanout 2>/dev/null || true
  for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep -E '^(fo|so)[0-9]'); do
    ip netns del "$ns" 2>/dev/null || true
  done
  for l in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | grep -E '^(fov|sov)[0-9]'); do
    ip link del "$l" 2>/dev/null || true
  done

  # 彻底清理 Caddy 与 cloudflared 反代隧道组件
  systemctl stop caddy 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || true
  systemctl stop cloudflared 2>/dev/null || true
  systemctl disable cloudflared 2>/dev/null || true
  rm -f /etc/systemd/system/caddy.service /etc/systemd/system/cloudflared.service 2>/dev/null || true
  rm -rf /etc/caddy /var/lib/caddy /var/log/caddy /usr/local/bin/caddy /usr/local/bin/cloudflared /usr/local/bin/sout-quick-tunnel /var/log/cloudflared* /home/acme 2>/dev/null || true

  # 彻底清理 sout 二进制与工作目录
  rm -f "/etc/systemd/system/sout.service" "/etc/systemd/system/fanout.service" "/etc/init.d/sout" "/etc/init.d/fanout"
  rm -f "$BIN" /usr/local/bin/sout /usr/local/bin/fanout /usr/local/bin/f /usr/local/bin/sout-cli
  rm -rf "$WORK_DIR" /var/lib/sout /var/lib/fanout 2>/dev/null || true

  # 彻底清理 s-ui
  systemctl stop s-ui 2>/dev/null || true
  systemctl disable s-ui 2>/dev/null || true
  rm -f /etc/systemd/system/s-ui.service /etc/init.d/s-ui 2>/dev/null || true
  systemctl daemon-reload 2>/dev/null || true
  systemctl reset-failed 2>/dev/null || true
  rm -rf /etc/s-ui /usr/local/s-ui 2>/dev/null || true
  rm -f /usr/bin/s-ui /usr/local/bin/s-ui /usr/bin/sui /usr/local/bin/sui 2>/dev/null || true

  svc_reload
  echo -e "  ${G}[✓] 所有组件 (sout, s-ui, Caddy, cloudflared) 已彻底卸载干净，系统已完全恢复初始状态！${N}"
  exit 0
}

do_uninstall() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  sout / s-ui 卸载管理${N}"
  echo -e "${B}========================================${N}"
  echo -e "   1) 仅卸载 sout (保留 s-ui 面板及其节点配置)"
  echo -e "   2) 仅卸载 s-ui (保留 sout 插件服务与设置)"
  echo -e "   3) 全部卸载   (同时彻底卸载 sout 与 s-ui)"
  echo -e "   0) 取消并返回"
  echo -e "${D}----------------------------------------${N}"
  local opt
  read -rp "  请选择 [0-3]: " opt
  case "$opt" in
    1) uninstall_sout_only ;;
    2) uninstall_sui_only ;;
    3) uninstall_all ;;
    *) echo "  已取消" ;;
  esac
}

check_and_update() {
  echo
  echo -e "  ${B}正在连接 GitHub 检查最新版本...${N}"
  local cur_ver="dev"
  if [[ -x "$BIN" ]]; then
    cur_ver=$("$BIN" -version 2>/dev/null | awk '{print $NF}' || echo "dev")
  fi
  [[ -z "$cur_ver" ]] && cur_ver="dev"

  local rel_json tag_name html_url
  rel_json=$(curl -fsSLm 8 "https://api.github.com/repos/ustdbus/sout/releases/latest" 2>/dev/null || true)
  if [[ -z "$rel_json" ]]; then
    echo -e "  ${R}检查更新失败，无法连接 GitHub API${N}"
    return
  fi

  tag_name=$(echo "$rel_json" | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | cut -d'"' -f4)
  html_url=$(echo "$rel_json" | grep -oE '"html_url"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | cut -d'"' -f4)

  if [[ -z "$tag_name" ]]; then
    echo -e "  ${R}未能获取到最新版本信息${N}"
    return
  fi

  echo -e "  当前安装版本: ${Y}${cur_ver}${N}"
  echo -e "  GitHub 最新版: ${G}${tag_name}${N}"

  local is_newer=0
  if python3 -c "
import sys, re
def p(v):
    nums = re.findall(r'\d+', v)
    return tuple(map(int, nums)) if nums else (0,)
cur = p('${cur_ver}')
latest = p('${tag_name}')
sys.exit(0 if latest > cur else 1)
" 2>/dev/null; then
    is_newer=1
  fi

  if [[ "$is_newer" -eq 1 ]]; then
    echo -e "  ${G}发现新版本 ${tag_name}！${N}"
    read -rp "  是否立即更新到最新版本？[Y/n]: " do_up
    [[ ${do_up,,} == n ]] && return
  else
    echo -e "  ${G}当前已是最新版本 (${cur_ver})！${N}"
    read -rp "  是否仍要重新下载并覆盖安装 ${tag_name}？[y/N]: " force_up
    [[ ${force_up,,} == y ]] || return
  fi

  echo -e "  正在更新 sout 到 ${tag_name}..."
  local arch uname_m
  uname_m=$(uname -m)
  case "$uname_m" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo -e "  ${R}不支持的系统架构: $uname_m${N}"; return ;;
  esac

  local tar_url="https://github.com/ustdbus/sout/releases/download/${tag_name}/sout-linux-${arch}.tar.gz"
  local tmp_dir
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' RETURN

  echo -e "  正在下载: ${tar_url} ..."
  if ! curl -fsSL "$tar_url" -o "$tmp_dir/sout.tar.gz"; then
    echo -e "  ${R}下载发布包失败！${N}"
    return
  fi

  tar -zxf "$tmp_dir/sout.tar.gz" -C "$tmp_dir"
  if [[ -f "$tmp_dir/sout-server" ]]; then
    cp -f "$tmp_dir/sout-server" "$BIN"
    chmod +x "$BIN"
    ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  elif [[ -f "$tmp_dir/sout" ]]; then
    cp -f "$tmp_dir/sout" "$BIN"
    chmod +x "$BIN"
    ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  elif [[ -f "$tmp_dir/fanout" ]]; then
    cp -f "$tmp_dir/fanout" "$BIN"
    chmod +x "$BIN"
    ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  fi

  if [[ -f "$tmp_dir/f.sh" ]]; then
    cp -f "$tmp_dir/f.sh" /usr/local/bin/sout
    chmod +x /usr/local/bin/sout
    rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
  fi

  svc_restart
  echo -e "  ${G}恭喜！sout 已成功更新至 ${tag_name}，服务已自动重启生效。${N}"
}

#!/usr/bin/env bash
# ==============================================================================
# sout - Cloudflare 隧道 4合1 统一反代独立管理脚本
# ==============================================================================

set -e

R='[0;31m'
G='[0;32m'
Y='[0;33m'
B='[0;34m'
C='[0;36m'
D='[0;90m'
N='[0m'

WORK_DIR="/var/lib/sout"
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
    cat > /etc/systemd/system/cloudflared.service <<EOF
[Unit]
Description=Cloudflare Quick Tunnel Agent
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/cloudflared tunnel --url http://127.0.0.1:${tun_p} --no-autoupdate
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

get_quick_tunnel_domain() {
  local max_wait=20
  local d=""
  for ((i=1; i<=max_wait; i++)); do
    d=$(journalctl -u cloudflared -n 50 --no-pager 2>/dev/null | grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' | tail -1 | sed 's|https://||' | tr -d ' 
')
    [[ -n "$d" ]] && break
    sleep 1
  done
  echo "$d"
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
  r=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' 
')
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
  local domain="${1:-}"
  local tunnel_token="${2:-}"
  local tunnel_port="${3:-8081}"

  local is_quick="false"
  if [[ -z "$domain" && -z "$tunnel_token" ]]; then
    is_quick="true"
  fi

  echo
  echo -e "${B}================================================================${N}"
  if [[ "$is_quick" == "true" ]]; then
    echo -e "${G}  正在配置 Cloudflare 官方免费临时隧道 (免域名 / 免Token)...${N}"
  else
    echo -e "${B}  正在配置 Cloudflare 隧道 4合1 统一反代 (${domain})...${N}"
  fi
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

  # 3. 写入纯净本地 Caddyfile (双栈通配监听 :${tunnel_port})
  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy
  cat > /etc/caddy/Caddyfile <<EOF
{
    admin off
    auto_https off
}

:${tunnel_port} {
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
  setup_cloudflared_service "$tunnel_token" "$tunnel_port"

  if [[ "$is_quick" == "true" ]]; then
    echo -e "  [+] 正在等待 Cloudflare 分配免费临时域名..."
    domain=$(get_quick_tunnel_domain)
    if [[ -z "$domain" ]]; then
      echo -e "  ${Y}[!] 暂未即时获取到临时域名，稍后可通过 sout 查看。${N}"
      domain="临时隧道连接中.trycloudflare.com"
    else
      echo -e "  ${G}[✓] 成功获取免费临时域名: https://${domain}${N}"
    fi
  fi

  # 4. 自动化配置 s-ui
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_admin_user
  sui_admin_user=$(get_sui_user)

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

# 1. 更新 settings
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
        if 'sniff' in opts_j: del opts_j['sniff']
        if 'sniff_override_destination' in opts_j: del opts_j['sniff_override_destination']
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
    }
]
addrs_blob = json.dumps(addrs_data, indent=2).encode('utf-8')

options_dict = {
    'listen': '127.0.0.1',
    'listen_port': node_port,
    'users': [
        {
            'flow': '',
            'name': 'admin',
            'uuid': client_uuid
        }
    ],
    'transport': {
        'early_data_header_name': 'Sec-WebSocket-Protocol',
        'max_early_data': 2560,
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
  local meta_mode="tunnel"
  [[ "$is_quick" == "true" ]] && meta_mode="quick_tunnel"
  cat > "$CADDY_META" <<METAEOF
{
  "enabled": true,
  "mode": "${meta_mode}",
  "domain": "${domain}",
  "tunnel_token": "${tunnel_token}",
  "tunnel_port": ${tunnel_port},
  "sout_port": ${sout_port},
  "sout_path": "${sout_path}",
  "sui_port": ${sui_port},
  "sui_path": "${sui_path}",
  "sui_user": "${sui_admin_user}",
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
  if [[ "$is_quick" == "true" ]]; then
    echo -e "${G}  🎉 Cloudflare 官方免费临时隧道 4合1 统一反代已成功开启！${N}"
  else
    echo -e "${G}  🎉 Cloudflare 隧道 4合1 统一反代已成功开启！${N}"
  fi
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
  echo -e "      管理密码:  ${D}[由您在 s-ui 中设置，已安全加密]${N}"
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
  read -rp "  1. 请输入您的访问域名 (如 example.com): " domain
  domain=$(echo "$domain" | tr -d ' 
')
  [[ -z "$domain" ]] && { echo -e "  ${R}域名不能为空！${N}"; return 1; }

  echo -e "  ${D}💡 提示：前往 Cloudflare Zero Trust -> Networks -> Tunnels 创建隧道并复制 Token${N}"
  read -rp "  2. 请输入 Cloudflare 隧道 Token (eyJh...): " tunnel_token
  tunnel_token=$(echo "$tunnel_token" | tr -d ' 
')
  [[ -z "$tunnel_token" ]] && { echo -e "  ${R}隧道 Token 不能为空！${N}"; return 1; }

  echo
  echo -e "  ${D}💡 本地回源端口用于 cloudflared 将流量转发至本地 Caddy，默认 8081 即可${N}"
  read -rp "  3. 请输入本地回源端口 [默认 8081]: " tunnel_port
  tunnel_port=$(echo "$tunnel_port" | tr -d ' 
')
  tunnel_port="${tunnel_port:-8081}"

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
      [[ -z "$tun_p" ]] && tun_p="8081"

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

menu() {
  while true; do
    clear
    echo -e "${B}========================================${N}"
    echo -e "${B}  sout - s-ui 动态家宽出口插件 (VPN Gate) ${N}"
    echo -e "${B}========================================${N}"
    show_info
    echo -e "${D}----------------------------------------${N}"
    echo -e "   1) 启动服务          2) 停止服务"
    echo -e "   3) 重启服务          4) 查看运行日志"
    echo
    echo -e "   5) 开关开机自启      6) 查看出口隧道"
    echo -e "   7) 修改面板端口      8) 修改监听地址"
    echo -e "   9) 重置访问口令     10) 重置访问路径"
    echo -e "  11) 面板 URL 设置    12) SSL / HTTPS 设置"
    echo -e "  13) 检查/更新版本    14) 卸载"
    echo -e "   0) 退出脚本"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-14]: " choice

    case "$choice" in
      1) svc_start   && echo -e "\n  ${G}已启动${N}"; pause ;;
      2) svc_stop    && echo -e "\n  ${Y}已停止${N}"; pause ;;
      3) svc_restart && echo -e "\n  ${G}已重启${N}"; pause ;;
      4) echo; svc_logs 40; pause ;;
      5)
        if svc_is_enabled; then
          svc_disable
          echo -e "\n  ${Y}已关闭开机自启${N}"
        else
          svc_enable
          echo -e "\n  ${G}已开启开机自启${N}"
        fi
        pause ;;
      6) list_tunnels; pause ;;
      7) change_port; pause ;;
      8) change_listen_addr; pause ;;
      9) reset_password; pause ;;
      10) reset_basepath; pause ;;
      11) change_panel_url; pause ;;
      12) change_ssl; pause ;;
      13) check_and_update; pause ;;
      14) do_uninstall; pause ;;
      0) exit 0 ;;
      *) ;;
    esac
  done
}

need_root

case "${1:-}" in
  setup_tunnel|setup_caddy)
    shift
    setup_caddy_proxy "$@"
    ;;
  start)     svc_start ;;
  stop)      svc_stop ;;
  restart)   svc_restart ;;
  status)    show_info ;;
  log)       svc_logs_follow ;;
  info)      show_info ;;
  list)      list_tunnels ;;
  listen)    change_listen_addr ;;
  url)       change_panel_url ;;
  ssl)       change_ssl ;;
  caddy)     caddy_menu ;;
  update)    check_and_update ;;
  upgrade)   check_and_update ;;
  uninstall) do_uninstall ;;
  "")        menu ;;
  *)
    echo "用法: sout [start|stop|restart|status|log|info|list|listen|url|ssl|caddy|update|uninstall]"
    echo "直接在终端输入 sout 即可进入交互控制菜单"
    ;;
esac
