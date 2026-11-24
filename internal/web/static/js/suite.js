/* ══════════════════════════════════════════════
   产品套件发布（P1-3：设置页卡片）
   勾选已装服务 + 套件元信息 → POST /api/publish/suite
   → 合成 .cairnproduct 上传 hub → 市场可见
   ══════════════════════════════════════════════ */

/* ── 进入设置页：加载服务勾选列表 ── */
async function loadSuiteServices() {
  const box = el('sp-services');
  if (!box) return;
  try {
    if (!state.services.length) {
      await loadServices(true);
    }
    const rows = state.services.filter(s => s.version && s.version !== 'current');
    if (!rows.length) {
      box.innerHTML = '<div class="text-muted small">没有可引用的已装服务（需包安装、有版本记录）</div>';
      return;
    }
    box.innerHTML = rows.map(s =>
      '<div class="form-check">' +
        '<input class="form-check-input sp-svc-check" type="checkbox" value="' + escapeHtml(s.name) + '" id="sp-svc-' + s.id + '">' +
        '<label class="form-check-label small" for="sp-svc-' + s.id + '">' +
          escapeHtml(svcTitle(s)) + ' <code>' + escapeHtml(s.version || '') + '</code>' +
        '</label>' +
      '</div>'
    ).join('');
  } catch (e) {
    box.innerHTML = '<div class="text-muted small">服务列表加载失败：' + escapeHtml(e.message) + '</div>';
  }
}

/* ── 发布 ── */
async function suitePublish() {
  const name = trimSpace(val('sp-name'));
  const version = trimSpace(val('sp-version'));
  const checked = Array.from(document.querySelectorAll('.sp-svc-check:checked')).map(c => c.value);

  // 前端校验
  if (!name || !version) { showToast('warning', '套件名与版本为必填'); return; }
  if (checked.length < 2) { showToast('warning', '至少勾选 2 个服务'); return; }

  const btn = el('sp-publish');
  btnBusy(btn, '发布中…');
  setText('sp-status', '正在合成并上传…');
  setHtml('sp-result', '');
  try {
    const r = await apiPost('/publish/suite', {
      name, version,
      displayName: trimSpace(val('sp-display')) || name,
      description: trimSpace(val('sp-desc')),
      services: checked,
    });
    showToast('success', '套件已发布：' + name + ' ' + version + '（引用 ' + checked.length + ' 个服务）');
    setText('sp-status', '已发布');
    setHtml('sp-result', '<div class="um-result-banner ok mt-2"><i class="bi bi-check-circle-fill me-2"></i>' +
      '套件 <b>' + escapeHtml(name) + ' ' + escapeHtml(version) + '</b> 已上传至服务端；市场页「产品」tab 可见。</div>');
    // 刷新市场产品列表（下次打开市场时生效）
    state.market.products = null;
  } catch (e) {
    setText('sp-status', '发布失败');
    showToast('danger', '套件发布失败：' + e.message);
    setHtml('sp-result', '<div class="um-result-banner err mt-2"><i class="bi bi-x-octagon-fill me-2"></i>' + escapeHtml(e.message) + '</div>');
  } finally {
    btnIdle(btn);
  }
}
