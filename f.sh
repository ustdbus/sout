#!/usr/bin/env bash
set -e

# ==============================================================================
# 统一 JSON 字段读取：替代 grep -oE/cut/awk 解析（键不存在或解析失败时输出空，由调用点 :- 兜底）
# ==============================================================================
json_get() {
  # 交给 Go 子命令解析（不再依赖 python3）。
  # 用绝对路径兜底：本函数可能在 $BIN 赋值之前就被调用。
  local _bin="/usr/local/bin/sout-server"
  [[ -x "$_bin" ]] || _bin="/usr/local/bin/sout"
  [[ -x "$_bin" ]] || _bin="/usr/local/bin/fanout"
  "$_bin" json get "${1:-}" "${2:-}" 2>/dev/null || printf '%s\n' ""
}

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

detect_adaptive_mem_tuning() {
  local mem_kb=0
  local swap_kb=0
  if [[ -f /proc/meminfo ]]; then
    mem_kb=$(grep -i 'MemTotal' /proc/meminfo 2>/dev/null | awk '{print $2}')
    swap_kb=$(grep -i 'SwapTotal' /proc/meminfo 2>/dev/null | awk '{print $2}')
  fi

  local cg_bytes=0
  for cg_path in /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory/memory.limit_in_bytes /sys/fs/cgroup/memory.limit_in_bytes; do
    if [[ -f "$cg_path" ]]; then
      local val
      val=$(cat "$cg_path" 2>/dev/null || true)
      if [[ "$val" =~ ^[0-9]+$ ]] && [[ "$val" -lt 1099511627776 ]]; then
        cg_bytes="$val"
        break
      fi
    fi
  done

  local sys_bytes=0
  if [[ -n "$mem_kb" && "$mem_kb" -gt 0 ]]; then
    sys_bytes=$(( mem_kb * 1024 ))
  fi

  local min_bytes=0
  if [[ "$sys_bytes" -gt 0 && "$cg_bytes" -gt 0 ]]; then
    if [[ "$sys_bytes" -lt "$cg_bytes" ]]; then
      min_bytes="$sys_bytes"
    else
      min_bytes="$cg_bytes"
    fi
  elif [[ "$sys_bytes" -gt 0 ]]; then
    min_bytes="$sys_bytes"
  elif [[ "$cg_bytes" -gt 0 ]]; then
    min_bytes="$cg_bytes"
  fi

  local mem_mb=0
  if [[ "$min_bytes" -gt 0 ]]; then
    mem_mb=$(( min_bytes / 1024 / 1024 ))
  fi

  HAS_SWAP=0
  if [[ -n "$swap_kb" && "$swap_kb" =~ ^[0-9]+$ && "$swap_kb" -ge 32768 ]]; then
    HAS_SWAP=1
  fi
  AUTO_HAS_SWAP="$HAS_SWAP"

  AUTO_MEM_MB="$mem_mb"
  AUTO_GOMEMLIMIT=""
  AUTO_GOGC=""
  if [[ "$HAS_SWAP" -eq 1 ]]; then
    # 德邦模式 (有 Swap 气囊)：不施加 GOMEMLIMIT 软限制，GOGC 默认 100 释放极致吞吐
    AUTO_GOMEMLIMIT=""
    AUTO_GOGC=""
  elif [[ "$mem_mb" -gt 0 && "$mem_mb" -le 180 ]]; then
    # 阿尔法安全模式 (仅针对 128M 左右且无 Swap 的机器，预留 >=15% 物理内存防爆隔离区，收紧 GOGC 至 60 避免堆瞬时翻倍溢出)
    AUTO_GOMEMLIMIT="18MiB"
    AUTO_GOGC="60"
  fi
}

# 在 OpenRC/Alpine 上自动把 systemctl 调用翻译为 rc-service / rc-update。
# 优先从 systemd unit 生成 OpenRC init 脚本，保证 cloudflared/sing-box 可被管理。

# ==============================================================================
# 日志轮转：给 sing-box / cloudflared / s-ui 的落盘日志加上大小上限
# 这些服务的 init 由各自安装器生成，sout 不覆盖它们，统一在这里兜底。
# 优先用系统 logrotate（Debian 自带），否则退化为自建脚本 + crond（Alpine/busybox）。
# ==============================================================================
SOUT_LOGROTATE_D="/etc/logrotate.d/sout-services"
SOUT_ROTATE_SCRIPT="/usr/local/bin/sout-logrotate"

install_log_rotation() {
  # 三个服务都已在某个 logrotate 规则里时才认为无需重复配置
  if command -v logrotate >/dev/null 2>&1 && command -v grep >/dev/null 2>&1; then
    if grep -rqls 'sing-box' /etc/logrotate.d/ 2>/dev/null &&
       grep -rqls 'cloudflared' /etc/logrotate.d/ 2>/dev/null &&
       grep -rqls 's-ui' /etc/logrotate.d/ 2>/dev/null; then
      return 0
    fi
  elif [[ -x "/usr/local/bin/sout-logrotate" ]]; then
    return 0
  fi

  if command -v logrotate >/dev/null 2>&1; then
    mkdir -p /etc/logrotate.d
    cat > "$SOUT_LOGROTATE_D" <<'ROTEOF'
/var/log/sing-box.log /var/log/sing-box.err /var/log/cloudflared.log /var/log/cloudflared.err /var/log/s-ui.log /var/log/s-ui.err {
    size 4M
    rotate 1
    missingok
    notifempty
    copytruncate
    compress
}
ROTEOF
    logrotate -f "$SOUT_LOGROTATE_D" >/dev/null 2>&1 || true
    echo "  [✓] 已为 sing-box / cloudflared / s-ui 配置 logrotate 日志轮转 (4MB x 1 份)"
    return 0
  fi

  cat > "$SOUT_ROTATE_SCRIPT" <<'ROTEOF'
#!/bin/sh
# sout 日志轮转：单文件超过 4MB 时滚动为 .1（保留 1 份），由 crond 周期调用。
MAX=$((4 * 1024 * 1024))
for f in /var/log/sing-box.log /var/log/sing-box.err \
         /var/log/cloudflared.log /var/log/cloudflared.err \
         /var/log/s-ui.log /var/log/s-ui.err \
         /var/log/sout.log /var/log/sout.err; do
  [ -f "$f" ] || continue
  size=$(wc -c < "$f" 2>/dev/null || echo 0)
  [ "$size" -gt "$MAX" ] || continue
  cat "$f" > "$f.1" 2>/dev/null || continue
  : > "$f"
done
ROTEOF
  chmod 755 "$SOUT_ROTATE_SCRIPT" 2>/dev/null || true

  local cron_line="*/30 * * * * $SOUT_ROTATE_SCRIPT >/dev/null 2>&1"
  if [[ -d /etc/cron.d ]]; then
    echo "$cron_line" > /etc/cron.d/sout-logrotate 2>/dev/null || true
    chmod 644 /etc/cron.d/sout-logrotate 2>/dev/null || true
  elif [[ -d /etc/crontabs ]]; then
    grep -q "sout-logrotate" /etc/crontabs/root 2>/dev/null || \
      echo "$cron_line" >> /etc/crontabs/root 2>/dev/null || true
  fi
  if command -v rc-service >/dev/null 2>&1; then
    rc-service crond start >/dev/null 2>&1 || true
  fi
  echo "  [✓] 已配置日志轮转脚本 (4MB x 1 份，每 30 分钟检查): $SOUT_ROTATE_SCRIPT"
}


# ==============================================================================
# s-ui 日志落盘：s-ui 官方 OpenRC init 未配置 output_log/error_log，
# 导致 Alpine 上「s-ui 日志」一栏为空。这里幂等地把日志路径补进它的 init 脚本。
# ==============================================================================
ensure_sui_log_redirect() {
  [[ -f /etc/init.d/s-ui ]] || return 0
  # systemd 平台由 journald 统一收取，无需落盘
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    return 0
  fi
  if grep -q 'output_log=' /etc/init.d/s-ui 2>/dev/null; then
    return 0
  fi
  cp -a /etc/init.d/s-ui "/etc/init.d/s-ui.bak-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
  if grep -q '^command_args=' /etc/init.d/s-ui 2>/dev/null; then
    sed -i '/^command_args=/a output_log="/var/log/s-ui.log"\nerror_log="/var/log/s-ui.err"' /etc/init.d/s-ui 2>/dev/null || true
  fi
  if ! grep -q 'output_log=' /etc/init.d/s-ui 2>/dev/null; then
    printf '\noutput_log="/var/log/s-ui.log"\nerror_log="/var/log/s-ui.err"\n' >> /etc/init.d/s-ui
  fi
  chmod +x /etc/init.d/s-ui 2>/dev/null || true
  echo "  [✓] 已为 s-ui 补全日志落盘: /var/log/s-ui.log /var/log/s-ui.err（原脚本已备份）"
}

_openrc_init_from_unit() {
  local name="$1"
  local unit="/etc/systemd/system/${name}.service"
  local init="/etc/init.d/${name}"
  local exec_start command command_args
  # 非 cloudflared/sing-box 已有 OpenRC 脚本时不要覆盖（如 sout/s-ui）
  if [[ -x "$init" && "$name" != "cloudflared" && "$name" != "sing-box" ]]; then
    return 0
  fi
  if [[ "$name" == "sing-box" && ! -f "$unit" ]]; then
    command="/usr/local/bin/sing-box"
    command_args="run -c /etc/sing-box/config.json"
  else
    [[ -f "$unit" ]] || return 1
    exec_start=$(sed -n 's/^ExecStart=//p' "$unit" | head -1)
    [[ -z "$exec_start" ]] && return 1
    command="${exec_start%% *}"
    command_args="${exec_start#* }"
    [[ -z "$command" ]] && return 1
  fi

  detect_adaptive_mem_tuning
  local env_export=""
  if [[ "$HAS_SWAP" -eq 1 || "$AUTO_MEM_MB" -gt 180 ]]; then
    env_export=""
  else
    local mlimit="18MiB"
    local ggc="50"
    if [[ "$name" == "sing-box" ]]; then
      mlimit="35MiB"
      ggc="60"
    elif [[ "$name" == "cloudflared" ]]; then
      mlimit="35MiB"
      ggc="60"
    fi
    if [[ -n "$mlimit" ]]; then
      env_export="export GOMEMLIMIT=\"${mlimit}\"
export GOGC=\"${ggc}\""
    fi
  fi

  local supervisor_line="command_background=\"yes\""
  if command -v supervise-daemon >/dev/null 2>&1; then
    supervisor_line="supervisor=\"supervise-daemon\""
  fi

  mkdir -p /var/log
  cat > "$init" <<EOF
#!/sbin/openrc-run
name="$name"
description="$name service"
${env_export}
${supervisor_line}
command="$command"
command_args="$command_args"
output_log="/var/log/${name}.log"
error_log="/var/log/${name}.log"
respawn_delay=2
respawn_max=0

depend() {
    need net
    after firewall
}

start_pre() {
  if [ -n "\$command" ]; then
    local bin_name
    bin_name="\$(basename "\$command")"
    pkill -9 -x "\$bin_name" 2>/dev/null || true
  fi
  if [ -f "/etc/sing-box/config.json" ] && [ "\$name" = "sing-box" ]; then
    for p in \$(grep -oE '"listen_port"[[:space:]]*:[[:space:]]*[0-9]+' /etc/sing-box/config.json 2>/dev/null | grep -oE '[0-9]+'); do
      fuser -k -n tcp "\$p" 2>/dev/null || true
    done
  fi
  if [ -f "\$pidfile" ]; then
    local p
    p=\$(cat "\$pidfile" 2>/dev/null)
    if [ -n "\$p" ] && ! kill -0 "\$p" 2>/dev/null; then
      rm -f "\$pidfile"
    fi
  fi
}

stop_post() {
  rm -f "\$pidfile"
  if [ -n "\$command" ]; then
    pkill -9 -x "\$(basename "\$command")" 2>/dev/null || true
  fi
}
EOF
  chmod +x "$init"
}

kill_port() {
  local port="$1"
  [[ -z "$port" ]] && return 0
  if command -v fuser >/dev/null 2>&1; then
    fuser -k -9 "${port}/tcp" >/dev/null 2>&1 || true
  elif command -v ss >/dev/null 2>&1; then
    local pids
    pids=$(ss -tlpn "sport = :${port}" 2>/dev/null | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u)
    if [[ -n "$pids" ]]; then
      echo "$pids" | xargs -r kill -9 >/dev/null 2>&1 || true
    fi
  elif command -v lsof >/dev/null 2>&1; then
    lsof -ti ":${port}" 2>/dev/null | xargs -r kill -9 >/dev/null 2>&1 || true
  fi
}

_openrc_clean_ports() {
  local name="$1"
  case "$name" in
    sout|fanout)
      local p="8899"
      if [[ -f "/var/lib/sout/settings.json" ]]; then
        local sp
        sp=$(json_get "/var/lib/sout/settings.json" port)
        [[ -n "$sp" ]] && p="$sp"
      fi
      kill_port "$p"
      ;;
  esac
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
      pkill -9 -f "sout-quick-tunnel" 2>/dev/null || true
      ;;
    sing-box)
      pkill -9 -f "/usr/local/bin/sing-box" 2>/dev/null || true
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
      if [[ "$name" == "caddy" || "$name" == "cloudflared" || "$name" == "sing-box" ]]; then
        _openrc_init_from_unit "$name" || true
      fi
      if [[ -x /etc/init.d/${name} ]]; then
        if [[ "$action" == "restart" ]]; then
          rc-service "$name" stop >/dev/null 2>&1 || true
          rc-service "$name" zap >/dev/null 2>&1 || true
          _openrc_force_stop "$name"
          _openrc_clean_ports "$name"
          sleep 0.3
          rc-service "$name" start
        elif [[ "$action" == "stop" ]]; then
          rc-service "$name" stop >/dev/null 2>&1 || true
          rc-service "$name" zap >/dev/null 2>&1 || true
          _openrc_force_stop "$name"
        else
          rc-service "$name" zap >/dev/null 2>&1 || true
          _openrc_force_stop "$name"
          _openrc_clean_ports "$name"
          sleep 0.2
          rc-service "$name" start
        fi
      else
        return 0
      fi
      ;;
    enable)
      if [[ "$name" == "caddy" || "$name" == "cloudflared" || "$name" == "sing-box" ]]; then
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

# 平台(容器/NAT机)是否锁定了内核参数：尝试按原值回写，成功才说明有权限。
# 用相同的值写入不会真正改变内核状态，因此是安全的探测方式。
sysctl_writable() {
  local key="$1" path orig
  path="/proc/sys/$(echo "$key" | tr '.' '/')"
  [[ -w "$path" ]] || return 1
  orig=$(sysctl -n "$key" 2>/dev/null) || return 1
  [[ -n "$orig" ]] || return 1
  sysctl -w "${key}=${orig}" >/dev/null 2>&1
}

apply_sysctl_optimization() {
  mkdir -p "$WORK_DIR" /etc/sysctl.d 2>/dev/null || true

  # 探测平台是否允许改内核参数：尝试按原值回写，能写通才算有权限。
  # 用相同值写入不会真正改变内核状态，因此是安全的探测方式。
  local probe_ok=0 pk
  for pk in net.core.rmem_max net.ipv4.tcp_congestion_control net.core.somaxconn; do
    if sysctl_writable "$pk"; then
      probe_ok=1
      break
    fi
  done

  local mem_total_mb=512
  if [[ -f /proc/meminfo ]]; then
    local mem_kb
    mem_kb=$(grep -i 'MemTotal' /proc/meminfo | awk '{print $2}')
    [[ -n "$mem_kb" && "$mem_kb" -gt 0 ]] && mem_total_mb=$(( mem_kb / 1024 ))
  fi

  if [[ $probe_ok -eq 0 ]]; then
    echo "      检测到内核参数被平台锁定（容器/NAT 机型），跳过 sysctl 调优以免留下无效配置"
    echo "      仍需防爆保护，继续执行内存自适应..."
    optimize_low_memory "$mem_total_mb"
    return 0
  fi

  # 1. 备份原系统参数（仅首次备份，避免覆盖原始值）
  if [[ ! -f "$SYSCTL_BACKUP" ]]; then
    local keys=(
      "net.core.rmem_max"
      "net.core.wmem_max"
      "net.core.rmem_default"
      "net.core.wmem_default"
      "net.core.netdev_max_backlog"
      "net.core.somaxconn"
      "net.ipv4.tcp_max_syn_backlog"
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

  # 2. 目标值：采用代理社区推荐
  #    Hysteria2 官方文档：QUIC 场景把 rmem_max / wmem_max 都设为 16MB
  #    Queqiao 调优脚本：quic-go 会请求 8MiB UDP 缓冲，上限需留余量；
  #                      并建议 somaxconn=8192、netdev_max_backlog=16384、tcp_max_syn_backlog=8192
  #    说明：这几个是「上限/队列」参数，设大不会预分配内存，对 128MB 小机同样安全
  local rmem_max=16777216
  local wmem_max=16777216
  local rmem_default=1048576
  local wmem_default=1048576
  local netdev_backlog=16384
  local somaxconn=8192
  local syn_backlog=8192
  local udp_mem="4096 87380 16777216"
  local udp_min=8192
  # 极低内存机器上收紧 UDP 全局内存池（该项直接影响内存占用，与上限型参数不同）
  if [[ $mem_total_mb -le 256 ]]; then
    udp_mem="2048 4096 8192"
  fi

  # 3. 尝试加载 BBR 模块
  modprobe tcp_bbr >/dev/null 2>&1 || true

  # 4. 逐项探测：只把平台允许修改的项写进配置，避免留下永远读不回来的声明
  local conf_content="# === SOUT SYSCTL START ==="
  local skipped=""
  local pairs=(
    "net.core.rmem_max=${rmem_max}"
    "net.core.wmem_max=${wmem_max}"
    "net.core.rmem_default=${rmem_default}"
    "net.core.wmem_default=${wmem_default}"
    "net.core.netdev_max_backlog=${netdev_backlog}"
    "net.core.somaxconn=${somaxconn}"
    "net.ipv4.tcp_max_syn_backlog=${syn_backlog}"
    "net.ipv4.udp_mem=${udp_mem}"
    "net.ipv4.udp_rmem_min=${udp_min}"
    "net.ipv4.udp_wmem_min=${udp_min}"
    "net.core.default_qdisc=fq"
    "net.ipv4.tcp_congestion_control=bbr"
  )
  local pair k v
  for pair in "${pairs[@]}"; do
    k="${pair%%=*}"
    v="${pair#*=}"
    if sysctl_writable "$k"; then
      conf_content="${conf_content}
${k} = ${v}"
    else
      skipped="${skipped}${k} "
    fi
  done
  conf_content="${conf_content}
# === SOUT SYSCTL END ==="

  if [[ -n "$skipped" ]]; then
    echo "      以下内核参数被平台锁定，已跳过（不会写入配置）:"
    for k in $skipped; do echo "        - ${k}"; done
  fi

  # 5. 写入独立配置文件与 /etc/sysctl.conf
  echo "$conf_content" > /etc/sysctl.d/99-sout.conf 2>/dev/null || true
  if [[ -f /etc/sysctl.conf ]]; then
    sed -i '/# === SOUT SYSCTL START ===/,/# === SOUT SYSCTL END ===/d' /etc/sysctl.conf 2>/dev/null || true
    echo "$conf_content" >> /etc/sysctl.conf 2>/dev/null || true
  fi
  sysctl -p /etc/sysctl.d/99-sout.conf >/dev/null 2>&1 || sysctl -p >/dev/null 2>&1 || true

  # 6. 回读校验
  echo "$conf_content" | grep -E '^net\.' | sed 's/[[:space:]]*=[[:space:]]*/ /' > /tmp/.sout_sysctl_kv
  local ok=0 locked=""
  while IFS=' ' read -r k v; do
    [[ -n "$k" ]] || continue
    local p cur
    p="/proc/sys/$(echo "$k" | tr '.' '/')"
    # udp_mem 这类多值参数内核以制表符分隔，先统一成单空格再比较
    cur=$(cat "$p" 2>/dev/null | tr -s ' \t' ' ' | sed 's/^ *//; s/ *$//')
    if [[ "$cur" == "$v" ]]; then
      ok=$(( ok + 1 ))
    else
      locked="${locked}\n${k}"
    fi
  done < /tmp/.sout_sysctl_kv
  rm -f /tmp/.sout_sysctl_kv

  if [[ -n "$locked" ]]; then
    echo "      sysctl 已生效 ${ok} 项；以下项写入后仍被覆盖:"
    echo "$locked" | sed 's/^\\n//; s/\\n/\n        - /g; s/^/        - /'
  else
    echo "      sysctl 调优已全部生效（${ok} 项）"
  fi

  # 7. 低内存 VPS/容器防爆保护
  optimize_low_memory "$mem_total_mb"
}

optimize_low_memory() {
  detect_adaptive_mem_tuning
  local mem_mb="${AUTO_MEM_MB:-${1:-512}}"
  local has_swap="${AUTO_HAS_SWAP:-0}"

  # 1. 清理 /tmp 内存文件系统历史残留的 tar.gz 与二进制
  rm -f /tmp/sout-linux-*.tar.gz /tmp/sout-server /tmp/fanout /tmp/f.sh 2>/dev/null || true

  # 2. 彻底清除历史强加的 MemorySwapMax=0，避免突发瞬时尖峰因 0 容错直接触发内核 OOM Killer
  sed -i '/MemorySwapMax/d' /etc/systemd/system/*.service.d/override.conf 2>/dev/null || true

  # 3. 清理 OpenRC /etc/init.d/ 历史遗留硬编码的 export GOMEMLIMIT 与 export GOGC
  for s in cloudflared sing-box caddy sout fanout s-ui; do
    if [[ -f "/etc/init.d/${s}" ]]; then
      sed -i '/export GOMEMLIMIT/d' "/etc/init.d/${s}" 2>/dev/null || true
      sed -i '/export GOGC/d' "/etc/init.d/${s}" 2>/dev/null || true
    fi
  done

  # 4. 调低 swappiness（从默认 100 降为 30），防止过早向虚拟 Swap 剧烈换页
  sysctl -w vm.swappiness=30 >/dev/null 2>&1 || true

  # 5. 限制 journald 运行时内存
  if [[ -d /run/systemd/system ]]; then
    mkdir -p /etc/systemd/journald.conf.d 2>/dev/null || true
    cat > /etc/systemd/journald.conf.d/00-mem-limit.conf <<'EOF'
[Journal]
RuntimeMaxUse=8M
SystemMaxUse=8M
MaxRetentionSec=3day
EOF
    systemctl restart systemd-journald >/dev/null 2>&1 || true
  fi

  # 分支 A：若存在有效 Swap (has_swap=1，德邦模式)
  if [[ "$has_swap" -eq 1 ]]; then
    echo "      检测到有效 Swap 缓冲，启用零限制全速原生 Go 运行时配置 (德邦模式)"
    if [[ -d /run/systemd/system ]]; then
      for svc in cloudflared sing-box caddy sout s-ui; do
        if [[ -f "/etc/systemd/system/${svc}.service.d/override.conf" ]]; then
          sed -i '/GOMEMLIMIT/d' "/etc/systemd/system/${svc}.service.d/override.conf" 2>/dev/null || true
          sed -i '/GOGC/d' "/etc/systemd/system/${svc}.service.d/override.conf" 2>/dev/null || true
          if ! grep -qE 'Environment|Exec|Limit' "/etc/systemd/system/${svc}.service.d/override.conf" 2>/dev/null; then
            rm -f "/etc/systemd/system/${svc}.service.d/override.conf" 2>/dev/null || true
          fi
        fi
      done
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi

    if [[ -f /etc/alpine-release ]] || command -v rc-service >/dev/null 2>&1; then
      for svc in cloudflared sing-box caddy sout s-ui; do
        if [[ -f "/etc/conf.d/${svc}" ]]; then
          sed -i '/GOMEMLIMIT/d' "/etc/conf.d/${svc}" 2>/dev/null || true
          sed -i '/GOGC/d' "/etc/conf.d/${svc}" 2>/dev/null || true
        fi
      done
    fi
    return 0
  fi

  # 分支 B：若不存在有效 Swap (has_swap=0，阿尔法安全模式，仅针对 128M 左右即 <=180MB 机型，收紧 GOGC 至 60 确保留足 >=15% 物理内存防爆隔离区；>180MB 则保持原生默认不设限)
  if [[ $mem_mb -le 180 ]]; then
    echo "      检测到无 Swap 极小内存环境 (${mem_mb} MB <= 180 MB)，为确保留足 15% 系统安全防爆余量，启用精细分层内存防护 (阿尔法安全模式)"

    local cf_memlimit="35MiB"
    local cf_gogc="60"
    local sb_memlimit="35MiB"
    local sb_gogc="60"
    local aux_memlimit="18MiB"
    local aux_gogc="50"

    # 1. systemd 环境注入
    if [[ -d /run/systemd/system ]]; then
      mkdir -p /etc/systemd/system/sing-box.service.d 2>/dev/null || true
      cat > /etc/systemd/system/sing-box.service.d/override.conf <<EOF
[Service]
Environment="GOMEMLIMIT=${sb_memlimit}"
Environment="GOGC=${sb_gogc}"
EOF

      mkdir -p /etc/systemd/system/cloudflared.service.d 2>/dev/null || true
      cat > /etc/systemd/system/cloudflared.service.d/override.conf <<EOF
[Service]
Environment="GOMEMLIMIT=${cf_memlimit}"
Environment="GOGC=${cf_gogc}"
EOF

      rm -rf /etc/systemd/system/caddy.service.d 2>/dev/null || true

      for svc in sout s-ui; do
        mkdir -p "/etc/systemd/system/${svc}.service.d" 2>/dev/null || true
        cat > "/etc/systemd/system/${svc}.service.d/override.conf" <<EOF
[Service]
Environment="GOMEMLIMIT=${aux_memlimit}"
Environment="GOGC=${aux_gogc}"
EOF
      done
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi

    # 2. OpenRC 环境注入
    if [[ -f /etc/alpine-release ]] || command -v rc-service >/dev/null 2>&1; then
      rm -f /etc/conf.d/caddy 2>/dev/null || true
      for svc in sing-box cloudflared sout s-ui; do
        local cur_limit="$aux_memlimit"
        local cur_gc="$aux_gogc"
        if [[ "$svc" == "sing-box" ]]; then
          cur_limit="$sb_memlimit"; cur_gc="$sb_gogc"
        elif [[ "$svc" == "cloudflared" ]]; then
          cur_limit="$cf_memlimit"; cur_gc="$cf_gogc"
        fi

        mkdir -p /etc/conf.d 2>/dev/null || true
        cat > "/etc/conf.d/${svc}" <<EOF
export GOMEMLIMIT="${cur_limit}"
export GOGC="${cur_gc}"
EOF
      done
    fi
  fi
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
    p=$(json_get "$WORK_DIR/settings.json" port)
    [[ -n "$p" ]] && { echo "$p"; return; }
  fi
  echo "$DEFAULT_PORT"
}

web_listen_addr() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local a
    a=$(json_get "$WORK_DIR/settings.json" listen_addr)
    [[ -n "$a" ]] && { echo "$a"; return; }
  fi
  echo "0.0.0.0"
}

web_panel_url() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local u
    u=$(json_get "$WORK_DIR/settings.json" panel_url)
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
    d=$(json_get "$WORK_DIR/settings.json" ssl_domain)
    [[ -n "$d" ]] && { echo "$d"; return; }
  fi
  echo ""
}

web_ssl_cert() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local c
    c=$(json_get "$WORK_DIR/settings.json" ssl_cert)
    [[ -n "$c" ]] && { echo "$c"; return; }
  fi
  echo ""
}

web_ssl_key() {
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    local k
    k=$(json_get "$WORK_DIR/settings.json" ssl_key)
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
  ip=$(curl -s4m 3 https://checkip.amazonaws.com || curl -s4m 3 https://api.ipify.org || curl -s4m 3 https://ifconfig.me || echo "127.0.0.1")
  echo "$ip"
}

pause() {
  echo
  read -rp "  按回车键继续..." _
}

CADDY_META="${WORK_DIR}/caddy_meta.json"

is_sui_backend() {
  local m
  m=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
  if [[ "$m" == "sing-box" ]]; then
    return 1
  elif [[ "$m" == "s-ui" ]]; then
    return 0
  fi
  if [[ -f /usr/local/s-ui/db/s-ui.db ]] || [[ -f /usr/local/s-ui/s-ui ]] || command -v sui >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

get_sui_user() {
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local u="admin"
  if [[ -f "$sui_db" ]]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      u=$(sqlite3 "$sui_db" "SELECT username FROM users LIMIT 1;" 2>/dev/null || echo "admin")
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
  local st c_en pw cur_ver cur_backend
  c_en=$(is_caddy_enabled)
  pw=$(web_password)
  st=$(svc_status)

  # 1. 优先从轻量静态文件读取版本，避免每次唤起重型 Go 二进制
  if [[ -f "${WORK_DIR}/version" ]]; then
    cur_ver=$(cat "${WORK_DIR}/version" 2>/dev/null | tr -d ' \r\n')
  fi
  if [[ -z "$cur_ver" ]] && command -v sout-server >/dev/null 2>&1; then
    cur_ver=$(sout-server -version 2>/dev/null | awk '{print $2}' | tr -d ' \r\n')
    [[ -n "$cur_ver" ]] && echo "$cur_ver" > "${WORK_DIR}/version" 2>/dev/null || true
  fi
  [[ -z "$cur_ver" ]] && cur_ver="dev"

  echo
  echo -e "  程序版本:    ${G}${cur_ver}${N}"
  if [[ "$st" == "active" ]]; then
    echo -e "  服务状态:    ${G}运行中 (active)${N}"
  else
    echo -e "  服务状态:    ${R}已停止 (${st})${N}"
  fi

  cur_backend=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
  if [[ -z "$cur_backend" ]]; then
    if [[ -f /usr/local/s-ui/db/s-ui.db ]] || [[ -f /usr/local/s-ui/s-ui ]] || command -v sui >/dev/null 2>&1; then
      cur_backend="s-ui"
    elif [[ -f /etc/sing-box/config.json ]] || command -v sing-box >/dev/null 2>&1; then
      cur_backend="sing-box"
    fi
    [[ -n "$cur_backend" ]] && echo -n "$cur_backend" > "${WORK_DIR}/panel_mode" 2>/dev/null || true
  fi

  if [[ "$cur_backend" == "sing-box" ]]; then
    local sb_ver=""
    if [[ -f "${WORK_DIR}/singbox_version" ]]; then
      sb_ver=$(cat "${WORK_DIR}/singbox_version" 2>/dev/null | tr -d ' \r\n')
    fi
    if [[ -z "$sb_ver" ]]; then
      sb_ver=$(/usr/local/bin/sing-box version 2>/dev/null | head -1 | awk '{print $3}' || echo "原生内核")
      [[ -n "$sb_ver" && "$sb_ver" != "原生内核" ]] && echo "$sb_ver" > "${WORK_DIR}/singbox_version" 2>/dev/null || true
    fi
    echo -e "  后端对接:    ${G}sing-box (${sb_ver}) 原生内核已就绪${N}"
  elif [[ "$cur_backend" == "s-ui" ]]; then
    echo -e "  后端对接:    ${G}s-ui (Sing-Box) 已就绪${N}"
  else
    echo -e "  后端对接:    ${R}未检测到后端 (s-ui / sing-box)${N}"
  fi

  if [[ "$c_en" == "true" ]]; then
    local c_mode="tunnel" c_dom="" c_sout_p="sout" c_sui_p="sui" c_sub_p="sub" c_tun_p="8081"
    if [[ -f "$CADDY_META" ]]; then
      c_mode=$(json_get "$CADDY_META" mode)
      [[ -z "$c_mode" ]] && c_mode="tunnel"
      c_dom=$(json_get "$CADDY_META" domain)
      c_sout_p=$(json_get "$CADDY_META" sout_path)
      [[ -z "$c_sout_p" ]] && c_sout_p="sout"
      c_sui_p=$(json_get "$CADDY_META" sui_path)
      [[ -z "$c_sui_p" ]] && c_sui_p="sui"
      c_sub_p=$(json_get "$CADDY_META" sub_path)
      [[ -z "$c_sub_p" ]] && c_sub_p="sub"
      local tp
      tp=$(json_get "$CADDY_META" tunnel_port)
      [[ -n "$tp" ]] && c_tun_p="$tp"
    fi

    local cf_st="${R}未运行${N}"
    if [[ "$INIT_SYS" == "systemd" ]]; then
      if [[ $(systemctl is-active cloudflared 2>/dev/null || echo "") == "active" ]]; then
        cf_st="${G}运行中 (active)${N}"
      fi
    else
      if rc-service cloudflared status >/dev/null 2>&1 || pgrep -f "cloudflared" >/dev/null 2>&1; then
        cf_st="${G}运行中 (active)${N}"
      fi
    fi

    # 如果是临时隧道，且记录域名失效/需要更新时，动态从 journalctl 抓取最新域名
    if [[ "$c_mode" == "quick_tunnel" ]]; then
      local real_d
      real_d=$(journalctl -u cloudflared -n 50 --no-pager 2>/dev/null | grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' | tail -1 | sed 's|https://||' | tr -d ' \r\n')
      [[ -n "$real_d" ]] && c_dom="$real_d"
      echo -e "  反代模式:    ${G}Cloudflare 官方免费临时隧道连接与轻量流量分流 (已开启)${N}"
    else
      echo -e "  反代模式:    ${G}Cloudflare 隧道连接与轻量流量分流 (已开启)${N}"
    fi

    echo -e "  隧道服务:    ${cf_st} (本地回源: 127.0.0.1:${c_tun_p})"
    echo -e "  管理面板:    ${B}https://${c_dom}/${c_sout_p}/${N}"
    echo -e "  访问口令:    ${Y}${pw}${N}"
    if is_sui_backend && [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
      local sui_u
      sui_u=$(get_sui_user)
      echo -e "  s-ui 面板:   ${B}https://${c_dom}/${c_sui_p}/${N}"
      echo -e "  s-ui 用户名: ${Y}${sui_u}${N}"
      echo -e "  s-ui 密  码: ${D}[由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]${N}"
    else
      echo -e "  核心配置:    ${B}/etc/sing-box/config.json${N}"
    fi
    echo -e "  订阅链接:    ${B}https://${c_dom}/${c_sout_p}/sub=${pw}${N}"
  else
    local la port bp purl full_url ssl_en ssl_dom scheme pip
    la=$(web_listen_addr)
    port=$(web_port)
    bp=$(web_basepath)
    purl=$(web_panel_url)
    ssl_en=$(web_ssl_enabled)
    ssl_dom=$(web_ssl_domain)
    pip=$(public_ip)

    scheme="http"
    [[ "$ssl_en" == "true" ]] && scheme="https"

    bp="/${bp#/}"
    [[ "$bp" != */ ]] && bp="${bp}/"
    if [[ -n "$purl" ]]; then
      purl="${purl%/}"
      full_url="${purl}${bp}"
    fi

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

    if ! is_sui_backend; then
      echo -e "  核心配置:    ${B}/etc/sing-box/config.json${N}"
    else
      local sui_db="/usr/local/s-ui/db/s-ui.db"
      if [[ -f "$sui_db" || -x /usr/local/s-ui/sui ]]; then
        local sui_port="8443"
        local sui_path="/app/"
        local sui_u
        sui_u=$(get_sui_user)
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
  fi
  is_sui_backend && [[ -f /usr/local/s-ui/db/s-ui.db ]] && echo -e "  s-ui 唤起命令: ${C}s-ui${N}"
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

  "$BIN" json set "$WORK_DIR/settings.json" "listen_addr=$new_addr" "port=$new_port" 2>/dev/null || true
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

  if [[ -n "$new_url" ]]; then
    "$BIN" json set "$WORK_DIR/settings.json" "panel_url=$new_url" 2>/dev/null || true
  else
    "$BIN" json del "$WORK_DIR/settings.json" panel_url 2>/dev/null || true
  fi
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
        "$BIN" json set "$WORK_DIR/settings.json" "ssl_enabled=false" 2>/dev/null || true
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
        "$BIN" json set "$WORK_DIR/settings.json" "ssl_cert=$new_cert" "ssl_key=$new_key" 2>/dev/null || true
        svc_restart
        echo -e "  ${G}SSL 证书路径已更新并重启生效${N}"
        ;;
      3)
        echo
        read -rp "  请输入新域名 (如 sout.example.com): " new_dom
        "$BIN" json set "$WORK_DIR/settings.json" "ssl_domain=$new_dom" 2>/dev/null || true
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
      "$BIN" json set "$WORK_DIR/settings.json" "ssl_enabled=true" "ssl_domain=$new_dom" "ssl_cert=$new_cert" "ssl_key=$new_key" 2>/dev/null || true
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
  rc-service sout stop 2>/dev/null || true
  rc-update del sout default 2>/dev/null || true
  rc-service fanout stop 2>/dev/null || true
  rc-update del fanout default 2>/dev/null || true
  rc-service caddy stop 2>/dev/null || true
  rc-update del caddy default 2>/dev/null || true
  rc-service cloudflared stop 2>/dev/null || true
  rc-update del cloudflared default 2>/dev/null || true

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
  public_ip=$(curl -s4m 2 https://checkip.amazonaws.com 2>/dev/null || curl -s4m 2 https://api.ipify.org 2>/dev/null || curl -s4m 2 https://icanhazip.com 2>/dev/null || curl -s4m 2 https://ifconfig.me 2>/dev/null || true)
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
  echo -e "${G}  🎉 sout 插件、轻量网关及 Cloudflare 隧道已彻底清理干净！${N}"
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
  rc-service sout stop 2>/dev/null || true
  rc-update del sout default 2>/dev/null || true
  rc-service fanout stop 2>/dev/null || true
  rc-update del fanout default 2>/dev/null || true
  rc-service caddy stop 2>/dev/null || true
  rc-update del caddy default 2>/dev/null || true
  rc-service cloudflared stop 2>/dev/null || true
  rc-update del cloudflared default 2>/dev/null || true
  rc-service s-ui stop 2>/dev/null || true
  rc-update del s-ui default 2>/dev/null || true
  rc-service sing-box stop 2>/dev/null || true
  rc-update del sing-box default 2>/dev/null || true
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

  # 彻底清理 sing-box 服务
  systemctl stop sing-box 2>/dev/null || rc-service sing-box stop 2>/dev/null || true
  systemctl disable sing-box 2>/dev/null || rc-update del sing-box default 2>/dev/null || true
  rm -f /etc/systemd/system/sing-box.service /etc/init.d/sing-box 2>/dev/null || true
  rm -f /var/log/sing-box.log /var/log/sing-box.err /run/sing-box.pid 2>/dev/null || true

  # 清理运行日志、PID 与可能存在的旧迁移目录
  rm -f /var/log/sout.log /var/log/sout.err /var/log/s-ui.log /var/log/caddy.log /var/log/cloudflared.log /var/log/cloudflared_quick.log 2>/dev/null || true
  rm -f /run/sout.pid /run/fanout.pid /run/caddy.pid /run/cloudflared.pid /run/s-ui.pid 2>/dev/null || true
  rm -rf /usr/local/sout 2>/dev/null || true
  rm -rf /root/.local/share/caddy /root/.config/caddy /root/.cache/caddy /root/.cloudflared 2>/dev/null || true

  svc_reload
  echo -e "  ${G}[✓] 所有组件 (sout, sing-box/s-ui, cloudflared) 已彻底卸载干净，系统已完全恢复初始状态！${N}"
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

update_singbox_kernel() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}   检查/更新 sing-box 原生内核到最新稳定版${N}"
  echo -e "${B}========================================${N}"

  local cur_sb_ver="未安装"
  if command -v sing-box >/dev/null 2>&1; then
    cur_sb_ver=$(sing-box version 2>/dev/null | head -1 | awk '{print $3}' || echo "已安装")
  elif [[ -x /usr/local/bin/sing-box ]]; then
    cur_sb_ver=$(/usr/local/bin/sing-box version 2>/dev/null | head -1 | awk '{print $3}' || echo "已安装")
  fi

  echo -e "  当前 sing-box 版本: ${Y}${cur_sb_ver}${N}"
  echo -e "  ${B}正在连接官方检查最新稳定版本...${N}"

  local tag=""
  tag=$(curl -sIL -m 8 "https://github.com/SagerNet/sing-box/releases/latest" 2>/dev/null | grep -i '^location:' | tail -1 | grep -oE 'v[0-9]+(\.[0-9]+)+' | tr -d ' \r\n' || true)
  if [[ -z "$tag" ]]; then
    tag=$(curl -sSL -m 8 "https://api.github.com/repos/SagerNet/sing-box/releases/latest" 2>/dev/null | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | cut -d'"' -f4 || true)
  fi
  [[ -z "$tag" ]] && tag="v1.11.5"
  local ver="${tag#v}"

  echo -e "  官方最新稳定版本:   ${G}${tag}${N}"
  echo

  if [[ "$cur_sb_ver" == "$ver" || "$cur_sb_ver" == "$tag" ]]; then
    echo -e "  ${G}当前 sing-box 已是官方最新稳定版！${N}"
    read -rp "  是否仍要强制重新下载并覆盖？[y/N]: " force_sb
    [[ ${force_sb,,} == y ]] || return 0
  else
    read -rp "  是否立即更新 sing-box 到 ${tag}？[Y/n]: " do_sb
    [[ ${do_sb,,} == n ]] && return 0
  fi

  local arch uname_m
  uname_m=$(uname -m)
  case "$uname_m" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    armv7l|armhf) arch="armv7" ;;
    *) echo -e "  ${R}不支持的系统架构: $uname_m${N}"; return 1 ;;
  esac

  local tmp_dir
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' RETURN

  local is_musl=0
  if [[ -f /etc/alpine-release ]] || (ldd --version 2>&1 | grep -iq musl); then
    is_musl=1
  fi

  local pkg_candidates=()
  if [[ "$is_musl" -eq 1 ]]; then
    pkg_candidates+=("sing-box-${ver}-linux-${arch}-musl.tar.gz" "sing-box-${ver}-linux-${arch}.tar.gz")
  else
    pkg_candidates+=("sing-box-${ver}-linux-${arch}.tar.gz" "sing-box-${ver}-linux-${arch}-glibc.tar.gz")
  fi

  local download_urls=()
  for pkg in "${pkg_candidates[@]}"; do
    download_urls+=(
      "https://github.com/SagerNet/sing-box/releases/download/${tag}/${pkg}"
      "https://ghproxy.net/https://github.com/SagerNet/sing-box/releases/download/${tag}/${pkg}"
    )
  done

  local dl_ok=0
  for u in "${download_urls[@]}"; do
    echo -e "  正在下载: ${u} ..."
    rm -rf "${tmp_dir:?}"/* /usr/local/bin/sing-box
    local archive_file="${tmp_dir}/sing-box.tar.gz"
    if curl -fL -# --connect-timeout 10 --max-time 180 "$u" -o "$archive_file" && [[ -s "$archive_file" ]]; then
      if tar -xzf "$archive_file" -C "$tmp_dir" --strip-components=1 2>/dev/null || tar -xzf "$archive_file" -C "$tmp_dir" 2>/dev/null; then
        local extracted_bin=""
        if [[ -f "${tmp_dir}/sing-box" ]]; then
          extracted_bin="${tmp_dir}/sing-box"
        else
          extracted_bin=$(find "$tmp_dir" -type f -name "sing-box" 2>/dev/null | head -1)
        fi
        if [[ -n "$extracted_bin" && -f "$extracted_bin" ]]; then
          mv -f "$extracted_bin" /usr/local/bin/sing-box
          chmod 755 /usr/local/bin/sing-box
          if /usr/local/bin/sing-box version >/dev/null 2>&1; then
            dl_ok=1
            break
          fi
        fi
      fi
    fi
  done

  if [[ "$dl_ok" -eq 0 ]]; then
    echo -e "  ${R}[!] 下载并校验 sing-box 失败，请检查网络连接。${N}"
    rm -f /usr/local/bin/sing-box
    return 1
  fi
  /usr/local/bin/sing-box version 2>/dev/null | head -1 | awk '{print $3}' > "${WORK_DIR}/singbox_version" 2>/dev/null || true
  echo -e "  ${G}[✓] sing-box 已成功更新到最新稳定版: $(/usr/local/bin/sing-box version 2>/dev/null | head -1)${N}"
  systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true
  systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || true
  return 0
}

check_and_update() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}       检查与更新组件版本${N}"
  echo -e "${B}========================================${N}"
  echo -e "   1) 检查/更新 sout 插件与管理脚本"
  echo -e "   2) 检查/更新 sing-box 原生内核到官方最新稳定版"
  echo -e "   0) 返回上级菜单"
  echo -e "${D}----------------------------------------${N}"
  read -rp "  请选择 [0-2] (默认 1): " up_choice
  up_choice=$(echo "$up_choice" | tr -d ' \r\n')
  [[ -z "$up_choice" ]] && up_choice="1"

  case "$up_choice" in
    1) ;;
    2) update_singbox_kernel; return $? ;;
    0) return 0 ;;
    *) return 0 ;;
  esac

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

  # 1. 下载前执行页缓存回收，降低内存碎片和缓存占用
  sync && echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true

  # 2. 解包覆盖前停止 sout 释放其占用的常驻内存
  svc_stop

  echo -e "  正在下载并解压: ${tar_url} ..."
  # 3. 采用管道流式下载解压；若管道解包失败则平滑降级到带重试的文件下载解压
  if ! (curl -fsSL "$tar_url" | tar -zxf - -C "$tmp_dir"); then
    echo -e "  ${Y}[!] 管道流式解压失败，尝试回退到带重试的文件下载...${N}"
    if ! curl -fsSL --retry 3 --connect-timeout 15 "$tar_url" -o "$tmp_dir/sout.tar.gz" || ! tar -zxf "$tmp_dir/sout.tar.gz" -C "$tmp_dir"; then
      echo -e "  ${R}下载或解压发布包失败！正在恢复服务...${N}"
      svc_start
      return 1
    fi
    rm -f "$tmp_dir/sout.tar.gz" 2>/dev/null || true
  fi

  # 4. 将解包出的二进制用 mv -f 覆盖至 $BIN 并赋予可执行权限
  if [[ -f "$tmp_dir/sout-server" ]]; then
    mv -f "$tmp_dir/sout-server" "$BIN"
    chmod +x "$BIN"
    ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  elif [[ -f "$tmp_dir/sout" ]]; then
    mv -f "$tmp_dir/sout" "$BIN"
    chmod +x "$BIN"
    ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  elif [[ -f "$tmp_dir/fanout" ]]; then
    mv -f "$tmp_dir/fanout" "$BIN"
    chmod +x "$BIN"
    ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  fi

  if [[ -f "$tmp_dir/f.sh" ]]; then
    cp -f "$tmp_dir/f.sh" /usr/local/bin/sout
    chmod +x /usr/local/bin/sout
    rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
  fi

  echo "$tag_name" > "${WORK_DIR}/version" 2>/dev/null || true
  rm -rf "$tmp_dir" /tmp/sout-linux-*.tar.gz /tmp/sout-server /tmp/fanout /tmp/f.sh 2>/dev/null || true

  echo
  echo -e "  ${B}[+] 正在启动服务并加载最新版本配置...${N}"
  # 5. 升级完成后调用 svc_start 拉起 sout
  svc_start

  # 6. 根据后端模式重启对应的核心服务
  if is_sui_backend; then
    systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || service s-ui restart 2>/dev/null || true
  else
    systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || service sing-box restart 2>/dev/null || true
  fi

  # 7. 若开启了 Cloudflare 隧道，联动重启隧道并执行内置轻量反代网关重新探测分流
  if [[ -f "$CADDY_META" ]] && grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META" 2>/dev/null; then
    echo -e "  ${B}[+] 检测到已开启 Cloudflare 隧道反代，正在自动重启隧道服务...${N}"
    systemctl restart cloudflared 2>/dev/null || rc-service cloudflared restart 2>/dev/null || service cloudflared restart 2>/dev/null || true
    echo -e "  ${B}[+] 正在自动执行内置轻量反代网关重新探测并分流...${N}"
    reload_caddy_proxy
  fi

  echo
  echo -e "  ${G}🎉 恭喜！sout 已成功更新至 ${tag_name}，所有关联服务已自动重启生效。${N}"
}

#!/usr/bin/env bash
# ==============================================================================
# sout - Cloudflare 隧道连接与轻量网关分流独立管理脚本
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

get_sys_arch() {
  local a
  a=$(uname -m)
  case "$a" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv7l|armv7) echo "armv7" ;;
    *) echo "amd64" ;;
  esac
}

install_cloudflared_bin() {
  if command -v cloudflared >/dev/null 2>&1; then
    return 0
  fi
  local arch
  arch=$(get_sys_arch)
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
  local protocol="${3:-${TUNNEL_PROTOCOL:-quic}}"
  [[ "$protocol" != "http2" ]] && protocol="quic"

  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    mkdir -p /etc/systemd/system
    if [[ -n "$token" ]]; then
      cat > /etc/systemd/system/cloudflared.service <<EOF
[Unit]
Description=Cloudflare Named Tunnel Agent
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/cloudflared tunnel --protocol ${protocol} --no-autoupdate run --token ${token}
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
ExecStart=/usr/local/bin/cloudflared tunnel --protocol ${protocol} --url http://127.0.0.1:${tun_p} --no-autoupdate
Restart=always
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    fi
    systemctl daemon-reload 2>/dev/null || true
    systemctl enable cloudflared >/dev/null 2>&1 || true
    systemctl restart cloudflared 2>/dev/null || true
  else
    # 兼容 OpenRC (Alpine Linux)
    mkdir -p /etc/init.d
    local cf_args
    if [[ -n "$token" ]]; then
      cf_args="tunnel --protocol ${protocol} --no-autoupdate run --token ${token}"
    else
      cf_args="tunnel --protocol ${protocol} --url http://127.0.0.1:${tun_p} --no-autoupdate"
    fi
    cat > /etc/init.d/cloudflared <<EOF
#!/sbin/openrc-run
name="cloudflared"
description="Cloudflare Tunnel Agent"
command="/usr/local/bin/cloudflared"
command_args="${cf_args}"
command_background="yes"
pidfile="/run/cloudflared.pid"
output_log="/var/log/cloudflared.log"
error_log="/var/log/cloudflared.err"
respawn_delay=5

depend() {
  need net
  after firewall
}

start_pre() {
  if [ -n "\$command" ]; then
    local bin_name
    bin_name="\$(basename "\$command")"
    pkill -9 -x "\$bin_name" 2>/dev/null || true
  fi
  if [ -f "\$pidfile" ]; then
    local p
    p=\$(cat "\$pidfile" 2>/dev/null)
    if [ -n "\$p" ] && ! kill -0 "\$p" 2>/dev/null; then
      rm -f "\$pidfile"
    fi
  fi
}

stop_post() {
  rm -f "\$pidfile"
  if [ -n "\$command" ]; then
    pkill -9 -x "\$(basename "\$command")" 2>/dev/null || true
  fi
}
EOF
    chmod +x /etc/init.d/cloudflared
    rc-update add cloudflared default >/dev/null 2>&1 || true
    rc-service cloudflared stop >/dev/null 2>&1 || true
    rc-service cloudflared zap >/dev/null 2>&1 || true
    pkill -9 -f "/usr/local/bin/cloudflared" 2>/dev/null || true
    pkill -9 -f "sout-quick-tunnel" 2>/dev/null || true
    sleep 0.3
    rc-service cloudflared start >/dev/null 2>&1 || true
  fi

  # 清空旧日志，避免 get_quick_tunnel_domain 读到上一次临时隧道的旧域名
  : > /var/log/cloudflared.log 2>/dev/null || true
  : > /var/log/cloudflared.err 2>/dev/null || true
}

get_quick_tunnel_domain() {
  local max_wait=20
  local d=""
  for ((i=1; i<=max_wait; i++)); do
    if command -v journalctl >/dev/null 2>&1; then
      d=$(journalctl -u cloudflared -n 50 --no-pager 2>/dev/null | grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' | tail -1 | sed 's|https://||' | tr -d ' \r\n')
    fi
    if [[ -z "$d" && -f /var/log/cloudflared.err ]]; then
      d=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' /var/log/cloudflared.err 2>/dev/null | tail -1 | sed 's|https://||' | tr -d ' \r\n')
    fi
    if [[ -z "$d" && -f /var/log/cloudflared.log ]]; then
      d=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' /var/log/cloudflared.log 2>/dev/null | tail -1 | sed 's|https://||' | tr -d ' \r\n')
    fi
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

is_tunnel_gateway_enabled() {
  if [[ -f "$CADDY_META" ]]; then
    if grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META"; then
      echo "true"
      return
    fi
  fi
  echo "false"
}

is_caddy_enabled() {
  is_tunnel_gateway_enabled
}

setup_caddy_proxy() {
  local domain="${1:-}"
  local tunnel_token="${2:-}"
  local tunnel_port="${3:-8081}"
  local apply_cert="${4:-n}"
  local cf_dns_key="${5:-}"
  local in_sui_pass="${6:-}"
  local in_sui_is_random="${7:-0}"
  local protocol="${TUNNEL_PROTOCOL:-quic}"
  [[ "$protocol" != "http2" ]] && protocol="quic"

  local meta_f="${WORK_DIR}/caddy_meta.json"
  if [[ -f "$meta_f" ]]; then
    if [[ -z "$tunnel_token" || "$tunnel_token" == *"..."* || "$tunnel_token" == *"***"* ]]; then
      local exist_token exist_dom
      exist_token=$("$BIN" json get "$meta_f" tunnel_token 2>/dev/null || true)
      exist_dom=$("$BIN" json get "$meta_f" domain 2>/dev/null || true)
      if [[ -n "$exist_token" ]]; then
        tunnel_token="$exist_token"
        [[ -z "$domain" ]] && domain="$exist_dom"
      fi
    fi
  fi

  local is_quick="false"
  if [[ -z "$domain" && -z "$tunnel_token" ]]; then
    is_quick="true"
  fi

  echo
  echo -e "${B}================================================================${N}"
  if [[ "$is_quick" == "true" ]]; then
    echo -e "${G}  正在配置 Cloudflare 官方免费临时隧道 (免域名 / 免Token)...${N}"
  else
    echo -e "${B}  正在配置 Cloudflare隧道连接与轻量流量分流 (${domain})...${N}"
  fi
  echo -e "${B}================================================================${N}"

  # 1. 确保已停用清理 Caddy，并安装 cloudflared
  if command -v caddy >/dev/null 2>&1; then
    rc-service caddy stop 2>/dev/null || true
    rc-update del caddy default 2>/dev/null || true
    systemctl stop caddy 2>/dev/null || true
    systemctl disable caddy 2>/dev/null || true
    pkill -9 -x caddy 2>/dev/null || true
  fi
  install_cloudflared_bin || { echo -e "  ${R}安装 cloudflared 失败${N}"; return 1; }

  # 2. 检查后端类型并分配本地端口与安全路径
  local has_sui=false
  local sui_token=""
  local sui_admin_user=""
  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_backup="${WORK_DIR}/sui_backup.json"

  if is_sui_backend && [[ -f "$sui_db" ]]; then
    has_sui=true
    sui_admin_user=$(get_sui_user)
    sui_token=$(get_or_create_sui_token "$sui_db")

    if [[ ! -f "$sui_backup" ]]; then
      # 用 sqlite3 读取后用 Go 子命令合成 JSON（不再依赖 python3）
      if command -v sqlite3 >/dev/null 2>&1; then
        local _bk_args=() _k _v
        for _k in webPort webListen webPath webDomain webCertFile webKeyFile webURI \
                  subPort subListen subPath subDomain subCertFile subKeyFile subURI subUpdates \
                  subEncode subShowInfo subClashExt subJsonExt subClashSprtAll subClashNoDefGrp; do
          _v=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='$_k' LIMIT 1;" 2>/dev/null || true)
          [[ -n "$_v" ]] && _bk_args+=("$_k=$_v")
        done
        if [[ ${#_bk_args[@]} -gt 0 ]]; then
          rm -f "$sui_backup"
          "$BIN" json set "$sui_backup" "${_bk_args[@]}" 2>/dev/null || true
        fi
      fi
    fi
  fi

  local sout_port sui_port sub_port node_port reality_port
  local sout_path sui_path sub_path ws_path

  local old_sout_port="" old_sui_port="" old_sub_port="" old_node_port="" old_reality_port=""
  local old_sout_path="" old_sui_path="" old_sub_path="" old_ws_path=""
  local meta_f="${WORK_DIR}/caddy_meta.json"
  if [[ -f "$meta_f" ]]; then
    eval $(python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        d = json.load(f)
    if isinstance(d, dict):
        sp = d.get("sout_port", 0)
        sup = d.get("sui_port", 0)
        subp = d.get("sub_port", 0)
        np = d.get("node_port", 0)
        rp = d.get("reality_port", 0)
        spa = d.get("sout_path", "")
        suia = d.get("sui_path", "")
        suba = d.get("sub_path", "")
        wsa = d.get("ws_path", "")
        if sp: print(f"old_sout_port={sp}")
        if sup: print(f"old_sui_port={sup}")
        if subp: print(f"old_sub_port={subp}")
        if np: print(f"old_node_port={np}")
        if rp: print(f"old_reality_port={rp}")
        if spa: print(f"old_sout_path={spa}")
        if suia: print(f"old_sui_path={suia}")
        if suba: print(f"old_sub_path={suba}")
        if wsa: print(f"old_ws_path={wsa}")
except Exception:
    pass
' "$meta_f" 2>/dev/null || true)
  fi

  # 原生 sing-box 模式：强制以 /etc/sing-box/config.json 中真实运行的入站端口与路径为最高优先级
  if [[ -f /etc/sing-box/config.json ]]; then
    eval $(python3 -c '
import json
try:
    with open("/etc/sing-box/config.json") as f:
        c = json.load(f)
    for ib in c.get("inbounds", []):
        t = ib.get("tag", "")
        tp = ib.get("type", "")
        lp = ib.get("listen_port", 0)
        if ("argo" in t or (tp in ("vless", "vmess") and isinstance(ib.get("transport"), dict) and ib["transport"].get("type") == "ws")) and lp:
            print(f"old_node_port={lp}")
            tr_p = ib.get("transport", {}).get("path", "")
            if tr_p:
                tr_p = tr_p.strip("/")
                print(f"old_ws_path={tr_p}")
        elif ("reality" in t or (isinstance(ib.get("tls"), dict) and "reality" in ib["tls"])) and lp:
            print(f"old_reality_port={lp}")
except Exception:
    pass
' 2>/dev/null || true)
  fi

  sout_port="${old_sout_port:-$(rand_local_port)}"
  sub_port="${old_sub_port:-$(rand_local_port)}"
  node_port="${old_node_port:-$(rand_local_port)}"
  reality_port="${old_reality_port:-$(rand_local_port)}"

  sout_path="${old_sout_path:-$(rand_safe_path "sout")}"
  sub_path="${old_sub_path:-$(rand_safe_path "sub")}"
  ws_path="${old_ws_path:-$(rand_safe_path "vlws")}"

  if [[ "$has_sui" == "true" ]]; then
    sui_port="${old_sui_port:-$(rand_local_port)}"
    sui_path="${old_sui_path:-$(rand_safe_path "sui")}"
  else
    sui_port=0
    sui_path=""
  fi

  local public_ip cur_cc
  public_ip=$(curl -s4m 5 https://checkip.amazonaws.com 2>/dev/null || curl -s4m 5 https://api.ipify.org 2>/dev/null || curl -s4m 5 https://ifconfig.me 2>/dev/null || echo "$domain")
  cur_cc=$(get_tcp_congestion)

  echo -e "  [+] 正在启动 Cloudflare 隧道服务 (${protocol})..."
  setup_cloudflared_service "$tunnel_token" "$tunnel_port" "$protocol"

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

  echo -e "  [+] 正在自动配置基础节点 (vless-argo 路径分流与 vless-reality)..."
  if ! SUI_API="http://127.0.0.1:${sui_port}/${sui_path}/apiv2" \
  SUI_TOKEN="$sui_token" \
  SUI_DB="$sui_db" \
  HAS_SUI="$has_sui" \
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
import json, os, uuid, urllib.request, urllib.parse, string, random, base64

has_sui = (os.environ.get('HAS_SUI') == 'true') and os.path.exists(os.environ.get('SUI_DB', '')) and bool(os.environ.get('SUI_TOKEN'))

if has_sui:
    import sqlite3
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
else:
    class SingboxAPICompat:
        def __init__(self, config_path='/etc/sing-box/config.json', addrs_path='/var/lib/sout/singbox_inbound_addrs.json'):
            self.config_path = config_path
            self.addrs_path = addrs_path
            self.tls_list = []
            self.inbounds_list = []
            self.clients_list = []
            self._load()

        def _load(self):
            try:
                with open(self.config_path, 'r') as f:
                    conf = json.load(f)
            except Exception:
                conf = {}
            for idx, ib in enumerate(conf.get('inbounds', [])):
                self.inbounds_list.append({
                    'id': (idx + 1) * 1000,
                    'type': ib.get('type', ''),
                    'tag': ib.get('tag', ''),
                    'listen_port': ib.get('listen_port', 0),
                    'addrs': ib.get('addrs', []),
                    'raw': ib
                })

        def request(self, method, endpoint, form=None):
            ep = endpoint.strip('/')
            if method == 'GET':
                if ep == 'inbounds': return {'success': True, 'obj': {'inbounds': self.inbounds_list}}
                elif ep == 'tls': return {'success': True, 'obj': {'tls': self.tls_list}}
                elif ep == 'clients': return {'success': True, 'obj': {'clients': self.clients_list}}
                return {'success': True, 'obj': {}}
            elif method == 'POST' and ep == 'save':
                obj = form.get('object')
                action = form.get('action')
                data = json.loads(form.get('data', '{}'))
                if obj == 'tls':
                    if action == 'new':
                        data['id'] = len(self.tls_list) + 1
                        self.tls_list.append(data)
                        return {'success': True, 'obj': {'id': data['id']}}
                    elif action == 'edit':
                        for i, t in enumerate(self.tls_list):
                            if t.get('id') == data.get('id') or t.get('name') == data.get('name'):
                                self.tls_list[i] = data
                                break
                        return {'success': True}
                elif obj == 'inbounds':
                    if action == 'new':
                        data['id'] = (len(self.inbounds_list) + 1) * 1000
                        self.inbounds_list.append(data)
                        return {'success': True, 'obj': {'id': data['id']}}
                    elif action == 'edit':
                        for i, ib in enumerate(self.inbounds_list):
                            if ib.get('id') == data.get('id') or ib.get('tag') == data.get('tag'):
                                self.inbounds_list[i] = data
                                break
                        return {'success': True}
                elif obj == 'clients':
                    if action == 'new':
                        data['id'] = 1
                        self.clients_list.append(data)
                        return {'success': True, 'obj': {'id': 1}}
                    elif action == 'edit':
                        for i, c in enumerate(self.clients_list):
                            if c.get('id') == data.get('id') or c.get('name') == data.get('name'):
                                self.clients_list[i] = data
                                break
                        else:
                            self.clients_list.append(data)
                        return {'success': True}
                elif obj == 'settings':
                    return {'success': True}
            return {'success': True}

        def flush(self):
            try:
                with open(self.config_path, 'r') as f: conf = json.load(f)
            except Exception: conf = {}
            admin_cfg = self.clients_list[0].get('config', {}) if self.clients_list else {}
            admin_name = self.clients_list[0].get('name', 'default') if self.clients_list else 'default'
            tls_map = {t['id']: t for t in self.tls_list}
            final_inbs = []
            addrs_store = {}

            seen_tags = set()
            for ib in self.inbounds_list:
                tag = ib.get('tag')
                proto = ib.get('type')
                if not tag or not proto or tag in seen_tags: continue
                seen_tags.add(tag)

                if ib.get('addrs'): addrs_store[tag] = ib['addrs']
                s_ib = {
                    'type': proto,
                    'tag': tag,
                    'listen': ib.get('listen', '::'),
                    'listen_port': int(ib.get('listen_port', 0))
                }
                p_cfg = admin_cfg.get(proto, {})
                u_uuid = p_cfg.get('uuid') or str(uuid.uuid4())
                u_pass = p_cfg.get('password') or ''
                u_flow = p_cfg.get('flow', '')
                if proto == 'vmess':
                    user_obj = {'name': admin_name, 'uuid': u_uuid}
                elif proto == 'vless':
                    user_obj = {'name': admin_name, 'uuid': u_uuid}
                    is_ws = (isinstance(ib.get('transport'), dict) and ib['transport'].get('type') == 'ws')
                    if u_flow and not is_ws: user_obj['flow'] = u_flow
                elif proto == 'tuic':
                    user_obj = {'name': admin_name, 'uuid': u_uuid, 'password': u_pass or os.urandom(8).hex()}
                elif proto in ['hysteria2', 'shadowsocks', 'trojan']:
                    user_obj = {'name': admin_name, 'password': u_pass or os.urandom(8).hex()}
                else:
                    user_obj = {'name': admin_name, 'uuid': u_uuid}
                    if u_pass: user_obj['password'] = u_pass
                s_ib['users'] = [user_obj]

                if proto == 'tuic':
                    s_ib['congestion_control'] = 'bbr'
                elif proto == 'hysteria2':
                    s_ib['ignore_client_bandwidth'] = True

                tid = ib.get('tls_id')
                if tid and tid in tls_map:
                    t_obj = tls_map[tid]
                    t_srv = t_obj.get('server', {})
                    if 'reality' in t_srv:
                        r_conf = dict(t_srv.get('reality', {}))
                        r_conf.pop('public_key', None)
                        s_ib['tls'] = {
                            'enabled': True,
                            'server_name': t_srv.get('server_name', 'apple.com'),
                            'reality': r_conf
                        }
                    elif t_obj.get('certificate_path') or t_srv.get('certificate_path'):
                        cert_p = t_obj.get('certificate_path') or t_srv.get('certificate_path')
                        key_p = t_obj.get('key_path') or t_srv.get('key_path')
                        s_name = t_obj.get('server_name') or t_srv.get('server_name', '')
                        tls_entry = {
                            'enabled': True,
                            'server_name': s_name,
                            'certificate_path': cert_p,
                            'key_path': key_p
                        }
                        if proto in ('tuic', 'hysteria2'):
                            tls_entry['alpn'] = ['h3']
                        s_ib['tls'] = tls_entry
                elif isinstance(ib.get('tls'), dict):
                    raw_tls = dict(ib['tls'])
                    if proto in ('tuic', 'hysteria2'):
                        raw_tls['alpn'] = ['h3']
                    s_ib['tls'] = raw_tls

                if ib.get('transport'): s_ib['transport'] = ib['transport']
                final_inbs.append(s_ib)

            conf['inbounds'] = final_inbs
            os.makedirs(os.path.dirname(self.config_path), exist_ok=True)
            with open(self.config_path, 'w') as f: json.dump(conf, f, indent=2)

            os.makedirs(os.path.dirname(self.addrs_path), exist_ok=True)
            try:
                with open(self.addrs_path, 'r') as f: cur_addrs = json.load(f)
            except Exception: cur_addrs = {}
            cur_addrs.update(addrs_store)
            with open(self.addrs_path, 'w') as f: json.dump(cur_addrs, f, indent=2)

    compat = SingboxAPICompat()
    def api(method, endpoint, form=None):
        return compat.request(method, endpoint, form)

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
        if t.startswith(prefix + '-') and len(t) == len(prefix) + 5:
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

# 固定启用 vless-argo 与 vless-reality 基础节点
# 优先查找已有的 vless-argo；若无则查找存量的旧 vmess-argo 进行平滑覆盖升级
existing_argo = None
for r in inbound_rows:
    t = r.get('tag', '')
    if t.startswith('vless-argo') or (r.get('type') == 'vless' and 'argo' in t):
        existing_argo = r
        break
if not existing_argo:
    for r in inbound_rows:
        t = r.get('tag', '')
        if t.startswith('vmess-argo') or r.get('type') == 'vmess':
            existing_argo = r
            break

argo_id = None
argo_tag = None
old_vmess_tag = None
if existing_argo:
    argo_id = existing_argo.get('id')
    old_tag = existing_argo.get('tag', '')
    if old_tag.startswith('vless-argo-') and len(old_tag) == len('vless-argo-') + 4:
        argo_tag = old_tag
    elif old_tag.startswith('vmess-argo-') and len(old_tag) == len('vmess-argo-') + 4:
        old_vmess_tag = old_tag
        argo_tag = 'vless-argo-' + old_tag[len('vmess-argo-'):]
    elif old_tag == 'vmess-argo':
        old_vmess_tag = old_tag
        argo_tag = f"vless-argo-{gen_rand_suffix()}"
    else:
        argo_tag = f"vless-argo-{gen_rand_suffix()}"
else:
    argo_tag = f"vless-argo-{gen_rand_suffix()}"

reality_tag, reality_id = get_or_create_tag_and_id('vless-reality')

node_port = int(os.environ['NODE_PORT'])
ws_path = os.environ['WS_PATH']
if existing_argo:
    if existing_argo.get('listen_port'):
        node_port = int(existing_argo['listen_port'])
    old_tr = existing_argo.get('transport')
    if isinstance(old_tr, dict) and old_tr.get('path'):
        ws_path = old_tr['path']
    elif existing_argo.get('raw', {}).get('transport', {}).get('path'):
        ws_path = existing_argo['raw']['transport']['path']

# 优选 IP/域名：保留所有已有的条目与备注，仅更新其 TLS server_name 为新域名
argo_addrs = []
old_addrs = existing_argo.get('addrs') if existing_argo else None
if not old_addrs:
    try:
        with open('/var/lib/sout/singbox_inbound_addrs.json', 'r') as af:
            ad_map = json.load(af)
            old_addrs = ad_map.get(argo_tag)
            if not old_addrs and old_vmess_tag:
                old_addrs = ad_map.get(old_vmess_tag)
    except Exception:
        pass

if old_addrs and isinstance(old_addrs, list) and len(old_addrs) > 0:
    for item in old_addrs:
        if not isinstance(item, dict): continue
        new_item = dict(item)
        tls_info = new_item.get('tls')
        if not isinstance(tls_info, dict):
            tls_info = {'enabled': True, 'insecure': False, 'utls': {'enabled': True, 'fingerprint': 'chrome'}}
        tls_info['server_name'] = domain
        tls_info['enabled'] = True
        new_item['tls'] = tls_info
        if len(old_addrs) == 1 and (new_item.get('server') == '' or '.trycloudflare.com' in str(new_item.get('server'))):
            new_item['server'] = domain
        argo_addrs.append(new_item)

if not argo_addrs:
    argo_addrs = [{
        'server': domain,
        'server_port': 443,
        'tls': {
            'disable_sni': False,
            'enabled': True,
            'insecure': False,
            'server_name': domain,
            'utls': {'enabled': True, 'fingerprint': 'chrome'}
        }
    }]

# 若从旧的 vmess-argo 升级，同步迁移 singbox_inbound_addrs.json 中的 key
if old_vmess_tag and old_vmess_tag != argo_tag:
    try:
        af_path = '/var/lib/sout/singbox_inbound_addrs.json'
        with open(af_path, 'r') as af:
            ad_map = json.load(af)
        if old_vmess_tag in ad_map:
            ad_map[argo_tag] = argo_addrs
            with open(af_path, 'w') as af:
                json.dump(ad_map, af, indent=2)
    except Exception:
        pass

vless_payload = {
    'id': argo_id or 0,
    'type': 'vless',
    'tag': argo_tag,
    'tls_id': 0,
    'listen': '127.0.0.1',
    'listen_port': node_port,
    'addrs': argo_addrs,
    'transport': {
        'early_data_header_name': 'Sec-WebSocket-Protocol',
        'max_early_data': 2560,
        'headers': {'Host': domain},
        'path': ws_path,
        'type': 'ws'
    }
}
api('POST', 'save', {
    'object': 'inbounds',
    'action': 'edit' if argo_id else 'new',
    'data': json.dumps(vless_payload)
})
created_inbound_tags.append(argo_tag)

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
if reality_id:
    for r in inbound_rows:
        if (r.get('id') == reality_id or r.get('tag') == reality_tag) and r.get('listen_port'):
            reality_port = int(r['listen_port'])
            break
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

if not has_sui:
    compat.flush()

try:
    with open('/var/lib/sout/.caddy_meta_vars', 'w') as mf:
        mf.write(f"node_port={node_port}\nws_path={ws_path.strip('/')}\nreality_port={reality_port}\n")
except Exception:
    pass
PYEOF
  then
    echo -e "  ${Y}[!] 节点 API 自动配置未完成，请稍后手动检查。${N}"
  fi

  if [[ -f "${WORK_DIR}/.caddy_meta_vars" ]]; then
    source "${WORK_DIR}/.caddy_meta_vars"
    rm -f "${WORK_DIR}/.caddy_meta_vars"
  fi

  if [[ "$has_sui" == "true" ]]; then
    systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || service s-ui restart 2>/dev/null || true
  else
    systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || service sing-box restart 2>/dev/null || true
  fi

  # 5. 自动化配置 sout
  echo -e "  [+] 正在自动配置 sout 插件 (绑定 127.0.0.1:${sout_port} 并挂载路径 /${sout_path}/)..."
  mkdir -p "$WORK_DIR"
  echo "$sout_path" > "${WORK_DIR}/basepath"
  chmod 600 "${WORK_DIR}/basepath"

  "$BIN" json set "$WORK_DIR/settings.json" \
    "port=$sout_port" "listen_addr=127.0.0.1" "panel_url=https://$domain" "ssl_enabled=false" 2>/dev/null || true

  # 6. 保存反代元数据
  local meta_mode="tunnel"
  [[ "$is_quick" == "true" ]] && meta_mode="quick_tunnel"
  cat > "$CADDY_META" <<METAEOF
{
  "enabled": true,
  "mode": "${meta_mode}",
  "protocol": "${protocol}",
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

  # 必须重启 sout 服务以应用新的内部端口 (127.0.0.1:${sout_port}) 和访问路径 (/${sout_path}/)
  if [[ "${SOUT_CALLED_FROM_WEB:-0}" != "1" ]]; then
    systemctl restart sout 2>/dev/null || rc-service sout restart 2>/dev/null || service sout restart 2>/dev/null || true
    sleep 1
  fi

  # 7. 统一调用终端菜单中的轻量反代网关探测并分流，确保分流规则与隧道回源完全一致
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
        create_tuic_hy2_nodes "$domain" "$cert_file" "$key_file" "false" "true"
      fi
    else
      echo -e "  ${Y}[!] 证书申请未成功或 API Key 无效，基础服务不受影响，您可稍后在终端菜单重新申请并补齐节点。${N}"
    fi
  elif [[ -n "$domain" && -s "$cert_file" && -s "$key_file" ]]; then
    local has_tuic_hy2=false
    if [[ -f /etc/sing-box/config.json ]] && grep -q '"tuic"' /etc/sing-box/config.json 2>/dev/null; then
      has_tuic_hy2=true
    elif [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
      has_tuic_hy2=true
    fi
    if [[ "$has_tuic_hy2" == "false" ]]; then
      echo
      echo -e "  ${G}[✓] 本地已存在域名 [${domain}] 的完整证书，正在自动挂载 TUIC 与 Hysteria2 节点...${N}"
      create_tuic_hy2_nodes "$domain" "$cert_file" "$key_file" "false" "true"
    else
      echo -e "  ${G}[✓] 本地已存在域名 [${domain}] 证书且已有高速节点，保持原配置运行。${N}"
    fi
  fi

  echo
  echo -e "${G}================================================================${N}"
  if [[ "$is_quick" == "true" ]]; then
    echo -e "${G}  🎉 sout 插件安装部署完成！(Cloudflare 官方免费临时隧道)${N}"
  else
    echo -e "${G}  🎉 sout 插件安装部署完成！(Cloudflare 隧道连接与轻量网关分流)${N}"
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
  if [[ "$has_sui" == "true" ]]; then
    echo -e "  [2] s-ui 节点与分流管理面板"
    echo -e "      访问地址:  ${B}https://${domain}/${sui_path}/${N}"
    echo -e "      管理账号:  ${Y}${sui_admin_user}${N}"
    if [[ -n "$in_sui_pass" && "$in_sui_is_random" == "1" ]]; then
      echo -e "      管理密码:  ${G}${in_sui_pass}${N}"
      echo -e "      ${Y}⚠️ 安全提示: 该随机密码仅在安装完成时显示一次，请务必妥善保存！${N}"
      echo -e "                 ${D}(若遗忘密码，可随时在终端输入 s-ui 进行重置修改)${N}"
    elif [[ -n "$in_sui_pass" && "$in_sui_is_random" != "1" ]]; then
      echo -e "      管理密码:  ${D}[已按您输入的自定义密码生效，若遗忘密码，可随时在终端输入 s-ui 进行重置修改]${N}"
    else
      echo -e "      管理密码:  ${D}[由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]${N}"
    fi
    echo -e "      唤起命令:  s-ui"
    echo
    echo -e "  [3] sout 订阅地址:  ${B}https://${domain}/${sout_path}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")${N}"
  else
    echo -e "  [2] sout 订阅地址:  ${B}https://${domain}/${sout_path}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")${N}"
  fi
  echo -e "${G}================================================================${N}"
  echo
}

disable_caddy_proxy() {
  local force="${1:-}"
  if [[ "$force" != "force" && "$force" != "-y" ]]; then
    echo
    read -rp "  确定关闭 Cloudflare 隧道反代并恢复默认独立端口模式吗？[y/N]: " yes
    [[ ${yes,,} == y ]] || { echo "  已取消"; return; }
  fi

  echo "  [+] 正在停止隧道与轻量网关分流..."
  systemctl stop caddy 2>/dev/null || rc-service caddy stop 2>/dev/null || true
  systemctl disable caddy 2>/dev/null || rc-update del caddy default 2>/dev/null || true
  systemctl stop cloudflared 2>/dev/null || rc-service cloudflared stop 2>/dev/null || true
  systemctl disable cloudflared 2>/dev/null || rc-update del cloudflared default 2>/dev/null || true

  if [[ -f "$CADDY_META" ]]; then
    rm -f "$CADDY_META"
  fi

  # 恢复 sout 为默认端口 8899 和 0.0.0.0 监听
  "$BIN" json set "$WORK_DIR/settings.json" "port=8899" "listen_addr=0.0.0.0" 2>/dev/null || true
  "$BIN" json del "$WORK_DIR/settings.json" panel_url 2>/dev/null || true

  # 恢复 s-ui 监听与配置 (优先从备份还原，若端口被占用则自动随机空闲端口)
  local public_ip
  public_ip=$(curl -s4m 2 https://checkip.amazonaws.com 2>/dev/null || curl -s4m 2 https://api.ipify.org 2>/dev/null || curl -s4m 2 https://icanhazip.com 2>/dev/null || curl -s4m 2 https://ifconfig.me 2>/dev/null || true)
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

  if [[ "${SOUT_CALLED_FROM_WEB:-0}" != "1" ]]; then
    systemctl restart sout 2>/dev/null || rc-service sout restart 2>/dev/null || systemctl restart fanout 2>/dev/null || true
  fi
  echo -e "  ${G}[✓] 已成功关闭隧道反代，所有服务已恢复公网 0.0.0.0 直连模式 (已还原证书与配置)：${N}"
  echo -e "      • sout 管理面板: ${B}http://${public_ip}:8899/$(cat "${WORK_DIR}/basepath" 2>/dev/null || echo "")/${N}"
  echo -e "      • s-ui 管理面板: ${B}${final_proto}://${show_web_host}${final_wpath}${N}"
  echo -e "      • s-ui 订阅地址: ${B}${final_sub_proto}://${show_sub_host}${final_spath}${N}"
}

caddy_interactive_setup() {
  echo
  echo -e "${B}================================================================${N}"
  echo -e "${B}  🚀 Cloudflare 隧道连接与轻量网关分流一键配置 (免开端口/杜绝525)${N}"
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
  echo -e "  ${D}💡 本地回源端口用于 cloudflared 将流量转发至内置轻量反代网关，默认 8081 即可${N}"
  read -rp "  3. 请输入本地回源端口 [默认 8081]: " tunnel_port
  tunnel_port=$(echo "$tunnel_port" | tr -d ' \r\n')
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

  local domain tunnel_port sout_p sui_p sub_p ws_p sout_port sui_port sub_port node_port meta_mode has_sui="false"
  meta_mode=$(json_get "$CADDY_META" mode)
  domain=$(json_get "$CADDY_META" domain)
  tunnel_port=$(json_get "$CADDY_META" tunnel_port)

  # 0. 动态探测 Cloudflare 隧道的实时端口与活跃域名
  if [[ -f /etc/systemd/system/cloudflared.service ]]; then
    local probed_cf_port
    probed_cf_port=$(grep -oE 'http://127\.0\.0\.1:[0-9]+' /etc/systemd/system/cloudflared.service 2>/dev/null | awk -F: '{print $3}' | head -1)
    [[ -n "$probed_cf_port" ]] && tunnel_port="$probed_cf_port"
  elif [[ -f /etc/init.d/cloudflared ]]; then
    local probed_cf_port
    probed_cf_port=$(grep -oE 'http://127\.0\.0\.1:[0-9]+' /etc/init.d/cloudflared 2>/dev/null | awk -F: '{print $3}' | head -1)
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
    # 读 settings.json、必要时回写，并输出 "port|needs_restart"（不再依赖 python3）
    local _si_p _si_la _si_purl _si_restart="False"
    _si_p=$("$BIN" json get "${WORK_DIR}/settings.json" port 2>/dev/null || true)
    [[ -z "$_si_p" ]] && _si_p=8899
    _si_la=$("$BIN" json get "${WORK_DIR}/settings.json" listen_addr 2>/dev/null || true)
    _si_purl=$("$BIN" json get "${WORK_DIR}/settings.json" panel_url 2>/dev/null || true)
    if [[ "$_si_la" != "127.0.0.1" ]]; then
      "$BIN" json set "${WORK_DIR}/settings.json" "listen_addr=127.0.0.1" 2>/dev/null || true
      _si_restart="True"
    fi
    if [[ "$_si_purl" != "https://${domain}" ]]; then
      "$BIN" json set "${WORK_DIR}/settings.json" "panel_url=https://${domain}" 2>/dev/null || true
    fi
    sout_info="${_si_p}|${_si_restart}"
    sout_port=$(echo "$sout_info" | cut -d'|' -f1)
    if [[ "$(echo "$sout_info" | cut -d'|' -f2)" == "True" ]]; then
      sout_needs_restart=1
    fi
  fi
  sout_p=$(json_get "$CADDY_META" sout_path)
  local bp_val
  bp_val=$(web_basepath)
  [[ -n "$bp_val" ]] && sout_p="$bp_val"
  [[ -z "$sout_p" ]] && sout_p="sout"
  local sout_port_listening=0
  if (ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | grep -q ":${sout_port} "; then
    sout_port_listening=1
  fi
  if [[ "$sout_needs_restart" -eq 1 || "$sout_port_listening" -eq 0 ]]; then
    echo -e "  [+] 检测到 sout 端口或配置需同步，正在重启服务..."
    systemctl restart sout 2>/dev/null || systemctl restart fanout 2>/dev/null || rc-service sout restart 2>/dev/null || true
  fi

  # 2. 检查后端类型；若为 s-ui 则动态探测并自动纠偏 s-ui 面板配置
  sui_port=0
  sui_p=""
  sub_p=$(json_get "$CADDY_META" sub_path)
  [[ -z "$sub_p" ]] && sub_p="sub"
  sub_port=$(json_get "$CADDY_META" sub_port)
  [[ -z "$sub_port" ]] && sub_port="2097"

  local sui_needs_restart=0
  if is_sui_backend && [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
    has_sui="true"
    sui_port="2096"
    sui_p=$(json_get "$CADDY_META" sui_path)
    [[ -z "$sui_p" ]] && sui_p="sui"
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
cur.execute(\"SELECT value FROM settings WHERE key='subPort'\")
r5 = cur.fetchone()
sp_val = r5[0] if r5 and r5[0] else '2097'

target_web_uri = 'https://${domain}/' + path.strip('/') + '/'
target_sub_uri = 'https://${domain}/${sub_p}/'

changed = False
api_success = False
try:
    token_file = '/var/lib/sout/sui-token'
    token = ''
    if os.path.exists(token_file):
        with open(token_file, 'r') as tf:
            token = tf.read().strip()
    if token:
        clean_p = '/' + path.strip('/') + '/'
        base_api = f'http://127.0.0.1:{port}{clean_p}apiv2'
        s_payload = {
            'webListen': '127.0.0.1',
            'subListen': '127.0.0.1',
            'webURI': target_web_uri,
            'subURI': target_sub_uri,
        }
        form_data = urllib.parse.urlencode({
            'object': 'settings',
            'action': 'set',
            'data': json.dumps(s_payload),
        }).encode()
        req_save = urllib.request.Request(base_api.rstrip('/') + '/save', data=form_data, headers={'Token': token, 'Content-Type': 'application/x-www-form-urlencoded'})
        with urllib.request.urlopen(req_save, timeout=5) as r_save:
            r_save.read()
        api_success = True
except Exception:
    pass

if not api_success:
    if w_listen != '127.0.0.1':
        cur.execute("UPDATE settings SET value='127.0.0.1' WHERE key='webListen'")
        changed = True
    if s_listen != '127.0.0.1':
        cur.execute("UPDATE settings SET value='127.0.0.1' WHERE key='subListen'")
        changed = True
    cur.execute("UPDATE settings SET value=? WHERE key='webURI'", (target_web_uri,))
    cur.execute("UPDATE settings SET value=? WHERE key='subURI'", (target_sub_uri,))
    if changed:
        con.commit()

con.close()
path = path.strip('/')
print(f'{port}|{path}|{changed}|{sp_val}')
" 2>/dev/null || true)
    if [[ -n "$sui_info" ]]; then
      local probed_port probed_path probed_changed probed_sub_port
      probed_port=$(echo "$sui_info" | cut -d'|' -f1)
      probed_path=$(echo "$sui_info" | cut -d'|' -f2)
      probed_changed=$(echo "$sui_info" | cut -d'|' -f3)
      probed_sub_port=$(echo "$sui_info" | cut -d'|' -f4)
      [[ -n "$probed_port" ]] && sui_port="$probed_port"
      [[ -n "$probed_path" ]] && sui_p="$probed_path"
      [[ -n "$probed_sub_port" ]] && sub_port="$probed_sub_port"
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
  ws_p=$(json_get "$CADDY_META" ws_path)
  node_port=$(json_get "$CADDY_META" node_port)
  [[ -z "$node_port" ]] && node_port="2082"

  if is_sui_backend && [[ -f /usr/local/s-ui/db/s-ui.db ]]; then
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
    existing_row = None
    for r in inbound_rows:
        t = r.get('tag', '')
        if t == 'vless-argo' or t.startswith('vless-argo-'):
            node_tag = t
            existing_id = r.get('id')
            existing_row = r
            break

    if not node_tag:
        for r in inbound_rows:
            t = r.get('tag', '')
            if t == 'vmess-argo' or t.startswith('vmess-argo-') or (r.get('type') == 'vmess' and 'argo' in t):
                existing_id = r.get('id')
                existing_row = r
                if t.startswith('vmess-argo-') and len(t) == len('vmess-argo-') + 4:
                    node_tag = 'vless-argo-' + t[len('vmess-argo-'):]
                else:
                    import string, random
                    chars = string.ascii_lowercase + string.digits
                    node_tag = f"vless-argo-{''.join(random.choices(chars, k=4))}"
                break

    if not node_tag:
        import string, random
        chars = string.ascii_lowercase + string.digits
        rand_suffix = "".join(random.choices(chars, k=4))
        node_tag = f"vless-argo-{rand_suffix}"

    client_uuid = str(uuid.uuid4())
    node_port = int(os.environ['NODE_PORT'])
    ws_path = os.environ['WS_PATH']
    if existing_row:
        if existing_row.get('listen_port'):
            node_port = int(existing_row['listen_port'])
        old_tr = existing_row.get('transport')
        if isinstance(old_tr, dict) and old_tr.get('path'):
            ws_path = old_tr['path']

    addrs_data = []
    old_addrs = existing_row.get('addrs') if existing_row else None
    if old_addrs and isinstance(old_addrs, list) and len(old_addrs) > 0:
        for item in old_addrs:
            if not isinstance(item, dict): continue
            new_item = dict(item)
            tls_info = new_item.get('tls')
            if not isinstance(tls_info, dict):
                tls_info = {'enabled': True, 'insecure': False, 'utls': {'enabled': True, 'fingerprint': 'chrome'}}
            tls_info['server_name'] = os.environ['DOMAIN']
            tls_info['enabled'] = True
            new_item['tls'] = tls_info
            if len(old_addrs) == 1 and (new_item.get('server') == '' or '.trycloudflare.com' in str(new_item.get('server'))):
                new_item['server'] = os.environ['DOMAIN']
            addrs_data.append(new_item)

    if not addrs_data:
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
        'type': 'vless',
        'tag': node_tag,
        'tls_id': 0,
        'listen': '127.0.0.1',
        'listen_port': node_port,
        'addrs': addrs_data,
        'transport': {
            'early_data_header_name': 'Sec-WebSocket-Protocol',
            'max_early_data': 2560,
            'headers': {'Host': os.environ['DOMAIN']},
            'path': ws_path,
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

  # 4. 更新分流元数据 caddy_meta.json
  local save_sout_path="${sout_p:-$sout_path}"
  [[ -z "$save_sout_path" ]] && save_sout_path=$(web_basepath)
  local save_sui_path="${sui_path:-$sui_p}"
  local save_sub_path="${sub_path:-$sub_p}"
  local save_ws_path="${ws_path:-$ws_p}"

  # 防空兜底自愈：若关键路径为空，自动生成随机安全路径
  [[ -z "$save_sout_path" ]] && save_sout_path=$(rand_safe_path "sout")
  [[ -z "$save_sub_path" ]] && save_sub_path=$(rand_safe_path "sub")
  [[ -z "$save_ws_path" ]] && save_ws_path=$(rand_safe_path "vlws")
  if [[ "$has_sui" == "true" && -z "$save_sui_path" ]]; then
    save_sui_path=$(rand_safe_path "sui")
  fi

  # 规范化去除所有首尾斜杠，杜绝 //path// 格式
  save_sout_path=$(echo "$save_sout_path" | sed -e 's|^/*||' -e 's|/*$||')
  save_sub_path=$(echo "$save_sub_path" | sed -e 's|^/*||' -e 's|/*$||')
  save_ws_path=$(echo "$save_ws_path" | sed -e 's|^/*||' -e 's|/*$||')
  save_sui_path=$(echo "$save_sui_path" | sed -e 's|^/*||' -e 's|/*$||')

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
d['sout_path'] = '${save_sout_path}'
d['sui_port'] = int('${sui_port}') if '${has_sui}' == 'true' else 0
d['sui_path'] = '${save_sui_path}' if '${has_sui}' == 'true' else ''
d['sub_port'] = int('${sub_port}')
d['sub_path'] = '${save_sub_path}'
d['ws_path'] = '${save_ws_path}'
d['node_port'] = int('${node_port}')
with open(p, 'w') as f:
    json.dump(d, f, indent=2)
" 2>/dev/null || true

  # 6. 停用并清理残留 Caddy，由 sout 内置轻量网关接管
  if command -v caddy >/dev/null 2>&1; then
    rc-service caddy stop 2>/dev/null || true
    rc-update del caddy default 2>/dev/null || true
    systemctl stop caddy 2>/dev/null || true
    systemctl disable caddy 2>/dev/null || true
    rm -f /etc/init.d/caddy /etc/systemd/system/caddy.service 2>/dev/null || true
    pkill -9 -x caddy 2>/dev/null || true
  fi

  # 重启/重载 sout 服务以激活内置网关
  systemctl restart sout 2>/dev/null || rc-service sout restart 2>/dev/null || service sout restart 2>/dev/null || true

  echo -e "  ${G}[✓] 内置轻量反代网关已重新加载最新分流配置并成功启动！${N}"
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  最新轻量流量反代与分流详情${N}"
  echo -e "${B}========================================${N}"
  echo -e "  网关正在监听:    ${Y}127.0.0.1:${tunnel_port}${N}"
  echo
  echo -e "  ${G}• 将 /${save_sout_path}/ 路径流量转发至:   127.0.0.1:${sout_port} (sout 管理面板)${N}"
  echo -e "    外网访问: https://${domain}/${save_sout_path}/"
  echo
  if [[ "$has_sui" == "true" && -n "$save_sui_path" && "$sui_port" -gt 0 ]]; then
    echo -e "  ${G}• 将 /${save_sui_path}/ 路径流量转发至:    127.0.0.1:${sui_port} (s-ui 面板)${N}"
    echo -e "    外网访问: https://${domain}/${save_sui_path}/"
    echo
  else
    echo -e "  ${G}• 核心后端:                       127.0.0.1:${node_port} (sing-box 原生内核)${N}"
    echo -e "    核心配置: /etc/sing-box/config.json"
    echo
  fi
  echo -e "  ${G}• 将 /${save_sout_path}/sub 路径流量转发至: 127.0.0.1:${sout_port} (订阅接口)${N}"
  echo -e "    订阅链接: https://${domain}/${save_sout_path}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
  if [[ -n "$save_ws_path" ]]; then
    echo
    echo -e "  ${G}• 将 /${save_ws_path}/ 路径流量转发至:     127.0.0.1:${node_port} (节点流量)${N}"
  fi
  echo
  echo -e "  ${G}• 将 / 根路径流量响应:            200 OK (伪装服务就绪)${N}"
  echo -e "${B}========================================${N}"
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

  echo -e "  ${B}[1/4] 正在检查并准备 acme.sh 证书引擎...${N}"
  local acme_cmd="/root/.acme.sh/acme.sh"
  if [[ ! -x "$acme_cmd" ]]; then
    if command -v acme.sh >/dev/null 2>&1; then
      acme_cmd=$(command -v acme.sh)
    else
      echo -e "  [+] 正在自动安装轻量开源 acme.sh 证书引擎..."
      local email="admin@${domain}"
      if command -v apk >/dev/null 2>&1; then
        apk add --no-cache curl openssl socat >/dev/null 2>&1 || true
      elif command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq curl openssl socat cron >/dev/null 2>&1 || true
      fi
      if ! curl -sSL https://get.acme.sh | sh -s email="${email}" --home /root/.acme.sh; then
        echo -e "  ${Y}[!] 官方安装源重试，尝试备用源...${N}"
        curl -sSL https://raw.githubusercontent.com/acmesh-official/acme.sh/master/acme.sh | sh -s -- --install-online -m "${email}" --home /root/.acme.sh || true
      fi
    fi
  fi

  if [[ ! -x "$acme_cmd" && -f /root/.acme.sh/acme.sh ]]; then
    chmod +x /root/.acme.sh/acme.sh
    acme_cmd="/root/.acme.sh/acme.sh"
  fi

  if [[ ! -x "$acme_cmd" ]]; then
    echo -e "  ${R}[✗] acme.sh 安装受阻，请检查网络连接。${N}"
    return 1
  fi
  echo -e "  ${G}[✓] acme.sh 引擎已就绪${N}"

  echo -e "  ${B}[2/4] 正在创建证书存储目录: ${cert_dir}...${N}"
  mkdir -p "$cert_dir"
  chmod 700 /home/acme "$cert_dir"

  echo -e "  ${B}[3/4] 正在通过 Cloudflare DNS-01 验证向 Let's Encrypt 发起申请...${N}"
  local cf_domains_meta="${WORK_DIR}/cf_ssl_domains.json"
  python3 -c '
import json, os, time, sys, urllib.request
p = sys.argv[1]
dom = sys.argv[2]
tok = sys.argv[3]
cdir = sys.argv[4]
try:
    headers = {"Authorization": f"Bearer {tok}", "Content-Type": "application/json"}
    parts = dom.split(".")
    if len(parts) >= 2:
        root_zone = ".".join(parts[-2:])
        req = urllib.request.Request(f"https://api.cloudflare.com/client/v4/zones?name={root_zone}", headers=headers)
        with urllib.request.urlopen(req, timeout=8) as resp:
            zdata = json.loads(resp.read().decode())
            if zdata.get("result"):
                zid = zdata["result"][0]["id"]
                req2 = urllib.request.Request(f"https://api.cloudflare.com/client/v4/zones/{zid}/dns_records?name=_acme-challenge.{dom}", headers=headers)
                with urllib.request.urlopen(req2, timeout=8) as resp2:
                    rdata = json.loads(resp2.read().decode())
                    for rec in rdata.get("result", []):
                        rid = rec["id"]
                        req_del = urllib.request.Request(f"https://api.cloudflare.com/client/v4/zones/{zid}/dns_records/{rid}", headers=headers, method="DELETE")
                        urllib.request.urlopen(req_del, timeout=8)
except Exception:
    pass

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

  export CF_Token="${cf_token}"
  "$acme_cmd" --set-default-ca --server letsencrypt >/dev/null 2>&1 || true

  local issue_args=("--issue" "--dns" "dns_cf" "-d" "${domain}")
  [[ "$force" == "true" ]] && issue_args+=("--force")

  echo -e "  [+] 正在与 Cloudflare DNS 握手验证所有权..."
  if ! "$acme_cmd" "${issue_args[@]}"; then
    echo -e "  ${R}[✗] 证书签发失败，请检查 Cloudflare API Token 权限是否包含 Zone.DNS:Edit。${N}"
    return 1
  fi

  echo -e "  ${B}[4/4] 正在分发并安装证书到目标路径 (/home/acme/${domain})...${N}"
  "$acme_cmd" --install-cert -d "${domain}" \
    --key-file "${cert_dir}/privkey.pem" \
    --fullchain-file "${cert_dir}/fullchain.pem" \
    --reloadcmd "systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true" >/dev/null 2>&1 || true

  cp -f "${cert_dir}/fullchain.pem" "${cert_dir}/cert.crt" 2>/dev/null || true
  cp -f "${cert_dir}/privkey.pem" "${cert_dir}/private.key" 2>/dev/null || true
  chmod 600 "${cert_dir}"/* 2>/dev/null || true

  if [[ -s "${cert_dir}/fullchain.pem" && -s "${cert_dir}/privkey.pem" ]]; then
    echo -e "  ${G}🎉 恭喜！Cloudflare SSL 证书申请并部署成功！${N}"
    echo -e "${B}========================================${N}"
    echo -e "  托管域名:    ${B}${domain}${N}"
    echo -e "  公钥路径:    ${G}${cert_dir}/fullchain.pem${N}"
    echo -e "  私钥路径:    ${G}${cert_dir}/privkey.pem${N}"
    echo -e "  备用公钥:    ${G}${cert_dir}/cert.crt${N}"
    echo -e "  备用私钥:    ${G}${cert_dir}/private.key${N}"
    echo -e "  自动续期:    ${G}已默认开启 (由 acme.sh 每天定时静默检查并自动续期)${N}"
    echo -e "${B}========================================${N}"
    return 0
  else
    echo -e "  ${R}[✗] 证书文件分发异常，请检查 ${cert_dir} 写入权限。${N}"
    return 1
  fi
}

apply_cf_ssl_cert() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}  申请 Cloudflare SSL 证书 (acme.sh DNS-01)${N}"
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
        echo -e "      自动续期: ${G}默认开启 (由 acme.sh 每天定时静默检查并自动续期)${N}"
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

cf_ssl_menu() {
  while true; do
    echo
    echo -e "${B}========================================${N}"
    echo -e "${B}  Cloudflare SSL 证书申请与管理 (acme.sh)${N}"
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

apply_tuic_hy2_to_singbox() {
  local cert_domain="$1"
  local cert_file="$2"
  local key_file="$3"
  local is_insecure="${4:-false}"
  local quiet="${5:-false}"   # true=首次安装流程，节点明细不刷屏

  echo
  echo -e "  ${B}[+] 正在为 sing-box 原生内核配置 TUIC / Hysteria2 入站...${N}"

  local sb_conf="/etc/sing-box/config.json"
  if [[ ! -f "$sb_conf" ]]; then
    echo -e "  ${R}[!] sing-box 配置文件 ${sb_conf} 不存在！${N}"
    return 1
  fi

  local pip
  pip=$(public_ip || true)
  local node_server="$pip"
  [[ -z "$node_server" ]] && node_server="$cert_domain"

  # 自动探测已有的 TUIC 与 Hysteria2 端口与凭据（若已存在则严格继承；不存在则自动分配随机高位端口，免除询问）
  local exist_tuic_p exist_hy2_p exist_tuic_uuid exist_tuic_pwd exist_hy2_pwd
  if [[ -f "$sb_conf" ]]; then
    local probed_th
    probed_th=$(python3 -c "
import json
try:
    with open('$sb_conf') as f: c = json.load(f)
    t_p = ''
    h_p = ''
    t_uid = ''
    t_pass = ''
    h_pass = ''
    for ib in c.get('inbounds', []):
        if not isinstance(ib, dict): continue
        if ib.get('type') == 'tuic' and not t_p:
            t_p = str(ib.get('listen_port', ''))
            users = ib.get('users', [])
            if users and isinstance(users[0], dict):
                t_uid = users[0].get('uuid', '')
                t_pass = users[0].get('password', '')
        elif ib.get('type') == 'hysteria2' and not h_p:
            h_p = str(ib.get('listen_port', ''))
            users = ib.get('users', [])
            if users and isinstance(users[0], dict):
                h_pass = users[0].get('password', '')
    print(f'{t_p}|{h_p}|{t_uid}|{t_pass}|{h_pass}')
except Exception:
    print('||||')
" 2>/dev/null || echo "||||")
    exist_tuic_p=$(echo "$probed_th" | cut -d'|' -f1)
    exist_hy2_p=$(echo "$probed_th" | cut -d'|' -f2)
    exist_tuic_uuid=$(echo "$probed_th" | cut -d'|' -f3)
    exist_tuic_pwd=$(echo "$probed_th" | cut -d'|' -f4)
    exist_hy2_pwd=$(echo "$probed_th" | cut -d'|' -f5)
  fi

  local tuic_port="$exist_tuic_p"
  [[ -z "$tuic_port" ]] && tuic_port=$(rand_local_port)

  local hy2_port="$exist_hy2_p"
  [[ -z "$hy2_port" ]] && hy2_port=$(rand_local_port)
  while [[ "$hy2_port" == "$tuic_port" ]]; do
    hy2_port=$(rand_local_port)
  done

  local tuic_uuid="$exist_tuic_uuid"
  [[ -z "$tuic_uuid" ]] && tuic_uuid=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo "a1b2c3d4-e5f6-7a8b-9c0d-ef1234567890")

  local tuic_pass="$exist_tuic_pwd"
  [[ -z "$tuic_pass" ]] && tuic_pass=$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-8)

  local hy2_pass="$exist_hy2_pwd"
  [[ -z "$hy2_pass" ]] && hy2_pass=$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-8)

  echo -e "  [+] TUIC 监听端口: ${Y}${tuic_port}${N} (自动分配随机高位端口)"
  echo -e "  [+] Hysteria2 监听端口: ${Y}${hy2_port}${N} (自动分配随机高位端口)"

  cp -f "$sb_conf" "${sb_conf}.bak"
  SB_CONF="$sb_conf" \
  CERT_DOM="$cert_domain" \
  CERT_FILE="$cert_file" \
  KEY_FILE="$key_file" \
  TUIC_PORT="$tuic_port" \
  HY2_PORT="$hy2_port" \
  TUIC_UUID="$tuic_uuid" \
  TUIC_PASS="$tuic_pass" \
  HY2_PASS="$hy2_pass" \
  NODE_SERVER="$node_server" \
  IS_INSECURE="$is_insecure" \
  python3 <<'PYEOF'
import json, os, random, string

def gen_suffix():
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=4))

p = os.environ['SB_CONF']
try:
    with open(p, 'r') as f:
        conf = json.load(f)
except Exception:
    conf = {}

inbounds = conf.setdefault('inbounds', [])
tuic_p = int(os.environ['TUIC_PORT'])
hy2_p = int(os.environ['HY2_PORT'])

tuic_uuid = os.environ['TUIC_UUID']
tuic_pass = os.environ['TUIC_PASS']
hy2_pass = os.environ['HY2_PASS']

tuic_tag = None
hy2_tag = None
for ib in inbounds:
    if not isinstance(ib, dict): continue
    t = ib.get('type')
    tg = ib.get('tag', '')
    users = ib.get('users', [])
    if t == 'tuic' and not tuic_tag:
        tuic_tag = tg
        if ib.get('listen_port'): tuic_p = int(ib['listen_port'])
        if users and isinstance(users[0], dict):
            if users[0].get('uuid'): tuic_uuid = users[0]['uuid']
            if users[0].get('password'): tuic_pass = users[0]['password']
    elif t == 'hysteria2' and not hy2_tag:
        hy2_tag = tg
        if ib.get('listen_port'): hy2_p = int(ib['listen_port'])
        if users and isinstance(users[0], dict):
            if users[0].get('password'): hy2_pass = users[0]['password']

if not tuic_tag or tuic_tag == 'tuic-in':
    tuic_tag = f"tuic-{gen_suffix()}"
if not hy2_tag or hy2_tag == 'hy2-in':
    hy2_tag = f"hysteria2-{gen_suffix()}"

cleaned = []
for ib in inbounds:
    if not isinstance(ib, dict): continue
    t = ib.get('type')
    port = ib.get('listen_port', 0)
    tag = ib.get('tag', '')
    if t in ('tuic', 'hysteria2') or port in (tuic_p, hy2_p) or tag.startswith('tuic-') or tag.startswith('hysteria2-') or tag in ('tuic-in', 'hy2-in'):
        continue
    cleaned.append(ib)

cleaned.append({
    "type": "tuic",
    "tag": tuic_tag,
    "listen": "::",
    "listen_port": tuic_p,
    "users": [
        {
            "name": "admin",
            "uuid": tuic_uuid,
            "password": tuic_pass
        }
    ],
    "congestion_control": "bbr",
    "tls": {
        "enabled": True,
        "server_name": os.environ['CERT_DOM'],
        "alpn": ["h3"],
        "certificate_path": os.environ['CERT_FILE'],
        "key_path": os.environ['KEY_FILE']
    }
})

cleaned.append({
    "type": "hysteria2",
    "tag": hy2_tag,
    "listen": "::",
    "listen_port": hy2_p,
    "ignore_client_bandwidth": True,
    "users": [
        {
            "name": "admin",
            "password": hy2_pass
        }
    ],
    "tls": {
        "enabled": True,
        "server_name": os.environ['CERT_DOM'],
        "alpn": ["h3"],
        "certificate_path": os.environ['CERT_FILE'],
        "key_path": os.environ['KEY_FILE']
    }
})

conf['inbounds'] = cleaned
with open(p, 'w') as f:
    json.dump(conf, f, indent=2)

addrs_path = '/var/lib/sout/singbox_inbound_addrs.json'
try:
    with open(addrs_path, 'r') as af:
        addrs_data = json.load(af)
except Exception:
    addrs_data = {}

srv_host = os.environ.get('NODE_SERVER') or os.environ.get('CERT_DOM') or ''
cert_dom = os.environ.get('CERT_DOM', '')
addrs_data[tuic_tag] = [{
    'server': srv_host,
    'server_port': tuic_p,
    'tls': {
        'enabled': True,
        'insecure': os.environ.get('IS_INSECURE') == 'true',
        'server_name': cert_dom
    }
}]
addrs_data[hy2_tag] = [{
    'server': srv_host,
    'server_port': hy2_p,
    'tls': {
        'enabled': True,
        'insecure': os.environ.get('IS_INSECURE') == 'true',
        'server_name': cert_dom
    }
}]

try:
    os.makedirs(os.path.dirname(addrs_path), exist_ok=True)
    with open(addrs_path, 'w') as af:
        json.dump(addrs_data, af, indent=2)
except Exception:
    pass

try:
    with open('/var/lib/sout/.tuic_hy2_vars', 'w') as tf:
        tf.write(f"tuic_uuid='{tuic_uuid}'\ntuic_pass='{tuic_pass}'\nhy2_pass='{hy2_pass}'\ntuic_port={tuic_p}\nhy2_port={hy2_p}\n")
except Exception:
    pass
PYEOF

  if [[ -f "${WORK_DIR}/.tuic_hy2_vars" ]]; then
    source "${WORK_DIR}/.tuic_hy2_vars"
    rm -f "${WORK_DIR}/.tuic_hy2_vars"
  fi

  if command -v sing-box >/dev/null 2>&1 || [[ -x /usr/local/bin/sing-box ]]; then
    local sb_bin
    sb_bin="$(command -v sing-box 2>/dev/null || echo '/usr/local/bin/sing-box')"
    if ! "$sb_bin" check -c "$sb_conf" >/dev/null 2>&1; then
      echo -e "  ${R}[×] sing-box 配置文件校验失败，已自动回滚原配置！${N}"
      cp -f "${sb_conf}.bak" "$sb_conf"
      return 1
    fi
  fi
  rm -f "${sb_conf}.bak"

  systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true

  local insec_flag=0
  [[ "$is_insecure" == "true" ]] && insec_flag=1

  local tuic_link="tuic://${tuic_uuid}:${tuic_pass}@${node_server}:${tuic_port}?sni=${cert_domain}&alpn=h3&congestion_control=bbr&allow_insecure=${insec_flag}#TUIC-${cert_domain}"
  local hy2_link="hysteria2://${hy2_pass}@${node_server}:${hy2_port}?sni=${cert_domain}&insecure=${insec_flag}#Hy2-${cert_domain}"

  mkdir -p "$WORK_DIR"
  cat > "${WORK_DIR}/nodes_tuic_hy2.txt" <<NODEOF
TUIC 节点链接:
${tuic_link}

Hysteria2 节点链接:
${hy2_link}
NODEOF
  chmod 600 "${WORK_DIR}/nodes_tuic_hy2.txt"

  echo
  if [[ "$quiet" == "true" ]]; then
    # 首次安装：只报结果，不刷节点明细（链接已落盘，需要时用 sout tuic 查看）
    echo -e "  ${G}[✓] TUIC / Hysteria2 节点已创建完成${N}"
    echo -e "      节点链接已保存至: ${WORK_DIR}/nodes_tuic_hy2.txt"
  else
    echo -e "${G}================================================================${N}"
    echo -e "${G}  🎉 TUIC / Hysteria2 节点已成功在 sing-box 原生内核中创建！${N}"
    echo -e "${G}================================================================${N}"
    echo -e "  [TUIC 节点]"
    echo -e "  端口:        ${tuic_port} (UDP)"
    echo -e "  UUID:        ${tuic_uuid}"
    echo -e "  密码:        ${tuic_pass}"
    echo -e "  分享链接:    ${B}${tuic_link}${N}"
    echo
    echo -e "  [Hysteria2 节点]"
    echo -e "  端口:        ${hy2_port} (UDP)"
    echo -e "  密码:        ${hy2_pass}"
    echo -e "  分享链接:    ${B}${hy2_link}${N}"
    echo -e "${G}================================================================${N}"
    echo -e "  💡 节点链接已自动保存至: ${WORK_DIR}/nodes_tuic_hy2.txt"
  fi
  return 0
}

create_tuic_hy2_nodes() {
  echo
  echo -e "${B}========================================${N}"
  echo -e "${B}     创建 TUIC / Hysteria2 节点${N}"
  echo -e "${B}========================================${N}"

  local cur_backend
  cur_backend=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
  if [[ -z "$cur_backend" ]]; then
    if [[ -f /usr/local/s-ui/db/s-ui.db ]]; then cur_backend="s-ui"
    elif [[ -f /etc/sing-box/config.json ]]; then cur_backend="sing-box"
    fi
    [[ -n "$cur_backend" ]] && echo -n "$cur_backend" > "${WORK_DIR}/panel_mode" 2>/dev/null || true
  fi

  local sui_db="/usr/local/s-ui/db/s-ui.db"
  local sui_token=""
  local cur_port="" cur_path="" sui_api=""

  if [[ "$cur_backend" != "sing-box" ]]; then
    if [[ ! -f "$sui_db" ]]; then
      echo -e "  ${R}[!] 未检测到 s-ui 面板，请先安装并配置 s-ui。${N}"
      return 1
    fi

    sui_token=$(get_or_create_sui_token "$sui_db")
    if [[ -z "$sui_token" ]]; then
      echo -e "  ${R}[!] 未能获取到 s-ui API Token，请先在终端运行一次 sout 生成凭据。${N}"
      return 1
    fi

    cur_port=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPort'" 2>/dev/null || echo "8443")
    cur_path=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPath'" 2>/dev/null || echo "/app/")
    cur_path="/${cur_path#/}"
    [[ "$cur_path" != */ ]] && cur_path="${cur_path}/"
    sui_api="http://127.0.0.1:${cur_port}${cur_path}apiv2"
  fi

  local in_domain="${1:-}"
  local in_cert_file="${2:-}"
  local in_key_file="${3:-}"
  local in_insecure="${4:-false}"
  local in_quiet="${5:-false}"   # true=首次安装流程，节点明细不刷屏

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

  if [[ "$cur_backend" == "sing-box" ]]; then
    apply_tuic_hy2_to_singbox "$cert_domain" "$cert_file" "$key_file" "$is_insecure" "$in_quiet"
    return $?
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
  public_ip=$(public_ip || true)
  [[ -z "$public_ip" ]] && public_ip="$cert_domain"

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
            'server_name': cert_domain,
            'alpn': ['h3']
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
            'server_name': cert_domain,
            'alpn': ['h3']
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
        tuic_t = existing_tuic.get('tag') or ''
        if not (tuic_t.startswith('tuic-') and len(tuic_t) == 9):
            tuic_t = f"tuic-{gen_suffix()}"
        tuic_payload = {
            'id': existing_tuic['id'],
            'type': 'tuic',
            'tag': tuic_t,
            'tls_id': new_tls_id,
            'listen': '::',
            'listen_port': tuic_port,
            'congestion_control': cc,
            'addrs': tuic_addrs
        }
        api('POST', 'save', {'object': 'inbounds', 'action': 'edit', 'data': json.dumps(tuic_payload)})
        print(f"\033[32m[✓] TUIC 节点 [{tuic_t}] 已成功更新 (TLS_ID: {new_tls_id}, 拥塞控制: {cc}, 多域名: {pub_ip}:{tuic_port})\033[0m")
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
        hy2_t = existing_hy2.get('tag') or ''
        if not (hy2_t.startswith('hysteria2-') and len(hy2_t) == 14):
            hy2_t = f"hysteria2-{gen_suffix()}"
        hy2_payload = {
            'id': existing_hy2['id'],
            'type': 'hysteria2',
            'tag': hy2_t,
            'tls_id': new_tls_id,
            'listen': '::',
            'listen_port': hy2_port,
            'ignore_client_bandwidth': True,
            'addrs': hy2_addrs
        }
        api('POST', 'save', {'object': 'inbounds', 'action': 'edit', 'data': json.dumps(hy2_payload)})
        print(f"\033[32m[✓] Hysteria2 节点 [{hy2_t}] 已成功更新 (TLS_ID: {new_tls_id}, 忽略客户端带宽: 开启, 多域名: {pub_ip}:{hy2_port})\033[0m")
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
    echo -e "${B}  Cloudflare隧道连接与轻量流量分流管理${N}"
    echo -e "${B}========================================${N}"
    local en dom cf_st
    en=$(is_caddy_enabled)
    # 兼容 systemd 与 OpenRC：Alpine 无 systemctl，需回落到 rc-service / 进程探测
    if [[ "$INIT_SYS" == "systemd" ]]; then
      cf_st=$(systemctl is-active cloudflared 2>/dev/null || echo "inactive")
    else
      if rc-service cloudflared status >/dev/null 2>&1 || pgrep -f "cloudflared" >/dev/null 2>&1; then
        cf_st="active"
      else
        cf_st="inactive"
      fi
    fi
    
    if [[ "$en" == "true" ]]; then
      dom=$(json_get "$CADDY_META" domain)
      local tun_p
      tun_p=$(json_get "$CADDY_META" tunnel_port)
      [[ -z "$tun_p" ]] && tun_p="8081"

      local cf_desc
      if [[ "$cf_st" == "active" ]]; then
        cf_desc="${G}运行中${N}"
      else
        cf_desc="${R}已停止 [${cf_st}]${N}"
      fi

      echo -e "  反代状态:      ${G}已开启 (Cloudflare 隧道模式)${N}"
      echo -e "  隧道服务:      ${cf_desc}"
      echo -e "  分流网关:      ${G}内置运行中 (sout-server)${N}"
      echo -e "  托管域名:      ${B}${dom}${N}"
      echo -e "  本地回源:      ${Y}127.0.0.1:${tun_p}${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 查看轻量反代与分流详情"
      echo "  2) 重新配置隧道与域名 (修改 Token/域名/端口)"
      echo "  3) 查看 cloudflared 隧道运行日志"
      echo "  4) 重启 Cloudflare 隧道"
      echo "  5) 重新应用分流配置 (重启网关)"
      echo "  6) 关闭隧道反代 (恢复独立端口模式)"
      echo "  0) 返回上级菜单"
      echo
      read -rp "  请选择 [0-6]: " opt
      case "$opt" in
        1)
          if [[ -f "$CADDY_META" ]]; then
            local sout_p sui_p sub_p ws_p sout_port sui_port node_port
            sout_p=$(json_get "$CADDY_META" sout_path)
            sui_p=$(json_get "$CADDY_META" sui_path)
            sub_p=$(json_get "$CADDY_META" sub_path)
            ws_p=$(json_get "$CADDY_META" ws_path)
            sout_port=$(json_get "$CADDY_META" sout_port)
            sui_port=$(json_get "$CADDY_META" sui_port)
            node_port=$(json_get "$CADDY_META" node_port)
            [[ -z "$sout_port" ]] && sout_port="8899"
            [[ -z "$sui_port" ]] && sui_port="0"
            [[ -z "$node_port" ]] && node_port="2082"

            echo
            echo -e "${B}========================================${N}"
            echo -e "${B}  内置轻量反代网关与流量分流详情${N}"
            echo -e "${B}========================================${N}"
            echo -e "  网关正在监听:    ${Y}127.0.0.1:${tun_p}${N}"
            echo
            echo -e "  ${G}• 将 /${sout_p}/ 路径流量转发至:   127.0.0.1:${sout_port} (sout 管理面板)${N}"
            echo -e "    外网访问: https://${dom}/${sout_p}/"
            echo
            if is_sui_backend && [[ -n "$sui_p" && "$sui_port" -gt 0 ]]; then
              echo -e "  ${G}• 将 /${sui_p}/ 路径流量转发至:    127.0.0.1:${sui_port} (s-ui 面板)${N}"
              echo -e "    外网访问: https://${dom}/${sui_p}/"
              echo
            else
              echo -e "  ${G}• 核心后端:                       127.0.0.1:${node_port} (sing-box 原生内核)${N}"
              echo -e "    核心配置: /etc/sing-box/config.json"
              echo
            fi
            echo -e "  ${G}• 将 /${sout_p}/sub 路径流量转发至: 127.0.0.1:${sout_port} (订阅接口)${N}"
            echo -e "    订阅链接: https://${dom}/${sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
            if [[ -n "$ws_p" ]]; then
              echo
              echo -e "  ${G}• 将 /${ws_p}/ 路径流量转发至:     127.0.0.1:${node_port} (节点流量)${N}"
            fi
            echo
            echo -e "  ${G}• 将 / 根路径流量响应:            200 OK (伪装服务就绪)${N}"
            echo -e "${B}========================================${N}"
          fi
          pause ;;
        2) caddy_interactive_setup; pause ;;
        3)
          echo
          if command -v journalctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
            journalctl -u cloudflared -n 40 --no-pager
          elif [[ -f /var/log/cloudflared.err || -f /var/log/cloudflared.log ]]; then
            tail -n 40 /var/log/cloudflared.err /var/log/cloudflared.log 2>/dev/null
          else
            echo "未找到 cloudflared 运行日志"
          fi
          pause ;;
        4)
          echo -e "  正在重启 Cloudflare 隧道服务..."
          local cf_re_ok=false
          if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
            systemctl restart cloudflared 2>/dev/null && cf_re_ok=true
          elif command -v rc-service >/dev/null 2>&1; then
            (rc-service cloudflared restart 2>/dev/null || rc-service cloudflared start 2>/dev/null) && cf_re_ok=true
          fi
          [[ "$cf_re_ok" == "true" ]] && echo -e "  ${G}[✓] Cloudflare 隧道服务已成功重启${N}" || echo -e "  ${R}[×] 隧道服务重启失败${N}"
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
      echo -e "  💡 提示:       ${Y}强烈推荐开启 Cloudflare隧道连接与轻量流量分流 (免开端口/杜绝525)${N}"
      echo -e "${D}----------------------------------------${N}"
      echo "  1) 开启 Cloudflare 官方免费临时隧道 (免域名/免Token)"
      echo "  2) 使用固定域名 + 隧道 Token 配置"
      echo "  3) 重新应用分流配置 (重启网关)"
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
    echo -e "   9) Cloudflare隧道/轻量分流配置"
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

# 老机器升级上来时，s-ui 日志落盘与日志轮转可能尚未配置，这里幂等补一次
if [[ "$INIT_SYS" == "openrc" ]]; then
  ensure_sui_log_redirect >/dev/null 2>&1 || true
  install_log_rotation >/dev/null 2>&1 || true
fi  # 静默执行：结果只在需要时由终端菜单展示

# 如果是被 source 载入而非直接执行，直接返回，避免在无 TTY 环境下误入交互菜单死循环
if [[ "${BASH_SOURCE[0]}" != "${0}" ]]; then
  return 0 2>/dev/null || exit 0
fi

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
  restart_tunnel)
    shift
    if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
      systemctl restart cloudflared 2>/dev/null || true
    elif command -v rc-service >/dev/null 2>&1; then
      rc-service cloudflared stop >/dev/null 2>&1 || true
      rc-service cloudflared zap >/dev/null 2>&1 || true
      pkill -9 -f "/usr/local/bin/cloudflared" 2>/dev/null || true
      sleep 0.5
      rc-service cloudflared start >/dev/null 2>&1 || true
    fi
    reload_caddy_proxy >/dev/null 2>&1 || true
    echo "OK"
    ;;
  disable_tunnel|delete_tunnel|stop_tunnel)
    shift
    disable_caddy_proxy force
    ;;
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

