/* ══════════════════════════════════════════════
   ④ log 日志中心：SSE 实时流消费、暂停（不断流）、
   关键字与级别过滤、自动跟随、断线重连横幅、导出
   ══════════════════════════════════════════════ */

let logFlushTimer = null;
let logReconnectTimer = null;
let logWatchdog = null;
const logView = { rendered: 0 };
const LOG_BACKOFF = [2000, 4000, 8000, 16000, 30000];

/* 前端日志缓冲上限：以设置为准（logQuota.bufferLines，500–5000），
   设置未加载或越界时回落默认值。此前一律硬编码 2000，导致设置项改了不生效。 */
function logBufferMax() {
  const v = state.settings && state.settings.logQuota && state.settings.logQuota.bufferLines;
  const n = Number(v);
  if (!Number.isFinite(n) || n < 500 || n > 5000) return LOG_BUFFER_MAX_DEFAULT;
  return Math.floor(n);
}

/* ── 生命周期 ── */
function logCenterEnter() {
  fillLogServiceSelect();
  openLogStream();
}
function logCenterLeave() {
  closeLogStreamConn();
  state.log.connected = false;
  setLogLed('off');
  setText('log-conn-text', '未连接');
}

function fillLogServiceSelect() {
  const sel = el('log-service');
  const cur = sel.value;
  // 被管服务日志聚合属 M3：后端对这些 service 一律返回 501。
  // 直接列为 disabled 并注明，避免用户选中后陷入无意义的退避重连。
  let html = '<option value="">全部服务</option><option value="platform">平台自身（platform）</option>';
  state.services.forEach(s => {
    html += '<option value="' + s.id + '" disabled>' + escapeHtml(svcTitle(s)) + '（M3 提供）</option>';
  });
  sel.innerHTML = html;
  if (cur && sel.querySelector('option[value="' + cur + '"]:not([disabled])')) sel.value = cur;
}
function onLogServiceChange() { openLogStream(); }
function onLogLevelChange() { refilterLogs(); }
const onLogKeywordInput = debounce(() => refilterLogs(), 200);

/* ── 连接（快照 + SSE 实时流） ── */
function closeLogStreamConn() {
  if (state.log.es) { try { state.log.es.close(); } catch (e) { } state.log.es = null; }
  clearTimeout(logFlushTimer); logFlushTimer = null;
  clearTimeout(logReconnectTimer); logReconnectTimer = null;
  clearInterval(logWatchdog); logWatchdog = null;
}
function openLogStream() {
  closeLogStreamConn();
  const svc = el('log-service').value;
  state.log.serviceFilter = svc;
  state.log.lines = [];
  state.log.pendingCount = 0;
  state.log.truncated = false;
  logView.rendered = 0;
  setLogLed('off');
  setText('log-conn-text', '连接中…');
  el('log-console').textContent = '连接中…';
  updatePauseBadge();
  updateLogBufferInfo();
  const query = '/logs?service=' + encodeURIComponent(svc) + '&limit=200';
  apiGet(query).then(rows => {
    handleLogSnapshot(rows || []);
    startLogStream(svc);
  }).catch(() => {
    /* 快照失败也尝试拉起流（重连横幅由流错误路径接管） */
    startLogStream(svc);
    onStreamError();
  });
}
function startLogStream(svc) {
  // svc 为空（"全部服务"）即平台自身日志流：后端对 "" 与 "platform" 一视同仁，
  // 无需单独分支（原先调用的 startLogPlatform 已随 mock 层删除，是遗漏的调用点）。
  const es = new EventSource('/api/logs/stream?service=' + encodeURIComponent(svc));
  state.log.es = es;
  state.log.lastEventAt = Date.now();
  es.onopen = () => { setLogConnected(); };
  es.onmessage = ev => {
    state.log.lastEventAt = Date.now();
    if (!ev.data || ev.data === '__heartbeat') return; /* 心跳不渲染 */
    try { ingestLogLines([JSON.parse(ev.data)]); } catch (e) { /* 非 JSON 忽略 */ }
  };
  es.onerror = () => { if (es.readyState === 2) onStreamError(); };
  /* 40s 心跳超时自检（防半开连接） */
  logWatchdog = setInterval(() => {
    if (Date.now() - state.log.lastEventAt > 40000) {
      try { es.close(); } catch (e) { }
      onStreamError();
    }
  }, 5000);
}
function handleLogSnapshot(rows) {
  state.log.lines = rows.slice(-logBufferMax());
  logView.rendered = 0;
  state.log.needRebuild = false;
  if (state.log.reconnectAttempts > 0) {
    setConnBanner('ok', '<i class="bi bi-check-circle-fill me-1"></i>已恢复连接');
    setTimeout(() => { if (el('log-conn-banner').classList.contains('is-ok')) el('log-conn-banner').classList.add('d-none'); }, 2000);
  }
  state.log.reconnectAttempts = 0;
  setLogConnected();
  refilterLogs();
  logScrollBottom();
}
function setLogConnected() {
  state.log.connected = true;
  setLogLed('on');
  setText('log-conn-text', '实时');
}

/* ── 断线重连（交互说明 5.3） ── */
function onStreamError() {
  if (currentSection !== 'log') return;
  state.log.connected = false;
  state.log.reconnectAttempts++;
  const n = state.log.reconnectAttempts;
  appendLogSeparator('连接断开 ' + fmtClock(Date.now()));
  setLogLed('warn');
  setText('log-conn-text', '重连中');
  if (n >= 5) {
    setConnBanner('err', '<i class="bi bi-wifi-off me-1"></i>无法连接日志服务，请检查平台状态 <button class="btn btn-sm btn-outline-secondary py-0 ms-2" onclick="retryLogConnect()"><i class="bi bi-arrow-clockwise me-1"></i>手动重试</button>');
    return;
  }
  setConnBanner('warn', '<i class="bi bi-wifi-off me-1"></i>日志连接已断开，正在重连…（第 ' + n + ' 次）');
  clearTimeout(logReconnectTimer);
  logReconnectTimer = setTimeout(() => openLogStream(), LOG_BACKOFF[Math.min(n - 1, LOG_BACKOFF.length - 1)]);
}
function retryLogConnect() {
  state.log.reconnectAttempts = 0;
  clearTimeout(logReconnectTimer);
  openLogStream();
}
function setConnBanner(kind, html) {
  const b = el('log-conn-banner');
  b.className = 'log-conn-banner mt-3 is-' + kind;
  b.innerHTML = html;
  b.classList.remove('d-none');
  b.setAttribute('role', kind === 'err' ? 'alert' : 'status');
}
function setLogLed(kind) {
  const led = el('log-conn-led');
  const cls = { on: 'led led-on', off: 'led led-off', warn: 'led led-warn', err: 'led led-err' }[kind] || 'led led-off';
  led.className = cls;
}

/* ── 行渲染 / 过滤 ── */
function logLineHtml(l, opts) {
  if (l.sep) return '<span class="log-sep">── ' + escapeHtml(l.text) + ' ──</span>\n';
  const ts = l.ts ? fmtClock(l.ts) : '--:--:--';
  const lvl = (l.level || 'INFO').toUpperCase();
  const lvlCls = (LOG_LEVEL_META[lvl] || {}).cls || '';
  let msg = escapeHtml(l.msg || '');
  const kw = opts && opts.keyword;
  if (kw) {
    const escKw = escapeHtml(kw).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    if (escKw) msg = msg.replace(new RegExp(escKw, 'gi'), m => '<mark class="log-hit">' + m + '</mark>');
  }
  const svcPart = (opts && opts.omitService) ? '' : ' <span class="log-svc">[' + escapeHtml(l.service || '') + ']</span>';
  return '<span class="log-ts">' + ts + '</span> <span class="' + lvlCls + '">' + lvl + '</span>' + svcPart + ' ' + msg + '\n';
}
function matchesLogFilter(l, kw, lvl) {
  if (l.sep) return true;
  if (lvl && (l.level || '').toUpperCase() !== lvl) return false;
  if (kw && !((l.msg + ' ' + l.service + ' ' + (l.level || '')).toLowerCase().includes(kw.toLowerCase()))) return false;
  return true;
}
function refilterLogs() {
  const c = el('log-console');
  const kw = state.log.keyword = val('log-keyword');
  const lvl = state.log.levelFilter = val('log-level');
  const omit = !!state.log.serviceFilter;
  const visible = state.log.lines.filter(l => matchesLogFilter(l, kw, lvl));
  if (!visible.length) {
    if (!state.log.lines.length) c.textContent = state.log.connected ? '等待日志…' : '连接中…';
    else c.textContent = '无匹配 “' + kw + '” 的日志行（缓冲 ' + state.log.lines.length + ' 行）';
  } else {
    c.innerHTML = visible.map(l => logLineHtml(l, { keyword: kw, omitService: omit })).join('');
  }
  logView.rendered = state.log.lines.length;
  updateLogBufferInfo();
}
function ingestLogLines(rows) {
  if (!rows || !rows.length) return;
  const lines = state.log.lines;
  let removed = 0;
  for (const r of rows) {
    lines.push(r);
    if (lines.length > logBufferMax()) { lines.shift(); removed++; state.log.truncated = true; }
  }
  if (removed) { logView.rendered = Math.max(0, logView.rendered - removed); state.log.needRebuild = true; }
  if (state.log.paused) {
    state.log.pendingCount += rows.length;
    updatePauseBadge();
    updateLogBufferInfo();
    return;
  }
  if (logFlushTimer) return;
  logFlushTimer = setTimeout(() => {
    logFlushTimer = null;
    if (state.log.needRebuild) { state.log.needRebuild = false; refilterLogs(); logScrollBottomMaybe(); }
    else flushLogAppend();
  }, 180);
}
function flushLogAppend() {
  const c = el('log-console');
  const lines = state.log.lines;
  if (logView.rendered === 0) c.innerHTML = '';
  if (logView.rendered >= lines.length) { updateLogBufferInfo(); return; }
  const near = consoleNearBottom();
  let html = '';
  for (let i = logView.rendered; i < lines.length; i++) html += logLineHtml(lines[i], { keyword: '', omitService: !!state.log.serviceFilter });
  c.insertAdjacentHTML('beforeend', html);
  logView.rendered = lines.length;
  updateLogBufferInfo();
  if (near) c.scrollTop = c.scrollHeight;
  else showLogJump(true);
}
function appendLogSeparator(text) {
  state.log.lines.push({ sep: true, text });
  if (!state.log.paused && logFlushTimer === null) {
    const c = el('log-console');
    c.insertAdjacentHTML('beforeend', logLineHtml({ sep: true, text }, {}));
    logView.rendered = state.log.lines.length;
  }
}
function consoleNearBottom() {
  const c = el('log-console');
  return c.scrollHeight - c.scrollTop - c.clientHeight < 40;
}
function onLogScroll() { showLogJump(!consoleNearBottom()); }
function showLogJump(show) { el('log-jump').classList.toggle('d-none', !show); }
function logScrollBottom() { const c = el('log-console'); c.scrollTop = c.scrollHeight; showLogJump(false); }
function logScrollBottomMaybe() { const c = el('log-console'); if (consoleNearBottom()) { c.scrollTop = c.scrollHeight; } }
function logJumpBottom() { logScrollBottom(); }

function updateLogBufferInfo() {
  const n = state.log.lines.length;
  setText('log-buffer-info', '缓冲 ' + n + (n >= logBufferMax() ? '+（已截断）' : '') + ' 行 / ' + logBufferMax());
}

/* ── 暂停 / 清屏 ── */
function toggleLogPause() {
  state.log.paused = !state.log.paused;
  const btn = el('log-pause');
  if (state.log.paused) {
    btn.innerHTML = '<i class="bi bi-play-fill me-1"></i>继续';
    el('log-paused-tip').classList.remove('d-none');
  } else {
    btn.innerHTML = '<i class="bi bi-pause-fill me-1"></i>暂停';
    el('log-paused-tip').classList.add('d-none');
    state.log.pendingCount = 0;
    updatePauseBadge();
    refilterLogs();
    logScrollBottom();
  }
  updatePauseBadge();
}
function updatePauseBadge() {
  const badge = el('log-pending-badge');
  const n = state.log.pendingCount;
  badge.textContent = '+' + n;
  badge.classList.toggle('d-none', n === 0);
}
function confirmClearLogs() {
  confirmDialog({ title: '清空日志缓冲', html: '清空当前面板缓冲？<span class="text-muted">不影响后端日志存储。</span>', level: 'L1', confirmText: '清屏' })
    .then(ok => {
      if (!ok) return;
      state.log.lines = [];
      state.log.pendingCount = 0;
      state.log.truncated = false;
      logView.rendered = 0;
      updatePauseBadge();
      refilterLogs();
    });
}

/* ── 导出 ── */
function openLogExport() {
  fillLeServiceSelect();
  leModal.show();
}
function fillLeServiceSelect() {
  const sel = el('le-service');
  const cur = state.log.serviceFilter;
  let html = '<option value="">全部服务</option><option value="platform">平台自身（platform）</option>';
  state.services.forEach(s => {
    html += '<option value="' + s.id + '" disabled>' + escapeHtml(svcTitle(s)) + '（M3 提供）</option>';
  });
  sel.innerHTML = html;
  if (cur && sel.querySelector('option[value="' + cur + '"]:not([disabled])')) sel.value = cur;
}
function onLeRangeChange() {
  el('le-custom').classList.toggle('d-none', val('le-range') !== 'custom');
}
async function doLogExport() {
  const btn = el('le-confirm');
  btnBusy(btn, '导出中…');
  const finish = () => { btnIdle(btn); leModal.hide(); };
  const q = new URLSearchParams({ service: val('le-service'), range: val('le-range') });
  if (val('le-range') === 'custom') { q.set('from', val('le-from')); q.set('to', val('le-to')); }
  try {
    // 用 fetch + blob 下载：window.location 方案在非 2xx（如选中被管服务返回 501）
    // 时会把整个 SPA 导航到 JSON 错误响应，丢失全部界面状态。
    const r = await fetch('/api/logs/export?' + q.toString());
    if (!r.ok) {
      let msg = '导出失败（HTTP ' + r.status + '）';
      try { const j = await r.json(); if (j && j.msg) msg = j.msg; } catch (e) { }
      showToast('danger', msg);
      btnIdle(btn);
      return;
    }
    const blob = await r.blob();
    downloadBlob(blob, 'cairn-logs.zip');
    showToast('success', '日志已导出');
    finish();
  } catch (e) {
    showToast('danger', '导出失败：' + e.message);
    btnIdle(btn);
  }
}