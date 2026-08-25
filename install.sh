#!/usr/bin/env bash
set -e

REPO="${REPO:-ustdbus/sout}"
BIN="/usr/local/bin/sout-server"
WORK_DIR="/var/lib/sout"
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

check_sui() {
  if [[ -f /usr/local/s-ui/db/s-ui.db ]] || [[ -f /usr/local/s-ui/s-ui ]] || command -v sui >/dev/null 2>&1 || [[ -f /usr/local/s-ui/sui ]]; then
    return 0
  fi
  return 1
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
  rm -f "$BIN" /usr/local/bin/sout-server /usr/local/bin/fanout /usr/local/bin/f /usr/local/bin/sout /usr/local/bin/sout-cli 2>/dev/null || true
  echo "      sout 已清理完毕。"
}

ensure_sui() {
  if check_sui; then
    return 0
  fi

  echo
  echo "================================================================"
  echo "  [!] 检测到当前 VPS 尚未安装 s-ui 面板！"
  echo
  echo "  sout 是专为 s-ui (Sing-Box) 设计的动态家宽出口与分流插件。"
  echo "  必须配合 s-ui 面板才能实现多协议入站、单端口多用户及分流联动。"
  echo
  echo "  请选择操作："
  echo "    y) 自动检测当前服务器系统并安装官方 s-ui 面板"
  echo "    n) 退出并卸载/取消安装脚本"
  echo "================================================================"
  echo

  local choice=""
  if [[ -t 0 ]]; then
    read -rp "  是否安装官方 s-ui 面板？[Y/n]: " choice
  else
    read -rp "  是否安装官方 s-ui 面板？[Y/n]: " choice < /dev/tty || choice="y"
  fi

  choice="${choice:-y}"
  case "${choice,,}" in
    y|yes)
      echo
      echo "  [+] 正在自动检测当前服务器系统架构并安装官方 s-ui 面板..."
      echo "      (官方仓库: https://github.com/alireza0/s-ui)"
      echo
      bash <(curl -Ls https://raw.githubusercontent.com/alireza0/s-ui/master/install.sh) || true
      
      if check_sui; then
        echo
        echo "  [✓] s-ui 面板安装完成并就绪！继续进行 sout 插件安装..."
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
    n|no)
      echo
      echo "  [-] 您选择了取消安装，正在卸载并退出..."
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

ensure_sui

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
  if [[ "$INIT_SYS" == systemd ]]; then
    # 停止并清理旧版 fanout.service
    systemctl stop fanout 2>/dev/null || true
    systemctl disable fanout 2>/dev/null || true
    rm -f /etc/systemd/system/fanout.service 2>/dev/null || true

    local svc_file="sout.service"
    [[ ! -f "$svc_file" && -f "fanout.service" ]] && svc_file="fanout.service"
    sed "s#-web [0-9]* ##; s#-dir /var/lib/[a-zA-Z0-9_-]*#-dir ${WORK_DIR}#; s#/usr/local/bin/[a-zA-Z0-9_-]*#${BIN}#" "$svc_file" \
      > /etc/systemd/system/sout.service
    ln -sf /etc/systemd/system/sout.service /etc/systemd/system/fanout.service 2>/dev/null || true
    systemctl daemon-reload
  else
    cat > /etc/init.d/sout <<INITEOF
#!/sbin/openrc-run
name="sout"
description="sout - s-ui 动态家宽出口插件"
command="${BIN}"
command_args="-dir ${WORK_DIR}"
command_background=true
pidfile="/run/sout.pid"
output_log="/var/log/sout.log"
error_log="/var/log/sout.log"
respawn_delay=5
respawn_max=0
supervisor=supervise-daemon
depend() { need net; after firewall; }
INITEOF
    chmod +x /etc/init.d/sout
    ln -sf /etc/init.d/sout /etc/init.d/fanout 2>/dev/null || true
  fi
}

svc_enable_start() {
  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl enable --now sout
  else
    rc-update add sout default >/dev/null 2>&1 || true
    rc-service sout restart
  fi
}

svc_is_active() {
  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl is-active --quiet sout
  else
    rc-service sout status >/dev/null 2>&1
  fi
}

svc_logs_hint() {
  [[ "$INIT_SYS" == systemd ]] && echo "journalctl -u sout -n 30" || echo "cat /var/log/sout.log"
}

echo "[1/6] 检查系统基础依赖..."

pkg_for() {
  local cmd="$1" mgr="$2"
  case "$cmd" in
    openvpn)  echo openvpn ;;
    curl)     echo curl ;;
    openssl)  echo openssl ;;
    tar)      echo tar ;;
    ip)       case "$mgr" in apk) echo iproute2 ;; pacman) echo iproute2 ;; *) echo iproute ;; esac ;;
    iptables) echo iptables ;;
    sqlite3)  case "$mgr" in yum|dnf) echo sqlite ;; *) echo sqlite3 ;; esac ;;
    unzip)    echo unzip ;;
  esac
}

detect_mgr() {
  for m in apt-get dnf yum pacman apk zypper; do
    command -v "$m" >/dev/null && { echo "$m"; return; }
  done
  echo ""
}

install_pkgs() {
  local mgr="$1"; shift
  case "$mgr" in
    apt-get)
      apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
      ;;
    dnf)    dnf install -y -q "$@" ;;
    yum)    yum install -y -q "$@" ;;
    pacman) pacman -Sy --noconfirm --needed "$@" ;;
    apk)    apk add --no-cache "$@" ;;
    zypper) zypper --non-interactive install -y "$@" ;;
  esac
}

MGR=$(detect_mgr)
[[ "$MGR" == "apt-get" ]] && iproute_pkg=iproute2 || iproute_pkg=iproute

need_cmd=()
for c in openvpn curl openssl tar iptables sqlite3; do
  command -v "$c" >/dev/null || need_cmd+=("$c")
done
command -v ip >/dev/null || need_cmd+=(ip)

if [[ ${#need_cmd[@]} -gt 0 ]]; then
  echo "      缺少依赖命令: ${need_cmd[*]}"
  if [[ -z "$MGR" ]]; then
    echo "      无法识别包管理器，请手动安装上述依赖" >&2
    exit 1
  fi
  pkgs=()
  for c in "${need_cmd[@]}"; do
    if [[ "$c" == "ip" ]]; then pkgs+=("$iproute_pkg"); else pkgs+=("$(pkg_for "$c" "$MGR")"); fi
  done
  echo "      正在自动安装: ${pkgs[*]}"
  install_pkgs "$MGR" "${pkgs[@]}" || {
    echo "      自动安装依赖失败，请手动安装: ${pkgs[*]}" >&2
    exit 1
  }
fi

echo "[2/6] 获取 sout 二进制文件..."
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "      暂不支持该架构: $ARCH" >&2; exit 1 ;;
esac

if [[ -f main.go ]] && command -v go >/dev/null; then
  echo "      检测到源码环境，正在本地编译..."
  go build -trimpath -ldflags "-s -w" -o "$BIN" .
else
  echo "      正在拉取预编译包 (${GOARCH})..."
  TMP=$(mktemp -d)
  URL="https://github.com/${REPO}/releases/latest/download/sout-linux-${GOARCH}.tar.gz"
  if ! curl -fsSL "$URL" -o "$TMP/f.tar.gz"; then
    echo "      下载失败: $URL" >&2
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
  [[ -f "$TMP/sout.service" ]] && cp "$TMP/sout.service" .
  [[ -f "$TMP/fanout.service" ]] && cp "$TMP/fanout.service" .
  if [[ -f "$TMP/f.sh" ]]; then
    install -m 755 "$TMP/f.sh" /usr/local/bin/f
  fi
  rm -rf "$TMP"
fi

echo "[3/6] 检测节点管理面板..."
if check_sui; then
  echo "      检测到已安装 s-ui 面板（将自动以 s-ui 模式接管分流）"
elif command -v /usr/local/x-ui/x-ui >/dev/null 2>&1 || [[ -x /usr/bin/x-ui ]]; then
  echo "      检测到已安装 3x-ui 面板（将自动以 3x-ui 模式接管出入站）"
else
  echo "      提示：未检测到 s-ui 面板，请配置 s-ui 以启用节点分流联动。"
fi

echo "[4/6] 配置网络转发与防火墙规则..."
sysctl -qw net.ipv4.ip_forward=1
grep -q '^net.ipv4.ip_forward=1' /etc/sysctl.conf 2>/dev/null \
  || echo 'net.ipv4.ip_forward=1' >> /etc/sysctl.conf
if ! iptables -C FORWARD -s 10.99.0.0/16 -j ACCEPT 2>/dev/null; then
  iptables -I FORWARD 1 -s 10.99.0.0/16 -j ACCEPT
fi
if ! iptables -C FORWARD -d 10.99.0.0/16 -j ACCEPT 2>/dev/null; then
  iptables -I FORWARD 1 -d 10.99.0.0/16 -j ACCEPT
fi
command -v netfilter-persistent >/dev/null && netfilter-persistent save >/dev/null 2>&1 || true

# 解除 Ubuntu/Debian AppArmor 对 openvpn 访问工作目录配置的限制
if command -v apparmor_parser >/dev/null 2>&1 && [[ -f /etc/apparmor.d/openvpn ]]; then
  aa-disable openvpn 2>/dev/null || (mkdir -p /etc/apparmor.d/disable && ln -sf /etc/apparmor.d/openvpn /etc/apparmor.d/disable/ && apparmor_parser -R /etc/apparmor.d/openvpn 2>/dev/null) || true
fi

echo "[5/6] 部署服务与终端管理命令..."
if [[ -f f.sh ]]; then
  install -m 755 f.sh /usr/local/bin/f
elif [[ -n "${TMP:-}" && -f "${TMP}/f.sh" ]]; then
  install -m 755 "${TMP}/f.sh" /usr/local/bin/f
fi
ln -sf /usr/local/bin/f /usr/local/bin/sout 2>/dev/null || true
mkdir -p "$WORK_DIR"
chmod 700 "$WORK_DIR"
seed_settings
svc_install
svc_enable_start

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

IP=$(curl -s --max-time 8 http://api.ipify.org || echo "<服务器IP>")
BP=$(cat "${WORK_DIR}/basepath" 2>/dev/null || true)
ACTUAL_PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' \
  "${WORK_DIR}/settings.json" 2>/dev/null | head -1)
[[ -n $ACTUAL_PORT ]] && WEB_PORT="$ACTUAL_PORT"

echo
echo "================================================================"
echo "  🎉 sout 插件安装部署完成！"
echo "================================================================"
echo "  [sout 动态家宽出口插件]"
echo "  管理面板:  http://${IP}:${WEB_PORT}/${BP}/"
echo "  访问口令:  $(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 ${WORK_DIR}/password")"
echo "  终端管理:  在终端输入 sout 或 f 呼出插件管理菜单"
echo
if check_sui; then
  echo "  [s-ui (Sing-Box) 节点面板]"
  echo "  终端管理:  在终端输入 s-ui 即可配置 s-ui 账号/端口与节点设置"
fi
echo "================================================================"
echo
