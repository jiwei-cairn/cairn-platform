/* ══════════════════════════════════════════════
   ② service 微服务管理：model-card 列表 + 新建/编辑 Modal（精简版）+ 启停/重启/卸载
   - 新建 = 统一安装：主程序/依赖文件/初始配置三上传框 → manifest 说明卡 → 安装管线
   - 重启策略 periodic（固定周期，最小 3s）/ backoff（指数 2s→300s）
   - 健康检查内置 L1（fork 进程存活），无需配置
   ══════════════════════════════════════════════ */

let smBaseline = '';
let smConfirmedClose = false;

/* 上传暂存（新建模式）：{main:{name,path}, extras:[{name,path}], config:{name,path}} */
let smUploads = { main: null, extras: [], config: null };

/* ── 表单字段映射（与表单 DOM 一致；精简后无 exePath/workDir/健康检查/依赖/配置文件项） ── */
const SM_TEXT = { name: 'sm-name', summary: 'sm-summary', webUrl: 'sm-webUrl', version: 'sm-version', argsText: 'sm-args', envText: 'sm-env', logPath: 'sm-logPath' };
const SM_SELECT = { serviceType: 'sm-serviceType', restartMode: 'sm-restartMode', logMode: 'sm-logMode' };
const SM_NUM = { priority: 'sm-priority', periodicSec: 'sm-periodicSec', maxRetries: 'sm-maxRetries', windowSec: 'sm-windowSec', backoffBase: 'sm-backoffBase', backoffMax: 'sm-backoffMax', logMaxSizeMB: 'sm-logMaxSizeMB', logMaxBackups: 'sm-logMaxBackups' };

function defaultServiceForm() {
  return {
    id: 0, name: '', summary: '', serviceType: 'native', webUrl: '', version: '1.0.0',
    enabled: true, autoStart: true, priority: 100,
    argsText: '', envText: '',
    restartMode: 'backoff', periodicSec: 3, maxRetries: 0, windowSec: 0, backoffBase: 2, backoffMax: 300,
    logMode: 'stdout', logPath: '', logMaxSizeMB: 10, logMaxBackups: 3,
  };
}
function fillSmForm(f) {
  Object.keys(SM_TEXT).forEach(k => setVal(SM_TEXT[k], f[k] ?? ''));
  Object.keys(SM_SELECT).forEach(k => setVal(SM_SELECT[k], f[k] ?? ''));
  Object.keys(SM_NUM).forEach(k => setVal(SM_NUM[k], f[k] ?? 0));
  setChecked('sm-enabled', f.enabled);
  setChecked('sm-autoStart', f.autoStart);
}
function collectSmForm() {
  const f = { id: state.editingServiceId || 0 };
  Object.keys(SM_TEXT).forEach(k => f[k] = val(SM_TEXT[k]));
  Object.keys(SM_SELECT).forEach(k => f[k] = val(SM_SELECT[k]));
  Object.keys(SM_NUM).forEach(k => f[k] = toNum(val(SM_NUM[k]), 0));
  f.enabled = checked('sm-enabled');
  f.autoStart = checked('sm-autoStart');
  return f;
}
function serializeSmForm() { return JSON.stringify(collectSmForm()); }
function smDirty() { return serializeSmForm() !== smBaseline; }

function clearSmInvalid() {
  document.querySelectorAll('#sm-form .is-invalid').forEach(e => e.classList.remove('is-invalid'));
}
function markSmIssues(issues) {
  issues.forEach(it => {
    const input = el(it.field);
    if (!input) return;
    input.classList.add('is-invalid');
    /* 三级反馈：① invalid-feedback div → ② title 悬停提示 → ③ 直接在下方注入 */
    const wrap = input.closest('.col-md-') || input.parentElement;
    let fb = wrap ? wrap.querySelector('.invalid-feedback') : null;
    if (!fb && input.parentElement) {
      fb = input.parentElement.querySelector('.invalid-feedback');
    }
    if (!fb) {
      /* 无 invalid-feedback 容器时动态创建一个紧贴输入框 */
      fb = document.createElement('div');
      fb.className = 'invalid-feedback';
      fb.style.display = 'block'; /* 强制显示（Bootstrap 需 :invalid 兄弟，这里手动） */
      if (input.parentElement && input.parentElement.classList.contains('input-group')) {
        input.parentElement.insertAdjacentElement('afterend', fb);
      } else {
        input.insertAdjacentElement('afterend', fb);
      }
    }
    if (fb && it.message) {
      fb.textContent = it.message;
      fb.style.display = 'block';
    }
    input.title = it.message; /* 悬停提示兜底 */
  });
  /* 滚动到第一个无效字段并聚焦 */
  const first = document.querySelector('#sm-form .is-invalid');
  if (first) {
    first.scrollIntoView({ behavior: 'smooth', block: 'center' });
    first.focus({ preventScroll: true });
  }
}

/* ── 校验 ── */
function validateSmForm(f) {
  const issues = [];
  if (!f.name) issues.push({ field: 'sm-name', message: '服务名称必填' });
  /* 名称同时是安装目录名：与后端 manifestNameRE 保持一致，避免往返一次才报错 */
  else if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(f.name)) issues.push({ field: 'sm-name', message: '仅允许小写字母/数字/._-，且以字母或数字开头（最长 64）' });
  if ((f.summary || '').length > 40) issues.push({ field: 'sm-summary', message: '摘要最长 40 个字符' });
  if (f.webUrl && !/^https?:\/\/.+/.test(f.webUrl)) issues.push({ field: 'sm-webUrl', message: '需以 http:// 或 https:// 开头' });
  const isNew = !f.id;
  if (isNew) {
    if (!f.version) issues.push({ field: 'sm-version', message: '初始版本必填' });
    if (!smUploads.main) issues.push({ field: 'sm-file-main', message: '必须选择主程序文件' });
  }
  const numRule = (field, label, min, max) => {
    const v = f[field];
    if (v < min || v > max) issues.push({ field: SM_NUM[field], message: label + '需在 ' + min + '–' + max + ' 之间' });
  };
  if (f.restartMode === 'periodic') numRule('periodicSec', '固定周期', 3, 86400);
  numRule('backoffBase', '起始间隔', 1, 3600);
  numRule('backoffMax', '间隔上限', 1, 86400);
  numRule('logMaxSizeMB', '日志单文件上限', 1, 512);
  numRule('logMaxBackups', '日志保留份数', 1, 30);
  return issues;
}

/* ══════════════ 卡片列表 ══════════════ */
function renderServiceCards() {
  const wrap = el('svc-cards');
  const st = el('svc-state');
  if (!wrap) return;
  setText('svc-count', state.services.length);
  if (state.servicesError) {
    wrap.innerHTML = '';
    st.innerHTML = errorState('服务列表加载失败：' + state.servicesError, 'retryLoadServices()');
    return;
  }
  if (state.servicesLoading && !state.services.length) {
    wrap.innerHTML = '';
    st.innerHTML = loadingState('服务列表加载中…');
    return;
  }
  if (!state.services.length) {
    wrap.innerHTML = '';
    st.innerHTML = emptyState('bi-diagram-3', '暂无纳管服务，点击右上角「新建服务」开始配置');
    return;
  }
  st.innerHTML = '';
  wrap.innerHTML = state.services.map(cardHtml).join('');
}

function cardHtml(s) {
  const st = rtState(s);
  const meta = SERVICE_STATE[st] || SERVICE_STATE.unknown;
  const rt = s.runtime || {};
  const disabled = st === 'disabled';
  const busy = ['starting', 'stopping', 'waiting', 'upgrading'].includes(st);
  const runningIsh = ['running', 'degraded', 'backoff'].includes(st);
  const stoppedIsh = ['stopped', 'failed'].includes(st);
  const locked = state.update.taskActive;
  const sub = s.exePath || '—';
  const icon = s.serviceType === 'opensource' ? 'bi-box-seam' : 'bi-app-window';
  /* 卡片标题用摘要（中文展示名）优先，回落服务名；服务名随后以次要样式补示，
     保证「摘要」与「唯一标识」都可见（外部 Web 链接仅在摘要上做穿越）。 */
  const title = svcTitle(s);
  const nameTag = s.summary && s.summary !== s.name
    ? '<span class="mc-tag mc-tag-name" title="服务名称（安装目录名）">' + escapeHtml(s.name) + '</span>'
    : '';
  const stats = disabled || ['stopped', 'failed'].includes(st)
    ? { up: '—', starts: rt.restartTotal ?? '—', cpu: '—', mem: '—' }
    : { up: fmtUptime(rt.uptimeSec), starts: rt.restartTotal ?? '—', cpu: fmtPct(rt.cpuPct), mem: fmtMB(rt.memMB) };
  const btn = (id, iconCls, title, fn, enabled, extraCls) =>
    '<button class="btn ' + (extraCls || 'btn-outline-secondary') + ' btn-sm" onclick="' + fn + '" ' + (enabled ? '' : 'disabled ') +
    'title="' + title + '" aria-label="' + title + '"><i class="bi ' + iconCls + '"></i></button>';
  const titleHtml = s.webUrl
    ? '<a class="svc-name-link" href="' + escapeHtml(s.webUrl) + '" target="_blank" rel="noopener" title="穿越到 ' + escapeHtml(s.webUrl) + '"><span class="led ' + meta.led + '"></span>' + escapeHtml(title) + '<i class="bi bi-box-arrow-up-right ms-1"></i></a>'
    : '<span class="led ' + meta.led + '"></span>' + escapeHtml(title);
  const avail = state.update.available[s.id];
  const hasWeb = !!s.webUrl;
  const hasCfg = !!(s.configFile && s.configFile.path);
  let actions = btn(0, 'bi-cloud-arrow-up' + (avail ? '-fill' : ''), avail ? '升级（可用 ' + avail.version + '）' : '升级（OTA / OTG / LOCAL）', 'openUpgradeSource(' + s.id + ')', !locked, 'btn-outline-primary');
  actions += btn(0, 'bi-trash', '卸载（停止并移除纳管）', 'svcUninstall(' + s.id + ', this)', true, 'btn-outline-danger');
  actions += btn(0, hasWeb ? 'bi-window' : 'bi-file-earmark-code', hasWeb ? '配置（穿越服务 Web）' : hasCfg ? '配置（编辑配置文件）' : '配置（未配置）', 'svcConfig(' + s.id + ')', hasWeb || hasCfg);
  actions += btn(0, 'bi-sliders', '详情', 'openServiceModal(' + s.id + ')', true, 'btn-outline-primary');
  if (!disabled) {
    actions += btn(0, 'bi-play-fill', '启动', 'svcStart(' + s.id + ')', !busy && stoppedIsh && !locked, 'btn-outline-success');
    actions += btn(0, 'bi-stop-fill', '停止', 'svcStopFlow(' + s.id + ')', !busy && runningIsh && !locked);
    actions += btn(0, 'bi-arrow-repeat', '重启', 'svcRestartFlow(' + s.id + ')', !busy && !locked, 'btn-outline-primary');
  }
  return '<div class="col-md-6 col-lg-4">' +
    '<div class="model-card' + (disabled ? ' svc-disabled' : '') + '" id="svc-card-' + s.id + '">' +
    '<div class="model-card-top">' +
      '<div class="model-card-icon"><i class="bi ' + icon + '"></i></div>' +
      '<div class="min-w-0 flex-grow-1">' +
        '<div class="model-card-title">' + titleHtml + '</div>' +
        '<div class="model-card-sub" title="' + escapeHtml(sub) + '">' + escapeHtml(sub) + '</div>' +
      '</div>' +
    '</div>' +
    '<div class="model-card-tags">' +
      nameTag +
      '<span class="mc-tag iface">' + (RESTART_MODE_LABEL[(s.restartPolicy || {}).mode] || '退避重启') + '</span>' +
      '<span class="mc-tag">' + (s.version ? escapeHtml(s.version) : '未安装') + '</span>' +
      (avail ? '<span class="mc-tag mc-tag-up" title="OTA 可用版本">可升级 ' + escapeHtml(avail.version) + '</span>' : '') +
      (s.webUrl ? '<span class="mc-tag mc-tag-web" title="' + escapeHtml(s.webUrl) + '"><i class="bi bi-window me-1"></i>独立 Web</span>' : '') +
      '<span class="mc-tag">' + (SERVICE_TYPE_LABEL[s.serviceType] || '自研') + '</span>' +
    '</div>' +
    '<div class="model-card-stats">' +
      '<div class="mc-stat"><div class="num">' + stats.up + '</div><div class="lbl">运行时长</div></div>' +
      '<div class="mc-stat"><div class="num">' + stats.starts + '</div><div class="lbl">启动次数</div></div>' +
      '<div class="mc-stat"><div class="num">' + stats.cpu + '</div><div class="lbl">CPU</div></div>' +
      '<div class="mc-stat"><div class="num">' + stats.mem + '</div><div class="lbl">内存</div></div>' +
    '</div>' +
    '<div class="model-card-actions">' + actions + '</div>' +
    '</div></div>';
}
function retryLoadServices() { loadServices().then(renderServiceCards); }
function dependentsOf(id) {
  return state.services.filter(s => (s.dependsOn || []).map(Number).includes(Number(id)) && isRunningIsh(s));
}

/* ══════════════ Modal：打开 / 填充 / 保存 ══════════════ */
function openServiceModal(id) {
  state.editingServiceId = id || 0;
  const row = id ? svcById(id) : null;
  const f = row ? serviceFromRow(row) : defaultServiceForm();
  fillSmForm(f);
  const isNew = !row;
  /* 安装文件区 / 版本：仅新建；名称：编辑时锁定（目录名/说明卡主键）。 */
  el('sm-files-section').classList.toggle('d-none', !isNew);
  el('sm-version-wrap').classList.toggle('d-none', !isNew);
  el('sm-name').disabled = !isNew;
  smUploads = { main: null, extras: [], config: null };
  /* 重置服务名自动填充状态（新建态：允许后续上传自动填充） */
  smNameTouched = false;
  smLastAutofill = '';
  /* 摘要自动填充状态同源重置（与名称一致的语义） */
  smSummaryTouched = false;
  smLastSummaryAutofill = '';
  resetSmFileInputs();
  /* 主程序选择器按运行平台过滤（Windows 只收 .exe）。 */
  const mainInput = el('sm-file-main');
  if (isWindowsPlatform()) mainInput.accept = '.exe,application/octet-stream';
  else mainInput.accept = '';
  onSmRestartModeChange(); onSmLogModeChange();
  setText('sm-title', row ? svcDisplayName(row) : '新建服务');
  const badge = el('sm-state-badge');
  if (row) {
    const meta = SERVICE_STATE[rtState(row)] || SERVICE_STATE.unknown;
    badge.className = 'badge ' + meta.badge;
    badge.textContent = meta.label;
    badge.classList.remove('d-none');
  } else badge.classList.add('d-none');
  const statsWrap = el('sm-stats');
  if (row) { statsWrap.classList.remove('d-none'); loadSvcStats(row.id); }
  else statsWrap.classList.add('d-none');
  const cfBtn = el('sm-open-cf');
  cfBtn.disabled = !row || !(row.configFile && row.configFile.path);
  cfBtn.title = cfBtn.disabled ? '编辑已有服务且已配置 configFile.path 后可用' : '打开配置文件编辑器';
  const expBtn = el('sm-export');
  expBtn.disabled = !row;
  clearSmInvalid();
  smBaseline = serializeSmForm();
  smConfirmedClose = false;
  svcModal.show();
}

/* ── 服务名自动派生（上传主程序后自动填充） ──
   语义：服务名 = 英文标识（唯一、作安装目录名），通常就是可执行程序名。
   产出与后端 manifestNameRE（^[a-z0-9][a-z0-9._-]{0,63}$）严格对齐，
   避免用户填完再被后端驳回。 */
const SVC_PLATFORM_SUFFIXES = ['linux', 'windows', 'darwin', 'amd64', 'arm64', 'arm', '386'];

function deriveServiceName(fileName) {
  let s = String(fileName || '').trim();
  if (!s) return '';
  // 去掉 Windows/POSIX 路径段（后端已用 filepath.Base，这里防御浏览器差异）
  s = s.split(/[\\/]/).pop();
  // 去扩展名：可执行/安装包扩展名，以及 .tar.gz 这类多段扩展名
  s = s.replace(/\.(tar\.)?(gz|xz|bz2|zip|pkg|run|bin|exe|msi)$/i, '');
  s = s.toLowerCase();
  // 非法字符 → 连字符，再收敛
  s = s.replace(/[^a-z0-9._-]+/g, '-').replace(/-{2,}/g, '-').replace(/^[-._]+|[-._]+$/g, '');
  // 剥离平台/架构后缀（如 energy-gateway-linux-amd64 → energy-gateway）
  const parts = s.split('-');
  while (parts.length > 1 && SVC_PLATFORM_SUFFIXES.includes(parts[parts.length - 1])) parts.pop();
  const stripped = parts.join('-');
  if (stripped) s = stripped;
  // 必须以字母或数字开头（后端规则）
  s = s.replace(/^[^a-z0-9]+/, '');
  return s.slice(0, 64);
}

/* 服务名是否被用户手工改过（改过就不再自动覆盖）；smLastAutofill 记录自动填充值。 */
let smNameTouched = false;
let smLastAutofill = '';

/* markSmNameTouched 表单名称框 oninput：标记用户已手工编辑。 */
function markSmNameTouched() { smNameTouched = true; }

/* autofillServiceName 上传主程序后自动填充服务名。
   仅在以下情形写入，绝不覆盖用户已输入的内容：
     - 服务名当前为空
     - 服务名仍是上一次自动填充的结果（用户未改）
   填充后清掉该字段的校验红框并刷新表单基线，避免误判为"有未保存更改"。 */
function autofillServiceName(fileName) {
  if (state.editingServiceId) return; // 编辑态：名称锁定，不动
  const derived = deriveServiceName(fileName);
  if (!derived) return;
  const input = el('sm-name');
  if (!input) return;
  if (smNameTouched && input.value.trim() !== smLastAutofill) return; // 用户改过且非自动值
  input.value = derived;
  smLastAutofill = derived;
  input.classList.remove('is-invalid');
  smBaseline = serializeSmForm(); // 自动填充不算"未保存更改"
}

/* ── 摘要自动派生（上传主程序后自动填充） ──
   与名称自动填充同源同语义：摘要 = 卡片展示名（可中文），来自说明卡
   displayName，缺省回落 description。平台表单点击主程序时还没有
   manifest.json（manifest 由 buildInstallRequest 反向拼出），因此此处
   仅能以主程序文件名为数据源；market.js 走真实 manifest。 */
function deriveSummaryFromFile(fileName) {
  const raw = String(fileName || '').trim().split(/[\\/]/).pop() || '';
  if (!raw) return '';
  /* 去掉可执行/安装包扩展名（.tar.gz 等多段扩展名一并处理）；
     截断到 40，与 #sm-summary 的 maxlength 及 validateSmForm 的长度校验一致 */
  return raw.replace(/\.(tar\.)?(gz|xz|bz2|zip|pkg|run|bin|exe|msi)$/i, '').trim().slice(0, 40);
}

/* 摘要是否被用户手工改过（改过就不再自动覆盖）；smLastSummaryAutofill 记录自动填充值。 */
let smSummaryTouched = false;
let smLastSummaryAutofill = '';

/* markSmSummaryTouched 表单摘要框 oninput：标记用户已手工编辑。 */
function markSmSummaryTouched() { smSummaryTouched = true; }

/* autofillServiceSummary 上传主程序后自动填充摘要（展示名）。
   写入条件与 autofillServiceName 完全一致，绝不覆盖用户已输入的内容：
     - 摘要当前为空
     - 摘要仍是上一次自动填充的结果（用户未改）
   填充后清掉该字段的校验红框并刷新表单基线，避免误判为"有未保存更改"。 */
function autofillServiceSummary(fileName) {
  if (state.editingServiceId) return; // 编辑态：不动已有摘要
  const derived = deriveSummaryFromFile(fileName);
  if (!derived) return;
  const input = el('sm-summary');
  if (!input) return;
  if (smSummaryTouched && input.value.trim() !== smLastSummaryAutofill) return; // 用户改过且非自动值
  input.value = derived;
  smLastSummaryAutofill = derived;
  input.classList.remove('is-invalid');
  smBaseline = serializeSmForm(); // 自动填充不算"未保存更改"
}

function resetSmFileInputs() {
  ['sm-file-main', 'sm-file-extras', 'sm-file-config'].forEach(id2 => { const i = el(id2); if (i) i.value = ''; });
  setText('sm-file-main-name', '可执行文件（Windows 运行选 .exe；Linux 选本机架构）');
  setText('sm-file-extras-name', 'dll / so / 静态库等 → dependencies/ 目录');
  setText('sm-file-config-name', '可选 → config/ 目录，参数用 -c config/文件名 引用');
}

/* ── 文件上传交互：选择即暂存（POST /services/upload）── */
async function onSmFilesPick(kind, input) {
  const files = Array.from(input.files || []);
  if (!files.length) return;
  clearSmInvalid();
  const fd = new FormData();
  files.forEach(f => fd.append('files', f));
  try {
    const r = await apiUpload('/services/upload', fd);
    if (kind === 'main') {
      smUploads.main = r.files[0];
      setText('sm-file-main-name', '✓ ' + r.files[0].name + '（' + fmtBytes(r.files[0].size) + '）');
      autofillServiceName(r.files[0].name);
      autofillServiceSummary(r.files[0].name);
    } else if (kind === 'extras') {
      smUploads.extras = r.files;
      setText('sm-file-extras-name', '✓ ' + r.files.map(f => f.name).join('、'));
    } else {
      smUploads.config = r.files[0];
      setText('sm-file-config-name', '✓ ' + r.files[0].name + ' → config/' + r.files[0].name + '（参数用 -c config/' + r.files[0].name + '）');
    }
  } catch (e) {
    showToast('danger', '上传失败：' + e.message);
  }
}

/* ── 重启模式联动 ── */
function onSmRestartModeChange() {
  const periodic = val('sm-restartMode') === 'periodic';
  el('sm-periodic-wrap').classList.toggle('d-none', !periodic);
  el('sm-backoffBase-wrap').classList.toggle('d-none', periodic);
  el('sm-backoffMax-wrap').classList.toggle('d-none', periodic);
}

function onSmLogModeChange() {
  const file = val('sm-logMode') === 'file';
  const input = el('sm-logPath');
  input.disabled = !file;
  const example = isWindowsPlatform() ? 'C:\\\\cairn\\\\logs\\xxx.log' : '/var/log/cairn/xxx.log';
  input.placeholder = file ? example : '标准输出由平台统一收集';
}

async function loadSvcStats(id) {
  setHtml('sm-stats-body', ['连续运行时长', '累计启动', 'CPU 时间', '网络累计流量'].map(l =>
    '<div class="col-6 col-md-3"><div class="summary-card mini"><div class="val">…</div><div class="lbl">' + l + '</div></div></div>').join(''));
  try {
    const st = await apiGet('/services/' + id + '/stats');
    const cells = [fmtUptime(st.uptimeSec), String(st.restartTotal ?? '—'), fmtUptime(st.cpuTimeSec), '↓' + fmtBytes((st.netRxKB || 0) * 1024) + ' ↑' + fmtBytes((st.netTxKB || 0) * 1024)];
    const labels = ['连续运行时长', '累计启动', 'CPU 时间', '网络累计流量'];
    let html = labels.map((l, i) => '<div class="col-6 col-md-3"><div class="summary-card mini"><div class="val">' + cells[i] + '</div><div class="lbl">' + l + '</div></div></div>').join('');
    if (st.cpu && st.cpu.length > 2) html += sparklineHtml(st.cpu);
    setHtml('sm-stats-body', html);
  } catch (e) {
    setHtml('sm-stats-body', '<div class="text-muted small">运行统计暂不可用：' + e.message + '</div>');
  }
}
function sparklineHtml(series) {
  const w = 240, h = 40, max = Math.max(...series, 1);
  const pts = series.map((v, i) => [(i / (series.length - 1)) * w, h - 3 - (v / max) * (h - 8)]);
  const line = pts.map(p => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' ');
  return '<div class="col-12"><svg class="sparkline" viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" role="img" aria-label="CPU 最近 30 个采样点">' +
    '<polygon class="spark-fill" points="0,' + h + ' ' + line + ' ' + w + ',' + h + '"></polygon>' +
    '<polyline points="' + line + '"></polyline></svg>' +
    '<div class="text-muted" style="font-size:11px;margin-top:2px;">CPU% · 最近 30 个采样点</div></div>';
}

async function saveService() {
  clearSmInvalid();
  const f = collectSmForm();
  const issues = validateSmForm(f);
  if (issues.length) {
    markSmIssues(issues);
    /* toast 逐条列出无效字段名与原因，不再只报数量 */
    const detail = issues.map(it => {
      const input = el(it.field);
      const label = input ? (input.closest('.col-md-')?.querySelector('.form-label')?.textContent?.replace('*','').trim() || it.field) : it.field;
      return label + '：' + it.message;
    }).join('；');
    showToast('danger', detail, { duration: 6000 });
    return;
  }
  const btn = el('sm-save');
  btnBusy(btn, '保存中…');
  try {
    const isNew = !f.id;
    let res;
    if (isNew) {
      res = await apiPost('/services/install', buildInstallRequest(f));
      showToast('success', '服务已创建（说明卡与版本目录已生成）');
    } else {
      res = await apiPost('/services', serviceToPayload(f));
      showToast('success', '服务已保存');
    }
    const wasNew = isNew;
    smConfirmedClose = true;
    svcModal.hide();
    await loadServices(true);
    renderServiceCards();
    if (wasNew && res && res.id) {
      const card = el('svc-card-' + res.id);
      if (card) card.classList.add('svc-card-fade-in');
    }
    if (!wasNew && res && res.needRestart) {
      const rid = res.id || f.id;
      showToast('warning', '配置已保存，重启服务后生效', { action: { label: '立即重启', fn: () => svcRestartFlow(rid) } });
    }
  } catch (e) {
    /* 安装预检 409 时展示后端返回的具体问题清单 */
    if (e.issues && e.issues.length) {
      const detail = e.issues.map(it => (it.msg || it.message || '')).filter(Boolean).join('；');
      showToast('danger', (f.id ? '保存失败：' : '创建失败：') + detail, { duration: 8000 });
    } else {
      showToast('danger', (f.id ? '保存失败：' : '创建失败：') + e.message);
    }
  }
  btnIdle(btn);
}

/* buildInstallRequest 表单 → 安装请求体（manifest + overrides，统一安装管线）。 */
function buildInstallRequest(f) {
  const env = {};
  String(f.envText || '').split('\n').map(s => s.trim()).filter(Boolean).forEach(line => {
    const i = line.indexOf('=');
    if (i > 0) env[line.slice(0, i)] = line.slice(i + 1);
  });
  const args = String(f.argsText || '').split('\n').map(s => s.trim()).filter(Boolean);
  const restartPolicy = f.restartMode === 'periodic'
    ? { mode: 'periodic', periodicSec: Math.max(3, toNum(f.periodicSec, 3)), maxRetries: toNum(f.maxRetries, 0), windowSec: toNum(f.windowSec, 0) }
    : { mode: 'backoff', backoffBase: toNum(f.backoffBase, 2), backoffMax: toNum(f.backoffMax, 300), maxRetries: toNum(f.maxRetries, 0), windowSec: toNum(f.windowSec, 0) };
  const manifest = {
    name: f.name.trim(), displayName: (f.summary || '').trim() || f.name.trim(),
    version: (f.version || '1.0.0').trim(),
    serviceType: f.serviceType,
    args, env,
    webUrl: (f.webUrl || '').trim(),
    restartPolicy,
    logPolicy: { mode: f.logMode, path: (f.logPath || '').trim(), maxSizeMB: toNum(f.logMaxSizeMB, 10), maxBackups: toNum(f.logMaxBackups, 3) },
    extraFiles: smUploads.extras.map(x => x.name),
  };
  const overrides = {
    enabled: f.enabled, autoStart: f.autoStart, priority: toNum(f.priority, 100),
    localBinary: smUploads.main ? smUploads.main.path : '',
    extraBinaries: smUploads.extras.map(x => x.path),
    initialConfig: smUploads.config ? smUploads.config.path : '',
  };
  return { manifest, overrides, source: 'manual' };
}

/* ── 导出说明卡（manifest.json）── */
async function exportSvcManifest(id) {
  if (!id) return;
  try {
    const r = await apiGet('/services/' + id + '/manifest');
    const blob = new Blob([JSON.stringify(r.manifest, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    const row = svcById(id);
    a.download = (row ? row.name : 'service') + '-manifest.json';
    a.click();
    URL.revokeObjectURL(a.href);
    showToast('success', '说明卡已导出（' + (r.origin === 'installed' ? '安装时点留存' : '当前态合成') + ' · v' + r.version + '）');
  } catch (e) {
    showToast('danger', '导出失败：' + e.message);
  }
}

function closeServiceModal() { svcModal.hide(); } /* dirty 守卫见 main.js hide 监听 */
function onServiceModalHideGuard(e) {
  if (smConfirmedClose || !smDirty()) return;
  e.preventDefault();
  confirmDialog({ title: '放弃未保存的修改？', html: '服务表单存在未保存更改，关闭后这些修改将被丢弃。', level: 'L1', confirmText: '放弃修改' })
    .then(ok => { if (ok) { smConfirmedClose = true; svcModal.hide(); } });
}

/* ══════════════ 启停 / 重启 / 卸载 ══════════════ */
const SVC_ACT_LABEL = { start: '启动', stop: '停止', restart: '重启' };

async function svcAction(id, act) {
  const row = svcById(id);
  if (!row) return;
  state.optimistic[id] = act === 'stop' ? 'stopping' : 'starting';
  renderServiceCards();
  try {
    await apiPost('/services/' + id + '/' + act);
    watchSvcTransition(id, act);
  } catch (e) {
    showToast('danger', SVC_ACT_LABEL[act] + '失败：' + e.message);
    delete state.optimistic[id];
    renderServiceCards();
    return;
  }
  await loadServices(true);
  renderServiceCards();
}
function svcStart(id) { svcAction(id, 'start'); }

function svcStopFlow(id) {
  const row = svcById(id);
  if (!row) return;
  const deps = dependentsOf(id);
  let html = '确认停止 <b>' + escapeHtml(svcDisplayName(row)) + '</b>？停止后该服务不再自动拉起，直到手动启动或重启平台。';
  if (deps.length) html += '<div class="mt-2" style="color:var(--warning)"><i class="bi bi-exclamation-triangle-fill me-1"></i>以下运行中服务依赖它，可能受影响：' + deps.map(d => '<b>' + escapeHtml(d.name) + '</b>').join('、') + '</div>';
  confirmDialog({ title: '停止服务', html, level: 'L1', confirmText: '确认停止' }).then(ok => { if (ok) svcAction(id, 'stop'); });
}
function svcRestartFlow(id) {
  const row = svcById(id);
  if (!row) return;
  confirmDialog({
    title: '重启服务',
    html: '确认重启 <b>' + escapeHtml(svcDisplayName(row)) + '</b>？重启期间该服务短暂不可用。',
    level: 'L1', confirmText: '确认重启',
  }).then(ok => { if (ok) svcAction(id, 'restart'); });
}

function watchSvcTransition(id, act) {
  const target = act === 'stop' ? 'stopped' : 'running';
  let tries = 0;
  const timer = setInterval(async () => {
    tries++;
    await loadServices(true);
    const row = svcById(id);
    const st = row ? rtState(row) : null;
    if (st === target) {
      clearInterval(timer);
      delete state.optimistic[id];
      showToast('success', '服务已' + (act === 'stop' ? '停止' : act === 'restart' ? '重启' : '启动'));
    } else if (st === 'failed' || st === 'backoff') {
      clearInterval(timer);
      delete state.optimistic[id];
      const meta = SERVICE_STATE[st];
      showToast('warning', (row ? row.name : '服务') + '进入「' + meta.label + '」，请在大盘查看异常原因');
    } else if (tries > 8) {
      clearInterval(timer);
      delete state.optimistic[id];
    }
    renderServiceCards();
  }, 1300);
}

/* ── 卸载（L2 强确认） ── */
async function svcUninstall(id, btn) {
  const row = svcById(id);
  if (!row) return;
  const running = isRunningIsh(row);
  const deps = dependentsOf(id);
  let html = '';
  if (running) html += '<div style="color:var(--danger)"><i class="bi bi-exclamation-triangle-fill me-1"></i>该服务正在运行，卸载将先停止并移除监管</div>';
  html += '<div class="mt-2">确认卸载 <b>' + escapeHtml(svcDisplayName(row)) + '</b>？以下影响不可逆：</div><ul class="mt-1 mb-0 ps-3" style="font-size:13px;">';
  html += '<li class="mt-1"><i class="bi bi-exclamation-triangle-fill me-1" style="color:var(--danger)"></i>服务将停止，纳管定义、监管策略、运行统计与本地保留版本将被移除</li>';
  if (deps.length) html += '<li class="mt-1"><i class="bi bi-exclamation-triangle-fill me-1" style="color:var(--danger)"></i>' + deps.length + ' 个服务的依赖引用将被清除（' + deps.map(d => '<b>' + escapeHtml(d.name) + '</b>').join('、') + '）</li>';
  html += '</ul>';
  const ok = await confirmDialog({ title: '卸载微服务', html, level: 'L2', confirmText: '确认卸载', confirmIcon: 'bi-trash' });
  if (!ok) return;
  if (btn) btnBusy(btn, '卸载中…');
  try {
    await apiDelete('/services/' + id);
    showToast('success', '服务已卸载：' + row.name);
    const card = el('svc-card-' + id);
    if (card) {
      card.classList.add('svc-card-fade-out');
      setTimeout(() => { loadServices(true).then(renderServiceCards); }, 200);
    } else { await loadServices(true); renderServiceCards(); }
  } catch (e) {
    showToast('danger', '卸载失败：' + e.message);
    if (btn) btnIdle(btn);
  }
}

/* ── 配置穿越：独立 Web 优先，否则配置文件编辑器 ── */
function svcConfig(id) {
  const row = svcById(id);
  if (!row) return;
  if (row.webUrl) {
    window.open(row.webUrl, '_blank', 'noopener');
    showToast('info', '正在打开 ' + row.name + ' 的独立 Web 页面');
    return;
  }
  if (row.configFile && row.configFile.path) {
    openServiceModal(id);
    setTimeout(() => openConfigfile(), 350);
    return;
  }
  showToast('warning', '该服务未配置独立 Web 或配置文件路径');
}