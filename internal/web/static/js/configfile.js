/* ══════════════════════════════════════════════
   配置编辑器（configfile-modal）
   读 / 改 / 保存 / 校验 / 保存并重启
   数据：GET/PUT/POST /api/services/{id}/configfile*
   交互约定：仅用显式「保存」「保存并生效」按钮落盘，无快捷键。
   ══════════════════════════════════════════════ */

let cfCtx = null; /* { svcId, path, modifiedAt, applyMode, validateCmd, original } */

/* ── 打开弹窗 ── */
async function openConfigfile(svcId) {
  const row = svcById(svcId);
  if (!row) return;
  cfCtx = { svcId, path: '', modifiedAt: 0, applyMode: 'restart', validateCmd: '', original: '' };
  setText('cf-svc-name', svcTitle(row));
  setText('cf-path', (row.configFile && row.configFile.path) || '—');
  setText('cf-status', '加载中…');
  setHtml('cf-state', loadingState('配置文件读取中…'));
  el('cf-editor').value = '';
  el('cf-editor').disabled = true;
  el('cf-dirty').classList.add('d-none');
  el('cf-out').classList.add('d-none');
  setHtml('cf-history-tbody', '');
  btnIdle(el('cf-save'));
  btnIdle(el('cf-apply'));
  cfModal.show();

  try {
    const d = await apiGet('/services/' + svcId + '/configfile');
    cfCtx.path = d.path;
    cfCtx.modifiedAt = d.modifiedAt;
    cfCtx.applyMode = d.applyMode || 'restart';
    cfCtx.validateCmd = d.validateCmd || '';
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
  if (cfIsDirty()) {
    confirmDialog({
      title: '放弃未保存的更改？',
      html: '配置内容已修改但未保存，关闭后这些更改将丢失。',
      level: 'L2', confirmText: '放弃更改',
    }).then(ok => { if (ok) cfModal.hide(); });
    return;
  }
  cfModal.hide();
}

/* 关闭前守卫（点右上角 ×、按 Esc、点遮罩时触发） */
function onConfigfileModalHideGuard(ev) {
  if (cfIsDirty() && !ev.target.dataset.confirmed) {
    ev.preventDefault();
    closeConfigfile(); /* 走确认流程；确认后 hide() 会带标志 */
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
  el('cf-apply').disabled = !dirty;
}
function onCfInput() {
  updateCfDirty();
  setText('cf-status', '已修改 · 未保存');
}

/* ── 校验（不落盘） ── */
async function cfValidate() {
  if (!cfCtx) return;
  const btn = el('cf-validate');
  btnBusy(btn, '校验中…');
  try {
    const d = await apiPut('/services/' + cfCtx.svcId + '/configfile', {
      content: el('cf-editor').value, dryRun: true,
    });
    const out = (d && d.output) || '';
    el('cf-out').classList.remove('d-none');
    el('cf-out').textContent = out || '(无输出)';
    if (d && d.ok) {
      setText('cf-status', '校验通过');
      showToast('success', '校验通过');
    } else {
      setText('cf-status', '校验未通过');
      showToast('warning', '校验未通过，见下方输出');
    }
  } catch (e) {
    setText('cf-status', '校验失败');
    showToast('danger', '校验失败：' + e.message);
  } finally { btnIdle(btn); }
}

/* ── 保存（apply=true 时同时生效） ── */
async function cfSave(apply) {
  if (!cfCtx) return;
  const btn = apply ? el('cf-apply') : el('cf-save');
  const content = el('cf-editor').value;
  if (content === cfCtx.original) { showToast('info', '内容未修改'); return; }
  btnBusy(btn, apply ? '保存中…' : '保存中…');
  try {
    const d = await apiPut('/services/' + cfCtx.svcId + '/configfile', {
      content, modifiedAt: cfCtx.modifiedAt,
    });
    cfCtx.original = content;
    cfCtx.modifiedAt = d.modifiedAt;
    updateCfDirty();
    setText('cf-status', '已保存 · 修改于 ' + fmtDateTime(d.modifiedAt));
    showToast('success', '已保存' + (d.backup ? '（原文件已备份）' : ''));
    loadConfigfileHistory(cfCtx.svcId);

    if (!apply) return;
    /* 生效：按 applyMode 走重启或命令 */
    try {
      const a = await apiPost('/services/' + cfCtx.svcId + '/configfile/apply', {});
      if (a && a.mode === 'restart') {
        showToast('info', '已发起重启，配置将在服务启动时生效');
        await loadServices(true);
        renderServiceCards();
      } else {
        showToast('success', '配置已生效');
      }
    } catch (e2) {
      showToast('warning', '保存成功，但生效失败：' + e2.message);
    }
  } catch (e) {
    showToast('danger', '保存失败：' + e.message);
    if ((e.status === 409) && /外部修改/.test(e.message)) {
      setText('cf-status', '保存冲突：请重新打开');
    }
  } finally { btnIdle(btn); }
}

/* ── 版本历史（备份列表） ── */
async function loadConfigfileHistory(svcId) {
  const tb = el('cf-history-tbody');
  if (!tb) return;
  try {
    const rows = await apiGet('/services/' + svcId + '/configfile/history') || [];
    if (!rows.length) {
      tb.innerHTML = '<tr><td colspan="4" class="text-center text-muted py-3">暂无备份（每次保存自动生成一份）</td></tr>';
      return;
    }
    tb.innerHTML = rows.map(r => (
      '<tr>' +
        '<td class="text-nowrap">' + fmtDateTime(r.at) + '</td>' +
        '<td class="text-muted">保存前快照</td>' +
        '<td class="num-cell">' + fmtBytes(r.bytes) + '</td>' +
        '<td class="text-end"><button class="btn btn-outline-secondary btn-sm" disabled title="备份内容查看随 M3 历史功能提供">查看</button></td>' +
      '</tr>'
    )).join('');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="4" class="text-center text-muted py-3">历史加载失败：' + escapeHtml(e.message) + '</td></tr>';
  }
}
