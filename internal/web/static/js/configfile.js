/* ══════════════════════════════════════════════
   ③ configfile 配置文件编辑 Modal：
   读 → 编辑（脏标记/Tab 两空格）→ 校验 → 保存（自动备份）
   → 生效（restart 重启 / command 热加载）→ 版本历史恢复
   ══════════════════════════════════════════════ */

const cfState = { serviceId: null, name: '', path: '', baseline: '', loaded: false, applyMode: 'restart', validateCmd: '', history: [] };
let cfConfirmedClose = false;

function cfRow() { return svcById(cfState.serviceId); }

function openConfigfile() {
  const row = cfRow() || svcById(state.editingServiceId);
  if (!row) return;
  if (!row.configFile || !row.configFile.path) { showToast('warning', '该服务未配置文件路径'); return; }
  cfState.serviceId = row.id;
  cfState.name = row.name;
  cfState.path = row.configFile.path;
  cfState.applyMode = (row.configFile || {}).applyMode || 'restart';
  cfState.validateCmd = (row.configFile || {}).validateCmd || '';
  cfState.loaded = false;
  cfState.baseline = '';
  svcModal.hide();                    /* 返回时由 hidden 事件带回服务 Modal */
  setText('cf-svc-name', row.name);
  setText('cf-path', row.configFile.path);
  el('cf-dirty').classList.add('d-none');
  setHtml('cf-out', ''); el('cf-out').classList.add('d-none');
  setText('cf-status', '就绪');
  const vBtn = el('cf-validate');
  vBtn.disabled = !cfState.validateCmd;
  vBtn.title = cfState.validateCmd ? '' : '该服务未配置校验命令';
  const applyBtn = el('cf-apply');
  applyBtn.innerHTML = '<i class="bi bi-check-lg me-1"></i>保存并生效（' + (APPLY_MODE_LABEL[cfState.applyMode] || '重启服务') + '）';
  cfModal.show();
  loadConfigfile();
}

async function loadConfigfile() {
  setHtml('cf-state', loadingState('配置读取中…'));
  el('cf-editor').value = '';
  el('cf-editor').classList.add('d-none');
  setHtml('cf-history-tbody', '');
  try {
    const d = await apiGet('/services/' + cfState.serviceId + '/configfile');
    const editor = el('cf-editor');
    editor.value = d.content;
    editor.classList.remove('d-none');
    cfState.baseline = d.content;
    cfState.loaded = true;
    setHtml('cf-state', '');
    setText('cf-status', '已加载 ' + fmtBytes(d.sizeBytes) + ' · ' + new Date().toLocaleTimeString());
    el('cf-dirty').classList.add('d-none');
    await loadConfigHistory();
  } catch (e) {
    setHtml('cf-state', errorState('配置文件加载失败：' + e.message, 'retryLoadConfigfile()'));
  }
}
function retryLoadConfigfile() { loadConfigfile(); }

async function loadConfigHistory() {
  try {
    cfState.history = await apiGet('/services/' + cfState.serviceId + '/configfile/history') || [];
    renderConfigHistory();
  } catch (e) {
    setHtml('cf-history-tbody', '<tr><td colspan="4" class="text-muted">历史加载失败：' + escapeHtml(e.message) + '</td></tr>');
  }
}
function renderConfigHistory() {
  const rows = cfState.history;
  if (!rows.length) {
    setHtml('cf-history-tbody', '<tr><td colspan="4" class="text-muted small">暂无备份（每次保存自动创建）</td></tr>');
    return;
  }
  setHtml('cf-history-tbody', rows.map(h =>
    '<tr>' +
    '<td class="text-nowrap">' + fmtDateTime(h.ts) + '</td>' +
    '<td>' + escapeHtml(h.operator || 'admin') + '</td>' +
    '<td class="num-cell">' + fmtBytes(h.sizeBytes) + '</td>' +
    '<td class="text-end text-nowrap">' +
      '<button class="btn btn-outline-secondary btn-sm me-1" onclick="restoreConfigVersion(' + h.id + ')" title="载入编辑器（不直接写盘）"><i class="bi bi-clock-history me-1"></i>恢复</button>' +
      '<button class="btn btn-outline-secondary btn-sm" onclick="downloadConfigVersion(' + h.id + ')" title="下载该备份"><i class="bi bi-download"></i></button>' +
    '</td></tr>'
  ).join(''));
}

/* ── 编辑 ── */
function onCfInput() {
  if (!cfState.loaded) return;
  const dirty = el('cf-editor').value !== cfState.baseline;
  el('cf-dirty').classList.toggle('d-none', !dirty);
  if (dirty) setText('cf-status', '有未保存更改');
}
function onCfKeydown(ev) {
  if (ev.key === 'Tab') {
    ev.preventDefault();
    const t = ev.target;
    const start = t.selectionStart, end = t.selectionEnd;
    t.setRangeText('  ', start, end, 'end');
    onCfInput();
    return false;
  }
  return true;
}
function cfDirty() { return cfState.loaded && el('cf-editor').value !== cfState.baseline; }

/* ── 校验（dry-run，不落盘） ── */
async function cfValidate() {
  const btn = el('cf-validate');
  btnBusy(btn, '校验中…');
  setText('cf-status', '校验中…');
  try {
    const r = await apiPost('/services/' + cfState.serviceId + '/configfile/validate', { content: el('cf-editor').value });
    const out = el('cf-out');
    out.classList.remove('d-none');
    const ok = r.exitCode === 0;
    out.innerHTML =
      '<span style="color:var(--' + (ok ? 'success' : 'danger') + ');font-weight:600">' + (ok ? '校验通过' : '校验失败') + '</span>' +
      ' <span style="color:var(--code-dim)">exit ' + r.exitCode + '</span>\n' +
      (r.stdout ? '# stdout\n' + escapeHtml(r.stdout) + '\n' : '') +
      (r.stderr ? '# stderr\n' + escapeHtml(r.stderr) : '');
    setText('cf-status', ok ? '校验通过' : '校验失败');
    showToast(ok ? 'success' : 'danger', ok ? '配置校验通过' : '配置校验失败：' + (r.stderr || '退出码 ' + r.exitCode));
  } catch (e) {
    setText('cf-status', '校验出错');
    showToast('danger', '校验失败：' + e.message);
  }
  btnIdle(btn);
}

/* ── 保存（写入 + 自动备份）/ 保存并生效 ── */
async function cfSave(apply) {
  if (!cfState.loaded) return;
  const content = el('cf-editor').value;
  if (apply && cfDirty()) {
    const saved = await doCfSave(content);
    if (!saved) return;
  }
  if (!apply) {
    if (!content.trim()) {
      const ok = await confirmDialog({ title: '保存空配置文件？', html: '当前内容为空（或全空白），确认继续写入？', level: 'L1', confirmText: '仍然保存' });
      if (!ok) return;
    }
    const saved = await doCfSave(content);
    return;
  }
  /* 生效确认（L1，文案按 applyMode 区分） */
  const isCmd = cfState.applyMode === 'command';
  const ok = await confirmDialog({
    title: isCmd ? '执行热加载' : '重启服务生效',
    html: isCmd
      ? '将执行热加载命令：<code>' + escapeHtml(cfRow() && cfRow().configFile.applyCmd || '') + '</code>（服务不重启）'
      : '将重启 <b>' + escapeHtml(cfState.name) + '</b> 使配置生效，服务将短暂不可用。',
    level: 'L1', confirmText: isCmd ? '执行热加载' : '确认重启生效',
  });
  if (!ok) return;
  const btn = el('cf-apply');
  btnBusy(btn, '生效中…');
  try {
    const r = await apiPost('/services/' + cfState.serviceId + '/configfile/apply', {});
    if (r && r.restarted) {
      showToast('info', '服务重启中，配置随重启生效');
      state.optimistic[cfState.serviceId] = 'starting';
      renderServiceCards();
      setTimeout(() => { delete state.optimistic[cfState.serviceId]; loadServices(true).then(renderServiceCards); }, 3000);
    } else {
      showToast('success', '热加载完成');
      const out = el('cf-out');
      out.classList.remove('d-none');
      out.innerHTML += '\n<span style="color:var(--success);font-weight:600">热加载输出</span>\n' + escapeHtml(r.output || '(无输出)');
    }
  } catch (e) {
    showToast('danger', '生效失败：' + e.message + '（文件内容未自动回滚，可从历史恢复）');
  }
  btnIdle(btn);
}
async function doCfSave(content) {
  const btn = el('cf-save');
  btnBusy(btn, '保存中…');
  try {
    await apiPut('/services/' + cfState.serviceId + '/configfile', { content });
    cfState.baseline = content;
    el('cf-dirty').classList.add('d-none');
    setText('cf-status', '已保存 ' + new Date().toLocaleTimeString());
    showToast('success', '已保存并创建备份');
    await loadConfigHistory();
    btnIdle(btn);
    return true;
  } catch (e) {
    showToast('danger', '保存失败：' + e.message);
    btnIdle(btn);
    return false;
  }
}

/* ── 历史恢复：仅载入编辑器 + 置 dirty，必须再走校验/保存 ── */
async function restoreConfigVersion(bid) {
  const h = cfState.history.find(x => x.id === bid);
  if (!h) return;
  const ok = await confirmDialog({
    title: '载入历史版本',
    html: '将 <b>' + fmtDateTime(h.ts) + '</b> 的备份内容载入编辑器（不会直接写盘）。确认后请再走「校验 → 保存」流程。',
    level: 'L1', confirmText: '载入编辑器',
  });
  if (!ok) return;
  el('cf-editor').value = h.content;
  onCfInput();
  showToast('info', '已载入 ' + fmtDateTime(h.ts) + ' 的版本，请校验后保存');
}
function downloadConfigVersion(bid) {
  const h = cfState.history.find(x => x.id === bid);
  if (!h) return;
  const row = cfRow();
  const ext = (cfState.path.split('.').pop() || 'conf').replace(/[^a-z0-9]/gi, '');
  downloadText(h.content, (row ? row.name : 'service') + '-config-' + new Date(h.ts).toISOString().replace(/[:T]/g, '-').slice(0, 17) + '.' + ext);
}

function closeConfigfile() {
  if (!cfDirty()) { cfConfirmedClose = true; cfModal.hide(); return; }
  confirmDialog({ title: '放弃未保存的更改？', html: '配置编辑器存在未保存更改，关闭后这些修改将被丢弃（已保存的历史备份不受影响）。', level: 'L1', confirmText: '放弃修改' })
    .then(ok => { if (ok) { cfConfirmedClose = true; cfModal.hide(); } });
}
function onConfigfileModalHideGuard(e) {
  if (cfConfirmedClose || !cfDirty()) { cfConfirmedClose = false; return; }
  e.preventDefault();
  confirmDialog({ title: '放弃未保存的更改？', html: '配置编辑器存在未保存更改，关闭后这些修改将被丢弃。', level: 'L1', confirmText: '放弃修改' })
    .then(ok => { if (ok) { cfConfirmedClose = true; cfModal.hide(); } });
}