/* ══════════════════════════════════════════════
   配置编辑器（configfile-modal）
   读 / 改 / 保存（保存后自动重启服务加载新配置）
   数据：GET/PUT /api/services/{id}/configfile(+apply)
   交互约定：只有「保存」一个动作，无快捷键、无校验按钮。
   业务闭环：保存 → 重启 → 服务起不来则由卡片的运行监控告警，
   用户据退出原因重新编辑保存即可。
   ══════════════════════════════════════════════ */

let cfCtx = null; /* { svcId, path, modifiedAt, original } */
let cfConfirmedClose = false; /* 已确认放弃更改：放行下一次 hide() */

/* ── 打开弹窗 ── */
async function openConfigfile(svcId) {
  const row = svcById(svcId);
  if (!row) return;
  cfCtx = { svcId, path: '', modifiedAt: 0, original: '' };
  cfConfirmedClose = false; /* 每次打开复位：上次「放弃更改」的标志不能带进本次 */
  setText('cf-svc-name', svcTitle(row));
  setText('cf-path', (row.configFile && row.configFile.path) || '—');
  setText('cf-status', '加载中…');
  setHtml('cf-state', loadingState('配置文件读取中…'));
  el('cf-editor').value = '';
  el('cf-editor').disabled = true;
  el('cf-dirty').classList.add('d-none');
  setHtml('cf-history-tbody', '');
  btnIdle(el('cf-save'));
  cfModal.show();

  try {
    const d = await apiGet('/services/' + svcId + '/configfile');
    cfCtx.path = d.path;
    cfCtx.modifiedAt = d.modifiedAt;
    cfCtx.original = d.content || '';
    setText('cf-path', d.path + ' · ' + fmtBytes(d.sizeBytes) +
      (d.location === 'data' ? ' · 数据目录' : ' · 产物目录'));
    setHtml('cf-state', '');

    const ed = el('cf-editor');
    ed.value = d.content || '';
    ed.disabled = !d.editable;
    if (!d.editable) {
      setHtml('cf-state', '<div class="info-banner mb-2"><i class="bi bi-exclamation-triangle me-2" style="color:var(--warning)"></i>该文件不是纯文本，仅可查看不可编辑。</div>');
    }
    setText('cf-status', '已加载 · 修改于 ' + fmtDateTime(d.modifiedAt));
    updateCfDirty();
    loadConfigfileHistory(svcId);
  } catch (e) {
    setHtml('cf-state', errorState('读取失败：' + e.message, 'openConfigfile(' + svcId + ')'));
    setText('cf-status', '读取失败');
  }
}

function closeConfigfile() {
  if (!cfIsDirty()) { cfModal.hide(); return; }
  confirmDialog({
    title: '放弃未保存的更改？',
    html: '配置内容已修改但未保存，关闭后这些更改将丢失。',
    level: 'L2', confirmText: '放弃更改',
  }).then(ok => {
    if (!ok) return;           /* 用户取消：保持弹窗与编辑内容 */
    cfConfirmedClose = true;   /* 放行守卫 */
    cfModal.hide();
  });
}

/* 关闭前守卫（点右上角 ×、按 Esc、点遮罩时触发）。
 *
 * Bootstrap 的 hide.bs.modal 里 ev.target 是模态框元素本身，触发元素在
 * ev.relatedTarget——早先读 ev.target.dataset.confirmed 且从未设置该属性，
 * 导致守卫永远拦截，连确认后也无法关闭。改用模块级标志位。
 */
function onConfigfileModalHideGuard(ev) {
  if (cfIsDirty() && !cfConfirmedClose) {
    ev.preventDefault();
    closeConfigfile(); /* 走确认流程；确认后置位并 hide() */
  }
}

/* ── 编辑态 ── */
function cfIsDirty() {
  if (!cfCtx) return false;
  return el('cf-editor').value !== cfCtx.original;
}
function updateCfDirty() {
  const dirty = cfIsDirty();
  el('cf-dirty').classList.toggle('d-none', !dirty);
  el('cf-save').disabled = !dirty;
}
function onCfInput() {
  updateCfDirty();
  setText('cf-status', '已修改 · 未保存');
}

/* ── 保存：写盘 → 重启服务加载新配置 ──
 *
 * 单一动作（需求定稿）：没有「仅保存」与「保存并生效」之分，也没有校验按钮。
 * 语义：先落盘（含自动备份），再重启服务。服务若因配置错误起不来，
 * 运行监控会把状态推到 failed/backoff 并在卡片上显示告警与退出原因——
 * 用户据此重新编辑保存即完成闭环。
 */
async function cfSave() {
  if (!cfCtx) return;
  const btn = el('cf-save');
  const content = el('cf-editor').value;
  if (content === cfCtx.original) { showToast('info', '内容未修改'); return; }
  btnBusy(btn, '保存中…');
  try {
    const d = await apiPut('/services/' + cfCtx.svcId + '/configfile', {
      content, modifiedAt: cfCtx.modifiedAt,
    });
    cfCtx.original = content;
    cfCtx.modifiedAt = d.modifiedAt;
    updateCfDirty();
    loadConfigfileHistory(cfCtx.svcId);

    /* 重启生效：配置在进程启动时读取 */
    try {
      await apiPost('/services/' + cfCtx.svcId + '/configfile/apply', {});
      showToast('success', '已保存，服务正在重启以加载新配置' +
        (d.backup ? '（原文件已备份）' : ''));
    } catch (e2) {
      showToast('warning', '已保存，但重启失败：' + e2.message + '；请在服务卡片上手动重启');
    }
    /* 关闭弹窗并刷新卡片：让用户立刻看到服务的新状态（含启动失败告警） */
    cfConfirmedClose = true;
    cfModal.hide();
    await loadServices(true);
    renderServiceCards();
    if (currentSection === 'dash') refreshDashboard();
  } catch (e) {
    showToast('danger', '保存失败：' + e.message);
    if ((e.status === 409) && /外部修改/.test(e.message)) {
      setText('cf-status', '保存冲突：请重新打开');
    }
  } finally { btnIdle(btn); }
}

/* ── 最后正常配置快照（P1-2 定案：单份 lastgood + 一键恢复） ──
 * GET history 返回 {exists, at, bytes}；恢复走 POST restore
 *（快照写回 + 重启，与「保存」同语义）。 */
async function loadConfigfileHistory(svcId) {
  const tb = el('cf-history-tbody');
  if (!tb) return;
  try {
    const d = await apiGet('/services/' + svcId + '/configfile/history') || {};
    if (!d.exists) {
      tb.innerHTML = '<tr><td colspan="3" class="text-center text-muted py-3">暂无快照（服务启动验证通过后自动留存）</td></tr>';
      return;
    }
    tb.innerHTML =
      '<tr>' +
        '<td class="text-nowrap">' + fmtDateTime(d.at) + '</td>' +
        '<td class="num-cell">' + fmtBytes(d.bytes) + '</td>' +
        '<td class="text-end"><button class="btn btn-outline-primary btn-sm" onclick="cfRestore()"><i class="bi bi-arrow-counterclockwise me-1"></i>恢复</button></td>' +
      '</tr>';
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="3" class="text-center text-muted py-3">快照信息加载失败：' + escapeHtml(e.message) + '</td></tr>';
  }
}

/* cfRestore 恢复最后正常配置：确认 → 快照写回 → 重启 → 关闭弹窗刷新卡片。
 * 业务闭环的兜底：用户改配置一直改不好时，一键回到上次能跑的版本。 */
async function cfRestore() {
  if (!cfCtx) return;
  const go = await confirmDialog({
    title: '恢复最后正常配置？',
    html: '当前配置文件内容将被<b>最后正常启动时的快照</b>覆盖，随后服务自动重启。' +
      '<div class="mt-1 text-muted">仅建议在修改后服务无法启动、且多次调整无果时使用。</div>',
    level: 'L2', confirmText: '恢复并重启',
  });
  if (!go) return;
  const btn = el('cf-save');
  btnBusy(btn, '恢复中…');
  try {
    await apiPost('/services/' + cfCtx.svcId + '/configfile/restore', {});
    showToast('success', '已恢复最后正常配置，服务正在重启');
    cfConfirmedClose = true;
    cfModal.hide();
    await loadServices(true);
    renderServiceCards();
    if (currentSection === 'dash') refreshDashboard();
  } catch (e) {
    showToast('danger', '恢复失败：' + e.message);
    btnIdle(btn);
  }
}
