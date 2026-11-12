/* DiskFleet console. All records come from the API; demo data is owned by the server. */
"use strict";

const $ = (selector, scope = document) => scope.querySelector(selector);
const $$ = (selector, scope = document) => [...scope.querySelectorAll(selector)];
// The gateway strips /storage; standalone delivery keeps the original /api routes.
const API_PREFIX = /^\/storage(?:\/|$)/.test(location.pathname) ? "/storage" : "";
const EMBEDDED = new URLSearchParams(location.search).get("embedded") === "1";
document.documentElement.classList.toggle("embedded", EMBEDDED);
$("#pulsar-console-link").hidden = !API_PREFIX || EMBEDDED;
const escapeHTML = value => String(value ?? "").replace(/[&<>"']/g, char => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char]));
const e = escapeHTML;
const paths = {
  overview: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
  disks: '<rect x="3" y="4" width="18" height="16" rx="3"/><circle cx="12" cy="11" r="4"/><circle cx="12" cy="11" r="1"/><path d="m14 14 4 4M6 17h2"/>',
  pools: '<path d="m12 3 9 5-9 5-9-5 9-5Zm-9 9 9 5 9-5M3 16l9 5 9-5"/>',
  volumes: '<rect x="4" y="3" width="16" height="18" rx="2"/><path d="M8 8h8M8 12h8M8 16h4"/>',
  events: '<path d="M8 3h8v4H8zM8 5H5v16h14V5h-3M8 11h8M8 15h8M8 18h4"/>',
  guide: '<path d="M12 5c-4-3-8-2-10-1v15c3-2 7-2 10 0 3-2 7-2 10 0V4c-2-1-6-2-10 1Zm0 0v14"/>',
  nodes: '<rect x="3" y="3" width="18" height="7" rx="2"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 6.5h.01M7 17.5h.01M12 6.5h5M12 17.5h5"/>',
  capacity: '<path d="M4 17a9 9 0 1 1 16 0M12 12l5-5M8 20h8"/><circle cx="12" cy="12" r="1.5"/>'
};
const icon = name => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths[name] || paths.disks}</svg>`;
const pages = {
  overview: ["集群总览", "掌握物理磁盘、存储池和持久卷的每一次变化。"],
  nodes: ["物理节点", "按可用区与物理机器查看磁盘分布，定位节点容量。"],
  disks: ["物理磁盘", "以稳定设备身份管理物理盘，按需接管空盘并创建存储池。"],
  pools: ["存储池", "一盘一池隔离故障，按节点掌握已分配与可用容量。"],
  volumes: ["PV / PVC", "为业务申请持久存储，跟踪分配、扩容与保留状态。"],
  events: ["操作审计", "追踪磁盘纳管、容量分配与卷生命周期的操作记录。"],
  guide: ["接盘指南", "了解物理磁盘如何接入 Linux，再成为 Kubernetes 持久卷。"]
};
const state = {page:"overview", mode:"demo", tokenRequired:false, offset:0, limit:12, q:"", diskState:"", diskNode:"", selected:new Map(), records:new Map(), loading:false, generation:0, timer:null};
const statusNames = {Available:"可接管", Managed:"已纳管", Protected:"受保护", Offline:"离线", Ready:"就绪", Pending:"等待中", Bound:"已绑定", Released:"已保留", ReleaseRequested:"已请求释放", Releasing:"等待卸载", Deleted:"已删除", Error:"异常", Failed:"失败", Conflict:"认领冲突", Online:"在线", Retained:"已保留", Active:"在线"};
let storageInitialized = false, storageReady = false;
function reportEmbedded(kind) {
  if (!EMBEDDED || window.parent === window || !["route", "ready"].includes(kind)) return;
  if (kind === "ready" && !storageInitialized) return;
  const message = {channel:"pulsar.storage", kind, page:state.page};
  if (kind === "ready") message.apiReady = storageReady;
  window.parent.postMessage(message, location.origin);
}
window.addEventListener("message", event => {
  if (!EMBEDDED || event.source !== window.parent || event.origin !== location.origin) return;
  if (event.data?.channel !== "pulsar.storage" || event.data.kind !== "request-state") return;
  reportEmbedded("ready");
});

function token() { try { return sessionStorage.getItem("diskfleet-token") || ""; } catch (_) { return ""; } }
function saveToken(value) { try { if (value) sessionStorage.setItem("diskfleet-token", value); else sessionStorage.removeItem("diskfleet-token"); } catch (_) { throw new Error("浏览器不允许会话存储，无法保存访问凭证。"); } }
async function api(path, options = {}) {
  const headers = {Accept:"application/json", ...options.headers};
  if (token()) headers.Authorization = `Bearer ${token()}`;
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  let response;
  try { response = await fetch(`${API_PREFIX}${path}`, {...options, headers, body:options.body === undefined ? undefined : JSON.stringify(options.body)}); }
  catch (_) { throw new Error("无法连接管理 API。请确认服务正在运行，然后重试。"); }
  let result;
  try { result = await response.json(); } catch (_) { result = null; }
  if (!response.ok) {
    if (response.status === 401 || response.status === 403) throw new Error("访问凭证缺失、失效或权限不足，请通过右上角“访问凭证”设置 Token。");
    const detail = result?.detail;
    throw new Error(typeof detail === "string" ? detail : Array.isArray(detail) ? detail.map(item => item.msg || JSON.stringify(item)).join("；") : `请求失败（HTTP ${response.status}）`);
  }
  return result;
}
function bytes(value, digits=1) {
  const n = Number(value) || 0;
  if (n === 0) return "0 B";
  const units = ["B","KiB","MiB","GiB","TiB","PiB"];
  const power = Math.min(Math.floor(Math.log(Math.abs(n))/Math.log(1024)), units.length-1);
  return `${(n/1024**power).toLocaleString("zh-CN", {maximumFractionDigits:digits})} ${units[Math.max(0,power)]}`;
}
function count(value) { return Number(value || 0).toLocaleString("zh-CN"); }
function percent(used, total) { return total > 0 ? Math.min(100, Math.max(0, Math.round(Number(used)/Number(total)*100))) : 0; }
function date(value, compact=false) {
  if (!value) return "—";
  const d = new Date(value);
  if (!Number.isFinite(d.getTime())) return String(value);
  return d.toLocaleString("zh-CN", {month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",...(compact?{}:{year:"numeric"})});
}
function badge(value) {
  const good = ["Available","Managed","Ready","Bound","Online","Active"].includes(value);
  const bad = ["Error","Offline","Failed","Conflict"].includes(value);
  const kind = good ? "good" : bad ? "bad" : ["Pending","ReleaseRequested","Releasing"].includes(value) ? "warn" : "neutral";
  return `<span class="badge ${kind}">${e(statusNames[value] || value || "未知")}</span>`;
}
function toast(message, error=false) {
  const el = document.createElement("div"); el.className = `toast${error ? " error" : ""}`; el.textContent = message;
  $("#toast-container").append(el); setTimeout(() => el.remove(), error ? 7000 : 4500);
}
function empty(title="暂无记录", description="新记录会在下一次同步后显示。", action="") {
  return `<div class="empty"><span class="empty-symbol" aria-hidden="true">◇</span><h3>${e(title)}</h3><p>${e(description)}</p>${action}</div>`;
}
function cache(records) { for (const item of records || []) state.records.set(String(item.id || item.name), item); }
function usage(item) {
  const p = percent(item.allocatedBytes,item.capacityBytes);
  return `<div class="usage"><p><span>${e(bytes(item.allocatedBytes))}</span><span>${p}%</span></p><div class="mini-bar" role="meter" aria-label="已分配比例" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${p}"><span style="width:${p}%"></span></div></div>`;
}
function pagination(data) {
  const total = Number(data.total || 0), start = total ? state.offset + 1 : 0, end = Math.min(state.offset + state.limit,total);
  return `<div class="pagination"><span>显示 ${count(start)}–${count(end)} 条，共 ${count(total)} 条</span><div class="pagination-controls"><button data-action="prev" ${state.offset===0?"disabled":""} aria-label="上一页">← 上一页</button><span>${Math.floor(state.offset/state.limit)+1} / ${Math.max(1,Math.ceil(total/state.limit))}</span><button data-action="next" ${end>=total?"disabled":""} aria-label="下一页">下一页 →</button></div></div>`;
}
function query(extra={}) { return new URLSearchParams({limit:state.limit, offset:state.offset, ...extra}).toString(); }
function volumeMessages(volume) {
  return [...new Set([volume.bindingMessage, volume.message].filter(value=>typeof value==="string" && value))];
}
function volumeReadOnlyReason(volume) {
  const messages=volumeMessages(volume);
  return messages.length ? messages.join("；") : volume.management==="external" ? "此 PVC 由外部 Operator 管理，请通过对应管理资源操作。" : "此卷的身份或回收状态尚未确认，暂时只读，请核对存储状态。";
}
function volumeReleasePath(volume) {
  const path=`/api/volumes/${encodeURIComponent(volume.id)}`;
  if (state.mode!=="cluster") return path;
  if (!volume.pvcUid) throw new Error("PVC 身份缺失，请刷新并核对后再释放。");
  return `${path}?uid=${encodeURIComponent(volume.pvcUid)}`;
}
function rowActions(id, type) {
  if (type === "disk") return `<button class="button-link" data-action="disk-detail" data-id="${e(id)}">查看详情</button>`;
  if (type === "pool") return `<button class="button-link" data-action="create-volume" data-id="${e(id)}">申请 PVC</button>`;
  if (state.records.get(String(id))?.canManage === false) return `<button class="button-link" data-action="volume-detail" data-id="${e(id)}">只读详情</button>`;
  return `<div class="table-actions"><button class="button-link" data-action="volume-detail" data-id="${e(id)}">详情</button><button class="button-link" data-action="expand-volume" data-id="${e(id)}">扩容</button><button class="button-link" data-action="release-volume" data-id="${e(id)}">释放</button></div>`;
}
function eventRows(events) {
  return events.map(item => `<div class="event-row"><span class="event-icon" aria-hidden="true">${item.level === "error" ? "!" : "✓"}</span><div class="event-body"><strong>${e(item.action || "状态更新")}</strong><p>${e(item.message)}</p></div><time class="event-time">${e(date(item.time,true))}</time></div>`).join("");
}
function nav() {
  $("#navigation").innerHTML = Object.entries(pages).map(([key,[label]]) => `${key==="guide"?'<div class="nav-separator"></div>':""}<a class="nav-link${state.page===key?" active":""}" href="#${key}" ${state.page===key?'aria-current="page"':""} title="${label}">${icon(key)}<span class="nav-label">${label}</span></a>`).join("");
  $("#page-title").textContent = pages[state.page][0]; $("#page-description").textContent = pages[state.page][1]; $("#breadcrumb").textContent = pages[state.page][0];
  document.title = `${pages[state.page][0]} · DiskFleet`;
}

async function overview() {
  const [summary,nodes,disks] = await Promise.all([api("/api/overview"),api("/api/nodes?limit=6&offset=0"),api("/api/disks?limit=72&offset=0")]);
  cache(disks.items); const t=summary.totals || {}, allocated=Number(t.allocatedBytes)||0, available=Number(t.availableBytes)||0, capacity=Number(t.capacityBytes)||0;
  const allocatedPercent=percent(allocated,capacity), availablePercent=percent(available,capacity);
  const metrics = [
    ["纳管总容量",bytes(capacity),`${count(t.pools)} 个存储池 · 厚置备`,"capacity",true],
    ["物理节点",count(t.nodes),"每节点独立发现与调谐","nodes",false],
    ["物理磁盘",count(t.disks),"稳定设备身份 · 一盘一池","disks",false],
    ["持久卷",count(t.volumes),"默认 Retain 保留数据","volumes",false]
  ];
  const alertHTML = (summary.alerts || []).map(item => `<div class="notice notice-warning">${e(typeof item === "string" ? item : item?.message || item?.title || JSON.stringify(item))}</div>`).join("");
  const nodeHTML = (nodes.items || []).map(item => `<article class="node-card"><div class="node-head"><span>${e(item.name)}</span><span class="dot" aria-label="${e(item.state || "状态未知")}"></span></div><div class="node-meta">${e(item.zone || "未设置可用区")} · ${count(item.disks)} 块磁盘</div><div class="mini-bar"><span style="width:${percent(item.allocatedBytes,item.capacityBytes)}%"></span></div><div class="node-foot"><span>${e(bytes(item.allocatedBytes))} 已分配</span><span>${e(bytes(item.capacityBytes))}</span></div></article>`).join("");
  return `${alertHTML}<div class="metrics">${metrics.map(([label,value,caption,name,accent]) => `<article class="metric${accent?" accent":""}"><div class="metric-top"><span class="metric-label">${label}</span><span class="metric-icon">${icon(name)}</span></div><div class="metric-value">${value}</div><div class="metric-bottom"><span class="dot"></span>${caption}</div></article>`).join("")}</div>
    <div class="overview-grid"><section class="panel"><div class="panel-heading"><div><h2>容量分配</h2><p>已纳管存储池的逻辑分配情况</p></div><span class="tag">NO OVERPROVISION</span></div><div class="panel-body"><div class="capacity-main"><strong>${e(bytes(available))}</strong><span>可用容量 / ${e(bytes(capacity))}</span></div><div class="capacity-bar" role="meter" aria-label="存储池容量使用率" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${allocatedPercent}"><span class="allocated" style="width:${allocatedPercent}%"></span><span class="available" style="width:${availablePercent}%"></span></div><div class="legend"><span><i></i>已分配 ${allocatedPercent}%</span><span><i class="available"></i>可用 ${availablePercent}%</span></div><div class="capacity-details"><div><span>已分配容量</span><strong>${e(bytes(allocated))}</strong></div><div><span>存储池</span><strong>${count(t.pools)} <span style="display:inline">个</span></strong></div><div><span>回收策略</span><strong>Retain</strong></div></div></div></section>
    <section class="panel"><div class="panel-heading"><div><h2>物理磁盘分布</h2><p>展示前 ${count(disks.items?.length)} 块 / 共 ${count(disks.total)} 块</p></div><a class="button-link" href="#disks">查看全部 ↗</a></div><div class="panel-body">${disks.items?.length?`<div class="disk-map">${disks.items.map(d=>`<button class="disk-cell ${["Available","Managed","Protected","Offline"].includes(d.state)?d.state:"Protected"}" data-action="disk-detail" data-id="${e(d.id)}" title="${e(d.node)} / ${e(d.name || d.path)} · ${e(statusNames[d.state] || d.state)} · ${e(bytes(d.sizeBytes))}" aria-label="${e(d.node)} ${e(d.name || d.path)} ${e(statusNames[d.state] || d.state)}"></button>`).join("")}</div><div class="legend"><span><i></i>已纳管</span><span><i class="available"></i>可接管</span><span><i class="protected"></i>受保护</span><span><i class="offline"></i>离线</span></div>`:empty("尚未发现磁盘","节点 Agent 运行后会在此展示磁盘。")}</div></section></div>
    <div class="overview-grid"><section class="panel"><div class="panel-heading"><div><h2>节点容量概览</h2><p>按节点管理物理容量与业务分配</p></div><a class="button-link" href="#nodes">查看全部 ${count(nodes.total)} 个 ↗</a></div>${nodeHTML?`<div class="node-list">${nodeHTML}</div>`:empty("暂无节点")}</section><section class="panel"><div class="panel-heading"><div><h2>最近操作</h2><p>纳管与容量生命周期记录</p></div><a class="button-link" href="#events">全部记录 ↗</a></div><div class="panel-body"><div class="events">${summary.recentEvents?.length?eventRows(summary.recentEvents.slice(0,4)):empty("暂无操作记录")}</div></div></section></div>`;
}

async function diskPage() {
  const data = await api(`/api/disks?${query({q:state.q,state:state.diskState,node:state.diskNode})}`); cache(data.items);
  const available=(data.items || []).filter(d=>d.state==="Available"); const allSelected=available.length>0 && available.every(d=>state.selected.has(String(d.id)));
  return `<section class="panel table-panel"><div class="toolbar"><div class="toolbar-left"><form id="disk-search" class="search-box" role="search"><span aria-hidden="true">⌕</span><input class="input" name="q" value="${e(state.q)}" placeholder="搜索设备、节点、序列号" aria-label="搜索磁盘"></form><select class="input" id="disk-state" aria-label="按磁盘状态筛选"><option value="">全部状态</option>${["Available","Managed","Protected","Offline"].map(s=>`<option value="${s}" ${state.diskState===s?"selected":""}>${statusNames[s]}</option>`).join("")}</select></div><div class="toolbar-right"><span class="toolbar-label">${count(data.total)} 块物理磁盘</span><button class="button button-primary" data-action="enroll" ${!state.selected.size?"disabled":""}>＋ 批量纳管</button></div></div>${state.diskNode?`<div class="selection-bar"><span>节点筛选：${e(state.diskNode)}</span><button class="button-link" data-action="clear-node">查看全部节点</button></div>`:""}${state.selected.size?`<div class="selection-bar"><span>已选择 ${state.selected.size} 块磁盘 · 每块创建独立存储池</span><button class="button-link" data-action="clear-selection">取消选择</button></div>`:""}${data.items?.length?`<div class="table-scroll"><table><thead><tr><th class="check-column"><input id="select-all" type="checkbox" ${allSelected?"checked":""} ${!available.length?"disabled":""} aria-label="选择本页所有可接管磁盘"></th><th>磁盘 / 设备路径</th><th>物理节点</th><th>容量</th><th>介质</th><th>状态</th><th>操作</th></tr></thead><tbody>${data.items.map(d=>`<tr><td class="check-column"><input class="disk-check" type="checkbox" data-id="${e(d.id)}" ${state.selected.has(String(d.id))?"checked":""} ${d.state!=="Available"?"disabled":""} aria-label="选择 ${e(d.node)} ${e(d.name || d.path)}"></td><td><strong>${e(d.name || d.path.split("/").pop())}</strong><span class="subline mono">${e(d.path)}</span></td><td><strong>${e(d.node)}</strong><span class="subline">${e(d.serial || "无序列号")}</span></td><td>${e(bytes(d.sizeBytes))}</td><td><span class="tag">${e(d.media || "未知")}</span></td><td>${badge(d.state)}</td><td>${rowActions(d.id,"disk")}</td></tr>`).join("")}</tbody></table></div>`:empty("没有匹配的磁盘","调整搜索条件，或等待节点完成磁盘发现。")}${pagination(data)}</section>`;
}
async function nodePage() {
  const data=await api(`/api/nodes?${query()}`);cache(data.items);
  return `<section class="panel table-panel"><div class="toolbar"><div class="toolbar-left"><strong>${count(data.total)} 个物理节点</strong><span class="toolbar-label">按节点隔离设备身份与存储池</span></div><span class="tag">NODE INVENTORY</span></div>${data.items?.length?`<div class="table-scroll"><table><thead><tr><th>物理节点</th><th>可用区</th><th>磁盘</th><th>物理总容量</th><th>已分配</th><th>状态</th><th>操作</th></tr></thead><tbody>${data.items.map(n=>`<tr><td><strong>${e(n.name)}</strong></td><td>${e(n.zone || "未设置")}</td><td>${count(n.disks)} 块</td><td>${e(bytes(n.capacityBytes))}</td><td>${usage(n)}</td><td>${badge(n.state)}</td><td><button class="button-link" data-action="node-disks" data-id="${e(n.name)}">查看磁盘 →</button></td></tr>`).join("")}</tbody></table></div>`:empty("尚未发现物理节点","部署节点 Agent 并为节点设置纳管标签后，磁盘发现结果会显示在此。")}${pagination(data)}</section>`;
}
async function poolPage() {
  const data=await api(`/api/pools?${query()}`); cache(data.items);
  return `<section class="panel table-panel"><div class="toolbar"><div class="toolbar-left"><strong>${count(data.total)} 个存储池</strong><span class="toolbar-label">一块物理盘 · 一个 Volume Group</span></div><a href="#disks" class="button button-primary">＋ 接管磁盘</a></div>${data.items?.length?`<div class="table-scroll"><table><thead><tr><th>存储池</th><th>节点</th><th>总容量</th><th>已分配</th><th>可用容量</th><th>状态</th><th>操作</th></tr></thead><tbody>${data.items.map(p=>`<tr><td><strong>${e(p.name)}</strong><span class="subline mono">${e(p.vgName)}</span>${p.message?`<span class="subline">${e(p.message)}</span>`:""}</td><td>${e(p.node)}</td><td>${e(bytes(p.capacityBytes))}</td><td>${usage(p)}</td><td>${e(bytes(p.availableBytes))}</td><td>${badge(p.state)}</td><td>${p.state==="Ready"?rowActions(p.id,"pool"):`<span class="subtle">等待调谐</span>`}</td></tr>`).join("")}</tbody></table></div>`:empty("创建第一个存储池","从物理磁盘页面选择一块空盘，预览接管计划后执行。",'<a class="button button-secondary" href="#disks">前往物理磁盘 →</a>')}${pagination(data)}</section><div class="notice notice-info" style="margin-top:20px">存储池绑定单个节点。删除 PVC 后，Retain 策略会保留数据与容量占用；释放 PVC 不等于清空磁盘。</div>`;
}
async function volumePage() {
  const data=await api(`/api/volumes?${query()}`); cache(data.items);
  return `<section class="panel table-panel"><div class="toolbar"><div class="toolbar-left"><strong>${count(data.total)} 个持久卷</strong><span class="toolbar-label">CSI 动态置备 · Retain</span></div><button class="button button-primary" data-action="create-volume">＋ 申请 PVC</button></div>${data.items?.length?`<div class="table-scroll"><table><thead><tr><th>PVC / 命名空间</th><th>存储池</th><th>节点</th><th>申请容量</th><th>状态</th><th>操作</th></tr></thead><tbody>${data.items.map(v=>`<tr><td><strong>${e(v.name)}</strong><span class="subline">${e(v.namespace)}${v.management==="external"?" · 外部 Operator · 只读":v.canManage===false?" · 只读":""}</span></td><td><strong>${e(v.pool || v.poolId || (v.status==="Bound" && v.bindingVerified===false ? "归属待核验" : "待分配"))}</strong><span class="subline mono">${e(v.storageClass)}</span></td><td>${e(v.status==="Bound" && v.bindingVerified===false ? "绑定身份待核验" : v.node || "等待调度")}${volumeMessages(v).map(message=>`<span class="subline">${e(message)}</span>`).join("")}</td><td>${e(bytes(v.sizeBytes))}</td><td>${badge(v.status)}</td><td>${["Released","Retained","ReleaseRequested","Releasing","Deleted"].includes(v.status)?`<button class="button-link" data-action="volume-detail" data-id="${e(v.id)}">查看详情</button>`:rowActions(v.id,"volume")}</td></tr>`).join("")}</tbody></table></div>`:empty("还没有持久卷","选择一个就绪的存储池，为业务创建 PVC。",'<button class="button button-secondary" data-action="create-volume">申请第一个 PVC →</button>')}${pagination(data)}</section><div class="notice notice-info" style="margin-top:20px">${state.mode==="demo"?"当前是模拟模式：Bound 表示模拟分配成功，不代表 Kubernetes 已绑定或磁盘已挂载。":"PVC 使用 WaitForFirstConsumer：首次有 Pod 使用 PVC 后，CSI 才会根据节点拓扑创建和挂载卷。"}</div>`;
}
async function eventPage() {
  const data=await api(`/api/events?${query()}`);
  return `<section class="panel table-panel"><div class="toolbar"><div class="toolbar-left"><strong>操作与状态记录</strong><span class="toolbar-label">共 ${count(data.total)} 条</span></div><span class="tag">AUDIT LOG</span></div>${data.items?.length?`<div class="table-scroll"><table><thead><tr><th>时间</th><th>级别</th><th>操作</th><th>节点</th><th>事件详情</th></tr></thead><tbody>${data.items.map(item=>`<tr><td class="mono">${e(date(item.time))}</td><td><span class="badge ${["error","Error","ERROR"].includes(item.level)?"bad":["warning","Warning","WARN"].includes(item.level)?"warn":"good"}">${e(item.level || "info")}</span></td><td><strong>${e(item.action)}</strong></td><td>${e(item.node || "—")}</td><td style="min-width:240px">${e(item.message)}</td></tr>`).join("")}</tbody></table></div>`:empty("还没有审计记录")}${pagination(data)}</section>`;
}

function guidePage() {
  return `<div class="guide-intro"><div><h2>一块磁盘，如何成为业务的持久存储？</h2><p>DiskFleet 负责发现、身份核验与存储池纳管。OpenEBS LVM CSI 根据 PVC 创建逻辑卷，并与 kubelet 协作完成格式化、挂载和扩容。</p></div><span class="tag">PHYSICAL DISK → PV</span></div><div class="guide-grid"><section class="panel guide-card"><h2>从接盘到容器的 6 个步骤</h2><div class="steps"><div class="step"><div><h3>把磁盘接入物理机器</h3><p>通过服务器支持的 SATA / SAS / NVMe 接口或 HBA 接入磁盘。热插拔与总线重扫必须遵循硬件手册；Operator 不会替代硬件驱动。</p></div></div><div class="step"><div><h3>确认 Linux 已识别设备</h3><p>使用只读命令检查设备容量、序列号、文件系统和挂载情况。使用 <code>/dev/disk/by-id/</code> 定位磁盘，避免依赖可能变化的 <code>/dev/sdX</code>。</p><pre>lsblk -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINTS,SERIAL,WWN\nls -l /dev/disk/by-id/\nfindmnt</pre></div></div><div class="step"><div><h3>Agent 发现，管理员选择纳管</h3><p>Agent 将磁盘上报为 PhysicalDisk。系统盘、分区盘、挂载盘、带文件系统或已有存储归属的盘会受保护。在“物理磁盘”中选择可接管磁盘并预览计划。</p></div></div><div class="step"><div><h3>创建独立 LVM 存储池</h3><p>确认后创建 DiskPool；节点 Agent 再次核验序列号、容量与占用状态，并创建 LVM Physical Volume 与 Volume Group。真实写入还需启用节点写入开关。默认一盘一池、厚置备、不自动擦盘。</p></div></div><div class="step"><div><h3>为工作负载申请 PVC</h3><p>选择存储池与申请容量。StorageClass 使用 <code>WaitForFirstConsumer</code>，PVC 等到消费它的 Pod 参与调度后再置备，保证卷与 Pod 位于同一物理节点。</p></div></div><div class="step"><div><h3>由 CSI 格式化并挂载到容器</h3><p>CSI 在 VG 内创建 LV，将文件系统挂载到 kubelet 管理的路径，再提供给容器。原始物理盘无需先挂载到 <code>/mnt</code>；业务通过 Pod 的 <code>volumeMounts</code> 使用卷。</p></div></div></div></section><div><section class="panel guide-card"><h2>两个 PV，含义不同</h2><p><strong>LVM Physical Volume</strong>：通过 <code>pvcreate</code> 纳入 LVM 的物理设备。</p><p style="margin-top:10px"><strong>Kubernetes PersistentVolume</strong>：集群中描述持久存储的 API 对象，由 CSI 驱动创建并绑定 PVC。</p></section><section class="panel guide-card"><h2>规模化管理边界</h2><ul><li>按节点发现与调谐，列表分页读取，批量任务逐项执行并显示结果。</li><li>初版一盘一池，避免普通 LVM 跨盘带来的故障扩散。</li><li>本地 PV 不提供跨节点数据复制。应用高可用需结合副本、备份或复制存储。</li><li>Retain 会保留后端数据与容量，回收物理空间需要独立的人工审查。</li><li>大规模生产部署前仍需测试 API Server、etcd 与控制器压力。</li></ul></section><section class="panel guide-card"><h2>模拟与真实部署</h2><p>模拟模式验证控制台、计划确认、分配、并发容量约束和保留流程，不能证明真实设备已初始化或挂载。真实模式需要有权限的 Linux 节点、Kubernetes 集群与 OpenEBS LVM CSI。</p><p style="margin-top:12px">详细部署、回滚与验收命令见项目 <code>README.md</code> 和 <code>docs/</code>。</p></section><section class="panel guide-card"><h2>参考文档</h2><p><a href="https://openebs.io/docs/user-guides/local-storage-user-guide/local-pv-lvm/configuration/lvm-storageclass-parameters" target="_blank" rel="noopener noreferrer">OpenEBS LVM StorageClass 参数 ↗</a></p><p style="margin-top:8px"><a href="https://kubernetes.io/docs/concepts/storage/storage-classes/" target="_blank" rel="noopener noreferrer">Kubernetes StorageClass 与延迟绑定 ↗</a></p></section></div></div>`;
}

async function render(silent=false) {
  const generation=++state.generation; state.loading=true; $("#refresh-button").disabled=true;
  if (!silent) $("#content").innerHTML='<div class="loading"><span class="spinner"></span> 正在读取存储状态…</div>';
  try {
    const html=state.page==="overview"?await overview():state.page==="nodes"?await nodePage():state.page==="disks"?await diskPage():state.page==="pools"?await poolPage():state.page==="volumes"?await volumePage():state.page==="events"?await eventPage():guidePage();
    if (generation!==state.generation) return;
    const search=$("#disk-search input"), draft=search?.value, editing=search && document.activeElement===search, cursor=search?.selectionStart;
    $("#content").innerHTML=html; $("#connection-error").hidden=true;
    const replacement=$("#disk-search input");
    if(replacement && draft!==undefined){replacement.value=draft;if(editing){replacement.focus();replacement.setSelectionRange(cursor,cursor);}}
    $("#last-updated").textContent=`最近同步 ${new Date().toLocaleTimeString("zh-CN",{hour12:false})}`;
    bindPage();
  } catch (error) {
    if (generation!==state.generation) return;
    $("#connection-error").textContent=error.message; $("#connection-error").hidden=false;
    if (!silent) $("#content").innerHTML=empty("暂时无法读取数据","检查 API 服务与访问凭证，点击刷新重试。");
  } finally { if (generation===state.generation) { state.loading=false; $("#refresh-button").disabled=false; } }
}
function navigate() {
  const requested=location.hash.slice(1); state.page=Object.hasOwn(pages,requested)?requested:"overview";
  state.offset=0; state.records.clear(); nav(); render(); reportEmbedded("route");
}
function bindPage() {
  $("#disk-search")?.addEventListener("submit",event=>{event.preventDefault();state.q=new FormData(event.target).get("q").trim();state.offset=0;render();});
  $("#disk-state")?.addEventListener("change",event=>{state.diskState=event.target.value;state.offset=0;render();});
  $$(".disk-check").forEach(input=>input.addEventListener("change",()=>{
    if (input.checked && state.selected.size>=20) { input.checked=false;toast("每批最多纳管 20 块磁盘，请分批操作。",true);return; }
    if (input.checked) state.selected.set(input.dataset.id,state.records.get(input.dataset.id)); else state.selected.delete(input.dataset.id);
    render(true);
  }));
  $("#select-all")?.addEventListener("change",event=>{
    $$(".disk-check:not(:disabled)").forEach(input=>{if(event.target.checked && state.selected.size<20)state.selected.set(input.dataset.id,state.records.get(input.dataset.id));else if(!event.target.checked)state.selected.delete(input.dataset.id);});
    render(true);
  });
}

function dialog(title, body, eyebrow="DISKFLEET / OPERATION") {
  $("#dialog-title").textContent=title;$("#dialog-eyebrow").textContent=eyebrow;$("#dialog-body").innerHTML=body;
  if (!$("#dialog").open) $("#dialog").showModal();
  $("#dialog-close").disabled=false;
  $$("[data-close]",$("#dialog")).forEach(button=>button.addEventListener("click",()=>$("#dialog").close()));
  $("#dialog input:not([type=checkbox]), #dialog select")?.focus();
}
function formError(error) { const el=$("#form-error");if(el){el.textContent=error.message;el.hidden=false;}else toast(error.message,true); }
function formBusy(form,busy) { $$("button,input,select",form).forEach(el=>{el.disabled=busy;});$("#dialog-close").disabled=busy;$("#dialog").dataset.busy=busy?"true":"false"; }
function errorBox() { return '<div id="form-error" class="form-error" role="alert" hidden></div>'; }
function actions(label, danger=false) { return `<div class="dialog-actions"><button class="button button-secondary" type="button" data-close>取消</button><button class="button ${danger?"button-danger":"button-primary"}" type="submit">${label}</button></div>`; }
function detailRows(rows) { return `<dl class="detail-grid">${rows.map(([label,value])=>`<dt>${e(label)}</dt><dd>${e(value || "—")}</dd>`).join("")}</dl>`; }
function diskDetail(id) {
  const d=state.records.get(id);if(!d)return;
  dialog("磁盘详情",`<div class="dialog-body">${badge(d.state)}${detailRows([["设备",d.name],["节点",d.node],["稳定路径",d.path],["序列号",d.serial],["容量",bytes(d.sizeBytes)],["介质",d.media],["型号",d.model],["存储池",d.poolId]])}${d.reasons?.length?`<div class="notice notice-warning" style="margin-top:20px;margin-bottom:0">${d.reasons.map(e).join("<br>")}</div>`:""}</div><div class="dialog-actions"><button class="button button-secondary" data-close>关闭</button></div>`,"PHYSICAL DISK / IDENTITY");
}
function volumeDetail(id) {
  const v=state.records.get(id);if(!v)return;
  dialog("持久卷详情",`<div class="dialog-body">${badge(v.status)}${volumeMessages(v).map(message=>`<div class="notice notice-warning" style="margin-top:16px">${e(message)}</div>`).join("")}${detailRows([["PVC",`${v.namespace}/${v.name}`],["PV",v.pvName],["存储池",v.pool || v.poolId],["节点",v.node],["StorageClass",v.storageClass],["容量",bytes(v.sizeBytes)],["挂载路径",v.mountPath],["创建时间",date(v.createdAt)],["回收策略",volumeMessages(v).length?"待核验（见提示）":v.management==="external"?"请查看实际 PV 策略":"Retain（保留后端数据）"],["管理归属",v.management==="external"?"外部 Operator（仅观察，请通过所属管理资源操作）":"DiskFleet"],["所有者",(v.owners || []).map(owner=>`${owner.kind}/${owner.name}`).join(", ")]])}${state.mode==="demo"?'<div class="notice notice-info" style="margin-top:20px;margin-bottom:0">模拟文件目录不是 Kubernetes CSI 挂载点。</div>':""}</div><div class="dialog-actions"><button class="button button-secondary" data-close>关闭</button></div>`,"PERSISTENT VOLUME / DETAILS");
}
function tokenDialog() {
  dialog("访问凭证",`<form id="token-form"><div class="dialog-body">${errorBox()}<div class="form-field"><label for="api-token">API Bearer Token</label><input id="api-token" name="token" class="input" type="password" autocomplete="off" placeholder="输入服务端配置的 API_TOKEN"><p class="help">凭证仅存储在当前浏览器标签页的 sessionStorage。${state.tokenRequired?"当前集群模式要求凭证。":"当前模式无需凭证时可留空。"}</p></div><p class="form-help">如通过远程网络访问，请使用 HTTPS 或 SSH 隧道。</p></div>${actions("保存并连接")}</form>`,"ACCESS / SESSION TOKEN");
  $("#token-form").addEventListener("submit",event=>{event.preventDefault();try{saveToken(new FormData(event.target).get("token").trim());$("#dialog").close();render();}catch(error){formError(error);}});
}

function enroll() {
  const selected=[...state.selected.values()];if(!selected.length)return;
  dialog("预览磁盘纳管计划",`<form id="plan-form"><div class="dialog-body">${errorBox()}<div class="notice notice-info">选择 ${selected.length} 块磁盘，将逐盘创建独立存储池。此步骤只生成计划，不写入磁盘。</div><div class="form-field"><label for="pool-prefix">存储池名称前缀</label><input id="pool-prefix" class="input" name="prefix" value="pool" pattern="[a-z][a-z0-9-]{0,30}[a-z0-9]|[a-z]" maxlength="32" required><p class="help">小写字母开头，可使用数字和连字符。系统自动追加设备标识。</p></div>${selected.map(d=>`<div class="plan-item"><h3>${e(d.node)} / ${e(d.name || d.path)}</h3><p>${e(d.path)} · ${e(bytes(d.sizeBytes))} · ${e(d.serial)}</p></div>`).join("")}</div>${actions("生成接管计划 →")}</form>`,"ENROLLMENT / PREVIEW");
  $("#plan-form").addEventListener("submit",async event=>{
    event.preventDefault();const form=event.target,prefix=new FormData(form).get("prefix").trim();formBusy(form,true);
    try {
      const plans=[];
      for (const [index,d] of selected.entries()) {
        const suffix=String(d.id).toLowerCase().replace(/[^a-z0-9]/g,"").slice(-8)||String(index+1);
        const name=`${prefix}-${index+1}-${suffix}`;
        const plan=await api("/api/pools/plan",{method:"POST",body:{name,node:d.node,diskIds:[d.id]}});plans.push({plan,disk:d,name});
      }
      formBusy(form,false);confirmPlans(plans);
    } catch(error){formError(error);formBusy(form,false);}
  });
}
function confirmPlans(plans) {
  const isDemo=state.mode==="demo";
  dialog("确认并执行纳管",`<form id="execute-plans"><div class="dialog-body">${errorBox()}<div class="notice ${isDemo?"notice-info":"notice-warning"}">${isDemo?"模拟模式：执行只更新演示数据，不会初始化或格式化真实磁盘。":"真实集群：执行将创建 DiskPool，节点 Agent 在写入开关开启时初始化选定空盘。请逐项核对设备身份。"}</div>${plans.map(({plan,disk,name},i)=>`<div class="plan-item"><h3>${e(name)} · ${e(disk.node)}</h3><p>${e(disk.path)}<br>序列号 ${e(disk.serial)} · ${e(bytes(disk.sizeBytes))}</p><pre>${e((plan.commands || []).map(cmd=>Array.isArray(cmd)?cmd.join(" "):cmd).join("\n"))}</pre>${(plan.warnings || []).map(w=>`<p style="margin-top:8px">${e(w)}</p>`).join("")}<p style="margin-top:8px">计划有效期至 ${e(date(plan.expiresAt))}</p><div id="batch-${i}" class="batch-status">等待确认</div></div>`).join("")}<label class="checkbox-label"><input type="checkbox" id="confirm-disks" required><span>我已核对设备身份与容量，确认接管以上 ${plans.length} 块${isDemo?"模拟":"空"}磁盘。</span></label></div>${actions(isDemo?"执行模拟纳管":"确认接管这些磁盘")}</form>`,"ENROLLMENT / REVIEW & EXECUTE");
  $("#execute-plans").addEventListener("submit",async event=>{
    event.preventDefault();const form=event.target;formBusy(form,true);let succeeded=0,failed=0;
    for(const [index,{plan,disk}] of plans.entries()){
      const status=$(`#batch-${index}`);status.textContent="正在提交…";
      try{const result=await api("/api/pools",{method:"POST",body:{planId:plan.planId,confirmation:plan.confirmation}});status.textContent=`✓ ${result.state==="Ready"?"存储池已就绪":"已提交，等待节点调谐"}`;state.selected.delete(String(disk.id));succeeded++;}
      catch(error){status.textContent=`✕ ${error.message}`;status.style.color="#b36a53";failed++;}
    }
    $("#dialog").dataset.busy="false";$("#dialog-close").disabled=false;
    const footer=$(".dialog-actions",form);footer.innerHTML=`<span class="form-help" style="margin-right:auto">成功 ${succeeded} / 失败 ${failed}</span><button type="button" class="button button-primary" id="batch-done">完成</button>`;
    $("#batch-done").addEventListener("click",()=>{$("#dialog").close();render();});
    toast(`纳管任务完成：成功 ${succeeded}，失败 ${failed}`,failed>0);
  });
}

async function createVolume(poolId) {
  let pools;
  try{pools=poolId?[state.records.get(poolId)].filter(Boolean):(await api("/api/pools?limit=100&offset=0")).items.filter(p=>p.state==="Ready");}
  catch(error){toast(error.message,true);return;}
  if (!pools.length) {dialog("申请持久卷",`<div class="dialog-body">${empty("没有可用的存储池","先纳管磁盘并等待存储池进入就绪状态。",'<a href="#disks" class="button button-primary" data-close>前往物理磁盘 →</a>')}</div>`);return;}
  dialog("申请持久卷",`<form id="volume-form"><div class="dialog-body">${errorBox()}<div class="form-grid"><div class="form-field"><label for="volume-name">PVC 名称</label><input id="volume-name" class="input" name="name" placeholder="例如 postgres-data" pattern="[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" maxlength="63" required></div><div class="form-field"><label for="volume-namespace">命名空间</label><input id="volume-namespace" class="input" name="namespace" value="default" pattern="[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" maxlength="63" required></div></div><div class="form-field"><label for="volume-pool">存储池</label><select id="volume-pool" class="input" name="poolId">${pools.map(p=>`<option value="${e(p.id)}">${e(p.name)} · ${e(p.node)} · 可用 ${e(bytes(p.availableBytes))}</option>`).join("")}</select>${poolId?"":'<p class="help">展示前 100 个池中的就绪池。也可从存储池列表的对应行直接申请。</p>'}</div><div class="form-field"><label for="volume-size">申请容量（GiB）</label><input id="volume-size" class="input" type="number" name="sizeGiB" value="10" min="1" step="1" required></div><div class="notice notice-info" style="margin-bottom:0">${state.mode==="demo"?"模拟模式会立即分配文件目录，不创建真实 PV。":"采用 WaitForFirstConsumer，创建使用该 PVC 的 Pod 后才会绑定卷。命名空间需已存在。"} 默认 Retain，删除 PVC 后保留数据。</div></div>${actions("创建 PVC")}</form>`,"PROVISION / PERSISTENT VOLUME CLAIM");
  $("#volume-form").addEventListener("submit",async event=>{event.preventDefault();const form=event.target,data=new FormData(form);formBusy(form,true);try{await api("/api/volumes",{method:"POST",body:{name:data.get("name").trim(),namespace:data.get("namespace").trim(),poolId:data.get("poolId"),sizeGiB:Number(data.get("sizeGiB"))}});formBusy(form,false);$("#dialog").close();toast("PVC 申请已提交");if(state.page!=="volumes")location.hash="volumes";else render();}catch(error){formError(error);formBusy(form,false);}});
}
function expandVolume(id) {
  const v=state.records.get(id);if(!v)return;if(v.canManage===false){toast(volumeReadOnlyReason(v),true);return;}const current=Math.ceil(Number(v.sizeBytes)/1024**3);
  dialog("扩容持久卷",`<form id="expand-form"><div class="dialog-body">${errorBox()}${detailRows([["PVC",`${v.namespace}/${v.name}`],["当前容量",bytes(v.sizeBytes)],["存储池",v.pool || v.poolId]])}<div class="form-field" style="margin-top:20px"><label for="new-size">目标总容量（GiB）</label><input class="input" type="number" name="sizeGiB" id="new-size" min="${current+1}" step="1" value="${current+10}" required><p class="help">填写扩容后的总容量。只支持增大，容量受存储池可用空间限制。</p></div>${state.mode!=="demo"?'<div class="notice notice-info" style="margin-bottom:0">CSI 异步完成逻辑卷与文件系统扩容，请等待 PVC 状态更新。</div>':""}</div>${actions("提交扩容")}</form>`,"EXPANSION / GROW VOLUME");
  $("#expand-form").addEventListener("submit",async event=>{event.preventDefault();const form=event.target,sizeGiB=Number(new FormData(form).get("sizeGiB"));formBusy(form,true);try{await api(`/api/volumes/${encodeURIComponent(id)}/expand`,{method:"POST",body:{sizeGiB}});formBusy(form,false);$("#dialog").close();toast("扩容请求已提交");render();}catch(error){formError(error);formBusy(form,false);}});
}
function releaseVolume(id) {
  const v=state.records.get(id);if(!v)return;if(v.canManage===false){toast(volumeReadOnlyReason(v),true);return;}
  dialog("释放 PVC，保留数据",`<form id="release-form"><div class="dialog-body">${errorBox()}${detailRows([["PVC",`${v.namespace}/${v.name}`],["容量",bytes(v.sizeBytes)]])}<div class="notice notice-warning" style="margin-top:20px;margin-bottom:0">${state.mode==="demo"?"卷将标记为 Released。":"将删除 PVC，请先停止使用它的业务 Pod；被使用的 PVC 可能等待保护机制解除。"} Retain 策略保留后端卷与数据，存储池容量不会因此释放。</div><label class="checkbox-label"><input type="checkbox" required><span>确认释放此 PVC，并保留后端数据。</span></label></div>${actions("释放并保留",true)}</form>`,"RECLAIM / RETAIN DATA");
  $("#release-form").addEventListener("submit",async event=>{event.preventDefault();const form=event.target;formBusy(form,true);try{await api(volumeReleasePath(v),{method:"DELETE"});formBusy(form,false);$("#dialog").close();toast("释放请求已提交，后端数据保留");render();}catch(error){formError(error);formBusy(form,false);}});
}

$("#content").addEventListener("click",event=>{
  const button=event.target.closest("[data-action]");if(!button || button.disabled)return;
  const id=button.dataset.id,action=button.dataset.action;
  if(action==="prev"){state.offset=Math.max(0,state.offset-state.limit);render();}
  else if(action==="next"){state.offset+=state.limit;render();}
  else if(action==="disk-detail")diskDetail(id);
  else if(action==="node-disks"){state.diskNode=id;state.q="";state.diskState="";location.hash="disks";}
  else if(action==="clear-node"){state.diskNode="";state.offset=0;render();}
  else if(action==="volume-detail")volumeDetail(id);
  else if(action==="create-volume")createVolume(id);
  else if(action==="expand-volume")expandVolume(id);
  else if(action==="release-volume")releaseVolume(id);
  else if(action==="enroll")enroll();
  else if(action==="clear-selection"){state.selected.clear();render(true);}
});
$("#dialog-close").addEventListener("click",()=>$("#dialog").close());
$("#dialog").addEventListener("cancel",event=>{if($("#dialog").dataset.busy==="true")event.preventDefault();});
$("#dialog").addEventListener("close",()=>{$("#dialog").dataset.busy="false";$("#dialog-close").disabled=false;});
$("#token-button").addEventListener("click",tokenDialog);
$("#refresh-button").addEventListener("click",()=>{const search=$("#disk-search input");if(search && search.value.trim()!==state.q){state.q=search.value.trim();state.offset=0;}render(true);});
window.addEventListener("hashchange",navigate);
function refreshTimer() {
  clearInterval(state.timer);const seconds=Number($("#auto-refresh").value);
  if(seconds)state.timer=setInterval(()=>{if(!state.loading && !$("#dialog").open && !document.hidden && !["INPUT","SELECT","TEXTAREA"].includes(document.activeElement?.tagName) && state.page!=="guide")render(true);},seconds*1000);
}
$("#auto-refresh").addEventListener("change",refreshTimer);
async function boot() {
  try {
    const config=await api("/api/config");
    if (!config || !["demo","cluster"].includes(config.mode) || typeof config.tokenRequired !== "boolean") throw new Error("存储 API 配置无效，请检查服务路由。");
    state.mode=config.mode;state.tokenRequired=config.tokenRequired;storageReady=true;
    const demo=config.mode==="demo";$("#mode").textContent=demo?"DEMO · 模拟环境":"CLUSTER · 真实集群";$("#mode").classList.toggle("demo",demo);
    if(demo){$("#mode-notice").textContent="当前为本地模拟环境。可体验纳管、容量分配与保留流程；页面数据和挂载路径不代表真实物理盘或 Kubernetes PV。";$("#mode-notice").hidden=false;}
  }catch(error){storageReady=false;$("#mode").textContent="连接异常";toast(error.message,true);}
  storageInitialized=true;navigate();refreshTimer();reportEmbedded("ready");
}
boot();
