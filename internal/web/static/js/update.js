/* ══════════════════════════════════════════════
   ⑤ update 升级能力（并入管理页）：已装版本表 / TFTP 检查 /
     升级任务轮询（GET /api/upgrade/tasks/{taskId}）/
     U盘状态轮询（供 OTG tab 消费）/ 升级历史与回滚
   主理人裁决：apply 返回 taskId，统一任务轮询
   ══════════════════════════════════════════════ */

let upUsbTimer = null;
let upTaskTimer = null;

function updateStartPolling() {
  loadUpdateAll();
  // USB/OTG 已裁撤（decisions.md A2）：无轮询任务。
  if (state.update.taskActive && state.update.taskId) startTaskPolling();
}
function updateStopPolling() { upUsbTimer = stopPolling(upUsbTimer); }
function upgradeLocked() { return !!state.update.taskActive; }

async function loadUpdateAll() {
  renderRecordsLoading();
  try { await loadServices(true); } catch (e) { /* services 错误态由卡片列表呈现 */ }
  renderServiceCards();
  loadUpgradeRecords();
  renderTftpBanner();
}
function retryLoadUpdate() { loadUpdateAll(); }

function renderRecordsLoading() {
  setHtml('up-records-state', loadingState('升级历史加载中…'));
  setHtml('up-records-tbody', '');
}

function upgradeTaskTouches(serviceId) {
  const t = state.update.taskSnapshot;
  if (!t) return false;
  if (Number(t.serviceId) === Number(serviceId)) return true;
  if (t.packages && t.packages.some(p => Number(p.serviceId) === Number(serviceId))) return true;
  return false;
}

/* ── OTA 源状态卡 ── */
function renderTftpBanner() {
  const ota = (state.settings && state.settings.ota) || {};
  const b = el('up-tftp-banner');
  if (!ota.serverUrl) {
    b.innerHTML = '<i class="bi bi-hdd-network me-2" style="color:var(--warning)"></i>尚未配置远程升级服务器，OTA 不可用（仍可通过本地包升级）。' +
      '<button class="btn btn-sm btn-outline-secondary py-0 ms-2" onclick="switchSection(\'settings\')">去设置</button>';
    el('up-check-btn').disabled = true;
    el('up-check-btn').title = '请先在系统设置中配置远程升级服务器';
    return;
  }
  el('up-check-btn').disabled = upgradeLocked() || state.update.checking;
  el('up-check-btn').title = '';
  const when = state.update.checkedAt ? fmtClock(state.update.checkedAt) : '尚未检查';
  const cnt = Object.keys(state.update.available).length;
  b.innerHTML = '<i class="bi bi-hdd-network me-2" style="color:var(--primary)"></i>OTA 源：<b>' + escapeHtml(ota.serverUrl) + '</b>' +
    ' · 上次检查 ' + when + (state.update.checkedAt ? ' · ' + cnt + ' 条可用' : '') +
    ' <span class="text-muted">（静态清单 /cairn/services/index.json）</span>';
}

/* ── OTA 检查更新（新清单协议：services[]） ── */
async function checkUpdates() {
  if (upgradeLocked() || state.update.checking) return;
  const btn = el('up-check-btn');
  btnBusy(btn, '检查中…');
  state.update.checking = true;
  try {
    const r = await apiPost('/upgrade/check', {});
    state.update.check = r; /* 原始结果（enabled/offline/services） */
    state.update.checkedAt = Date.now();
    state.update.available = {}; /* name → 条目 */
    (r.services || []).forEach(it => { if (it.hasUpdate) state.update.available[it.name] = it; });
    renderServiceCards();
    renderTftpBanner();
    const n = Object.keys(state.update.available).length;
    showToast('info', r.offline ? 'OTA 服务器不可达，显示内置清单' : (n ? '发现 ' + n + ' 个可升级服务' : '全部服务已是最新版本'));
    renderUsOta();
  } catch (e) {
    showToast('danger', '检查失败：' + e.message);
  }
  state.update.checking = false;
  btnIdle(btn);
  renderTftpBanner();
}

/* ── 单服务 OTA 升级（L1 确认，apply by name） ── */
function confirmUpgradeSvc(serviceId) {
  if (upgradeLocked()) return;
  const row = svcById(serviceId);
  if (!row) return;
  const item = state.update.available[row.name];
  if (!item) return;
  confirmDialog({
    title: '升级服务（OTA）',
    html: '<b>' + escapeHtml(svcDisplayName(row)) + '</b>：<code>' + escapeHtml(row.version || '—') + '</code> → <code>' + escapeHtml(item.latest) + '</code>' +
      '<div class="mt-1 text-muted">来源 OTA 最新版 · ' + fmtBytes(item.sizeBytes || 0) + ' · 升级期间服务将短暂停止</div>',
    level: 'L1', confirmText: '确认升级',
  }).then(ok => { if (ok) { closeUpgradeSource(); startUpgrade({ name: row.name }); } });
}

async function startUpgrade(body) {
  if (upgradeLocked()) { showToast('warning', '已有升级任务进行中'); return; }
  try {
    const r = await apiPost('/upgrade/apply', body);
    state.update.taskId = r.taskId;
    state.update.taskActive = true;
    openUpgradeModal();
    startTaskPolling();
    renderTaskBanner();
    renderServiceCards();
    loadUpgradeRecords();
    renderTftpBanner();
  } catch (e) {
    showToast('danger', '升级受理失败：' + e.message);
  }
}

/* ── 新后端任务 → 旧 UI 模型适配（decisions.md D10：无 health 观察阶段） ── */
const TASK_PHASE_MAP = {
  queued:     { key: 'download', idx: 0 },
  downloading:{ key: 'download', idx: 0 },
  verifying:  { key: 'verify',   idx: 1 },
  precheck:   { key: 'precheck', idx: 2 },
  stopping:   { key: 'stop',     idx: 3 },
  swapping:   { key: 'switch',   idx: 5 }, /* install+switch 合并为 swapping */
  starting:   { key: 'health',   idx: 6 },
  success:    { key: 'commit',   idx: 7 },
};
function adaptTask(t) {
  const m = TASK_PHASE_MAP[t.phase] || TASK_PHASE_MAP.queued;
  const running = t.phase !== 'success' && t.phase !== 'failed';
  return {
    status: t.phase === 'success' ? 'success' : t.phase === 'failed' ? 'failed' : 'running',
    phase: m.key, phaseIndex: m.idx, mode: t.source === 'ota' ? 'ota' : 'manual',
    serviceName: t.name, targetVersion: t.toVersion, fromVersion: t.fromVersion,
    detail: t.error || t.detail || '', source: t.source, taskId: t.taskId,
  };
}

/* ── 任务轮询（1s，主理人裁决的统一任务接口） ── */
function startTaskPolling() {
  stopTaskPolling();
  upTaskTimer = startPolling(pollUpgradeTask, 1000);
  pollUpgradeTask();
}
function stopTaskPolling() { upTaskTimer = stopPolling(upTaskTimer); }
async function pollUpgradeTask() {
  if (!state.update.taskId) return;
  try {
    const raw = await apiGet('/upgrade/tasks/' + encodeURIComponent(state.update.taskId));
    const t = adaptTask(raw);
    state.update.taskSnapshot = t;
    renderUpgradeModal(t);
    renderTaskBanner(t);
    if (!['running'].includes(t.status)) finalizeUpgrade(raw, t);
  } catch (e) {
    /* 任务查询失败：静默重试，5 次后放弃 */
    state.update._taskErrors = (state.update._taskErrors || 0) + 1;
    if (state.update._taskErrors >= 5) {
      stopTaskPolling();
      state.update.taskActive = false; state.update.taskId = null;
      showToast('danger', '升级任务状态查询失败：' + e.message);
      renderTaskBanner();
    }
  }
}
function finalizeUpgrade(raw, t) {
  stopTaskPolling();
  state.update._taskErrors = 0;
  state.update.taskActive = false;
  state.update.taskId = null;
  if (t.status === 'success') showToast('success', '部署成功：' + (t.targetVersion || '完成') + '（启动结果由运行监控接管）');
  else showToast('danger', '部署失败：' + (t.detail || raw.error || '未知原因'));
  el('um-bg').classList.add('d-none');
  el('um-close').classList.remove('d-none');
  renderTaskBanner();
  loadServices(true).then(() => { renderServiceCards(); if (currentSection === 'service') renderServiceCards(); });
  loadUpgradeRecords();
  // 修复「升级完成后依旧提示可升级」：清单快照必须随终态刷新，
  // 否则卡片上的升级提示（available 旧快照）在版本已相同后仍然残留。
  checkUpdates();
}

/* ── 全局任务横幅（后台化观察入口） ── */
function renderTaskBanner(t) {
  const b = el('up-task-banner');
  if (!state.update.taskActive) { b.classList.add('d-none'); renderServiceCards(); renderTftpBanner(); return; }
  t = t || state.update.taskSnapshot;
  const phases = t && t.mode === 'rollback' ? ROLLBACK_PHASES : UPGRADE_PHASES;
  const phaseLabel = t && t.phase ? ((phases.find(p => p.key === t.phase) || {}).label || t.phase) : '准备中';
  const name = t && t.mode === 'usb' ? 'U盘批量升级' : (t && t.serviceName) || '服务';
  b.innerHTML = '<i class="bi bi-arrow-repeat me-2"></i><b>升级进行中：' + escapeHtml(name) + '</b> · 当前阶段：' + phaseLabel +
    ' <span class="text-muted">（点击查看进度）</span>';
  b.classList.remove('d-none');
}
function reopenUpgradeModal() {
  if (state.update.taskSnapshot) { openUpgradeModal(); renderUpgradeModal(state.update.taskSnapshot); }
}
/* ── 升级 Modal 渲染（阶段清单 + 进度条 + 结果） ── */
function openUpgradeModal() {
  el('um-bg').classList.remove('d-none');
  el('um-close').classList.add('d-none');
  umModal.show();
}
function upgradeBgContinue() { umModal.hide(); } /* 后台继续：关 Modal 不断任务 */
function closeUpgradeModal() { umModal.hide(); }

function renderUpgradeModal(t) {
  if (!t) return;
  setText('um-svc', t.mode === 'usb' ? 'U盘批量升级' : (t.serviceName || '服务'));
  const isRollback = t.mode === 'rollback';
  const phases = isRollback ? ROLLBACK_PHASES : UPGRADE_PHASES;
  if (t.batch) {
    el('um-batch').classList.remove('d-none');
    setText('um-batch-text', '第 ' + t.batch.index + '/' + t.batch.total + ' 个包：' + t.batch.packageName);
  } else el('um-batch').classList.add('d-none');

  const failedIdx = (t.status === 'failed' || t.status === 'rolledback') ? t.phaseIndex : -1;
  el('um-phases').innerHTML = phases.map((p, i) => {
    let cls = '', dot = '', meta = '';
    const isAutoRollbackTail = t.status === 'rolledback' && p.key === 'commit';
    if (i < t.phaseIndex) { cls = 'done'; dot = '<i class="bi bi-check-lg"></i>'; }
    else if (i === t.phaseIndex) {
      if (t.status === 'running') { cls = 'active'; dot = '<span class="spinner-border spinner-border-sm" style="width:11px;height:11px;border-width:2px"></span>'; }
      else if (t.status === 'failed' || t.status === 'rolledback') { cls = 'failed'; dot = '<i class="bi bi-x-lg"></i>'; }
      else { cls = 'done'; dot = '<i class="bi bi-check-lg"></i>'; }
    } else if (isAutoRollbackTail && i === t.phaseIndex + 1) {
      cls = 'rolledback'; dot = '<i class="bi bi-arrow-counterclockwise"></i>';
    }
    if (t.status === 'success') { cls = 'done'; dot = '<i class="bi bi-check-lg"></i>'; }
    if (t.status === 'running') {
      if (p.key === 'health' && t.healthTotal) meta = '观察中 ' + Math.round(t.healthElapsed || 0) + 's/' + Math.round(t.healthTotal) + 's，失败将自动回滚';
      if (p.key === 'download') meta = t.progress != null ? Math.round(t.progress) + '%' : '已接收 ' + fmtBytes(t.receivedBytes || 0);
    }
    const label = isAutoRollbackTail ? '自动回滚' : p.label;
    return '<li class="phase-item ' + cls + '"><span class="phase-dot">' + dot + '</span><span>' + label + '</span>' +
      (meta ? '<span class="phase-meta">' + meta + '</span>' : '') + '</li>';
  }).join('');

  /* 进度条：下载（可量）/ 健康观察（倒计时） */
  const wrap = el('um-progress-wrap');
  const bar = el('um-progress-bar');
  const ptxt = el('um-progress-text');
  if (t.status === 'running' && (t.phase === 'download' || t.phase === 'health')) {
    wrap.classList.remove('d-none');
    if (t.phase === 'download') {
      if (t.totalBytes) {
        bar.style.width = (t.progress || 0) + '%';
        bar.classList.remove('progress-bar-animated');
        ptxt.textContent = fmtBytes(t.receivedBytes || 0) + ' / ' + fmtBytes(t.totalBytes);
      } else {
        bar.style.width = '100%';
        bar.classList.add('progress-bar-animated'); /* 无 size：不定条纹进度 */
        ptxt.textContent = '已接收 ' + fmtBytes(t.receivedBytes || 0);
      }
    } else {
      const pct = t.healthTotal ? Math.min(100, Math.round((t.healthElapsed || 0) / t.healthTotal * 100)) : 0;
      bar.style.width = pct + '%';
      bar.classList.remove('progress-bar-animated');
      ptxt.textContent = '健康验证期 ' + Math.round(t.healthElapsed || 0) + 's / ' + Math.round(t.healthTotal || 0) + 's';
    }
  } else wrap.classList.add('d-none');

  /* 结果区 */
  const result = el('um-result');
  if (t.status === 'running') { result.classList.add('d-none'); result.innerHTML = ''; }
  else {
    result.classList.remove('d-none');
    let html = '';
    if (t.status === 'success') {
      html = '<div class="um-result-banner ok"><i class="bi bi-check-circle-fill me-2"></i>升级成功' +
        (t.targetVersion ? '，当前版本 <b>' + escapeHtml(t.targetVersion) + '</b>' : '') + '</div>';
    } else if (t.status === 'rolledback') {
      html = '<div class="um-result-banner warn"><i class="bi bi-arrow-counterclockwise me-2"></i>已自动回滚至 <b>' + escapeHtml(t.rolledBackTo || t.targetVersion || '旧版本') + '</b>（健康观察失败）</div>';
    } else if (t.status === 'failed') {
      html = '<div class="um-result-banner err"><i class="bi bi-x-octagon-fill me-2"></i>升级失败：' + escapeHtml(t.detail || '未知原因') + '</div>';
    }
    if (t.packages && t.packages.length) {
      const RS = { success: ['badge-ok', '成功'], rolledback: ['badge-r', '已回滚'], failed: ['badge-err', '失败'], pending: ['badge-muted', '未执行'] };
      html += '<div class="table-responsive mt-2"><table class="table table-sm table-hover align-middle"><thead><tr><th>包</th><th>结果</th><th>说明</th></tr></thead><tbody>' +
        t.packages.map(p => {
          const svc = svcById(p.serviceId);
          const meta = RS[p.result] || RS.pending;
          return '<tr><td>' + escapeHtml(p.packageName) + '<div class="text-muted small">' + escapeHtml(svc ? svcTitle(svc) : '#' + p.serviceId) + ' → ' + escapeHtml(p.version) + '</div></td>' +
            '<td><span class="badge ' + meta[0] + '">' + meta[1] + '</span></td><td class="text-muted small">' + escapeHtml(p.detail || '—') + '</td></tr>';
        }).join('') + '</tbody></table></div>';
    }
    if (t.status !== 'running' && t.reportNote) html += '<div class="text-muted small mt-1"><i class="bi bi-usb-symbol me-1"></i>' + escapeHtml(t.reportNote) + '</div>';
    result.innerHTML = html;
  }
}

/* ── 升级历史 ── */
async function loadUpgradeRecords() {
  try {
    state.update.records = await apiGet('/upgrade/records') || [];
    state.update.recordsError = null;
  } catch (e) { state.update.recordsError = e.message; }
  renderRecords();
}
function renderRecords() {
  const tb = el('up-records-tbody');
  const st = el('up-records-state');
  if (state.update.recordsError) {
    tb.innerHTML = '';
    st.innerHTML = errorState('升级历史加载失败：' + state.update.recordsError, 'retryLoadUpdate()');
    return;
  }
  const rows = state.update.records;
  if (!rows.length) { tb.innerHTML = ''; st.innerHTML = emptyState('bi-journal-clock', '暂无升级记录'); return; }
  st.innerHTML = '';
  const locked = upgradeLocked();
  tb.innerHTML = rows.map(r => {
    const [badgeCls, label] = RECORD_STATUS[r.status] || ['badge-muted', r.status];
    // 覆盖式升级无多版本目录，回退 = 重新上传旧版包（decisions.md B6/D1）。
    let action = '<button class="btn btn-outline-secondary btn-sm" onclick="promptLocalRollback(' + r.serviceId + ')" title="通过上传旧版本包回退"><i class="bi bi-arrow-counterclockwise me-1"></i>旧版重装</button>';
    return '<tr>' +
      '<td class="text-nowrap">' + fmtDateTime(r.createdAt) + '</td>' +
      '<td class="fw-semibold">' + escapeHtml(r.serviceName || '#' + r.serviceId) + '</code></td>' +
      '<td><code>' + escapeHtml(r.fromVersion) + '</code> → <code>' + escapeHtml(r.toVersion) + '</code></td>' +
      '<td><span class="mc-tag">' + (UPGRADE_SOURCE[r.source] || r.source) + '</span></td>' +
      '<td><span class="badge ' + badgeCls + '">' + label + '</span>' +
        (r.detail ? '<div class="text-muted small" style="max-width:260px;" title="' + escapeHtml(r.detail) + '">' + escapeHtml(r.detail) + '</div>' : '') + '</td>' +
      '<td class="text-end text-nowrap">' + action + '</td>' +
      '</tr>';
  }).join('');
}

// promptLocalRollback 引导用户走本地包回退（不再有服务端一键回滚端点）。
function promptLocalRollback(serviceId) {
  const svc = svcById(serviceId);
  showToast('info', '回退方式：获取旧版本 .cairnpkg（如从导出或服务器历史包下载），在升级页选择「本地包升级」上传安装。' + (svc ? '（' + svc.name + '）' : ''));
}

/* ══════════════ 升级来源 modal（OTA / OTG / LOCAL） ══════════════ */
/* USB/OTG 已随方案裁撤（decisions.md A2）：rescan/batch 等函数一并移除。 */
function openUpgradeSource(serviceId) {
  const row = svcById(serviceId);
  if (!row) return;
  if (upgradeLocked()) { showToast('warning', '已有升级任务进行中'); return; }
  state.update.usSvcId = serviceId;
  setText('us-svc', row.name);
  renderUsOta();
  renderUsLocal();
  usModal.show();
}
function closeUpgradeSource() { if (typeof usModal !== 'undefined' && usModal) usModal.hide(); }

/* ── OTA tab：远程最新版 ── */
function renderUsOta() {
  const body = el('us-ota-body');
  if (!body) return;
  const row = svcById(state.update.usSvcId);
  if (!row) { body.innerHTML = ''; return; }
  const ota = (state.settings && state.settings.ota) || {};
  const check = state.update.check;
  const item = (check && (check.services || []).find(s => s.name === row.name)) || null;
  let html = '<div class="info-banner mb-3"><i class="bi bi-broadcast me-2" style="color:var(--primary)"></i>OTA 源 <b>' + escapeHtml(ota.serverUrl || '未配置') + '</b>' +
    ' · 当前版本 <code>' + escapeHtml(row.version || '—') + '</code>' + (item && item.latest ? ' · 最新版 <code>' + escapeHtml(item.latest) + '</code>' : '') + '</div>';
  if (item && item.hasUpdate) {
    html += '<div class="d-flex align-items-center flex-wrap p-3" style="gap:12px;border:1px solid var(--primary-border);border-radius:10px;">' +
      '<div><div class="fw-semibold">' + escapeHtml(item.latest) + '</div>' +
      '<div class="small text-muted">' + fmtBytes(item.sizeBytes || 0) + (item.notes ? ' · ' + escapeHtml(item.notes) : '') + '</div></div>' +
      '<span class="badge badge-r ms-auto">可升级</span>' +
      '<button class="btn btn-primary" onclick="confirmUpgradeSvc(' + row.id + ')"><i class="bi bi-cloud-arrow-down me-1"></i>升级到 ' + escapeHtml(item.latest) + '</button></div>';
  } else if (item && item.latest) {
    html += '<div class="text-muted" style="padding:6px 2px;"><i class="bi bi-check-circle me-1"></i>已是最新版本 ' + escapeHtml(item.latest) + '。</div>';
  } else {
    html += '<div class="text-muted" style="padding:6px 2px;"><i class="bi bi-info-circle me-1"></i>暂无该服务的远程版本信息。' +
      (ota.serverUrl ? '点击「检查更新」从清单刷新。' : '请先在「设置」中配置远程升级服务器。') + '</div>' +
      '<button class="btn btn-outline-primary btn-sm mt-1" onclick="checkUpdates()"' + (ota.serverUrl ? '' : ' disabled') + '><i class="bi bi-arrow-repeat me-1"></i>检查更新</button>';
  }
  body.innerHTML = html;
}
/* ── LOCAL tab：上传 .cairnpkg（安装或升级，自动判别） ── */
function renderUsLocal() {
  const body = el('us-local-body');
  if (!body) return;
  const row = svcById(state.update.usSvcId);
  if (!row) { body.innerHTML = ''; return; }
  body.innerHTML =
    '<div class="mb-3"><label class="form-label fw-semibold" for="us-local-file">升级包（.cairnpkg）<span class="text-danger">*</span></label>' +
    '<input type="file" class="form-control" id="us-local-file" accept=".cairnpkg">' +
    '<div class="form-text">包内清单声明的服务与版本为准；与当前版本不一致即可升级（含回退）。</div></div>' +
    '<div class="info-banner mb-0"><i class="bi bi-info-circle me-2" style="color:var(--primary)"></i>覆盖式部署：停服 → 换文件 → 启服。数据目录 data/' + escapeHtml(row.name) + '/ 不受影响；被覆盖的用户配置保留 .bak 一份。</div>' +
    '<button class="btn btn-primary mt-3" onclick="startLocalUpgrade()"><i class="bi bi-box-arrow-in-up me-1"></i>上传并部署</button>';
}
function startLocalUpgrade() {
  if (upgradeLocked()) { showToast('warning', '已有升级任务进行中'); return; }
  const row = svcById(state.update.usSvcId);
  if (!row) return;
  const input = el('us-local-file');
  const file = input && input.files && input.files[0];
  if (!file) { showToast('warning', '请选择 .cairnpkg 包文件'); return; }
  const fd = new FormData();
  fd.append('file', file);
  btnBusy(el('um-confirm') || input, '上传中…');
  fetch('/api/upgrade/local', { method: 'POST', body: fd })
    .then(async r => {
      const j = await r.json().catch(() => ({}));
      if (!r.ok || j.code) throw new Error(j.msg || ('HTTP ' + r.status));
      return j.data;
    })
    .then(d => {
      closeUpgradeSource();
      state.update.taskId = d.taskId;
      state.update.taskActive = true;
      openUpgradeModal();
      startTaskPolling();
      renderTaskBanner();
      showToast('info', '已受理（' + (d.mode === 'upgrade' ? '升级' : '安装') + '），任务执行中');
    })
    .catch(e => showToast('danger', '上传失败：' + e.message));
}