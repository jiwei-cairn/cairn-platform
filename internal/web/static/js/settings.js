/* ══════════════════════════════════════════════
   ⑥ settings 系统设置：表单回填、逐字段校验、
   保存（热应用 / 需重启提示）
   ══════════════════════════════════════════════ */

let settingsClockTimer = null;

/* 字段映射（路径 ↔ DOM ID，camelCase 全链路一致） */
const SET_NUM = [
  ['ota.timeoutSec', 'set-ota-timeout'],
  ['web.port', 'set-web-port'], ['web.sessionTimeoutMin', 'set-web-session'],
  ['logQuota.platformMaxMB', 'set-log-maxmb'], ['logQuota.platformMaxBackups', 'set-log-backups'], ['logQuota.retentionDays', 'set-log-days'], ['logQuota.bufferLines', 'set-log-buffer'],
  ['collect.intervalSec', 'set-collect-interval'], ['collect.keepVersions', 'set-collect-keepversions'], ['collect.dbQuotaMB', 'set-collect-dbquota'],
];
const SET_TEXT = [['ota.serverUrl', 'set-ota-server']];
const SET_SELECT = [['logQuota.level', 'set-log-level'], ['collect.downsample', 'set-collect-downsample']];
const SET_BOOL = [];

function getPath(obj, path) { return path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function setPath(obj, path, v) {
  const keys = path.split('.');
  let o = obj;
  for (let i = 0; i < keys.length - 1; i++) { if (!o[keys[i]]) o[keys[i]] = {}; o = o[keys[i]]; }
  o[keys[keys.length - 1]] = v;
}
const SET_FIELD_LABEL = {
  'ota.serverUrl': '远程升级服务器', 'ota.timeoutSec': 'OTA 超时', 'web.port': 'Web 监听端口',
  'logQuota.platformMaxMB': '日志单文件上限', 'collect.intervalSec': '指标采集周期',
};

function settingsEnter() {
  startSettingsClock();
  loadSettingsPage();
}
function settingsLeave() {
  if (settingsClockTimer) { clearInterval(settingsClockTimer); settingsClockTimer = null; }
}
function startSettingsClock() {
  if (settingsClockTimer) return;
  const tick = () => setText('set-about-time', fmtDateTime(Date.now()));
  tick();
  settingsClockTimer = setInterval(tick, 1000);
}

async function loadSettingsPage() {
  setHtml('set-state', loadingState('加载设置中…'));
  el('set-cards').classList.add('d-none');
  el('set-save').disabled = true;
  state.settingsLoading = true;
  try {
    state.settings = await apiGet('/settings');
    state.settingsError = null;
    setHtml('set-state', '');
    el('set-cards').classList.remove('d-none');
    el('set-save').disabled = false;
    fillSettingsForm(state.settings);
    fillAboutCard();
    dashboardRestart(); /* 采集周期可能变化，重建大盘轮询 */
  } catch (e) {
    state.settingsError = e.message;
    setHtml('set-state', errorState('无法读取平台设置：' + e.message, 'retryLoadSettings()'));
    el('set-save').disabled = true;
  }
  state.settingsLoading = false;
}
function retryLoadSettings() { loadSettingsPage(); }

function fillSettingsForm(st) {
  SET_NUM.forEach(([path, id]) => setVal(id, getPath(st, path) ?? ''));
  SET_TEXT.forEach(([path, id]) => setVal(id, getPath(st, path) ?? ''));
  SET_SELECT.forEach(([path, id]) => setVal(id, getPath(st, path) ?? ''));
  SET_BOOL.forEach(([path, id]) => setChecked(id, !!getPath(st, path)));
  clearSetInvalid();
}
function fillAboutCard() {
  const v = state.version || {};
  setText('set-about-version', v.version || '—');
  setText('set-about-build', v.buildTime || '—');
  setText('set-about-os', v.os || '—');
  if (v.mock) setText('set-about-version', (v.version || '—') + '（演示数据）');
}
function collectSettings() {
  const out = {};
  SET_NUM.forEach(([path, id]) => setPath(out, path, toNum(val(id), 0)));
  SET_TEXT.forEach(([path, id]) => setPath(out, path, val(id)));
  SET_SELECT.forEach(([path, id]) => setPath(out, path, val(id)));
  SET_BOOL.forEach(([path, id]) => setPath(out, path, checked(id)));
  return out;
}
const IPV4_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
function validateSettings(s) {
  const issues = [];
  const add = (field, message) => issues.push({ field, message });
  const idOf = path => {
    const all = SET_NUM.concat(SET_TEXT, SET_SELECT, SET_BOOL);
    const hit = all.find(([p]) => p === path);
    return hit ? hit[1] : null;
  };
  // OTA 服务器：空 = 未启用远程升级（出厂默认）；非空必须是 http(s):// 基址。
  const otaUrl = String(s.ota.serverUrl || '').trim();
  if (otaUrl !== '' && !/^https?:\/\/[A-Za-z0-9.:-]+(:[0-9]{1,5})?([\/A-Za-z0-9._\/-]*)?$/.test(otaUrl)) {
    add(idOf('ota.serverUrl'), '必须是 http:// 或 https:// 开头的基址（不含 /cairn 路径与尾斜杠）');
  }
  if (!(s.ota.timeoutSec >= 5 && s.ota.timeoutSec <= 120)) add(idOf('ota.timeoutSec'), 'OTA 超时需在 5–120 秒');
  const port = (p, label) => { if (!(s[p.split('.')[0]] && s[p.split('.')[0]][p.split('.')[1]] >= 1 && s[p.split('.')[0]][p.split('.')[1]] <= 65535)) add(idOf(p), label + '需在 1–65535 之间'); };
  port('web.port', 'Web 监听端口');
  if (!(s.web.sessionTimeoutMin >= 5 && s.web.sessionTimeoutMin <= 1440)) add(idOf('web.sessionTimeoutMin'), '会话超时需在 5–1440 分钟');
  if (!(s.logQuota.platformMaxMB >= 1 && s.logQuota.platformMaxMB <= 512)) add(idOf('logQuota.platformMaxMB'), '单文件上限需在 1–512 MB');
  if (!(s.logQuota.platformMaxBackups >= 1 && s.logQuota.platformMaxBackups <= 20)) add(idOf('logQuota.platformMaxBackups'), '保留份数需在 1–20');
  if (!(s.logQuota.retentionDays >= 1 && s.logQuota.retentionDays <= 365)) add(idOf('logQuota.retentionDays'), '保留天数需在 1–365 天');
  if (!(s.logQuota.bufferLines >= 500 && s.logQuota.bufferLines <= 5000)) add(idOf('logQuota.bufferLines'), '前端缓冲需在 500–5000 行');
  if (!(s.collect.intervalSec >= 2 && s.collect.intervalSec <= 3600)) add(idOf('collect.intervalSec'), '采集周期需在 2–3600 秒');
  if (!(s.collect.keepVersions >= 1 && s.collect.keepVersions <= 5)) add(idOf('collect.keepVersions'), '版本保留数需在 1–5');
  if (!(s.collect.dbQuotaMB >= 64 && s.collect.dbQuotaMB <= 16384)) add(idOf('collect.dbQuotaMB'), '数据库配额需在 64–16384 MB');
  return issues;
}
function clearSetInvalid() {
  document.querySelectorAll('#set-cards .is-invalid').forEach(e => e.classList.remove('is-invalid'));
}
function markSetIssues(issues) {
  issues.forEach(it => {
    if (!it.field) return;
    const input = el(it.field);
    if (!input) return;
    input.classList.add('is-invalid');
    const fb = input.parentElement.querySelector('.invalid-feedback');
    if (fb && it.message) fb.textContent = it.message;
  });
}

async function saveSettings() {
  clearSetInvalid();
  const s = collectSettings();
  const issues = validateSettings(s);
  if (issues.length) {
    markSetIssues(issues);
    showToast('danger', issues.length + ' 处字段无效');
    return;
  }
  const btn = el('set-save');
  btnBusy(btn, '保存中…');
  try {
    const res = await apiPut('/settings', s);
    state.settings = deepCopy(state.settings) || {};
    Object.keys(s).forEach(k => { state.settings[k] = Object.assign(state.settings[k] || {}, s[k]); });
    showSetRestartBanner(res || {});
    dashboardRestart();
  } catch (e) {
    if (e.issues && e.issues.length) {
      const mapped = e.issues.map(it => ({
        field: (SET_NUM.concat(SET_TEXT, SET_SELECT, SET_BOOL).find(([p]) => p === it.field) || [null, null])[1],
        message: it.message,
      })).filter(it => it.field);
      markSetIssues(mapped);
      showToast('danger', '保存失败：' + mapped.length + ' 处字段无效');
    } else showToast('danger', '保存失败：' + e.message);
  }
  btnIdle(btn);
}
function showSetRestartBanner(res) {
  const banner = el('set-restart-banner');
  const need = res.needRestart || [];
  el('set-web-restart-badge').classList.toggle('d-none', !need.some(n => n.field === 'web.port'));
  if (!need.length) {
    banner.classList.add('d-none');
    showToast('success', '设置已保存并生效');
    return;
  }
  const labels = need.map(n => '<b>' + escapeHtml(n.label || SET_FIELD_LABEL[n.field] || n.field) + '</b>').join('、');
  banner.innerHTML = '<i class="bi bi-info-circle-fill me-2" style="color:var(--primary)"></i>以下字段已保存，重启平台后生效：' + labels;
  banner.classList.remove('d-none');
  showToast('warning', '设置已保存：其余字段已热应用，重启平台后生效：' + need.map(n => n.label || n.field).join('、'));
}