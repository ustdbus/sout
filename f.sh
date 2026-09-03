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
  if [[ "$INIT_SYS" == "systemd" ]]; then
    command systemctl "$@"
    return $?
  fi

  local action="$1"
  shift
  # 过滤常用的 systemctl 参数
  while [[ $# -gt 0 && "$1" == -* ]]; do
    shift
  done
  local name="${1:-}"
  [[ -n "$name" ]] && name="${name%.service}"

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

# ==============================================================================
# 系统内核与网络缓冲区参数自动调优 / 备份 / 还原
# ==============================================================================
SYSCTL_BACKUP="${WORK_DIR}/sysctl_backup.conf"

apply_sysctl_optimization() {
  mkdir -p "$WORK_DIR" /etc/sysctl.d 2>/dev/null || true

  # 1. 备份原系统参数（仅首次备份，避免覆盖原始值）
  if [[ ! -f "$SYSCTL_BACKUP" ]]; then
    local keys=(
      "net.core.rmem_max"
      "net.core.wmem_max"
      "net.core.rmem_default"
      "net.core.wmem_default"
      "net.core.netdev_max_backlog"
      "net.core.somaxconn"
      "net.ipv4.udp_mem"
      "net.ipv4.udp_rmem_min"
      "net.ipv4.udp_wmem_min"
      "net.core.default_qdisc"
      "net.ipv4.tcp_congestion_control"
    )
    : > "$SYSCTL_BACKUP"
    for k in "${keys[@]}"; do
      local val
      val=$(sysctl -n "$k" 2>/dev/null || true)
      if [[ -n "$val" ]]; then
        echo "${k}=${val}" >> "$SYSCTL_BACKUP"
      fi
    done
  fi

  # 2. 根据内存大小动态选择缓冲区
  local mem_total_mb=512
  if [[ -f /proc/meminfo ]]; then
    local mem_kb
    mem_kb=$(grep -i 'MemTotal' /proc/meminfo | awk '{print $2}')
    [[ -n "$mem_kb" && "$mem_kb" -gt 0 ]] && mem_total_mb=$(( mem_kb / 1024 ))
  fi

  local rmem_max=4194304
  local wmem_max=4194304
  local udp_mem="4096 8192 16384"
  if [[ $mem_total_mb -gt 512 ]]; then
    rmem_max=8388608
    wmem_max=8388608
    udp_mem="8192 16384 32768"
  fi

  # 3. 尝试加载 BBR 模块
  modprobe tcp_bbr >/dev/null 2>&1 || true

  # 4. 写入独立 sysctl 配置文件与 /etc/sysctl.conf
  local conf_content="# === SOUT SYSCTL START ===
net.core.rmem_max = ${rmem_max}
net.core.wmem_max = ${wmem_max}
net.core.rmem_default = 262144
net.core.wmem_default = 262144
net.core.netdev_max_backlog = 2500
net.core.somaxconn = 4096
net.ipv4.udp_mem = ${udp_mem}
net.ipv4.udp_rmem_min = 8192
net.ipv4.udp_wmem_min = 8192
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
# === SOUT SYSCTL END ==="

  echo "$conf_content" > /etc/sysctl.d/99-sout.conf 2>/dev/null || true

  if [[ -f /etc/sysctl.conf ]]; then
    sed -i '/# === SOUT SYSCTL START ===/,/# === SOUT SYSCTL END ===/d' /etc/sysctl.conf 2>/dev/null || true
    echo "$conf_content" >> /etc/sysctl.conf 2>/dev/null || true
  fi

  # 5. 生效参数（容器无权修改时静默忽略）
  sysctl -p /etc/sysctl.d/99-sout.conf >/dev/null 2>&1 || sysctl -p >/dev/null 2>&1 || true
}

get_tcp_congestion() {
  local cc
  cc=$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null || cat /proc/sys/net/ipv4/tcp_congestion_control 2>/dev/null || echo "cubic")
  cc=$(echo "$cc" | tr -d ' \r\n')
  [[ -z "$cc" ]] && cc="cubic"
  echo "$cc"
}

restore_sysctl() {
  if [[ -f "$SYSCTL_BACKUP" ]]; then
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
      [[ -z "$key" || "$key" =~ ^# ]] && continue
      sysctl -w "${key}=${val}" >/dev/null 2>&1 || true
    done < "$SYSCTL_BACKUP"
    rm -f "$SYSCTL_BACKUP" 2>/dev/null || true
  fi

  rm -f /etc/sysctl.d/99-sout.conf 2>/dev/null || true

  if [[ -f /etc/sysctl.conf ]]; then
    sed -i '/# === SOUT SYSCTL START ===/,/# === SOUT SYSCTL END ===/d' /etc/sysctl.conf 2>/dev/null || true
    sysctl -p >/dev/null 2>&1 || true
  fi
}

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
svc_start()      { apply_sysctl_optimization; systemctl start "$UNIT"; }
svc_stop()       { systemctl stop "$UNIT"; }
svc_restart()    { apply_sysctl_optimization; systemctl restart "$UNIT"; }
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

get_or_create_sui_token() {
  local sui_db="${1:-/usr/local/s-ui/db/s-ui.db}"
  [[ ! -f "$sui_db" ]] && return 1

  local tok=""
  tok=$(cat "${WORK_DIR}/sui-token" 2>/dev/null || true)

  # 1. 若本地持久化文件中有 token，核查数据库中是否为未过期的有效令牌
  if [[ -n "$tok" ]]; then
    local in_db
    in_db=$(sqlite3 "$sui_db" "SELECT 1 FROM tokens WHERE token='$tok' AND (desc='sout' OR desc='fanout') AND (expiry=0 OR expiry > strftime('%s','now')) LIMIT 1;" 2>/dev/null || true)
    if [[ "$in_db" == "1" ]]; then
      # 存在有效令牌，顺便清理多余重复同名令牌，保持数据库纯净
      sqlite3 "$sui_db" "DELETE FROM tokens WHERE (desc='sout' OR desc='fanout') AND token != '$tok';" 2>/dev/null || true
      echo "$tok"
      return 0
    fi
  fi

  # 2. 本地文件无有效 token，则从数据库中提取最新的有效 sout 令牌复用（按 id DESC 倒序，优先最新的）
  tok=$(sqlite3 "$sui_db" "SELECT token FROM tokens WHERE (desc='sout' OR desc='fanout') AND (expiry=0 OR expiry > strftime('%s','now')) ORDER BY id DESC LIMIT 1;" 2>/dev/null || true)
  if [[ -n "$tok" ]]; then
    # 存在有效令牌，顺便清理其他多余重复历史令牌
    sqlite3 "$sui_db" "DELETE FROM tokens WHERE (desc='sout' OR desc='fanout') AND token != '$tok';" 2>/dev/null || true
    mkdir -p "$WORK_DIR"
    echo "$tok" > "${WORK_DIR}/sui-token"
    chmod 600 "${WORK_DIR}/sui-token" 2>/dev/null || true
    echo "$tok"
    return 0
  fi

  # 3. 现存所有令牌均失效或不存在，先清理所有旧的残留记录，再插入唯一的新令牌
  sqlite3 "$sui_db" "DELETE FROM tokens WHERE desc='sout' OR desc='fanout';" 2>/dev/null || true
  tok=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  local admin_id
  admin_id=$(sqlite3 "$sui_db" "SELECT id FROM users LIMIT 1;" 2>/dev/null || echo "1")
  [[ -z "$admin_id" ]] && admin_id="1"
  sqlite3 "$sui_db" "INSERT INTO tokens (desc, token, expiry, user_id) VALUES ('sout', '$tok', 0, $admin_id);" 2>/dev/null || true
  systemctl restart s-ui 2>/dev/null || true
  mkdir -p "$WORK_DIR"
  echo "$tok" > "${WORK_DIR}/sui-token"
  chmod 600 "${WORK_DIR}/sui-token" 2>/dev/null || true
  echo "$tok"
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

  # 还原内核与网络系统参数备份
  restore_sysctl

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

  # 还原内核与网络系统参数备份
  restore_sysctl

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

  echo
  echo -e "  ${B}[+] 正在重启服务并加载最新版本配置...${N}"
  # 1. 重启 sout 服务
  svc_restart

  # 2. 重启 s-ui 面板服务
  systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || service s-ui restart 2>/dev/null || true

  # 3. 若开启了 Cloudflare 隧道和 Caddy 代理，联动重启隧道并执行 Caddy 重新探测分流
  if [[ -f "$CADDY_META" ]] && grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META" 2>/dev/null; then
    echo -e "  ${B}[+] 检测到已开启 Cloudflare 隧道反代，正在自动重启隧道服务...${N}"
    systemctl restart cloudflared 2>/dev/null || rc-service cloudflared restart 2>/dev/null || service cloudflared restart 2>/dev/null || true
    echo -e "  ${B}[+] 正在自动执行 Caddy 重新探测并分流...${N}"
    reload_caddy_proxy
  fi

  echo
  echo -e "  ${G}🎉 恭喜！sout 已成功更新至 ${tag_name}，所有关联服务已自动重启生效。${N}"
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
ExecStart=/usr/local/bin/cloudflared tunnel --protocol quic --no-autoupdate run --token ${token}
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
  systemctl daemon-reload 2>/dev/null || true
  systemctl enable cloudflared >/dev/null 2>&1 || rc-update add cloudflared default >/dev/null 2>&1 || true
  systemctl restart cloudflared 2>/dev/null || rc-service cloudflared restart 2>/dev/null || service cloudflared restart 2>/dev/null || true
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
  local apply_cert="${4:-n}"
  local cf_dns_key="${5:-}"

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
  if [[ "$apply_cert" == "y" && -n "$domain" && -n "$cf_dns_key" ]]; then
    ensure_caddy_with_cloudflare || install_caddy_bin || { echo -e "  ${R}安装 Caddy 失败${N}"; return 1; }
  else
    install_caddy_bin || { echo -e "  ${R}安装 Caddy 失败${N}"; return 1; }
  fi
  install_cloudflared_bin || { echo -e "  ${R}安装 cloudflared 失败${N}"; return 1; }

  # 2. 分配本地端口与安全路径
  local sout_port sui_port sub_port node_port reality_port
  local sout_path sui_path sub_path ws_path

  sout_port=$(rand_local_port)
  sui_port=$(rand_local_port)
  sub_port=$(rand_local_port)
  node_port=$(rand_local_port)
  reality_port=$(rand_local_port)

  sout_path=$(rand_safe_path "sout")
  sui_path=$(rand_safe_path "sui")
  sub_path=$(rand_safe_path "sub")
  ws_path=$(rand_safe_path "vlws")

  local public_ip cur_cc
  public_ip=$(curl -s4m 5 https://api.ipify.org 2>/dev/null || curl -s4m 5 https://ifconfig.me 2>/dev/null || echo "$domain")
  cur_cc=$(get_tcp_congestion)

  echo -e "  [+] 正在启动 Cloudflare 隧道服务..."
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
    sui_token=$(get_or_create_sui_token "$sui_db")

    if [[ -z "$sui_token" ]]; then
      echo -e "  ${Y}[!] 未找到 s-ui API Token，跳过自动配置（请先启动 sout 生成 Token）${N}"
    else
      echo -e "  [+] 正在通过 s-ui API 自动配置 (路径分流与动态分流节点)..."
      if ! SUI_API="http://127.0.0.1:${sui_port}/${sui_path}/apiv2" \
      SUI_TOKEN="$sui_token" \
      SUI_DB="$sui_db" \
      DOMAIN="$domain" \
      PUBLIC_IP="$public_ip" \
      SUI_PORT="$sui_port" \
      SUI_PATH="/${sui_path}/" \
      SUB_PORT="$sub_port" \
      SUB_PATH="/${sub_path}/" \
      NODE_PORT="$node_port" \
      REALITY_PORT="$reality_port" \
      TUIC_PORT="$tuic_port" \
      HY2_PORT="$hy2_port" \
      WS_PATH="/${ws_path}" \
      SUI_ADMIN_USER="$sui_admin_user" \
      APPLY_CERT="$apply_cert" \
      CONGESTION_CONTROL="$cur_cc" \
      python3 <<'PYEOF'
import json, os, sqlite3, uuid, urllib.request, urllib.parse, string, random, base64

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

# 2. 准备辅助函数与已有入站数据
def gen_rand_suffix():
    chars = string.ascii_lowercase + string.digits
    return "".join(random.choices(chars, k=4))

inbounds_resp = api('GET', 'inbounds') or {}
inbounds_obj = inbounds_resp.get('obj') or []
if isinstance(inbounds_obj, dict):
    inbound_rows = inbounds_obj.get('inbounds') or []
else:
    inbound_rows = inbounds_obj or []
inbound_rows = [r for r in inbound_rows if isinstance(r, dict)]

def get_or_create_tag_and_id(prefix):
    for row in inbound_rows:
        t = row.get('tag', '')
        if t == prefix or t.startswith(prefix + '-'):
            return t, row.get('id')
    return f"{prefix}-{gen_rand_suffix()}", None

def gen_x25519_keypair():
    try:
        from cryptography.hazmat.primitives.asymmetric import x25519
        k = x25519.X25519PrivateKey.generate()
        priv = base64.urlsafe_b64encode(k.private_bytes_raw()).decode().rstrip('=')
        pub = base64.urlsafe_b64encode(k.public_key().public_bytes_raw()).decode().rstrip('=')
        return priv, pub
    except Exception:
        P = 2**255 - 19
        A24 = 121665
        def cswap(swap, x_2, x_3):
            dummy = swap * ((x_2 - x_3) % P)
            return (x_2 - dummy) % P, (x_3 + dummy) % P
        def clamp(n):
            n &= ~7
            n &= ~(128 << 8 * 31)
            n |= 64 << 8 * 31
            return n
        raw_priv = os.urandom(32)
        k = clamp(int.from_bytes(raw_priv, 'little'))
        x_1 = 9; x_2 = 1; z_2 = 0; x_3 = x_1; z_3 = 1; swap = 0
        for t in reversed(range(255)):
            k_t = (k >> t) & 1
            swap ^= k_t
            x_2, x_3 = cswap(swap, x_2, x_3)
            z_2, z_3 = cswap(swap, z_2, z_3)
            swap = k_t
            A = (x_2 + z_2) % P; AA = (A * A) % P; B = (x_2 - z_2) % P; BB = (B * B) % P
            E = (AA - BB) % P; C = (x_3 + z_3) % P; D = (x_3 - z_3) % P
            DA = (D * A) % P; CB = (C * B) % P
            x_3 = ((DA + CB) ** 2) % P; z_3 = (x_1 * ((DA - CB) ** 2)) % P
            x_2 = (AA * BB) % P; z_2 = (E * ((AA + A24 * E) % P)) % P
        x_2, x_3 = cswap(swap, x_2, x_3); z_2, z_3 = cswap(swap, z_2, z_3)
        pub_int = (x_2 * pow(z_2, P - 2, P)) % P
        raw_pub = pub_int.to_bytes(32, 'little')
        priv_b64 = base64.urlsafe_b64encode(raw_priv).decode().rstrip('=')
        pub_b64 = base64.urlsafe_b64encode(raw_pub).decode().rstrip('=')
        return priv_b64, pub_b64

domain = os.environ['DOMAIN']
created_inbound_tags = []

# 固定启用 vmess-argo 与 vless-reality 基础节点
vmess_tag, vmess_id = get_or_create_tag_and_id('vmess-argo')
reality_tag, reality_id = get_or_create_tag_and_id('vless-reality')

vmess_payload = {
    'id': vmess_id or 0,
    'type': 'vmess',
    'tag': vmess_tag,
    'tls_id': 0,
    'listen': '127.0.0.1',
    'listen_port': int(os.environ['NODE_PORT']),
    'addrs': [{
        'server': domain,
        'server_port': 443,
        'tls': {
            'disable_sni': False,
            'enabled': True,
            'insecure': False,
            'server_name': domain,
            'utls': {'enabled': True, 'fingerprint': 'chrome'}
        }
    }],
    'transport': {
        'early_data_header_name': 'Sec-WebSocket-Protocol',
        'max_early_data': 2560,
        'headers': {'Host': domain},
        'path': os.environ['WS_PATH'],
        'type': 'ws'
    }
}
api('POST', 'save', {
    'object': 'inbounds',
    'action': 'edit' if vmess_id else 'new',
    'data': json.dumps(vmess_payload)
})
created_inbound_tags.append(vmess_tag)

# 先检查/创建 reality tls 对象
all_tls_resp = api('GET', 'tls') or {}
all_tls_obj = all_tls_resp.get('obj') or {}
all_tls = all_tls_obj.get('tls', []) if isinstance(all_tls_obj, dict) else (all_tls_obj or [])
reality_tid = None
for t in all_tls:
    if t.get('name') == 'reality' or 'reality' in t.get('server', {}):
        reality_tid = t.get('id')
        break

priv_k, pub_k = gen_x25519_keypair()
sid = os.urandom(4).hex()
if not reality_tid:
    reality_tls_payload = {
        'id': 0,
        'name': 'reality',
        'server': {
            'enabled': True,
            'server_name': 'apple.com',
            'reality': {
                'enabled': True,
                'handshake': {
                    'server_port': 443,
                    'server': 'apple.com'
                },
                'short_id': [sid],
                'private_key': priv_k
            }
        },
        'client': {
            'reality': {
                'public_key': pub_k,
                'short_id': sid
            },
            'utls': {
                'enabled': True,
                'fingerprint': 'chrome'
            }
        }
    }
    api('POST', 'save', {
        'object': 'tls',
        'action': 'new',
        'data': json.dumps(reality_tls_payload)
    })
    all_tls_resp = api('GET', 'tls') or {}
    all_tls_obj = all_tls_resp.get('obj') or {}
    all_tls = all_tls_obj.get('tls', []) if isinstance(all_tls_obj, dict) else (all_tls_obj or [])
    for t in all_tls:
        if t.get('name') == 'reality' or 'reality' in t.get('server', {}):
            reality_tid = t.get('id')
            break

reality_port = int(os.environ['REALITY_PORT'])
pub_host = os.environ.get('PUBLIC_IP') or domain

reality_payload = {
    'id': reality_id or 0,
    'type': 'vless',
    'tag': reality_tag,
    'tls_id': reality_tid or 0,
    'listen': '::',
    'listen_port': reality_port,
    'addrs': [{
        'server': pub_host,
        'server_port': reality_port
    }]
}
api('POST', 'save', {
    'object': 'inbounds',
    'action': 'edit' if reality_id else 'new',
    'data': json.dumps(reality_payload)
})
created_inbound_tags.append(reality_tag)

# 3. 重新查询最新所有入站 ID
# 3. 重新查询最新所有入站 ID (100% 原生 API)
inbounds_resp = api('GET', 'inbounds') or {}
inbounds_obj = inbounds_resp.get('obj') or []
if isinstance(inbounds_obj, dict):
    inbound_rows = inbounds_obj.get('inbounds') or []
else:
    inbound_rows = inbounds_obj or []
inbound_rows = [r for r in inbound_rows if isinstance(r, dict)]
all_current_ib_ids = [r.get('id') for r in inbound_rows if r.get('id') is not None]

# 4. 配置 admin 客户端，关联全部入站 (100% 原生 API)
clients_resp = api('GET', 'clients') or {}
clients_obj = clients_resp.get('obj') or []
if isinstance(clients_obj, dict):
    clients_rows = clients_obj.get('clients') or []
else:
    clients_rows = clients_obj or []
clients_rows = [r for r in clients_rows if isinstance(r, dict)]

admin_name = os.environ.get('SUI_ADMIN_USER', 'admin')
admin = next((c for c in clients_rows if c.get('id') == 1), None)
if not admin:
    admin = next((c for c in clients_rows if c.get('name') == admin_name), None)
if not admin and len(clients_rows) > 0:
    admin = clients_rows[0]
if admin:
    admin_name = admin.get('name', admin_name)

client_uuid = str(uuid.uuid4())
client_pass = os.urandom(8).hex()

existing_cfg = admin.get('config') if (admin and isinstance(admin.get('config'), dict)) else {}
client_config = {
    'vless': {
        'name': admin_name,
        'uuid': existing_cfg.get('vless', {}).get('uuid') or client_uuid,
        'flow': 'xtls-rprx-vision'
    },
    'vmess': {
        'name': admin_name,
        'uuid': existing_cfg.get('vmess', {}).get('uuid') or client_uuid
    },
    'tuic': {
        'name': admin_name,
        'uuid': existing_cfg.get('tuic', {}).get('uuid') or client_uuid,
        'password': existing_cfg.get('tuic', {}).get('password') or client_pass
    },
    'hysteria2': {
        'name': admin_name,
        'password': existing_cfg.get('hysteria2', {}).get('password') or client_pass
    }
}
existing_cfg.update(client_config)

existing_inbs = admin.get('inbounds') if (admin and isinstance(admin.get('inbounds'), list)) else []
combined_inbs = list(set(existing_inbs + all_current_ib_ids))

client_payload = {
    'id': admin.get('id', 0) if admin else 0,
    'enable': True,
    'name': admin_name,
    'remark': '默认用户',
    'config': existing_cfg,
    'inbounds': combined_inbs,
    'links': admin.get('links', []) if admin else [],
    'volume': admin.get('volume', 0) if admin else 0,
    'expiry': admin.get('expiry', 0) if admin else 0,
    'down': admin.get('down', 0) if admin else 0,
    'up': admin.get('up', 0) if admin else 0,
    'desc': admin.get('desc', '') if admin else '',
    'group': admin.get('group', '') if admin else '',
    'delayStart': False,
    'autoReset': False,
    'resetDays': 0,
    'nextReset': 0,
    'totalUp': admin.get('totalUp', 0) if admin else 0,
    'totalDown': admin.get('totalDown', 0) if admin else 0,
    'createdAt': admin.get('createdAt', 0) if admin else 0,
    'onlineAt': 0
}

api('POST', 'save', {
    'object': 'clients',
    'action': 'edit' if admin else 'new',
    'data': json.dumps(client_payload)
})
PYEOF
      then
        echo -e "  ${Y}[!] s-ui API 自动配置未完成，请稍后在 s-ui 面板手动补充节点/订阅。${N}"
      fi
      systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || service s-ui restart 2>/dev/null || true
    fi
    systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || service s-ui restart 2>/dev/null || true
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

  systemctl restart sout 2>/dev/null || rc-service sout restart 2>/dev/null || service sout restart 2>/dev/null || true

  # 7. 统一调用终端菜单中的 Caddy 探测并分流，确保 Caddyfile 规则与隧道回源完全一致
  reload_caddy_proxy >/dev/null 2>&1 || true

  # 8. 基础服务与节点已全部就绪后，若用户要求申请证书或本地已有证书，发起申请并增设 TUIC 与 Hysteria2
  local cert_file="/home/acme/${domain}/fullchain.pem"
  local key_file="/home/acme/${domain}/privkey.pem"
  if [[ "$apply_cert" == "y" && -n "$domain" && -n "$cf_dns_key" ]]; then
    echo
    echo -e "  ${B}[+] 基础服务与节点已就绪，正在申请 Cloudflare SSL 证书并配置 TUIC / Hysteria2...${N}"
    if do_apply_cf_ssl_cert "$domain" "$cf_dns_key"; then
      if [[ -s "$cert_file" && -s "$key_file" ]]; then
        echo -e "  [+] 证书就绪，正在自动创建并挂载 TUIC 与 Hysteria2 高速节点..."
        create_tuic_hy2_nodes "$domain" "$cert_file" "$key_file" "false"
      fi
    else
      echo -e "  ${Y}[!] 证书申请未成功或 API Key 无效，基础服务不受影响，您可稍后在终端菜单重新申请并补齐节点。${N}"
    fi
  elif [[ -n "$domain" && -s "$cert_file" && -s "$key_file" ]]; then
    echo
    echo -e "  ${G}[✓] 本地已存在域名 [${domain}] 的完整证书，正在自动挂载 TUIC 与 Hysteria2 节点...${N}"
    create_tuic_hy2_nodes "$domain" "$cert_file" "$key_file" "false"
  fi

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
  echo -e "      唤起命令:  sout"
  echo
  echo -e "  [2] s-ui 节点与分流管理面板"
  echo -e "      访问地址:  ${B}https://${domain}/${sui_path}/${N}"
  echo -e "      管理账号:  ${Y}${sui_admin_user}${N}"
  echo -e "      管理密码:  ${D}[由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]${N}"
  echo -e "      唤起命令:  s-ui"
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
  echo
  echo -e "  [Cloudflare SSL 证书 (Caddy DNS-01)]"
  echo -e "  ${D}• 令牌需含「区域.DNS / 编辑」权限，用于自动签发证书并开启 TUIC / Hysteria2 节点${N}"
  echo -e "  ${D}• 直接按回车将跳过申请，自动配置常规节点 (vmess-argo / vless-reality)${N}"
  local cf_dns_key="" apply_cert="n"
  read -rp "  4. 请输入 Cloudflare API 令牌 (直接回车跳过): " cf_dns_key
  cf_dns_key=$(echo "$cf_dns_key" | tr -d ' \r\n')
  if [[ -n "$cf_dns_key" ]]; then
    apply_cert="y"
    echo -e "  ${G}[✓] Cloudflare 令牌已记录，部署时将通过 DNS-01 验证自动签发证书。${N}"
  else
    echo -e "  ${G}[✓] 已跳过证书申请，将自动配置常规节点 (vmess-argo 与 vless-reality)。${N}"
  fi

  setup_caddy_proxy "$domain" "$tunnel_token" "$tunnel_port" "$apply_cert" "$cf_dns_key"
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

def to_str(v):
    if isinstance(v, bytes): return v.decode('utf-8', errors='replace')
    return str(v) if v is not None else ''

db_path = '/usr/local/s-ui/db/s-ui.db'
con = sqlite3.connect(db_path)
cur = con.cursor()
cur.execute('SELECT id, type, tag, options, addrs FROM inbounds')
rows = cur.fetchall()

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
            
        # 同步更新节点域名与 SNI 及 transport Header 中的 Host
        try:
            domain_val = '${domain}'
            if domain_val:
                addrs = json.loads(addrs_raw) if addrs_raw else []
                if isinstance(addrs, list) and len(addrs) > 0:
                    addrs[0]['server'] = domain_val
                    if 'tls' in addrs[0] and isinstance(addrs[0]['tls'], dict):
                        addrs[0]['tls']['server_name'] = domain_val
                    cur.execute('UPDATE inbounds SET addrs=? WHERE id=?', (sqlite3.Binary(json.dumps(addrs).encode('utf-8')), ib_id))
                    
                if isinstance(tr, dict) and 'headers' in tr and isinstance(tr['headers'], dict):
                    tr['headers']['Host'] = domain_val
                    opt['transport'] = tr
                    cur.execute('UPDATE inbounds SET options=? WHERE id=?', (sqlite3.Binary(json.dumps(opt).encode('utf-8')), ib_id))
                    
                con.commit()
        except Exception:
            pass
        break

con.close()
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
      sui_token=$(get_or_create_sui_token "/usr/local/s-ui/db/s-ui.db")
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

  # 4. 重新生成纯净 Caddyfile (精确绑定当前检测到的隧道回源端口，并追加 SSL 域名自动续期块)
  mkdir -p /etc/caddy
  cat > /etc/caddy/Caddyfile <<EOF
{
    admin off
    auto_https disable_redirects
}

http://127.0.0.1:${tunnel_port}, http://:${tunnel_port} {
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

  # 附加所有已申请的 Cloudflare SSL 域名 (由 Caddy 在独立 8443 端口自动续期，绝不抢占 80/443 与隧道反代产生冲突)
  if [[ -f "${WORK_DIR}/cf_ssl_domains.json" ]]; then
    python3 -c '
import json, os
p = "/var/lib/sout/cf_ssl_domains.json"
try:
    with open(p) as f:
        d = json.load(f)
    with open("/etc/caddy/Caddyfile", "a") as cf:
        for dom, info in d.items():
            tok = info.get("token", "")
            if dom and tok:
                block = f"\n{dom}:8443 {{\n    tls {{\n        dns cloudflare \"{tok}\"\n    }}\n    respond \"SSL Ready\" 200\n}}\n"
                cf.write(block)
except Exception:
    pass
' 2>/dev/null || true
  fi

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

  # 6. 重启 Caddy 服务 (兼容 systemd 与 OpenRC/Alpine)
  local caddy_restart_ok=false
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    systemctl restart caddy 2>/dev/null && caddy_restart_ok=true
    systemctl enable caddy 2>/dev/null || true
  elif command -v rc-service >/dev/null 2>&1; then
    rc-service caddy restart 2>/dev/null && caddy_restart_ok=true
  elif command -v service >/dev/null 2>&1; then
    service caddy restart 2>/dev/null && caddy_restart_ok=true
  fi

  if [[ "$caddy_restart_ok" == "true" ]]; then
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

do_apply_cf_ssl_cert() {
  local domain="$1"
  local cf_token="$2"
  local force="${3:-false}"

  domain=$(echo "$domain" | tr -d ' \r\n' | sed -e 's|^https\?://||' -e 's|/.*||')
  cf_token=$(echo "$cf_token" | tr -d ' \r\n')
  [[ -z "$domain" || -z "$cf_token" ]] && return 1

  local cert_dir="/home/acme/${domain}"
  local cert_file="${cert_dir}/fullchain.pem"
  local key_file="${cert_dir}/privkey.pem"
  if [[ "$force" != "true" && -s "$cert_file" && -s "$key_file" ]]; then
    echo -e "  ${G}[✓] 本地已存在域名 [${domain}] 的完整证书，直接复用。${N}"
    return 0
  fi

  echo -e "  ${B}[1/4] 正在检查并准备 Caddy 服务 (集成 Cloudflare DNS 模块)...${N}"
  ensure_caddy_with_cloudflare || { echo -e "  ${R}准备 Caddy 失败${N}"; return 1; }

  echo -e "  ${B}[2/4] 正在创建证书存储目录: ${cert_dir}...${N}"
  mkdir -p "$cert_dir"
  chmod 700 /home/acme "$cert_dir"

  echo -e "  ${B}[3/4] 正在配置 Caddy 并通过 Cloudflare DNS-01 验证发起申请...${N}"
  local cf_domains_meta="${WORK_DIR}/cf_ssl_domains.json"
  python3 -c '
import json, os, time, sys
p = sys.argv[1]
dom = sys.argv[2]
tok = sys.argv[3]
cdir = sys.argv[4]
d = {}
if os.path.exists(p):
    try:
        with open(p) as f:
            d = json.load(f)
    except Exception:
        pass
d[dom] = {
    "token": tok,
    "applied_at": int(time.time()),
    "cert_dir": cdir
}
with open(p, "w") as f:
    json.dump(d, f, indent=2)
try:
    os.chmod(p, 0o600)
except Exception:
    pass
' "$cf_domains_meta" "$domain" "$cf_token" "$cert_dir" 2>/dev/null || true

  if [[ -f "$CADDY_META" ]] && grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META" 2>/dev/null; then
    reload_caddy_proxy
  else
    # 尚无隧道元数据时（如全新安装初期，或独立申请证书），直接生成包含该域名 DNS-01 验证的基础 Caddyfile 并启动 Caddy
    mkdir -p /etc/caddy /var/log/caddy
    cat > /etc/caddy/Caddyfile <<EOF
{
    admin off
    auto_https disable_redirects
}

${domain}:8443 {
    tls {
        dns cloudflare "${cf_token}"
    }
    respond "SSL Ready" 200
}
EOF
    if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
      systemctl restart caddy 2>/dev/null || true
      systemctl enable caddy 2>/dev/null || true
    elif command -v rc-service >/dev/null 2>&1; then
      rc-service caddy restart 2>/dev/null || true
    elif command -v service >/dev/null 2>&1; then
      service caddy restart 2>/dev/null || true
    fi
  fi

  echo -e "  ${B}[4/4] 正在等待 Cloudflare DNS 解析生效并签发证书 (通常需 15-40 秒)...${N}"
  local ok=false
  for i in $(seq 1 45); do
    echo -ne "  正在等待签发中... (${i}/45s)\r"
    local found_crt found_key
    found_crt=$(find /var/lib/caddy /root/.local/share/caddy /root/.config/caddy /etc/caddy /var/log/caddy "${HOME:-/root}/.local/share/caddy" -name "${domain}.crt" -size +100c 2>/dev/null | head -1)
    found_key=$(find /var/lib/caddy /root/.local/share/caddy /root/.config/caddy /etc/caddy /var/log/caddy "${HOME:-/root}/.local/share/caddy" -name "${domain}.key" -size +100c 2>/dev/null | head -1)
    if [[ -n "$found_crt" && -n "$found_key" ]]; then
      cp -f "$found_crt" "${cert_dir}/fullchain.pem"
      cp -f "$found_key" "${cert_dir}/privkey.pem"
      cp -f "$found_crt" "${cert_dir}/cert.crt"
      cp -f "$found_key" "${cert_dir}/private.key"
      chmod 600 "${cert_dir}"/*
      ok=true
      break
    fi
    sleep 1
  done
  echo

  if [[ "$ok" == "true" ]]; then
    echo -e "  ${G}🎉 恭喜！Cloudflare SSL 证书申请成功！${N}"
    echo -e "${B}========================================${N}"
    echo -e "  托管域名:    ${B}${domain}${N}"
    echo -e "  公钥路径:    ${G}${cert_dir}/fullchain.pem${N}"
    echo -e "  私钥路径:    ${G}${cert_dir}/privkey.pem${N}"
    echo -e "  备用公钥:    ${G}${cert_dir}/cert.crt${N}"
    echo -e "  备用私钥:    ${G}${cert_dir}/private.key${N}"
    echo -e "  自动续期:    ${G}已默认开启 (Caddy 后台静默自动续期)${N}"
    echo -e "${B}========================================${N}"
    return 0
  else
    echo -e "  ${Y}[!] 签发超时或正在后台排队验证，请查看 Caddy 运行日志：${N}"
    journalctl -u caddy -n 25 --no-pager 2>/dev/null || tail -n 25 /var/log/caddy.log 2>/dev/null || tail -n 25 /var/log/messages 2>/dev/null || true
    echo
    echo -e "  ${Y}若 Cloudflare DNS API 权限正确，Caddy 会在后台继续完成签发并自动存入 ${cert_dir}/${N}"
    return 1
  fi
}

apply_cf_ssl_cert() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  申请 Cloudflare SSL 证书 (Caddy DNS-01)${N}"
  echo -e "${B}========================================${N}"
  echo -e "  ${D}• 使用 Cloudflare DNS-01 验证，无需开放 80/443 端口即可申请${N}"
  echo -e "  ${D}• 证书将自动存放在 /home/acme/<域名>/ 目录下，默认开启自动续期${N}"
  echo

  local domain cf_token
  read -rp "  [1/2] 请输入要申请证书的域名 (如 example.com): " domain
  domain=$(echo "$domain" | tr -d ' \r\n' | sed -e 's|^https\?://||' -e 's|/.*||')
  if [[ -z "$domain" ]]; then
    echo -e "  ${R}域名不能为空，已取消申请。${N}"
    return
  fi

  # 检查本地是否已存在完整证书
  local cert_dir="/home/acme/${domain}"
  local cert_file="${cert_dir}/fullchain.pem"
  local key_file="${cert_dir}/privkey.pem"
  local force_apply="false"
  if [[ -s "$cert_file" && -s "$key_file" ]]; then
    echo
    echo -e "  ${Y}⚠️  检测到本地已存在域名 [${domain}] 的完整证书与私钥：${N}"
    echo -e "      公钥: ${B}${cert_file}${N}"
    echo -e "      私钥: ${B}${key_file}${N}"
    if command -v openssl >/dev/null 2>&1; then
      local not_after
      not_after=$(openssl x509 -in "$cert_file" -noout -enddate 2>/dev/null | sed 's/notAfter=//')
      [[ -n "$not_after" ]] && echo -e "      有效期至: ${G}${not_after}${N}"
    fi
    echo
    read -rp "  是否要强制申请并覆盖本地所保存的证书？[y/N] (默认 N): " overwrite
    overwrite=$(echo "$overwrite" | tr -d ' \r\n')
    if [[ "$overwrite" != "y" && "$overwrite" != "Y" ]]; then
      echo -e "  ${Y}已保留本地现有证书，取消重新申请。${N}"
      return
    fi
    force_apply="true"
    echo -e "  ${Y}用户确认强制覆盖，将重新向 Cloudflare 发起申请...${N}"
  fi

  echo
  echo -e "  [2/2] 请输入 Cloudflare API 令牌 (API Token):"
  echo -e "  ${D}提示: 该令牌需包含权限「区域.DNS / 编辑」 (Zone.DNS:Edit)${N}"
  read -rp "  API Token: " cf_token
  cf_token=$(echo "$cf_token" | tr -d ' \r\n')
  if [[ -z "$cf_token" ]]; then
    echo -e "  ${R}API Token 不能为空，已取消申请。${N}"
    return
  fi

  do_apply_cf_ssl_cert "$domain" "$cf_token" "$force_apply"
}

ensure_caddy_with_cloudflare() {
  if command -v caddy >/dev/null 2>&1 && caddy list-modules 2>/dev/null | grep -q "dns.providers.cloudflare"; then
    return 0
  fi

  echo -e "  ${B}[+] 正在获取集成 Cloudflare DNS 模块的 Caddy 反代服务...${N}"
  local arch
  arch=$(get_caddy_arch)
  local tmp
  tmp=$(mktemp -d)
  local url="https://caddyserver.com/api/download?os=linux&arch=${arch}&p=github.com%2Fcaddy-dns%2Fcloudflare"

  if ! curl -fsSL "$url" -o "$tmp/caddy"; then
    echo -e "  ${Y}[!] 官方下载稍慢，正在重试...${N}"
    if ! curl -fsSL "$url" -o "$tmp/caddy"; then
      echo -e "  ${R}[✗] Caddy (Cloudflare 模块版) 下载失败，请检查网络连接${N}" >&2
      rm -rf "$tmp"
      return 1
    fi
  fi

  systemctl stop caddy 2>/dev/null || true
  install -m 755 "$tmp/caddy" /usr/local/bin/caddy
  rm -rf "$tmp"

  mkdir -p /etc/caddy /var/log/caddy /var/lib/caddy /home/acme
  chmod 755 /etc/caddy /var/log/caddy
  chmod 700 /home/acme
  mkdir -p /etc/systemd/system

  if [[ ! -f /etc/systemd/system/caddy.service ]]; then
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
LimitNPROC=512
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload 2>/dev/null || true
  fi

  return 0
}

view_cf_ssl_certs() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  当前已申请的域名证书列表 (/home/acme)${N}"
  echo -e "${B}========================================${N}"
  local count=0
  if [[ -d "/home/acme" ]]; then
    for dir in /home/acme/*/; do
      [[ ! -d "$dir" ]] && continue
      local dom
      dom=$(basename "$dir")
      local cert_f="" key_f=""
      if [[ -s "${dir}fullchain.pem" ]]; then
        cert_f="${dir}fullchain.pem"
      elif [[ -s "${dir}cert.crt" ]]; then
        cert_f="${dir}cert.crt"
      elif [[ -s "${dir}${dom}.crt" ]]; then
        cert_f="${dir}${dom}.crt"
      fi

      if [[ -s "${dir}privkey.pem" ]]; then
        key_f="${dir}privkey.pem"
      elif [[ -s "${dir}private.key" ]]; then
        key_f="${dir}private.key"
      elif [[ -s "${dir}${dom}.key" ]]; then
        key_f="${dir}${dom}.key"
      fi

      if [[ -n "$cert_f" && -n "$key_f" ]]; then
        count=$((count + 1))
        echo -e "  ${G}[${count}] 域名: ${B}${dom}${N}"
        echo -e "      公钥路径: ${Y}${cert_f}${N}"
        echo -e "      私钥路径: ${Y}${key_f}${N}"
        if command -v openssl >/dev/null 2>&1; then
          local issuer not_after
          issuer=$(openssl x509 -in "$cert_f" -noout -issuer 2>/dev/null | sed 's/issuer=//' | sed 's/.*CN = //' | tr -d '\n')
          not_after=$(openssl x509 -in "$cert_f" -noout -enddate 2>/dev/null | sed 's/notAfter=//')
          if [[ -n "$not_after" ]]; then
            local expire_sec now_sec diff_days
            expire_sec=$(date -d "$not_after" +%s 2>/dev/null || date -jf "%b %d %T %Y %Z" "$not_after" +%s 2>/dev/null || echo "0")
            now_sec=$(date +%s)
            if (( expire_sec > now_sec )); then
              diff_days=$(( (expire_sec - now_sec) / 86400 ))
              local disp_issuer="${issuer:-Lets Encrypt}"
              echo -e "      签发机构: ${C}${disp_issuer}${N}"
              echo -e "      证书状态: ${G}有效 (剩余 ${diff_days} 天，到期时间: ${not_after})${N}"
            else
              echo -e "      证书状态: ${R}已过期 (到期时间: ${not_after})${N}"
            fi
          fi
        fi
        echo -e "      自动续期: ${G}默认开启 (由 Caddy 服务后台自动托管续期)${N}"
        echo -e "  ${D}----------------------------------------${N}"
      fi
    done
  fi
  if (( count == 0 )); then
    echo -e "  ${Y}暂未在 /home/acme 目录下检索到任何已申请的域名证书。${N}"
    echo -e "  ${D}提示: 您可以通过选项 [2] 输入域名与 Cloudflare API 令牌立即申请。${N}"
  else
    echo -e "  共检索到 ${G}${count}${N} 个有效域名证书。"
  fi
  echo
}

apply_cf_ssl_cert() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  申请 Cloudflare SSL 证书 (Caddy DNS-01)${N}"
  echo -e "${B}========================================${N}"
  echo -e "  ${D}• 使用 Cloudflare DNS-01 验证，无需开放 80/443 端口即可申请${N}"
  echo -e "  ${D}• 证书将自动存放在 /home/acme/<域名>/ 目录下，默认开启自动续期${N}"
  echo

  local domain cf_token
  read -rp "  [1/2] 请输入要申请证书的域名 (如 example.com): " domain
  domain=$(echo "$domain" | tr -d ' \r\n' | sed -e 's|^https\?://||' -e 's|/.*||')
  if [[ -z "$domain" ]]; then
    echo -e "  ${R}域名不能为空，已取消申请。${N}"
    return
  fi

  # 检查本地是否已存在完整证书
  local cert_dir="/home/acme/${domain}"
  local cert_file="${cert_dir}/fullchain.pem"
  local key_file="${cert_dir}/privkey.pem"
  if [[ -s "$cert_file" && -s "$key_file" ]]; then
    echo
    echo -e "  ${Y}⚠️  检测到本地已存在域名 [${domain}] 的完整证书与私钥：${N}"
    echo -e "      公钥: ${B}${cert_file}${N}"
    echo -e "      私钥: ${B}${key_file}${N}"
    if command -v openssl >/dev/null 2>&1; then
      local not_after
      not_after=$(openssl x509 -in "$cert_file" -noout -enddate 2>/dev/null | sed 's/notAfter=//')
      [[ -n "$not_after" ]] && echo -e "      有效期至: ${G}${not_after}${N}"
    fi
    echo
    read -rp "  是否要强制申请并覆盖本地所保存的证书？[y/N] (默认 N): " overwrite
    overwrite=$(echo "$overwrite" | tr -d ' \r\n')
    if [[ "$overwrite" != "y" && "$overwrite" != "Y" ]]; then
      echo -e "  ${Y}已保留本地现有证书，取消重新申请。${N}"
      return
    fi
    echo -e "  ${Y}用户确认强制覆盖，将重新向 Cloudflare 发起申请...${N}"
  fi

  echo
  echo -e "  [2/2] 请输入 Cloudflare API 令牌 (API Token):"
  echo -e "  ${D}提示: 该令牌需包含权限「区域.DNS / 编辑」 (Zone.DNS:Edit)${N}"
  read -rp "  API Token: " cf_token
  cf_token=$(echo "$cf_token" | tr -d ' \r\n')
  if [[ -z "$cf_token" ]]; then
    echo -e "  ${R}API Token 不能为空，已取消申请。${N}"
    return
  fi

  do_apply_cf_ssl_cert "$domain" "$cf_token" true
}

cf_ssl_menu() {
  while true; do
    echo
    echo -e "${B}========================================${N}"
    echo -e "${B}  Cloudflare SSL 证书申请与管理 (Caddy)${N}"
    echo -e "${B}========================================${N}"
    echo -e "   1) 查看当前域名证书"
    echo -e "   2) 申请证书 (Cloudflare DNS-01 API)"
    echo -e "   0) 返回上级菜单"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-2]: " opt
    case "$opt" in
      1) view_cf_ssl_certs; pause ;;
      2) apply_cf_ssl_cert; pause ;;
      0) break ;;
      *) ;;
    esac
  done
}

create_tuic_hy2_nodes() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}     创建 TUIC / Hysteria2 节点${N}"
  echo -e "${B}========================================${N}"

  local sui_db="/usr/local/s-ui/db/s-ui.db"
  if [[ ! -f "$sui_db" ]]; then
    echo -e "  ${R}[!] 未检测到 s-ui 面板，请先安装并配置 s-ui。${N}"
    return 1
  fi

  local sui_token
  sui_token=$(get_or_create_sui_token "$sui_db")
  if [[ -z "$sui_token" ]]; then
    echo -e "  ${R}[!] 未能获取到 s-ui API Token，请先在终端运行一次 sout 生成凭据。${N}"
    return 1
  fi

  local cur_port cur_path
  cur_port=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPort'" 2>/dev/null || echo "8443")
  cur_path=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPath'" 2>/dev/null || echo "/app/")
  cur_path="/${cur_path#/}"
  [[ "$cur_path" != */ ]] && cur_path="${cur_path}/"
  local sui_api="http://127.0.0.1:${cur_port}${cur_path}apiv2"

  local in_domain="${1:-}"
  local in_cert_file="${2:-}"
  local in_key_file="${3:-}"
  local in_insecure="${4:-false}"

  local cert_file="" key_file="" cert_domain="" is_insecure="false"

  if [[ -n "$in_domain" && -s "$in_cert_file" && -s "$in_key_file" ]]; then
    cert_domain="$in_domain"
    cert_file="$in_cert_file"
    key_file="$in_key_file"
    is_insecure="$in_insecure"
    echo -e "  ${G}[✓] 自动应用指定证书: ${B}${cert_domain}${N}"
  else
    echo -e "  请选择用于 TUIC / Hysteria2 的 TLS 证书来源："
    echo -e "   1) 使用本地证书 (推荐 - 自动扫描 /home/acme/ 目录)"
    echo -e "   2) 使用自定义路径证书 (手动指定域名与公私钥路径)"
    echo -e "   3) 使用自签证书 (自动生成 10 年期自签证书，启用客户端允许不安全连接)"
    echo -e "   0) 返回上级菜单"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-3] (默认 1): " cert_opt
    cert_opt=$(echo "$cert_opt" | tr -d ' \r\n')
    [[ -z "$cert_opt" ]] && cert_opt="1"
    [[ "$cert_opt" == "0" ]] && return 0
  fi

  if [[ -z "$cert_domain" || ! -s "$cert_file" || ! -s "$key_file" ]]; then
    if [[ "$cert_opt" == "1" ]]; then
      local acme_dir="/home/acme"
      if [[ ! -d "$acme_dir" ]]; then
        echo -e "  ${R}[!] 本地证书目录 ${acme_dir} 不存在，暂无可用证书。${N}"
        echo -e "  ${Y}提示: 请先在主菜单中申请 Cloudflare 证书，或选择模式 3 生成自签证书。${N}"
        return 1
      fi

      local valid_domains=()
      local d
      for d in "$acme_dir"/*; do
        if [[ -d "$d" ]]; then
          local dom_name
          dom_name=$(basename "$d")
          local pub_cand="" priv_cand=""
          if [[ -s "${d}/fullchain.pem" ]]; then pub_cand="${d}/fullchain.pem"
          elif [[ -s "${d}/cert.crt" ]]; then pub_cand="${d}/cert.crt"
          elif [[ -s "${d}/${dom_name}.cer" ]]; then pub_cand="${d}/${dom_name}.cer"
          fi

          if [[ -s "${d}/privkey.pem" ]]; then priv_cand="${d}/privkey.pem"
          elif [[ -s "${d}/private.key" ]]; then priv_cand="${d}/private.key"
          elif [[ -s "${d}/${dom_name}.key" ]]; then priv_cand="${d}/${dom_name}.key"
          fi

          if [[ -n "$pub_cand" && -n "$priv_cand" ]]; then
            valid_domains+=("${dom_name}|${pub_cand}|${priv_cand}")
          fi
        fi
      done

      local count=${#valid_domains[@]}
      if [[ "$count" -eq 0 ]]; then
        echo -e "  ${R}[!] 在 ${acme_dir} 下未发现有效证书 (需同时包含有效公钥与私钥)。${N}"
        echo -e "  ${Y}提示: 请先在主菜单中申请证书，或选择模式 3 生成自签证书。${N}"
        return 1
      elif [[ "$count" -eq 1 ]]; then
        IFS='|' read -r cert_domain cert_file key_file <<< "${valid_domains[0]}"
        echo -e "  ${G}[✓] 自动应用本地唯一有效证书: ${B}${cert_domain}${N}"
      else
        echo
        echo -e "  ${G}检测到本地存在多个有效证书，请选择要应用的域名：${N}"
        local idx=1
        for item in "${valid_domains[@]}"; do
          IFS='|' read -r d_name _ _ <<< "$item"
          echo -e "   ${idx}) ${B}${d_name}${N}"
          idx=$((idx + 1))
        done
        echo -e "   0) 取消"
        read -rp "  请选择 [1-${count}]: " sel
        sel=$(echo "$sel" | tr -d ' \r\n')
        if [[ "$sel" =~ ^[1-9][0-9]*$ ]] && [[ "$sel" -le "$count" ]]; then
          IFS='|' read -r cert_domain cert_file key_file <<< "${valid_domains[$((sel - 1))]}"
          echo -e "  ${G}[✓] 已选择域名: ${B}${cert_domain}${N}"
        else
          echo -e "  ${Y}输入无效，已取消操作。${N}"
          return 1
        fi
      fi

    elif [[ "$cert_opt" == "2" ]]; then
      read -rp "  请输入证书对应域名 (如 example.com): " cert_domain
      cert_domain=$(echo "$cert_domain" | tr -d ' \r\n' | sed -e 's|^https\?://||' -e 's|/.*||')
      if [[ -z "$cert_domain" ]]; then
        echo -e "  ${R}[!] 域名不能为空！${N}"
        return 1
      fi
      read -rp "  请输入公钥证书文件绝对路径 (如 /etc/ssl/fullchain.pem): " cert_file
      cert_file=$(echo "$cert_file" | tr -d ' \r\n')
      if [[ ! -s "$cert_file" ]]; then
        echo -e "  ${R}[!] 公钥文件不存在或为空: ${cert_file}${N}"
        return 1
      fi
      read -rp "  请输入私钥文件绝对路径 (如 /etc/ssl/privkey.pem): " key_file
      key_file=$(echo "$key_file" | tr -d ' \r\n')
      if [[ ! -s "$key_file" ]]; then
        echo -e "  ${R}[!] 私钥文件不存在或为空: ${key_file}${N}"
        return 1
      fi

    elif [[ "$cert_opt" == "3" ]]; then
      read -rp "  请输入自签证书伪装域名 [直接回车默认 apple.com]: " cert_domain
      cert_domain=$(echo "$cert_domain" | tr -d ' \r\n' | sed -e 's|^https\?://||' -e 's|/.*||')
      [[ -z "$cert_domain" ]] && cert_domain="apple.com"

      local ssc_dir="/home/ssc/${cert_domain}"
      mkdir -p "$ssc_dir"
      cert_file="${ssc_dir}/fullchain.pem"
      key_file="${ssc_dir}/privkey.pem"

      echo -e "  [+] 正在生成 ${cert_domain} 的 10 年期自签证书..."
      if ! command -v openssl >/dev/null 2>&1; then
        echo -e "  [+] 正在自动安装 openssl 证书工具..."
        apk add --no-cache openssl 2>/dev/null || apt-get update -y && apt-get install -y openssl 2>/dev/null || yum install -y openssl 2>/dev/null || true
      fi

      if command -v openssl >/dev/null 2>&1; then
        openssl req -x509 -nodes -days 3650 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
          -keyout "$key_file" -out "$cert_file" -subj "/CN=${cert_domain}" >/dev/null 2>&1 || \
        openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
          -keyout "$key_file" -out "$cert_file" -subj "/CN=${cert_domain}" >/dev/null 2>&1
      else
        echo -e "  ${R}[!] 系统未找到 openssl，无法生成自签证书。${N}"
        return 1
      fi

      if [[ ! -s "$cert_file" || ! -s "$key_file" ]]; then
        echo -e "  ${R}[!] 自签证书生成失败！${N}"
        return 1
      fi

      is_insecure="true"
      echo -e "  ${G}[✓] 自签证书已生成在: ${ssc_dir}${N}"
      echo -e "  ${Y}💡 注意: 已启用「允许不安全连接 (Insecure)」，客户端连接时将跳过证书信任链校验。${N}"
    else
      echo -e "  ${Y}无效选项，已取消。${N}"
      return 1
    fi
  fi

  # 查询当前是否已存在 TUIC / Hysteria2 节点
  local existing_nodes
  existing_nodes=$(python3 -c "
import urllib.request, urllib.parse, json
BASE = '${sui_api}'
TOKEN = '${sui_token}'
headers = {'Token': TOKEN}
req = urllib.request.Request(BASE.rstrip('/') + '/inbounds', headers=headers)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        d = json.loads(resp.read().decode('utf-8'))
        inbs = d.get('obj', {}).get('inbounds', [])
        tuic_n = next((x for x in inbs if x.get('type') == 'tuic'), None)
        hy2_n = next((x for x in inbs if x.get('type') == 'hysteria2'), None)
        out = []
        if tuic_n: out.append(f\"TUIC|{tuic_n.get('tag')}|{tuic_n.get('listen_port')}|{tuic_n.get('tls_id')}\")
        if hy2_n: out.append(f\"Hysteria2|{hy2_n.get('tag')}|{hy2_n.get('listen_port')}|{hy2_n.get('tls_id')}\")
        print(';'.join(out))
except Exception:
    pass
" 2>/dev/null || true)

  local action_mode="create_both"
  if [[ -n "$existing_nodes" ]]; then
    IFS=';' read -ra node_list <<< "$existing_nodes"
    local count_exist=${#node_list[@]}

    if [[ "$count_exist" -ge 2 ]]; then
      echo
      echo -e "  ${Y}⚠️  检测到已存在完整的 TUIC 与 Hysteria2 节点：${N}"
      for n_item in "${node_list[@]}"; do
        IFS='|' read -r n_type n_tag n_port n_tid <<< "$n_item"
        echo -e "    • ${n_type} 节点: Tag=${n_tag} 端口=${n_port} 当前TLS_ID=${n_tid}"
      done
      echo
      if [[ -n "$in_domain" ]]; then
        action_mode="change_existing_only"
      else
        read -rp "  是否仅仅变更现有节点的证书(TLS)？[y/N]: " do_chg
        do_chg=$(echo "$do_chg" | tr -d ' \r\n' | tr '[:upper:]' '[:lower:]')
        if [[ "$do_chg" != "y" && "$do_chg" != "yes" ]]; then
          echo -e "  ${D}已取消操作，未对现有节点进行修改。${N}"
          return 0
        fi
        action_mode="change_existing_only"
      fi

    elif [[ "$count_exist" -eq 1 ]]; then
      local exist_type exist_tag exist_port exist_tid missing_type
      IFS='|' read -r exist_type exist_tag exist_port exist_tid <<< "${node_list[0]}"
      if [[ "$exist_type" == "TUIC" ]]; then
        missing_type="Hysteria2"
      else
        missing_type="TUIC"
      fi

      echo
      echo -e "  ${Y}⚠️  检测到当前仅存在单节点：${N}"
      echo -e "    • 已有节点: ${G}${exist_type}${N} (Tag=${exist_tag} 端口=${exist_port} 当前TLS_ID=${exist_tid})"
      echo -e "    • 缺失节点: ${R}${missing_type}${N}"
      echo
      if [[ -n "$in_domain" ]]; then
        action_mode="change_and_fill"
      else
        echo -e "  请选择处理方式："
        echo -e "   1) 变更现有 [${exist_type}] 证书，并自动补齐创建缺失的 [${missing_type}] 节点 (推荐)"
        echo -e "   2) 仅变更现有 [${exist_type}] 节点的证书，不补齐缺失节点"
        echo -e "   0) 取消退出"
        echo -e "${D}----------------------------------------${N}"
        read -rp "  请选择 [0-2] (默认 1): " single_choice
        single_choice=$(echo "$single_choice" | tr -d ' \r\n')
        [[ -z "$single_choice" ]] && single_choice="1"

        if [[ "$single_choice" == "1" ]]; then
          action_mode="change_and_fill"
        elif [[ "$single_choice" == "2" ]]; then
          action_mode="change_existing_only"
        else
          echo -e "  ${D}已取消操作，未对现有节点进行修改。${N}"
          return 0
        fi
      fi
    fi
  fi

  local cur_cc
  cur_cc=$(get_tcp_congestion)
  local admin_user
  admin_user=$(get_sui_user)
  local public_ip
  public_ip=$(curl -s4m 5 https://api.ipify.org 2>/dev/null || curl -s4m 5 https://ifconfig.me 2>/dev/null || echo "$cert_domain")

  local tuic_p hy2_p
  tuic_p=$(rand_local_port)
  hy2_p=$(rand_local_port)

  # 纯原生 REST API 执行 TLS 注册与节点创建/变更
  SUI_API="$sui_api" \
  SUI_TOKEN="$sui_token" \
  CERT_DOMAIN="$cert_domain" \
  CERT_FILE="$cert_file" \
  KEY_FILE="$key_file" \
  IS_INSECURE="$is_insecure" \
  ACTION_MODE="$action_mode" \
  SUI_ADMIN_USER="$admin_user" \
  PUBLIC_IP="$public_ip" \
  CONGESTION_CONTROL="$cur_cc" \
  TUIC_PORT="$tuic_p" \
  HY2_PORT="$hy2_p" \
  python3 <<'PYEOF'
import json, os, sys, urllib.request, urllib.parse, re, random, string

BASE = os.environ['SUI_API']
TOKEN = os.environ['SUI_TOKEN']
cert_domain = os.environ['CERT_DOMAIN']
cert_file = os.environ['CERT_FILE']
key_file = os.environ['KEY_FILE']
is_insecure = (os.environ.get('IS_INSECURE', 'false').lower() == 'true')
action_mode = os.environ.get('ACTION_MODE', 'create_both')
admin_name = os.environ.get('SUI_ADMIN_USER', 'admin')
pub_ip = os.environ.get('PUBLIC_IP', cert_domain)
cc = os.environ.get('CONGESTION_CONTROL', 'bbr')

def api(method, endpoint, form=None):
    url = BASE.rstrip('/') + '/' + endpoint.lstrip('/')
    data = urllib.parse.urlencode(form).encode() if form else None
    headers = {'Token': TOKEN}
    if data is not None: headers['Content-Type'] = 'application/x-www-form-urlencoded'
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read().decode('utf-8'))

# 1. 检查现有 TLS 并复用或更新匹配的 TLS 模板（彻底防止重复生成 mytls1, mytls2, mytls3 残留）
tls_resp = api('GET', 'tls') or {}
tls_obj = tls_resp.get('obj') or {}
tls_list = tls_obj.get('tls', []) if isinstance(tls_obj, dict) else (tls_obj or [])

target_tls = None
for t in tls_list:
    srv = t.get('server', {})
    if srv.get('server_name') == cert_domain or srv.get('certificate_path') == cert_file:
        target_tls = t
        break

if target_tls:
    new_tls_id = target_tls.get('id')
    new_tls_name = target_tls.get('name', 'mytls1')
    tls_payload = {
        'id': new_tls_id,
        'name': new_tls_name,
        'server': {
            'enabled': True,
            'certificate_path': cert_file,
            'key_path': key_file,
            'server_name': cert_domain
        },
        'client': {
            'insecure': is_insecure,
            'utls': {
                'enabled': True,
                'fingerprint': 'chrome'
            }
        }
    }
    api('POST', 'save', {'object': 'tls', 'action': 'edit', 'data': json.dumps(tls_payload)})
    print(f"\033[32m[✓] 复用现有 TLS 配置: {new_tls_name} (ID: {new_tls_id}, 允许不安全连接: {is_insecure})\033[0m")
else:
    max_num = 0
    for t in tls_list:
        name = t.get('name', '')
        m = re.match(r'^mytls(\d+)$', name)
        if m:
            max_num = max(max_num, int(m.group(1)))
    new_tls_name = f"mytls{max_num + 1}"
    new_tls_payload = {
        'id': 0,
        'name': new_tls_name,
        'server': {
            'enabled': True,
            'certificate_path': cert_file,
            'key_path': key_file,
            'server_name': cert_domain
        },
        'client': {
            'insecure': is_insecure,
            'utls': {
                'enabled': True,
                'fingerprint': 'chrome'
            }
        }
    }
    save_tls_res = api('POST', 'save', {'object': 'tls', 'action': 'new', 'data': json.dumps(new_tls_payload)})
    if not save_tls_res.get('success'):
        print(f"\033[31m[!] 注册 TLS 对象失败: {save_tls_res.get('msg')}\033[0m")
        sys.exit(1)

    tls_resp2 = api('GET', 'tls') or {}
    tls_obj2 = tls_resp2.get('obj') or {}
    tls_list2 = tls_obj2.get('tls', []) if isinstance(tls_obj2, dict) else (tls_obj2 or [])
    new_tls_id = None
    for t in tls_list2:
        if t.get('name') == new_tls_name:
            new_tls_id = t.get('id')
            break

    if not new_tls_id:
        print(f"\033[31m[!] 未能检索到新创建的 TLS ID ({new_tls_name})\033[0m")
        sys.exit(1)

    print(f"\033[32m[✓] 成功注册 TLS 配置: {new_tls_name} (ID: {new_tls_id}, 允许不安全连接: {is_insecure})\033[0m")

# 顺便清理未被任何入站关联的同域名历史冗余 TLS 模板，保持模板列表纯净
try:
    inb_check = api('GET', 'inbounds') or {}
    inb_rows_c = (inb_check.get('obj') or {}).get('inbounds', []) if isinstance(inb_check.get('obj'), dict) else (inb_check.get('obj') or [])
    used_tids = set(ib.get('tls_id') for ib in inb_rows_c if isinstance(ib, dict) and ib.get('tls_id'))
    for t in tls_list:
        tid = t.get('id')
        tname = t.get('name', '')
        if tname.startswith('mytls') and tid != new_tls_id and tid not in used_tids:
            api('POST', 'save', {'object': 'tls', 'action': 'del', 'data': str(tid)})
except Exception:
    pass

# 2. 查询当前入站节点并根据 action_mode 执行变更或补齐
inb_resp = api('GET', 'inbounds') or {}
inb_obj_first = inb_resp.get('obj') or {}
inbound_rows = inb_obj_first.get('inbounds', []) if isinstance(inb_obj_first, dict) else (inb_obj_first or [])
inbound_rows = [ib for ib in inbound_rows if isinstance(ib, dict)]
existing_tuic = next((ib for ib in inbound_rows if ib.get('type') == 'tuic'), None)
existing_hy2 = next((ib for ib in inbound_rows if ib.get('type') == 'hysteria2'), None)

def gen_suffix():
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=4))

# 处理 TUIC 节点 (确保拥塞控制与多域名完整写入)
tuic_addrs = [{'server': pub_ip, 'server_port': int(os.environ['TUIC_PORT'])}]
if existing_tuic:
    tuic_port = existing_tuic.get('listen_port') or int(os.environ['TUIC_PORT'])
    tuic_addrs = [{'server': pub_ip, 'server_port': tuic_port}]
    if action_mode in ('change_existing_only', 'change_and_fill'):
        tuic_payload = {
            'id': existing_tuic['id'],
            'type': 'tuic',
            'tag': existing_tuic.get('tag') or f"tuic-{gen_suffix()}",
            'tls_id': new_tls_id,
            'listen': '::',
            'listen_port': tuic_port,
            'congestion_control': cc,
            'addrs': tuic_addrs
        }
        api('POST', 'save', {'object': 'inbounds', 'action': 'edit', 'data': json.dumps(tuic_payload)})
        print(f"\033[32m[✓] TUIC 节点 [{existing_tuic.get('tag')}] 已成功更新 (TLS_ID: {new_tls_id}, 拥塞控制: {cc}, 多域名: {pub_ip}:{tuic_port})\033[0m")
else:
    if action_mode in ('create_both', 'change_and_fill'):
        tuic_port = int(os.environ['TUIC_PORT'])
        tuic_tag = f"tuic-{gen_suffix()}"
        tuic_payload = {
            'id': 0,
            'type': 'tuic',
            'tag': tuic_tag,
            'tls_id': new_tls_id,
            'listen': '::',
            'listen_port': tuic_port,
            'congestion_control': cc,
            'addrs': tuic_addrs
        }
        api('POST', 'save', {'object': 'inbounds', 'action': 'new', 'data': json.dumps(tuic_payload)})
        print(f"\033[32m[✓] 已成功创建并上线 TUIC 节点: tag={tuic_tag}, 端口={tuic_port}, 拥塞控制={cc}, 多域名={pub_ip}:{tuic_port}\033[0m")

# 处理 Hysteria2 节点 (确保忽略客户端带宽与多域名完整写入)
hy2_addrs = [{'server': pub_ip, 'server_port': int(os.environ['HY2_PORT'])}]
if existing_hy2:
    hy2_port = existing_hy2.get('listen_port') or int(os.environ['HY2_PORT'])
    hy2_addrs = [{'server': pub_ip, 'server_port': hy2_port}]
    if action_mode in ('change_existing_only', 'change_and_fill'):
        hy2_payload = {
            'id': existing_hy2['id'],
            'type': 'hysteria2',
            'tag': existing_hy2.get('tag') or f"hysteria2-{gen_suffix()}",
            'tls_id': new_tls_id,
            'listen': '::',
            'listen_port': hy2_port,
            'ignore_client_bandwidth': True,
            'addrs': hy2_addrs
        }
        api('POST', 'save', {'object': 'inbounds', 'action': 'edit', 'data': json.dumps(hy2_payload)})
        print(f"\033[32m[✓] Hysteria2 节点 [{existing_hy2.get('tag')}] 已成功更新 (TLS_ID: {new_tls_id}, 忽略客户端带宽: 开启, 多域名: {pub_ip}:{hy2_port})\033[0m")
else:
    if action_mode in ('create_both', 'change_and_fill'):
        hy2_port = int(os.environ['HY2_PORT'])
        hy2_tag = f"hysteria2-{gen_suffix()}"
        hy2_payload = {
            'id': 0,
            'type': 'hysteria2',
            'tag': hy2_tag,
            'tls_id': new_tls_id,
            'listen': '::',
            'listen_port': hy2_port,
            'ignore_client_bandwidth': True,
            'addrs': hy2_addrs
        }
        api('POST', 'save', {'object': 'inbounds', 'action': 'new', 'data': json.dumps(hy2_payload)})
        print(f"\033[32m[✓] 已成功创建并上线 Hysteria2 节点: tag={hy2_tag}, 端口={hy2_port}, 忽略客户端带宽=开启, 多域名={pub_ip}:{hy2_port}\033[0m")

# 3. 重新获取所有最新的 inbound ID
inb_resp2 = api('GET', 'inbounds') or {}
inb_obj2 = inb_resp2.get('obj') or []
if isinstance(inb_obj2, dict):
    inb_rows2 = inb_obj2.get('inbounds') or []
else:
    inb_rows2 = inb_obj2 or []
inb_rows2 = [r for r in inb_rows2 if isinstance(r, dict)]
all_ib_ids = [ib['id'] for ib in inb_rows2 if ib.get('id') is not None]

# 4. 四级优先级精准定位主用户（彻底防止改名后识别不到或新建重名用户）
cli_resp = api('GET', 'clients') or {}
cli_obj = cli_resp.get('obj') or []
if isinstance(cli_obj, dict):
    clients = cli_obj.get('clients') or []
else:
    clients = cli_obj or []
clients = [c for c in clients if isinstance(c, dict)]

target_client = None
# 级别 1: 查找内部数据库 id == 1 的核心账号（用户改名后 ID 仍固定为 1）
target_client = next((c for c in clients if c.get('id') == 1), None)

# 级别 2: 查找名称匹配 admin_name (默认 admin) 的用户
if not target_client:
    target_client = next((c for c in clients if c.get('name') == admin_name), None)

# 级别 3: 若均无，选用当前客户端列表中的首位活跃用户
if not target_client and len(clients) > 0:
    target_client = clients[0]

# 确定最终用户名与客户端 ID
if target_client:
    user_name = target_client.get('name', admin_name)
    user_id = target_client.get('id', 1)
    is_new_user = False
else:
    user_name = admin_name
    user_id = 0
    is_new_user = True

# 合并入站列表（并集，无损保留已绑定的所有节点）
raw_user_inbs = target_client.get('inbounds') if target_client else []
existing_ib_ids = set(raw_user_inbs or [])
merged_inbounds = sorted(list(existing_ib_ids | set(all_ib_ids)))

import uuid

# 补齐或更新全协议凭据
client_pass = os.urandom(8).hex()
client_uuid = str(uuid.uuid4())
client_cfg = target_client.get('config', {}) if (target_client and isinstance(target_client.get('config'), dict)) else {}

if 'vless' not in client_cfg:
    client_cfg['vless'] = {'name': user_name, 'uuid': client_uuid, 'flow': 'xtls-rprx-vision'}
else:
    if isinstance(client_cfg['vless'], dict):
        client_cfg['vless']['name'] = user_name
        if len(str(client_cfg['vless'].get('uuid', ''))) != 36:
            client_cfg['vless']['uuid'] = client_uuid

if 'vmess' not in client_cfg:
    client_cfg['vmess'] = {'name': user_name, 'uuid': client_uuid}
else:
    if isinstance(client_cfg['vmess'], dict):
        client_cfg['vmess']['name'] = user_name
        if len(str(client_cfg['vmess'].get('uuid', ''))) != 36:
            client_cfg['vmess']['uuid'] = client_uuid

if 'tuic' not in client_cfg:
    client_cfg['tuic'] = {'name': user_name, 'uuid': str(uuid.uuid4()), 'password': client_pass}
else:
    if isinstance(client_cfg['tuic'], dict):
        client_cfg['tuic']['name'] = user_name
        if len(str(client_cfg['tuic'].get('uuid', ''))) != 36:
            client_cfg['tuic']['uuid'] = str(uuid.uuid4())

if 'hysteria2' not in client_cfg:
    client_cfg['hysteria2'] = {'name': user_name, 'password': client_pass}
else:
    if isinstance(client_cfg['hysteria2'], dict):
        client_cfg['hysteria2']['name'] = user_name

client_payload = {
    'id': user_id,
    'enable': True,
    'name': user_name,
    'remark': target_client.get('remark', '默认用户') if target_client else '默认用户',
    'config': client_cfg,
    'inbounds': merged_inbounds,
    'links': target_client.get('links', []) if target_client else [],
    'volume': target_client.get('volume', 0) if target_client else 0,
    'expiry': target_client.get('expiry', 0) if target_client else 0,
    'down': target_client.get('down', 0) if target_client else 0,
    'up': target_client.get('up', 0) if target_client else 0,
    'desc': target_client.get('desc', '') if target_client else '',
    'group': target_client.get('group', '') if target_client else '',
    'delayStart': target_client.get('delayStart', False) if target_client else False,
    'autoReset': target_client.get('autoReset', False) if target_client else False,
    'resetDays': target_client.get('resetDays', 0) if target_client else 0,
    'nextReset': target_client.get('nextReset', 0) if target_client else 0,
    'totalUp': target_client.get('totalUp', 0) if target_client else 0,
    'totalDown': target_client.get('totalDown', 0) if target_client else 0,
    'createdAt': target_client.get('createdAt', 0) if target_client else 0,
    'onlineAt': target_client.get('onlineAt', 0) if target_client else 0
}
save_cli_res = api('POST', 'save', {'object': 'clients', 'action': 'new' if is_new_user else 'edit', 'data': json.dumps(client_payload)})
if not save_cli_res.get('success'):
    print(f"\033[31m[!] 关联用户失败: {save_cli_res.get('msg')}\033[0m")
else:
    print(f"\033[32m[✓] 主用户 [{user_name}] (ID: {user_id or 1}) 已成功关联绑定全部入站节点。\033[0m")
PYEOF
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

      local cf_desc
      if [[ "$cf_st" == "active" ]]; then
        cf_desc="${G}运行中${N}"
      else
        cf_desc="${R}已停止 [${cf_st}]${N}"
      fi

      local caddy_desc
      if [[ "$st" == "active" ]]; then
        caddy_desc="${G}运行中${N}"
      else
        caddy_desc="${R}已停止 [${st}]${N}"
      fi

      echo -e "  反代状态:      ${G}已开启 (Cloudflare 隧道模式)${N}"
      echo -e "  隧道服务:      ${cf_desc}"
      echo -e "  Caddy 服务:    ${caddy_desc}"
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
    echo -e "   1) 启动/重启服务     2) 停止服务"
    echo -e "   3) 查看运行日志      4) 重置访问口令"
    echo -e "   5) 重置访问路径      6) 面板 URL 设置"
    echo -e "   7) SSL / HTTPS 设置  8) 修改面板监听地址和端口"
    echo -e "   9) Cloudflare隧道/Caddy配置"
    echo -e "  10) 申请 Cloudflare SSL 证书"
    echo -e "  11) 创建 TUIC / Hysteria2 节点"
    echo -e "  12) 检查/更新版本    13) 卸载"
    echo -e "   0) 退出脚本"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-13]: " choice

    case "$choice" in
      1) svc_restart && echo -e "\n  ${G}服务已启动/重启${N}"; pause ;;
      2) svc_stop    && echo -e "\n  ${Y}已停止${N}"; pause ;;
      3) echo; svc_logs 40; pause ;;
      4) reset_password; pause ;;
      5) reset_basepath; pause ;;
      6) change_panel_url; pause ;;
      7) change_ssl; pause ;;
      8) change_listen_and_port; pause ;;
      9) caddy_menu; pause ;;
      10) cf_ssl_menu ;;
      11) create_tuic_hy2_nodes; pause ;;
      12) check_and_update; pause ;;
      13) do_uninstall; pause ;;
      0) exit 0 ;;
      *) ;;
    esac
  done
}

need_root
apply_sysctl_optimization

case "${1:-}" in
  setup_tunnel|setup_caddy)
    shift
    setup_caddy_proxy "$@"
    ;;
  start|restart) svc_restart ;;
  stop)      svc_stop ;;
  status)    show_info ;;
  log)       svc_logs_follow ;;
  info)      show_info ;;
  listen|port) change_listen_and_port ;;
  url)       change_panel_url ;;
  ssl)       change_ssl ;;
  caddy|cf|tunnel) caddy_menu ;;
  reload_caddy) reload_caddy_proxy ;;
  cert|ssl_cf|acme|cf_ssl) cf_ssl_menu ;;
  view_cert) view_cf_ssl_certs ;;
  apply_cert) apply_cf_ssl_cert ;;
  tuic|hy2|create_node|create_nodes)
    shift
    create_tuic_hy2_nodes "$@"
    ;;
  token) get_or_create_sui_token ;;
  update)    check_and_update ;;
  upgrade)   check_and_update ;;
  uninstall) do_uninstall ;;
  "")        menu ;;
  *)
    echo "用法: sout [start|stop|restart|status|log|info|listen|port|url|ssl|caddy|cert|tuic|hy2|update|uninstall]"
    echo "直接在终端输入 sout 即可进入交互控制菜单"
    ;;
esac

