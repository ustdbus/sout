#!/usr/bin/env bash
set -e
mkdir -p /var/tmp && export TMPDIR=/var/tmp

REPO="${REPO:-ustdbus/sout}"
BIN="/usr/local/bin/sout-server"
WORK_DIR="/var/lib/sout"
[[ -d "/usr/local/sout" && ! -d "/var/lib/sout" ]] && cp -rf /usr/local/sout /var/lib/sout 2>/dev/null || true
WEB_PORT=43322

# ==============================================================================
# 统一 JSON 字段读取：替代 grep -oE/cut/awk 解析（键不存在或解析失败时输出空，由调用点 :- 兜底）
# ==============================================================================
json_get() {
  local f="${1:-}"
  local k="${2:-}"
  local def="${3:-}"
  if command -v sout-server >/dev/null 2>&1; then
    sout-server tool json-get "$f" "$k" "$def" 2>/dev/null || true
    return
  elif [[ -x "$BIN" ]]; then
    "$BIN" tool json-get "$f" "$k" "$def" 2>/dev/null || true
    return
  fi
  [[ ! -f "$f" ]] && { echo "$def"; return; }
  local val
  val=$(grep -oE "\"${k}\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9.]+|true|false|null)" "$f" 2>/dev/null | head -1 | awk -F: '{print $2}' | tr -d ' "\r\n' || true)
  if [[ -n "$val" && "$val" != "null" ]]; then
    echo "$val"
  else
    echo "$def"
  fi
}
if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行此脚本 (sudo ./install.sh 或 sudo bash ...)" >&2
  exit 1
fi

# ==============================================================================
# 纯 IPv6 环境自动配置公共 DNS64 + NAT64 服务与环境还原机制
# ==============================================================================
SOUT_NAT64_BACKUP="/etc/resolv.conf.sout.orig"

setup_nat64_if_pure_ipv6() {
  local has_v4=0
  local has_v6=0

  # 优先检测原生 IPv4 出口连通性并记录
  local detected_v4=""
  detected_v4=$(curl -s4m 3 https://api.ipify.org 2>/dev/null || \
                curl -s4m 3 https://checkip.amazonaws.com 2>/dev/null || \
                curl -s4m 3 http://v4.ident.me 2>/dev/null || true)
  if [[ -n "$detected_v4" ]]; then
    has_v4=1
  fi

  # 检测 IPv6 出口连通性并记录
  local detected_v6=""
  detected_v6=$(curl -s6m 3 https://api64.ipify.org 2>/dev/null || \
                curl -s6m 3 https://ipv6.icanhazip.com 2>/dev/null || \
                curl -s6m 3 https://v6.ident.me 2>/dev/null || true)
  if [[ -n "$detected_v6" ]]; then
    has_v6=1
  fi

  # 记录母机主公网 IP，持久化落盘至 /var/lib/sout/host_ip，杜绝后续流程中重复探测出错
  local primary_ip=""
  if [[ "$has_v4" -eq 1 ]]; then
    primary_ip="$detected_v4"
  elif [[ "$has_v6" -eq 1 ]]; then
    primary_ip="$detected_v6"
  fi

  mkdir -p /var/lib/sout
  if [[ -n "$primary_ip" ]]; then
    echo "$primary_ip" > /var/lib/sout/host_ip 2>/dev/null || true
    export HOST_PUBLIC_IP="$primary_ip"
  fi
  if [[ -n "$detected_v6" ]]; then
    echo "$detected_v6" > /var/lib/sout/host_ipv6 2>/dev/null || true
    export HOST_PUBLIC_IPV6="$detected_v6"
  fi
  if [[ -n "$detected_v4" ]]; then
    echo "$detected_v4" > /var/lib/sout/host_ipv4 2>/dev/null || true
    export HOST_PUBLIC_IPV4="$detected_v4"
  fi

  mkdir -p /var/lib/sout 2>/dev/null || true
  if [[ "$has_v4" -eq 1 && "$has_v6" -eq 1 ]]; then
    echo "dual_stack" > /var/lib/sout/network_mode 2>/dev/null || true
    export SOUT_NETWORK_MODE="dual_stack"
  elif [[ "$has_v4" -eq 0 && "$has_v6" -eq 1 ]]; then
    echo "ipv6_only" > /var/lib/sout/network_mode 2>/dev/null || true
    export SOUT_NETWORK_MODE="ipv6_only"
  else
    echo "ipv4_only" > /var/lib/sout/network_mode 2>/dev/null || true
    export SOUT_NETWORK_MODE="ipv4_only"
  fi

  # 若无公网 IPv4 且有 IPv6，则判定为纯 IPv6 机器
  if [[ "$has_v4" -eq 0 && "$has_v6" -eq 1 ]]; then
    export IS_PURE_IPV6=1
    # 检查是否已包含主流 NAT64 / DNS64 前缀
    if ! grep -qE '(2001:67c:2b0|2a00:1098:2b|2a01:4f8|2a01:4f9|2606:4700:4700::64|2001:4860:4860::64)' /etc/resolv.conf 2>/dev/null; then
      echo "  [+] 检测到当前服务器为纯 IPv6 环境 (无原生公网 IPv4)"
      echo "      正在自动配置公共 DNS64 + NAT64 服务以保障安装全流程与网络互联..."
      if [[ ! -f "$SOUT_NAT64_BACKUP" ]]; then
        cp /etc/resolv.conf "$SOUT_NAT64_BACKUP" 2>/dev/null || true
      fi
      cat << 'EOF' > /etc/resolv.conf
nameserver 2a01:4f8:c2c:123f::1
nameserver 2a00:1098:2c::1
nameserver 2a00:1098:2b::1
EOF
      export SOUT_CONFIGURED_NAT64=1
      echo "  [✓] 已配置公共 DNS64 + NAT64 服务"
    else
      echo "  [✓] 检测到当前服务器为纯 IPv6 环境 (无原生公网 IPv4)"
      echo "      公共 DNS64 + NAT64 服务已在运行中 (保障 IPv4 互联互通)"
    fi
  else
    export IS_PURE_IPV6=0
  fi
}

restore_nat64_if_temporary() {
  if [[ "${IS_PURE_IPV6:-0}" == "1" ]]; then
    # 纯 IPv6 机器必须依赖公共 DNS64/NAT64 才能访问纯 IPv4 资源，安装完成后永久保持该解析配置
    rm -f "$SOUT_NAT64_BACKUP" 2>/dev/null || true
    return 0
  fi
  if [[ "${SOUT_CONFIGURED_NAT64:-0}" == "1" && -f "$SOUT_NAT64_BACKUP" ]]; then
    echo
    echo "  [+] 安装与配置流程已全部就绪，正在安全还原安装前备份的 DNS 配置..."
    cp "$SOUT_NAT64_BACKUP" /etc/resolv.conf 2>/dev/null || true
    rm -f "$SOUT_NAT64_BACKUP" 2>/dev/null || true
    echo "  [✓] 临时 DNS64 + NAT64 服务已安全清理并恢复原始网络配置"
  fi
}

setup_nat64_if_pure_ipv6

# 自动平滑迁移旧工作目录
if [[ -d "/var/lib/fanout" && ! -d "/var/lib/sout" ]]; then
  mv /var/lib/fanout /var/lib/sout 2>/dev/null || true
fi

detect_init() {
  if [[ -f /etc/alpine-release ]] || command -v rc-service >/dev/null 2>&1; then
    echo "openrc"
  elif command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    echo "systemd"
  else
    echo "systemd"
  fi
}
INIT_SYS=$(detect_init)


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



detect_pkg_mgr() {
  if command -v apt-get >/dev/null 2>&1; then
    echo "apt"
  elif command -v dnf >/dev/null 2>&1; then
    echo "dnf"
  elif command -v yum >/dev/null 2>&1; then
    echo "yum"
  elif command -v pacman >/dev/null 2>&1; then
    echo "pacman"
  elif command -v zypper >/dev/null 2>&1; then
    echo "zypper"
  elif command -v apk >/dev/null 2>&1; then
    echo "apk"
  else
    echo "unknown"
  fi
}

install_pkgs() {
  local mgr="$1"
  shift
  local pkgs=("$@")
  case "$mgr" in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq && apt-get install -y -qq "${pkgs[@]}"
      ;;
    dnf)
      dnf install -y -q "${pkgs[@]}"
      ;;
    yum)
      yum install -y -q "${pkgs[@]}"
      ;;
    pacman)
      pacman -Sy --noconfirm "${pkgs[@]}"
      ;;
    zypper)
      zypper --non-interactive install -y "${pkgs[@]}"
      ;;
    apk)
      apk add --no-cache "${pkgs[@]}"
      ;;
    *)
      return 1
      ;;
  esac
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

check_and_install_deps() {
  echo
  echo "================================================================"
  echo "  🚀 开始安装部署 sout - s-ui 动态家宽出口插件"
  echo "================================================================"
  echo "[1/6] 检查系统基础依赖..."
  local mgr
  mgr=$(detect_pkg_mgr)
  local needed=()
  local base_cmds=(curl tar ip ss)
  if check_sui; then
    base_cmds+=(sqlite3)
  fi
  for cmd in "${base_cmds[@]}"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      needed+=("$cmd")
    fi
  done

  # 在 Alpine (apk) 环境下，检测 tar 是否为 busybox 版本。如果是，确保安装 GNU tar 以支持完整选项
  if [[ "$mgr" == "apk" ]]; then
    if tar --version 2>&1 | grep -iq "busybox"; then
      needed+=("tar")
    fi
    if ! command -v ss >/dev/null 2>&1; then
      needed+=("ss")
    fi
    if ! ip netns list >/dev/null 2>&1; then
      needed+=("ip")
    fi
  fi

  if [[ ${#needed[@]} -gt 0 ]]; then
    echo "      发现缺失或需要补充的命令: ${needed[*]}，正在匹配软件包..."
    local pkgs=()
    for cmd in "${needed[@]}"; do
      case "$cmd" in
        curl) pkgs+=("curl") ;;
        tar)  pkgs+=("tar") ;;
        ip)   pkgs+=("iproute2") ;;
        ss)   pkgs+=("iproute2") ;;
        bash) pkgs+=("bash") ;;
        sqlite3) [[ "$mgr" == "apk" ]] && pkgs+=("sqlite") || pkgs+=("sqlite3") ;;
      esac
    done

    # 数组去重
    local uniq_pkgs=()
    for p in "${pkgs[@]}"; do
      local dup=0
      for u in "${uniq_pkgs[@]}"; do
        [[ "$u" == "$p" ]] && dup=1 && break
      done
      [[ $dup -eq 0 ]] && uniq_pkgs+=("$p")
    done

    echo "      正在自动安装: ${uniq_pkgs[*]}"
    install_pkgs "$mgr" "${uniq_pkgs[@]}" || {
      echo "      自动安装依赖失败，请手动安装: ${uniq_pkgs[*]}" >&2
      exit 1
    }
  else
    echo "      基础依赖已完整。"
  fi
}

check_sui() {
  if [[ -f /usr/local/s-ui/db/s-ui.db ]] || [[ -f /usr/local/s-ui/s-ui ]] || command -v sui >/dev/null 2>&1 || [[ -f /usr/local/s-ui/sui ]]; then
    return 0
  fi
  return 1
}

check_singbox() {
  if /usr/local/bin/sing-box version >/dev/null 2>&1; then
    return 0
  fi
  if command -v sing-box >/dev/null 2>&1 && sing-box version >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

install_singbox() {
  echo
  echo "  [+] 正在准备 sout 内嵌 sing-box 轻量内核运行配置..."

  # 清理可能残留的历史独立外部 sing-box 服务，避免双进程争抢内存与端口
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    systemctl stop sing-box >/dev/null 2>&1 || true
    systemctl disable sing-box >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/sing-box.service
    systemctl daemon-reload >/dev/null 2>&1 || true
  elif command -v rc-service >/dev/null 2>&1; then
    rc-service sing-box stop >/dev/null 2>&1 || true
    rc-update del sing-box default >/dev/null 2>&1 || true
    rm -f /etc/init.d/sing-box
  fi
  pkill -9 -f "supervise-daemon.*sing-box" >/dev/null 2>&1 || true

  # 初始化目录和精简配置模板
  mkdir -p /etc/sing-box
  if [[ ! -f /etc/sing-box/config.json ]]; then
    cat > /etc/sing-box/config.json <<'SBCONF'
{
  "log": {
    "level": "warn",
    "timestamp": true
  },
  "inbounds": [],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    },
    {
      "type": "block",
      "tag": "block"
    }
  ],
  "route": {
    "rules": []
  }
}
SBCONF
    chmod 644 /etc/sing-box/config.json
  fi

  mkdir -p "$WORK_DIR"
  echo "sing-box" > "${WORK_DIR}/panel_mode"

  echo
  echo "================================================================"
  echo "  [✓] sout 内嵌 sing-box 轻量内核已就绪！继续进行 sout 插件安装..."
  echo "================================================================"
  echo
  return 0
}

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

  # 5. 写入独立配置文件与 /etc/sysctl.conf
  echo "$conf_content" > /etc/sysctl.d/99-sout.conf 2>/dev/null || true
  if [[ -f /etc/sysctl.conf ]]; then
    sed -i '/# === SOUT SYSCTL START ===/,/# === SOUT SYSCTL END ===/d' /etc/sysctl.conf 2>/dev/null || true
    echo "$conf_content" >> /etc/sysctl.conf 2>/dev/null || true
  fi
  sysctl -p /etc/sysctl.d/99-sout.conf >/dev/null 2>&1 || sysctl -p >/dev/null 2>&1 || true

  # 6. 回读校验与清理
  rm -f /tmp/.sout_sysctl_kv 2>/dev/null || true

  # 7. 低内存 VPS/容器防爆保护
  optimize_low_memory "$mem_total_mb"
}

optimize_low_memory() {
  # 1. 清理 /tmp 与 /var/tmp 历史残留的 tar.gz 与二进制 (释放内存)
  rm -f /tmp/sout-linux-*.tar.gz /tmp/sout-server /tmp/s.sh /tmp/f.sh 2>/dev/null || true
  rm -f /var/tmp/sout-linux-*.tar.gz /var/tmp/sout-server /var/tmp/tmp.* 2>/dev/null || true

  # 2. 彻底清除历史强加的 MemorySwapMax=0，避免突发瞬时尖峰因 0 容错直接触发内核 OOM Killer
  sed -i '/MemorySwapMax/d' /etc/systemd/system/*.service.d/override.conf 2>/dev/null || true

  # 3. 调低 swappiness（从默认 100 降为 30），防止过早向虚拟 Swap 剧烈换页
  sysctl -w vm.swappiness=30 >/dev/null 2>&1 || true

  # 4. 限制 journald 运行时内存
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

cleanup_sout() {
  echo "      正在清理并卸载 sout 脚本与服务..."
  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl stop sout 2>/dev/null || true
    systemctl disable sout 2>/dev/null || true
    systemctl stop fanout 2>/dev/null || true
    systemctl disable fanout 2>/dev/null || true
    rm -f /etc/systemd/system/sout.service /etc/systemd/system/fanout.service
    systemctl daemon-reload 2>/dev/null || true
  else
    rc-service sout stop 2>/dev/null || true
    rc-update del sout default 2>/dev/null || true
    rc-service fanout stop 2>/dev/null || true
    rc-update del fanout default 2>/dev/null || true
    rm -f /etc/init.d/sout /etc/init.d/fanout
  fi
  # 停止并彻底清理 Caddy 与 cloudflared
  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl stop caddy 2>/dev/null || true
    systemctl disable caddy 2>/dev/null || true
    systemctl stop cloudflared 2>/dev/null || true
    systemctl disable cloudflared 2>/dev/null || true
    rm -f /etc/systemd/system/caddy.service /etc/systemd/system/cloudflared.service 2>/dev/null || true
  else
    rc-service caddy stop 2>/dev/null || true
    rc-update del caddy default 2>/dev/null || true
    rc-service cloudflared stop 2>/dev/null || true
    rc-update del cloudflared default 2>/dev/null || true
    rm -f /etc/init.d/caddy /etc/init.d/cloudflared 2>/dev/null || true
  fi
  rm -rf /etc/caddy /var/lib/caddy /var/log/caddy /usr/local/bin/caddy /usr/local/bin/cloudflared /usr/local/bin/sout-quick-tunnel /var/log/cloudflared* 2>/dev/null || true

  # 还原内核与网络系统参数备份
  restore_sysctl

  rm -f "$BIN" /usr/local/bin/sout-server /usr/local/bin/fanout /usr/local/bin/f /usr/local/bin/sout /usr/local/bin/sout-cli 2>/dev/null || true
  echo "      sout 及相关反代隧道组件已彻底清理完毕。"
}

# ==============================================================================
# [第一步] 一开始首先询问 Cloudflare隧道连接与轻量流量分流配置
# ==============================================================================
WANT_TUNNEL="n"
TUNNEL_DOMAIN=""
TUNNEL_TOKEN=""
TUNNEL_PORT="8081"
APPLY_CERT="n"
CF_DNS_KEY=""

ask_tunnel_setup() {
  echo
  echo "================================================================"
  echo "  💡 提示：NAT 机推荐开启 Cloudflare 隧道代理，正常 VPS 可不启用。"
  echo "  👉 提示：直接回车将自动开启 Cloudflare 官方免费临时隧道 (即开即用)。"
  echo "  📌 提示：如使用固定隧道，请提前准备好："
  echo "       1) 已在 Cloudflare 托管的访问域名"
  echo "       2) Cloudflare 隧道 Token"
  echo "       3) 本地回源端口 [默认 8081]"
  echo "       4) (可选) Cloudflare API 令牌 [需含 区域.DNS:编辑 权限，用于签发证书]"
  echo "================================================================"
  local prompt_choice=""
  if [[ -t 0 ]]; then
    read -rp "  是否配置 Cloudflare隧道连接与轻量流量分流？[y/N]: " prompt_choice
  else
    if [[ -c /dev/tty ]]; then
      read -rp "  是否配置 Cloudflare隧道连接与轻量流量分流？[y/N]: " prompt_choice < /dev/tty || prompt_choice="n"
    fi
  fi

  if [[ "${prompt_choice,,}" == "y" || "${prompt_choice,,}" == "yes" ]]; then
    WANT_TUNNEL="y"
    echo
    echo "  [Cloudflare 隧道参数设置]"
    if [[ -t 0 ]]; then
      read -rp "  1. 请输入您的访问域名 (如 example.com，直接回车则启用免费临时隧道): " TUNNEL_DOMAIN
      read -rp "  2. 请输入 Cloudflare 隧道 Token (直接回车则启用免费临时隧道): " TUNNEL_TOKEN
      read -rp "  3. 请输入本地回源端口 [默认 8081]: " TUNNEL_PORT
    else
      if [[ -c /dev/tty ]]; then
        read -rp "  1. 请输入您的访问域名 (如 example.com，直接回车则启用免费临时隧道): " TUNNEL_DOMAIN < /dev/tty
        read -rp "  2. 请输入 Cloudflare 隧道 Token (直接回车则启用免费临时隧道): " TUNNEL_TOKEN < /dev/tty
        read -rp "  3. 请输入本地回源端口 [默认 8081]: " TUNNEL_PORT < /dev/tty
      fi
    fi
    TUNNEL_DOMAIN=$(echo "$TUNNEL_DOMAIN" | tr -d ' \r\n')
    TUNNEL_TOKEN=$(echo "$TUNNEL_TOKEN" | tr -d ' \r\n')
    TUNNEL_PORT=$(echo "$TUNNEL_PORT" | tr -d ' \r\n')
    TUNNEL_PORT="${TUNNEL_PORT:-8081}"

    if [[ -z "$TUNNEL_DOMAIN" || -z "$TUNNEL_TOKEN" ]]; then
      TUNNEL_DOMAIN=""
      TUNNEL_TOKEN=""
      APPLY_CERT="n"
      CF_DNS_KEY=""
      echo "  [✓] 已选择开启 Cloudflare 官方免费临时隧道 (免域名/免Token)，将在核心组件就绪后自动分配！"
    else
      echo "  [✓] 隧道参数已保存！"
      echo
      if [[ -t 0 ]]; then
        read -rp "  4. 请输入 Cloudflare API 令牌 [需含 Zone.DNS:Edit 权限，回车跳过]: " CF_DNS_KEY
      else
        if [[ -c /dev/tty ]]; then
          read -rp "  4. 请输入 Cloudflare API 令牌 [需含 Zone.DNS:Edit 权限，回车跳过]: " CF_DNS_KEY < /dev/tty || CF_DNS_KEY=""
        fi
      fi
      CF_DNS_KEY=$(echo "$CF_DNS_KEY" | tr -d ' \r\n')
      if [[ -n "$CF_DNS_KEY" ]]; then
        APPLY_CERT="y"
        echo "  [✓] Cloudflare 令牌已记录，部署时将通过 DNS-01 验证自动签发证书。"
      else
        APPLY_CERT="n"
        echo "  [✓] 已跳过证书申请，将自动配置常规节点 (vless-argo 与 vless-reality)。"
      fi
    fi
  fi
}

check_and_install_deps
ask_tunnel_setup

# ==============================================================================
# [第二步] 询问 / 检测并安装后端 (s-ui 面板 / sing-box 原生内核)
# ==============================================================================
SUI_INSTALLED_BY_US=0

ensure_backend() {
  local has_sui=0
  check_sui && has_sui=1

  echo
  echo "================================================================"
  if [[ "$has_sui" -eq 1 ]]; then
    echo "  [+] 检测到当前 VPS 已安装 s-ui 面板"
  else
    echo "  [!] 检测到当前 VPS 尚未安装 s-ui 面板！"
  fi
  echo
  echo "  sout 支持两种节点运行后端："
  echo "    1) 安装 / 对接 s-ui 面板"
  echo "    2) 仅使用 sout 内嵌 sing-box（推荐）"
  echo "    3) 不安装退出"
  echo
  echo "  直接回车默认选择 [2]"
  echo "================================================================"
  echo

  local choice=""
  # 低内存机器（<=180MB）直接用 sout 内嵌 sing-box，不再询问：
  # s-ui 面板自身常驻约 50MB，在 128MB 级机器上会明显挤压转发与出口进程的内存
  local _mem_now=0
  _mem_now=$(grep -i 'MemTotal' /proc/meminfo 2>/dev/null | awk '{print int($2/1024)}')
  if [[ "${_mem_now:-0}" -gt 0 && "${_mem_now:-0}" -le 180 ]]; then
    choice="2"
    echo "  [!] 检测到内存仅 ${_mem_now}MB，已自动选择 [2] 仅使用 sout 内嵌 sing-box（推荐）"
    echo "      (s-ui 面板常驻约 50MB，低内存机型上会挤压转发性能)"
  elif [[ -t 0 ]]; then
    read -rp "  请选择操作 [1-3] (默认: 2): " choice
  else
    if [[ -c /dev/tty ]]; then
      read -rp "  请选择操作 [1-3] (默认: 2): " choice < /dev/tty || choice="2"
    else
      choice="2"
    fi
  fi

  choice=$(echo "$choice" | tr -d ' \r\n')
  [[ -z "$choice" ]] && choice="2"

  case "$choice" in
    1)
      if [[ "$has_sui" -eq 1 ]]; then
        echo "  [✓] 检测到已存在 s-ui 面板，直接对接当前 s-ui 面板！"
        mkdir -p "$WORK_DIR"
        echo "s-ui" > "${WORK_DIR}/panel_mode"
        if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
          systemctl daemon-reload >/dev/null 2>&1 || true
          systemctl enable s-ui >/dev/null 2>&1 || true
          systemctl restart s-ui >/dev/null 2>&1 || true
        elif command -v rc-service >/dev/null 2>&1; then
          rc-update add s-ui default >/dev/null 2>&1 || true
          rc-service s-ui restart >/dev/null 2>&1 || true
        fi
        sleep 1.5
        return 0
      fi
      echo
      echo "  [+] 正在自动检测当前服务器系统架构并安装官方 s-ui 面板..."
      echo "      (官方仓库: https://github.com/alireza0/s-ui)"
      echo
      local sui_log="/tmp/sui_install.log"
      rm -f "$sui_log" 2>/dev/null || true
      if [[ -c /dev/tty ]]; then
        bash <(curl -Ls https://raw.githubusercontent.com/alireza0/s-ui/master/install.sh) < /dev/tty 2>&1 | tee "$sui_log" || true
      else
        bash <(curl -Ls https://raw.githubusercontent.com/alireza0/s-ui/master/install.sh) 2>&1 | tee "$sui_log" || true
      fi
      
      if check_sui; then
        SUI_INSTALLED_BY_US=1
        mkdir -p "$WORK_DIR"
        echo "s-ui" > "${WORK_DIR}/panel_mode"
        # 解析官方日志中是否打印了随机生成的账号和密码
        if [[ -f "$sui_log" ]]; then
          local parsed_u parsed_p
          parsed_u=$(sed -r "s/\x1B\[[0-9;]*[a-zA-Z]//g" "$sui_log" | grep -E '^username:' | tail -1 | cut -d: -f2 | tr -d ' \r\n')
          parsed_p=$(sed -r "s/\x1B\[[0-9;]*[a-zA-Z]//g" "$sui_log" | grep -E '^password:' | tail -1 | cut -d: -f2 | tr -d ' \r\n')
          rm -f "$sui_log" 2>/dev/null || true
          if [[ -n "$parsed_u" && -n "$parsed_p" ]]; then
            SUI_ADMIN_USER="$parsed_u"
            SUI_ADMIN_PASS="$parsed_p"
            SUI_PASS_IS_RANDOM=1
          fi
        fi

        # s-ui 官方 OpenRC init 不会配置日志落盘，这里补全（systemd 平台自动跳过）
        ensure_sui_log_redirect
        # 轻量内核宿主机：把日志轮转一并注册
        install_log_rotation

        if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
          systemctl daemon-reload >/dev/null 2>&1 || true
          systemctl enable s-ui >/dev/null 2>&1 || true
          systemctl restart s-ui >/dev/null 2>&1 || true
        elif command -v rc-service >/dev/null 2>&1; then
          rc-update add s-ui default >/dev/null 2>&1 || true
          rc-service s-ui restart >/dev/null 2>&1 || true
        fi
        sleep 1.5

        echo
        echo "================================================================"
        echo "  [✓] s-ui 面板安装完成并就绪！继续进行 sout 插件安装..."
        echo "================================================================"
        echo
      else
        echo
        echo "  [!] 未能检测到 s-ui 面板组件（可能安装被取消或中断）。"
        local continue_choice=""
        if [[ -t 0 ]]; then
          read -rp "  是否仍要继续安装 sout？[y/N]: " continue_choice
        else
          read -rp "  是否仍要继续安装 sout？[y/N]: " continue_choice < /dev/tty || continue_choice="n"
        fi
        if [[ "${continue_choice,,}" != "y" ]]; then
          cleanup_sout
          echo "  已退出安装。"
          exit 0
        fi
      fi
      ;;
    2)
      install_singbox
      ;;
    3)
      echo
      echo "  [-] 您选择了取消安装，正在退出..."
      cleanup_sout
      exit 0
      ;;
    *)
      echo "  [-] 输入无效，已取消安装并退出。"
      cleanup_sout
      exit 0
      ;;
  esac
}

# 全局变量记录 s-ui 登录凭据与随机生成状态
SUI_ADMIN_USER=""
SUI_ADMIN_PASS=""
SUI_PASS_IS_RANDOM=0

ensure_backend

# 若未抓取到随机用户名（如已预装或用户在官方脚本中自定义设置），从数据库读取用户名
if [[ -z "$SUI_ADMIN_USER" && -f "/usr/local/s-ui/db/s-ui.db" ]] && command -v sqlite3 >/dev/null 2>&1; then
  SUI_ADMIN_USER=$(sqlite3 "/usr/local/s-ui/db/s-ui.db" "SELECT username FROM users LIMIT 1;" 2>/dev/null || echo "admin")
fi
[[ -z "$SUI_ADMIN_USER" ]] && SUI_ADMIN_USER="admin"

seed_settings() {
  local target="${WORK_DIR}/settings.json"
  if [[ -f "$target" ]]; then
    return
  fi
  cat > "$target" <<SEEOF
{
  "port": ${WEB_PORT},
  "listen_addr": "0.0.0.0"
}
SEEOF
  chmod 600 "$target"
}

svc_install() {
  echo "      正在注册系统服务 (${INIT_SYS})..."
  local sout_env_systemd=""
  local sout_env_openrc=""

  if [[ "$INIT_SYS" == systemd ]]; then
    cat > /etc/systemd/system/sout.service <<SVCEOF
[Unit]
Description=sout - s-ui 动态家宽出口插件 (VPN Gate)
After=network.target network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
${sout_env_systemd}
ExecStart=${BIN} -dir ${WORK_DIR}
WorkingDirectory=${WORK_DIR}
Restart=always
RestartSec=3
LimitNOFILE=65536
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW

[Install]
WantedBy=multi-user.target
SVCEOF
    systemctl daemon-reload
    rm -f /etc/systemd/system/fanout.service 2>/dev/null || true
  else
    local supervisor_line="command_background=\"yes\""
    if command -v supervise-daemon >/dev/null 2>&1; then
      supervisor_line="supervisor=\"supervise-daemon\""
    fi
    cat > /etc/init.d/sout <<OPENRCEOF
#!/sbin/openrc-run
name="sout"
description="sout - s-ui 动态家宽出口插件"
${sout_env_openrc}
${supervisor_line}
command="/usr/local/bin/sout-server"
command_args="-dir /var/lib/sout"
pidfile="/run/sout.pid"
retry="TERM/3/KILL/3"
output_log="/var/log/sout.log"
error_log="/var/log/sout.log"
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
OPENRCEOF
    chmod +x /etc/init.d/sout
    rm -f /etc/init.d/fanout 2>/dev/null || true
  fi
}

svc_enable_start() {
  echo "      正在启动服务..."
  local web_p="${WEB_PORT:-43322}"
  if [[ -f "${WORK_DIR}/settings.json" ]]; then
    local parsed_p
    parsed_p=$(json_get "${WORK_DIR}/settings.json" port)
    [[ -n "$parsed_p" ]] && web_p="$parsed_p"
  fi
  kill_port "$web_p"

  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl enable sout >/dev/null 2>&1 || true
    systemctl restart sout
  else
    rc-update add sout default >/dev/null 2>&1 || true
    rc-service sout stop >/dev/null 2>&1 || true
    rc-service sout zap >/dev/null 2>&1 || true
    sleep 0.3
    rc-service sout start
  fi
}

svc_is_active() {
  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl is-active sout >/dev/null 2>&1
  else
    rc-service sout status >/dev/null 2>&1
  fi
}

svc_logs_hint() {
  if [[ "$INIT_SYS" == systemd ]]; then
    echo "journalctl -u sout -n 30 --no-pager"
  else
    echo "tail -n 30 /var/log/sout.err"
  fi
}

MGR=$(detect_pkg_mgr)


echo "[2/6] 获取 sout 二进制文件..."
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "      暂不支持该架构: $ARCH" >&2; exit 1 ;;
esac

if ([[ -f main.go ]] || [[ -f cmd/sout-server/main.go ]]) && command -v go >/dev/null; then
  echo "      检测到源码环境，正在本地编译 (内嵌 sing-box 1.14 + gVisor)..."
  BUILD_TARGET="."
  [[ -d cmd/sout-server ]] && BUILD_TARGET="./cmd/sout-server"
  CGO_ENABLED=0 go build -trimpath -tags "with_gvisor with_quic netgo osusergo" -ldflags "-s -w" -o "$BIN" "$BUILD_TARGET"
else
  echo "      正在拉取预编译包 (${GOARCH})..."
  TMP=$(mktemp -d -p /var/tmp)
  sout_urls=(
    "https://github.com/${REPO}/releases/latest/download/sout-linux-${GOARCH}.tar.gz"
    "https://ghproxy.net/https://github.com/${REPO}/releases/latest/download/sout-linux-${GOARCH}.tar.gz"
  )
  sout_dl_ok=0

  # 若本机运行着旧版且有就绪 SOCKS5 代理端口，优先尝试带代理高速拉取
  active_socks_port=$(grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' "${WORK_DIR}/state.json" 2>/dev/null | head -1 | grep -oE '[0-9]+' || true)
  for u in "${sout_urls[@]}"; do
    if [[ -n "$active_socks_port" ]] && (echo > /dev/tcp/127.0.0.1/"$active_socks_port") >/dev/null 2>&1; then
      if curl -fsSL --socks5-hostname 127.0.0.1:"$active_socks_port" --connect-timeout 8 --max-time 60 "$u" -o "$TMP/f.tar.gz" 2>/dev/null && tar -tzf "$TMP/f.tar.gz" >/dev/null 2>&1; then
        sout_dl_ok=1
        break
      fi
    fi
    if curl -fL -# --connect-timeout 10 --max-time 120 "$u" -o "$TMP/f.tar.gz" && tar -tzf "$TMP/f.tar.gz" >/dev/null 2>&1; then
      sout_dl_ok=1
      break
    fi
  done
  if [[ "$sout_dl_ok" -eq 0 ]]; then
    echo "      下载失败: 无法获取 sout-linux-${GOARCH}.tar.gz，请检查网络！" >&2
    rm -rf "$TMP"
    exit 1
  fi
  tar --no-same-owner -xzf "$TMP/f.tar.gz" -C "$TMP" 2>/dev/null || tar xzf "$TMP/f.tar.gz" -C "$TMP"

  # 下载并解包成功后，安全停止旧服务并执行毫秒级替换
  svc_stop 2>/dev/null || true
  pkill -9 -f "/usr/local/bin/sout-server" 2>/dev/null || true
  pkill -9 -f "sout-server" 2>/dev/null || true
  sleep 0.5

  if [[ -f "$TMP/sout-server" ]]; then
    install -m 755 "$TMP/sout-server" "$BIN"
  elif [[ -f "$TMP/sout" ]]; then
    install -m 755 "$TMP/sout" "$BIN"
  fi
  if [[ -f "$TMP/s.sh" ]]; then
    install -m 755 "$TMP/s.sh" /usr/local/bin/sout
    rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
  elif [[ -f "$TMP/f.sh" ]]; then
    install -m 755 "$TMP/f.sh" /usr/local/bin/sout
    rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
  fi
  rm -rf "$TMP"
fi

echo "[3/6] 准备节点管理后端..."
backend_kind=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
if [[ "$backend_kind" == "s-ui" ]] || check_sui; then
  echo "      已就绪: s-ui 面板接管模式"
else
  echo "      已就绪: sout 内嵌 sing-box 轻量内核接管模式"
fi

echo "[4/6] 准备网络运行环境并优化内核套接字/UDP缓冲区..."
apply_sysctl_optimization
detected_cc=$(get_tcp_congestion)
echo "      当前系统生效的 TCP 拥塞控制算法: ${detected_cc}"

echo "[5/6] 部署服务与终端管理命令..."
if [[ -f s.sh ]]; then
  install -m 755 s.sh /usr/local/bin/sout
elif [[ -f f.sh ]]; then
  install -m 755 f.sh /usr/local/bin/sout
fi
rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
mkdir -p "$WORK_DIR"
chmod 700 "$WORK_DIR"
if [[ -x "$BIN" ]]; then
  "$BIN" -version 2>/dev/null | awk '{print $2}' > "${WORK_DIR}/version" 2>/dev/null || true
fi

# 确保持久化 panel_mode，若非 s-ui 则统一由 sout-server 内嵌 sing-box 单核接管
if [[ "$backend_kind" == "s-ui" ]] || check_sui; then
  echo -n "s-ui" > "${WORK_DIR}/panel_mode"
else
  echo -n "sing-box" > "${WORK_DIR}/panel_mode"
  if [[ "$INIT_SYS" == "systemd" ]]; then
    systemctl stop sing-box >/dev/null 2>&1 || true
    systemctl disable sing-box >/dev/null 2>&1 || true
  else
    rc-service sing-box stop >/dev/null 2>&1 || true
    rc-update del sing-box default >/dev/null 2>&1 || true
  fi
fi

seed_settings
svc_install
svc_enable_start
optimize_low_memory

echo "[6/6] 检查运行状态..."
sleep 3
svc_is_active && echo "      服务启动成功（${INIT_SYS}）" || {
  echo "      启动失败，查看日志: $(svc_logs_hint)" >&2
  exit 1
}

for _ in $(seq 1 10); do
  [[ -s "${WORK_DIR}/password" && -s "${WORK_DIR}/basepath" ]] && break
  sleep 1
done

# 纯 IPv6 小鸡自愈流水线检测与初始化
is_pure_ipv6=0
has_v4=0
has_v6=0
if curl -s4m 3 https://api.ipify.org >/dev/null 2>&1 || curl -s4m 3 https://checkip.amazonaws.com >/dev/null 2>&1; then
  has_v4=1
fi
if curl -s6m 3 https://api64.ipify.org >/dev/null 2>&1 || curl -s6m 3 https://ipv6.icanhazip.com >/dev/null 2>&1; then
  has_v6=1
fi
if [[ "$has_v4" -eq 0 && "$has_v6" -eq 1 ]]; then
  is_pure_ipv6=1
fi

if [[ "$is_pure_ipv6" -eq 1 ]]; then
  echo "  [+] 检测到当前服务器为纯 IPv6 环境 (无原生公网 IPv4)"
  echo "      正在自动申请注册 Cloudflare WARP 账号并建立前置出口隧道..."
  sout_pw=$(cat "${WORK_DIR}/password" 2>/dev/null | tr -d ' \r\n')
  cur_port=$(json_get "${WORK_DIR}/settings.json" port)
  cur_port="${cur_port:-43322}"
  sout_bp=$(cat "${WORK_DIR}/basepath" 2>/dev/null | tr -d ' \r\n')
  sout_bp="/${sout_bp#/}"
  sout_bp="${sout_bp%/}/"

  # 1. 注册 WARP
  warp_gen_res=$(curl -s -X POST -H "X-Sout-Password: ${sout_pw}" "http://127.0.0.1:${cur_port}${sout_bp}api/custom/warp/generate" 2>/dev/null || true)
  if echo "$warp_gen_res" | grep -q '"ok":true'; then
    echo "      WARP 节点账号申请成功，正在挂载为出口隧道..."
    # 2. 导入为出口
    import_res=$(curl -s -X POST -H "X-Sout-Password: ${sout_pw}" "http://127.0.0.1:${cur_port}${sout_bp}api/custom/source/import?id=preset-warp" 2>/dev/null || true)
    slot_num=$(echo "$import_res" | grep -o '"slot":[0-9]*' | cut -d: -f2)
    if [[ -n "$slot_num" ]]; then
      # 3. 设为全局前置链式出站
      toggle_res=$(curl -s -X POST -H "X-Sout-Password: ${sout_pw}" "http://127.0.0.1:${cur_port}${sout_bp}api/exit/chain_toggle?slot=${slot_num}" 2>/dev/null || true)
      if echo "$toggle_res" | grep -q '"active":true'; then
        echo "      已成功将 WARP 出口 (槽位 ${slot_num}) 设为全局前置链式出站，赋予全系统 IPv4 出口互联能力！"
      fi
    fi
  else
    echo "      WARP 注册请求跳过或暂不可用"
  fi
fi

# 如果用户选择开启隧道（无论是自定义域名+Token，还是直接回车开启免费临时隧道）
if [[ "$WANT_TUNNEL" == "y" ]]; then
  echo
  if [[ -z "$TUNNEL_DOMAIN" && -z "$TUNNEL_TOKEN" ]]; then
    echo "  [+] 检测到未输入域名与 Token，正在自动开启 Cloudflare 官方免费临时隧道..."
  else
    echo "  [+] 正在根据第一步输入的参数配置 Cloudflare隧道连接与轻量流量分流..."
  fi
  if [[ -x /usr/local/bin/sout ]]; then
    /usr/local/bin/sout setup_tunnel "$TUNNEL_DOMAIN" "$TUNNEL_TOKEN" "$TUNNEL_PORT" "${APPLY_CERT:-n}" "${CF_DNS_KEY:-}" "$SUI_ADMIN_PASS" "$SUI_PASS_IS_RANDOM"
  fi
else
  # 直连模式：自动配置免域名的直连 VLESS-Reality 节点
  echo
  echo "  [+] 检测到未配置 Cloudflare 隧道，正在自动为您创建直连免域名的 VLESS-Reality 节点..."
  if [[ -x /usr/local/bin/sout ]]; then
    /usr/local/bin/sout init_reality
  fi

IP=$(cat "${WORK_DIR}/host_ip" 2>/dev/null | tr -d ' \r\n')
if [[ -z "$IP" ]]; then
  IP=$(curl -s4m 5 https://checkip.amazonaws.com || curl -s4m 5 https://api.ipify.org || curl -s4m 5 https://ifconfig.me || echo "")
  if [[ -z "$IP" ]]; then
    IP=$(curl -s6m 5 https://ipv6.icanhazip.com || curl -s6m 5 https://api64.ipify.org || echo "127.0.0.1")
  fi
fi
if [[ "$IP" =~ : && ! "$IP" =~ ^\[ ]]; then
  IP="[${IP}]"
fi
BP=$(cat "${WORK_DIR}/basepath" 2>/dev/null | tr -d ' \r\n')
BP="/${BP#/}"
BP="${BP%/}/"

ACTUAL_PORT=$(json_get "${WORK_DIR}/settings.json" port)
WEB_PORT="${ACTUAL_PORT:-43322}"

GATEWAY_META="${WORK_DIR}/gateway_meta.json"
[[ ! -f "$GATEWAY_META" && -f "${WORK_DIR}/caddy_meta.json" ]] && GATEWAY_META="${WORK_DIR}/caddy_meta.json"
backend_mode=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")

if [[ -f "$GATEWAY_META" ]] && grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$GATEWAY_META"; then
  c_dom=$(json_get "$GATEWAY_META" domain)
  c_sout_p=$(json_get "$GATEWAY_META" sout_path)
  c_sui_p=$(json_get "$GATEWAY_META" sui_path)
  c_sui_u=$(json_get "$GATEWAY_META" sui_user)
  c_sui_w=$(json_get "$GATEWAY_META" sui_pass)
  c_sub_p=$(json_get "$GATEWAY_META" sub_path)
  if [[ "$WANT_TUNNEL" != "y" && -x /usr/local/bin/sout ]]; then
    tun_eng=$(json_get "$GATEWAY_META" tunnel_engine)
    if [[ "$tun_eng" != "sing-box" ]]; then
      systemctl restart cloudflared 2>/dev/null || rc-service cloudflared restart 2>/dev/null || service cloudflared restart 2>/dev/null || true
    fi
    systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || true
    systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true
    /usr/local/bin/sout reload_gateway >/dev/null 2>&1 || /usr/local/bin/sout reload_caddy >/dev/null 2>&1 || true
  fi
  echo
  echo "================================================================"
  echo "  🎉 sout 插件安装部署完成！(Cloudflare隧道连接与轻量流量分流)"
  echo "================================================================"
  echo "  [sout 动态家宽出口插件]"
  echo "  管理面板:      https://${c_dom}/${c_sout_p}/"
  echo "  访问口令:      $(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 ${WORK_DIR}/password")"
  echo "  sout 唤起命令: sout"
  echo

  if [[ "$backend_mode" == "sing-box" ]] || ! check_sui; then
    echo "  [sout 内嵌 sing-box 轻量内核]"
    echo "  运行后端:      sout 内嵌 sing-box（单核架构）"
    echo "  核心配置:      /etc/sing-box/config.json"
  elif [[ "$backend_mode" == "s-ui" ]] || check_sui; then
    echo "  [s-ui (Sing-Box) 节点面板]"
    echo "  s-ui 面板:     https://${c_dom}/${c_sui_p}/"
    echo "  s-ui 用户名:   ${SUI_ADMIN_USER:-${c_sui_u:-admin}}"
    if [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" == "1" ]]; then
      echo "  s-ui 密  码:   ${SUI_ADMIN_PASS}"
      echo "  ⚠️ 安全提示:   该随机密码仅在安装完成时显示一次，请务必妥善保存！"
    elif [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" != "1" ]]; then
      echo "  s-ui 密  码:   [已按您输入的自定义密码生效]"
    else
      echo "  s-ui 密  码:   [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
    fi
    echo "                 (若遗忘密码，可随时在终端输入 s-ui 进行重置修改)"
    echo "  s-ui 唤起命令: s-ui"
  fi
  echo
  echo "  [订阅与分流]"
  echo "  订阅链接:      https://${c_dom}/${c_sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
  echo "================================================================"
  echo
else
  # 读取 s-ui 独立模式下的端口、路径与管理员账号密码
  sui_u="admin"
  sui_port="8443"
  sui_path="/app/"
  sui_db="/usr/local/s-ui/db/s-ui.db"
  if [[ -f "$sui_db" ]]; then
    if command -v sqlite3 >/dev/null 2>&1; then
      sui_u=$(sqlite3 "$sui_db" "SELECT username FROM users LIMIT 1;" 2>/dev/null || echo "admin")
      local_p_val=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPort' LIMIT 1;" 2>/dev/null || true)
      [[ -n "$local_p_val" ]] && sui_port="$local_p_val"
      local_path_val=$(sqlite3 "$sui_db" "SELECT value FROM settings WHERE key='webPath' LIMIT 1;" 2>/dev/null || true)
      [[ -n "$local_path_val" ]] && sui_path="$local_path_val"
    elif command -v sout-server >/dev/null 2>&1; then
      sui_u=$(sout-server tool sui-query "$sui_db" username 2>/dev/null || echo "admin")
    elif [[ -x "$BIN" ]]; then
      sui_u=$("$BIN" tool sui-query "$sui_db" username 2>/dev/null || echo "admin")
    fi
  fi
  [[ -z "$sui_u" ]] && sui_u="admin"

  sui_path="/${sui_path#/}"
  [[ "$sui_path" != */ ]] && sui_path="${sui_path}/"

  echo
  echo "================================================================"
  echo "  🎉 sout 插件安装部署完成！"
  echo "================================================================"
  echo "  [sout 动态家宽出口插件]"
  echo "  管理面板:      http://${IP}:${WEB_PORT}${BP}"
  echo "  访问口令:      $(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 ${WORK_DIR}/password")"
  echo "  sout 唤起命令: sout"
  echo

  if [[ "$backend_mode" == "sing-box" ]] || ! check_sui; then
    echo "  [sout 内嵌 sing-box 轻量内核]"
    echo "  运行后端:      sout 内嵌 sing-box（单核架构）"
    echo "  核心配置:      /etc/sing-box/config.json"
  elif [[ "$backend_mode" == "s-ui" ]] || check_sui; then
    echo "  [s-ui (Sing-Box) 节点面板]"
    echo "  s-ui 面板:     http://${IP}:${sui_port}${sui_path}"
    echo "  s-ui 用户名:   ${SUI_ADMIN_USER:-${sui_u}}"
    if [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" == "1" ]]; then
      echo "  s-ui 密  码:   ${SUI_ADMIN_PASS}"
      echo "  ⚠️ 安全提示:   该随机密码仅在安装完成时显示一次，请务必妥善保存！"
    elif [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" != "1" ]]; then
      echo "  s-ui 密  码:   [已按您输入的自定义密码生效]"
    else
      echo "  s-ui 密  码:   [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
    fi
    echo "                 (若遗忘密码，可随时在终端输入 s-ui 进行重置修改)"
    echo "  s-ui 唤起命令: s-ui"
  fi
  echo
  echo "  [直连节点]"
  echo "  节点类型:      VLESS-Reality (已自动创建，无需域名，母机直连)"
  echo "  节点获取:      登录管理面板点击右上角「复制全部节点链接」或查看节点列表"
  echo "================================================================"
  echo
fi
fi

# 安装全流程结束，安全还原临时配置的 DNS64 / NAT64
restore_nat64_if_temporary
