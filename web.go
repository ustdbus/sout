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
<title>fanout - s-ui 外部对接插件</title>
<style>
:root{
  --bg:#0e1117; --panel:#161b22; --card:#1b212b; --line:#2d333b; --text:#e6edf3;
  --dim:#8b949e; --accent:#58a6ff; --accent-hover:#79b8ff; --ok:#3fb950; --warn:#d29922; --bad:#f85149;
  --proto-vless:#a371f7; --proto-tuic:#f0883e; --proto-vmess:#1f6feb; --proto-trojan:#56d364;
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

.section-head{display:flex;align-items:center;gap:12px;margin:24px 0 14px}
.section-head:first-child{margin-top:0}
.section-head h2{font-size:14px;font-weight:700;margin:0;color:var(--text);display:flex;align-items:center;gap:8px}
.section-head .desc{color:var(--dim);font-size:12px}

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

.dot{width:8px;height:8px;border-radius:50%;background:var(--dim);flex-shrink:0}
.dot.up{background:var(--ok)}
.dot.starting{background:var(--warn);animation:pulse 1.2s ease-in-out infinite}
.dot.failed{background:var(--bad)}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}

.exits-box{background:var(--panel);border:1px solid var(--line);border-radius:8px;overflow:hidden;margin-bottom:20px}
.exit-row{display:grid;grid-template-columns:14px 140px 1fr auto auto;align-items:center;gap:12px;padding:10px 16px;border-bottom:1px solid #21262d}
.exit-row:last-child{border-bottom:none}
.exit-ip{font-weight:700;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.exit-meta{color:var(--dim);font-size:12px}

.empty{border:1px dashed var(--line);border-radius:8px;padding:32px 20px;text-align:center;color:var(--dim)}

.modal{position:fixed;inset:0;background:rgba(1,4,9,.75);display:none;align-items:center;justify-content:center;z-index:50;padding:20px;backdrop-filter:blur(2px)}
.modal.open{display:flex}
.sheet{background:var(--panel);border:1px solid var(--line);border-radius:8px;width:min(580px,100%);max-height:86vh;display:flex;flex-direction:column;box-shadow:0 8px 24px rgba(0,0,0,.5)}
.sheet .head{display:flex;align-items:center;gap:10px;padding:12px 18px;border-bottom:1px solid var(--line);background:var(--card);border-radius:8px 8px 0 0}
.sheet .head h2{font-size:14px;margin:0;font-weight:700}
.sheet .body{overflow:auto;padding:18px}
.sheet .foot{display:flex;align-items:center;gap:10px;padding:12px 18px;border-top:1px solid var(--line);background:var(--card);border-radius:0 0 8px 8px}

.tab-nav{display:flex;gap:4px;border-bottom:1px solid var(--line);margin-bottom:16px;padding-bottom:2px}
.tab-btn{background:transparent;border:0;border-bottom:2px solid transparent;border-radius:0;color:var(--dim);padding:6px 12px;font-size:13px}
.tab-btn.active{color:var(--accent);border-bottom-color:var(--accent);font-weight:600}

label.f{display:block;margin-bottom:16px}
label.f>span{display:block;color:var(--dim);font-size:12px;margin-bottom:6px;font-weight:500}
select,input[type=search],input[type=text],input[type=password]{font:inherit;background:#0d1117;border:1px solid var(--line);color:var(--text);border-radius:6px;padding:7px 10px;width:100%}
select:focus,input:focus{outline:none;border-color:var(--accent)}
.regions{display:grid;grid-template-columns:repeat(auto-fill,minmax(130px,1fr));gap:6px;max-height:220px;overflow:auto;margin-top:8px}
.rg{border:1px solid var(--line);background:#0d1117;border-radius:6px;padding:7px 9px;cursor:pointer;text-align:left;display:block;width:100%}
.rg:hover{border-color:var(--accent)}
.rg.sel{border-color:var(--accent);background:rgba(88,166,255,.12)}
.rg b{font-weight:600;font-size:12px;display:block}
.rg em{display:block;font-style:normal;color:var(--dim);font-size:11px;margin-top:2px}

.toast{position:fixed;left:50%;bottom:28px;transform:translateX(-50%);background:#1f242c;border:1px solid var(--line);border-radius:6px;padding:9px 16px;font-size:13px;z-index:80;opacity:0;pointer-events:none;transition:opacity .18s;box-shadow:0 4px 12px rgba(0,0,0,.4)}
.toast.show{opacity:1}
.toast.bad{border-color:rgba(248,81,73,.5);color:var(--bad)}
textarea{width:100%;min-height:280px;background:#0d1117;border:1px solid var(--line);color:var(--text);border-radius:6px;font:12px/1.8 ui-monospace,SFMono-Regular,Menlo,monospace;padding:12px;resize:vertical}
</style>
</head>
<body>
<header>
  <h1>
    <svg viewBox="0 0 24 24" style="color:var(--accent)"><path d="M12 2L2 7l10 5 10-5-10-5zM2 17l10 5 10-5M2 12l10 5 10-5"/></svg>
    fanout
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
  <div class="section-head">
    <h2>
      <svg viewBox="0 0 24 24" style="color:var(--accent)"><rect x="2" y="2" width="20" height="8" rx="2" ry="2"/><rect x="2" y="14" width="20" height="8" rx="2" ry="2"/><line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/></svg>
      s-ui 节点与分流管理
    </h2>
    <span class="desc">直接读取 s-ui 面板中的节点，支持点击每个节点的「+」同时添加直连与各国出站</span>
    <span class="spacer"></span>
    <button class="primary" id="refreshNodesBtn">
      <svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>
      刷新节点
    </button>
  </div>

  <div id="nodesContainer"></div>

  <div class="section-head" style="margin-top:36px">
    <h2>
      <svg viewBox="0 0 24 24" style="color:var(--ok)"><circle cx="12" cy="12" r="10"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
      VPN Gate 出口隧道池
    </h2>
    <span class="desc">当前连通的公共家宽出口</span>
    <span class="spacer"></span>
    <button id="newExitBtn">
      <svg viewBox="0 0 24 24"><path d="M12 5v14"/><path d="M5 12h14"/></svg>
      新建国家出口
    </button>
    <button id="stopAllExitsBtn">全部停止</button>
  </div>

  <div id="exitsContainer"></div>
</main>

<!-- Modal: 添加出口分流 -->
<div class="modal" id="addBranchModal">
  <div class="sheet">
    <div class="head">
      <h2 id="abTitle">添加出口分流</h2>
      <span class="spacer"></span>
      <button class="icon" data-close="addBranchModal" title="关闭"><svg viewBox="0 0 24 24"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>
    </div>
    <div class="body">
      <div class="tab-nav">
        <button class="tab-btn active" id="tabExist">从已有出站选择</button>
        <button class="tab-btn" id="tabNew">新建国家出站</button>
      </div>

      <div id="paneExist">
        <label class="f">
          <span>选择已连通的出口</span>
          <select id="existExitSelect"></select>
        </label>
        <div style="color:var(--dim);font-size:12px">选择后将为此节点克隆一个独立端口，并将流量分流到该出站，原直连节点不受影响。</div>
      </div>

      <div id="paneNew" style="display:none">
        <label class="f">
          <span>选择目标国家/地区</span>
          <input type="search" id="rgFilter" placeholder="搜索地区，如 JP、美国、日本...">
          <div class="regions" id="rgList"></div>
        </label>
        <div style="color:var(--dim);font-size:12px" id="newExitHint">将自动拉起选定国家的 VPN 隧道并绑定到此节点。</div>
      </div>
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="addBranchModal">取消</button>
      <button class="primary" id="confirmAddBranchBtn">确定添加</button>
    </div>
  </div>
</div>

<!-- Modal: 导出全部链接 -->
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

<!-- Modal: 设置 -->
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
    </div>
    <div class="foot">
      <span class="spacer"></span>
      <button data-close="settingsModal">取消</button>
      <button class="primary" id="saveSettingsBtn">保存设置</button>
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
  lock:'<svg viewBox="0 0 24 24"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>'
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
let currentTab = 'exist';
let selectedRegion = 'JP';
let regionList = [];

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
      +   '<button class="primary" data-add-node="' + n.base_id + '" data-node-name="' + esc(n.name) + '" style="box-shadow:0 2px 6px rgba(88,166,255,.3)">'
      +     ICON.plus + ' 添加出口分流'
      +   '</button>'
      + '</div>'
      + '<div class="branch-table">'
      +   branches.map(b => {
            const hasLink = b.links && b.links.length;
            const firstLink = hasLink ? b.links[0] : '';
            return '<div class="branch-row">'
              + '<div class="branch-info">'
              +   '<span class="dot up" title="正常"></span>'
              +   '<span class="branch-name">' + esc(b.bound_label) + '</span>'
              +   '<span class="branch-tag">' + esc(b.tag) + '</span>'
              +   '<span class="branch-port">:' + b.port + '</span>'
              + '</div>'
              + '<div class="branch-acts">'
              +   (hasLink ? '<button class="chip-btn" data-copy="' + esc(firstLink) + '">' + ICON.copy + ' 复制链接</button>' : '')
              +   (!b.is_base ? '<button class="icon danger" data-delone="' + b.id + '" data-name="' + esc(b.tag) + '" title="删除此出口分流">' + ICON.trash + '</button>' : '<span style="font-size:11px;color:var(--dim);margin-right:6px">基础直连</span>')
              + '</div>'
              + '</div>';
          }).join('')
      + '</div>'
      + '</div>';
  }).join('');
}

function renderExits(){
  const box = $('#exitsContainer');
  const exits = viewData.exits || [];
  if(!exits.length){
    box.innerHTML = '<div class="empty">暂无运行中的 VPN 出口隧道</div>';
    return;
  }

  box.innerHTML = '<div class="exits-box">' + exits.map(e => {
    const label = e.exit_ip || (e.status === 'starting' ? '连接中…' : '—');
    const place = e.country || e.region || '—';
    return '<div class="exit-row">'
      + '<span class="dot ' + e.status + '"></span>'
      + '<span class="exit-ip">' + esc(label) + '</span>'
      + '<span class="exit-meta">' + esc(place) + ' · ' + esc(e.host) + ' · SOCKS5 :' + e.port + '</span>'
      + '<span class="branch-acts">'
      +   '<button class="icon" data-swap="' + e.slot + '" title="换一个节点">' + ICON.redo + '</button>'
      +   '<button class="icon" data-stop="' + e.slot + '" title="停止此隧道">' + ICON.trash + '</button>'
      + '</span>'
      + '</div>';
  }).join('') + '</div>';
}

async function poll(){
  try{
    viewData = await api('/api/exits');
    renderNodes();
    renderExits();
  }catch(e){}
}

// ---- 添加分流弹窗与事件 ----
document.addEventListener('click', async e => {
  const addBtn = e.target.closest('[data-add-node]');
  if(addBtn){
    targetNodeID = Number(addBtn.dataset.addNode);
    targetNodeName = addBtn.dataset.nodeName;
    $('#abTitle').textContent = '为「' + targetNodeName + '」添加分流出站';

    // 填充已有出口下拉
    const upExits = (viewData.exits || []).filter(x => x.status === 'up');
    const sel = $('#existExitSelect');
    if(!upExits.length){
      sel.innerHTML = '<option value="">暂无可用出站（请点右侧新建）</option>';
      switchTab('new');
    } else {
      sel.innerHTML = upExits.map(x =>
        '<option value="' + esc(x.host) + '">' + esc((x.country || x.region) + ' (' + (x.exit_ip || x.host) + ' · SOCKS5:' + x.port + ')') + '</option>').join('');
      switchTab('exist');
    }

    if(!regionList.length){
      try{
        regionList = await api('/api/regions') || [];
        renderRegionList();
      }catch(err){}
    } else {
      renderRegionList();
    }

    openModal('addBranchModal');
  }

  const cp = e.target.closest('[data-copy]');
  if(cp) copy(cp.dataset.copy);

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

  const stop = e.target.closest('[data-stop]');
  if(stop){
    stop.disabled = true;
    try{ await api('/api/stop?slot=' + stop.dataset.stop, {method:'POST'}); toast('已停止'); poll(); }
    catch(err){ toast(err.message, true); }
  }

  const swap = e.target.closest('[data-swap]');
  if(swap){
    swap.disabled = true;
    try{ await api('/api/swap?slot=' + swap.dataset.swap, {method:'POST'}); toast('正在换节点'); poll(); }
    catch(err){ toast(err.message, true); }
  }

  const rg = e.target.closest('[data-rgcode]');
  if(rg){
    selectedRegion = rg.dataset.rgcode;
    renderRegionList();
  }
});

function switchTab(t){
  currentTab = t;
  $('#tabExist').className = 'tab-btn' + (t === 'exist' ? ' active' : '');
  $('#tabNew').className = 'tab-btn' + (t === 'new' ? ' active' : '');
  $('#paneExist').style.display = t === 'exist' ? 'block' : 'none';
  $('#paneNew').style.display = t === 'new' ? 'block' : 'none';
}
$('#tabExist').onclick = () => switchTab('exist');
$('#tabNew').onclick = () => switchTab('new');

function renderRegionList(){
  const kw = ($('#rgFilter').value || '').trim().toLowerCase();
  const filtered = regionList.filter(r => !kw || r.code.toLowerCase().includes(kw) || r.name.toLowerCase().includes(kw));
  $('#rgList').innerHTML = filtered.map(r =>
    '<button class="rg' + (selectedRegion === r.code ? ' sel' : '') + '" data-rgcode="' + esc(r.code) + '">'
    + '<b>' + esc(r.code) + ' ' + esc(r.name) + '</b>'
    + '<em>' + r.available + ' 个可用 · ' + r.best_speed_mbps.toFixed(0) + ' Mbps</em>'
    + '</button>').join('');
}
$('#rgFilter').oninput = renderRegionList;

$('#confirmAddBranchBtn').onclick = async e => {
  e.target.disabled = true;
  try{
    if(currentTab === 'exist'){
      const host = $('#existExitSelect').value;
      if(!host) throw new Error('请选择一个已有出站');
      await api('/api/node/add_branch?template_id=' + targetNodeID + '&host=' + encodeURIComponent(host), {method:'POST'});
      toast('已成功为「' + targetNodeName + '」添加分流出口');
    } else {
      if(!selectedRegion) throw new Error('请选择国家/地区');
      await api('/api/node/add_branch?template_id=' + targetNodeID + '&region=' + encodeURIComponent(selectedRegion), {method:'POST'});
      toast('正在拉起「' + selectedRegion + '」出口隧道并绑定节点...');
    }
    closeModal('addBranchModal');
    poll();
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

$('#refreshNodesBtn').onclick = () => { toast('已刷新'); poll(); };

$('#exportAll').onclick = () => {
  const allLinks = [];
  (viewData.nodes || []).forEach(n => {
    (n.branches || []).forEach(b => {
      (b.links || []).forEach(l => allLinks.push(l));
    });
  });
  if(!allLinks.length){ toast('暂无可用分享链接', true); return; }
  $('#allLinksBox').value = allLinks.join('\n');
  openModal('exportModal');
};
$('#copyAllLinksBtn').onclick = () => copy($('#allLinksBox').value);

$('#stopAllExitsBtn').onclick = async () => {
  if(!confirm('停止全部 VPN 出口？')) return;
  for(const x of (viewData.exits || [])){
    try{ await api('/api/stop?slot=' + x.slot, {method:'POST'}); }catch(e){}
  }
  toast('已停止全部出口');
  poll();
};

$('#newExitBtn').onclick = () => {
  targetNodeID = (viewData.nodes && viewData.nodes.length) ? viewData.nodes[0].base_id : 1;
  targetNodeName = (viewData.nodes && viewData.nodes.length) ? viewData.nodes[0].name : '';
  switchTab('new');
  openModal('addBranchModal');
};

$('#settingsBtn').onclick = async () => {
  openModal('settingsModal');
  try{
    const s = await api('/api/settings');
    $('#setPath').value = (s.base_path || '').replace(/^\//, '');
    $('#setPort').value = s.port || '';
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
  try{
    await api('/api/settings', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify(body),
    });
    toast('设置已保存');
    closeModal('settingsModal');
  }catch(err){ toast(err.message, true); }
  e.target.disabled = false;
};

poll();
setInterval(poll, 3000);
</script>
</body>
</html>`
