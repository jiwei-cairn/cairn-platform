/* ══════════════════════════════════════════════
   main：Modal 装配 + init（永远最后加载）
   init：loadVersion → settings 后台预取 → loadServices
        → 恢复未完成升级任务 → switchSection('dash')
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

  /* 配置文件 Modal：static（有未保存更改时由 JS 守卫）；关闭后返回服务 Modal */
  el('configfile-modal').addEventListener('hide.bs.modal', onConfigfileModalHideGuard);
  el('configfile-modal').addEventListener('hidden.bs.modal', () => {
    if (currentSection === 'service' && state.editingServiceId !== null) svcModal.show();
  });
}

function renderVersionBadge() {
  const v = state.version || {};
  setText('sidebar-version', '版本 ' + (v.version || '—') + (v.mock ? ' · 演示模式' : ''));
}

/* 恢复未完成的升级任务：依赖后端任务查询（/upgrade/tasks 属 M2）。
   此处仅在升级任务接口可用后接线，当前不做任何动作。 */
function restoreUpgradeTask() {
  /* 占位：M2 升级管线落地后由 taskSnapshot 恢复。 */
}

function onVisibilityChange() {
  if (document.hidden) {
    dashboardStopPolling();
    updateStopPolling();
    sparkStop();
  } else {
    /* 恢复可见立即刷一次再续轮询 */
    if (['dash', 'service'].includes(currentSection)) dashboardStartPolling();
    if (currentSection === 'service') updateStartPolling();
    if (currentSection === 'dash') sparkStart();
  }
}

async function init() {
  initModals();
  document.addEventListener('visibilitychange', onVisibilityChange);
  window.addEventListener('beforeunload', () => logCenterLeave());

  await loadVersion();
  renderVersionBadge();
  loadSettingsQuiet(); /* 后台预取（大盘轮询周期等） */

  state.servicesLoading = true;
  renderServiceCards();
  renderDashLoading();
  await loadServices();
  restoreUpgradeTask();
  switchSection('dash');

  
}

document.addEventListener('DOMContentLoaded', init);