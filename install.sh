#!/usr/bin/env bash
set -e

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

ask_tunnel_setup() {
  echo
  echo "================================================================"
  echo "  💡 提示：NAT 机推荐开启 Cloudflare 隧道进行代理，正常 VPS 可不启用，需在 Cloudflare 中配置回源。"
  echo "  👉 提示：若不填写域名与 Token（直接按回车），将自动为您开启 Cloudflare 官方免费临时隧道（免域名 / 免Token / 即开即用）"
  echo "  📌 提示：如要使用固定隧道，请提前准备好："
  echo "       1) 已在 Cloudflare 中添加的访问域名"
  echo "       2) Cloudflare 隧道 Token"
  echo "       3) 在 Cloudflare 中为该隧道配置的端口/回源端口"
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
    TUNNEL_DOMAIN=$(echo "$TUNNEL_DOMAIN" | tr -d ' 
')
    TUNNEL_TOKEN=$(echo "$TUNNEL_TOKEN" | tr -d ' 
')
    TUNNEL_PORT=$(echo "$TUNNEL_PORT" | tr -d ' 
')
    TUNNEL_PORT="${TUNNEL_PORT:-8081}"

    if [[ -z "$TUNNEL_DOMAIN" || -z "$TUNNEL_TOKEN" ]]; then
      TUNNEL_DOMAIN=""
      TUNNEL_TOKEN=""
      echo "  [✓] 已选择开启 Cloudflare 官方免费临时隧道 (免域名/免Token)，将在核心组件就绪后自动分配！"
    else
      echo "  [✓] 隧道参数已保存，将在核心组件就绪后自动启动并绑定！"
    fi
  fi
}

ask_tunnel_setup

# ==============================================================================
# [第二步] 询问 / 检测并安装官方 s-ui 面板
# ==============================================================================
SUI_INSTALLED_BY_US=0

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
      if [[ -c /dev/tty ]]; then
        bash <(curl -Ls https://raw.githubusercontent.com/alireza0/s-ui/master/install.sh) < /dev/tty || true
      else
        bash <(curl -Ls https://raw.githubusercontent.com/alireza0/s-ui/master/install.sh) || true
      fi
      
      if check_sui; then
        SUI_INSTALLED_BY_US=1
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
  echo "      正在注册系统服务 (${INIT_SYS})..."
  if [[ "$INIT_SYS" == systemd ]]; then
    cat > /etc/systemd/system/sout.service <<SVCEOF
[Unit]
Description=sout - s-ui 动态家宽出口插件 (VPN Gate)
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
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
    cat > /etc/init.d/sout <<'OPENRCEOF'
#!/sbin/openrc-run
name="sout"
description="sout - s-ui 动态家宽出口插件"
command="/usr/local/bin/sout-server"
command_args="-dir /var/lib/sout"
command_background="yes"
pidfile="/run/sout.pid"
output_log="/var/log/sout.log"
error_log="/var/log/sout.err"

depend() {
  need net
  after firewall
}
OPENRCEOF
    chmod +x /etc/init.d/sout
    rm -f /etc/init.d/fanout 2>/dev/null || true
  fi
}

svc_enable_start() {
  echo "      正在启动服务..."
  if [[ "$INIT_SYS" == systemd ]]; then
    systemctl enable sout >/dev/null 2>&1 || true
    systemctl restart sout
  else
    rc-update add sout default >/dev/null 2>&1 || true
    rc-service sout restart
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

echo
echo "================================================================"
echo "  🚀 开始安装部署 sout - s-ui 动态家宽出口插件"
echo "================================================================"

echo "[1/6] 检查系统依赖..."
MGR=$(detect_pkg_mgr)
needed=()
for cmd in curl tar ip ss python3 sqlite3; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    needed+=("$cmd")
  fi
done

if [[ ${#needed[@]} -gt 0 ]]; then
  echo "      发现缺失命令: ${needed[*]}，正在匹配软件包..."
  pkgs=()
  for cmd in "${needed[@]}"; do
    case "$cmd" in
      curl) pkgs+=("curl") ;;
      tar)  pkgs+=("tar") ;;
      ip)   [[ "$MGR" == "apk" ]] && pkgs+=("iproute2") || pkgs+=("iproute2") ;;
      ss)   [[ "$MGR" == "apk" ]] && pkgs+=("iproute2") || pkgs+=("iproute2") ;;
      python3) pkgs+=("python3") ;;
      sqlite3) [[ "$MGR" == "apk" ]] && pkgs+=("sqlite") || pkgs+=("sqlite3") ;;
    esac
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
  echo "      检测到源码环境，正在本地编译 (内嵌 sing-box 1.14 + gVisor)..."
  CGO_ENABLED=0 go build -trimpath -tags "with_gvisor with_quic netgo osusergo" -ldflags "-s -w" -o "$BIN" .
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
  if [[ -f "$TMP/f.sh" ]]; then
    install -m 755 "$TMP/f.sh" /usr/local/bin/sout
    rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
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

echo "[4/6] 准备用户态网络运行环境..."
# 用户态 gVisor 协议栈完全在内存中运行，无需修改宿主路由与防火墙规则

echo "[5/6] 部署服务与终端管理命令..."
if [[ -f f.sh ]]; then
  install -m 755 f.sh /usr/local/bin/sout
fi
rm -f /usr/local/bin/f /usr/local/bin/sout-cli 2>/dev/null || true
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

# 如果用户选择开启隧道（无论是自定义域名+Token，还是直接回车开启免费临时隧道）
if [[ "$WANT_TUNNEL" == "y" ]]; then
  echo
  if [[ -z "$TUNNEL_DOMAIN" && -z "$TUNNEL_TOKEN" ]]; then
    echo "  [+] 检测到未输入域名与 Token，正在自动开启 Cloudflare 官方免费临时隧道..."
  else
    echo "  [+] 正在根据第一步输入的参数配置 Cloudflare隧道连接和Caddy流量代理..."
  fi
  if [[ -x /usr/local/bin/sout ]]; then
    /usr/local/bin/sout setup_tunnel "$TUNNEL_DOMAIN" "$TUNNEL_TOKEN" "$TUNNEL_PORT"
  fi
if [[ "$WANT_TUNNEL" == "y" ]]; then
  exit 0
fi
fi

IP=$(curl -s4m 5 https://api.ipify.org || curl -s4m 5 https://ifconfig.me || echo "127.0.0.1")
BP=$(cat "${WORK_DIR}/basepath" 2>/dev/null | tr -d ' \r\n')
BP="/${BP#/}"
BP="${BP%/}/"

ACTUAL_PORT=$(grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' "${WORK_DIR}/settings.json" 2>/dev/null | awk -F: '{print $2}' | tr -d ' ')
WEB_PORT="${ACTUAL_PORT:-8899}"

CADDY_META="${WORK_DIR}/caddy_meta.json"
if [[ -f "$CADDY_META" ]] && grep -q '"enabled"[[:space:]]*:[[:space:]]*true' "$CADDY_META"; then
  c_dom=$(grep -oE '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sout_p=$(grep -oE '"sout_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sui_p=$(grep -oE '"sui_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sui_u=$(grep -oE '"sui_user"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sui_w=$(grep -oE '"sui_pass"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  c_sub_p=$(grep -oE '"sub_path"[[:space:]]*:[[:space:]]*"[^"]*"' "$CADDY_META" 2>/dev/null | cut -d'"' -f4)
  echo
  echo "================================================================"
  echo "  🎉 sout 插件安装部署完成！(Cloudflare隧道连接和Caddy流量代理)"
  echo "================================================================"
  echo "  [sout 动态家宽出口插件]"
  echo "  管理面板:  https://${c_dom}/${c_sout_p}/"
  echo "  访问口令:  $(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 ${WORK_DIR}/password")"
  echo "  sout 唤起命令:  sout"
  echo
  echo "  [s-ui (Sing-Box) 节点面板]"
  echo "  s-ui 面板:  https://${c_dom}/${c_sui_p}/"
  echo "  s-ui 用户名:  ${c_sui_u:-admin}"
  echo "  s-ui 密  码:  [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
    echo "  订阅链接:    https://${c_dom}/${c_sout_p}/sub=$(cat "${WORK_DIR}/password" 2>/dev/null || echo "")"
  echo "  s-ui 唤起命令:  s-ui"
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
  echo "  管理面板:  http://${IP}:${WEB_PORT}${BP}"
  echo "  访问口令:  $(cat "${WORK_DIR}/password" 2>/dev/null || echo "见 ${WORK_DIR}/password")"
  echo "  sout 唤起命令:  sout"
  echo
  if check_sui; then
    echo "  [s-ui (Sing-Box) 节点面板]"
    echo "  s-ui 面板:  http://${IP}:${sui_port}${sui_path}"
    echo "  s-ui 用户名:  ${sui_u}"
    echo "  s-ui 密  码:  [由您在 s-ui 中设置，若未进行设置，可在终端唤起 s-ui 进行配置]"
    echo "  s-ui 唤起命令:  s-ui"
  fi
  echo "================================================================"
  echo
fi
