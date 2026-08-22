#!/usr/bin/env bash
set -e

UNIT="fanout.service"
BIN="/usr/local/bin/fanout"
WORK_DIR="/var/lib/fanout"
DEFAULT_PORT=8899

R='\033[31m'; G='\033[32m'; Y='\033[33m'; B='\033[34m'; D='\033[90m'; N='\033[0m'

need_root() {
  if [[ $EUID -ne 0 ]]; then
    echo -e "${R}请使用 root 权限运行此脚本 (sudo f)${N}"
    exit 1
  fi
}

svc_status()     { systemctl is-active "$UNIT" 2>/dev/null || echo inactive; }
svc_is_enabled() { systemctl is-enabled "$UNIT" 2>/dev/null | grep -q 'enabled'; }
svc_start()      { systemctl start "$UNIT"; }
svc_stop()       { systemctl stop "$UNIT"; }
svc_restart()    { systemctl restart "$UNIT"; }
svc_reload()     { systemctl daemon-reload; }
svc_enable()     { systemctl enable "$UNIT"; }
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

show_info() {
  local st la port bp pw pip purl
  st=$(svc_status)
  la=$(web_listen_addr)
  port=$(web_port)
  bp=$(web_basepath)
  pw=$(web_password)
  pip=$(public_ip)
  purl=$(web_panel_url)

  echo
  if [[ "$st" == "active" ]]; then
    echo -e "  服务状态:    ${G}运行中 (active)${N}"
  else
    echo -e "  服务状态:    ${R}已停止 (${st})${N}"
  fi
  echo -e "  开机自启:    $(svc_is_enabled && echo -e "${G}已开启${N}" || echo -e "${D}已关闭${N}")"
  
  if [[ "$la" == "127.0.0.1" ]]; then
    echo -e "  监听地址:    ${Y}127.0.0.1 (本地反向代理模式)${N}"
    if [[ -n "$purl" ]]; then
      echo -e "  本地地址:    ${B}${purl}${N}"
    else
      echo -e "  本地地址:    ${B}http://127.0.0.1:${port}${bp}${N}"
      echo -e "  公网访问:    ${D}(仅能通过您配置的反向代理域名访问)${N}"
    fi
  else
    echo -e "  监听地址:    ${G}0.0.0.0 (所有公网网卡)${N}"
    if [[ -n "$purl" ]]; then
      echo -e "  管理面板:    ${B}${purl}${N}"
    else
      echo -e "  管理面板:    ${B}http://${pip}:${port}${bp}${N}"
    fi
  fi
  echo -e "  访问口令:    ${Y}${pw}${N}"
  echo
}

list_tunnels() {
  local port bp pw
  port=$(web_port)
  bp=$(web_basepath)
  pw=$(web_password)
  echo
  echo -e "  ${B}当前运行中的出口隧道列表：${N}"
  python3 -c "
import urllib.request, urllib.parse, http.cookiejar, json
try:
    cj = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
    login_url = f'http://127.0.0.1:${port}${bp}login'
    login_data = urllib.parse.urlencode({'password': '${pw}'}).encode()
    opener.open(login_url, data=login_data, timeout=3)
    
    req = urllib.request.Request(f'http://127.0.0.1:${port}${bp}api/tunnels')
    res = opener.open(req, timeout=4)
    data = json.loads(res.read().decode())
    if not data:
        print('  (暂无运行中的出口隧道，请在面板中新建)')
    else:
        print('  ' + f'{\"槽位\":<6} {\"状态\":<10} {\"出口 IP\":<18} {\"SOCKS5 端口\":<14} {\"国家/地区\"}')
        print('  ' + '-'*68)
        for t in data:
            slot = t.get('slot', '-')
            st = t.get('status', '-')
            ip = t.get('exit_ip') or '连接中...'
            port = t.get('port', '-')
            node = t.get('node', {})
            c = node.get('country') or node.get('country_code') or '-'
            print('  ' + f'{slot:<6} {st:<10} {ip:<18} 127.0.0.1:{port:<6} {c}')
except Exception as e:
    print('  获取隧道列表失败:', e)
"
  echo
}

change_port() {
  local cur new
  cur=$(web_port)
  echo
  read -rp "  请输入新管理端口 (当前 ${cur}): " new
  [[ -z "$new" ]] && { echo "  未修改"; return; }
  if ! [[ "$new" =~ ^[0-9]+$ ]] || (( new < 1 || new > 65535 )); then
    echo -e "  ${R}端口不合法${N}"; return
  fi
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    sed -i "s/\"port\"[[:space:]]*:[[:space:]]*[0-9]*/\"port\": ${new}/" "$WORK_DIR/settings.json"
  else
    printf '{\n  \"port\": %s,\n  \"listen_addr\": \"\"\n}\n' "$new" > "$WORK_DIR/settings.json"
    chmod 600 "$WORK_DIR/settings.json"
  fi
  svc_restart
  echo -e "  ${G}管理端口已修改为 ${new}${N}"
}

change_listen_addr() {
  local cur
  cur=$(web_listen_addr)
  echo
  echo -e "  当前监听地址: ${B}${cur}${N}"
  echo "  1) 127.0.0.1 (仅本地监听，便于 Nginx / 1Panel / Caddy 等反向代理)"
  echo "  2) 0.0.0.0   (公网 IP 直接访问)"
  echo
  read -rp "  请选择 [1/2]: " opt
  local new_addr=""
  case "$opt" in
    1) new_addr="127.0.0.1" ;;
    2) new_addr="0.0.0.0" ;;
    *) echo "  取消操作"; return ;;
  esac

  local port
  port=$(web_port)
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    if grep -q '"listen_addr"' "$WORK_DIR/settings.json"; then
      sed -i "s/\"listen_addr\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"listen_addr\": \"${new_addr}\"/" "$WORK_DIR/settings.json"
    else
      sed -i "s/\"port\"[[:space:]]*:[[:space:]]*[0-9]*/\"port\": ${port},\n  \"listen_addr\": \"${new_addr}\"/" "$WORK_DIR/settings.json"
    fi
  else
    printf '{\n  \"port\": %s,\n  \"listen_addr\": \"%s\"\n}\n' "$port" "$new_addr" > "$WORK_DIR/settings.json"
    chmod 600 "$WORK_DIR/settings.json"
  fi

  svc_restart
  if [[ "$new_addr" == "127.0.0.1" ]]; then
    echo -e "  ${G}已成功切换为 127.0.0.1 监听（仅允许本地反向代理访问）${N}"
  else
    echo -e "  ${G}已成功切换为 0.0.0.0 监听（公网 IP 可直接访问）${N}"
  fi
}

change_panel_url() {
  local cur new_url
  cur=$(web_panel_url)
  echo
  echo -e "  当前面板 URL: ${B}${cur:-(未设置)}${N}"
  read -rp "  请输入新面板 URL (如 https://example.com/xxxx/，留空清除): " new_url
  new_url=$(echo "$new_url" | tr -d ' \r\n')

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
    echo -e "  ${G}面板 URL 已更新为: ${new_url}${N}"
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

cleanup_sui() {
  local db="/usr/local/s-ui/db/s-ui.db"
  if [[ ! -f "$db" ]]; then
    return
  fi
  echo -e "  正在清理 s-ui 中由 fanout 创建的所有出入站及路由规则..."
  python3 -c "
import sqlite3, json

db = '/usr/local/s-ui/db/s-ui.db'
try:
    con = sqlite3.connect(db)
    cur = con.cursor()
    
    # 1. 查找所有 fanout 创建的入站 ID 和 Tag
    cur.execute('SELECT id, tag FROM inbounds')
    inbounds = cur.fetchall()
    
    fanout_inb_ids = []
    fanout_inb_tags = []
    for (ib_id, tag) in inbounds:
        tag_str = str(tag)
        if '家宽' in tag_str or 'fanout' in tag_str:
            fanout_inb_ids.append(ib_id)
            fanout_inb_tags.append(tag_str)
    
    if fanout_inb_ids:
        for ib_id in fanout_inb_ids:
            con.execute('DELETE FROM inbounds WHERE id = ?', (ib_id,))
        print(f'    - 已清理 {len(fanout_inb_ids)} 个 fanout 分流入站: {fanout_inb_tags}')
    
    # 2. 删除所有 fanout-* 出站
    cur.execute(\"DELETE FROM outbounds WHERE tag LIKE 'fanout-%'\")
    out_deleted = cur.rowcount
    if out_deleted > 0:
        print(f'    - 已清理 {out_deleted} 个 fanout 出站隧道')

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
                if not outbound.startswith('fanout-'):
                    new_rules.append(r)
            if len(new_rules) != len(rules):
                cfg['route']['rules'] = new_rules
                con.execute(\"UPDATE settings SET value = ? WHERE key = 'config'\", (json.dumps(cfg, indent=2),))
                print(f'    - 已清理 {len(rules) - len(new_rules)} 条 fanout 分流路由规则')
        except Exception as e:
            print('    - 清理路由规则警告:', e)

    # 4. 清理 clients
    cur.execute(\"DELETE FROM clients WHERE name LIKE 'fanout-%'\")

    con.commit()
    con.close()
    print('  已完全恢复 s-ui 原始数据库与节点配置。')
except Exception as e:
    print('  清理 s-ui 数据时出现异常:', e)
" 2>/dev/null || true

  systemctl restart s-ui 2>/dev/null || true
}

do_uninstall() {
  local yes
  echo
  read -rp "  确定彻底卸载 fanout 并清理所有相关出入站节点吗？[y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  echo "  正在停止并清理 fanout 服务..."
  svc_stop >/dev/null 2>&1 || true
  svc_disable >/dev/null 2>&1 || true
  for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep '^fo[0-9]'); do
    ip netns del "$ns" 2>/dev/null || true
  done
  for l in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | grep '^fov[0-9]'); do
    ip link del "$l" 2>/dev/null || true
  done

  # 清理 s-ui 中所有 fanout 生成的出入站与路由
  cleanup_sui

  rm -f "/etc/systemd/system/$UNIT" "$BIN" /usr/local/bin/f
  rm -rf "$WORK_DIR"
  svc_reload
  echo -e "  ${G}fanout 已彻底卸载完成，所有由 fanout 创建的出入站已全部清理，s-ui 完全保持原样！${N}"
  exit 0
}

menu() {
  while true; do
    clear
    echo -e "${B}========================================${N}"
    echo -e "${B}  fanout - VPN Gate 动态出口管理 (s-ui)  ${N}"
    echo -e "${B}========================================${N}"
    show_info
    echo -e "${D}----------------------------------------${N}"
    echo -e "   1) 启动服务          2) 停止服务"
    echo -e "   3) 重启服务          4) 查看运行日志"
    echo
    echo -e "   5) 查看出口隧道      6) 查看连接信息"
    echo
    echo -e "   7) 修改面板端口      8) 修改监听地址"
    echo -e "   9) 重置访问口令     10) 重置访问路径"
    echo -e "  11) 面板 URL 设置"
    echo
    echo -e "  12) 开关开机自启     13) 彻底卸载 fanout"
    echo -e "   0) 退出脚本"
    echo -e "${D}----------------------------------------${N}"
    read -rp "  请选择 [0-13]: " choice

    case "$choice" in
      1) svc_start   && echo -e "\n  ${G}已启动${N}"; pause ;;
      2) svc_stop    && echo -e "\n  ${Y}已停止${N}"; pause ;;
      3) svc_restart && echo -e "\n  ${G}已重启${N}"; pause ;;
      4) echo; svc_logs 40; pause ;;
      5) list_tunnels; pause ;;
      6) show_info; pause ;;
      7) change_port; pause ;;
      8) change_listen_addr; pause ;;
      9) reset_password; pause ;;
      10) reset_basepath; pause ;;
      11) change_panel_url; pause ;;
      12)
        if svc_is_enabled; then
          svc_disable
          echo -e "\n  ${Y}已关闭开机自启${N}"
        else
          svc_enable
          echo -e "\n  ${G}已开启开机自启${N}"
        fi
        pause ;;
      13) do_uninstall; pause ;;
      0) exit 0 ;;
      *) ;;
    esac
  done
}

need_root

case "${1:-}" in
  start)     svc_start ;;
  stop)      svc_stop ;;
  restart)   svc_restart ;;
  status)    show_info ;;
  log)       svc_logs_follow ;;
  info)      show_info ;;
  list)      list_tunnels ;;
  listen)    change_listen_addr ;;
  url)       change_panel_url ;;
  uninstall) do_uninstall ;;
  "")        menu ;;
  *)
    echo "用法: f [start|stop|restart|status|log|info|list|listen|url|uninstall]"
    echo "直接输入 f 即可进入交互控制菜单"
    ;;
esac
