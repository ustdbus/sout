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

  local cur_ver=""
  if command -v sout-server >/dev/null 2>&1; then
    cur_ver=$(sout-server -version 2>/dev/null | awk '{print $2}' | tr -d ' \r\n')
  fi
  if [[ -z "$cur_ver" && -f "${WORK_DIR}/version" ]]; then
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
      echo -e "  监听地址:    ${Y}127.0.0.1 (仅内网，用于反代)${N}"
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
  echo "  1) 127.0.0.1 (仅内网，用于反代)"
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
    cur.execute("DELETE FROM clients WHERE name LIKE 'sout-%' OR name LIKE 'fanout-%'")

    # 5. 清理 API Token
    cur.execute("DELETE FROM tokens WHERE desc='sout' OR desc='fanout'")

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

  # 1. 彻底清理 s-ui 中由 sout 创建的出入站、分流路由、clients 与注入的 API Token
  cleanup_sui

  # 2. 彻底清理 Caddy 与 cloudflared 隧道及相关数据
  systemctl stop caddy 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || true
  systemctl stop cloudflared 2>/dev/null || true
  systemctl disable cloudflared 2>/dev/null || true
  rm -f /etc/systemd/system/caddy.service /etc/systemd/system/cloudflared.service 2>/dev/null || true
  rm -f /etc/init.d/caddy /etc/init.d/cloudflared 2>/dev/null || true
  rm -rf /etc/caddy /var/lib/caddy /var/log/caddy /usr/local/bin/caddy /usr/local/bin/cloudflared /usr/local/bin/sout-quick-tunnel /var/log/cloudflared* /home/acme 2>/dev/null || true
  rm -rf /root/.local/share/caddy /root/.config/caddy /root/.cache/caddy /root/.cloudflared 2>/dev/null || true

  # 3. 恢复 s-ui 监听与配置 (优先从备份还原，若端口被占用则自动随机空闲端口)
  local public_ip
  public_ip=$(curl -s4m 2 https://api.ipify.org 2>/dev/null || curl -s4m 2 https://icanhazip.com 2>/dev/null || curl -s4m 2 https://ifconfig.me 2>/dev/null || true)
  public_ip=$(echo "$public_ip" | tr -d ' \r\n')
  [[ -z "$public_ip" ]] && public_ip="服务器公网IP"

  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_backup="${WORK_DIR}/sui_backup.json"
  local final_wp="8443" final_wpath="/app/" final_sp="8444" final_spath="/sub/"
  if [[ -f "$sui_db" ]]; then
    local restore_info
    restore_info=$(python3 -c "
import sqlite3, json, os, socket

db = '$sui_db'
backup_file = '$sui_backup'
pub_ip = '$public_ip'

def is_port_free(port):
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(0.5)
        res = s.connect_ex(('127.0.0.1', int(port)))
        s.close()
        return res != 0
    except:
        return True

def get_free_port(start=8443):
    import random
    if is_port_free(start):
        return start
    for _ in range(50):
        p = random.randint(10000, 60000)
        if is_port_free(p):
            return p
    return start

con = sqlite3.connect(db)
cur = con.cursor()

b = {}
if os.path.exists(backup_file):
    try:
        with open(backup_file) as f:
            b = json.load(f)
    except:
        pass

# 1. 恢复 webPort / webPath
orig_wp = b.get('webPort', '8443')
final_wp = orig_wp if is_port_free(orig_wp) else get_free_port(8443)
orig_wpath = b.get('webPath', '/app/')
if not orig_wpath.startswith('/'): orig_wpath = '/' + orig_wpath
if not orig_wpath.endswith('/'): orig_wpath += '/'

# 2. 恢复 subPort / subPath
orig_sp = b.get('subPort', '8444')
final_sp = orig_sp if is_port_free(orig_sp) else get_free_port(8444)
orig_spath = b.get('subPath', '/sub/')
if not orig_spath.startswith('/'): orig_spath = '/' + orig_spath
if not orig_spath.endswith('/'): orig_spath += '/'

    # 3. 恢复证书配置 (如果有)
    orig_wcert = b.get('webCertFile', '')
    orig_wkey = b.get('webKeyFile', '')
    orig_scert = b.get('subCertFile', '')
    orig_skey = b.get('subKeyFile', '')
    orig_wdom = b.get('webDomain', '')
    orig_sdom = b.get('subDomain', '')
    orig_supd = b.get('subUpdates', '12')

    cur.execute('UPDATE settings SET value=? WHERE key="webCertFile"', (orig_wcert,))
    cur.execute('UPDATE settings SET value=? WHERE key="webKeyFile"', (orig_wkey,))
    cur.execute('UPDATE settings SET value=? WHERE key="subCertFile"', (orig_scert,))
    cur.execute('UPDATE settings SET value=? WHERE key="subKeyFile"', (orig_skey,))
    cur.execute('UPDATE settings SET value=? WHERE key="webDomain"', (orig_wdom,))
    cur.execute('UPDATE settings SET value=? WHERE key="subDomain"', (orig_sdom,))
    cur.execute('UPDATE settings SET value=? WHERE key="subUpdates"', (orig_supd,))

    # 恢复其他高级订阅项
    for ext_k in ['subEncode', 'subShowInfo', 'subClashExt', 'subJsonExt', 'subClashSprtAll', 'subClashNoDefGrp']:
        if ext_k in b:
            cur.execute('UPDATE settings SET value=? WHERE key=?', (str(b[ext_k]), ext_k))

    proto = 'https' if (orig_wcert and orig_wkey) else 'http'
    sub_proto = 'https' if (orig_scert and orig_skey) else 'http'

    host_web = orig_wdom if orig_wdom else f'{pub_ip}:{final_wp}'
    host_sub = orig_sdom if orig_sdom else f'{pub_ip}:{final_sp}'

    # 4. 恢复监听地址为 0.0.0.0 (空字符串) 并更新公网直连 URI
    cur.execute('UPDATE settings SET value=? WHERE key="webPort"', (str(final_wp),))
    cur.execute('UPDATE settings SET value="" WHERE key="webListen"')
    cur.execute('UPDATE settings SET value=? WHERE key="webPath"', (orig_wpath,))
    cur.execute('UPDATE settings SET value=? WHERE key="webURI"', (f'{proto}://{host_web}{orig_wpath}',))

    cur.execute('UPDATE settings SET value=? WHERE key="subPort"', (str(final_sp),))
    cur.execute('UPDATE settings SET value="" WHERE key="subListen"')
    cur.execute('UPDATE settings SET value=? WHERE key="subPath"', (orig_spath,))
    cur.execute('UPDATE settings SET value=? WHERE key="subURI"', (f'{sub_proto}://{host_sub}{orig_spath}',))

    con.commit()
    con.close()

    print(f'{final_wp}|{orig_wpath}|{final_sp}|{orig_spath}|{proto}|{sub_proto}|{orig_wdom}|{orig_sdom}')
" 2>/dev/null || echo "8443|/app/|8444|/sub/|http|http||")

    final_wp=$(echo "$restore_info" | cut -d'|' -f1)
    final_wpath=$(echo "$restore_info" | cut -d'|' -f2)
    final_sp=$(echo "$restore_info" | cut -d'|' -f3)
    final_spath=$(echo "$restore_info" | cut -d'|' -f4)
    local final_proto final_sub_proto final_wdom final_sdom
    final_proto=$(echo "$restore_info" | cut -d'|' -f5)
    final_sub_proto=$(echo "$restore_info" | cut -d'|' -f6)
    final_wdom=$(echo "$restore_info" | cut -d'|' -f7)
    final_sdom=$(echo "$restore_info" | cut -d'|' -f8)
    [[ -z "$final_proto" ]] && final_proto="http"
    [[ -z "$final_sub_proto" ]] && final_sub_proto="http"
    local show_web_host show_sub_host
    show_web_host="$([[ -n "$final_wdom" ]] && echo "$final_wdom" || echo "${public_ip}:${final_wp}")"
    show_sub_host="$([[ -n "$final_sdom" ]] && echo "$final_sdom" || echo "${public_ip}:${final_sp}")"
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

  systemctl daemon-reload 2>/dev/null || true
  systemctl reset-failed 2>/dev/null || true
  svc_reload

  echo
  echo -e "${G}================================================================${N}"
  echo -e "${G}  🎉 sout 插件、Caddy 及 Cloudflare 隧道已彻底清理干净！${N}"
  echo -e "${G}  🎉 s-ui 面板已完全恢复公网 0.0.0.0 直连模式 (已还原证书与配置)${N}"
  echo -e "${G}================================================================${N}"
  echo -e "  [1] s-ui 管理面板:  ${B}${final_proto}://${show_web_host}${final_wpath}${N}"
  echo -e "  [2] s-ui 唤起命令:  ${G}s-ui${N}"
  echo -e "${G}================================================================${N}"
  echo
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

  echo "$tag_name" > "${WORK_DIR}/version" 2>/dev/null || true

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

  # 4. 自动化配置 s-ui (配置前先做持久化备份)
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_admin_user
  sui_admin_user=$(get_sui_user)
  local sui_backup="${WORK_DIR}/sui_backup.json"

  if [[ -f "$sui_db" && ! -f "$sui_backup" ]]; then
    python3 -c "
import sqlite3, json
try:
    con = sqlite3.connect('$sui_db')
    cur = con.cursor()
    keys = [
        'webPort', 'webListen', 'webPath', 'webDomain', 'webCertFile', 'webKeyFile', 'webURI',
        'subPort', 'subListen', 'subPath', 'subDomain', 'subCertFile', 'subKeyFile', 'subURI', 'subUpdates',
        'subEncode', 'subShowInfo', 'subClashExt', 'subJsonExt', 'subClashSprtAll', 'subClashNoDefGrp'
    ]
    backup = {}
    for k in keys:
        cur.execute('SELECT value FROM settings WHERE key=?', (k,))
        row = cur.fetchone()
        if row and row[0] is not None:
            backup[k] = row[0]
    con.close()
    with open('$sui_backup', 'w') as f:
        json.dump(backup, f, indent=2)
except Exception:
    pass
" 2>/dev/null || true
  fi

  if [[ -f "$sui_db" ]]; then
    local sui_token
    sui_token=$(cat "${WORK_DIR}/sui-token" 2>/dev/null || true)
    if [[ -z "$sui_token" ]]; then
      sui_token=$(sqlite3 "$sui_db" "SELECT token FROM tokens WHERE (desc='sout' OR desc='fanout') AND (expiry=0 OR expiry > strftime('%s','now')) LIMIT 1;" 2>/dev/null || true)
      if [[ -z "$sui_token" ]]; then
        sui_token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
        sqlite3 "$sui_db" "INSERT INTO tokens (desc, token, expiry, user_id) VALUES ('sout', '$sui_token', 0, 1);" 2>/dev/null || true
        systemctl restart s-ui 2>/dev/null || true
      fi
      mkdir -p "$WORK_DIR"
      echo "$sui_token" > "${WORK_DIR}/sui-token"
      chmod 600 "${WORK_DIR}/sui-token" 2>/dev/null || true
    fi

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

  # 恢复 s-ui 监听与配置 (优先从备份还原，若端口被占用则自动随机空闲端口)
  local public_ip
  public_ip=$(curl -s4m 2 https://api.ipify.org 2>/dev/null || curl -s4m 2 https://icanhazip.com 2>/dev/null || curl -s4m 2 https://ifconfig.me 2>/dev/null || true)
  public_ip=$(echo "$public_ip" | tr -d ' \r\n')
  [[ -z "$public_ip" ]] && public_ip="服务器公网IP"

  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_backup="${WORK_DIR}/sui_backup.json"
  local final_wp="8443" final_wpath="/app/" final_sp="8444" final_spath="/sub/"
  if [[ -f "$sui_db" ]]; then
    local restore_info
    restore_info=$(python3 -c "
import sqlite3, json, os, socket

db = '$sui_db'
backup_file = '$sui_backup'
pub_ip = '$public_ip'

def is_port_free(port):
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(0.5)
        res = s.connect_ex(('127.0.0.1', int(port)))
        s.close()
        return res != 0
    except:
        return True

def get_free_port(start=8443):
    import random
    if is_port_free(start):
        return start
    for _ in range(50):
        p = random.randint(10000, 60000)
        if is_port_free(p):
            return p
    return start

con = sqlite3.connect(db)
cur = con.cursor()

b = {}
if os.path.exists(backup_file):
    try:
        with open(backup_file) as f:
            b = json.load(f)
    except:
        pass

# 1. 恢复 webPort / webPath
orig_wp = b.get('webPort', '8443')
final_wp = orig_wp if is_port_free(orig_wp) else get_free_port(8443)
orig_wpath = b.get('webPath', '/app/')
if not orig_wpath.startswith('/'): orig_wpath = '/' + orig_wpath
if not orig_wpath.endswith('/'): orig_wpath += '/'

# 2. 恢复 subPort / subPath
orig_sp = b.get('subPort', '8444')
final_sp = orig_sp if is_port_free(orig_sp) else get_free_port(8444)
orig_spath = b.get('subPath', '/sub/')
if not orig_spath.startswith('/'): orig_spath = '/' + orig_spath
if not orig_spath.endswith('/'): orig_spath += '/'

    # 3. 恢复证书配置 (如果有)
    orig_wcert = b.get('webCertFile', '')
    orig_wkey = b.get('webKeyFile', '')
    orig_scert = b.get('subCertFile', '')
    orig_skey = b.get('subKeyFile', '')
    orig_wdom = b.get('webDomain', '')
    orig_sdom = b.get('subDomain', '')
    orig_supd = b.get('subUpdates', '12')

    cur.execute('UPDATE settings SET value=? WHERE key=\"webCertFile\"', (orig_wcert,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"webKeyFile\"', (orig_wkey,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"subCertFile\"', (orig_scert,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"subKeyFile\"', (orig_skey,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"webDomain\"', (orig_wdom,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"subDomain\"', (orig_sdom,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"subUpdates\"', (orig_supd,))

    # 恢复其他高级订阅项
    for ext_k in ['subEncode', 'subShowInfo', 'subClashExt', 'subJsonExt', 'subClashSprtAll', 'subClashNoDefGrp']:
        if ext_k in b:
            cur.execute('UPDATE settings SET value=? WHERE key=?', (str(b[ext_k]), ext_k))

    proto = 'https' if (orig_wcert and orig_wkey) else 'http'
    sub_proto = 'https' if (orig_scert and orig_skey) else 'http'

    host_web = orig_wdom if orig_wdom else f'{pub_ip}:{final_wp}'
    host_sub = orig_sdom if orig_sdom else f'{pub_ip}:{final_sp}'

    # 4. 恢复监听地址为 0.0.0.0 (空字符串) 并更新公网直连 URI
    cur.execute('UPDATE settings SET value=? WHERE key=\"webPort\"', (str(final_wp),))
    cur.execute('UPDATE settings SET value=\"\" WHERE key=\"webListen\"')
    cur.execute('UPDATE settings SET value=? WHERE key=\"webPath\"', (orig_wpath,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"webURI\"', (f'{proto}://{host_web}{orig_wpath}',))

    cur.execute('UPDATE settings SET value=? WHERE key=\"subPort\"', (str(final_sp),))
    cur.execute('UPDATE settings SET value=\"\" WHERE key=\"subListen\"')
    cur.execute('UPDATE settings SET value=? WHERE key=\"subPath\"', (orig_spath,))
    cur.execute('UPDATE settings SET value=? WHERE key=\"subURI\"', (f'{sub_proto}://{host_sub}{orig_spath}',))

    con.commit()
    con.close()

    print(f'{final_wp}|{orig_wpath}|{final_sp}|{orig_spath}|{proto}|{sub_proto}|{orig_wdom}|{orig_sdom}')
" 2>/dev/null || echo "8443|/app/|8444|/sub/|http|http||")

    final_wp=$(echo "$restore_info" | cut -d'|' -f1)
    final_wpath=$(echo "$restore_info" | cut -d'|' -f2)
    final_sp=$(echo "$restore_info" | cut -d'|' -f3)
    final_spath=$(echo "$restore_info" | cut -d'|' -f4)
    local final_proto final_sub_proto final_wdom final_sdom
    final_proto=$(echo "$restore_info" | cut -d'|' -f5)
    final_sub_proto=$(echo "$restore_info" | cut -d'|' -f6)
    final_wdom=$(echo "$restore_info" | cut -d'|' -f7)
    final_sdom=$(echo "$restore_info" | cut -d'|' -f8)
    [[ -z "$final_proto" ]] && final_proto="http"
    [[ -z "$final_sub_proto" ]] && final_sub_proto="http"
    local show_web_host show_sub_host
    show_web_host="$([[ -n "$final_wdom" ]] && echo "$final_wdom" || echo "${public_ip}:${final_wp}")"
    show_sub_host="$([[ -n "$final_sdom" ]] && echo "$final_sdom" || echo "${public_ip}:${final_sp}")"
    systemctl restart s-ui 2>/dev/null || true
  fi

  systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || true
  echo -e "  ${G}[✓] 已成功关闭隧道反代，所有服务已恢复公网 0.0.0.0 直连模式 (已还原证书与配置)：${N}"
  echo -e "      • sout 管理面板: ${B}http://${public_ip}:8899/$(cat "${WORK_DIR}/basepath" 2>/dev/null || echo "")/${N}"
  echo -e "      • s-ui 管理面板: ${B}${final_proto}://${show_web_host}${final_wpath}${N}"
  echo -e "      • s-ui 订阅地址: ${B}${final_sub_proto}://${show_sub_host}${final_spath}${N}"
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

  # 2. 动态探测并自动纠偏 s-ui 面板配置 (确保监听 127.0.0.1 并更新 webURI/subURI)
  sui_port="2096"
  sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  [[ -z "$sui_p" ]] && sui_p="sui"
  sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  [[ -z "$sub_p" ]] && sub_p="sub"

  local sui_needs_restart=0
  if [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
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

  # 3. 动态识别/创建 s-ui 隧道节点入站 (核心：识别监听在 127.0.0.1 的隧道节点并同步更新域名SNI)
  ws_p=$(grep -oE '"ws_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  node_port=$(grep -oE '"node_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
  [[ -z "$node_port" ]] && node_port="2082"

  if [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
    local node_result
    node_result=$(python3 -c "
import sqlite3, json

db_path = '/usr/local/s-ui/db/s-ui.db'
con = sqlite3.connect(db_path)
cur = con.cursor()
cur.execute(\"SELECT id, listen_port, listen, transport, tag, type, addrs FROM inbounds\")
rows = cur.fetchall()

found_port = ''
found_path = ''

# 遍历寻找 listen 为 127.0.0.1 且包含 ws 传输协议的隧道节点，并更新域名SNI
for r in rows:
    ib_id, p, listen_ip, tr_str, tag, typ, addrs_str = r[0], r[1], r[2], r[3], r[4], r[5], r[6]
    if listen_ip == '127.0.0.1':
        try:
            tr = json.loads(tr_str) if tr_str else {}
        except:
            tr = {}
        if isinstance(tr, dict) and tr.get('type') == 'ws' and tr.get('path'):
            found_port = str(p)
            found_path = tr.get('path').strip('/')
            # 同步更新节点域名与SNI
            try:
                addrs = json.loads(addrs_str) if addrs_str else []
                if isinstance(addrs, list) and len(addrs) > 0:
                    addrs[0]['server'] = '${domain}'
                    if 'tls' in addrs[0] and isinstance(addrs[0]['tls'], dict):
                        addrs[0]['tls']['server_name'] = '${domain}'
                    cur.execute(\"UPDATE inbounds SET addrs=? WHERE id=?\", (json.dumps(addrs), ib_id))
                    con.commit()
            except Exception:
                pass
            break
        elif tag == 'vmess-argo' or typ in ('vmess', 'vless'):
            found_port = str(p)
            if isinstance(tr, dict) and tr.get('path'):
                found_path = tr.get('path').strip('/')

con.close()
if found_port and found_path:
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
      if [[ -z "$sui_token" ]]; then
        sui_token=$(sqlite3 "/usr/local/s-ui/db/s-ui.db" "SELECT token FROM tokens WHERE (desc='sout' OR desc='fanout') AND (expiry=0 OR expiry > strftime('%s','now')) LIMIT 1;" 2>/dev/null || true)
        if [[ -z "$sui_token" ]]; then
          sui_token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
          sqlite3 "/usr/local/s-ui/db/s-ui.db" "INSERT INTO tokens (desc, token, expiry, user_id) VALUES ('sout', '$sui_token', 0, 1);" 2>/dev/null || true
          systemctl restart s-ui 2>/dev/null || true
        fi
        mkdir -p "$WORK_DIR"
        echo "$sui_token" > "${WORK_DIR}/sui-token"
        chmod 600 "${WORK_DIR}/sui-token" 2>/dev/null || true
      fi
      local sui_admin_user
      sui_admin_user=$(get_sui_user)
      if [[ -z "$ws_p" ]]; then
        ws_p=$(rand_safe_path "vlws")
      fi
      node_port=$(rand_local_port)
      
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
    node_tag = 'vmess-argo'
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
        'id': 0,
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
        'action': 'new',
        'data': json.dumps(inbound_payload),
    })
except Exception:
    pass
PYEOF
      systemctl restart s-ui 2>/dev/null || true
      echo -e "  ${G}[✓] 127.0.0.1 隧道节点创建完成 (端口: ${node_port}, 路径: /${ws_p}/)${N}"
    fi
  fi

  # 4. 重新生成纯净 Caddyfile (精确绑定当前检测到的隧道回源端口)
  mkdir -p /etc/caddy
  cat > /etc/caddy/Caddyfile <<EOF
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
d['domain'] = '${domain}'
d['tunnel_port'] = int('${tunnel_port}')
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
    echo
    echo -e "${B}========================================${N}"
    echo -e "${B}  最新 Caddy 流量反代与分流详情${N}"
    echo -e "${B}========================================${N}"
    echo -e "  Caddy 正在监听:    ${Y}127.0.0.1:${tunnel_port}${N}"
    echo
    echo -e "  ${G}• Caddy 将 /${sout_p}/ 路径流量转发至:   127.0.0.1:${sout_port} (sout 管理面板)${N}"
    echo -e "    外网访问: https://${domain}/${sout_p}/"
    echo
    echo -e "  ${G}• Caddy 将 /${sui_p}/ 路径流量转发至:    127.0.0.1:${sui_port} (s-ui 面板)${N}"
    echo -e "    外网访问: https://${domain}/${sui_p}/"
    echo
    echo -e "  ${G}• Caddy 将 /${sout_p}/sub 路径流量转发至: 127.0.0.1:${sout_port} (订阅接口)${N}"
    echo -e "    订阅链接: https://${domain}/${sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
    if [[ -n "$ws_p" ]]; then
      echo
      echo -e "  ${G}• Caddy 将 /${ws_p}/ 路径流量转发至:     127.0.0.1:${node_port} (节点流量)${N}"
    fi
    echo
    echo -e "  ${G}• Caddy 将 / 根路径流量响应:            200 OK (伪装服务就绪)${N}"
    echo -e "${B}========================================${N}"
  else
    echo -e "  ${R}[×] Caddy 重启失败，请检查 Caddyfile 或端口占用${N}"
  fi
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
      echo "  1) 查看 Caddy 反代信息"
      echo "  2) 重新配置隧道与域名 (修改 Token/域名/端口)"
      echo "  3) 查看 cloudflared 隧道运行日志"
      echo "  4) 重启 Cloudflare 隧道"
      echo "  5) 启用/重启 Caddy 服务 (重新探测并分流)"
      echo "  6) 关闭隧道反代 (恢复独立端口模式)"
      echo "  0) 返回上级菜单"
      echo
      read -rp "  请选择 [0-6]: " opt
      case "$opt" in
        1)
          if [[ -f "$CADDY_META" ]]; then
            local sout_p sui_p sub_p ws_p sout_port sui_port node_port
            sout_p=$(grep -oE '"sout_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            ws_p=$(grep -oE '"ws_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" | cut -d'"' -f4)
            sout_port=$(grep -oE '"sout_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
            sui_port=$(grep -oE '"sui_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
            node_port=$(grep -oE '"node_port"[[:space:]]*:[[:space:]]*[0-9]+' "$CADDY_META" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
            [[ -z "$sout_port" ]] && sout_port="8899"
            [[ -z "$sui_port" ]] && sui_port="2096"
            [[ -z "$node_port" ]] && node_port="2082"

            echo
            echo -e "${B}========================================${N}"
            echo -e "${B}  Caddy 流量反代与转发详情${N}"
            echo -e "${B}========================================${N}"
            echo -e "  Caddy 正在监听:    ${Y}127.0.0.1:${tun_p}${N}"
            echo
            echo -e "  ${G}• Caddy 将 /${sout_p}/ 路径流量转发至:   127.0.0.1:${sout_port} (sout 管理面板)${N}"
            echo -e "    外网访问: https://${dom}/${sout_p}/"
            echo
            echo -e "  ${G}• Caddy 将 /${sui_p}/ 路径流量转发至:    127.0.0.1:${sui_port} (s-ui 面板)${N}"
            echo -e "    外网访问: https://${dom}/${sui_p}/"
            echo
            echo -e "  ${G}• Caddy 将 /${sout_p}/sub 路径流量转发至: 127.0.0.1:${sout_port} (订阅接口)${N}"
            echo -e "    订阅链接: https://${dom}/${sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
            if [[ -n "$ws_p" ]]; then
              echo
              echo -e "  ${G}• Caddy 将 /${ws_p}/ 路径流量转发至:     127.0.0.1:${node_port} (节点流量)${N}"
            fi
            echo
            echo -e "  ${G}• Caddy 将 / 根路径流量响应:            200 OK (伪装服务就绪)${N}"
            echo -e "${B}========================================${N}"
          fi
          pause ;;
        2) caddy_interactive_setup; pause ;;
        3) echo; journalctl -u cloudflared -n 40 --no-pager; pause ;;
        4)
          echo -e "  正在重启 Cloudflare 隧道服务..."
          systemctl restart cloudflared 2>/dev/null && echo -e "  ${G}[✓] Cloudflare 隧道服务已成功重启${N}" || echo -e "  ${R}[×] 隧道服务重启失败${N}"
          pause ;;
        5)
          reload_caddy_proxy
          pause ;;
        6) disable_caddy_proxy; pause; break ;;
        0) break ;;
        *) ;;
      esac
    else
      echo -e "  反代状态:      ${D}未开启 (当前为独立多端口模式)${N}"
      echo -e "  💡 提示:       ${Y}强烈推荐开启 Cloudflare隧道连接和Caddy流量代理 (免开端口/杜绝525)${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 开启 Cloudflare 官方免费临时隧道 (免域名/免Token)"
      echo "  2) 使用固定域名 + 隧道 Token 配置"
      echo "  3) 启用/重启 Caddy 服务 (重新探测并分流)"
      echo "  0) 返回上级菜单"
      echo
      read -rp "  请选择 [0-3]: " opt
      case "$opt" in
        1) setup_caddy_proxy "" "" ""; pause ;;
        2) caddy_interactive_setup; pause ;;
        3) reload_caddy_proxy; pause ;;
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
    echo -e "   5) 重置访问口令      6) 重置访问路径"
    echo -e "   7) 面板 URL 设置     8) SSL / HTTPS 设置"
    echo -e "   9) 修改面板监听地址和端口"
    echo -e "  10) Cloudflare隧道/Caddy配置"
    echo -e "  11) 检查/更新版本    12) 卸载"
    echo -e "   0) 退出脚本"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-12]: " choice

    case "$choice" in
      1) svc_start   && echo -e "\n  ${G}已启动${N}"; pause ;;
      2) svc_stop    && echo -e "\n  ${Y}已停止${N}"; pause ;;
      3) svc_restart && echo -e "\n  ${G}已重启${N}"; pause ;;
      4) echo; svc_logs 40; pause ;;
      5) reset_password; pause ;;
      6) reset_basepath; pause ;;
      7) change_panel_url; pause ;;
      8) change_ssl; pause ;;
      9) change_listen_and_port; pause ;;
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

