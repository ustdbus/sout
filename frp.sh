#!/bin/bash
# FRP 管理脚本（systemd 用 systemctl，supervisord 用 nohup）
# 集成式FRP内网穿透配置脚本（带信息保存功能）
# 安装服务端/客户端时自动保存配置到/home/frp/info.txt
# 官方服务端口示例配置：https://github.com/fatedier/frp/blob/dev/conf/frps_full_example.toml
# 官方客户端口示例配置：https://github.com/fatedier/frp/blob/dev/conf/frpc_full_example.toml

red() { echo -e "\e[1;91m$1\033[0m"; }
green() { echo -e "\e[1;32m$1\033[0m"; }
yellow() { echo -e "\e[1;33m$1\033[0m"; }
purple() { echo -e "\e[1;35m$1\033[0m"; }
reading() { read -p "$(red "$1")" "$2"; }

# 环境变量
export FRP_VERSION=${FRP_VERSION:-'0.70.0'}  
export FRP_DIR=${FRP_DIR:-'/home/frp'} 
export SSH_PORT=${SSH_PORT:-'22'} 
export HTTPS_PORT=${HTTPS_PORT:-'443'} 
export INFO_FILE="${FRP_DIR}/info.txt"

# 检测 init 系统
get_init_system() {
    local init=$(ps -p 1 -o comm=)
    echo "$init"
}
INIT_SYSTEM=$(get_init_system)

check_root() {
    [ "$(id -u)" != "0" ] && { red "错误: 此脚本需要以root权限运行"; exit 1; }
}

get_server_ip() {
    local ipv4=$(curl -s --max-time 2 ipv4.ip.sb)
    if [ -n "$ipv4" ]; then
        echo "$ipv4"
    else
        ipv6=$(curl -s --max-time 2 ipv6.ip.sb)
        echo "[$ipv6]"
    fi
}

get_arch() {
    ARCH=$(uname -m)
    case ${ARCH} in
        x86_64|amd64) echo "amd64";;
        arm64|aarch64) echo "arm64";;
        *) red "不支持的架构: ${ARCH}"; exit 1;;
    esac
}

init_frp_dir() {
    mkdir -p "${FRP_DIR}" || { red "创建FRP目录失败"; exit 1; }
    cd "${FRP_DIR}" || exit 1
}

download_file() {
    local url="$1"
    local output="$2"
    if command -v wget &>/dev/null; then
        wget -q --show-progress "$url" -O "$output"
    elif command -v curl &>/dev/null; then
        curl -L -o "$output" "$url"
    else
        red "既没有 wget 也没有 curl，无法下载"
        exit 1
    fi
}

download_frp() {
    local ARCH=$1
    FRP_PACKAGE="frp_${FRP_VERSION}_linux_${ARCH}.tar.gz"
    local URL="https://github.com/fatedier/frp/releases/download/v${FRP_VERSION}/${FRP_PACKAGE}"
    
    [ ! -f "${FRP_PACKAGE}" ] && {
        yellow "下载frp v${FRP_VERSION}..."
        download_file "$URL" "${FRP_PACKAGE}" || {
            red "下载frp失败"; exit 1
        }
    }
    
    tar -zxvf "${FRP_PACKAGE}" >/dev/null || { red "解压frp失败"; exit 1; }
    mv frp_${FRP_VERSION}_linux_${ARCH}/* ${FRP_DIR}/
    rm -rf frp_${FRP_VERSION}_linux_${ARCH} ${FRP_PACKAGE}
}

# ==================== SSH 配置（通用） ====================
set_root_password() {
    yellow "正在安装依赖并配置SSH..."
    apt update -qq
    apt install -y -qq openssh-server openssl wget curl

    mkdir -p /run/sshd
    chmod 0755 /run/sshd
    if [ ! -f /etc/ssh/ssh_host_rsa_key ]; then
        ssh-keygen -A
    fi

    grep -q "^PermitRootLogin" /etc/ssh/sshd_config && \
        sed -i 's/^PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config || \
        echo "PermitRootLogin yes" >> /etc/ssh/sshd_config
    grep -q "^PasswordAuthentication" /etc/ssh/sshd_config && \
        sed -i 's/^PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config || \
        echo "PasswordAuthentication yes" >> /etc/ssh/sshd_config

    reading "请输入root密码 [提示: 回车留空将随机生成]: " ROOT_PWD
    [ -z "$ROOT_PWD" ] && {
        ROOT_PWD=$(openssl rand -hex 8)
        yellow "随机root密码为: $ROOT_PWD"
    }
    echo "root:${ROOT_PWD}" | chpasswd || { red "root密码设置失败"; exit 1; }

    pkill -9 sshd 2>/dev/null
    /usr/sbin/sshd
    sleep 1
    if pgrep -x sshd >/dev/null; then
        green "SSH服务运行正常"
    else
        /usr/sbin/sshd
        sleep 1
        pgrep -x sshd >/dev/null && green "SSH服务已手动启动" || red "SSH服务启动失败，请检查 /var/log/auth.log"
    fi
}

# ==================== 服务管理（systemd 专用） ====================
systemd_manage() {
    local action=$1
    local service=$2  # frps 或 frpc
    local servicename="frp${service#frp}"
    case $action in
        start)   systemctl start ${servicename} ;;
        stop)    systemctl stop ${servicename} ;;
        restart) systemctl restart ${servicename} ;;
        status)  systemctl is-active ${servicename} ;;
        enable)  systemctl enable --now ${servicename} ;;
    esac
}

# ==================== nohup 管理（supervisord 专用） ====================
nohup_start() {
    local service=$1
    local pidfile="/var/run/${service}.pid"
    local logfile="/var/log/${service}.log"
    
    if [ -f "$pidfile" ] && kill -0 $(cat "$pidfile") 2>/dev/null; then
        yellow "${service} 已在运行 (PID: $(cat $pidfile))"
        return 0
    fi
    
    cd "${FRP_DIR}" || exit 1
    nohup ./${service} -c ./${service}.toml >> "$logfile" 2>&1 &
    local pid=$!
    echo $pid > "$pidfile"
    sleep 1
    if kill -0 $pid 2>/dev/null; then
        green "${service} 启动成功 (PID: $pid)"
        return 0
    else
        red "${service} 启动失败，请检查日志: $logfile"
        return 1
    fi
}

nohup_stop() {
    local service=$1
    local pidfile="/var/run/${service}.pid"
    if [ -f "$pidfile" ]; then
        local pid=$(cat "$pidfile")
        if kill -0 $pid 2>/dev/null; then
            kill $pid
            sleep 1
            kill -0 $pid 2>/dev/null && kill -9 $pid
        fi
        rm -f "$pidfile"
    fi
    pkill -f "${service} -c" 2>/dev/null
    green "${service} 已停止"
}

nohup_status() {
    local service=$1
    local pidfile="/var/run/${service}.pid"
    if [ -f "$pidfile" ] && kill -0 $(cat "$pidfile") 2>/dev/null; then
        echo "active"
    else
        echo "inactive"
    fi
}

# ==================== 保存与显示配置 ====================
save_config_info() {
    local mode=$1
    shift
    echo "=== FRP ${mode} 配置信息 ===" > "$INFO_FILE"
    echo "生成时间: $(date "+%Y-%m-%d %H:%M:%S")" >> "$INFO_FILE"
    for item in "$@"; do
        IFS='|' read -r name value <<< "$item"
        echo "${name}: ${value}" >> "$INFO_FILE"
    done
    echo "=========================" >> "$INFO_FILE"
}

show_config() {
    local mode=$1
    shift
    yellow "\n============= ${mode}配置确认 ============="
    for item in "$@"; do
        IFS='|' read -r name value <<< "$item"
        purple "${name}: ${value}"
    done
    purple "===================================="
    
    reading "确认以上配置是否正确？(y/n) [默认: y]: " CONFIRM
    CONFIRM=${CONFIRM:-"y"}
    [[ "${CONFIRM,,}" != "y" && "${CONFIRM,,}" != "yes" ]] && { yellow "配置已取消"; exit 1; }
}

show_info() {
    clear
    if [ -f "$INFO_FILE" ]; then
        echo ""
        cat "$INFO_FILE"
        
        local service_name status_display
        if grep -q "服务端" "$INFO_FILE"; then
            service_name="frps"
        else
            service_name="frpc"
        fi

        # 根据 init 系统获取状态
        if [ "$INIT_SYSTEM" = "systemd" ]; then
            local status=$(systemd_manage status "$service_name")
            [ "$status" = "active" ] && status_display="\e[1;32mactive\033[0m" || status_display="\e[1;31minactive\033[0m"
        else
            local status=$(nohup_status "$service_name")
            [ "$status" = "active" ] && status_display="\e[1;32mactive\033[0m" || status_display="\e[1;31minactive\033[0m"
        fi
        echo -e "\e[1;35m服务运行状态: ${status_display}\033[0m\n\n"
    else
        red "未找到配置信息文件，请先安装FRP服务\n"
    fi
    
    read -rsn1 -p "$(red "按任意键返回主菜单...")"
    echo
    main_menu
}

# ==================== 配置输入 ====================
server_config() {
    reading "请输入FRP服务端监听端口 [默认: 7000]: " BIND_PORT
    BIND_PORT=${BIND_PORT:-"7000"}
    green "服务端监听端口为：$BIND_PORT"
    
    reading "请输入认证TOKEN [回车将自动随机生成]: " TOKEN
    [ -z "$TOKEN" ] && TOKEN=$(< /dev/urandom tr -dc 'A-Za-z0-9' | head -c 16)
    green "验证token为：$TOKEN"
    
    reading "请输入web端口 [默认: 7500]: " DASHBOARD_PORT
    DASHBOARD_PORT=${DASHBOARD_PORT:-"7500"}
    green "web端口为：$DASHBOARD_PORT"
    
    reading "请输入web用户名 [默认: admin]: " DASHBOARD_USER
    DASHBOARD_USER=${DASHBOARD_USER:-"admin"}
    green "web登录用户名为：$DASHBOARD_USER"
    
    reading "请输入web登录密码 [默认: 回车将随机生成]: " DASHBOARD_PWD
    [ -z "$DASHBOARD_PWD" ] && DASHBOARD_PWD=$(openssl rand -hex 8)
    green "web登录密码为：$DASHBOARD_PWD"
}

client_config() {
    reading "请输入中继服务器公网IP: " SERVER_IP
    while [ -z "$SERVER_IP" ]; do
        reading "中继服务器IP不能为空，请重新输入: " SERVER_IP
    done
    green "FRP中继服务器IP为：$SERVER_IP"
    
    reading "请输入中继服务器FRP端口 [默认: 7000]: " SERVER_PORT
    SERVER_PORT=${SERVER_PORT:-"7000"}
    green "FRP中继服务器通信端口为：$SERVER_PORT"
    
    reading "请输入认证TOKEN: " TOKEN
    while [ -z "$TOKEN" ]; do
        reading "TOKEN不能为空，请重新输入: " TOKEN
    done
    green "认证token为：$TOKEN"
    
    reading "请输入SSH远程映射端口 (本地22) [默认: 7322]: " REMOTE_SSH_PORT
    REMOTE_SSH_PORT=${REMOTE_SSH_PORT:-"7322"}
    green "SSH远程映射端口为：$REMOTE_SSH_PORT"

    reading "请输入HTTPS远程映射端口 (本地443) [默认: 7323]: " REMOTE_HTTPS_PORT
    REMOTE_HTTPS_PORT=${REMOTE_HTTPS_PORT:-"7323"}
    green "HTTPS远程映射端口为：$REMOTE_HTTPS_PORT"
}

# ==================== 安装服务端 ====================
install_server() {
    yellow "\n开始安装FRP服务端 v${FRP_VERSION}..."
    
    ARCH=$(get_arch)
    init_frp_dir
    download_frp "$ARCH"
    
    cat > ${FRP_DIR}/frps.toml <<EOF
bindAddr = "0.0.0.0"
bindPort = ${BIND_PORT}
quicBindPort = ${BIND_PORT}

auth.method = "token"
auth.token = "${TOKEN}"

webServer.addr = "0.0.0.0"
webServer.port = ${DASHBOARD_PORT}
webServer.user = "${DASHBOARD_USER}"
webServer.password = "${DASHBOARD_PWD}"

log.to = "/var/log/frps.log"
log.level = "info"
log.maxDays = 3

enablePrometheus = true
EOF

    if [ "$INIT_SYSTEM" = "systemd" ]; then
        cat > /etc/systemd/system/frps.service <<EOF
[Unit]
Description=Frp Server Service
After=network.target

[Service]
Type=simple
User=root
Restart=on-failure
RestartSec=5s
ExecStart=${FRP_DIR}/frps -c ${FRP_DIR}/frps.toml
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemd_manage enable frps
        if [ "$(systemd_manage status frps)" = "active" ]; then
            echo -e "\n\e[1;35m服务状态: \e[1;32mactive\033[0m\n"
            local SERVER_IP=$(get_server_ip)
            green "\nFRP服务端安装完成!\n"
            save_config_info "服务端" \
                "FRP版本号|${FRP_VERSION}" \
                "安装目录|${FRP_DIR}" \
                "监听IP|${SERVER_IP}" \
                "监听端口|${BIND_PORT}" \
                "认证TOKEN|${TOKEN}" \
                "web端口|${DASHBOARD_PORT}" \
                "web登录用户名|${DASHBOARD_USER}" \
                "web登录密码|${DASHBOARD_PWD}"
            yellow "====== 客户端与服务端通信信息 ======"
            green "监听端口: ${BIND_PORT}"
            green "监听IP: ${SERVER_IP}"
            green "认证TOKEN: ${TOKEN}\n"
            purple "====== web管理信息 ======"
            green "Web地址: http://${SERVER_IP}:${DASHBOARD_PORT}"
            green "用户名: ${DASHBOARD_USER}"
            green "登录密码: ${DASHBOARD_PWD}\n"
        else
            red "FRP服务端启动失败，请检查日志"
            exit 1
        fi
    else
        # supervisord 或其他：使用 nohup 立即启动，同时生成 supervisor 配置以便持久化
        yellow "检测到当前 init 系统为 ${INIT_SYSTEM}，将使用 nohup 启动并生成 Supervisor 配置文件"
        cat > /etc/supervisor/conf.d/frps.conf <<EOF
[program:frps]
command=${FRP_DIR}/frps -c ${FRP_DIR}/frps.toml
autostart=true
autorestart=true
user=root
stdout_logfile=/var/log/frps.out.log
stderr_logfile=/var/log/frps.err.log
EOF
        green "Supervisor 配置文件已生成（重启容器后由 Supervisor 接管）"
        nohup_start frps
        if [ "$(nohup_status frps)" = "active" ]; then
            echo -e "\n\e[1;35m服务状态: \e[1;32mactive\033[0m\n"
            local SERVER_IP=$(get_server_ip)
            green "\nFRP服务端已通过 nohup 启动成功!\n"
            save_config_info "服务端" \
                "FRP版本号|${FRP_VERSION}" \
                "安装目录|${FRP_DIR}" \
                "监听IP|${SERVER_IP}" \
                "监听端口|${BIND_PORT}" \
                "认证TOKEN|${TOKEN}" \
                "web端口|${DASHBOARD_PORT}" \
                "web登录用户名|${DASHBOARD_USER}" \
                "web登录密码|${DASHBOARD_PWD}"
            yellow "====== 客户端与服务端通信信息 ======"
            green "监听端口: ${BIND_PORT}"
            green "监听IP: ${SERVER_IP}"
            green "认证TOKEN: ${TOKEN}\n"
            purple "====== web管理信息 ======"
            green "Web地址: http://${SERVER_IP}:${DASHBOARD_PORT}"
            green "用户名: ${DASHBOARD_USER}"
            green "登录密码: ${DASHBOARD_PWD}\n"
            yellow "提示: 重启容器后服务将由 Supervisor 自动管理"
        else
            red "FRP服务端启动失败，请检查 /var/log/frps.log"
            exit 1
        fi
    fi
}

# ==================== 安装客户端 ====================
install_client() {
    yellow "\n开始安装FRP客户端 v${FRP_VERSION}..."
    
    ARCH=$(get_arch)
    init_frp_dir
    download_frp "$ARCH"
    
    cat > ${FRP_DIR}/frpc.toml <<EOF
serverAddr = "${SERVER_IP}"
serverPort = ${SERVER_PORT}

auth.method = "token"
auth.token = "${TOKEN}"

log.to = "/var/log/frpc.log"
log.level = "error"
log.maxDays = 3

transport.poolCount = 5
transport.heartbeatInterval = 10
transport.heartbeatTimeout = 30
transport.dialServerKeepalive = 10
transport.dialServerTimeout = 30
transport.tcpMuxKeepaliveInterval = 10

[[proxies]]
name = "ssh_$(hostname)"
type = "tcp"
localIP = "127.0.0.1"
localPort = ${SSH_PORT}
remotePort = ${REMOTE_SSH_PORT}

[[proxies]]
name = "https_$(hostname)"
type = "tcp"
localIP = "127.0.0.1"
localPort = ${HTTPS_PORT}
remotePort = ${REMOTE_HTTPS_PORT}
EOF

    if [ "$INIT_SYSTEM" = "systemd" ]; then
        cat > /etc/systemd/system/frpc.service <<EOF
[Unit]
Description=Frp Client Service
After=network.target

[Service]
Type=simple
User=root
Restart=on-failure
RestartSec=5s
ExecStart=${FRP_DIR}/frpc -c ${FRP_DIR}/frpc.toml
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemd_manage enable frpc
        if [ "$(systemd_manage status frpc)" = "active" ]; then
            echo -e "\n\e[1;35m服务状态: \e[1;32mactive\033[0m\n"
            save_config_info "客户端" \
                "FRP版本号|${FRP_VERSION}" \
                "安装目录|${FRP_DIR}" \
                "中继服务器IP|${SERVER_IP}" \
                "中继服务器端口|${SERVER_PORT}" \
                "认证TOKEN|${TOKEN}" \
                "本地SSH端口|${SSH_PORT}" \
                "SSH远程映射端口|${REMOTE_SSH_PORT}" \
                "本地HTTPS端口|${HTTPS_PORT}" \
                "HTTPS远程映射端口|${REMOTE_HTTPS_PORT}" \
                "root密码|${ROOT_PWD}"
            green "FRP客户端安装完成!\n"
            purple "====== 端口映射与连接信息 ======"
            green "服务器IP: ${SERVER_IP}"
            green "SSH映射: 本地 ${SSH_PORT} -> 远程 ${REMOTE_SSH_PORT}"
            green "HTTPS映射: 本地 ${HTTPS_PORT} -> 远程 ${REMOTE_HTTPS_PORT}"
            green "SSH用户: root"
            green "SSH密码: ${ROOT_PWD}"
            yellow "\n温馨提示: 确保服务端已开放端口 ${SERVER_PORT}、${REMOTE_SSH_PORT} 和 ${REMOTE_HTTPS_PORT}\n"
        else
            red "FRP客户端启动失败，请检查日志"
            exit 1
        fi
    else
        # supervisord 或其他：使用 nohup 立即启动，同时生成 supervisor 配置以便持久化
        yellow "检测到当前 init 系统为 ${INIT_SYSTEM}，将使用 nohup 启动并生成 Supervisor 配置文件"
        cat > /etc/supervisor/conf.d/frpc.conf <<EOF
[program:frpc]
command=${FRP_DIR}/frpc -c ${FRP_DIR}/frpc.toml
autostart=true
autorestart=true
user=root
stdout_logfile=/var/log/frpc.out.log
stderr_logfile=/var/log/frpc.err.log
EOF
        green "Supervisor 配置文件已生成（重启容器后由 Supervisor 接管）"
        nohup_start frpc
        if [ "$(nohup_status frpc)" = "active" ]; then
            echo -e "\n\e[1;35m服务状态: \e[1;32mactive\033[0m\n"
            save_config_info "客户端" \
                "FRP版本号|${FRP_VERSION}" \
                "安装目录|${FRP_DIR}" \
                "中继服务器IP|${SERVER_IP}" \
                "中继服务器端口|${SERVER_PORT}" \
                "认证TOKEN|${TOKEN}" \
                "本地SSH端口|${SSH_PORT}" \
                "SSH远程映射端口|${REMOTE_SSH_PORT}" \
                "本地HTTPS端口|${HTTPS_PORT}" \
                "HTTPS远程映射端口|${REMOTE_HTTPS_PORT}" \
                "root密码|${ROOT_PWD}"
            green "FRP客户端已通过 nohup 启动成功!\n"
            purple "====== 端口映射与连接信息 ======"
            green "服务器IP: ${SERVER_IP}"
            green "SSH映射: 本地 ${SSH_PORT} -> 远程 ${REMOTE_SSH_PORT}"
            green "HTTPS映射: 本地 ${HTTPS_PORT} -> 远程 ${REMOTE_HTTPS_PORT}"
            green "SSH用户: root"
            green "SSH密码: ${ROOT_PWD}"
            yellow "\n温馨提示: 确保服务端已开放端口 ${SERVER_PORT}、${REMOTE_SSH_PORT} 和 ${REMOTE_HTTPS_PORT}"
            yellow "提示: 重启容器后服务将由 Supervisor 自动管理\n"
        else
            red "FRP客户端启动失败，请检查 /var/log/frpc.log"
            exit 1
        fi
    fi
}

# ==================== 卸载 ====================
uninstall_frp() {
    yellow "\n开始卸载FRP..."
    
    if [ "$INIT_SYSTEM" = "systemd" ]; then
        systemctl stop frps frpc 2>/dev/null
        rm -f /etc/systemd/system/frps.service /etc/systemd/system/frpc.service
        systemctl daemon-reload
    else
        nohup_stop frps
        nohup_stop frpc
        rm -f /etc/supervisor/conf.d/frps.conf /etc/supervisor/conf.d/frpc.conf 2>/dev/null
    fi

    [ -d "${FRP_DIR}" ] && {
        rm -rf "${FRP_DIR}"
        green "已删除FRP安装目录: ${FRP_DIR}"
    }
    
    rm -f /var/log/frps.log /var/log/frpc.log
    clear
    green "FRP卸载完成"
}

# ==================== 主菜单 ====================
main_menu() {
    clear
    purple "\n======== FRP 管理脚本 ========\n"
    green "1. 安装 FRP 服务端 (公网服务器)\n"
    green "2. 安装 FRP 客户端 (内网服务器)\n"
    purple "3. 显示当前配置信息\n"
    red "4. 卸载 FRP\n"
    yellow "0. 退出脚本\n"
    yellow "=========================="
    
    reading "请选择操作 [0-4]: " CHOICE
    case $CHOICE in
        1)
            server_config
            show_config "服务端" \
                "FRP版本号|${FRP_VERSION}" \
                "安装目录|${FRP_DIR}" \
                "监听端口|${BIND_PORT}" \
                "认证TOKEN|${TOKEN}" \
                "web端口|${DASHBOARD_PORT}" \
                "web登录用户名|${DASHBOARD_USER}" \
                "web登录密码|${DASHBOARD_PWD}" 
            install_server
            ;;
        2)
            set_root_password
            client_config
            show_config "客户端" \
                "FRP版本号|${FRP_VERSION}" \
                "安装目录|${FRP_DIR}" \
                "中继服务器IP|${SERVER_IP}" \
                "中继服务器端口|${SERVER_PORT}" \
                "认证TOKEN|${TOKEN}" \
                "本地SSH端口|${SSH_PORT}" \
                "SSH远程映射端口|${REMOTE_SSH_PORT}" \
                "本地HTTPS端口|${HTTPS_PORT}" \
                "HTTPS远程映射端口|${REMOTE_HTTPS_PORT}" \
                "root密码|${ROOT_PWD}"
            install_client
            ;;
        3)
            show_info
            ;;
        4)
            uninstall_frp
            exit 0
            ;;
        0)
            clear
            exit 0
            ;;
        *)
            red "无效选择，请重新输入"
            sleep 1
            main_menu
            ;;
    esac
    
    read -rsn1 -p "$(red "按任意键返回主菜单...")"
    echo
    main_menu
}

# ==================== 启动 ====================
check_root
main_menu
