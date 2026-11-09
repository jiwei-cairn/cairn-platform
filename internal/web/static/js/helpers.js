/* ══════════════════════════════════════════════
   通用工具 + 区块切换 + 确认弹窗 + Toast + 轮询
   ══════════════════════════════════════════════ */

/* ── DOM 小工具 ── */
function el(id) { return document.getElementById(id); }
function val(id) { const e = el(id); return e ? String(e.value).trim() : ''; }
function setVal(id, v) { const e = el(id); if (e) e.value = (v === null || v === undefined) ? '' : v; }
function setText(id, t) { const e = el(id); if (e) e.textContent = (t === null || t === undefined) ? '—' : String(t); }
function setHtml(id, h) { const e = el(id); if (e) e.innerHTML = h; }
function checked(id) { const e = el(id); return !!(e && e.checked); }
function setChecked(id, v) { const e = el(id); if (e) e.checked = !!v; }
function escapeHtml(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c])); }
function toNum(v, def) { if (v === '' || v === null || v === undefined) return def; const n = Number(v); return isNaN(n) ? def : n; }
function deepCopy(o) { return (o === undefined) ? undefined : JSON.parse(JSON.stringify(o)); }
function clamp(v, min, max) { return Math.min(max, Math.max(min, v)); }
function pad2(n) { return String(n).padStart(2, '0'); }

/* ── 运行平台判定 ──
   state.version.os 形如 "windows · amd64" / "linux · arm64"（后端 goruntimeOS
   拼接 GOOS + " · " + GOARCH），故只能按前缀判定，不可整体等值比较。 */
function versionOS() { return String((state.version && state.version.os) || '').toLowerCase(); }
function isWindowsPlatform() { return versionOS().indexOf('windows') === 0; }

/* ── 服务展示名 ──
   summary 是面向人的展示名（可中文），name 是安装目录名（唯一标识）。
   展示名优先；两者不同时用「展示名（目录名）」让标识仍可见，
   避免同一服务在不同页面显示成两个不相干的字符串。 */
function svcDisplayName(s) {
  if (!s) return '';
  const name = String(s.name || '');
  const summary = String(s.summary || '').trim();
  if (!summary || summary === name) return name;
  return summary + '（' + name + '）';
}
/* 仅展示名（不含目录名），用于空间紧张处（如下拉选项、表格）。 */
function svcTitle(s) {
  if (!s) return '';
  const summary = String(s.summary || '').trim();
  return summary || String(s.name || '');
}

/* ── 格式化 ── */
function fmtClock(ts) { const d = new Date(ts); return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()); }
function fmtDateTime(ts) { const d = new Date(ts); return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + fmtClock(ts); }
function fmtUptime(sec) {
  if (sec === null || sec === undefined || isNaN(sec)) return '—';
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (d > 0) return d + 'd ' + h + 'h';
  if (h > 0) return h + 'h ' + m + 'm';
  if (m > 0) return m + 'm ' + s + 's';
  return s + 's';
}
function fmtBytes(b) {
  if (b === null || b === undefined || isNaN(b)) return '—';
  if (b >= 1073741824) return (b / 1073741824).toFixed(2) + ' GB';
  if (b >= 1048576) return (b / 1048576).toFixed(1) + ' MB';
  if (b >= 1024) return (b / 1024).toFixed(1) + ' KB';
  return b + ' B';
}
function fmtMB(mb) { return (mb === null || mb === undefined || isNaN(mb)) ? '—' : (mb >= 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb.toFixed(0) + ' MB'); }
function fmtPct(v) { return (v === null || v === undefined || isNaN(v)) ? '—' : Number(v).toFixed(1) + '%'; }
function fmtKBps(v) { return (v === null || v === undefined || isNaN(v)) ? '—' : (v >= 1024 ? (v / 1024).toFixed(1) + ' MB/s' : v.toFixed(1) + ' KB/s'); }
function debounce(fn, ms) { let t = null; return function (...a) { clearTimeout(t); t = setTimeout(() => fn.apply(this, a), ms); }; }

/* ── 下载 ── */
function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url; a.download = filename;
  document.body.appendChild(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
}
function downloadText(text, filename) { downloadBlob(new Blob([text], { type: 'text/plain;charset=utf-8' }), filename); }

/* ── 三态占位（加载 / 空白 / 错误） ── */
function loadingState(text) {
  return '<div class="empty-state"><div class="spinner-border spinner-border-sm me-2" style="color:var(--primary)"></div>' + escapeHtml(text || '加载中…') + '</div>';
}
function emptyState(icon, text, extra) {
  return '<div class="empty-state"><i class="bi ' + icon + '"></i><div>' + text + '</div>' + (extra || '') + '</div>';
}
function errorState(msg, retryExpr) {
  return '<div class="empty-state"><i class="bi bi-exclamation-circle"></i><div>' + escapeHtml(msg) + '</div>' +
    '<button class="btn btn-outline-secondary btn-sm mt-2" onclick="' + retryExpr + '"><i class="bi bi-arrow-clockwise me-1"></i>重试</button></div>';
}

/* ── 轮询 ── */
function startPolling(fn, ms) { return setInterval(fn, ms); }
function stopPolling(h) { if (h) clearInterval(h); return null; }

/* ── 按钮 spinner（交互说明 5.2.1） ── */
function btnBusy(btn, text) {
  if (!btn) return;
  btn.dataset.origHtml = btn.innerHTML;
  btn.dataset.origDisabled = btn.disabled ? '1' : '';
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner-border spinner-border-sm me-1"></span>' + escapeHtml(text || '处理中…');
}
function btnIdle(btn) {
  if (!btn || !btn.dataset.origHtml) return;
  btn.innerHTML = btn.dataset.origHtml;
  if (!btn.dataset.origDisabled) btn.disabled = false;
  delete btn.dataset.origHtml; delete btn.dataset.origDisabled;
}

/* ══════════════ 区块切换（显隐 + 高亮 + 各区块启停钩子） ══════════════ */
let currentSection = '';
function switchSection(key) {
  currentSection = key;
  document.querySelectorAll('.app-section').forEach(s => s.classList.add('d-none'));
  el('section-' + key).classList.remove('d-none');
  document.querySelectorAll('.sidebar-item').forEach(i => i.classList.remove('active'));
  el('nav-' + key).classList.add('active');
  if (['dash', 'service'].includes(key)) dashboardStartPolling(); else dashboardStopPolling();
  if (key === 'service') { renderServiceCards(); updateStartPolling(); } else updateStopPolling();
  if (key === 'dash') sparkStart(); else sparkStop();
  if (key === 'market') marketEnter(); else marketLeave();
  if (key === 'log') logCenterEnter(); else logCenterLeave();
  if (key === 'settings') settingsEnter(); else settingsLeave();
}

/* ══════════════ 统一确认弹窗（单例 #confirm-modal，L1/L2） ══════════════ */
let _confirmResolvers = [];
function confirmDialog(opts) {
  opts = opts || {};
  return new Promise(resolve => {
    setText('cm-title', opts.title || '确认');
    setHtml('cm-body', opts.html || '');
    const btn = el('cm-confirm');
    const isL2 = opts.level === 'L2';
    btn.className = 'btn ' + (isL2 ? 'btn-danger' : 'btn-primary');
    btn.innerHTML = '<i class="bi ' + (opts.confirmIcon || (isL2 ? 'bi-exclamation-triangle-fill' : 'bi-check-lg')) + ' me-1"></i>' + escapeHtml(opts.confirmText || '确认');
    _confirmResolvers.push(resolve);
    confirmModal.show();
    /* 默认焦点落在「取消」，防止回车误触发（交互说明 5.1） */
    setTimeout(() => { const c = el('cm-cancel'); if (c) c.focus(); }, 80);
  });
}
function _confirmDone(v) {
  if (typeof confirmModal !== 'undefined' && confirmModal) confirmModal.hide();
  const list = _confirmResolvers; _confirmResolvers = [];
  list.forEach(r => r(v));
}

/* ══════════════ Toast（右上角，替代 alert） ══════════════ */
const TOAST_META = {
  success: { icon: 'bi-check-circle-fill',           cls: 'toast-success', delay: 3000, role: 'status' },
  info:    { icon: 'bi-info-circle-fill',            cls: 'toast-info',    delay: 3000, role: 'status' },
  warning: { icon: 'bi-exclamation-triangle-fill',   cls: 'toast-warning', delay: 5000, role: 'status' },
  danger:  { icon: 'bi-x-octagon-fill',              cls: 'toast-danger',  delay: 6000, role: 'alert' },
};
const _toastActions = [];
let _toastLast = { msg: '', at: 0 };
function _runToastAction(i) { const fn = _toastActions[i]; if (fn) fn(); }
function showToast(level, message, opts) {
  const meta = TOAST_META[level] || TOAST_META.info;
  const now = Date.now();
  if (_toastLast.msg === message && now - _toastLast.at < 3000) return; // 同类节流 3s
  _toastLast = { msg: message, at: now };
  const box = el('toast-box');
  if (!box) return;
  while (box.children.length >= 4) box.removeChild(box.firstChild);     // 堆叠上限 4
  let actionHtml = '';
  if (opts && opts.action && opts.action.label) {
    const idx = _toastActions.length; _toastActions[idx] = opts.action.fn;
    actionHtml = ' <button class="btn btn-sm btn-link p-0 ms-2 fw-semibold" style="font-size:12.5px;color:var(--primary)" onclick="_runToastAction(' + idx + ')">' + escapeHtml(opts.action.label) + '</button>';
  }
  const t = document.createElement('div');
  t.className = 'toast ' + meta.cls;
  t.setAttribute('role', meta.role);
  t.innerHTML = '<div class="toast-body d-flex align-items-start">' +
    '<i class="bi ' + meta.icon + ' me-2 mt-1" style="font-size:15px;color:var(--' + (level === 'success' ? 'success' : level === 'warning' ? 'warning' : level === 'danger' ? 'danger' : 'primary') + ')"></i>' +
    '<div class="flex-grow-1" style="font-size:13.5px;line-height:1.5">' + escapeHtml(message) + actionHtml + '</div>' +
    '<button type="button" class="btn-close ms-2" data-bs-dismiss="toast" aria-label="关闭"></button></div>';
  box.appendChild(t);
  const bsToast = new bootstrap.Toast(t, { delay: meta.delay });
  let remain = meta.delay, hideTimer = null, startAt = Date.now();
  hideTimer = setTimeout(() => bsToast.hide(), remain);
  t.addEventListener('mouseenter', () => { clearTimeout(hideTimer); remain = Math.max(800, remain - (Date.now() - startAt)); }); // hover 暂停计时
  t.addEventListener('mouseleave', () => { startAt = Date.now(); hideTimer = setTimeout(() => bsToast.hide(), remain); });
  t.addEventListener('hidden.bs.toast', () => { clearTimeout(hideTimer); t.remove(); });
  bsToast.show();
}