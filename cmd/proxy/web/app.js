// =============================================
// PacketLab v2.0 — Frontend Application
// =============================================

// ── i18n ──────────────────────────────────────
const LOCALE = {
  zh: { recording: "录制中", paused: "已暂停", request_list: "请求列表", filter_all: "全部",
    filter_placeholder: "过滤 URL / 状态码 / 方法...", select_request: "选择一个请求查看详情",
    select_request_hint: "从左侧请求列表点击任意条目，即可查看请求/响应详情、编辑并重发",
    tab_request: "请求", tab_response: "响应", tab_resend: "重发", tab_apimap: "API地图",
    overview: "概要", req_headers: "请求头", req_body: "请求体", res_headers: "响应头", res_body: "响应体",
    edit_request: "编辑请求", req_headers_mini: "请求头", add_header: "+ 添加 Header", send: "发送",
    copy: "复制", api_map: "API 地图", select_host: "选择站点...", refresh: "刷新",
    no_requests: "暂无匹配的请求", theme_toggle: "切换主题", lang_toggle: "切换语言",
    error_filter: "仅显示错误", clear_history: "清空记录",
    confirm_clear: "确定要清空所有捕获记录？此操作不可撤销。", cleared: "记录已清空",
    copied: "已复制到剪贴板", sent: "请求已发送", send_failed: "发送失败",
    recordings: "条记录", requests: "请求", errors: "错误", empty: "(空)",
    edit_note: "编辑接口备注", delete_note: "删除备注", cancel: "取消", save_note: "保存",
    loading: "加载中..." },
  en: { recording: "Recording", paused: "Paused", request_list: "Requests", filter_all: "All",
    filter_placeholder: "Filter URL / Status / Method...", select_request: "Select a request to inspect",
    select_request_hint: "Click any entry from the request list to view request/response details, edit and resend",
    tab_request: "Request", tab_response: "Response", tab_resend: "Resend", tab_apimap: "API Map",
    overview: "Overview", req_headers: "Request Headers", req_body: "Request Body",
    res_headers: "Response Headers", res_body: "Response Body", edit_request: "Edit Request",
    req_headers_mini: "Headers", add_header: "+ Add Header", send: "Send", copy: "Copy",
    api_map: "API Map", select_host: "Select host...", refresh: "Refresh",
    no_requests: "No matching requests", theme_toggle: "Toggle theme", lang_toggle: "Switch language",
    error_filter: "Errors only", clear_history: "Clear history",
    confirm_clear: "Clear all captured requests? This cannot be undone.", cleared: "History cleared",
    copied: "Copied to clipboard", sent: "Request sent", send_failed: "Send failed",
    recordings: "records", requests: "requests", errors: "errors", empty: "(empty)",
    edit_note: "Edit API Note", delete_note: "Delete Note", cancel: "Cancel", save_note: "Save",
    loading: "Loading..." }
};

let lang = localStorage.getItem('packetlab_lang') || 'zh';
function t(key) { return (LOCALE[lang] && LOCALE[lang][key]) || (LOCALE.en && LOCALE.en[key]) || key; }
function applyLang() {
  lang = localStorage.getItem('packetlab_lang') || 'zh';
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  document.querySelectorAll('[data-i18n]').forEach(el => {
    const k = el.dataset.i18n;
    if (el.tagName === 'INPUT' && el.hasAttribute('placeholder')) {
      el.placeholder = t(el.dataset.i18nPlaceholder || k);
    } else { el.textContent = t(k); }
  });
  document.querySelectorAll('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
  document.querySelectorAll('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
  document.getElementById('langBtn').textContent = lang === 'zh' ? 'EN' : '中文';
}
function toggleLang() { lang = lang === 'zh' ? 'en' : 'zh'; localStorage.setItem('packetlab_lang', lang); applyLang(); }

// ── Theme ────────────────────────────────────
let theme = localStorage.getItem('packetlab_theme') || 'dark';
function applyTheme() { document.documentElement.setAttribute('data-theme', theme); }
function toggleTheme() { theme = theme === 'dark' ? 'light' : 'dark'; localStorage.setItem('packetlab_theme', theme); applyTheme(); }
applyTheme();

// ── State ────────────────────────────────────
const API_BASE = '';
let requests = [];
let requestDetailCache = {};
let requestElCache = new Map(); // id → { el, sizeSpan, durSpan } DOM 缓存
let selectedRequestId = null;
let activeTab = 'request';
let currentFilter = 'all', currentHost = '', errorFilterOnly = false, starredOnly = false, isRecording = true;

// ── 刷新模式 ──────────────────────────────────
// refreshMode: 'realtime' = WebSocket 实时追加（无分页）
//              '1s'/'3s'/'5s'/'15s'/'30s'/'60s' = 定时整页刷新（分页生效）
//              'manual' = 仅手动刷新
let refreshMode = 'realtime';
let refreshTimer = null;
const REFRESH_INTERVALS = { '1s': 1000, '3s': 3000, '5s': 5000, '15s': 15000, '30s': 30000, '60s': 60000 };

// ── 分页 ──────────────────────────────────────
const PAGE_SIZE = 50;
let currentPage = 1;       // 1-based
let pageTotal = 0;         // 当前过滤条件下的总条数（由后端返回）
// isPaged: 是否启用分页（非 realtime 模式启用）
let ws = null, wsReconnectTimer = null;
let requestVersion = 0;

// ── 虚拟滚动 ──────────────────────────────────
const VIRTUAL_ITEM_HEIGHT = 50;   // 每个列表项高度（与 CSS padding 对齐）
const VIRTUAL_OVERSCAN = 8;        // 视口外额外渲染的项数（减少滚动时空白）
const VIRTUAL_THRESHOLD = 200;     // 超过此数量启用虚拟滚动
let virtualFiltered = [];          // 当前过滤后的列表（虚拟滚动用）
let virtualScrollTop = 0;          // 当前滚动位置
let virtualRafPending = false;     // rAF 节流标记

// ── API Client ───────────────────────────────
const ERROR_MESSAGES = {
  NOT_FOUND: '请求的资源不存在',
  VALIDATION_ERROR: '输入参数有误，请检查后重试',
  METHOD_NOT_ALLOWED: '请求方法不被允许',
  INTERNAL_ERROR: '服务器内部错误，请稍后重试',
  SERVICE_UNAVAILABLE: '服务暂不可用，请稍后重试',
  BAD_GATEWAY: '上游服务不可达，请检查目标地址',
  RATE_LIMITED: '请求过于频繁，请稍后重试',
  CONFLICT: '操作冲突，请刷新后重试',
};

function getErrorMessage(body, status) {
  if (body && body.code && ERROR_MESSAGES[body.code]) return ERROR_MESSAGES[body.code];
  if (body && body.message) return body.message;
  if (body && body.error) return body.error;
  if (status === 404) return '请求的资源不存在';
  if (status === 429) return '请求过于频繁，请稍后重试';
  if (status >= 500) return '服务器错误，请稍后重试';
  if (status >= 400) return '请求参数有误';
  return '网络请求失败';
}

async function apiRequest(path, options) {
  try {
    const r = await fetch(API_BASE + path, options);
    if (!r.ok) {
      let body = null;
      try { body = await r.json(); } catch {}
      const msg = getErrorMessage(body, r.status);
      showToast('error', msg);
      const err = new Error(msg);
      err.code = body?.code || '';
      err.status = r.status;
      err.requestId = body?.request_id || '';
      throw err;
    }
    if (r.status === 204) return null;
    return r.json();
  } catch (e) {
    if (e instanceof TypeError && e.message.includes('fetch')) {
      showToast('error', '无法连接服务器，请检查网络');
    }
    throw e;
  }
}

async function apiGet(p) {
  return apiRequest(p);
}
async function apiPost(p, b) {
  return apiRequest(p, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b) });
}
async function apiDelete(p) {
  return apiRequest(p, { method: 'DELETE' });
}
async function apiPut(p, b) {
  return apiRequest(p, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b) });
}

// ── Data Loading ─────────────────────────────
async function loadRequests() {
  try {
    const p = new URLSearchParams();
    if (currentFilter !== 'all') p.set('method', currentFilter);
    if (errorFilterOnly) p.set('error_only', 'true');
    const s = document.getElementById('searchInput').value.trim();
    if (s) p.set('search', s);
    if (currentHost) p.set('host', currentHost);
    // 分页模式：传 offset + limit（页面大小）；实时模式：拉取较大窗口供虚拟滚动
    const paged = isPaged();
    if (paged) {
      p.set('limit', String(PAGE_SIZE));
      p.set('offset', String((currentPage - 1) * PAGE_SIZE));
    } else {
      p.set('limit', '200');
    }
    const r = await apiGet('/api/requests?' + p.toString());
    requestVersion++;
    requests = (r.data || []).map(normalizeReq);
    pageTotal = r.total || 0;
    // 过滤/重新加载后回到顶部，避免虚拟滚动残留位置错位
    virtualScrollTop = 0;
    const listEl = document.getElementById('requestList');
    if (listEl) listEl.scrollTop = 0;
    renderRequestList();
    updatePagination();
  } catch (e) { console.error('loadRequests failed:', e); }
}

// 是否启用分页：仅当刷新模式不是 realtime 时启用
function isPaged() {
  return refreshMode !== 'realtime';
}

// 切换刷新模式
function setRefreshMode(mode) {
  refreshMode = mode;
  const sel = document.getElementById('refreshModeSelect');
  if (sel) sel.value = mode;
  // 清理旧的定时器
  if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
  // 模式切换时分页语义重置
  if (mode === 'realtime') {
    // realtime: 回到不分页的大窗口加载
    currentPage = 1;
    loadRequests();
  } else {
    // 定时 / 手动：启用分页，回到第 1 页
    currentPage = 1;
    loadRequests();
    const ms = REFRESH_INTERVALS[mode];
    if (ms) {
      refreshTimer = setInterval(() => { if (document.visibilityState !== 'hidden') loadRequests(); }, ms);
    }
  }
  updateRefreshUI();
}

// 手动刷新按钮
function manualRefresh() {
  loadRequests();
  // 视觉反馈：按钮短暂高亮
  const btn = document.getElementById('manualRefreshBtn');
  if (btn) {
    btn.classList.add('spin');
    setTimeout(() => btn.classList.remove('spin'), 400);
  }
}

function updateRefreshUI() {
  const btn = document.getElementById('manualRefreshBtn');
  if (btn) btn.style.display = (refreshMode === 'manual') ? '' : 'none';
}

// ── 分页控制 ──────────────────────────────────
function updatePagination() {
  const bar = document.getElementById('paginationBar');
  if (!bar) return;
  if (!isPaged()) { bar.style.display = 'none'; return; }
  const totalPages = Math.max(1, Math.ceil(pageTotal / PAGE_SIZE));
  if (currentPage > totalPages) currentPage = totalPages;
  bar.style.display = '';
  const info = document.getElementById('pageInfo');
  if (info) info.textContent = `${currentPage} / ${totalPages}`;
  const prev = document.getElementById('pagePrev');
  const next = document.getElementById('pageNext');
  if (prev) prev.disabled = (currentPage <= 1);
  if (next) next.disabled = (currentPage >= totalPages);
}

function goToPage(delta) {
  const totalPages = Math.max(1, Math.ceil(pageTotal / PAGE_SIZE));
  const np = currentPage + delta;
  if (np < 1 || np > totalPages) return;
  currentPage = np;
  loadRequests();
}

async function loadRequestDetail(id) {
  if (requestDetailCache[id]) return requestDetailCache[id];
  try { const d = await apiGet('/api/requests/' + id); requestDetailCache[id] = d; return d; }
  catch (e) { console.warn('loadRequestDetail failed for', id, e); return null; }
}

async function loadStats() {
  try {
    const s = await apiGet('/api/stats');
    document.getElementById('totalRequests').textContent = `${s.total || 0} ${t('requests')}`;
    document.getElementById('totalSize').textContent = formatSize(s.total_size || 0);
    document.getElementById('errorCount').textContent = `${s.errors || 0} ${t('errors')}`;
  } catch (e) { /* non-critical */ }
}

function normalizeReq(r) {
  return {
    id: r.id, method: r.method, url: r.url, host: r.host,
    status: r.status_code, status_code: r.status_code,
    duration_ms: r.duration_ms, size_bytes: r.size_bytes,
    time: `${r.duration_ms}ms`, size: formatSize(r.size_bytes),
    timestamp: r.captured_at ? new Date(r.captured_at).getTime() : Date.now(),
    captured_at: r.captured_at, is_https: r.is_https,
    protocol: r.is_https ? 'HTTPS/1.1' : 'HTTP/1.1',
    reqHeaders: r.req_headers || {}, reqBody: r.req_body,
    resHeaders: r.res_headers || {}, resBody: r.res_body,
    // 保留 starred 字段，避免 loadStarred/loadRequests 后二次过滤丢失收藏项
    starred: !!r.starred
  };
}

function formatSize(b) {
  if (!b || b === 0) return '0 B';
  if (b < 1024) return b + ' B';
  if (b < 1048576) return (b / 1024).toFixed(1) + ' KB';
  return (b / 1048576).toFixed(1) + ' MB';
}

// SSE 等流式响应的增量更新：更新列表中已有请求的 size/duration
function updateRequestInList(data) {
  const idx = requests.findIndex(r => r.id === data.id);
  if (idx < 0) return;
  const r = requests[idx];
  if (data.size_bytes !== undefined) { r.size_bytes = data.size_bytes; r.size = formatSize(data.size_bytes); }
  if (data.duration_ms !== undefined) { r.duration_ms = data.duration_ms; r.time = `${data.duration_ms}ms`; }
  if (data.status_code !== undefined) { r.status = data.status_code; r.status_code = data.status_code; }
  // SSE 流式内容增量更新：同步到缓存对象
  if (data.is_sse !== undefined) { r.is_sse = data.is_sse; }
  let bodyChanged = false;
  if (data.res_body !== undefined) { r.resBody = data.res_body; bodyChanged = true; }
  if (data.sse_events !== undefined) { r.sseEvents = data.sse_events; bodyChanged = true; }
  // 优先从 DOM 缓存 Map 获取，O(1) 查找
  const cached = requestElCache.get(r.id);
  if (cached) {
    if (cached.sizeSpan) cached.sizeSpan.textContent = r.size;
    if (cached.durSpan) cached.durSpan.textContent = r.time;
  } else {
    // 回退到 querySelector（首次渲染后尚未缓存）
    const el = document.querySelector(`[data-id="${r.id}"]`);
    if (el) {
      const sizeSpan = el.querySelector('.item-size');
      const durSpan = el.querySelector('.item-duration');
      requestElCache.set(r.id, { el, sizeSpan, durSpan });
      if (sizeSpan) sizeSpan.textContent = r.size;
      if (durSpan) durSpan.textContent = r.time;
    }
  }
  // 若当前正查看该请求且 body 有更新，实时刷新响应体（SSE 场景）
  if (bodyChanged && String(selectedRequestId) === String(r.id)) {
    const bodyEl = document.getElementById('res-body-content');
    if (bodyEl) bodyEl.textContent = formatJSONBody(r.resBody) || t('empty');
    const sizeEl = document.getElementById('res-size');
    if (sizeEl && r.size) sizeEl.textContent = r.size;
    const timeEl = document.getElementById('res-time');
    if (timeEl && r.time) timeEl.textContent = r.time;
  }
}

// ── WebSocket ────────────────────────────────
function connectWebSocket() {
  if (ws && ws.readyState === WebSocket.OPEN) return;
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  try {
    ws = new WebSocket(`${protocol}//${location.host}/ws`);
    ws.onopen = () => { if (wsReconnectTimer) { clearTimeout(wsReconnectTimer); wsReconnectTimer = null; } };
    ws.onmessage = (e) => {
      try {
        const m = JSON.parse(e.data);
        if (m.type === 'new_request' && m.data) {
          // 仅 realtime 模式实时追加；定时/手动模式由刷新或翻页统一加载
          if (refreshMode !== 'realtime') return;
          requestVersion++; requests.unshift(normalizeReq(m.data));
          // 虚拟滚动模式：保持视口稳定，新条目加到顶部时同步下移滚动位置
          if (virtualFiltered.length > VIRTUAL_THRESHOLD) {
            const list = document.getElementById('requestList');
            virtualScrollTop = list.scrollTop += VIRTUAL_ITEM_HEIGHT;
          }
          renderRequestList();
        }
        if (m.type === 'intercept_request' && m.data) { addPendingToList(m.data); }
        if (m.type === 'update_request' && m.data) { updateRequestInList(m.data); }
      } catch { /* ignore parse errors */ }
    };
    ws.onclose = () => { wsReconnectTimer = setTimeout(connectWebSocket, 3000); };
    ws.onerror = () => ws.close();
  } catch { wsReconnectTimer = setTimeout(connectWebSocket, 3000); }
}

// =============================================
// 微交互引擎 — Micro-Interaction Engine
// =============================================

/**
 * 列表项点击波纹效果 (Ripple)
 * GPU-only: transform + opacity
 */
function createRipple(e, el) {
  const ripple = document.createElement('span');
  ripple.className = 'item-ripple';
  const rect = el.getBoundingClientRect();
  const size = Math.max(rect.width, rect.height) * 2;
  ripple.style.width = ripple.style.height = size + 'px';
  ripple.style.left = (e.clientX - rect.left - size / 2) + 'px';
  ripple.style.top = (e.clientY - rect.top - size / 2) + 'px';
  el.appendChild(ripple);
  ripple.addEventListener('animationend', () => ripple.remove(), { once: true });
}

/**
 * 内容渐入 (Fade In)
 * GPU-only: opacity + transform
 */
function animateContentIn(container, delay) {
  container.style.opacity = '0';
  container.style.transform = 'translateY(6px)';
  container.style.transition = 'none';
  void container.offsetWidth; // force reflow
  container.style.transition = `opacity .28s cubic-bezier(.16,1,.3,1) ${delay}ms, transform .28s cubic-bezier(.16,1,.3,1) ${delay}ms`;
  container.style.opacity = '1';
  container.style.transform = 'translateY(0)';
}

// =============================================
// Rendering — 请求列表
// =============================================

function renderRequestList() {
  const list = document.getElementById('requestList');
  virtualFiltered = requests.filter(r => {
    if (currentFilter !== 'all' && r.method !== currentFilter) return false;
    if (errorFilterOnly && r.status < 400) return false;
    if (starredOnly && !r.starred) return false;
    const q = document.getElementById('searchInput').value.toLowerCase();
    if (q) { return r.url.toLowerCase().includes(q) || r.method.toLowerCase().includes(q) || String(r.status).includes(q); }
    return true;
  });
  const filtered = virtualFiltered;

  if (filtered.length === 0) {
    list.innerHTML = `<div class="request-list-empty">
      <svg width="36" height="36" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.2" opacity=".4"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>
      <span style="font-size:13px">${t('no_requests')}</span></div>`;
    // innerHTML 替换后旧 DOM 缓存失效，清空让 updateRequestInList 重新建立
    requestElCache.clear();
    document.getElementById('requestCount').textContent = `${listCount()} ${t('recordings')}`;
    loadStats();
    return;
  }

  // 超过阈值启用虚拟滚动，避免大量 DOM 卡顿
  if (filtered.length > VIRTUAL_THRESHOLD) {
    renderVirtualList();
  } else {
    list.innerHTML = filtered.map((r, i) => {
      const sc = r.is_pending ? '' : `status-${Math.floor(r.status / 100)}xx`;
      const ts = r.captured_at ? new Date(r.captured_at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '';
      const pcls = r.is_pending ? ' pending' : '';
      const pendingExtra = r.is_pending ? '<span class="pending-tag">PENDING</span>' : '';
      const isActive = String(selectedRequestId) === String(r.id) ? ' active' : '';
      return `<div class="request-item${pcls}${isActive}" data-id="${r.id}">
        <span class="method-badge method-${escAttr(r.method)}">${esc(r.method)}</span>
        <span class="status-code ${sc}">${r.is_pending ? '—' : r.status}</span>
        <div class="request-info">
          <span class="request-url">${esc(r.url)}</span>
          <div class="request-meta"><span>${esc(r.host)}</span>${r.process_name ? `<span style="color:var(--accent)">🐧 ${esc(r.process_name)}</span>` : ''}${r.capture_mode === 'nic' ? '<span style="color:var(--accent)">NIC</span>' : ''}<span class="item-duration">${r.time}</span><span class="item-size">${r.size}</span></div>
        </div>
        ${pendingExtra}
        <span class="request-arrow"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="m9 18 6-6-6-6"/></svg></span>
      </div>`;
    }).join('');
    // innerHTML 替换后旧 DOM 缓存失效，清空让 updateRequestInList 重新建立
    requestElCache.clear();
  }
  document.getElementById('requestCount').textContent = `${listCount()} ${t('recordings')}`;
  loadStats();
}

// 列表计数：分页模式显示后端 total，否则显示本地条数
function listCount() {
  if (isPaged() && pageTotal > 0) return pageTotal;
  return requests.length;
}

// 渲染单个请求项的 HTML（虚拟滚动与全量渲染共用）
function renderItemHTML(r, i) {
  const sc = r.is_pending ? '' : `status-${Math.floor(r.status / 100)}xx`;
  const pcls = r.is_pending ? ' pending' : '';
  const pendingExtra = r.is_pending ? '<span class="pending-tag">PENDING</span>' : '';
  const isActive = String(selectedRequestId) === String(r.id) ? ' active' : '';
  const starIcon = r.starred
    ? '<span class="star-btn starred" onclick="event.stopPropagation();toggleStar(' + r.id + ',true)" title="取消收藏">★</span>'
    : '<span class="star-btn" onclick="event.stopPropagation();toggleStar(' + r.id + ',false)" title="收藏">☆</span>';
  return `<div class="request-item${pcls}${isActive}" data-id="${r.id}">
    <span class="method-badge method-${escAttr(r.method)}">${esc(r.method)}</span>
    <span class="status-code ${sc}">${r.is_pending ? '—' : r.status}</span>
    <div class="request-info">
      <span class="request-url">${esc(r.url)}</span>
      <div class="request-meta"><span>${esc(r.host)}</span>${r.process_name ? `<span style="color:var(--accent)">🐧 ${esc(r.process_name)}</span>` : ''}${r.capture_mode === 'nic' ? '<span style="color:var(--accent)">NIC</span>' : ''}<span class="item-duration">${r.time}</span><span class="item-size">${r.size}</span></div>
    </div>
    ${starIcon}
    ${pendingExtra}
    <span class="request-arrow"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="m9 18 6-6-6-6"/></svg></span>
  </div>`;
}

// 虚拟滚动：用顶部 spacer 撑起总高度，只渲染 [start, end) 区间的项
function renderVirtualList() {
  const list = document.getElementById('requestList');
  const filtered = virtualFiltered;
  const total = filtered.length;
  const viewH = list.clientHeight || 600;
  const start = Math.max(0, Math.floor(virtualScrollTop / VIRTUAL_ITEM_HEIGHT) - VIRTUAL_OVERSCAN);
  const end = Math.min(total, Math.ceil((virtualScrollTop + viewH) / VIRTUAL_ITEM_HEIGHT) + VIRTUAL_OVERSCAN);

  // 顶部 spacer（撑起滚动位置）+ 可见项 + 底部 spacer（撑起总高度）
  const topH = start * VIRTUAL_ITEM_HEIGHT;
  const bottomH = (total - end) * VIRTUAL_ITEM_HEIGHT;
  const items = filtered.slice(start, end).map((r, i) => {
    return `<div style="height:${VIRTUAL_ITEM_HEIGHT}px;box-sizing:border-box">${renderItemHTML(r, start + i)}</div>`;
  }).join('');
  list.innerHTML = `<div style="height:${topH}px"></div>${items}${bottomH > 0 ? `<div style="height:${bottomH}px"></div>` : ''}`;
  requestElCache.clear();
}

// 滚动事件处理（rAF 节流）
function onListScroll() {
  if (virtualFiltered.length <= VIRTUAL_THRESHOLD) return;
  if (virtualRafPending) return;
  virtualRafPending = true;
  requestAnimationFrame(() => {
    virtualRafPending = false;
    virtualScrollTop = document.getElementById('requestList').scrollTop || 0;
    renderVirtualList();
  });
}

// =============================================
// 核心交互 — selectRequest
// =============================================

async function selectRequest(id) {
  selectedRequestId = id;
  let r = requests.find(r => String(r.id) === id);
  if (!r) return;
  const isPending = !!r.is_pending;

  // 1. 立即切换视觉 — 显示骨架屏
  showSkeletonLoading(isPending);

  try {
    // 3. 异步加载详情（如果需要）
    if (!isPending && (!r.reqHeaders || Object.keys(r.reqHeaders).length === 0)) {
      const d = await loadRequestDetail(id);
      if (d) {
        r = normalizeReq(d);
        const idx = requests.findIndex(x => String(x.id) === id);
        if (idx >= 0) requests[idx] = r;
        if (!r) return;
      }
    }

    // 4. 填充内容
    fillContent(r, isPending);

    // 5. 移除骨架屏，渐入真实内容
    removeSkeleton();
    document.querySelectorAll('.tab-panel.active .detail-section').forEach((s, i) => {
      animateContentIn(s, i * 50);
    });

    renderRequestList();
    scrollToActiveItem();
    if (r.host) syncAPIMapHost(r.host);
  } catch (e) {
    console.error('selectRequest error:', e);
    removeSkeleton();
    document.getElementById('detailEmpty').style.display = 'none';
    document.getElementById('detailContent').style.display = 'flex';
    switchTab(isPending ? 'resend' : 'request');
    renderRequestList();
    scrollToActiveItem();
  }
}

function fillContent(r, isPending) {
  document.getElementById('detailEmpty').style.display = 'none';
  document.getElementById('detailContent').style.display = 'flex';
  document.getElementById('interceptBar').classList.toggle('show', isPending);

  // 请求 tab
  document.getElementById('req-url').textContent = r.url || '';
  document.getElementById('req-method').textContent = r.method || '';
  document.getElementById('req-host').textContent = r.host || '';
  document.getElementById('req-protocol').textContent = r.protocol || '';
  const rh = r.reqHeaders || {};
  document.getElementById('req-headers-content').textContent =
    Object.keys(rh).length > 0 ? Object.entries(rh).map(([k, v]) => `${k}: ${v}`).join('\n') : t('empty');
  document.getElementById('req-body-content').textContent = formatJSONBody(r.reqBody) || t('empty');

  // 响应 tab
  const sc = `status-${Math.floor(r.status / 100)}xx`;
  document.getElementById('res-status').innerHTML = isPending ? '—' : `<span class="status-code ${sc}">${r.status}</span>`;
  document.getElementById('res-time').textContent = r.time || '';
  document.getElementById('res-size').textContent = r.size || '';
  const rsh = r.resHeaders || {};
  document.getElementById('res-headers-content').textContent =
    Object.keys(rsh).length > 0 ? Object.entries(rsh).map(([k, v]) => `${k}: ${v}`).join('\n') : t('empty');
  document.getElementById('res-body-content').textContent = formatJSONBody(r.resBody) || t('empty');

  // 重发 tab
  document.getElementById('resendMethod').value = r.method || 'GET';
  document.getElementById('resendUrl').value = r.url || '';
  document.getElementById('resendBody').value = r.reqBody || '';
  const hc = document.getElementById('resendHeaders'); hc.innerHTML = '';
  if (rh && Object.keys(rh).length > 0) {
    Object.entries(rh).forEach(([k, v]) => addHeaderRow(k, v));
  } else {
    addHeaderRow('Host', r.host || '');
  }

  // LLM tab: show/hide based on detection
  const llmTabBtn = document.getElementById('tabBtnLLM');
  const isLLM = isLLMRequest(r);
  if (llmTabBtn) llmTabBtn.style.display = isLLM ? '' : 'none';
  // If switching away and LLM tab was active, go back to request tab
  if (!isLLM && activeTab === 'llm') switchTab('request');
}

// ── 骨架屏 (Skeleton Loading) ────────────────
// 使用独立覆盖层，不破坏原始 tab panel DOM
function showSkeletonLoading(isPending) {
  document.getElementById('detailEmpty').style.display = 'none';
  document.getElementById('detailContent').style.display = 'flex';
  document.getElementById('interceptBar').classList.toggle('show', isPending);
  switchTab(isPending ? 'resend' : 'request');

  // 在每个 tab panel 内插入骨架覆盖层
  document.querySelectorAll('.tab-panel').forEach(panel => {
    // 先移除旧骨架（如果有）
    const old = panel.querySelector('.skeleton-overlay');
    if (old) old.remove();

    const overlay = document.createElement('div');
    overlay.className = 'skeleton-overlay';
    overlay.innerHTML = `
      <div class="detail-section skeleton-shimmer">
        <div class="skeleton-line skeleton-line-short"></div>
        <div class="skeleton-line skeleton-line-long"></div>
        <div class="skeleton-line skeleton-line-long"></div>
        <div class="skeleton-line skeleton-line-med"></div>
      </div>
      <div class="detail-section skeleton-shimmer">
        <div class="skeleton-line skeleton-line-short"></div>
        <div class="skeleton-block"></div>
      </div>
      <div class="detail-section skeleton-shimmer">
        <div class="skeleton-line skeleton-line-short"></div>
        <div class="skeleton-block skeleton-block-sm"></div>
      </div>`;
    panel.appendChild(overlay);

    // 渐入骨架
    const sections = overlay.querySelectorAll('.detail-section');
    sections.forEach((s, i) => {
      s.style.opacity = '0';
      s.style.transform = 'translateY(4px)';
      s.style.transition = 'none';
      void s.offsetWidth;
      s.style.transition = `opacity .25s ease ${i * 60}ms, transform .25s ease ${i * 60}ms`;
      s.style.opacity = '1';
      s.style.transform = 'translateY(0)';
    });
  });
}

function removeSkeleton() {
  document.querySelectorAll('.tab-panel .skeleton-overlay').forEach(el => {
    el.style.opacity = '0';
    el.style.transform = 'translateY(-6px)';
    el.style.transition = 'opacity .18s ease, transform .18s ease';
    el.addEventListener('transitionend', () => el.remove(), { once: true });
    // 兜底：300ms 后强制移除
    setTimeout(() => { if (el.parentNode) el.remove(); }, 300);
  });
}

// ── 滚动到选中项 ─────────────────────────────
function scrollToActiveItem() {
  const active = document.querySelector('.request-item.active');
  if (active) {
    active.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  }
}

// ── Tab 切换 ─────────────────────────────────
function switchTab(tab) {
  if (!tab) return;
  if (activeTab === tab) return; // 已是当前 tab，不重复切换
  activeTab = tab;
  document.querySelectorAll('.detail-tab').forEach(t => t.classList.remove('active'));
  document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
  const bt = document.querySelector(`.detail-tab[data-tab="${tab}"]`);
  if (!bt) { console.warn('switchTab: tab button not found for', tab); return; }
  const tp = document.getElementById(`tab-${tab}`);
  if (!tp) { console.warn('switchTab: tab panel not found for', tab); return; }
  bt.classList.add('active');
  tp.classList.add('active');
  void bt.offsetWidth;
  const ind = document.getElementById('tabIndicator');
  if (ind) {
    ind.style.left = bt.offsetLeft + 'px';
    ind.style.width = bt.offsetWidth + 'px';
  }
  if (tab === 'apimap') loadAPIMapHosts();
  if (tab === 'llm' && selectedRequestId) loadLLMDetail(selectedRequestId);
}

function syncAPIMapHost(host) {
  // 去端口匹配: httpbin.org:443 → httpbin.org
  const baseHost = host.replace(/:(\d+)$/, '');
  const sel = document.getElementById('apimapHostSelect');
  for (let i = 0; i < sel.options.length; i++) {
    if (sel.options[i].value === baseHost) {
      sel.value = baseHost;
      if (document.getElementById('tab-apimap').classList.contains('active')) loadAPIMap();
      return;
    }
  }
}

// ── Resend ───────────────────────────────────
function addHeaderRow(k, v) {
  k = k || ''; v = v || '';
  const c = document.getElementById('resendHeaders');
  const row = document.createElement('div');
  row.className = 'kv-editor-row';
  row.innerHTML = `<input type="text" placeholder="Header name" value="${escAttr(k)}" class="header-key" style="max-width:200px">
    <input type="text" placeholder="Header value" value="${escAttr(v)}" class="header-value">
    <button class="kv-remove-btn" onclick="this.parentElement.remove()">
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
    </button>`;
  c.appendChild(row);
}

async function sendResend() {
  const method = document.getElementById('resendMethod').value;
  const url = document.getElementById('resendUrl').value;
  const body = document.getElementById('resendBody').value;
  const hrs = document.querySelectorAll('#resendHeaders .kv-editor-row');
  const headers = {};
  hrs.forEach(r => {
    const k = r.querySelector('.header-key').value.trim();
    const v = r.querySelector('.header-value').value.trim();
    if (k) headers[k] = v;
  });
  if (!url) { showToast('error', 'URL required'); return; }

  const btn = document.getElementById('resendBtn');
  btn.classList.add('sending');
  btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" class="spin"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>...';
  try {
    const result = await apiPost('/api/resend', { method, url, headers, body });
    await loadRequests();
    if (result && result.id) selectRequest(result.id);
    showToast('success', `${t('sent')} — ${result.status_code} (${result.duration_ms}ms)`);
  } catch (err) {
    showToast('error', t('send_failed') + ': ' + err.message);
  } finally {
    btn.classList.remove('sending');
    btn.innerHTML = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg> ${t('send')}`;
  }
}

// ── API Map ──────────────────────────────────

async function loadAPIMapHosts() {
  const sel = document.getElementById('apimapHostSelect');
  try {
    const r = await apiGet('/api/apimap/hosts?limit=500');
    const hosts = r.data || [];
    sel.innerHTML = `<option value="">${t('select_host')}</option>`;
    const frag = document.createDocumentFragment();
    hosts.forEach(h => {
      const o = document.createElement('option');
      o.value = h.split(' (')[0];
      o.textContent = h;
      frag.appendChild(o);
    });
    sel.appendChild(frag);
  } catch (e) {
    console.warn('loadAPIMapHosts failed:', e);
    sel.innerHTML = `<option value="">${t('select_host')}</option>`;
  }
}

function filterHostDropdown() {
  const s = document.getElementById('hostSearchInput').value;
  const sel = document.getElementById('apimapHostSelect');
  for (let i = 1; i < sel.options.length; i++) {
    const opt = sel.options[i];
    opt.style.display = !s || opt.textContent.toLowerCase().includes(s.toLowerCase()) ? '' : 'none';
  }
}

async function loadAPIMap() {
  const host = document.getElementById('apimapHostSelect').value;
  if (!host) return;
  let success = false;
  try {
    const tree = await apiGet('/api/apimap?host=' + encodeURIComponent(host));
    renderAPIMapTree(tree);
    success = true;
  } catch (e) {
    console.warn('loadAPIMap failed:', e);
    document.getElementById('apimapTree').innerHTML =
      '<div style="padding:20px;color:var(--red);font-size:12px;text-align:center">Failed to load API map</div>';
  }
  // 仅在成功时联动请求列表过滤，避免失败时错误清空
  if (success) {
    currentHost = host;
    document.getElementById('searchInput').value = '';
    currentFilter = 'all';
    document.querySelectorAll('.filter-chip').forEach(c => c.classList.remove('active'));
    const allBtn = document.querySelector('[data-filter="all"]');
    if (allBtn) allBtn.classList.add('active');
    loadRequests();
  }
}

function renderAPIMapTree(node) {
  const container = document.getElementById('apimapTree');
  if (!node || (!node.children || node.children.length === 0) && !node.isLeaf && (!node.methods || node.methods.length === 0)) {
    container.innerHTML = '<div style="padding:20px;color:var(--text-tertiary);font-size:12px;text-align:center">No endpoints captured</div>';
    return;
  }
  let html = '';
  // 渲染根节点自身的方法（如 CONNECT /）
  if (node.methods && node.methods.length > 0) {
    html += renderLeaf(node, 0);
  }
  // 渲染子树
  if (node.children && node.children.length > 0) {
    html += renderTreeNode(node, 0);
  }
  container.innerHTML = html;
}

function renderTreeNode(node, depth) {
  let html = '';
  (node.children || []).forEach(child => {
    if (child.is_leaf) {
      html += renderLeaf(child, depth + 1);
    } else {
      const hasChildren = child.children && child.children.length > 0;
      const allMethods = [...new Set((child.methods || []))];
      const dominantMethod = allMethods[0] || '';
      const borderClass = dominantMethod ? `border-${dominantMethod}` : '';
      html += `<div class="tree-node"><div class="tree-node-header ${borderClass}" style="padding-left:${depth * 20 + 4}px" onclick="toggleTreeNode(this)">
        <span class="tree-node-toggle ${hasChildren ? 'open' : ''}"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="m9 18 6-6-6-6"/></svg></span>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="flex-shrink:0;color:var(--yellow)"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
        <span class="tree-node-name">${esc(child.name)}</span><span class="tree-node-count">(${child.count || 0})</span>
        <div class="tree-node-methods">${allMethods.map(m => `<span class="tree-method-tag method-${escAttr(m)}">${esc(m)}</span>`).join('')}</div>
      </div><div class="tree-node-children">${hasChildren ? renderTreeNode(child, depth + 1) : ''}</div></div>`;
    }
  });
  return html;
}

function renderLeaf(node, depth) {
  const methods = node.methods || [];
  const statuses = node.statuses || {};
  let html = '';
  methods.forEach(method => {
    const statusHtml = Object.entries(statuses).sort((a, b) => b[1] - a[1]).slice(0, 3)
      .map(([s, c]) => { const cls = 's' + Math.floor(parseInt(s) / 100); return `<span class="${cls}">${s}</span>`; }).join('');
    const hasNote = node.note && node.note_id;
    html += `<div class="tree-leaf-row ${method}" onclick="leafClick('${escJS(node.full_path)}','${escJS(method)}')" oncontextmenu="leafContext(event,'${escJS(node.full_path)}','${escJS(method)}','${escJS(node.note || '')}',${node.note_id || 0})" title="点击过滤 | 右键更多">
      <span class="tree-leaf-method method-${escAttr(method)}">${esc(method)}</span>
      <span class="tree-leaf-path">${esc(node.full_path)}</span>
      <span class="tree-leaf-status">${statusHtml}</span>
      ${hasNote ? `<span class="tree-leaf-note">${esc((node.note || '').substring(0, 24))}</span>` : ''}
    </div>`;
  });
  return html;
}

// Context menu
let ctxPath = '', ctxMethod = '', ctxNote = '', ctxNoteId = 0;
function leafClick(path, method) {
  const host = document.getElementById('apimapHostSelect').value;
  currentHost = host ? host : ''; currentFilter = 'all';
  document.getElementById('searchInput').value = path;
  document.querySelectorAll('.filter-chip').forEach(c => c.classList.remove('active'));
  const allBtn = document.querySelector('[data-filter="all"]');
  if (allBtn) allBtn.classList.add('active');
  loadRequests();
}
function leafContext(e, path, method, note, noteId) {
  e.preventDefault();
  ctxPath = path; ctxMethod = method; ctxNote = note; ctxNoteId = noteId;
  const menu = document.getElementById('ctxMenu');
  menu.classList.remove('hidden');
  menu.style.left = Math.min(e.clientX, window.innerWidth - 190) + 'px';
  menu.style.top = Math.min(e.clientY, window.innerHeight - 120) + 'px';
}
function ctxOpenNote() { closeCtx(); editAPINoteLeaf(ctxPath, ctxMethod, ctxNote, ctxNoteId); }
function ctxCopyURL() {
  closeCtx();
  const host = document.getElementById('apimapHostSelect').value;
  navigator.clipboard.writeText((host || '') + ctxPath).then(() => showToast('success', t('copied'))).catch(() => showToast('error', 'Failed'));
}
function ctxFilterHost() {
  closeCtx();
  currentHost = document.getElementById('apimapHostSelect').value || '';
  document.getElementById('searchInput').value = '';
  currentFilter = 'all';
  document.querySelectorAll('.filter-chip').forEach(c => c.classList.remove('active'));
  const allBtn = document.querySelector('[data-filter="all"]');
  if (allBtn) allBtn.classList.add('active');
  loadRequests();
}
function closeCtx() { document.getElementById('ctxMenu').classList.add('hidden'); }

// ── Rules Panel ──────────────────────────────
async function openRulesPanel() {
  document.getElementById('rulesPanel').classList.remove('hidden');
  await loadRules();
}
function closeRulesPanel() { document.getElementById('rulesPanel').classList.add('hidden'); }
async function loadRules() {
  try {
    const rules = await apiGet('/api/intercept/rules');
    let html = rules.map(r => `
      <div class="rule-item">
        <span class="rule-item-pattern">${esc(r.pattern)}</span>
        ${r.method ? `<span class="method-badge method-${escAttr(r.method)}" style="font-size:9px;padding:1px 5px">${esc(r.method)}</span>` : ''}
        <span class="rule-item-action ${r.action}">${r.action.toUpperCase()}</span>
        <div class="rule-item-actions">
          <button class="${r.enabled ? 'enabled' : 'disabled'}" onclick="toggleRule(${r.id},${!r.enabled})">${r.enabled ? 'ON' : 'OFF'}</button>
          <button class="delete" onclick="deleteRule(${r.id})">✕</button>
        </div>
      </div>`).join('');
    document.getElementById('ruleList').innerHTML = html || '<div style="color:var(--text-tertiary);padding:8px 0">暂无规则</div>';
  } catch(e) { showToast('error', '加载规则失败'); }
}
async function addRule() {
  const pattern = document.getElementById('newRulePattern').value.trim();
  const action = document.getElementById('newRuleAction').value;
  const method = document.getElementById('newRuleMethod').value;
  if (!pattern) { showToast('error', '请输入匹配模式'); return; }
  try {
    await apiPost('/api/intercept/rules', { pattern, method, action, enabled: true });
    document.getElementById('newRulePattern').value = '';
    document.getElementById('newRuleMethod').value = '';
    showToast('success', '规则已添加');
    await loadRules();
  } catch(e) { showToast('error', '添加失败: ' + e.message); }
}
async function toggleRule(id, enabled) {
  try {
    await apiPut('/api/intercept/rules/' + id, { enabled });
    if (typeof currentHost !== 'undefined' && currentHost) {
      const r = await apiGet('/api/intercept/rules');
      // rules auto-reload via server-side interceptor
    }
    await loadRules();
  } catch(e) { showToast('error', '操作失败'); }
}
async function deleteRule(id) {
  if (!confirm('删除该规则？')) return;
  try {
    await apiDelete('/api/intercept/rules/' + id);
    await loadRules();
    showToast('success', '已删除');
  } catch(e) { showToast('error', '删除失败'); }
}

let noteEditHost = '', noteEditPath = '', noteEditMethod = '', noteEditId = 0;
function editAPINoteLeaf(path, method, note, noteId) {
  noteEditHost = document.getElementById('apimapHostSelect').value;
  noteEditPath = path; noteEditMethod = method; noteEditId = noteId || 0;
  document.getElementById('noteModalPath').textContent = path + '  [' + method + ']';
  document.getElementById('noteModalInput').value = note;
  document.getElementById('noteModalDelete').style.display = noteId ? 'inline-block' : 'none';
  document.getElementById('noteModal').classList.remove('hidden');
  document.getElementById('noteModalInput').focus();
}
function closeNoteModal() { document.getElementById('noteModal').classList.add('hidden'); }
async function saveNoteModal() {
  const note = document.getElementById('noteModalInput').value; closeNoteModal();
  try { await apiPost('/api/apimap/notes', { host: noteEditHost, path: noteEditPath, method: noteEditMethod, note }); loadAPIMap(); showToast('success', 'Note saved'); }
  catch (e) { showToast('error', 'Failed to save note'); }
}
async function deleteAPINote() {
  if (!noteEditId) return; if (!confirm('删除该备注？')) return; closeNoteModal();
  try { await apiDelete('/api/apimap/notes/' + noteEditId); loadAPIMap(); showToast('success', 'Note deleted'); }
  catch (e) { showToast('error', 'Failed to delete note'); }
}

// ── Controls ─────────────────────────────────
function toggleCapture() {
  isRecording = !isRecording;
  const btn = document.getElementById('captureBtn'), label = document.getElementById('captureLabel'),
    dot = document.getElementById('statusDot'), text = document.getElementById('statusText');
  if (isRecording) {
    btn.classList.add('recording'); label.textContent = t('recording');
    dot.classList.add('recording'); text.textContent = t('recording');
  } else {
    btn.classList.remove('recording'); label.textContent = t('paused');
    dot.classList.remove('recording'); text.textContent = t('paused');
  }
}
function setFilter(btn, f) { currentFilter = f; currentHost = ''; currentPage = 1; document.querySelectorAll('.filter-chip').forEach(c => c.classList.remove('active')); btn.classList.add('active'); loadRequests(); }
function toggleErrorFilter() {
  errorFilterOnly = !errorFilterOnly; currentHost = ''; currentPage = 1;
  const btn = document.getElementById('errorFilterBtn');
  if (errorFilterOnly) { btn.style.background = 'var(--red-muted)'; btn.style.color = 'var(--red)'; btn.classList.add('active'); }
  else { btn.style.background = 'transparent'; btn.style.color = 'var(--text-secondary)'; btn.classList.remove('active'); }
  loadRequests();
}
// 搜索输入防抖：避免每次按键都请求；重置到第 1 页
let searchTimer = null;
function debounceSearch() {
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { currentPage = 1; loadRequests(); }, 300);
}
function toggleStarredOnly() {
  starredOnly = !starredOnly;
  const btn = document.getElementById('starredFilterBtn');
  if (starredOnly) {
    btn.style.color = 'var(--yellow)';
    btn.classList.add('active');
    // 切换到收藏视图时，从后端拉取所有收藏请求
    loadStarred();
  } else {
    btn.style.color = '';
    btn.classList.remove('active');
    loadRequests();
  }
}
async function loadStarred() {
  try {
    const r = await apiGet('/api/starred?limit=1000');
    requestVersion++;
    // 修复：normalizeReq 不保留 starred 字段，但 /api/starred 返回的必然是已收藏项
    // 显式标记 starred=true，避免后续 renderRequestList 的二次过滤把它们全部丢掉
    requests = (r.data || []).map(r => { const n = normalizeReq(r); n.starred = true; return n; });
    virtualScrollTop = 0;
    const listEl = document.getElementById('requestList');
    if (listEl) listEl.scrollTop = 0;
    renderRequestList();
  } catch (e) { console.error('loadStarred failed:', e); }
}
async function clearHistory() {
  if (!confirm(t('confirm_clear'))) return;
  try {
    await apiPost('/api/clear', {});
    requests = []; requestDetailCache = {}; selectedRequestId = null; currentHost = ''; requestVersion++;
    currentPage = 1; pageTotal = 0;
    document.getElementById('searchInput').value = '';
    document.getElementById('detailEmpty').style.display = 'flex';
    document.getElementById('detailContent').style.display = 'none';
    renderRequestList();
    updatePagination();
    showToast('success', t('cleared'));
  } catch (e) { showToast('error', t('send_failed') + ': ' + e.message); }
}

// ── Intercept ────────────────────────────────
let interceptMode = 'auto', pendingRequests = {};
async function toggleInterceptMode() {
  interceptMode = interceptMode === 'auto' ? 'manual' : 'auto';
  try {
    await apiPost('/api/intercept/mode', { mode: interceptMode });
    const ind = document.getElementById('modeIndicator');
    ind.textContent = interceptMode === 'auto' ? 'AUTO' : 'MANUAL';
    ind.className = 'mode-indicator ' + (interceptMode === 'auto' ? 'mode-auto' : 'mode-manual');
  } catch (e) { showToast('error', 'Mode switch failed'); }
}
function addPendingToList(p) {
  pendingRequests[p.id] = p;
  const r = { id: p.id, method: p.method, url: p.url, host: p.host, path: p.path,
    status: 0, reqHeaders: p.headers || {}, reqBody: p.body || '',
    resHeaders: {}, resBody: '', protocol: '', is_pending: true, size: '', captured_at: p.timestamp };
  requestVersion++; requests.unshift(r); renderRequestList();
}
async function interceptAction(action) {
  if (!selectedRequestId || !pendingRequests[selectedRequestId]) return;
  const p = pendingRequests[selectedRequestId];
  // 始终读取当前表单值，以 'modify' 发送（允许未修改时原样转发）
  const m = document.getElementById('resendMethod').value, u = document.getElementById('resendUrl').value,
    b = document.getElementById('resendBody').value;
  const hrs = document.querySelectorAll('#resendHeaders .kv-editor-row'); const h = {};
  hrs.forEach(r => { const k = r.querySelector('.header-key').value.trim(); const v = r.querySelector('.header-value').value.trim(); if (k) h[k] = v; });
  try {
    await apiPost('/api/intercept/action', { request_id: selectedRequestId, action: action === 'drop' ? 'drop' : 'modify', method: m, url: u, new_headers: h, new_body: b });
  } catch (e) { showToast('error', 'Intercept failed'); }
  removePending(selectedRequestId);
}
function removePending(id) {
  delete pendingRequests[id]; requests = requests.filter(r => r.id !== id);
  selectedRequestId = null; requestVersion++;
  document.getElementById('interceptBar').classList.remove('show');
  document.getElementById('detailEmpty').style.display = 'flex';
  document.getElementById('detailContent').style.display = 'none'; renderRequestList();
}

// ── Utilities ────────────────────────────────
function esc(s) { if (!s) return ''; const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }
function escAttr(s) { return (s || '').replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/'/g, '&#39;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
function escJS(s) { return esc(s).replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/"/g, '&quot;'); }
function shellQuote(s) {
  // 单引号字符串中转义单引号：' -> '\''
  return String(s).replace(/'/g, "'\\''");
}
function copyToClipboard(cid) {
  const el = document.getElementById(cid); const pre = el.querySelector('pre');
  if (!pre) return;
  navigator.clipboard.writeText(pre.textContent).then(() => showToast('success', t('copied'))).catch(() => showToast('error', 'Copy failed'));
}
// 切换请求收藏状态（starred=true 表示当前已是收藏，点击则取消）
async function toggleStar(id, starred) {
  try {
    await apiPost('/api/starred', { id, starred: !starred });
    // 更新内存中的请求对象
    const idx = requests.findIndex(r => r.id === id);
    if (idx >= 0) requests[idx].starred = !starred;
    // 收藏视图下取消收藏需移除
    if (starredOnly && starred) {
      requests.splice(idx, 1);
    }
    renderRequestList();
    showToast('success', !starred ? '已收藏' : '已取消收藏');
  } catch (e) { showToast('error', '操作失败: ' + e.message); }
}

function copyAsCurl() {
  if (!selectedRequestId) { showToast('error', '请先选择一个请求'); return; }
  const req = requestDetailCache[selectedRequestId];
  if (!req) { showToast('error', '请求详情未加载'); return; }
  let curl = `curl -X '${shellQuote(req.method)}' '${shellQuote(req.url)}'`;
  if (req.req_headers) Object.entries(req.req_headers).forEach(([k, v]) => {
    curl += ` -H '${shellQuote(k)}: ${shellQuote(v)}'`;
  });
  if (req.req_body) curl += ` -d '${shellQuote(req.req_body)}'`;
  navigator.clipboard.writeText(curl).then(() => showToast('success', 'curl 命令已复制')).catch(() => showToast('error', 'Copy failed'));
}

// 生成 fetch / python-requests 代码并复制到剪贴板
function copyAsFetch() {
  if (!selectedRequestId) { showToast('error', '请先选择一个请求'); return; }
  const req = requestDetailCache[selectedRequestId];
  if (!req) { showToast('error', '请求详情未加载'); return; }
  const opts = { method: req.method };
  const hdrs = {};
  if (req.req_headers) Object.entries(req.req_headers).forEach(([k, v]) => { hdrs[k] = v; });
  if (Object.keys(hdrs).length > 0) opts.headers = hdrs;
  if (req.req_body) {
    // 尝试解析为 JSON，否则作为纯文本
    try { opts.body = JSON.parse(req.req_body); } catch { opts.body = req.req_body; }
  }
  let code = `fetch('${req.url}', ${JSON.stringify(opts, null, 2)})\n  .then(res => res.text())\n  .then(text => console.log(text));`;
  navigator.clipboard.writeText(code).then(() => showToast('success', 'fetch 代码已复制')).catch(() => showToast('error', 'Copy failed'));
}

function copyAsPython() {
  if (!selectedRequestId) { showToast('error', '请先选择一个请求'); return; }
  const req = requestDetailCache[selectedRequestId];
  if (!req) { showToast('error', '请求详情未加载'); return; }
  const hdrs = {};
  if (req.req_headers) Object.entries(req.req_headers).forEach(([k, v]) => { hdrs[k] = v; });
  let code = `import requests\n\n`;
  code += `url = '${req.url}'\n`;
  if (Object.keys(hdrs).length > 0) code += `headers = ${JSON.stringify(hdrs, null, 4)}\n`;
  if (req.req_body) {
    // 尝试解析为 JSON
    try {
      const parsed = JSON.parse(req.req_body);
      code += `json_data = ${JSON.stringify(parsed, null, 4)}\n`;
      code += `\nresponse = requests.${req.method.toLowerCase()}(url`;
      if (Object.keys(hdrs).length > 0) code += `, headers=headers`;
      code += `, json=json_data)`;
    } catch {
      code += `data = ${JSON.stringify(req.req_body)}\n`;
      code += `\nresponse = requests.${req.method.toLowerCase()}(url`;
      if (Object.keys(hdrs).length > 0) code += `, headers=headers`;
      code += `, data=data)`;
    }
  } else {
    code += `\nresponse = requests.${req.method.toLowerCase()}(url`;
    if (Object.keys(hdrs).length > 0) code += `, headers=headers`;
    code += `)`;
  }
  code += `\nprint(response.status_code)\nprint(response.text)`;
  navigator.clipboard.writeText(code).then(() => showToast('success', 'python 代码已复制')).catch(() => showToast('error', 'Copy failed'));
}
function formatJSONBody(text) {
  try { const obj = JSON.parse(text); return JSON.stringify(obj, null, 2); }
  catch { return text; }
}
async function exportHAR() {
  try {
    const r = await fetch('/api/export/har?limit=500');
    if (!r.ok) {
      let body = null;
      try { body = await r.json(); } catch {}
      showToast('error', getErrorMessage(body, r.status));
      return;
    }
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'packetlab.har';
    document.body.appendChild(a);
    a.click();
    setTimeout(() => { document.body.removeChild(a); URL.revokeObjectURL(a.href); }, 100);
    showToast('success', 'HAR 文件下载中...');
  } catch (e) {
    showToast('error', '导出失败: ' + (e.message || '网络错误'));
  }
}
function showToast(type, msg) {
  const c = document.getElementById('toastContainer');
  const toast = document.createElement('div');
  toast.className = `toast ${type}`;
  const icons = {
    success: '<polyline points="20 6 9 17 4 12"/>',
    error: '<circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/>',
    warn: '<path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>',
    info: '<circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/>'
  };
  const svg = icons[type] || icons.error;
  toast.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">${svg}</svg><span>${msg.replace(/\n/g, '<br>')}</span>`;
  c.appendChild(toast);
  setTimeout(() => { toast.style.opacity = '0'; toast.style.transition = 'opacity .2s'; }, 2800);
  setTimeout(() => toast.remove(), 2500);
}
function toggleTreeNode(header) {
  const toggle = header.querySelector('.tree-node-toggle');
  const children = header.nextElementSibling;
  if (children && children.classList.contains('tree-node-children')) {
    toggle.classList.toggle('open');
    children.classList.toggle('open');
  }
}

// ── Capture Panel ────────────────────────────
let captureRunning = false;
async function toggleNICCapture() { await openCapturePanel(); }
async function openCapturePanel() {
  document.getElementById('capturePanel').classList.remove('hidden');
  // 刷新状态和网卡列表
  try {
    const status = await apiGet('/api/capture/status');
    captureRunning = status.running;
    updateCaptureUI();
    const ifaces = await apiGet('/api/capture/interfaces') || [];
    const sel = document.getElementById('captureIface');
    sel.innerHTML = ifaces.map(i => `<option value="${i}">${i}</option>`).join('');
  } catch { /* ignore */ }
}
function closeCapturePanel() { document.getElementById('capturePanel').classList.add('hidden'); }
function updateCaptureUI() {
  const btn = document.getElementById('captureToggleBtn');
  const status = document.getElementById('captureStatus');
  const startBtn = document.getElementById('captureStartBtn');
  if (captureRunning) {
    if (btn) btn.classList.add('recording');
    if (status) status.innerHTML = '状态: <span style="color:var(--green);font-weight:600">● 抓包中</span>';
    if (startBtn) { startBtn.textContent = '停止抓包'; startBtn.className = 'modal-btn danger'; }
  } else {
    if (btn) btn.classList.remove('recording');
    if (status) status.textContent = '状态: 未启动';
    if (startBtn) { startBtn.textContent = '启动抓包'; startBtn.className = 'modal-btn primary'; }
  }
}
async function startCaptureFromPanel() {
  if (captureRunning) {
    try { await apiPost('/api/capture/stop', {}); captureRunning = false; updateCaptureUI(); showToast('success', '抓包已停止'); }
    catch (e) { showToast('error', '停止失败: ' + e.message); }
    return;
  }
  const iface = document.getElementById('captureIface').value;
  const bpf = document.getElementById('captureBpfInput').value;
  try {
    await apiPost('/api/capture/start', { interface: iface, bpf });
    captureRunning = true;
    updateCaptureUI();
    showToast('success', `抓包已启动 — ${iface}`);
  } catch (e) {
    const msg = e.message || '';
    if (msg.includes('permission') || msg.includes('Permission'))
      showToast('warn', '权限不足\n请用 sudo 运行 PacketLab');
    else
      showToast('error', '启动失败: ' + msg);
  }
}
// ── Intercept Logs ──────────────────────────
function switchRulesTab(tab) {
  document.querySelectorAll('.rules-tab').forEach(t => t.classList.remove('active'));
  document.querySelectorAll('.rules-tab').forEach(t => { if (t.textContent.trim().startsWith(tab === 'rules' ? '规则' : '拦截')) t.classList.add('active'); });
  document.getElementById('rulesPanelContent').style.display = tab === 'rules' ? '' : 'none';
  document.getElementById('logsPanelContent').style.display = tab === 'logs' ? '' : 'none';
  if (tab === 'logs') loadInterceptLogs();
}

// 输入框过滤的防抖（避免每次按键都发请求）
let logFilterTimer = null;
function debounceLogFilter() {
  if (logFilterTimer) clearTimeout(logFilterTimer);
  logFilterTimer = setTimeout(loadInterceptLogs, 300);
}

async function loadInterceptLogs() {
  try {
    const action = document.getElementById('logActionFilter').value;
    const limit = document.getElementById('logLimitSelect').value || '50';
    const host = (document.getElementById('logHostFilter')?.value || '').trim();
    const pattern = (document.getElementById('logPatternFilter')?.value || '').trim();
    const p = new URLSearchParams();
    if (action) p.set('action', action);
    if (host) p.set('host', host);
    if (pattern) p.set('pattern', pattern);
    p.set('limit', limit);
    const r = await apiGet('/api/intercept/logs?' + p.toString());
    renderInterceptLogs(r.data || []);
  } catch (e) { console.warn('loadInterceptLogs failed:', e); }
}

function renderInterceptLogs(logs) {
  const container = document.getElementById('logList');
  if (!logs || logs.length === 0) {
    container.innerHTML = '<div style="color:var(--text-tertiary);padding:16px;text-align:center;font-size:11px">暂无拦截日志</div>';
    return;
  }
  container.innerHTML = logs.map(l => {
    const actionLabel = { allow: 'ALLOW', drop: 'DROP', modify: 'MODIFY' }[l.action] || l.action;
    const time = l.created_at ? new Date(l.created_at.replace(' ', 'T') + 'Z').toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '';
    const rule = l.rule_pattern ? `<span style="color:var(--accent);font-size:9px">${esc(l.rule_pattern)}</span>` : '';
    return `<div class="log-item">
      <span class="log-item-action ${l.action}">${actionLabel}</span>
      <span class="log-item-url" title="${escAttr(l.request_url)}">${esc(l.request_method)} ${esc(l.request_url)}</span>
      <span class="log-item-meta">
        ${rule}
        <span class="log-item-mode">${l.mode}</span>
        <span class="log-item-time">${time}</span>
      </span>
    </div>`;
  }).join('');
}

// ── Init ─────────────────────────────────────
(function init() {
  applyLang();

  // 请求列表点击事件委托 — DOM 此时已就绪
  const listEl = document.getElementById('requestList');
  listEl.addEventListener('scroll', onListScroll, { passive: true });
  // 窗口大小变化时重新计算可见区
  window.addEventListener('resize', () => {
    if (virtualFiltered.length > VIRTUAL_THRESHOLD) renderVirtualList();
  });
  listEl.addEventListener('click', (e) => {
    const item = e.target.closest('.request-item');
    if (item && item.dataset.id) {
      createRipple(e, item);
      selectRequest(item.dataset.id);
    }
  });

  document.addEventListener('click', closeCtx);
  document.getElementById('noteModal').addEventListener('click', function (e) { if (e.target === this) closeNoteModal(); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeNoteModal(); });
  document.getElementById('searchInput').addEventListener('mousedown', function (e) {
    if (e.button === 1) { e.preventDefault(); this.value = ''; currentHost = ''; loadRequests(); }
  });
  document.addEventListener('keydown', (e) => {
    const mod = e.metaKey || e.ctrlKey;
    if (mod && e.key === 'k') { e.preventDefault(); document.getElementById('searchInput').focus(); }
    if (mod && e.key === 'f' && document.activeElement !== document.getElementById('searchInput')) { e.preventDefault(); document.getElementById('searchInput').focus(); }
    if (mod && e.key === 'Enter' && activeTab === 'resend') { e.preventDefault(); sendResend(); }
    if (mod && e.key === '1') { e.preventDefault(); switchTab('request'); }
    if (mod && e.key === '2') { e.preventDefault(); switchTab('response'); }
    if (mod && e.key === '3') { e.preventDefault(); switchTab('resend'); }
    if (mod && e.key === '4') { e.preventDefault(); switchTab('apimap'); }
  });
  (function () {
    const h = document.getElementById('resizeHandle'), p = document.getElementById('requestPanel'); let sx, sw;
    h.addEventListener('mousedown', (ev) => {
      sx = ev.clientX; sw = p.offsetWidth; h.classList.add('dragging');
      document.body.style.cursor = 'col-resize'; document.body.style.userSelect = 'none';
      const onM = (e) => { p.style.width = Math.max(220, Math.min(600, sw + e.clientX - sx)) + 'px'; };
      const onU = () => { h.classList.remove('dragging'); document.body.style.cursor = ''; document.body.style.userSelect = '';
        document.removeEventListener('mousemove', onM); document.removeEventListener('mouseup', onU); };
      document.addEventListener('mousemove', onM); document.addEventListener('mouseup', onU);
    });
  })();
  loadRequests().then(() => {
    connectWebSocket();
    setTimeout(() => {
      if (requests.length > 0 && !selectedRequestId) selectRequest(requests[0].id);
      const at = document.querySelector('.detail-tab.active');
      if (at) { const ind = document.getElementById('tabIndicator'); ind.style.left = at.offsetLeft + 'px'; ind.style.width = at.offsetWidth + 'px'; }
    }, 400);
  });
})();

// =============================================
// LLM AI 对话视图
// =============================================

const LLM_HOST_PATTERNS = [
  { host: 'openai.com', provider: 'openai' },
  { host: 'api.openai.com', provider: 'openai' },
  { host: 'anthropic.com', provider: 'anthropic' },
  { host: 'generativelanguage.googleapis.com', provider: 'gemini' },
  { host: 'aiplatform.googleapis.com', provider: 'gemini' },
];
const LLM_PATH_PATTERNS = ['/chat/completions', '/v1/messages', '/generateContent', '/streamGenerateContent'];

// 检测请求是否为 LLM API 调用
function isLLMRequest(r) {
  if (!r) return false;
  const host = (r.host || '').toLowerCase();
  const url = (r.url || '').toLowerCase();
  for (const p of LLM_HOST_PATTERNS) {
    if (host.includes(p.host)) return true;
  }
  for (const p of LLM_PATH_PATTERNS) {
    if (url.includes(p)) return true;
  }
  return false;
}

// 渲染 LLM 对话内容
function renderLLMContent(exchange) {
  const container = document.getElementById('llmContent');
  if (!exchange) {
    container.innerHTML = '<div class="llm-empty">暂无 LLM 数据</div>';
    return;
  }

  const provider = exchange.provider || 'unknown';
  const model = exchange.model || '';
  const usage = exchange.usage || {};
  const messages = exchange.messages || [];
  const system = exchange.system || '';
  const response = exchange.response || '';

  let html = '<div class="llm-exchange">';

  // Header: provider badge + model
  html += '<div class="llm-header">';
  html += `<span class="llm-badge ${provider}">${provider.toUpperCase()}</span>`;
  if (model) html += `<span class="llm-model">${escapeHTML(model)}</span>`;
  if (exchange.stream) html += '<span class="llm-model">stream</span>';
  html += '</div>';

  // Usage stats
  if (usage.prompt_tokens || usage.completion_tokens || usage.total_tokens) {
    html += '<div class="llm-usage">';
    if (usage.prompt_tokens) html += `<div class="llm-usage-item"><span>Prompt Tokens</span><span class="llm-usage-val">${usage.prompt_tokens}</span></div>`;
    if (usage.completion_tokens) html += `<div class="llm-usage-item"><span>Completion Tokens</span><span class="llm-usage-val">${usage.completion_tokens}</span></div>`;
    if (usage.total_tokens) html += `<div class="llm-usage-item"><span>Total Tokens</span><span class="llm-usage-val">${usage.total_tokens}</span></div>`;
    if (usage.cost_usd) html += `<div class="llm-usage-item"><span>Cost</span><span class="llm-usage-val">$${usage.cost_usd.toFixed(4)}</span></div>`;
    html += '</div>';
  }

  // System prompt (if separate, Anthropic style)
  if (system) {
    html += `<div class="llm-message system"><div class="llm-message-role">System</div>${escapeHTML(system)}</div>`;
  }

  // Conversation messages
  for (const msg of messages) {
    const role = msg.role || 'user';
    const cssClass = role === 'system' ? 'system' : (role === 'assistant' || role === 'model' ? 'assistant' : 'user');
    const roleLabel = role === 'assistant' ? 'Assistant' : (role === 'model' ? 'Model' : role.charAt(0).toUpperCase() + role.slice(1));
    html += `<div class="llm-message ${cssClass}"><div class="llm-message-role">${escapeHTML(roleLabel)}</div>${escapeHTML(msg.content || '')}</div>`;
  }

  // Model response
  if (response) {
    html += `<div class="llm-response"><div class="llm-message-role">Response</div>${escapeHTML(response)}</div>`;
  }

  // Copy prompt button
  html += '<button class="mini-btn llm-copy-btn" onclick="copyLLMPrompt()">📋 复制提示词</button>';

  html += '</div>';
  container.innerHTML = html;

  // Store exchange data for copy
  window._currentLLMExchange = exchange;
  window._currentLLMExchangeVersion = requestVersion;
}

function escapeHTML(s) {
  if (!s) return '';
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function copyLLMPrompt() {
  const ex = window._currentLLMExchange;
  if (!ex) return;
  if (window._currentLLMExchangeVersion !== requestVersion) {
    showToast('error', '请求已切换，请重新选择');
    return;
  }
  let text = '';
  if (ex.system) text += `[System]\n${ex.system}\n\n`;
  for (const msg of (ex.messages || [])) {
    text += `[${msg.role}]\n${msg.content}\n\n`;
  }
  navigator.clipboard.writeText(text).then(() => showToast('success', t('copied')));
}

// 异步加载并渲染 LLM 详情
async function loadLLMDetail(id) {
  try {
    const result = await apiGet('/api/llm/' + id);
    if (result && result.data) {
      // data is a raw JSON (RawMessage), ensure it's parsed
      const exchange = typeof result.data === 'string' ? JSON.parse(result.data) : result.data;
      renderLLMContent(exchange);
    } else {
      renderLLMContent(null);
    }
  } catch (e) {
    console.warn('loadLLMDetail failed', e);
    renderLLMContent(null);
  }
}
