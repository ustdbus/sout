#!/usr/bin/env bash
set -e

# ==============================================================================
# 初始化系统检测：systemd / OpenRC (Alpine)
# ==============================================================================
detect_init() {
  if [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null 2>&1; then
    echo "systemd"
  else
    echo "openrc"
  fi
}
INIT_SYS=$(detect_init)

# 在 OpenRC/Alpine 上自动把 systemctl 调用翻译为 rc-service / rc-update。
# 优先从 systemd unit 生成 OpenRC init 脚本，保证 caddy/cloudflared 可被管理。
_openrc_init_from_unit() {
  local name="$1"
  local unit="/etc/systemd/system/${name}.service"
  local init="/etc/init.d/${name}"
  local exec_start command command_args
  [[ -f "$unit" ]] || return 1
  # 非 caddy/cloudflared 已有 OpenRC 脚本时不要覆盖（如 sout/s-ui）
  if [[ -x "$init" && "$name" != "caddy" && "$name" != "cloudflared" ]]; then
    return 0
  fi
  exec_start=$(sed -n 's/^ExecStart=//p' "$unit" | head -1)
  [[ -z "$exec_start" ]] && return 1
  command="${exec_start%% *}"
  command_args="${exec_start#* }"
  [[ -z "$command" ]] && return 1
  mkdir -p /var/log
  cat > "$init" <<EOF
#!/sbin/openrc-run
name="$name"
description="$name service"
command="$command"
command_args="$command_args"
command_background=true
pidfile="/run/${name}.pid"
output_log="/var/log/${name}.log"
error_log="/var/log/${name}.log"
respawn_delay=5
supervisor=supervise-daemon

depend() {
    need net
    after firewall
}
EOF
  chmod +x "$init"
}

_openrc_force_stop() {
  local name="$1"
  case "$name" in
    s-ui)
      pkill -9 -f "supervise-daemon s-ui" 2>/dev/null || true
      pkill -9 -f "/usr/local/s-ui/sui" 2>/dev/null || true
      ;;
    sout|fanout)
      pkill -9 -f "/usr/local/bin/sout-server" 2>/dev/null || true
      pkill -9 -f "/usr/local/bin/fanout" 2>/dev/null || true
      ;;
    caddy)
      pkill -9 -f "/usr/local/bin/caddy" 2>/dev/null || true
      ;;
    cloudflared)
      pkill -9 -f "/usr/local/bin/cloudflared" 2>/dev/null || true
      ;;
  esac
}

systemctl() {
  local action="$1"
  shift
  # 过滤常用的 systemctl 参数
  while [[ $# -gt 0 && "$1" == -* ]]; do
    shift
  done
  local name="${1:-}"
  [[ -n "$name" ]] && name="${name%.service}"

  if [[ "$INIT_SYS" == "systemd" ]]; then
    # 调用真实 systemctl
    if [[ -z "$name" && ( "$action" == "daemon-reload" || "$action" == "reset-failed" ) ]]; then
      command systemctl "$action" "$@"
    else
      command systemctl "$action" "$name" "$@"
    fi
    return $?
  fi

  case "$action" in
    daemon-reload|reset-failed)
      return 0
      ;;
    is-active)
      if [[ -x /etc/init.d/${name} ]] && rc-service "$name" status >/dev/null 2>&1; then
        echo "active"
        return 0
      fi
      return 3
      ;;
    is-enabled)
      if [[ -x /etc/init.d/${name} ]] && rc-update show default 2>/dev/null | grep -qw "$name"; then
        echo "enabled"
        return 0
      fi
      return 1
      ;;
    is-failed)
      if [[ -x /etc/init.d/${name} ]] && rc-service "$name" status >/dev/null 2>&1; then
        echo "running"
        return 0
      fi
      echo "failed"
      return 1
      ;;
    start|stop|restart)
      if [[ "$name" == "caddy" || "$name" == "cloudflared" ]]; then
        _openrc_init_from_unit "$name" || true
      fi
      if [[ -x /etc/init.d/${name} ]]; then
        if [[ "$action" == "restart" ]]; then
          rc-service "$name" stop >/dev/null 2>&1 || true
          _openrc_force_stop "$name"
          sleep 0.3
          rc-service "$name" start
        elif [[ "$action" == "stop" ]]; then
          rc-service "$name" stop >/dev/null 2>&1 || true
          _openrc_force_stop "$name"
        else
          rc-service "$name" start
        fi
      else
        return 0
      fi
      ;;
    enable)
      if [[ "$name" == "caddy" || "$name" == "cloudflared" ]]; then
        _openrc_init_from_unit "$name" || true
      fi
      if [[ -x /etc/init.d/${name} ]]; then
        rc-update add "$name" default >/dev/null 2>&1 || true
      fi
      return 0
      ;;
    disable)
      if [[ -x /etc/init.d/${name} ]]; then
        rc-update del "$name" default >/dev/null 2>&1 || true
      fi
      return 0
      ;;
    status)
      if [[ -x /etc/init.d/${name} ]]; then
        rc-service "$name" status
        return $?
      fi
      echo "Unit $name.service could not be found."
      return 4
      ;;
    *)
      return 0
      ;;
  esac
}

journalctl() {
  local unit="" lines="50" follow=0
  if [[ "$INIT_SYS" == "systemd" ]]; then
    command journalctl "$@"
    return $?
  fi
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -u) unit="$2"; shift 2;;
      -n) lines="$2"; shift 2;;
      -f|--follow) follow=1; shift;;
      --no-pager|-e|--quiet|-q|--no-tail|--all) shift;;
      -*) shift;;
      *) shift;;
    esac
  done
  unit="${unit%.service}"
  if [[ -z "$unit" ]]; then
    return 0
  fi
  if [[ "$unit" == "sout" || "$unit" == "fanout" ]]; then
    tail -n "$lines" /var/log/${unit}.log /var/log/${unit}.err 2>/dev/null
  elif [[ -f "/var/log/${unit}.log" ]]; then
    tail -n "$lines" "/var/log/${unit}.log"
  elif [[ -f "/var/log/${unit}_quick.log" ]]; then
    tail -n "$lines" "/var/log/${unit}_quick.log"
  fi
  if [[ "$follow" == "1" ]]; then
    if [[ -f "/var/log/${unit}.log" ]]; then
      tail -n 0 -f "/var/log/${unit}.log"
    else
      sleep 3600
    fi
  fi
  return 0
}


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
svc_start()      { systemctl start "$UNIT"; }
svc_stop()       { systemctl stop "$UNIT"; }
svc_restart()    { systemctl restart "$UNIT"; }
svc_reload()     { systemctl daemon-reload; }
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
  purl=$(web_panel_url)
  ssl_en=$(web_ssl_enabled)
  ssl_dom=$(web_ssl_domain)
  c_en=$(is_caddy_enabled)
  # 隧道模式下不需要探测公网 IP，减少不必要的网络请求（对小内存机更友好）
  pip=""
  if [[ "$c_en" != "true" ]]; then
    pip=$(public_ip)
  fi

  scheme="http"
  [[ "$ssl_en" == "true" ]] && scheme="https"

  bp="/${bp#/}"
  [[ "$bp" != */ ]] && bp="${bp}/"
  if [[ -n "$purl" ]]; then
    purl="${purl%/}"
    full_url="${purl}${bp}"
  fi

  local cur_ver="dev"
  if [[ -f "${WORK_DIR}/version" ]]; then
    cur_ver=$(cat "${WORK_DIR}/version" 2>/dev/null | tr -d ' \r\n')
  fi
  [[ -z "$cur_ver" ]] && cur_ver="dev"

  echo
  echo -e "  程序版本:    ${G}${cur_ver}${N}"
  if [[ "$st" == "active" ]]; then
    echo -e "  服务状态:    ${G}运行中 (active)${N}"
  else
    echo -e "  服务状态:    ${R}已停止 (${st})${N}"
  fi

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
      echo -e "  反代模式:    ${G}Cloudflare 官方免费临时隧道连接和Caddy流量代理 (已开启)${N}"
    else
      echo -e "  反代模式:    ${G}Cloudflare隧道连接和Caddy流量代理 (已开启)${N}"
    fi

    echo -e "  隧道服务:    ${cf_st} (本地回源: 127.0.0.1:${c_tun_p})"
    echo -e "  管理面板:    ${B}https://${c_dom}/${c_sout_p}/${N}"
    echo -e "  访问口令:    ${Y}${pw}${N}"
    echo -e "  s-ui 面板:   ${B}https://${c_dom}/${c_sui_p}/${N}"
    echo -e "  s-ui 用户名: ${Y}${sui_u}${N}"
    echo -e "  s-ui 密  码: ${D}[由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]${N}"
      echo -e "  订阅链接:    ${B}https://${c_dom}/${c_sout_p}/sub=${pw}${N}"
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
      echo -e "  s-ui 密  码: ${D}[由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]${N}"
    fi
  fi
  echo -e "  s-ui 唤起命令: ${C}s-ui${N}"
  echo -e "  sout 唤起命令: ${C}sout${N}"
  echo
}

change_listen_and_port() {
  local cur_addr cur_port new_addr new_port
  cur_addr=$(web_listen_addr)
  cur_port=$(web_port)
  [[ -z "$cur_addr" ]] && cur_addr="0.0.0.0"
  [[ -z "$cur_port" ]] && cur_port="8899"

  echo
  echo -e "  当前监听地址: ${B}${cur_addr}${N}"
  echo -e "  当前管理端口: ${B}${cur_port}${N}"
  echo
  echo "  [1/2] 设置面板监听地址："
  echo "  1) 127.0.0.1 (仅内网 / 便于 Nginx、1Panel、Caddy 等反向代理)"
  echo "  2) 0.0.0.0   (公网 IP 直接访问)"
  read -rp "  请选择 [1/2] (直接回车保持当前: ${cur_addr}): " opt_addr

  case "$opt_addr" in
    1) new_addr="127.0.0.1" ;;
    2) new_addr="0.0.0.0" ;;
    "") new_addr="$cur_addr" ;;
    *) echo -e "  ${Y}输入无效，保持当前监听地址: ${cur_addr}${N}"; new_addr="$cur_addr" ;;
  esac

  echo
  echo "  [2/2] 设置面板管理端口："
  read -rp "  请输入新管理端口 (直接回车保持当前: ${cur_port}): " input_port
  if [[ -z "$input_port" ]]; then
    new_port="$cur_port"
  elif ! [[ "$input_port" =~ ^[0-9]+$ ]] || (( input_port < 1 || input_port > 65535 )); then
    echo -e "  ${R}端口不合法 (必须为 1-65535 之间的整数)，取消修改${N}"
    return
  else
    new_port="$input_port"
  fi

  if [[ "$new_addr" == "$cur_addr" && "$new_port" == "$cur_port" ]]; then
    echo -e "  ${Y}监听地址与端口未做任何修改${N}"
    return
  fi

  python3 -c "
import json, os
path = '$WORK_DIR/settings.json'
data = {}
if os.path.exists(path):
    try:
        with open(path, 'r') as f:
            data = json.load(f)
    except: pass
data['listen_addr'] = '$new_addr'
data['port'] = int('$new_port')
with open(path, 'w') as f:
    json.dump(data, f, indent=2)
os.chmod(path, 0o600)
"
  svc_restart
  echo -e "  ${G}面板配置已更新并生效: 监听地址 -> ${new_addr}，管理端口 -> ${new_port}${N}"
}

change_port() {
  change_listen_and_port
}

change_listen_addr() {
  change_listen_and_port
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
  rm -f /etc/init.d/caddy /etc/init.d/cloudflared 2>/dev/null || true
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
  rm -f /etc/init.d/caddy /etc/init.d/cloudflared 2>/dev/null || true
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

  # 清理运行日志、PID 与可能存在的旧迁移目录
  rm -f /var/log/sout.log /var/log/sout.err /var/log/s-ui.log /var/log/caddy.log /var/log/cloudflared.log /var/log/cloudflared_quick.log 2>/dev/null || true
  rm -f /run/sout.pid /run/fanout.pid /run/caddy.pid /run/cloudflared.pid /run/s-ui.pid 2>/dev/null || true
  rm -rf /usr/local/sout 2>/dev/null || true
  rm -rf /root/.local/share/caddy /root/.config/caddy /root/.cache/caddy /root/.cloudflared 2>/dev/null || true

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
  if [[ -f "${WORK_DIR}/version" ]]; then
    cur_ver=$(cat "${WORK_DIR}/version" 2>/dev/null | tr -d ' \r\n')
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
# sout - Cloudflare隧道连接和Caddy流量代理独立管理脚本
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
  local arch
  arch=$(get_caddy_arch)
  if ! command -v caddy >/dev/null 2>&1; then
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
  fi

  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy
  chmod 755 /etc/caddy /var/log/caddy
  mkdir -p /etc/systemd/system

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

  mkdir -p /etc/systemd/system
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

  # 清空旧日志，避免 get_quick_tunnel_domain 读到上一次临时隧道的旧域名
  : > /var/log/cloudflared.log 2>/dev/null || true
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
    echo -e "${B}  正在配置 Cloudflare隧道连接和Caddy流量代理 (${domain})...${N}"
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

      # 3. sout 订阅接口（重写到 sout 面板的 /sub）
      handle /${sub_path}* {
          rewrite * /${sout_path}/sub{uri}
          reverse_proxy 127.0.0.1:${sout_port}
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
    local sui_token
    sui_token=$(cat "${WORK_DIR}/sui-token" 2>/dev/null || true)
    if [[ -z "$sui_token" ]]; then
      echo -e "  ${Y}[!] 未找到 s-ui API Token，跳过自动配置（请先启动 sout 生成 Token）${N}"
    else
      echo -e "  [+] 正在通过 s-ui API 自动配置 (路径分流与 vmess-argo 节点)..."
      if ! SUI_API="http://127.0.0.1:${sui_port}/${sui_path}/apiv2" \
      SUI_TOKEN="$sui_token" \
        SUI_DB="$sui_db" \
      DOMAIN="$domain" \
      SUI_PORT="$sui_port" \
      SUI_PATH="/${sui_path}/" \
      SUB_PORT="$sub_port" \
      SUB_PATH="/${sub_path}/" \
      NODE_PORT="$node_port" \
      WS_PATH="/${ws_path}" \
      SUI_ADMIN_USER="$sui_admin_user" \
      python3 <<'PYEOF'
import json, os, sqlite3, uuid, urllib.request, urllib.parse

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

def api(method, endpoint, form=None):
    url = BASE.rstrip('/') + '/' + endpoint.lstrip('/')
    data = urllib.parse.urlencode(form).encode() if form else None
    headers = {'Token': TOKEN}
    if data is not None:
        headers['Content-Type'] = 'application/x-www-form-urlencoded'
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=20) as resp:
        return json.loads(resp.read().decode('utf-8'))

# 1. 更新 s-ui settings，全部走 s-ui API
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
api('POST', 'save', {
    'object': 'settings',
    'action': 'set',
    'data': json.dumps(settings_data),
})

# 2. 通过 s-ui API 创建/更新 vmess-argo 入站
inbounds_resp = api('GET', 'inbounds') or {}
inbounds_obj = inbounds_resp.get('obj') or []
if isinstance(inbounds_obj, dict):
    inbound_rows = inbounds_obj.get('inbounds') or []
else:
    inbound_rows = inbounds_obj or []
inbound_rows = [r for r in inbound_rows if isinstance(r, dict)]
node_tag = 'vmess-argo'
existing_inbound_id = None
for row in inbound_rows:
    if row.get('tag') == node_tag:
        existing_inbound_id = row.get('id')
        break

client_uuid = str(uuid.uuid4())
addrs_data = [{
    'server': os.environ['DOMAIN'],
    'server_port': 443,
    'tls': {
        'disable_sni': False,
        'enabled': True,
        'insecure': False,
        'server_name': os.environ['DOMAIN'],
        'utls': {
            'enabled': True,
            'fingerprint': 'chrome'
        }
    }
}]
inbound_payload = {
    'id': existing_inbound_id or 0,
    'type': 'vmess',
    'tag': node_tag,
    'tls_id': 0,
    'listen': '127.0.0.1',
    'listen_port': int(os.environ['NODE_PORT']),
    'addrs': addrs_data,
    'transport': {
        'early_data_header_name': 'Sec-WebSocket-Protocol',
        'max_early_data': 2560,
        'headers': {
            'Host': os.environ['DOMAIN']
        },
        'path': os.environ['WS_PATH'],
        'type': 'ws'
    }
}
api('POST', 'save', {
    'object': 'inbounds',
    'action': 'edit' if existing_inbound_id else 'new',
    'data': json.dumps(inbound_payload),
})

# 3. 重新查询入站 id（新建后需要）
inbounds_resp = api('GET', 'inbounds') or {}
inbounds_obj = inbounds_resp.get('obj') or []
if isinstance(inbounds_obj, dict):
    inbound_rows = inbounds_obj.get('inbounds') or []
else:
    inbound_rows = inbounds_obj or []
inbound_rows = [r for r in inbound_rows if isinstance(r, dict)]
inbound_id = None
for row in inbound_rows:
    if row.get('tag') == node_tag:
        inbound_id = row.get('id')
        break
if not inbound_id:
    raise SystemExit('创建 vmess-argo 入站失败')

# 4. 通过 s-ui API 保存 admin 客户端，由 s-ui 自动生成订阅链接（含 ed/fp）
clients_resp = api('GET', 'clients') or {}
clients_obj = clients_resp.get('obj') or []
if isinstance(clients_obj, dict):
    clients_rows = clients_obj.get('clients') or []
else:
    clients_rows = clients_obj or []
clients_rows = [r for r in clients_rows if isinstance(r, dict)]
admin = None
for row in clients_rows:
    if row.get('name') == os.environ.get('SUI_ADMIN_USER', 'admin'):
        admin = row
        break
client_payload = {
    'id': admin.get('id', 0) if admin else 0,
    'enable': True,
    'name': os.environ.get('SUI_ADMIN_USER', 'admin'),
    'remark': '默认用户',
    'config': {
        'vmess': {
            'name': os.environ.get('SUI_ADMIN_USER', 'admin'),
            'uuid': client_uuid
        }
    },
    'inbounds': [inbound_id],
    'links': [],
    'volume': 0,
    'expiry': 0,
    'down': 0,
    'up': 0,
    'desc': '',
    'group': '',
    'delayStart': False,
    'autoReset': False,
    'resetDays': 0,
    'nextReset': 0,
    'totalUp': 0,
    'totalDown': 0,
    'createdAt': 0,
    'onlineAt': 0,
}
api('POST', 'save', {
    'object': 'clients',
    'action': 'edit' if admin else 'new',
    'data': json.dumps(client_payload),
})
PYEOF
      then
        echo -e "  ${Y}[!] s-ui API 自动配置未完成，请稍后在 s-ui 面板手动补充节点/订阅。${N}"
      fi
      systemctl restart s-ui 2>/dev/null || true
    fi
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
    echo -e "${G}  🎉 Cloudflare 官方免费临时隧道连接和Caddy流量代理已成功开启！${N}"
  else
    echo -e "${G}  🎉 Cloudflare隧道连接和Caddy流量代理已成功开启！${N}"
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
  echo -e "      管理密码:  ${D}[由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]${N}"
  echo
    echo -e "  [3] sout 订阅地址:  ${B}https://${domain}/${sout_path}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")${N}"
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
  echo -e "${B}  🚀 Cloudflare隧道连接和Caddy流量代理一键配置 (免开端口/杜绝525)${N}"
  echo -e "${B}================================================================${N}"
  echo -e "  特点：无需公网端口、无视NAT网络、免申请SSL证书、杜绝525握手错误"
  echo -e "${D}----------------------------------------------------------------${N}"
  echo -e "  ${Y}💡 提示：如果你要使用固定隧道，请提前准备好以下内容：${N}"
  echo -e "     ${D}1) 已在 Cloudflare 中添加的访问域名${N}"
  echo -e "     ${D}2) Cloudflare 隧道 Token${N}"
  echo -e "     ${D}3) 在 Cloudflare 中为该隧道配置的端口/回源端口${N}"
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
    echo -e "${B}  Cloudflare隧道连接和Caddy流量代理管理${N}"
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
              echo -e "  [3] sout 订阅地址:  ${B}https://${dom}/${sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")${N}"
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
      echo -e "  💡 提示:       ${Y}强烈推荐开启 Cloudflare隧道连接和Caddy流量代理 (免开端口/杜绝525)${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 开启 Cloudflare 官方免费临时隧道 (免域名/免Token)"
      echo "  2) 使用固定域名 + 隧道 Token 配置"
      echo "  0) 返回上级菜单"
      echo
      read -rp "  请选择 [0-2]: " opt
      case "$opt" in
        1) setup_caddy_proxy "" "" ""; pause ;;
        2) caddy_interactive_setup; pause ;;
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
    echo -e "   5) 修改面板监听地址和端口"
    echo -e "   6) 重置访问口令      7) 重置访问路径"
    echo -e "   8) 面板 URL 设置     9) SSL / HTTPS 设置"
    echo -e "  10) Cloudflare 隧道查看/更换"
    echo -e "  11) 检查/更新版本    12) 卸载"
    echo -e "   0) 退出脚本"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-12]: " choice

    case "$choice" in
      1) svc_start   && echo -e "\n  ${G}已启动${N}"; pause ;;
      2) svc_stop    && echo -e "\n  ${Y}已停止${N}"; pause ;;
      3) svc_restart && echo -e "\n  ${G}已重启${N}"; pause ;;
      4) echo; svc_logs 40; pause ;;
      5) change_listen_and_port; pause ;;
      6) reset_password; pause ;;
      7) reset_basepath; pause ;;
      8) change_panel_url; pause ;;
      9) change_ssl; pause ;;
      10) caddy_menu; pause ;;
      11) check_and_update; pause ;;
      12) do_uninstall; pause ;;
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
  listen|port) change_listen_and_port ;;
  url)       change_panel_url ;;
  ssl)       change_ssl ;;
  caddy|cf|tunnel) caddy_menu ;;
  update)    check_and_update ;;
  upgrade)   check_and_update ;;
  uninstall) do_uninstall ;;
  "")        menu ;;
  *)
    echo "用法: sout [start|stop|restart|status|log|info|listen|port|url|ssl|caddy|update|uninstall]"
    echo "直接在终端输入 sout 即可进入交互控制菜单"
    ;;
esac

