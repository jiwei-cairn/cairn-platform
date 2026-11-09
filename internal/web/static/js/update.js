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
  upUsbTimer = startPolling(loadUsbStatus, 5000);
  if (state.update.taskActive && state.update.taskId) startTaskPolling();
}
function updateStopPolling() { upUsbTimer = stopPolling(upUsbTimer); }
function upgradeLocked() { return !!state.update.taskActive; }

async function loadUpdateAll() {
  renderRecordsLoading();
  try { await loadServices(true); } catch (e) { /* services 错误态由卡片列表呈现 */ }
  renderServiceCards();
  loadUpgradeRecords();
  loadUsbStatus();
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

/* ── TFTP 源状态卡 ── */
function renderTftpBanner() {
  const tftp = (state.settings && state.settings.tftp) || {};
  const b = el('up-tftp-banner');
  if (!tftp.server) {
    b.innerHTML = '<i class="bi bi-hdd-network me-2" style="color:var(--warning)"></i>尚未配置 TFTP 服务端，在线升级不可用。' +
      '<button class="btn btn-sm btn-outline-secondary py-0 ms-2" onclick="switchSection(\'settings\')">去设置</button>';
    el('up-check-btn').disabled = true;
    el('up-check-btn').title = '请先在系统设置中配置 TFTP 服务端';
    return;
  }
  el('up-check-btn').disabled = upgradeLocked() || state.update.checking;
  el('up-check-btn').title = '';
  const when = state.update.checkedAt ? fmtClock(state.update.checkedAt) : '尚未检查';
  const cnt = Object.keys(state.update.available).length;
  b.innerHTML = '<i class="bi bi-hdd-network me-2" style="color:var(--primary)"></i>OTA（TFTP）源：<b>' + escapeHtml(tftp.server) + ':' + tftp.port + '</b>' +
    ' · 上次检查 ' + when + (state.update.checkedAt ? ' · 清单 ' + cnt + ' 条可用' : '') +
    ' <span class="text-muted">（清单路径 ' + escapeHtml(tftp.indexJsonPath || '/index.json') + '）</span>';
}

/* ── TFTP 检查更新 ── */
async function checkUpdates() {
  if (upgradeLocked() || state.update.checking) return;
  const btn = el('up-check-btn');
  btnBusy(btn, '检查中…');
  state.update.checking = true;
  try {
    const r = await apiPost('/upgrade/check', {});
    state.update.checkedAt = r.checkedAt || Date.now();
    state.update.available = {};
    (r.items || []).forEach(it => { state.update.available[it.serviceId] = it; });
    renderServiceCards();
    renderTftpBanner();
    showToast('info', (r.items || []).length ? '发现 ' + r.items.length + ' 个可用更新' : '当前已是最新版本');
    renderUsOta(); /* 升级来源 modal 若打开则同步刷新 */
  } catch (e) {
    showToast('danger', '检查失败：' + e.message);
  }
  state.update.checking = false;
  btnIdle(btn);
  renderTftpBanner();
}

/* ── 单服务升级（L1 确认） ── */
function confirmUpgradeSvc(serviceId) {
  if (upgradeLocked()) return;
  const row = svcById(serviceId);
  const item = state.update.available[serviceId];
  if (!row || !item) return;
  confirmDialog({
    title: '升级服务',
    html: '<b>' + escapeHtml(svcDisplayName(row)) + '</b>：<code>' + escapeHtml(row.version || '—') + '</code> → <code>' + escapeHtml(item.version) + '</code>' +
      '<div class="mt-1 text-muted">来源 TFTP · 包 ' + escapeHtml(item.packageName) + ' · 升级期间服务将短暂停止</div>' +
      '<div class="mt-1 text-muted" style="font-size:12px;">sha256 ' + escapeHtml(String(item.sha256 || '').slice(0, 16)) + '… · ' + fmtBytes(item.sizeBytes) + '</div>',
    level: 'L1', confirmText: '确认升级',
  }).then(ok => { if (ok) { closeUpgradeSource(); startUpgrade({ source: 'tftp', serviceId, version: item.version }); } });
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
    renderRecords();
    renderTftpBanner();
  } catch (e) {
    showToast('danger', '升级受理失败：' + e.message);
  }
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
    const t = await apiGet('/upgrade/tasks/' + encodeURIComponent(state.update.taskId));
    state.update.taskSnapshot = t;
    renderUpgradeModal(t);
    renderTaskBanner(t);
    if (t.status !== 'running') finalizeUpgrade(t);
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
function finalizeUpgrade(t) {
  stopTaskPolling();
  state.update._taskErrors = 0;
  state.update.taskActive = false;
  state.update.taskId = null;
  if (t.status === 'success') showToast('success', '升级成功：' + (t.targetVersion || '完成'));
  else if (t.status === 'rolledback') showToast('warning', '健康观察失败，已自动回滚至 ' + (t.rolledBackTo || t.targetVersion));
  else if (t.status === 'partial') showToast('warning', 'U盘批量升级完成：部分包失败或已回滚，详见结果汇总');
  else showToast('danger', '升级失败：' + (t.detail || '未知原因'));
  el('um-bg').classList.add('d-none');
  el('um-close').classList.remove('d-none');
  renderTaskBanner();
  loadServices(true).then(() => { renderServiceCards(); if (currentSection === 'service') renderServiceCards(); });
  loadUpgradeRecords();
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
    let action = '—';
    if (r.canRollback) {
      action = '<button class="btn btn-outline-secondary btn-sm" onclick="rollbackVersion(' + r.id + ')"' + (locked ? ' disabled title="升级进行中"' : ' title="回滚到 ' + escapeHtml(r.fromVersion) + '"') + '><i class="bi bi-arrow-counterclockwise me-1"></i>回滚</button>';
    }
    return '<tr>' +
      '<td class="text-nowrap">' + fmtDateTime(r.createdAt) + '</td>' +
      '<td class="fw-semibold">' + escapeHtml(r.serviceName || '#' + r.serviceId) + '</td>' +
      '<td><code>' + escapeHtml(r.fromVersion) + '</code> → <code>' + escapeHtml(r.toVersion) + '</code></td>' +
      '<td><span class="mc-tag">' + (UPGRADE_SOURCE[r.source] || r.source) + '</span></td>' +
      '<td><span class="badge ' + badgeCls + '">' + label + '</span>' +
        (r.detail ? '<div class="text-muted small" style="max-width:260px;" title="' + escapeHtml(r.detail) + '">' + escapeHtml(r.detail) + '</div>' : '') + '</td>' +
      '<td class="text-end text-nowrap">' + action + '</td>' +
      '</tr>';
  }).join('');
}

/* ── 版本回滚（L2 强确认） ── */
function rollbackVersion(recordId) {
  if (upgradeLocked()) return;
  const rec = state.update.records.find(r => r.id === recordId);
  if (!rec) return;
  const svc = svcById(rec.serviceId);
  const target = rec.fromVersion;
  confirmDialog({
    title: '版本回滚',
    html: '<b>' + escapeHtml(rec.serviceName || '') + '</b>：<code>' + escapeHtml(rec.toVersion) + '</code> → <code>' + escapeHtml(target) + '</code>（上一版本）' +
      '<ul class="mt-2 mb-0 ps-3" style="font-size:13px;">' +
      '<li class="mt-1"><i class="bi bi-exclamation-triangle-fill me-1" style="color:var(--danger)"></i>服务将停止后切换，健康观察失败将停留在回滚版本</li>' +
      '<li class="mt-1"><i class="bi bi-exclamation-triangle-fill me-1" style="color:var(--danger)"></i>当前版本 ' + escapeHtml(rec.toVersion) + ' 将转为备用版本（保留 ' + ((state.settings && state.settings.collect && state.settings.collect.keepVersions) || 2) + ' 版）</li>' +
      '</ul>',
    level: 'L2', confirmText: '确认回滚', confirmIcon: 'bi-arrow-counterclockwise',
  }).then(ok => {
    if (!ok) return;
    apiPost('/upgrade/records/' + recordId + '/rollback', {})
      .then(r => {
        state.update.taskId = r.taskId;
        state.update.taskActive = true;
        openUpgradeModal();
        startTaskPolling();
        renderTaskBanner();
        renderServiceCards();
        renderRecords();
      })
      .catch(e => showToast('danger', '回滚受理失败：' + e.message));
  });
}

/* ── U盘状态轮询（GET /api/usb/status，供升级来源 modal OTG tab 消费） ── */
async function loadUsbStatus() {
  try {
    state.update.usb = await apiGet('/usb/status');
    state.update.usbError = null;
  } catch (e) {
    state.update.usbError = e.message;
    // 501 = OTG 能力尚未实现（M2）。永久性不可用不应每 5s 重试：
    // 停掉轮询，等用户手动打开 OTG tab 或点重新扫描时再触发。
    if (e.status === 501) {
      upUsbTimer = stopPolling(upUsbTimer);
    }
  }
  renderUsOtg();
}
async function rescanUsb() {
  try {
    state.update.usb = await apiGet('/usb/status?rescan=1');
    if (state.update.usb.scan) state.update.usb.scan.state = 'scanning';
    renderUsOtg();
    showToast('info', '已触发重新扫描');
    setTimeout(loadUsbStatus, 2600);
  } catch (e) { showToast('danger', '扫描失败：' + e.message); }
}

/* ── U盘批量升级（L2 强确认） ── */
function confirmUsbUpgrade() {
  if (upgradeLocked()) return;
  const usb = state.update.usb || {};
  const pkgs = ((usb.scan && usb.scan.packages) || []).filter(p => p.signOk);
  const bad = ((usb.scan && usb.scan.packages) || []).filter(p => !p.signOk);
  if (!pkgs.length) return;
  const lines = pkgs.map(p => {
    const svc = svcById(p.serviceId);
    return '<li class="mt-1"><b>' + escapeHtml(svc ? svcTitle(svc) : '#' + p.serviceId) + '</b>：<code>' + escapeHtml(svc ? (svc.version || '—') : '—') + '</code> → <code>' + escapeHtml(p.version) + '</code></li>';
  }).join('');
  confirmDialog({
    title: 'U盘批量升级',
    html: '将升级以下 <b>' + pkgs.length + '</b> 个服务：<ul class="mt-1 mb-0 ps-3" style="font-size:13px;">' + lines + '</ul>' +
      '<div class="mt-2 text-muted" style="font-size:12.5px;">升级期间相关服务按依赖逆序短暂停止，全部结果写回 U盘 report.txt。</div>' +
      (bad.length ? '<div class="mt-1" style="color:var(--warning)"><i class="bi bi-exclamation-triangle-fill me-1"></i>' + bad.length + ' 个包验签失败已跳过</div>' : '') +
      '<ul class="mt-2 mb-0 ps-3" style="font-size:13px;"><li class="mt-1"><i class="bi bi-exclamation-triangle-fill me-1" style="color:var(--danger)"></i>多包升级为原子流水线，任一包健康观察失败将单独回滚该包</li></ul>',
    level: 'L2', confirmText: '确认批量升级', confirmIcon: 'bi-usb-symbol',
  }).then(ok => { if (ok) { closeUpgradeSource(); startUpgrade({ source: 'usb' }); } });
}

/* ══════════════ 升级来源 modal（OTA / OTG / LOCAL） ══════════════ */
function openUpgradeSource(serviceId) {
  const row = svcById(serviceId);
  if (!row) return;
  if (upgradeLocked()) { showToast('warning', '已有升级任务进行中'); return; }
  state.update.usSvcId = serviceId;
  setText('us-svc', row.name);
  renderUsOta();
  renderUsOtg();
  renderUsLocal();
  usModal.show();
}
function closeUpgradeSource() { if (typeof usModal !== 'undefined' && usModal) usModal.hide(); }

/* ── OTA tab：远程版本选择 ── */
function renderUsOta() {
  const body = el('us-ota-body');
  if (!body) return;
  const svcId = state.update.usSvcId;
  const row = svcId ? svcById(svcId) : null;
  if (!row) { body.innerHTML = ''; return; }
  const avail = state.update.available[svcId];
  const tftp = (state.settings && state.settings.tftp) || {};
  let html = '<div class="info-banner mb-3"><i class="bi bi-broadcast me-2" style="color:var(--primary)"></i>远程源 <b>' + escapeHtml(tftp.server || '未配置') + (tftp.server ? ':' + tftp.port : '') + '</b>' +
    ' · 当前版本 <code>' + escapeHtml(row.version || '—') + '</code></div>';
  if (avail) {
    html += '<div class="d-flex align-items-center flex-wrap p-3" style="gap:12px;border:1px solid var(--primary-border);border-radius:10px;">' +
      '<div><div class="fw-semibold">' + escapeHtml(avail.packageName) + '</div>' +
      '<div class="small text-muted">' + fmtBytes(avail.sizeBytes) + ' · sha256 ' + escapeHtml(String(avail.sha256 || '').slice(0, 16)) + '…</div></div>' +
      '<span class="badge badge-r ms-auto">可升级 ' + escapeHtml(avail.version) + '</span>' +
      '<button class="btn btn-primary" onclick="confirmUpgradeSvc(' + svcId + ')"><i class="bi bi-cloud-arrow-down me-1"></i>升级到 ' + escapeHtml(avail.version) + '</button></div>';
  } else {
    html += '<div class="text-muted" style="padding:6px 2px;"><i class="bi bi-info-circle me-1"></i>暂无该服务的可用远程版本。' +
      (tftp.server ? '点击「检查更新」从清单刷新。' : '请先在「设置」中配置 TFTP 服务端。') + '</div>' +
      '<button class="btn btn-outline-primary btn-sm mt-1" onclick="checkUpdates()"' + (tftp.server ? '' : ' disabled') + '><i class="bi bi-arrow-repeat me-1"></i>检查更新</button>';
  }
  body.innerHTML = html;
}
/* ── OTG tab：U盘升级包（原独立 OTG 入口卡并入此处） ── */
function renderUsOtg() {
  const body = el('us-otg-body');
  if (!body) return;
  const svcId = state.update.usSvcId;
  const usb = state.update.usb || {};
  const dir = (state.settings && state.settings.usb && state.settings.usb.upgradeDir) || 'upgrade';
  /* 状态轮询失败：错误态 + 重试 */
  if (state.update.usbError) { body.innerHTML = errorState('U盘状态获取失败：' + state.update.usbError, 'loadUsbStatus()'); return; }
  /* 首次加载中：尚未拉取过状态 */
  if (!state.update.usb) { body.innerHTML = loadingState('U盘状态检测中…'); return; }
  if (usb.status === 'absent') {
    body.innerHTML = emptyState('bi-usb-symbol', '未检测到 U盘，插入后自动扫描 <b>' + escapeHtml(dir) + '/</b> 目录');
    return;
  }
  /* 接入后扫描中 */
  if (usb.scan && usb.scan.state === 'scanning') {
    body.innerHTML = '<div class="empty-state"><div class="spinner-border spinner-border-sm me-2" style="color:var(--primary)"></div>' +
      '已接入 <b>' + escapeHtml(usb.label || usb.device) + '</b>，正在扫描 ' + escapeHtml(dir) + '/ 目录…</div>';
    return;
  }
  /* 盘符信息行 + 重新扫描 */
  let html = '<div class="d-flex align-items-center flex-wrap mb-2" style="gap:10px;">' +
    '<span class="led led-on"></span><b>' + escapeHtml(usb.label || usb.device) + '</b>' +
    '<span class="text-muted small">' + escapeHtml(usb.device) + ' · ' + (usb.capacityGB || '—') + 'GB</span>' +
    '<button class="btn btn-outline-secondary btn-sm ms-auto" onclick="rescanUsb()" title="重新扫描"><i class="bi bi-arrow-repeat me-1"></i>重新扫描</button></div>';
  const pkgs = (usb.scan && usb.scan.packages) || [];
  if (!pkgs.length) {
    body.innerHTML = html + '<div class="info-banner mb-0"><i class="bi bi-info-circle-fill me-2" style="color:var(--primary)"></i>U盘已接入，' + escapeHtml(dir) + '/ 目录未发现有效升级包</div>';
    return;
  }
  const mine = pkgs.filter(p => Number(p.serviceId) === Number(svcId) && p.signOk);
  const bad = pkgs.filter(p => !p.signOk);
  html += '<div class="table-responsive"><table class="table table-sm table-hover align-middle"><thead><tr><th>包</th><th>目标服务</th><th>版本</th><th>验签</th></tr></thead><tbody>' +
    pkgs.map(p => {
      const svc = svcById(p.serviceId);
      return '<tr' + (p.signOk ? '' : ' style="opacity:.55"') + '><td class="text-truncate" style="max-width:220px;" title="' + escapeHtml(p.packageName) + '">' + escapeHtml(p.packageName) + '</td>' +
        '<td class="fw-semibold">' + escapeHtml(svc ? svcTitle(svc) : '#' + p.serviceId) + (mine.includes(p) ? ' <span class="badge badge-ok ms-1">本服务</span>' : '') + '</td>' +
        '<td><code>' + escapeHtml(p.version) + '</code></td>' +
        '<td>' + (p.signOk ? '<span class="badge badge-ok">通过</span>' : '<span class="badge badge-err" title="' + escapeHtml(p.reason || '') + '">失败</span>') + '</td></tr>';
    }).join('') + '</tbody></table></div>';
  if (mine.length) {
    html += '<div class="d-flex align-items-center mt-2 flex-wrap" style="gap:10px;"><span class="text-muted small">U盘升级将按依赖拓扑序批量执行全部有效包 · 结果写回 U盘 <code>report.txt</code></span>' +
      '<button class="btn btn-primary btn-sm ms-auto" onclick="confirmUsbUpgrade()"' + (upgradeLocked() ? ' disabled' : '') + '><i class="bi bi-usb-symbol me-1"></i>执行 U盘升级</button></div>';
  } else {
    html += '<div class="text-muted small mt-2"><i class="bi bi-info-circle me-1"></i>U盘中没有该服务的有效包，可执行批量升级其余包</div>' +
      '<div class="mt-1"><button class="btn btn-outline-primary btn-sm" onclick="confirmUsbUpgrade()"' + (upgradeLocked() ? ' disabled' : '') + '><i class="bi bi-usb-symbol me-1"></i>批量升级其余包</button></div>';
  }
  if (bad.length) html += '<div class="text-muted small mt-1"><i class="bi bi-exclamation-triangle-fill me-1" style="color:var(--warning)"></i>' + bad.length + ' 个包验签失败已跳过</div>';
  body.innerHTML = html;
}

/* ── LOCAL tab：本地包路径升级 ── */
function renderUsLocal() {
  const body = el('us-local-body');
  if (!body) return;
  const row = svcById(state.update.usSvcId);
  if (!row) { body.innerHTML = ''; return; }
  /* 路径示例按运行平台给出，避免 Windows 用户照抄 Linux 路径。 */
  const example = isWindowsPlatform()
    ? 'C:\\packages\\' + row.name + '_v1.0.0_windows_amd64.zip'
    : '/opt/packages/' + row.name + '_v1.0.0_linux_arm64.pkg';
  const hint = isWindowsPlatform()
    ? '设备本机文件系统中的升级包绝对路径（例如工程师通过共享目录预先拷贝）'
    : '设备本机文件系统中的升级包绝对路径（例如工程师通过 SCP 预先拷贝）';
  body.innerHTML =
    '<div class="mb-3"><label class="form-label fw-semibold" for="us-local-path">本地包路径 <span class="text-danger">*</span></label>' +
    '<input type="text" class="form-control" id="us-local-path" placeholder="' + escapeHtml(example) + '">' +
    '<div class="form-text">' + escapeHtml(hint) + '</div></div>' +
    '<div class="mb-3"><label class="form-label fw-semibold" for="us-local-version">目标版本 <span class="text-danger">*</span></label>' +
    '<input type="text" class="form-control" id="us-local-version" placeholder="v1.0.0"></div>' +
    '<div class="info-banner mb-0"><i class="bi bi-info-circle me-2" style="color:var(--primary)"></i>LOCAL 升级沿用统一任务流水线：下载阶段为本地拷贝，验签失败将中止且服务不受影响。</div>' +
    '<button class="btn btn-primary mt-3" onclick="startLocalUpgrade()"><i class="bi bi-hdd me-1"></i>开始 LOCAL 升级</button>';
}
function startLocalUpgrade() {
  if (upgradeLocked()) return;
  const row = svcById(state.update.usSvcId);
  if (!row) return;
  const p = val('us-local-path');
  const v = val('us-local-version');
  if (!p || !v) { showToast('warning', '请填写本地包路径与目标版本'); return; }
  confirmDialog({
    title: 'LOCAL 本地升级',
    html: '<b>' + escapeHtml(svcDisplayName(row)) + '</b>：<code>' + escapeHtml(row.version || '—') + '</code> → <code>' + escapeHtml(v) + '</code>' +
      '<div class="mt-1 text-muted">包 ' + escapeHtml(p) + ' · 升级期间服务将短暂停止</div>',
    level: 'L1', confirmText: '确认升级',
  }).then(ok => {
    if (!ok) return;
    closeUpgradeSource();
    startUpgrade({ source: 'local', serviceId: row.id, version: v, localPath: p });
  });
}