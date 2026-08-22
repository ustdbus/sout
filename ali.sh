#!/usr/bin/env bash

set -e
export UUID=${UUID:-$(cat /proc/sys/kernel/random/uuid 2>/dev/null)}
export DEBIAN_FRONTEND=noninteractive

# 安装目录
APP_DIR="/run/csi/mount-root/nas/myapp"
STATE_FILE="${APP_DIR}/.project_type"
APP_NAME=$(tr -dc a-z </dev/urandom | head -c 6)
export ARGO_PORT=${ARGO_PORT:-'8001'} 
export ARGO_DOMAIN=${ARGO_DOMAIN}
export ARGO_AUTH=${ARGO_AUTH}
export NEZHA_SERVER=${NEZHA_SERVER}
export NEZHA_PORT=${NEZHA_PORT}
export NEZHA_KEY=${NEZHA_KEY}
export SUB_PATH=${SUB_PATH}

# 运行前先清理可能重复运行的旧进程
pkill -f '\.npm/' >/dev/null 2>&1 || true
pkill -f '\.cache/' >/dev/null 2>&1 || true
screen -ls | grep ${APP_NAME} | awk '{print $1}' | xargs -I {} screen -X -S {} quit >/dev/null 2>&1 || true

# 卸载模式：支持 -u 或 uni
if [[ "$1" == "-u" || "$1" == "u" || "$1" == "uninstall" ]]; then
    echo "执行卸载操作..."

    if [[ -f "${STATE_FILE}" ]]; then
        INSTALLED_TYPE=$(cat "${STATE_FILE}")
        echo "检测到已安装项目: ${INSTALLED_TYPE}"
    else
        echo "未检测到项目状态文件，将执行清理..."
        INSTALLED_TYPE="unknown"
    fi
    
    # 清理进程和screen
    pkill -f '\.npm/' >/dev/null 2>&1 || true
    pkill -f '\.cache/' >/dev/null 2>&1 || true
    screen -ls | grep ${APP_NAME} | awk '{print $1}' | xargs -I {} screen -X -S {} quit 2>/dev/null || true

    echo "删除项目目录"
    rm -rf "${APP_DIR}"

    echo ""
    echo -e "\e[1;32m卸载完成\033[0m"
    exit 0
fi

# 随机选择项目类型
if [[ "$1" == "-js" || "$1" == "js" || "$1" == "nodejs" ]]; then
    PROJECT_TYPE="nodejs"
elif [[ "$1" == "-py" || "$1" == "py" || "$1" == "python" ]]; then
    PROJECT_TYPE="python"
elif [[ -z "$1" ]]; then
    RANDOM_CHOICE=$((RANDOM % 2))
    if [[ $RANDOM_CHOICE -eq 0 ]]; then
        PROJECT_TYPE="nodejs"
        echo -e "\e[1;33m未指定项目类型，随机选择: Nodejs\033[0m"
    else
        PROJECT_TYPE="python"
        echo -e "\e[1;33m未指定项目类型，随机选择: Python\033[0m"
    fi
else
    echo -e "\e[1;31m错误：无效参数\033[0m"
    echo "用法："
    echo "  bash install.sh         随机选择 Nodejs 或 Python 项目"
    echo "  bash install.sh -js     启动 Node.js 项目"
    echo "  bash install.sh -py     启动 Python 项目"
    echo "  bash install.sh -u      卸载项目"
    exit 1
fi

# 安装公共依赖
echo "安装系统依赖中，请稍等..."

apt-get update -qq

apt-get install -y -qq \
curl \
wget \
git \
screen \
ca-certificates \
gnupg >/dev/null 2>&1

mkdir -p "${APP_DIR}"
echo "${PROJECT_TYPE}" > "${STATE_FILE}"
cd "${APP_DIR}"

# Node.js 项目流程
if [[ "$PROJECT_TYPE" == "nodejs" ]]; then
    if ! command -v node &> /dev/null; then
        echo "正在安装 Node.js，请稍等..."
        curl -fsSL https://deb.nodesource.com/setup_current.x | bash - >/dev/null 2>&1
        apt-get install -y -qq nodejs >/dev/null 2>&1
    else
        echo "Node.js 已安装，跳过"
    fi

    echo "下载核心文件..."
    wget -q -O index.html https://raw.githubusercontent.com/eooce/node-ws/main/index.html
    wget -q -O index.js https://raw.githubusercontent.com/eooce/nodejs-argo/native/index.js

    echo "初始化 npm ..."
    npm init -y >/dev/null 2>&1

    echo "安装项目依赖中, 请稍等..."
    npm install axios ws dotenv koffi javascript-obfuscator >/dev/null 2>&1
    
    echo "配置环境变量..."
    echo "UUID=${UUID}" > "${APP_DIR}/.env"
    echo "SHOW_LOG=no" >> "${APP_DIR}/.env"
    [[ -n "${SUB_PATH}" ]] && echo "SUB_PATH=${SUB_PATH}" >> "${APP_DIR}/.env"
    [[ -n "${NEZHA_SERVER}" ]] && echo "NEZHA_SERVER=${NEZHA_SERVER}" >> "${APP_DIR}/.env"
    [[ -n "${NEZHA_PORT}" ]] && echo "NEZHA_PORT=${NEZHA_PORT}" >> "${APP_DIR}/.env"
    [[ -n "${NEZHA_KEY}" ]] && echo "NEZHA_KEY=${NEZHA_KEY}" >> "${APP_DIR}/.env"
    [[ -n "${ARGO_DOMAIN}" ]] && echo "ARGO_DOMAIN=${ARGO_DOMAIN}" >> "${APP_DIR}/.env"
    [[ -n "${ARGO_AUTH}" ]] && echo "ARGO_AUTH=${ARGO_AUTH}" >> "${APP_DIR}/.env"
    [[ -n "${ARGO_PORT}" ]] && echo "ARGO_PORT=${ARGO_PORT}" >> "${APP_DIR}/.env"

    echo "正在混淆文件..."
    npx javascript-obfuscator index.js \
    --output ${APP_NAME}.js \
    --compact true \
    --control-flow-flattening true \
    --control-flow-flattening-threshold 0.5 \
    --dead-code-injection true \
    --dead-code-injection-threshold 0.2 \
    --string-array true \
    --string-array-threshold 0.75 \
    --rename-globals false \
    >/dev/null 2>&1

    rm -f index.js  >/dev/null 2>&1

    echo "启动项目..."
    set -a; source "${APP_DIR}/.env"; set +a
    screen -dm -S ${APP_NAME} node ${APP_NAME}.js

# Python 项目流程
elif [[ "$PROJECT_TYPE" == "python" ]]; then
    if ! command -v python3 &> /dev/null; then
        echo "正在安装 Python3，请稍等..."
        apt-get install -y -qq python3 python3-venv python3-pip >/dev/null 2>&1
    else
        echo "Python3 已安装，跳过"
    fi
    
    if ! command -v python3 &> /dev/null; then
        echo -e "\e[1;31mPython3 安装失败\033[0m"
        exit 1
    fi

    echo "下载 Python 项目文件..."
    wget -q -O app.py https://raw.githubusercontent.com/eooce/python-xray-argo/native/app.py
    wget -q -O index.html https://github.com/eooce/python-ws/raw/refs/heads/main/index.html
    echo "创建 Python 虚拟环境..."
    python3 -m venv venv
    source venv/bin/activate
    echo "安装 Python 依赖..."
    pip install requests python-dotenv cryptography -qqq 
    echo "配置环境变量..."
    echo "UUID=${UUID}" > "${APP_DIR}/.env"
    echo "SHOW_LOG=no" >> "${APP_DIR}/.env"
    [[ -n "${SUB_PATH}" ]] && echo "SUB_PATH=${SUB_PATH}" >> "${APP_DIR}/.env"
    [[ -n "${NEZHA_SERVER}" ]] && echo "NEZHA_SERVER=${NEZHA_SERVER}" >> "${APP_DIR}/.env"
    [[ -n "${NEZHA_PORT}" ]] && echo "NEZHA_PORT=${NEZHA_PORT}" >> "${APP_DIR}/.env"
    [[ -n "${NEZHA_KEY}" ]] && echo "NEZHA_KEY=${NEZHA_KEY}" >> "${APP_DIR}/.env"
    [[ -n "${ARGO_DOMAIN}" ]] && echo "ARGO_DOMAIN=${ARGO_DOMAIN}" >> "${APP_DIR}/.env"
    [[ -n "${ARGO_AUTH}" ]] && echo "ARGO_AUTH=${ARGO_AUTH}" >> "${APP_DIR}/.env"
    [[ -n "${ARGO_PORT}" ]] && echo "ARGO_PORT=${ARGO_PORT}" >> "${APP_DIR}/.env"

    echo "正在混淆 Python 代码..."
    # 读取文件内容并进行 JSON 转义
    CODE_JSON=$(cat app.py | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')
    
    # 调用混淆 API 并提取 obfuscated 字段
    OBFUSCATED=$(curl -s -X POST https://obf.eooce.com/api/obfuscate \
        -H "Content-Type: application/json" \
        -d "{\"code\": ${CODE_JSON}}" | \
        grep -o '"obfuscated":"[^"]*"' | \
        sed 's/"obfuscated":"//' | \
        sed 's/"$//' | \
        sed 's/\\n/\n/g' | \
        sed 's/\\"/"/g' | \
        sed 's/\\\\/\\/g')
    
    if [[ -n "${OBFUSCATED}" ]]; then
        echo "${OBFUSCATED}" > ${APP_NAME}.py
        rm -f app.py  >/dev/null 2>&1
    else
        echo -e "\e[1;33m警告：代码混淆失败，使用原始代码\033[0m"
    fi

    echo "启动 Python 项目..."
    set -a; source "${APP_DIR}/.env"; set +a
    screen -dm -S ${APP_NAME} ${APP_DIR}/venv/bin/python3 ${APP_NAME}.py
fi

echo "请稍等35秒，等待项目启动并生成节点..."
sleep 35

echo ""
echo -e "\e[1;32m安装完成\033[0m"
echo ""
echo "项目类型: ${PROJECT_TYPE}"
echo "APP_NAME: ${APP_NAME}"
echo "节点信息如下: "
if [ "$PROJECT_TYPE" == "nodejs" ]; then
    cat ${APP_DIR}/.npm/sub.txt
elif [ "$PROJECT_TYPE" == "python" ]; then
    cat ${APP_DIR}/.cache/sub.txt
fi
echo ""
exit 0
