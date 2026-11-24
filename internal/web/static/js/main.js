/* ══════════════════════════════════════════════
   main：Modal 装配 + init（永远最后加载）
   init：loadVersion → settings 后台预取 → loadServices → switchSection('dash')
   ══════════════════════════════════════════════ */

let svcModal, cfModal, leModal, umModal, usModal, installModal, confirmModal;

function initModals() {
  svcModal = new bootstrap.Modal(el('service-modal'));
  cfModal = new bootstrap.Modal(el('configfile-modal'));
  leModal = new bootstrap.Modal(el('log-export-modal'));
  umModal = new bootstrap.Modal(el('upgrade-modal'));
  usModal = new bootstrap.Modal(el('upgrade-source-modal'));
  installModal = new bootstrap.Modal(el('install-modal'));
  confirmModal = new bootstrap.Modal(el('confirm-modal'));

  /* 统一确认弹窗：Esc / 遮罩拦截 = 取消（static backdrop + hidePrevented） */
  el('confirm-modal').addEventListener('hidePrevented.bs.modal', () => _confirmDone(false));

  /* 服务 Modal：dirty 时拦截关闭（含遮罩点击），L1 确认后放行 */
  el('service-modal').addEventListener('hide.bs.modal', onServiceModalHideGuard);
  el('service-modal').addEventListener('hidden.bs.modal', () => { smConfirmedClose = false; });

  /* 配置文件 Modal：dirty 时守卫关闭（确认放弃后放行）；关闭后返回服务 Modal */
  el('configfile-modal').addEventListener('hide.bs.modal', onConfigfileModalHideGuard);
  el('configfile-modal').addEventListener('hidden.bs.modal', () => {
    cfConfirmedClose = false; /* 复位：本次关闭已完成，下次打开不继承 */
    if (currentSection === 'service' && state.editingServiceId !== null) svcModal.show();
  });
}

function renderVersionBadge() {
  const v = state.version || {};
  setText('sidebar-version', '版本 ' + (v.version || '—'));
}

function onVisibilityChange() {
  if (document.hidden) {
    dashboardStopPolling();
    updateStopPolling();
  } else {
    /* 恢复可见立即刷一次再续轮询 */
    if (['dash', 'service'].includes(currentSection)) dashboardStartPolling();
    if (currentSection === 'service') updateStartPolling();
  }
}

async function init() {
  initModals();
  initTooltips();
  document.addEventListener('visibilitychange', onVisibilityChange);
  window.addEventListener('beforeunload', () => logCenterLeave());

  await loadVersion();
  renderVersionBadge();
  loadSettingsQuiet(); /* 后台预取（大盘轮询周期等） */

  state.servicesLoading = true;
  renderServiceCards();
  renderDashLoading();
  await loadServices();
  switchSection('dash');

  
}

/* ── 字段说明 tooltip（label 旁的 ? 图标承载） ──
   title 属性的固有限制：聚焦输入框即失效、有悬停延迟、触屏完全不显示、
   与 placeholder/aria-label 互相干扰。改用 Bootstrap tooltip：
   聚焦仍可见、触屏点按可见、样式统一。
   事件委托 + data-bs-toggle 选择器，对动态渲染内容（服务卡片等）同样生效。 */
function initTooltips() {
  if (typeof bootstrap === 'undefined' || !bootstrap.Tooltip) return;
  new bootstrap.Tooltip(document.body, {
    selector: '[data-bs-toggle="tooltip"]',
    trigger: 'hover focus',   /* focus：点进输入框后仍可查看说明 */
    placement: 'top',
    container: 'body',        /* 避免被 modal 的 overflow 裁切 */
    delay: { show: 200, hide: 0 },
  });
}

document.addEventListener('DOMContentLoaded', init);