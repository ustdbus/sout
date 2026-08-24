package main

import (
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	healthInterval = 15 * time.Second
	healthFailures = 3 // 连续失败 3 次才判定掉线，避免网络抖动或公共 API 限流误杀
	healthTimeout  = 8 * time.Second
)

// WatchHealth 周期检查每条隧道是否还能出网，掉线的自动换节点重连；离线的自动尝试复活。
func (m *Manager) WatchHealth() {
	fails := map[int]int{}
	failedCooldown := map[int]time.Time{}

	for range time.Tick(healthInterval) {
		for _, t := range m.Tunnels() {
			if t.Status == "stopped" {
				continue
			}

			// 如果是已失效/离线（failed）状态的出口，每 30 分钟进行一次周期性兜底重试
			if t.Status == "failed" {
				if last, ok := failedCooldown[t.Slot]; ok && time.Since(last) < 30*time.Minute {
					continue
				}
				failedCooldown[t.Slot] = time.Now()
				log.Printf("槽位 %d (%s) 处于离线状态，触发 30 分钟周期性兜底自愈重试...", t.Slot, t.Node.HostName)
				m.reconnect(t, t.Node.HostName)
				continue
			}

			if t.Status != "up" {
				continue
			}

			if m.tunnelHealthy(t) {
				fails[t.Slot] = 0
				continue
			}

			fails[t.Slot]++
			if fails[t.Slot] < healthFailures {
				log.Printf("隧道 %d (%s) 探测失败 %d 次", t.Slot, t.Node.HostName, fails[t.Slot])
				continue
			}

			log.Printf("隧道 %d (%s) 已掉线，正在换节点重连", t.Slot, t.Node.HostName)
			fails[t.Slot] = 0
			m.reconnect(t, t.Node.HostName)
		}
	}
}

// tunnelHealthy 判断隧道是否还真的走在 VPN 上。
//
// 只看"能不能出网"是不够的：netns 通过 veth 走母机 NAT，
// openvpn 死掉后照样能出网，只是出口变回了母机 IP。
// 所以要比对出口 IP 是否仍是建立隧道时拿到的那个。
func (m *Manager) tunnelHealthy(t *Tunnel) bool {
	if t.Kind == "custom" {
		remoteAddr := fmt.Sprintf("%s:%d", t.CustomHost, t.CustomPort)
		ip, _, _, _, err := ProbeCustomSocks(remoteAddr, t.CustomUser, t.CustomPass, healthTimeout)
		if err != nil {
			return false
		}
		if t.ExitIP != "" && ip != t.ExitIP {
			t.mu.Lock()
			t.ExitIP = ip
			t.mu.Unlock()
		}
		return true
	}
	out, err := exec.Command("ip", "netns", "exec", t.nsName(),
		"curl", "-s", "--max-time", strconv.Itoa(int(healthTimeout.Seconds())),
		"http://api.ipify.org").Output()
	if err != nil {
		return false
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		return false
	}
	// 出口 IP 变了说明 VPN 已经断开，流量退回了母机
	return got == t.ExitIP
}

// reconnect 就地把一条隧道换到别的节点上，保持槽位与端口不变，
// 这样已经分发出去的客户端配置仍然可用。
//
// oldHost 必须是本次重连前那条隧道真正绑着的节点名。调用方若已经
// 改过 t.Node（比如手动换节点），就要把改之前的名字传进来，
// 否则 rebind 找不到旧绑定，入站会掉成孤儿。
func (m *Manager) reconnect(t *Tunnel, oldHost string) {
	t.mu.Lock()
	t.Status = "starting"
	t.Err = "正在换节点重连"
	t.ExitIP = ""
	if t.listener != nil && t.Kind == "custom" {
		_ = t.listener.Close()
		t.listener = nil
	}
	t.mu.Unlock()

	if t.ovpn != nil && t.ovpn.Process != nil {
		_ = t.ovpn.Process.Kill()
		t.ovpn = nil
	}
	if t.Kind != "custom" {
		t.teardownNetns()
	}

	go func() {
		// 通知延后到 rebind/resync 之后：那两步会把入站改绑到新节点，
		// 提前重建配置会因为入站还指着旧节点名而丢掉路由规则
		m.bringUpPersist(t, false, true)

		// 无论新节点最终是否连通成功（up 还是 failed），
		// 只要节点名发生了变更，都必须执行 rebind 把 s-ui 面板中绑定的 Client 备注和出站规则同步到新节点，
		// 保证分流管理界面与出口池绝对同步，并在用户删除该出口时能够精准级联清理！
		if t.Node.HostName != oldHost {
			if err := m.rebind(oldHost, t); err != nil {
				log.Printf("换节点后同步 s-ui 绑定失败: %v", err)
			}
			return
		}

		if t.Status == "up" {
			// 节点名没变也要重写一次出站：出口 IP 可能变了，
			// 而且上一轮换节点时留下的绑定需要重新指回来。
			if err := m.resync(t); err != nil {
				log.Printf("重连后重写 s-ui 出站失败: %v", err)
			}
		}
	}()
}

// ReviveFailedTunnels 唤醒并重试处于离线状态（failed）的出口隧道（当订阅源更新或刷新时触发）
func (m *Manager) ReviveFailedTunnels() {
	go func() {
		// 稍微等待 1 秒让新节点完全入库落盘
		time.Sleep(1 * time.Second)
		for _, t := range m.Tunnels() {
			if t.Status == "failed" {
				log.Printf("订阅源已完成更新，正在尝试复活离线出口 %d (%s)...", t.Slot, t.Node.HostName)
				m.reconnect(t, t.Node.HostName)
			}
		}
	}()
}
