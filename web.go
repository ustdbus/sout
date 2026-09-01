package main

import (
	"net/http"
)

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sout - s-ui 动态家宽出口插件</title>
<style>
:root{
  color-scheme: dark;
  --bg:#0e1117; --panel:#161b22; --card:#1b212b; --line:#2d333b; --text:#e6edf3;
  --dim:#8b949e; --accent:#58a6ff; --accent-hover:#79b8ff; --ok:#3fb950; --warn:#d29922; --bad:#f85149;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
  font:13px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif,ui-monospace}
header{display:flex;align-items:center;gap:16px;padding:12px 20px;
  border-bottom:1px solid var(--line);background:var(--panel)}
h1{font-size:15px;font-weight:700;margin:0;display:flex;align-items:center;gap:8px}
.badge{background:#238636;color:#fff;font-size:11px;padding:2px 8px;border-radius:12px;font-weight:600}
.spacer{flex:1}
button{font:inherit;color:var(--text);background:#21262d;border:1px solid var(--line);
  border-radius:6px;padding:6px 12px;cursor:pointer;display:inline-flex;
  align-items:center;gap:6px;white-space:nowrap;font-weight:500;transition:all .15s}
button:hover:not(:disabled){border-color:var(--accent);color:var(--accent)}
button:disabled{opacity:.45;cursor:default}
button.primary{background:var(--accent);border-color:var(--accent);color:#0d1117;font-weight:600}
button.primary:hover:not(:disabled){background:var(--accent-hover);border-color:var(--accent-hover);color:#0d1117}
button.success{background:#238636;border-color:#238636;color:#fff;font-weight:600}
button.success:hover:not(:disabled){background:#2ea043;border-color:#2ea043;color:#fff}
button.icon{padding:4px 8px;background:transparent;border-color:transparent;color:var(--dim)}
button.icon:hover:not(:disabled){color:var(--accent);border-color:var(--line);background:var(--panel)}
button.icon.danger:hover:not(:disabled){color:var(--bad);border-color:rgba(248,81,73,.35);background:rgba(248,81,73,.1)}
button.chip-btn{padding:3px 9px;font-size:12px;background:#21262d;border-radius:4px}
svg{width:15px;height:15px;stroke:currentColor;fill:none;stroke-width:2;
  stroke-linecap:round;stroke-linejoin:round;flex:none}
main{padding:20px;max-width:1120px;margin:0 auto}

.section-head{display:flex;align-items:center;gap:12px;margin:28px 0 14px}
.section-head:first-child{margin-top:0}
.section-head h2{font-size:14px;font-weight:700;margin:0;color:var(--text);display:flex;align-items:center;gap:8px}
.section-head .desc{color:var(--dim);font-size:12px}

/* VPN 出口隧道池 */
.exits-box{background:var(--panel);border:1px solid var(--line);border-radius:8px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,.2)}
.exit-row{display:grid;grid-template-columns:14px 140px auto 1fr auto;align-items:center;gap:12px;padding:11px 16px;border-bottom:1px solid #21262d}
.exit-row:last-child{border-bottom:none}
.exit-ip{font-weight:700;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--text)}
.exit-meta{color:var(--dim);font-size:12px;display:flex;align-items:center;gap:8px;min-width:0;overflow:hidden}
.exit-place-text{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:320px;display:inline-block;vertical-align:middle}
.source-tag{font-size:10px;padding:2px 6px;border-radius:3px;font-weight:600;white-space:nowrap;max-width:100px;overflow:hidden;text-overflow:ellipsis;display:inline-block;vertical-align:middle}
.socks-tag{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:#0d1117;border:1px solid var(--line);padding:1px 6px;border-radius:4px;color:var(--dim);font-size:11px}
.metric-tag{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;padding:2px 6px;border-radius:4px;font-weight:600}
.metric-tag.speed{color:#3fb950;border:1px solid rgba(63,185,80,.3);background:rgba(63,185,80,.1)}
.metric-tag.ping{color:#58a6ff;border:1px solid rgba(88,166,255,.3);background:rgba(88,166,255,.1)}

/* s-ui 节点管理 */
.node-card{background:var(--panel);border:1px solid var(--line);border-radius:8px;margin-bottom:16px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,.2)}
.node-header{display:flex;align-items:center;justify-content:space-between;padding:12px 16px;background:var(--card);border-bottom:1px solid var(--line)}
.node-title{display:flex;align-items:center;gap:10px}
.node-title h3{font-size:14px;font-weight:700;margin:0;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.proto-tag{font-size:11px;font-weight:700;padding:2px 7px;border-radius:4px;text-transform:uppercase;background:#30363d;color:#fff}
.proto-tag.VLESS{background:rgba(163,113,247,.2);color:#d2a8ff;border:1px solid rgba(163,113,247,.4)}
.proto-tag.TUIC{background:rgba(240,136,62,.2);color:#ffa657;border:1px solid rgba(240,136,62,.4)}
.proto-tag.VMESS{background:rgba(31,111,235,.2);color:#79c0ff;border:1px solid rgba(31,111,235,.4)}
.proto-tag.TROJAN{background:rgba(86,211,100,.2);color:#7ee787;border:1px solid rgba(86,211,100,.4)}
.base-port{color:var(--dim);font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}

.branch-table{display:flex;flex-direction:column}
.branch-row{display:flex;align-items:center;justify-content:space-between;padding:10px 16px;border-bottom:1px solid #21262d}
.branch-row:last-child{border-bottom:none}
.branch-row:hover{background:rgba(255,255,255,.02)}
.branch-info{display:flex;align-items:center;gap:12px;flex-wrap:wrap}
.branch-name{font-weight:600;font-size:13px;color:var(--text)}
.branch-tag{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:var(--dim);background:#0e1117;padding:1px 6px;border-radius:3px;border:1px solid var(--line)}
.branch-port{color:var(--accent);font-weight:600;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
.branch-acts{display:flex;align-items:center;gap:8px}
.toggle-switch{position:relative;display:inline-block;width:32px;height:18px;flex-shrink:0;vertical-align:middle}
.toggle-switch input{opacity:0;width:0;height:0;position:absolute}
.toggle-slider{position:absolute;cursor:pointer;inset:0;background-color:#30363d;transition:.2s;border-radius:18px;border:1px solid rgba(255,255,255,.08)}
.toggle-slider:before{position:absolute;content:"";height:12px;width:12px;left:2px;bottom:2px;background-color:#8b949e;transition:.2s;border-radius:50%}
.toggle-switch input:checked + .toggle-slider{background-color:#238636;border-color:rgba(63,185,80,.4)}
.toggle-switch input:checked + .toggle-slider:before{transform:translateX(14px);background-color:#fff}
.branch-row.disabled{opacity:.6}
.branch-row.disabled .branch-name{color:var(--dim)}

.dot{width:8px;height:8px;border-radius:50%;background:var(--dim);flex-shrink:0}
.dot.up{background:var(--ok)}
.dot.starting{background:var(--warn);animation:pulse 1.2s ease-in-out infinite}
.dot.failed{background:var(--bad)}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}

.empty{border:1px dashed var(--line);border-radius:8px;padding:32px 20px;text-align:center;color:var(--dim)}

.modal{position:fixed;inset:0;background:rgba(1,4,9,.75);display:none;align-items:center;justify-content:center;z-index:50;padding:20px;backdrop-filter:blur(2px)}
.modal.open{display:flex}
.sheet{background:var(--panel);border:1px solid var(--line);border-radius:8px;width:min(580px,100%);max-height:86vh;display:flex;flex-direction:column;box-shadow:0 8px 24px rgba(0,0,0,.5)}
.sheet .head{display:flex;align-items:center;gap:10px;padding:12px 18px;border-bottom:1px solid var(--line);background:var(--card);border-radius:8px 8px 0 0}
.sheet .head h2{font-size:14px;margin:0;font-weight:700}
.sheet .body{overflow:auto;padding:18px}
.sheet .foot{display:flex;align-items:center;gap:10px;padding:12px 18px;border-top:1px solid var(--line);background:var(--card);border-radius:0 0 8px 8px}

label.f{display:block;margin-bottom:16px}
label.f>span{display:block;color:var(--dim);font-size:12px;margin-bottom:6px;font-weight:500}
select,textarea,input[type=search],input[type=text],input[type=password]{font:inherit;background:#0d1117;border:1px solid var(--line);color:var(--text);border-radius:6px;padding:7px 10px;width:100%;color-scheme:dark}
option{background:#161b22;color:var(--text);padding:8px}
.card-input-box:focus-within{border-color:var(--accent)!important}
.listen-opt-item:hover{background:rgba(88,166,255,.15)!important;color:var(--accent)!important}
.listen-opt-item.selected{background:rgba(88,166,255,.08);color:var(--accent)}
.regions{display:grid;grid-template-columns:repeat(auto-fill,minmax(130px,1fr));gap:6px;max-height:240px;overflow:auto;margin-top:8px}
.rg{border:1px solid var(--line);background:#0d1117;border-radius:6px;padding:7px 9px;cursor:pointer;text-align:left;display:block;width:100%}
.rg:hover{border-color:var(--accent)}
.rg.sel{border-color:var(--accent);background:rgba(88,166,255,.12)}
.rg b{font-weight:600;font-size:12px;display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.rg em{display:block;font-style:normal;color:var(--dim);font-size:11px;margin-top:2px}

.stepper{display:flex;align-items:center;width:fit-content;border:1px solid var(--line);border-radius:6px;overflow:hidden;background:#0d1117}
.stepper button{border:0;border-radius:0;background:transparent;padding:7px 15px;cursor:pointer;color:var(--text);font-size:15px;font-weight:700}
.stepper button:hover{background:rgba(255,255,255,.06)}
.stepper input{width:60px;text-align:center;font:inherit;background:transparent;border:0;border-left:1px solid var(--line);border-right:1px solid var(--line);color:var(--text);padding:7px 0;font-variant-numeric:tabular-nums;border-radius:0}

.toast{position:fixed;left:50%;bottom:28px;transform:translateX(-50%);background:#1f242c;border:1px solid var(--line);border-radius:6px;padding:9px 16px;font-size:13px;z-index:80;opacity:0;pointer-events:none;transition:opacity .18s;box-shadow:0 4px 12px rgba(0,0,0,.4)}
.toast.show{opacity:1}
.toast.bad{border-color:rgba(248,81,73,.5);color:var(--bad)}
.tab-pill{background:#161b22;border:1px solid var(--line);color:var(--dim);font-size:12px;font-weight:600;padding:5px 12px;border-radius:6px;cursor:pointer;transition:all .15s}
.tab-pill:hover{border-color:var(--accent);color:var(--text)}
.tab-pill.active{background:rgba(88,166,255,.15);border-color:var(--accent);color:var(--accent)}
.pool-tag{font-size:10px;padding:2px 6px;border-radius:3px;font-weight:600;display:inline-flex;align-items:center;gap:3px}
.pool-tag.residential{background:rgba(63,185,80,.15);color:#3fb950;border:1px solid rgba(63,185,80,.3)}
.pool-tag.datacenter{background:rgba(88,166,255,.15);color:#58a6ff;border:1px solid rgba(88,166,255,.3)}
</style>
</head>
<body>
<header>
  <h1>
    <svg viewBox="0 0 24 24" style="color:var(--accent)"><path d="M12 2L2 7l10 5 10-5-10-5zM2 17l10 5 10-5M2 12l10 5 10-5"/></svg>
    sout
    <span class="badge" id="backendBadge">s-ui 已连接</span>
  </h1>
  <span class="spacer"></span>
  <button id="exportAll">
    <svg viewBox="0 0 24 24"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="M7 10l5 5 5-5"/><path d="M12 15V3"/></svg>
    导出全部链接
  </button>
  <button class="icon" id="settingsBtn" title="设置">
    <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
  </button>
</header>

<main>
  <!-- 模块一：VPN Gate 出口隧道池（专注管理出站隧道） -->
  <div class="section-head">
    <h2>
      <svg viewBox="0 0 24 24" style="color:var(--ok)"><circle cx="12" cy="12" r="10"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
      出口隧道池 (VPN Gate / 自定义 SOCKS5)
    </h2>
    <span class="desc">在此拉取并运行各公共家宽与自定义 SOCKS5 出口隧道</span>
    <span class="spacer"></span>
    <button class="primary" id="openNewExitModalBtn" style="box-shadow:0 2px 6px rgba(88,166,255,.3)">
      <svg viewBox="0 0 24 24"><path d="M12 5v14"/><path d="M5 12h14"/></svg>
      新建国家/地区出口
    </button>
    <button id="openCustomExitModalBtn">
      <svg viewBox="0 0 24 24"><path d="M12 5v14"/><path d="M5 12h14"/></svg>
      添加自定义出口
    </button>
    <button id="openCustomSourceModalBtn">
      <svg viewBox="0 0 24 24"><path d="M4 19.5v-15A2.5 2.5 0 0 1 6.5 2H20v20H6.5a2.5 2.5 0 0 1-2.5-2.5Z"/><path d="M6 6h10"/><path d="M6 10h10"/></svg>
      SOCKS5 订阅源
    </button>
    <button id="stopAllExitsBtn">全部删除</button>
  </div>

  <div id="exitsContainer"></div>

  <!-- 模块二：s-ui 节点与分流管理（专注从已存在的出站中添加绑定） -->
  <div class="section-head" style="margin-top:36px">
    <h2>
      <svg viewBox="0 0 24 24" style="color:var(--accent)"><rect x="2" y="2" width="20" height="8" rx="2" ry="2"/><rect x="2" y="14" width="20" height="8" rx="2" ry="2"/><line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/></svg>
      s-ui 节点与分流管理
    </h2>
    <span class="desc">直接读取 s-ui 面板中的已有节点，点击每个节点的「+」从上方隧道池中选择出口绑定</span>
    <span class="spacer"></span>
    <button id="refreshNodesBtn">
      <svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>
      刷新节点
    </button>
  </div>

  <div id="nodesContainer"></div>
</main>

<!-- Modal 1: 在隧道池中「新建国家/地区出口」 -->
<div class="modal" id="newExitModal">
  <div class="sheet">
    <div class="head">
      <h2>新建国家/地区出口</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="newExitModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <div style="display:flex;gap:6px;margin-bottom:14px" id="poolTabs">
        <button type="button" class="tab-pill active" data-pool="all">全部节点</button>
        <button type="button" class="tab-pill" data-pool="residential">🏠 家宽池</button>
        <button type="button" class="tab-pill" data-pool="datacenter">🏢 机房池</button>
      </div>
      <label class="f">
        <span>选择目标国家/地区</span>
        <input type="search" id="rgFilter" placeholder="搜索地区，如 JP、美国、日本、韩国...">
        <div class="regions" id="rgList"></div>
      </label>
      <label class="f" style="margin-top:14px">
        <span>同时建立出口数量</span>
        <div class="stepper">
          <button type="button" id="stepDecBtn">−</button>
          <input id="exitCountInput" type="text" value="1" readonly>
          <button type="button" id="stepIncBtn">+</button>
        </div>
      </label>
      <div style="color:var(--dim);font-size:12px" id="newExitSummary">拉取成功后，将在上方出口隧道池中生成对应的 SOCKS5 出口，供下方各节点选择绑定。</div>
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="newExitModal">取消</button>
      <button class="primary" id="startProvisionBtn">拉取并建立出口</button>
    </div>
  </div>
</div>

<!-- Modal 2: 在节点上「添加分流出口」（仅从已有出站中选择） -->
<div class="modal" id="chooseExitModal">
  <div class="sheet">
    <div class="head">
      <h2 id="ceTitle">为节点添加分流出口</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="chooseExitModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <div id="hasExitsBox">
        <label class="f">
          <span>从已有出口隧道中选择</span>
          <select id="existExitSelect"></select>
        </label>
        <div style="color:var(--dim);font-size:12px">
          选择后将为此节点克隆一个独立端口，并将流量分流到该出口，原直连节点继续保留可用。
        </div>
      </div>
      <div id="noExitsBox" style="display:none" class="empty">
        <div style="color:var(--text);font-weight:600;margin-bottom:6px">当前隧道池中暂无可用出口</div>
        <div style="margin-bottom:14px">请先在上方「VPN Gate 出口隧道池」中拉取出口隧道。</div>
        <button class="primary" id="goToNewExitBtn">
          <svg viewBox="0 0 24 24"><path d="M12 5v14"/><path d="M5 12h14"/></svg>
          去新建出口
        </button>
      </div>
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="chooseExitModal">取消</button>
      <button class="primary" id="confirmChooseExitBtn">确定添加</button>
    </div>
  </div>
</div>

<!-- Modal 2.5: 修改节点配置 -->
<div class="modal" id="editNodeModal">
  <div class="sheet" style="max-width:580px;overflow:visible">
    <div class="head">
      <h2 id="editNodeTitle">修改节点配置</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="editNodeModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body" style="padding:14px 18px 20px;overflow:visible">
      <!-- 上半部分：客户端 -->
      <div style="margin-bottom:14px">
        <div style="text-align:center;font-weight:600;font-size:13px;color:var(--text);margin-bottom:10px;position:relative">
          <span style="background:var(--panel);padding:0 12px;position:relative;z-index:1">客户端</span>
          <div style="position:absolute;top:50%;left:0;right:0;height:1px;background:var(--line)"></div>
        </div>
        <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:6px">
          <span style="font-size:11px;color:var(--dim)">若该节点为CDN/argo节点，此处可填写优选域名/ip。</span>
          <span style="font-size:12px;color:var(--dim);font-weight:500">连接</span>
        </div>

        <div class="card-input-box" style="background:#0d1117;border:1px solid var(--line);border-radius:6px;padding:8px 12px;transition:border-color .15s">
          <textarea id="nodeClientAddrsText" rows="4" spellcheck="false"
            placeholder="66.66.55.44:443&#10;example.com:443#HK1-线路 246 Mbps&#10;88.88.77.66:443&#10;99.99.88.77"
            style="width:100%;border:none;background:transparent;color:var(--text);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;line-height:1.6;resize:vertical;min-height:90px;padding:0;outline:none"></textarea>
        </div>
        <div style="color:var(--dim);font-size:11px;line-height:1.4;margin-top:6px">
          <div style="margin-bottom:2px">连接地址与端口 (一行一个，支持批量直接粘贴)</div>
          格式示例：<code style="color:var(--accent);font-family:ui-monospace,SFMono-Regular,Menlo,monospace">66.66.55.44</code>、<code style="color:var(--accent);font-family:ui-monospace,SFMono-Regular,Menlo,monospace">66.66.55.44:443</code> 或 <code style="color:var(--accent);font-family:ui-monospace,SFMono-Regular,Menlo,monospace">66.66.55.44:443#备注</code>（如未写端口将自动使用下方的监听端口）
        </div>
      </div>

      <div style="border-top:1px solid var(--line);margin-bottom:14px"></div>

      <!-- 下半部分：服务端（参考图二） -->
      <div>
        <div style="text-align:center;font-weight:600;font-size:13px;color:var(--text);margin-bottom:10px;position:relative">
          <span style="background:var(--panel);padding:0 12px;position:relative;z-index:1">服务端</span>
          <div style="position:absolute;top:50%;left:0;right:0;height:1px;background:var(--line)"></div>
        </div>
        <div style="display:flex;align-items:center;justify-content:flex-end;margin-bottom:6px">
          <span style="font-size:12px;color:var(--dim);font-weight:500">监听</span>
        </div>
        <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
          <div class="card-input-box" id="nodeListenAddrBox" style="position:relative;background:#0d1117;border:1px solid var(--line);border-radius:6px;padding:8px 12px;cursor:pointer;user-select:none;transition:border-color .15s">
            <span style="display:block;color:var(--dim);font-size:11px;margin-bottom:4px">地址</span>
            <div style="display:flex;align-items:center;justify-content:space-between">
              <span id="nodeListenAddrDisplay" style="font-weight:600;font-size:13px;color:var(--text)">::</span>
              <svg id="nodeListenAddrArrow" viewBox="0 0 24 24" style="width:14px;height:14px;color:var(--dim);transition:transform .2s"><path d="m6 9 6 6 6-6"/></svg>
            </div>
            <input type="hidden" id="nodeListenAddr" value="::">
            
            <div id="nodeListenAddrMenu" style="display:none;position:absolute;left:-1px;right:-1px;top:calc(100% + 4px);background:#161b22;border:1px solid var(--line);border-radius:6px;box-shadow:0 12px 28px rgba(0,0,0,.85);z-index:999;overflow:hidden">
              <div class="listen-opt-item" data-val="::" style="padding:9px 12px;font-size:13px;cursor:pointer;display:flex;align-items:center;justify-content:space-between;color:var(--text);transition:background .15s">
                <span style="font-weight:600">::</span>
                <span style="color:var(--dim);font-size:11px">（ipv4/6外网通信）</span>
              </div>
              <div class="listen-opt-item" data-val="127.0.0.1" style="padding:9px 12px;font-size:13px;cursor:pointer;display:flex;align-items:center;justify-content:space-between;color:var(--text);border-top:1px solid var(--line);transition:background .15s">
                <span style="font-weight:600">127.0.0.1</span>
                <span style="color:var(--dim);font-size:11px">（隧道内网反代）</span>
              </div>
            </div>
          </div>
          <div class="card-input-box" style="background:#0d1117;border:1px solid var(--line);border-radius:6px;padding:8px 12px;transition:border-color .15s">
            <span style="display:block;color:var(--dim);font-size:11px;margin-bottom:4px">端口</span>
            <input type="text" id="nodeListenPort" inputmode="numeric" style="border:none;background:transparent;padding:0;font-weight:600;font-size:13px;width:100%" placeholder="26060">
          </div>
        </div>
      </div>
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="editNodeModal">取消</button>
      <button class="primary" id="saveEditNodeBtn">保存修改</button>
    </div>
  </div>
</div>

<!-- Modal 3: SOCKS5 凭据与端口设置 -->
<div class="modal" id="credModal">
  <div class="sheet">
    <div class="head">
      <h2>SOCKS5 出口凭据与配置</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="credModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <div style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:#0d1117;padding:10px;border-radius:6px;border:1px solid var(--line);word-break:break-all;margin-bottom:12px" id="credURL"></div>
      <label class="f"><span>用户名</span><input id="crUser" type="text" placeholder="输入用户名"></label>
      <label class="f"><span>密码</span><input id="crPass" type="text" placeholder="输入密码"></label>
      <label class="f"><span>SOCKS5 端口</span><input id="crPort" type="text" inputmode="numeric" placeholder="如 43440"></label>
    </div>
    <div class="foot">
      <button id="copyCredBtn">复制连接串</button>
      <span class="spacer"></span>
      <button data-close="credModal">取消</button>
      <button class="primary" id="saveCredBtn">保存修改</button>
    </div>
  </div>
</div>

<!-- Modal 4: 导出全部链接 -->
<div class="modal" id="exportModal">
  <div class="sheet">
    <div class="head">
      <h2>全部节点链接</h2>
      <span class="spacer"></span>
      <button class="primary" id="copyAllLinksBtn">全部复制</button>
      <button class="icon" data-close="exportModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <textarea id="allLinksBox" readonly spellcheck="false"></textarea>
    </div>
  </div>
</div>

<!-- Modal 5: 设置 -->
<div class="modal" id="settingsModal">
  <div class="sheet">
    <div class="head">
      <h2>设置</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="settingsModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <label class="f"><span>访问口令</span>
        <input id="setPw" type="password" placeholder="留空则不修改"></label>
      <label class="f"><span>访问路径</span>
        <input id="setPath" type="text" placeholder="留空去掉前缀"></label>
      <label class="f"><span>管理端口</span>
        <input id="setPort" type="text" inputmode="numeric"></label>
      <label class="f"><span>监听地址</span>
        <select id="setListen">
          <option value="0.0.0.0">0.0.0.0 (公网直接访问)</option>
          <option value="127.0.0.1">127.0.0.1 (仅内网，用于反代)</option>
        </select>
      </label>
      <label class="f"><span>面板 URL (如 https://example.com 或 https://example.com/，不带路径)</span>
        <input id="setPanelUrl" type="text" placeholder="如 https://example.com 或 https://example.com/"></label>

      <div style="border-top:1px solid var(--line);margin-top:14px;padding-top:14px">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;font-weight:600;font-size:13px;user-select:none">
          <input type="checkbox" id="setSSLEnabled" style="width:16px;height:16px;cursor:pointer">
          <span>开启 SSL (HTTPS 安全连接)</span>
        </label>
        <div id="sslConfigBox" style="display:none;margin-top:10px">
          <label class="f"><span>域名</span>
            <input id="setSSLDomain" type="text" placeholder="如 sout.example.com"></label>
          <label class="f"><span>SSL 密钥 (Key) 路径</span>
            <input id="setSSLKey" type="text" placeholder="如 /etc/s-ui/cert/private.key 或 /root/cert/key.pem"></label>
          <label class="f"><span>SSL 证书 (cert) 路径</span>
            <input id="setSSLCert" type="text" placeholder="如 /etc/s-ui/cert/fullchain.pem 或 /root/cert/cert.crt"></label>
        </div>
      </div>

      <div style="border-top:1px solid var(--line);margin-top:14px;padding-top:14px">
        <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">
          <div>
            <div style="font-weight:600;font-size:13px">版本与系统更新</div>
            <div id="updateCurVer" style="font-size:12px;color:var(--dim)">当前版本: 点击右侧按钮检查</div>
          </div>
          <button type="button" id="checkUpdateBtn" style="font-size:12px;padding:5px 10px">检查更新</button>
        </div>
        <div id="updateInfoBox" style="display:none;background:#12151a;border:1px solid var(--line);border-radius:4px;padding:10px;margin-top:8px;font-size:12px"></div>
      </div>
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="settingsModal">取消</button>
      <button class="primary" id="saveSettingsBtn">保存设置</button>
    </div>
  </div>
</div>

<!-- Modal 6: 添加自定义 SOCKS5 出口 -->
<div class="modal" id="customExitModal">
  <div class="sheet">
    <div class="head">
      <h2>添加自定义 SOCKS5 出口</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="customExitModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <label class="f">
        <span>快捷导入 (socks5:// 链接或 host:port:user:pass)</span>
        <input type="text" id="csRawUrl" placeholder="如 socks5://user:pass@1.2.3.4:1080#香港家宽">
      </label>
      <div style="display:grid;grid-template-columns:2fr 1fr;gap:12px;margin-top:10px">
        <label class="f"><span>服务器地址 (IP 或域名)</span><input type="text" id="csHost" placeholder="如 123.45.67.89"></label>
        <label class="f"><span>端口</span><input type="text" id="csPort" inputmode="numeric" placeholder="如 1080"></label>
      </div>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:10px">
        <label class="f"><span>用户名 (可选)</span><input type="text" id="csUser" placeholder="留空为无认证"></label>
        <label class="f"><span>密码 (可选)</span><input type="password" id="csPass" placeholder="留空为无认证"></label>
      </div>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:10px">
        <label class="f"><span>地区/国家标识</span><input type="text" id="csCountry" placeholder="如 香港 / 日本 / 美国 / 自定义"></label>
        <label class="f"><span>节点备注名</span><input type="text" id="csRemark" placeholder="如 私人住宅S5"></label>
      </div>
      <div id="csTestResult" style="min-height:20px;font-size:12px;margin-top:8px"></div>
    </div>
    <div class="foot">
      <button id="csTestBtn">测试连通性</button>
      <span class="spacer"></span>
      <button data-close="customExitModal">取消</button>
      <button class="primary" id="saveCustomExitBtn">添加并启用</button>
    </div>
  </div>
</div>

<!-- Modal 7: SOCKS5 订阅源管理 -->
<div class="modal" id="customSourceModal">
  <div class="sheet" style="max-width:580px">
    <div class="head">
      <h2>SOCKS5 订阅源管理</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="customSourceModal"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <div style="background:#181c23;border:1px solid var(--line);border-radius:6px;padding:12px;margin-bottom:16px">
        <div style="font-weight:600;margin-bottom:8px;font-size:13px">添加新源</div>
        <div style="display:grid;grid-template-columns:1fr 2fr auto;gap:8px">
          <input type="text" id="srcName" placeholder="源名称 (如 我的代理池)">
          <input type="text" id="srcURL" placeholder="订阅/API 链接 (http/https)">
          <button class="primary" id="addSourceBtn">添加</button>
        </div>
      </div>
      <div style="font-weight:600;font-size:13px;margin-bottom:8px">已添加的源列表</div>
      <div id="sourcesContainer" style="display:flex;flex-direction:column;gap:8px"></div>
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="customSourceModal">关闭</button>
    </div>
  </div>
</div>

<div class="toast" id="toast"></div>

<script>
const $ = s => document.querySelector(s);
const ICON = {
  copy:'<svg viewBox="0 0 24 24"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>',
  plus:'<svg viewBox="0 0 24 24"><path d="M12 5v14"/><path d="M5 12h14"/></svg>',
  trash:'<svg viewBox="0 0 24 24"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>',
  redo:'<svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>',
  lock:'<svg viewBox="0 0 24 24"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>',
  edit:'<svg viewBox="0 0 24 24"><path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z"/><path d="m15 5 4 4"/></svg>'
};

async function api(path, opts){
  const r = await fetch(path.replace(/^\//, ''), opts);
  const d = await r.json().catch(()=>({}));
  if(!r.ok) throw new Error(d.error || ('HTTP '+r.status));
  return d;
}
function esc(s){ return String(s == null ? '' : s).replace(/[&<>"']/g, c =>
  ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }

let toastTimer;
function toast(msg, bad){
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast show' + (bad ? ' bad' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast'; }, 2400);
}

async function copy(text){
  if(navigator.clipboard && window.isSecureContext){
    try{ await navigator.clipboard.writeText(text); toast('已复制链接'); return; }
    catch(e){}
  }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.cssText = 'position:fixed;top:0;left:0;width:1px;height:1px;opacity:0;padding:0;border:0';
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  let ok = false;
  try{ ok = document.execCommand('copy'); }catch(e){}
  ta.remove();
  toast(ok ? '已复制链接' : '复制失败，请手动选中', !ok);
}

function openModal(id){ $('#' + id).classList.add('open'); }
function closeModal(id){ $('#' + id).classList.remove('open'); }
document.addEventListener('click', e => {
  const c = e.target.closest('[data-close]');
  if(c) closeModal(c.dataset.close);
});

let viewData = {nodes:[], exits:[], public_ip:''};
let targetNodeID = 0;
let targetNodeName = '';
let selectedRegion = 'JP';
let regionList = [];
let curCred = null;

// ---- 渲染出口隧道池 ----
function renderExits(){
  const box = $('#exitsContainer');
  const exits = viewData.exits || [];
  if(!exits.length){
    box.innerHTML = '<div class="empty">出口隧道池中暂无运行的隧道，请点击右上角「+ 新建国家/地区出口」拉取</div>';
    return;
  }

  box.innerHTML = '<div class="exits-box">' + exits.map(e => {
    const label = e.exit_ip || (e.status === 'starting' ? '连接中…' : '—');
    const place = (e.country && e.country.toUpperCase() !== (e.region||'').toUpperCase())
      ? (e.region + ' ' + e.country) : (e.region || '—');
    const srcName = e.source_name || (e.kind === 'custom' ? '自定义' : 'VPN Gate');
    const kindBadge = e.kind === 'custom'
      ? '<span class="source-tag" style="background:#1f6feb;color:#fff" title="' + esc(srcName) + '">' + esc(srcName) + '</span>'
      : '<span class="source-tag" style="background:#238636;color:#fff">VPN Gate</span>';
    const poolBadge = e.ip_type === 'datacenter'
      ? '<span class="pool-tag datacenter">🏢 机房</span>'
      : '<span class="pool-tag residential">🏠 家宽</span>';
    const pingBadge = e.ping > 0 ? ('<span class="metric-tag ping" title="延迟">' + e.ping + ' ms</span>') : '';
    const speedBadge = e.speed_mbps > 0 ? ('<span class="metric-tag speed" title="带宽">' + e.speed_mbps.toFixed(0) + ' Mbps</span>') : '';
    const swapBtn = '<button class="icon" data-swap="' + e.slot + '" title="换一个同地区/同源节点">' + ICON.redo + '</button>';
    const hostClean = (e.host || '').replace(/^(custom-|cs-)/, '');
    const fullMeta = place + (e.isp ? ' (' + e.isp + ')' : '') + ' · ' + (e.host || '');
    return '<div class="exit-row">'
      + '<span class="dot ' + e.status + '" title="' + e.status + '"></span>'
      + '<span class="exit-ip">' + esc(label) + '</span>'
      + '<div style="display:flex;gap:4px;align-items:center;flex-shrink:0">' + kindBadge + poolBadge + '</div>'
      + '<div class="exit-meta">'
      +   '<span class="exit-place-text" title="' + esc(fullMeta) + '">' + esc(place) + ' · ' + esc(hostClean) + '</span>'
      +   pingBadge
      +   speedBadge
      +   '<button class="chip-btn" data-cred="' + e.slot + '" title="查看/修改 SOCKS5 凭据">' + ICON.lock + ' SOCKS5 :' + e.port + '</button>'
      + '</div>'
      + '<div class="branch-acts">'
      +   swapBtn
      +   '<button class="icon danger" data-stop="' + e.slot + '" title="停止此出口">' + ICON.trash + '</button>'
      + '</div>'
      + '</div>';
  }).join('') + '</div>';
}

// ---- 渲染 s-ui 节点列表 ----
function renderNodes(){
  const box = $('#nodesContainer');
  const nodes = viewData.nodes || [];
  if(!nodes.length){
    box.innerHTML = '<div class="empty">s-ui 面板中暂无入站节点，请在 s-ui 中创建入站后刷新</div>';
    return;
  }

  box.innerHTML = nodes.map(n => {
    const branches = n.branches || [];
    return '<div class="node-card">'
      + '<div class="node-header">'
      +   '<div class="node-title">'
      +     '<span class="proto-tag ' + esc(n.protocol) + '">' + esc(n.protocol) + '</span>'
      +     '<h3>' + esc(n.name) + '</h3>'
      +     '<span class="base-port">默认端口 :' + n.port + '</span>'
      +   '</div>'
      +   '<div style="display:flex;gap:8px;align-items:center">'
      +     '<button class="primary" data-add-node="' + n.base_id + '" data-node-name="' + esc(n.name) + '" style="box-shadow:0 2px 6px rgba(88,166,255,.3)">'
      +       ICON.plus + ' 添加出口分流'
      +     '</button>'
      +     '<button data-edit-node="' + n.base_id + '" data-node-name="' + esc(n.name) + '" title="修改节点配置">'
      +       ICON.edit + ' 修改节点'
      +     '</button>'
      +   '</div>'
      + '</div>'
      + '<div class="branch-table">'
      +   branches.map(b => {
            const isEn = b.enabled !== false;
            const hasLink = isEn && b.links && b.links.length;
            const firstLink = hasLink ? b.links[0] : '';
            return '<div class="branch-row' + (isEn ? '' : ' disabled') + '">'
              + '<div class="branch-info">'
              +   '<span class="dot ' + (isEn ? 'up' : '') + '" title="' + (isEn ? '已就绪' : '已停用') + '"></span>'
              +   '<span class="branch-name">' + esc(b.bound_label) + '</span>'
              +   '<span class="branch-tag">' + esc(b.tag) + '</span>'
              +   '<span class="branch-port">:' + b.port + '</span>'
              + '</div>'
              + '<div class="branch-acts">'
              +   '<label class="toggle-switch" title="' + (isEn ? '已启用 - 点击停用此节点链接生成' : '已停用 - 点击启用此节点链接生成') + '">'
              +     '<input type="checkbox" class="branch-toggle" data-tag="' + esc(b.tag) + '" data-port="' + b.port + '"' + (isEn ? ' checked' : '') + '>'
              +     '<span class="toggle-slider"></span>'
              +   '</label>'
              +   (hasLink ? '<button class="chip-btn" data-copy="' + esc(firstLink) + '">' + ICON.copy + ' 复制链接</button>' : '')
              +   (!b.is_base ? '<button class="icon danger" data-delone="' + b.id + '" data-name="' + esc(b.tag) + '" title="删除此出口分流">' + ICON.trash + '</button>' : '<span style="font-size:11px;color:var(--dim);margin-right:6px">基础直连</span>')
              + '</div>'
              + '</div>';
          }).join('')
      + '</div>'
      + '</div>';
  }).join('');
}

async function poll(){
  try{
    viewData = await api('/api/exits');
    renderExits();
    renderNodes();
  }catch(e){}
}

let editNodeTargetID = 0;

function setListenAddr(val){
  const realVal = (val === '127.0.0.1') ? '127.0.0.1' : '::';
  $('#nodeListenAddr').value = realVal;
  $('#nodeListenAddrDisplay').textContent = realVal;
  document.querySelectorAll('.listen-opt-item').forEach(el => {
    el.classList.toggle('selected', el.dataset.val === realVal);
  });
  $('#nodeListenAddrMenu').style.display = 'none';
  $('#nodeListenAddrArrow').style.transform = 'rotate(0deg)';
}

$('#nodeListenAddrBox').onclick = e => {
  const opt = e.target.closest('.listen-opt-item');
  if(opt){
    setListenAddr(opt.dataset.val);
    return;
  }
  const menu = $('#nodeListenAddrMenu');
  const arrow = $('#nodeListenAddrArrow');
  const open = menu.style.display === 'block';
  menu.style.display = open ? 'none' : 'block';
  arrow.style.transform = open ? 'rotate(0deg)' : 'rotate(180deg)';
};

document.addEventListener('click', e => {
  if(!e.target.closest('#nodeListenAddrBox')){
    const m = $('#nodeListenAddrMenu');
    if(m) m.style.display = 'none';
    const a = $('#nodeListenAddrArrow');
    if(a) a.style.transform = 'rotate(0deg)';
  }
});

// ---- 事件绑定 ----
document.addEventListener('click', async e => {
  // 点击节点的「修改节点」
  const editBtn = e.target.closest('[data-edit-node]');
  if(editBtn){
    const id = Number(editBtn.dataset.editNode);
    const name = editBtn.dataset.nodeName || '';
    editNodeTargetID = id;
    $('#editNodeTitle').textContent = '修改节点 - ' + name;
    $('#nodeClientAddrsText').value = '';
    $('#nodeListenPort').value = '';
    setListenAddr('::');
    openModal('editNodeModal');

    try {
      const detail = await api('/api/node/detail?id=' + id);
      if(detail){
        setListenAddr(detail.listen);
        $('#nodeListenPort').value = detail.listen_port || '';
        if(detail.addrs && detail.addrs.length){
          const lines = detail.addrs.map(a => {
            let s = a.server || '';
            if(a.server_port && a.server_port !== detail.listen_port) {
              s += ':' + a.server_port;
            } else if(a.server_port) {
              s += ':' + a.server_port;
            }
            if(a.remark) s += '#' + a.remark;
            return s;
          }).filter(Boolean);
          $('#nodeClientAddrsText').value = lines.join('\n');
        } else {
          $('#nodeClientAddrsText').value = '';
        }
      }
    } catch(err) {
      toast(err.message, true);
    }
  }

  // 点击节点的「+ 添加出口分流」-> 仅从已有出站中选择
  const addBtn = e.target.closest('[data-add-node]');
  if(addBtn){
    targetNodeID = Number(addBtn.dataset.addNode);
    targetNodeName = addBtn.dataset.nodeName;
    $('#ceTitle').textContent = '为「' + targetNodeName + '」选择分流出口';

    const upExits = (viewData.exits || []).filter(x => x.status === 'up');
    const sel = $('#existExitSelect');
    if(!upExits.length){
      $('#hasExitsBox').style.display = 'none';
      $('#noExitsBox').style.display = 'block';
      $('#confirmChooseExitBtn').disabled = true;
    } else {
      $('#hasExitsBox').style.display = 'block';
      $('#noExitsBox').style.display = 'none';
      $('#confirmChooseExitBtn').disabled = false;
      sel.innerHTML = upExits.map(x =>
        '<option value="' + esc(x.host) + '">' + esc((x.country || x.region) + ' (' + (x.exit_ip || x.host) + ' · SOCKS5:' + x.port + ')') + '</option>').join('');
    }
    openModal('chooseExitModal');
  }

  // 复制链接
  const cp = e.target.closest('[data-copy]');
  if(cp) copy(cp.dataset.copy);
});

// 分支启用/停用开关
document.addEventListener('change', async e => {
  const toggle = e.target.closest('.branch-toggle');
  if(toggle){
    const tag = toggle.dataset.tag;
    const port = parseInt(toggle.dataset.port, 10);
    const enabled = toggle.checked;
    toggle.disabled = true;
    try {
      await api('/api/branch/toggle', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ tag, port, enabled })
      });
      toast(enabled ? '已启用该节点链接' : '已停用该节点链接');
      poll();
    } catch(err) {
      toast(err.message, true);
      toggle.checked = !enabled;
      toggle.disabled = false;
    }
  }
});

document.addEventListener('click', async e => {

  // 删除分流节点
  const del = e.target.closest('[data-delone]');
  if(del){
    if(!confirm('删除分流节点「' + del.dataset.name + '」？')) return;
    del.disabled = true;
    try{
      await api('/api/xui/delete?ids=' + del.dataset.delone, {method:'POST'});
      toast('已删除');
      poll();
    }catch(err){ toast(err.message, true); }
  }

  // 停止出口
  const stop = e.target.closest('[data-stop]');
  if(stop){
    stop.disabled = true;
    try{ await api('/api/stop?slot=' + stop.dataset.stop, {method:'POST'}); toast('已停止出口'); poll(); }
    catch(err){ toast(err.message, true); }
  }

  // 换出口节点
  const swap = e.target.closest('[data-swap]');
  if(swap){
    swap.disabled = true;
    try{ await api('/api/swap?slot=' + swap.dataset.swap, {method:'POST'}); toast('正在换节点'); poll(); }
    catch(err){ toast(err.message, true); }
  }

  // 查看 SOCKS5 凭据
  const cred = e.target.closest('[data-cred]');
  if(cred){
    const slot = Number(cred.dataset.cred);
    const ex = (viewData.exits || []).find(x => x.slot === slot);
    if(ex){
      curCred = ex;
      const host = viewData.public_ip || location.hostname;
      const u = ex.socks_user ? (ex.socks_user + ':' + ex.socks_pass + '@') : '';
      const url = 'socks5://' + u + host + ':' + ex.port;
      $('#credURL').textContent = url;
      $('#crUser').value = ex.socks_user || '';
      $('#crPass').value = ex.socks_pass || '';
      $('#crPort').value = ex.port || '';
      openModal('credModal');
    }
  }

  // 地区选择
  const rg = e.target.closest('[data-rgcode]');
  if(rg){
    selectedRegion = rg.dataset.rgcode;
    renderRegionList();
  }
});

// 确认从已有出口添加分流
$('#confirmChooseExitBtn').onclick = async e => {
  const host = $('#existExitSelect').value;
  if(!host){ toast('请选择一个出口', true); return; }
  e.target.disabled = true;
  try{
    await api('/api/node/add_branch?template_id=' + targetNodeID + '&host=' + encodeURIComponent(host), {method:'POST'});
    toast('已成功为「' + targetNodeName + '」添加分流出口');
    closeModal('chooseExitModal');
    poll();
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

// 保存修改节点配置
$('#saveEditNodeBtn').onclick = async () => {
  if(!editNodeTargetID) return;
  const rawText = $('#nodeClientAddrsText').value.trim();
  const listenAddr = $('#nodeListenAddr').value;
  const listenPortStr = $('#nodeListenPort').value.trim();

  const listenPort = parseInt(listenPortStr, 10);
  if(!listenPort || listenPort < 1 || listenPort > 65535){
    toast('请输入有效的新服务端监听端口 (1-65535)', true);
    return;
  }

  const rawLines = rawText ? rawText.split('\n').map(l => l.trim()).filter(Boolean) : [];
  const parsedAddrs = [];

  for(let i = 0; i < rawLines.length; i++){
    const line = rawLines[i];
    let remark = '';
    let hostPort = line;
    const hashIdx = line.indexOf('#');
    if(hashIdx !== -1){
      hostPort = line.substring(0, hashIdx).trim();
      remark = line.substring(hashIdx + 1).trim();
    }

    let server = hostPort;
    let port = listenPort;

    if(hostPort.startsWith('[') && hostPort.includes(']')){
      const endBracket = hostPort.indexOf(']');
      server = hostPort.substring(0, endBracket + 1);
      const colon = hostPort.indexOf(':', endBracket);
      if(colon !== -1){
        const pStr = hostPort.substring(colon + 1).trim();
        const p = parseInt(pStr, 10);
        if(!isNaN(p) && p > 0 && p <= 65535) port = p;
      }
    } else {
      const colonIdx = hostPort.lastIndexOf(':');
      if(colonIdx !== -1 && colonIdx === hostPort.indexOf(':')){
        const pStr = hostPort.substring(colonIdx + 1).trim();
        const p = parseInt(pStr, 10);
        if(!isNaN(p) && p > 0 && p <= 65535){
          server = hostPort.substring(0, colonIdx).trim();
          port = p;
        }
      }
    }

    if(!server){
      toast('第 ' + (i + 1) + ' 行地址为空', true);
      return;
    }
    parsedAddrs.push({ server, server_port: port, remark });
  }

  const btn = $('#saveEditNodeBtn');
  btn.disabled = true;
  btn.textContent = '保存中…';

  try {
    await api('/api/node/update', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        id: editNodeTargetID,
        listen: listenAddr,
        listen_port: listenPort,
        addrs: parsedAddrs
      })
    });
    toast('节点配置已保存并生效');
    closeModal('editNodeModal');
    poll();
  } catch(err) {
    toast(err.message, true);
  } finally {
    btn.disabled = false;
    btn.textContent = '保存修改';
  }
};

// 从无出口提示跳转到新建出口
$('#goToNewExitBtn').onclick = () => {
  closeModal('chooseExitModal');
  $('#openNewExitModalBtn').click();
};

// ---- 新建国家/地区出口相关 ----
let exitCount = 1;
$('#stepDecBtn').onclick = () => {
  if(exitCount > 1){
    exitCount--;
    $('#exitCountInput').value = exitCount;
  }
};
$('#stepIncBtn').onclick = () => {
  if(exitCount < 20){
    exitCount++;
    $('#exitCountInput').value = exitCount;
  }
};

let currentPoolType = 'all';

$('#poolTabs').onclick = async e => {
  const btn = e.target.closest('[data-pool]');
  if(btn){
    document.querySelectorAll('#poolTabs .tab-pill').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    currentPoolType = btn.dataset.pool;
    try{
      regionList = await api('/api/regions?type=' + encodeURIComponent(currentPoolType)) || [];
    }catch(err){ regionList = []; }
    renderRegionList();
  }
};

$('#openNewExitModalBtn').onclick = async () => {
  exitCount = 1;
  $('#exitCountInput').value = exitCount;
  try{
    regionList = await api('/api/regions?type=' + encodeURIComponent(currentPoolType)) || [];
  }catch(e){}
  renderRegionList();
  openModal('newExitModal');
};

function renderRegionList(){
  const kw = ($('#rgFilter').value || '').trim().toLowerCase();
  const filtered = regionList.filter(r => !kw || r.code.toLowerCase().includes(kw) || r.name.toLowerCase().includes(kw));
  $('#rgList').innerHTML = filtered.map(r => {
    let title = '';
    if(r.code === 'ALL'){
      title = '🌐 ' + esc(r.name);
    } else if(r.code.startsWith('SRC:')){
      title = '📦 ' + esc(r.name);
    } else {
      title = esc(r.code) + ' ' + esc(r.name);
    }
    return '<button class="rg' + (selectedRegion === r.code ? ' sel' : '') + '" data-rgcode="' + esc(r.code) + '" title="' + esc(r.name) + '">'
      + '<b class="rg-title">' + title + '</b>'
      + '<em>' + r.available + ' 个可用 · ' + r.best_speed_mbps.toFixed(0) + ' Mbps</em>'
      + '</button>';
  }).join('');
}
$('#rgFilter').oninput = renderRegionList;

$('#startProvisionBtn').onclick = async e => {
  if(!selectedRegion){ toast('请选择目标地区', true); return; }
  const count = Math.max(1, Math.min(20, parseInt($('#exitCountInput').value, 10) || 1));
  e.target.disabled = true;
  let label = selectedRegion;
  if(selectedRegion === 'ALL'){
    label = '全球最高速';
  } else if(selectedRegion.startsWith('SRC:')){
    const match = regionList.find(r => r.code === selectedRegion);
    label = match ? match.name : '自定义源';
  }
  try{
    await api('/api/provision?count=' + count + '&region=' + encodeURIComponent(selectedRegion) + '&type=' + encodeURIComponent(currentPoolType), {method:'POST'});
    toast('正在拉取 ' + count + ' 条「' + label + '」出口隧道...');
    closeModal('newExitModal');
    poll();
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

$('#refreshNodesBtn').onclick = () => { toast('已刷新'); poll(); };

const updateCredPreview = () => {
  if(!curCred) return;
  const host = viewData.public_ip || location.hostname;
  const u = $('#crUser').value.trim();
  const p = $('#crPass').value.trim();
  const pt = $('#crPort').value.trim() || curCred.port;
  const auth = (u || p) ? (u + ':' + p + '@') : '';
  $('#credURL').textContent = 'socks5://' + auth + host + ':' + pt;
};
$('#crUser').oninput = updateCredPreview;
$('#crPass').oninput = updateCredPreview;
$('#crPort').oninput = updateCredPreview;

$('#copyCredBtn').onclick = () => copy($('#credURL').textContent);

$('#saveCredBtn').onclick = async e => {
  if(!curCred) return;
  e.target.disabled = true;
  const user = $('#crUser').value.trim();
  const pass = $('#crPass').value.trim();
  const port = parseInt($('#crPort').value.trim(), 10);
  if(!user || !pass){ toast('用户名和密码不能为空', true); e.target.disabled = false; return; }
  if(!port || port < 1 || port > 65535){ toast('端口不合法 (1-65535)', true); e.target.disabled = false; return; }
  
  try{
    await api('/api/cred', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        slot: curCred.slot,
        user: user,
        pass: pass,
        port: port,
      }),
    });
    toast('SOCKS5 凭据与端口已保存');
    closeModal('credModal');
    poll();
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

$('#exportAll').onclick = async () => {
  try {
    const s = await api('/api/settings');
    const subURL = new URL('sub=' + encodeURIComponent(s.password || ''), location.href);
    window.open(subURL, '_blank');
    closeModal('exportModal');
  } catch(e) { toast(e.message, true); }
};
$('#copyAllLinksBtn').onclick = () => copy($('#allLinksBox').value);

$('#stopAllExitsBtn').onclick = async () => {
  if(!confirm('确定删除全部出口？')) return;
  for(const x of (viewData.exits || [])){
    try{ await api('/api/stop?slot=' + x.slot, {method:'POST'}); }catch(e){}
  }
  toast('已删除全部出口');
  poll();
};

$('#setSSLEnabled').onchange = () => {
  $('#sslConfigBox').style.display = $('#setSSLEnabled').checked ? 'block' : 'none';
};

$('#settingsBtn').onclick = async () => {
  openModal('settingsModal');
  try{
    const s = await api('/api/settings');
    $('#setPath').value = (s.base_path || '').replace(/^\//, '');
    $('#setPort').value = s.port || '';
    $('#setListen').value = (s.listen_addr === '127.0.0.1') ? '127.0.0.1' : '0.0.0.0';
    $('#setPanelUrl').value = s.panel_url || '';
    $('#setSSLEnabled').checked = !!s.ssl_enabled;
    $('#sslConfigBox').style.display = s.ssl_enabled ? 'block' : 'none';
    $('#setSSLDomain').value = s.ssl_domain || '';
    $('#setSSLKey').value = s.ssl_key || '';
    $('#setSSLCert').value = s.ssl_cert || '';
  }catch(e){}
};

$('#saveSettingsBtn').onclick = async e => {
  e.target.disabled = true;
  const body = {};
  const pw = $('#setPw').value.trim();
  if(pw) body.password = pw;
  body.base_path = $('#setPath').value.trim();
  const port = parseInt($('#setPort').value.trim(), 10);
  if(port) body.port = port;
  body.listen_addr = $('#setListen').value;
  body.panel_url = $('#setPanelUrl').value.trim();
  body.ssl_enabled = $('#setSSLEnabled').checked;
  body.ssl_domain = $('#setSSLDomain').value.trim();
  body.ssl_key = $('#setSSLKey').value.trim();
  body.ssl_cert = $('#setSSLCert').value.trim();
  try{
    await api('/api/settings', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify(body),
    });
    toast('设置已保存');
    closeModal('settingsModal');
    if(body.ssl_enabled && location.protocol === 'http:'){
      setTimeout(() => {
        const host = body.ssl_domain || location.hostname;
        const p = body.port || location.port;
        const bp = (body.base_path ? '/' + body.base_path.replace(/^\/+|\/+$/g, '') : '') + '/';
        location.href = 'https://' + host + (p && p != 443 ? ':' + p : '') + bp;
      }, 1200);
    }
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

$('#checkUpdateBtn').onclick = async e => {
  e.target.disabled = true;
  const box = $('#updateInfoBox');
  box.style.display = 'block';
  box.innerHTML = '<span style="color:var(--dim)">正在连接 GitHub 检查最新版本...</span>';
  try{
    const st = await api('/api/update/check');
    $('#updateCurVer').textContent = '当前版本: ' + (st.current || 'dev');
    if(st.has_update){
      box.innerHTML = '<div style="color:var(--ok);font-weight:600;margin-bottom:4px">发现新版本: ' + esc(st.latest) + '</div>'
        + '<div style="color:var(--dim);margin-bottom:8px;max-height:80px;overflow-y:auto;white-space:pre-wrap">' + esc(st.notes || '暂无更新日志') + '</div>'
        + '<button class="primary" id="applyUpdateBtn" style="font-size:12px;padding:4px 10px">立即一键更新</button>';
      $('#applyUpdateBtn').onclick = async btn => {
        btn.target.disabled = true;
        btn.target.textContent = '正在下载与更新...';
        try{
          await api('/api/update/apply', {method:'POST'});
          toast('更新成功，服务正在重启...');
          setTimeout(() => location.reload(), 3000);
        }catch(err){
          toast('更新失败: ' + err.message, true);
          btn.target.disabled = false;
          btn.target.textContent = '立即一键更新';
        }
      };
    } else {
      box.innerHTML = '<span style="color:var(--ok)">✓ 当前已是最新版本 (' + esc(st.latest || st.current) + ')</span>';
    }
  }catch(err){
    box.innerHTML = '<span style="color:var(--danger)">✗ 检查更新失败: ' + esc(err.message) + '</span>';
  }
  e.target.disabled = false;
};

// ---- 自定义 SOCKS5 出口相关 ----
$('#openCustomExitModalBtn').onclick = () => {
  $('#csRawUrl').value = '';
  $('#csHost').value = '';
  $('#csPort').value = '';
  $('#csUser').value = '';
  $('#csPass').value = '';
  $('#csCountry').value = '';
  $('#csRemark').value = '';
  $('#csTestResult').innerHTML = '';
  openModal('customExitModal');
};

$('#csRawUrl').oninput = () => {
  const raw = $('#csRawUrl').value.trim();
  if(!raw) return;
  try{
    if(raw.startsWith('socks5://') || raw.startsWith('socks://')){
      const u = new URL(raw);
      $('#csHost').value = u.hostname;
      $('#csPort').value = u.port || 1080;
      $('#csUser').value = u.username || '';
      $('#csPass').value = u.password || '';
      if(u.hash){
        $('#csRemark').value = decodeURIComponent(u.hash.replace(/^#/, ''));
      }
    } else {
      const parts = raw.split(':');
      if(parts.length >= 2){
        $('#csHost').value = parts[0];
        $('#csPort').value = parts[1];
        if(parts.length >= 4){
          $('#csUser').value = parts[2];
          $('#csPass').value = parts[3];
        }
      }
    }
  }catch(e){}
};

$('#csTestBtn').onclick = async e => {
  const host = $('#csHost').value.trim();
  const port = parseInt($('#csPort').value.trim(), 10);
  if(!host || !port){ toast('请填写地址和端口', true); return; }
  e.target.disabled = true;
  $('#csTestResult').innerHTML = '<span style="color:var(--dim)">正在连接并探测出口 IP...</span>';
  try{
    const res = await api('/api/custom/socks/test', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        host: host,
        port: port,
        user: $('#csUser').value.trim(),
        pass: $('#csPass').value.trim(),
      }),
    });
    $('#csTestResult').innerHTML = '<span style="color:var(--ok)">✓ 连通成功！出口 IP: ' + esc(res.exit_ip) + ' (' + res.ping + ' ms)</span>';
  }catch(err){
    $('#csTestResult').innerHTML = '<span style="color:var(--danger)">✗ 连接失败: ' + esc(err.message) + '</span>';
  }
  e.target.disabled = false;
};

$('#saveCustomExitBtn').onclick = async e => {
  const host = $('#csHost').value.trim();
  const port = parseInt($('#csPort').value.trim(), 10);
  if(!host || !port){ toast('请填写服务器地址和端口', true); return; }
  e.target.disabled = true;
  try{
    await api('/api/custom/socks/add', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        host: host,
        port: port,
        user: $('#csUser').value.trim(),
        pass: $('#csPass').value.trim(),
        country: $('#csCountry').value.trim() || '自定义',
        country_code: 'CUSTOM',
        remark: $('#csRemark').value.trim() || host,
      }),
    });
    toast('自定义 SOCKS5 出口已添加并启用');
    closeModal('customExitModal');
    poll();
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

// ---- SOCKS5 订阅源管理相关 ----
async function loadSourcesList(){
  const box = $('#sourcesContainer');
  try{
    const list = await api('/api/custom/source/list');
    if(!list || !list.length){
      box.innerHTML = '<div style="color:var(--dim);font-size:12px;text-align:center;padding:12px">暂无订阅源，请在上方添加</div>';
      return;
    }
    box.innerHTML = list.map(s => {
      const typeBadge = s.is_builtin
        ? '<span style="background:#238636;color:#fff;font-size:10px;padding:2px 6px;border-radius:3px;font-weight:600">系统内置</span>'
        : '<span style="background:#1f6feb;color:#fff;font-size:10px;padding:2px 6px;border-radius:3px;font-weight:600">自定义源</span>';
      
      const isEn = s.enabled !== false;
      const statusBadge = isEn
        ? '<span style="background:rgba(63,185,80,.15);color:#3fb950;border:1px solid rgba(63,185,80,.3);font-size:10px;padding:2px 6px;border-radius:3px;font-weight:600">已启用</span>'
        : '<span style="background:rgba(248,81,73,.15);color:#f85149;border:1px solid rgba(248,81,73,.3);font-size:10px;padding:2px 6px;border-radius:3px;font-weight:600">已禁用</span>';

      const toggleBtn = isEn
        ? '<button class="chip-btn danger" data-toggle-src="' + esc(s.id) + '" title="点击禁用此源（节点将从家宽/机房池移除）">禁用</button>'
        : '<button class="chip-btn" style="background:#238636;color:#fff" data-toggle-src="' + esc(s.id) + '" title="点击启用此源（节点将加入家宽/机房池）">启用</button>';

      const autoSelect = s.is_builtin ? '' : (
        '<div style="display:inline-flex;align-items:center;gap:4px;margin-right:2px">'
        + '<span style="font-size:11px;color:var(--dim)">自动更新:</span>'
        + '<select class="source-auto-select" data-src-id="' + esc(s.id) + '" style="padding:2px 6px;font-size:11px;background:#0d1117;border:1px solid var(--line);border-radius:4px;color:var(--text);cursor:pointer">'
        +   '<option value="0"' + (!s.auto_update ? ' selected' : '') + '>关闭</option>'
        +   '<option value="30"' + (s.auto_update && s.update_interval_m === 30 ? ' selected' : '') + '>每 30 分钟</option>'
        +   '<option value="60"' + (s.auto_update && (s.update_interval_m === 60 || !s.update_interval_m) ? ' selected' : '') + '>每 1 小时 (默认)</option>'
        +   '<option value="120"' + (s.auto_update && s.update_interval_m === 120 ? ' selected' : '') + '>每 2 小时</option>'
        +   '<option value="360"' + (s.auto_update && s.update_interval_m === 360 ? ' selected' : '') + '>每 6 小时</option>'
        +   '<option value="720"' + (s.auto_update && s.update_interval_m === 720 ? ' selected' : '') + '>每 12 小时</option>'
        +   '<option value="1440"' + (s.auto_update && s.update_interval_m === 1440 ? ' selected' : '') + '>每 24 小时</option>'
        + '</select>'
        + '</div>'
      );

      const actBtns = s.is_builtin
        ? ('<button class="chip-btn" data-refresh-src="' + esc(s.id) + '" title="立即刷新官方节点数据">' + ICON.redo + ' 刷新节点池</button>')
        : (toggleBtn
           + autoSelect
           + '<button class="icon" data-refresh-src="' + esc(s.id) + '" title="立即手动拉取更新源节点">' + ICON.redo + '</button>'
           + '<button class="icon danger" data-del-src="' + esc(s.id) + '" title="删除此源">' + ICON.trash + '</button>');

      const timeStr = s.updated_at ? (' · 上次更新: ' + new Date(s.updated_at).toLocaleTimeString()) : '';
      const nodeCounts = s.is_builtin
        ? (s.count + ' 个节点 (全部为 🏠 家宽)' + timeStr)
        : (s.count + ' 个节点 (🏠 家宽: ' + (s.residential_count || 0) + ' · 🏢 机房: ' + (s.datacenter_count || 0) + ')' + timeStr);

      return '<div style="background:#12151a;border:1px solid var(--line);border-radius:4px;padding:10px 12px;display:flex;align-items:center;justify-content:space-between;gap:12px">'
        + '<div style="min-width:0;flex:1">'
        +   '<div style="font-weight:600;font-size:13px;display:flex;align-items:center;gap:6px">' 
        +     esc(s.name) + ' ' + typeBadge + ' ' + statusBadge
        +   '</div>'
        +   '<div style="font-size:11px;color:var(--dim);margin-top:2px">' + esc(nodeCounts) + '</div>'
        +   '<div style="font-size:11px;color:var(--dim);margin-top:2px;max-width:320px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(s.url) + '">' + esc(s.url) + '</div>'
        + '</div>'
        + '<div style="display:flex;gap:6px;align-items:center;flex-shrink:0">'
        +   actBtns
        + '</div>'
        + '</div>';
    }).join('');
  }catch(err){ box.innerHTML = '<div style="color:var(--danger)">加载失败: ' + esc(err.message) + '</div>'; }
}

$('#openCustomSourceModalBtn').onclick = () => {
  $('#srcName').value = '';
  $('#srcURL').value = '';
  openModal('customSourceModal');
  loadSourcesList();
};

$('#addSourceBtn').onclick = async e => {
  const name = $('#srcName').value.trim();
  const url = $('#srcURL').value.trim();
  if(!name || !url){ toast('请填写源名称和 URL', true); return; }
  e.target.disabled = true;
  try{
    const res = await api('/api/custom/source/add', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify({name: name, url: url}),
    });
    toast('源添加成功（已默认启用并开启 1 小时自动更新），解析 ' + res.count + ' 个节点');
    $('#srcName').value = '';
    $('#srcURL').value = '';
    loadSourcesList();
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

$('#sourcesContainer').onchange = async e => {
  const sel = e.target.closest('.source-auto-select');
  if(sel){
    const id = sel.dataset.srcId;
    const val = parseInt(sel.value, 10);
    const autoUpdate = val > 0;
    const interval = autoUpdate ? val : 60;
    try{
      await api('/api/custom/source/settings', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({id: id, auto_update: autoUpdate, update_interval_m: interval})
      });
      toast(autoUpdate ? ('已设置自动更新: ' + sel.options[sel.selectedIndex].text) : '已关闭该源自动更新');
    }catch(err){ toast('设置失败: ' + err.message, true); }
  }
};

$('#sourcesContainer').onclick = async e => {
  const tog = e.target.closest('[data-toggle-src]');
  if(tog){
    const id = tog.dataset.toggleSrc;
    tog.disabled = true;
    try{
      const res = await api('/api/custom/source/toggle?id=' + encodeURIComponent(id), {method:'POST'});
      toast(res.enabled ? '已启用该源，节点已分类加入家宽/机房池' : '已禁用该源，节点已从池中移出');
      loadSourcesList();
    }catch(err){ toast(err.message, true); }
    tog.disabled = false;
    return;
  }

  const ref = e.target.closest('[data-refresh-src]');
  if(ref){
    const id = ref.dataset.refreshSrc;
    try{
      const res = await api('/api/custom/source/refresh?id=' + encodeURIComponent(id), {method:'POST'});
      toast('源已手动拉取更新，当前 ' + res.count + ' 个节点');
      loadSourcesList();
    }catch(err){ toast(err.message, true); }
    return;
  }

  const del = e.target.closest('[data-del-src]');
  if(del){
    if(!confirm('确定删除此源？')) return;
    const id = del.dataset.delSrc;
    try{
      await api('/api/custom/source/delete?id=' + encodeURIComponent(id), {method:'POST'});
      toast('已删除该源');
      loadSourcesList();
    }catch(err){ toast(err.message, true); }
    return;
  }
};

poll();
setInterval(poll, 3000);
</script>
</body>
</html>`
