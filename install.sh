#!/usr/bin/env bash
set -e
mkdir -p /var/tmp && export TMPDIR=/var/tmp

REPO="${REPO:-ustdbus/sout}"
BIN="/usr/local/bin/sout-server"
WORK_DIR="/var/lib/sout"
[[ -d "/usr/local/sout" && ! -d "/var/lib/sout" ]] && cp -rf /usr/local/sout /var/lib/sout 2>/dev/null || true
WEB_PORT=8899

if [[ $EUID -ne 0 ]]; then
  echo "请使用 root 权限运行此脚本 (sudo ./install.sh 或 sudo bash ...)" >&2
  exit 1
fi

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
  local base_cmds=(curl tar ip ss python3)
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
        python3) pkgs+=("python3") ;;
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
  echo "  [+] 正在检查并配置 sing-box 原生内核..."

  # 1. 优先检测是否可直接复用本地现存且能正常运行的 sing-box 内核
  if ! /usr/local/bin/sing-box version >/dev/null 2>&1; then
    if /usr/local/s-ui/bin/sing-box version >/dev/null 2>&1; then
      echo "      发现 s-ui 现有 sing-box 内核 (/usr/local/s-ui/bin/sing-box)，正在建立复用软链接..."
      mkdir -p /usr/local/bin
      ln -sf /usr/local/s-ui/bin/sing-box /usr/local/bin/sing-box
    elif /usr/bin/sing-box version >/dev/null 2>&1; then
      echo "      发现系统 /usr/bin/sing-box，正在建立复用软链接..."
      mkdir -p /usr/local/bin
      ln -sf /usr/bin/sing-box /usr/local/bin/sing-box
    elif command -v sing-box >/dev/null 2>&1 && sing-box version >/dev/null 2>&1; then
      local existing_sb
      existing_sb="$(command -v sing-box)"
      echo "      发现 PATH 中已有 sing-box (${existing_sb})，正在建立复用软链接..."
      mkdir -p /usr/local/bin
      ln -sf "$existing_sb" /usr/local/bin/sing-box
    fi
  fi

  # 2. 若本地无可用内核，自动从官方拉取最新稳定版 (Release)
  if ! /usr/local/bin/sing-box version >/dev/null 2>&1; then
    echo "      本地未发现兼容且可运行的 sing-box 内核，正在自动获取官方最新稳定版内核..."
    local arch
    arch=$(uname -m)
    local goarch=""
    case "$arch" in
      x86_64) goarch="amd64" ;;
      aarch64|arm64) goarch="arm64" ;;
      armv7l|armhf) goarch="armv7" ;;
      *) echo "  [!] 暂不支持该系统架构: $arch" >&2; return 1 ;;
    esac

    local tag=""
    # 方式一：通过 HTTP 302 重定向解析最新稳定 Release 标签（免 GitHub API 频率限制）
    tag=$(curl -sIL -m 8 "https://github.com/SagerNet/sing-box/releases/latest" 2>/dev/null | grep -i '^location:' | tail -1 | grep -oE 'v[0-9]+(\.[0-9]+)+' | tr -d ' \r\n' || true)
    
    # 方式二：备用通过 GitHub API 获取
    if [[ -z "$tag" ]]; then
      tag=$(curl -sSL -m 8 "https://api.github.com/repos/SagerNet/sing-box/releases/latest" 2>/dev/null | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | cut -d'"' -f4 || true)
    fi

    # 方式三：兜底官方稳定版
    [[ -z "$tag" ]] && tag="v1.11.5"
    local ver="${tag#v}"
    echo "      识别到 sing-box 最新稳定版本: ${tag} (${goarch})"

    local tmp_dir
    tmp_dir=$(mktemp -d)
    local is_musl=0
    if [[ -f /etc/alpine-release ]] || (ldd --version 2>&1 | grep -iq musl); then
      is_musl=1
    fi

    local pkg_candidates=()
    if [[ "$is_musl" -eq 1 ]]; then
      pkg_candidates+=("sing-box-${ver}-linux-${goarch}-musl.tar.gz" "sing-box-${ver}-linux-${goarch}.tar.gz")
    else
      pkg_candidates+=("sing-box-${ver}-linux-${goarch}.tar.gz" "sing-box-${ver}-linux-${goarch}-glibc.tar.gz")
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
      echo "      正在下载内核: ${u} ..."
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
    rm -rf "$tmp_dir"

    if [[ "$dl_ok" -eq 0 ]]; then
      echo "  [!] 下载并校验 sing-box 内核失败，请检查网络连接。" >&2
      rm -f /usr/local/bin/sing-box
      return 1
    fi

    echo "      sing-box 原生内核已就绪: $(/usr/local/bin/sing-box version 2>/dev/null | head -1 || echo 'ok')"
  else
    echo "      成功复用本地 sing-box 内核: $(/usr/local/bin/sing-box version 2>/dev/null | head -1 || echo 'ok')"
  fi

  # 初始化目录和精简配置
  mkdir -p /etc/sing-box
  if [[ ! -f /etc/sing-box/config.json ]]; then
    cat > /etc/sing-box/config.json <<'SBCONF'
{
  "log": {
    "level": "info",
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

  # 注册并启动系统服务
  echo "      正在注册 sing-box 服务 (${INIT_SYS})..."
  detect_adaptive_mem_tuning
  local sb_limit="35MiB"
  local sb_gc="60"
  if [[ "$HAS_SWAP" -eq 1 || ( -n "$AUTO_MEM_MB" && "$AUTO_MEM_MB" -gt 180 ) ]]; then
    # 德邦模式或内存 > 180MB：不限制 GOMEMLIMIT，使用原生默认
    sb_limit=""
    sb_gc="100"
  fi
  local sb_env_systemd=""
  local sb_env_openrc=""
  if [[ -n "$sb_limit" ]]; then
    sb_env_systemd="Environment=\"GOMEMLIMIT=${sb_limit}\"
Environment=\"GOGC=${sb_gc}\""
    sb_env_openrc="export GOMEMLIMIT=\"${sb_limit}\"
export GOGC=\"${sb_gc}\""
  fi

  if [[ "$INIT_SYS" == "systemd" ]]; then
    cat > /etc/systemd/system/sing-box.service <<SBEU
[Unit]
Description=sing-box service
Documentation=https://sing-box.sagernet.org
After=network.target nss-lookup.target network-online.target

[Service]
${sb_env_systemd}
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json
Restart=on-failure
RestartSec=3s
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
SBEU
    systemctl daemon-reload
    systemctl enable sing-box >/dev/null 2>&1 || true
    systemctl restart sing-box >/dev/null 2>&1 || true
  else
    local supervisor_line="command_background=\"yes\"\npidfile=\"/run/sing-box.pid\""
    if command -v supervise-daemon >/dev/null 2>&1; then
      supervisor_line="supervisor=\"supervise-daemon\""
    fi
    cat > /etc/init.d/sing-box <<SBRC
#!/sbin/openrc-run
name="sing-box"
description="sing-box service"
${sb_env_openrc}
${supervisor_line}
command="/usr/local/bin/sing-box"
command_args="run -c /etc/sing-box/config.json"
output_log="/var/log/sing-box.log"
error_log="/var/log/sing-box.err"
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
  if [ -f "/etc/sing-box/config.json" ]; then
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
SBRC
    chmod +x /etc/init.d/sing-box
    rc-update add sing-box default >/dev/null 2>&1 || true
    rc-service sing-box stop >/dev/null 2>&1 || true
    rc-service sing-box zap >/dev/null 2>&1 || true
    sleep 0.3
    rc-service sing-box start >/dev/null 2>&1 || true
  fi

  mkdir -p "$WORK_DIR"
  echo "sing-box" > "${WORK_DIR}/panel_mode"

  echo
  echo "================================================================"
  echo "  [✓] sing-box 原生内核已成功就绪并启动！继续进行 sout 插件安装..."
  echo "================================================================"
  echo
  return 0
}

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
  elif [[ $mem_total_mb -le 256 ]]; then
    rmem_max=2097152
    wmem_max=2097152
    udp_mem="2048 4096 8192"
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

  # 6. 低内存 VPS/容器防爆保护
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
    local caddy_memlimit="25MiB"
    local caddy_gogc="60"
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

      mkdir -p /etc/systemd/system/caddy.service.d 2>/dev/null || true
      cat > /etc/systemd/system/caddy.service.d/override.conf <<EOF
[Service]
Environment="GOMEMLIMIT=${caddy_memlimit}"
Environment="GOGC=${caddy_gogc}"
EOF

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
      for svc in sing-box cloudflared caddy sout s-ui; do
        local cur_limit="$aux_memlimit"
        local cur_gc="$aux_gogc"
        if [[ "$svc" == "sing-box" ]]; then
          cur_limit="$sb_memlimit"; cur_gc="$sb_gogc"
        elif [[ "$svc" == "cloudflared" ]]; then
          cur_limit="$cf_memlimit"; cur_gc="$cf_gogc"
        elif [[ "$svc" == "caddy" ]]; then
          cur_limit="$caddy_memlimit"; cur_gc="$caddy_gogc"
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
# [第一步] 一开始首先询问 Cloudflare隧道连接和Caddy流量代理配置
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
    read -rp "  是否配置 Cloudflare隧道连接和Caddy流量代理？[y/N]: " prompt_choice
  else
    if [[ -c /dev/tty ]]; then
      read -rp "  是否配置 Cloudflare隧道连接和Caddy流量代理？[y/N]: " prompt_choice < /dev/tty || prompt_choice="n"
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
      echo "  [Cloudflare SSL 证书 (Caddy DNS-01)]"
      echo "  • 令牌需含「区域.DNS / 编辑」权限，用于自动签发证书并开启 TUIC / Hysteria2 节点"
      echo "  • 直接按回车将跳过申请，自动配置常规节点 (vless-argo / vless-reality)"
      if [[ -t 0 ]]; then
        read -rp "  4. 请输入 Cloudflare API 令牌 (直接回车跳过): " CF_DNS_KEY
      else
        if [[ -c /dev/tty ]]; then
          read -rp "  4. 请输入 Cloudflare API 令牌 (直接回车跳过): " CF_DNS_KEY < /dev/tty || CF_DNS_KEY=""
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
  if check_sui; then
    mkdir -p "$WORK_DIR"
    [[ ! -f "${WORK_DIR}/panel_mode" ]] && echo "s-ui" > "${WORK_DIR}/panel_mode"
    return 0
  fi
  if check_singbox; then
    mkdir -p "$WORK_DIR"
    [[ ! -f "${WORK_DIR}/panel_mode" ]] && echo "sing-box" > "${WORK_DIR}/panel_mode"
    return 0
  fi

  echo
  echo "================================================================"
  echo "  [!] 检测到当前 VPS 尚未安装 s-ui 面板或 sing-box 内核！"
  echo
  echo "  sout 支持两种节点运行后端："
  echo "    1) 安装 s-ui 面板"
  echo "    2) 安装 singbox 内核（默认）"
  echo "    3) 不安装退出"
  echo
  echo "  直接回车默认选择 [2]"
  echo "================================================================"
  echo

  local choice=""
  if [[ -t 0 ]]; then
    read -rp "  请选择操作 [1-3] (默认: 2): " choice
  else
    read -rp "  请选择操作 [1-3] (默认: 2): " choice < /dev/tty || choice="2"
  fi

  choice=$(echo "$choice" | tr -d ' \r\n')
  [[ -z "$choice" ]] && choice="2"

  case "$choice" in
    1)
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
      if ! install_singbox; then
        echo "  [!] sing-box 安装失败，正在退出..."
        cleanup_sout
        exit 1
      fi
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
  detect_adaptive_mem_tuning
  local sout_env_systemd=""
  local sout_env_openrc=""
  if [[ -n "$AUTO_GOMEMLIMIT" ]]; then
    sout_env_systemd="Environment=\"GOMEMLIMIT=${AUTO_GOMEMLIMIT}\"
Environment=\"GOGC=${AUTO_GOGC}\""
    sout_env_openrc="export GOMEMLIMIT=\"${AUTO_GOMEMLIMIT}\"
export GOGC=\"${AUTO_GOGC}\""
  fi

  if [[ "$INIT_SYS" == systemd ]]; then
    cat > /etc/systemd/system/sout.service <<SVCEOF
[Unit]
Description=sout - s-ui 动态家宽出口插件 (VPN Gate)
After=network.target network-online.target
Wants=network-online.target

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
    cat > /etc/init.d/sout <<OPENRCEOF
#!/sbin/openrc-run
name="sout"
description="sout - s-ui 动态家宽出口插件"
${sout_env_openrc}
command="/usr/local/bin/sout-server"
command_args="-dir /var/lib/sout"
command_background="yes"
pidfile="/run/sout.pid"
output_log="/var/log/sout.log"
error_log="/var/log/sout.log"
respawn_delay=3

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
  local web_p="${WEB_PORT:-8899}"
  if [[ -f "${WORK_DIR}/settings.json" ]]; then
    local parsed_p
    parsed_p=$(grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' "${WORK_DIR}/settings.json" 2>/dev/null | grep -oE '[0-9]+' || true)
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

if [[ -f main.go ]] && command -v go >/dev/null; then
  echo "      检测到源码环境，正在本地编译 (内嵌 sing-box 1.14 + gVisor)..."
  CGO_ENABLED=0 go build -trimpath -tags "with_gvisor with_quic netgo osusergo" -ldflags "-s -w" -o "$BIN" .
else
  echo "      正在拉取预编译包 (${GOARCH})..."
  TMP=$(mktemp -d -p /var/tmp)
  sout_urls=(
    "https://github.com/${REPO}/releases/latest/download/sout-linux-${GOARCH}.tar.gz"
    "https://ghproxy.net/https://github.com/${REPO}/releases/latest/download/sout-linux-${GOARCH}.tar.gz"
  )
  sout_dl_ok=0
  for u in "${sout_urls[@]}"; do
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
  tar xzf "$TMP/f.tar.gz" -C "$TMP"
  if [[ -f "$TMP/sout-server" ]]; then
    install -m 755 "$TMP/sout-server" "$BIN"
  elif [[ -f "$TMP/sout" ]]; then
    install -m 755 "$TMP/sout" "$BIN"
  elif [[ -f "$TMP/fanout" ]]; then
    install -m 755 "$TMP/fanout" "$BIN"
  fi
  ln -sf "$BIN" /usr/local/bin/fanout 2>/dev/null || true
  if [[ -f "$TMP/f.sh" ]]; then
    install -m 755 "$TMP/f.sh" /usr/local/bin/sout
    rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
  fi
  rm -rf "$TMP"
fi

echo "[3/6] 检测节点管理后端..."
backend_kind=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")
if [[ "$backend_kind" == "sing-box" ]] || (! check_sui && check_singbox); then
  echo "      检测到 sing-box 原生内核后端（将以原生内核模式接管分流）"
elif check_sui; then
  echo "      检测到已安装 s-ui 面板（将自动以 s-ui 模式接管分流）"
else
  echo "      提示：未检测到后端，可在安装后配置 sing-box 或 s-ui 以启用分流联动。"
fi

echo "[4/6] 准备网络运行环境并优化内核套接字/UDP缓冲区..."
apply_sysctl_optimization
detected_cc=$(get_tcp_congestion)
echo "      当前系统生效的 TCP 拥塞控制算法: ${detected_cc}"

echo "[5/6] 部署服务与终端管理命令..."
if [[ -f f.sh ]]; then
  install -m 755 f.sh /usr/local/bin/sout
fi
rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
mkdir -p "$WORK_DIR"
chmod 700 "$WORK_DIR"

# 若为 sing-box 后端，确保持久化 panel_mode 并确保守护进程服务存在
if [[ "$backend_kind" == "sing-box" ]] || (! check_sui && check_singbox); then
  echo -n "sing-box" > "${WORK_DIR}/panel_mode"
  detect_adaptive_mem_tuning
  local sb_limit="35MiB"
  local sb_gc="60"
  if [[ "$HAS_SWAP" -eq 1 || ( -n "$AUTO_MEM_MB" && "$AUTO_MEM_MB" -gt 180 ) ]]; then
    # 德邦模式或内存 > 180MB：不限制 GOMEMLIMIT，使用原生默认
    sb_limit=""
    sb_gc="100"
  fi
  local sb_env_systemd=""
  local sb_env_openrc=""
  if [[ -n "$sb_limit" ]]; then
    sb_env_systemd="Environment=\"GOMEMLIMIT=${sb_limit}\"
Environment=\"GOGC=${sb_gc}\""
    sb_env_openrc="export GOMEMLIMIT=\"${sb_limit}\"
export GOGC=\"${sb_gc}\""
  fi

  if [[ "$INIT_SYS" == "systemd" ]]; then
    if [[ ! -f /etc/systemd/system/sing-box.service ]]; then
      cat > /etc/systemd/system/sing-box.service <<SBEU
[Unit]
Description=sing-box service
Documentation=https://sing-box.sagernet.org
After=network.target nss-lookup.target network-online.target

[Service]
${sb_env_systemd}
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json
Restart=on-failure
RestartSec=3s
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
SBEU
      systemctl daemon-reload
    fi
    systemctl enable sing-box >/dev/null 2>&1 || true
    systemctl restart sing-box >/dev/null 2>&1 || true
  else
    local supervisor_line="command_background=\"yes\"\npidfile=\"/run/sing-box.pid\""
    if command -v supervise-daemon >/dev/null 2>&1; then
      supervisor_line="supervisor=\"supervise-daemon\""
    fi
    cat > /etc/init.d/sing-box <<SBRC
#!/sbin/openrc-run
name="sing-box"
description="sing-box service"
${sb_env_openrc}
${supervisor_line}
command="/usr/local/bin/sing-box"
command_args="run -c /etc/sing-box/config.json"
output_log="/var/log/sing-box.log"
error_log="/var/log/sing-box.err"
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
  if [ -f "/etc/sing-box/config.json" ]; then
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
SBRC
    chmod +x /etc/init.d/sing-box
    rc-update add sing-box default >/dev/null 2>&1 || true
    rc-service sing-box stop >/dev/null 2>&1 || true
    rc-service sing-box zap >/dev/null 2>&1 || true
    sleep 0.3
    rc-service sing-box start >/dev/null 2>&1 || true
  fi
elif [[ "$backend_kind" == "s-ui" ]] || check_sui; then
  echo -n "s-ui" > "${WORK_DIR}/panel_mode"
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

# 如果用户选择开启隧道（无论是自定义域名+Token，还是直接回车开启免费临时隧道）
if [[ "$WANT_TUNNEL" == "y" ]]; then
  echo
  if [[ -z "$TUNNEL_DOMAIN" && -z "$TUNNEL_TOKEN" ]]; then
    echo "  [+] 检测到未输入域名与 Token，正在自动开启 Cloudflare 官方免费临时隧道..."
  else
    echo "  [+] 正在根据第一步输入的参数配置 Cloudflare隧道连接和Caddy流量代理..."
  fi
  if [[ -x /usr/local/bin/sout ]]; then
    /usr/local/bin/sout setup_tunnel "$TUNNEL_DOMAIN" "$TUNNEL_TOKEN" "$TUNNEL_PORT" "${APPLY_CERT:-n}" "${CF_DNS_KEY:-}" "$SUI_ADMIN_PASS" "$SUI_PASS_IS_RANDOM"
  fi
  # setup_tunnel 已完整打印包含全部隧道、面板与订阅的部署完成日志，直接退出避免重复打印
  exit 0
fi

IP=$(curl -s4m 5 https://checkip.amazonaws.com || curl -s4m 5 https://api.ipify.org || curl -s4m 5 https://ifconfig.me || echo "127.0.0.1")
BP=$(cat "${WORK_DIR}/basepath" 2>/dev/null | tr -d ' \r\n')
BP="/${BP#/}"
BP="${BP%/}/"

ACTUAL_PORT=$(grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' "${WORK_DIR}/settings.json" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
WEB_PORT="${ACTUAL_PORT:-8899}"

CADDY_META="${WORK_DIR}/caddy_meta.json"
backend_mode=$(cat "${WORK_DIR}/panel_mode" 2>/dev/null || echo "")

if [[ -f "$CADDY_META" ]] && grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META"; then
  c_dom=$(grep -oE '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sout_p=$(grep -oE '"sout_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sui_u=$(grep -oE '"sui_user"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sui_w=$(grep -oE '"sui_pass"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  if [[ "$WANT_TUNNEL" != "y" && -x /usr/local/bin/sout ]]; then
    systemctl restart cloudflared 2>/dev/null || rc-service cloudflared restart 2>/dev/null || service cloudflared restart 2>/dev/null || true
    systemctl restart s-ui 2>/dev/null || rc-service s-ui restart 2>/dev/null || true
    systemctl restart sing-box 2>/dev/null || rc-service sing-box restart 2>/dev/null || true
    /usr/local/bin/sout reload_caddy >/dev/null 2>&1 || true
  fi
  echo
  echo "================================================================"
  echo "  🎉 sout 插件安装部署完成！(Cloudflare隧道连接和Caddy流量代理)"
  echo "================================================================"
  echo "  [sout 动态家宽出口插件]"
  echo "  管理面板:      https://${c_dom}/${c_sout_p}/"
  echo "  访问口令:      $(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 ${WORK_DIR}/password")"
  echo "  sout 唤起命令: sout"
  echo

  if [[ "$backend_mode" == "sing-box" ]] || (! check_sui && check_singbox); then
    echo "  [sing-box 原生内核]"
    echo "  运行后端:      sing-box 原生内核"
    echo "  核心配置:      /etc/sing-box/config.json"
  elif check_sui; then
    echo "  [s-ui (Sing-Box) 节点面板]"
    echo "  s-ui 面板:     https://${c_dom}/${c_sui_p}/"
    echo "  s-ui 用户名:   ${SUI_ADMIN_USER:-${c_sui_u:-admin}}"
    if [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" == "1" ]]; then
      echo "  s-ui 密  码:   ${SUI_ADMIN_PASS}"
      echo "  ⚠️ 安全提示:   该随机密码仅在安装完成时显示一次，请务必妥善保存！"
      echo "                 (若遗忘密码，可随时在终端输入 s-ui 进行重置修改)"
    elif [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" != "1" ]]; then
      echo "  s-ui 密  码:   [已按您输入的自定义密码生效，若遗忘密码，可随时在终端输入 s-ui 进行重置修改]"
    else
      echo "  s-ui 密  码:   [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
    fi
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
    elif command -v python3 >/dev/null 2>&1; then
      sui_u=$(python3 -c "import sqlite3; con=sqlite3.connect('$sui_db'); cur=con.cursor(); r=cur.execute('SELECT username FROM users LIMIT 1').fetchone(); print(r[0] if r else 'admin'); con.close()" 2>/dev/null || echo "admin")
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

  if [[ "$backend_mode" == "sing-box" ]] || (! check_sui && check_singbox); then
    echo "  [sing-box 原生内核]"
    echo "  运行后端:      sing-box 原生内核"
    echo "  核心配置:      /etc/sing-box/config.json"
  elif check_sui; then
    echo "  [s-ui (Sing-Box) 节点面板]"
    echo "  s-ui 面板:     http://${IP}:${sui_port}${sui_path}"
    echo "  s-ui 用户名:   ${SUI_ADMIN_USER:-${sui_u}}"
    if [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" == "1" ]]; then
      echo "  s-ui 密  码:   ${SUI_ADMIN_PASS}"
      echo "  ⚠️ 安全提示:   该随机密码仅在安装完成时显示一次，请务必妥善保存！"
      echo "                 (若遗忘密码，可随时在终端输入 s-ui 进行重置修改)"
    elif [[ -n "$SUI_ADMIN_PASS" && "$SUI_PASS_IS_RANDOM" != "1" ]]; then
      echo "  s-ui 密  码:   [已按您输入的自定义密码生效，若遗忘密码，可随时在终端输入 s-ui 进行重置修改]"
    else
      echo "  s-ui 密  码:   [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
    fi
    echo "  s-ui 唤起命令: s-ui"
  fi
  echo "================================================================"
  echo
fi
