/* API 客户端：全部 fetch 真实后端。 */
const API = '/api';

class ApiError extends Error {
  constructor(message, status, payload) {
    super(message);
    this.name = 'ApiError';
    this.status = status || 0;
    this.issues = (payload && payload.issues) || [];
  }
  get hasIssues() { return this.issues.length > 0; }
}

async function apiReq(path, opts) {
  opts = opts || {};

  const r = await fetch(API + path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok || j.code) throw new ApiError(j.msg || ('HTTP ' + r.status), r.status, j);
  return j.data;
}
function apiGet(p) { return apiReq(p); }
function apiPost(p, body) { return apiReq(p, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }); }
function apiPut(p, body) { return apiReq(p, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }); }
function apiDelete(p) { return apiReq(p, { method: 'DELETE' }); }
async function apiUpload(path, formData) {

  const r = await fetch(API + path, { method: 'POST', body: formData });
  const j = await r.json().catch(() => ({}));
  if (!r.ok || j.code) throw new ApiError(j.msg || ('HTTP ' + r.status), r.status, j);
  return j.data;
}

/* ══════════════ 服务定义：扁平表单 ↔ 嵌套 payload（对齐架构 §2.2） ══════════════ */
function serviceFromRow(r) {
  const rp = r.restartPolicy || {}, hc = r.healthCheck || {}, cf = r.configFile || {}, lp = r.logPolicy || {};
  return {
    id: r.id || 0, name: r.name || '', summary: r.summary || '', serviceType: r.serviceType || 'native',
    webUrl: r.webUrl || '', exePath: r.exePath || '', workDir: r.workDir || '',
    enabled: r.enabled !== false, autoStart: !!r.autoStart, priority: toNum(r.priority, 100),
    argsText: (r.args || []).join('\n'),
    envText: Object.entries(r.env || {}).map(([k, v]) => k + '=' + v).join('\n'),
    restartMode: (rp.mode === 'periodic') ? 'periodic' : 'backoff', periodicSec: Math.max(3, toNum(rp.periodicSec, 3)), maxRetries: toNum(rp.maxRetries, 0), windowSec: toNum(rp.windowSec, 0),
    backoffBase: toNum(rp.backoffBase, 2), backoffMax: toNum(rp.backoffMax, 300),
    healthType: hc.type || 'none', healthTarget: hc.target || '',
    intervalSec: toNum(hc.intervalSec, 10), timeoutSec: toNum(hc.timeoutSec, 3), readyHoldSec: toNum(hc.readyHoldSec, 5),
    failuresThreshold: toNum(hc.failuresThreshold, 3), degradedRestartSec: toNum(hc.degradedRestartSec, 30),
    dependsOn: (r.dependsOn || []).map(String),
    cfgPath: cf.path || '', validateCmd: cf.validateCmd || '', applyMode: cf.applyMode || 'restart',
    applyCmd: cf.applyCmd || '', cfgMaxHistory: toNum(cf.maxHistory, 10),
    logMode: lp.mode || 'stdout', logPath: lp.path || '',
    logMaxSizeMB: toNum(lp.maxSizeMB, 10), logMaxBackups: toNum(lp.maxBackups, 3),
  };
}
function serviceToPayload(f) {
  /* 编辑提交：不可编辑字段（exePath/workDir/依赖/健康检查/配置文件）从现有定义透传。 */
  const row = svcById(f.id) || {};
  const env = {};
  String(f.envText || '').split('\n').map(s => s.trim()).filter(Boolean).forEach(line => {
    const i = line.indexOf('=');
    if (i > 0) env[line.slice(0, i)] = line.slice(i + 1);
  });
  const restartPolicy = f.restartMode === 'periodic'
    ? { mode: 'periodic', periodicSec: Math.max(3, toNum(f.periodicSec, 3)), maxRetries: toNum(f.maxRetries, 0), windowSec: toNum(f.windowSec, 0) }
    : { mode: 'backoff', backoffBase: toNum(f.backoffBase, 2), backoffMax: toNum(f.backoffMax, 300), maxRetries: toNum(f.maxRetries, 0), windowSec: toNum(f.windowSec, 0) };
  return {
    id: toNum(f.id, 0), name: row.name || (f.name || '').trim(), serviceType: row.serviceType || 'native',
    summary: (f.summary || '').trim(),
    webUrl: (f.webUrl || '').trim(),
    exePath: row.exePath || '', workDir: row.workDir || '',
    enabled: !!f.enabled, autoStart: !!f.autoStart, priority: toNum(f.priority, 100),
    args: String(f.argsText || '').split('\n').map(s => s.trim()).filter(Boolean),
    env,
    restartPolicy,
    healthCheck: row.healthCheck || { type: 'none' },
    configFile: row.configFile || {},
    logPolicy: { mode: f.logMode, path: (f.logPath || '').trim(), maxSizeMB: toNum(f.logMaxSizeMB, 10), maxBackups: toNum(f.logMaxBackups, 3) },
    dependsOn: (row.dependsOn || []).map(Number),
  };
}

/* ══════════════ 各区块数据入口 ══════════════ */
async function loadVersion() {
  try { state.version = await apiGet('/version'); } catch (e) { state.version = null; }
}
async function loadServices(silent) {
  if (!state.services.length && !silent) state.servicesLoading = true;
  try {
    const rows = await apiGet('/services');
    state.services = rows || [];
    state.servicesError = null;
  } catch (e) { state.servicesError = e.message; }
  state.servicesLoading = false;
  if (currentSection === 'service') renderServiceCards();
}
async function loadSettingsQuiet() {
  try { state.settings = await apiGet('/settings'); state.settingsError = null; }
  catch (e) { state.settingsError = e.message; }
}
function svcById(id) { return state.services.find(s => Number(s.id) === Number(id)); }
async function loadMarket(silent) {
  if (!silent) state.market.loading = true;
  try {
    const [catalog, products] = await Promise.all([apiGet('/market/catalog'), apiGet('/market/products')]);
    state.market.catalog = catalog || [];
    state.market.products = products || [];
    state.market.catalogError = null;
  } catch (e) { state.market.catalogError = e.message; }
  state.market.loading = false;
}
function rtState(row) {
  if (!row) return 'unknown';
  if (!row.enabled) return 'disabled';
  if (state.optimistic && state.optimistic[row.id]) return state.optimistic[row.id];
  return (row.runtime && row.runtime.state) || 'stopped';
}
function isRunningIsh(row) { return ['running', 'degraded', 'backoff', 'starting', 'waiting', 'upgrading'].includes(rtState(row)); }