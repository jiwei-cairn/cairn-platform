/* ══════════════════════════════════════════════
   ① dash 总览：统计摘要卡（可点击，联动下方状态表筛选）/
      系统性能指标 / 平台只读条 / 异常微服务表
   （原「运行」页全服务状态列表已合并回本页，见 renderDashServices）
   ══════════════════════════════════════════════ */
let dashTimer = null;
let dashFilter = 'all'; /* all | ok | warn | err —— 与四张统计卡一一对应，见 DASH_FILTERS */

const DASH_FILTERS = [
  { key: 'all',  label: '全部' },
  { key: 'ok',   label: '正常' },
  { key: 'warn', label: '告警' },
  { key: 'err',  label: '异常' },
];

function dashIntervalSec() {
  return (state.settings && state.settings.collect && toNum(state.settings.collect.intervalSec, 10)) || 10;
}
function dashboardStartPolling() {
  if (dashTimer) return;
  refreshDashboard();
  dashTimer = startPolling(refreshDashboard, dashIntervalSec() * 1000);
}
function dashboardStopPolling() { dashTimer = stopPolling(dashTimer); }
function dashboardRestart() { dashboardStopPolling(); if (['dash', 'service'].includes(currentSection)) dashboardStartPolling(); }

async function refreshDashboard() {
  try {
    state.dashboard = await apiGet('/dashboard');
    state.dashboardError = false;
    state.dashboardAt = Date.now();
    /* /api/services 列表响应为「定义+运行态」聚合视图 —— 与大盘同源合并 */
    if (state.dashboard.services) { state.services = state.dashboard.services; state.servicesError = null; }
    renderDashAll();
  } catch (e) {
    state.dashboardError = true;
    if (!state.dashboard) renderDashError(e.message);
    else showDashStale();
    if (currentSection === 'service' && !state.services.length) { state.servicesError = e.message; renderServiceCards(); }
  }
}

function renderDashAll() {
  const d = state.dashboard;
  if (!d) return;
  const stale = el('dash-stale');
  if (stale && !state.dashboardError) stale.classList.add('d-none');
  setText('dash-refresh-info', '上次刷新 ' + fmtClock(state.dashboardAt) + ' · 自动 ' + dashIntervalSec() + 's');
  renderDashPlatform();
  renderDashOverview();
  renderDashServices(); // 服务运行状态表（原运行页功能，与大盘同源合并渲染；
  // 原异常置顶表已并入：点「异常」卡或筛选即可得到原置顶表的行与告警原因）
  renderHostSparks(); // 重绘 sparkline（当前值着色/窗口右移），不额外发请求
  if (currentSection === 'service') renderServiceCards();
}
/* ══════════════ 主机指标 sparkline（近 1 小时） ══════════════

数据源 GET /api/dashboard/host-metrics?range=1h（后端按窗口桶聚合到 ≤120 点）。
绘制用内联 SVG，遵循「不引图表库」的既定约定。

刷新节奏与大盘解耦：大盘 10s 轮询，但 sparkline 每分钟才新增约 6 个采样点，
故每 60s 拉取一次即可，避免每 10s 重复拉取时序 JSON。 */

const SPARK_RANGE = '1h';
const SPARK_REFRESH_MS = 60000; // 数据拉取节流（后端采样默认 10s，曲线按分钟变化）
let sparkTimer = null;
let sparkFetchedAt = 0;
let sparkCache = { cpu: [], mem: [], disk: [], rx: [], tx: [] }; // metric -> number[]

/* 触发一次 sparkline 数据拉取（带节流：SPARK_REFRESH_MS 内不重复请求）。 */
function refreshHostSparks(force) {
  if (currentSection !== 'dash') return;
  const now = Date.now();
  if (!force && sparkFetchedAt && now - sparkFetchedAt < SPARK_REFRESH_MS) {
    renderHostSparks(); // 已有数据：仅重绘（窗口右移 + 当前值着色）
    return;
  }
  sparkFetchedAt = now;
  apiGet('/dashboard/host-metrics?range=' + SPARK_RANGE).then(v => {
    const pts = (v && v.points) || [];
    ['cpu', 'mem', 'disk'].forEach(m => {
      sparkCache[m] = pts.map(p => Number(p[m]) || 0);
    });
    /* 网络速率与使用率共用同一份时序响应，无需第二个请求 */
    sparkCache.rx = pts.map(p => Number(p.rxKBps) || 0);
    sparkCache.tx = pts.map(p => Number(p.txKBps) || 0);
    renderHostSparks();
  }).catch(() => {
    /* 静默：sparkline 是增强信息，失败不打扰用户（大盘自身会呈现错误态） */
    renderHostSparks();
  });
}
function sparkStart() {
  refreshHostSparks(true);
  sparkTimer = startPolling(refreshHostSparks, SPARK_REFRESH_MS);
}
function sparkStop() { sparkTimer = stopPolling(sparkTimer); }

/* renderHostSparks 重绘三行趋势小图（每行一张）。 */
function renderHostSparks() {
  ['cpu', 'mem', 'disk'].forEach(metric => {
    const host = el('dash-spark-' + metric);
    if (!host) return;
    const pts = sparkCache[metric] || [];
    /* 行级状态类：CPU/内存按阈值着色（>60% 琥珀 / >80% 红，与左侧进度条同源——
       颜色类来自 sparkLevelClass，是否着色由 metricNeutral 决定，两侧判断始终一致）；
       磁盘恒为中性色（is-neutral），不参与阈值着色。 */
    const row = host.closest('.host-metric');
    if (row) {
      row.classList.remove('is-warn', 'is-err', 'is-neutral');
      if (metricNeutral(metric)) row.classList.add('is-neutral');
      else {
        const lvl = sparkLevelClass(currentMetricPct(metric));
        if (lvl) row.classList.add(lvl);
      }
    }
    host.innerHTML = pts.length
      ? sparkSvg(pts)
      : '<div class="spark-empty">暂无趋势数据</div>';
  });
  renderNetSpark();
}

/* renderNetSpark 重绘「网络流量」卡的双线小图（下行=主色 / 上行=琥珀）。
   与三行使用率小图共用 sparkCache，故不额外发请求。 */
function renderNetSpark() {
  const host = el('dash-spark-net');
  if (!host) return;
  const rx = sparkCache.rx || [], tx = sparkCache.tx || [];
  host.innerHTML = rx.length
    ? sparkSvgDual(rx, tx)
    : '<div class="spark-empty">暂无趋势数据</div>';
}

/* currentMetricPct 当前实时值（用于着色与曲线末尾标注）。 */
function currentMetricPct(metric) {
  const o = (state.dashboard && state.dashboard.overview) || {};
  if (metric === 'cpu') return o.hostCpuPct;
  if (metric === 'mem') return o.hostMemPct;
  return o.hostDiskPct;
}

/* metricNeutral 该指标是否不参与阈值着色。
   磁盘使用率在嵌入式设备上长期偏高，染红只会变成噪声，故取中性色；
   setMetricValue（进度条）与 renderHostSparks（趋势图）共用此判定，避免两侧不一致。 */
function metricNeutral(metric) { return metric === 'disk'; }

/* sparkLevelClass 告警分级：>80% 红 / >60% 琥珀。
   仅用于非中性指标（见 metricNeutral）；与左侧进度条使用同一阈值，两侧颜色始终一致。 */
function sparkLevelClass(pct) {
  if (pct === null || pct === undefined || isNaN(pct)) return '';
  if (pct > 80) return 'is-err';
  if (pct > 60) return 'is-warn';
  return '';
}

/* sparkSvg 把数值序列画成折线 + 浅填充。
   坐标系用固定 viewBox（100×40），实际显示尺寸交给 CSS 拉伸到列宽——
   这样小图宽度自适应而无需在 JS 里测量布局（避免 resize 重算）。
   Y 轴固定 0–100（使用率语义），使三列曲线可直接横向比较；
   仅当数据全在 0–5 的极低区间时才放大到 0–10，避免一条贴底的直线。 */
const SPARK_VB_W = 100;
const SPARK_VB_H = 40;
function sparkSvg(pts) {
  const n = pts.length;
  const maxRaw = Math.max.apply(null, pts);
  const top = maxRaw <= 5 ? 10 : 100;
  const x = i => (n === 1 ? SPARK_VB_W / 2 : (i / (n - 1)) * (SPARK_VB_W - 2) + 1);
  const y = v => SPARK_VB_H - 1 - (clamp(Number(v) || 0, 0, top) / top) * (SPARK_VB_H - 2);
  let line = '', area = '';
  for (let i = 0; i < n; i++) {
    const px = x(i).toFixed(2), py = y(pts[i]).toFixed(2);
    line += (i ? 'L' : 'M') + px + ' ' + py;
    area += (i ? 'L' : 'M') + px + ' ' + py;
  }
  area += 'L' + x(n - 1).toFixed(2) + ' ' + SPARK_VB_H + 'L' + x(0).toFixed(2) + ' ' + SPARK_VB_H + 'Z';
  return '<svg class="spark-svg" viewBox="0 0 ' + SPARK_VB_W + ' ' + SPARK_VB_H +
    '" preserveAspectRatio="none" role="img" aria-label="近 1 小时使用率趋势">' +
    '<path class="spark-area" d="' + area + '"></path>' +
    '<path class="spark-line" d="' + line + '"></path></svg>';
}

/* sparkSvgDual 把两条序列画进同一张图（网络下行/上行）。
   与 sparkSvg 的关键差别在 Y 轴标度：速率没有 100% 这样的天然上限，
   用固定量程只会画成贴底直线，故以两条序列的公共最大值定标——
   共用同一量程才能横向比较上下行（各自定标会让两条线都顶格，看着一样高）。
   上限留 10% 余量并取整到易读刻度，避免峰值恰好贴顶。
   量程为 0（无流量）时退回 1，防止除零。 */
function sparkSvgDual(a, b) {
  const n = Math.max(a.length, b.length);
  if (!n) return '';
  const peak = Math.max(Math.max.apply(null, a.concat([0])), Math.max.apply(null, b.concat([0])));
  const top = peak > 0 ? niceCeil(peak * 1.1) : 1;
  const x = i => (n === 1 ? SPARK_VB_W / 2 : (i / (n - 1)) * (SPARK_VB_W - 2) + 1);
  const y = v => SPARK_VB_H - 1 - (clamp(Number(v) || 0, 0, top) / top) * (SPARK_VB_H - 2);
  const path = pts => {
    let d = '';
    for (let i = 0; i < pts.length; i++) d += (i ? 'L' : 'M') + x(i).toFixed(2) + ' ' + y(pts[i]).toFixed(2);
    return d;
  };
  /* 只给下行线加浅填充：两条填充会互相遮挡，反而看不清趋势 */
  const areaRx = path(a) + 'L' + x(a.length - 1).toFixed(2) + ' ' + SPARK_VB_H + 'L' + x(0).toFixed(2) + ' ' + SPARK_VB_H + 'Z';
  const label = '近 1 小时网络速率，峰值 ' + fmtKBps(peak);
  return '<svg class="spark-svg" viewBox="0 0 ' + SPARK_VB_W + ' ' + SPARK_VB_H +
    '" preserveAspectRatio="none" role="img" aria-label="' + label + '">' +
    '<path class="spark-area-rx" d="' + areaRx + '"></path>' +
    '<path class="spark-line-rx" d="' + path(a) + '"></path>' +
    '<path class="spark-line-tx" d="' + path(b) + '"></path></svg>';
}

/* niceCeil 把量程上限抬到易读刻度（1/2/5 × 10^n），使纵轴刻度规整。 */
function niceCeil(v) {
  if (!(v > 0)) return 1;
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  const norm = v / mag;
  const step = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 5 ? 5 : 10;
  return step * mag;
}

/* ── 平台自身只读条（Q3：展示但不参与监管，无任何操作按钮） ── */
function renderDashPlatform() {
  const pf = state.dashboard.platform || {};
  setText('dash-platform-version', pf.version || '—');
  setText('dash-platform-uptime', fmtUptime(pf.uptimeSec));
  /* 守护方式按平台由后端给出（Linux=systemd / Windows=SCM），避免前端写死 */
  setText('dash-platform-daemon', pf.daemon || '系统服务');
}

/* ── 统计摘要卡 ×4 + 系统性能指标 ── */
function renderDashOverview() {
  const o = state.dashboard.overview || {};
  const total = o.total ?? (state.services ? state.services.length : 0);
  setText('dash-total-count', total);
  setText('dash-ok-count', o.ok ?? 0);
  setText('dash-warn-count', o.warn ?? 0);
  setText('dash-err-count', o.err ?? 0);
  el('dash-warn-count').style.color = o.warn > 0 ? 'var(--warning)' : '';
  const errCard = el('dash-card-err');
  if (errCard) errCard.classList.toggle('is-err', (o.err ?? 0) > 0);
  renderDashFilterChips(); // 筛选 chips 与卡片选中态随计数刷新
  /* 主机指标：当前值 + 进度条（CPU/内存按阈值着色，磁盘取中性色；趋势图着色见 renderHostSparks） */
  setMetricValue('cpu', o.hostCpuPct);
  setMetricValue('mem', o.hostMemPct);
  setMetricValue('disk', o.hostDiskPct);
  renderDashHostCards(o);
}

/* renderDashHostCards 左栏两卡：整机运行时长 / 网络流量。
   口径均为「开机以来」：运行时长取 GetTickCount64（或 /proc/uptime），
   累计流量直接读网卡计数器——故平台重启不会让这两个数字归零或跳变。 */
function renderDashHostCards(o) {
  const up = Number(o.hostUptimeSec);
  setText('dash-host-uptime', (o.hostUptimeSec === null || o.hostUptimeSec === undefined || isNaN(up)) ? '—' : fmtUptime(up));
  /* 开机时刻由「当前时间 - 已运行时长」反推：后端没有该字段，
     但两者都在前端可得，无需为一行展示再加一个 API 字段。 */
  setText('dash-host-boot', (up > 0) ? '开机 ' + fmtDateTime(Date.now() - up * 1000) : '—');

  setText('dash-net-rx-rate', fmtKBps(o.netRxKBps));
  setText('dash-net-tx-rate', fmtKBps(o.netTxKBps));
  /* 累计流量沿用 fmtBytes（B/KB/MB/GB 自适应），与本页状态表网络列同口径。
     收/发各写一行：拼一行在 180px 卡宽内放不下会被 nowrap 截断 */
  setText('dash-net-rx-total', fmtBytes(o.netRxBytes));
  setText('dash-net-tx-total', fmtBytes(o.netTxBytes));
}
/* setMetricValue 写入当前值文本与进度条宽度/告警色。
   CPU/内存阈值分级：>60% 琥珀、>80% 红；磁盘走中性色（见 metricNeutral），
   该判定与趋势图共用，两侧颜色始终一致；无效值显示 — 且条宽归零。 */
function setMetricValue(kind, pct) {
  setText('dash-m-' + kind, fmtPct(pct));
  const bar = el('dash-m-' + kind + '-bar');
  if (!bar) return;
  const num = Number(pct);
  const valid = pct !== null && pct !== undefined && !isNaN(num);
  const v = valid ? clamp(num, 0, 100) : 0;
  bar.style.width = v + '%';
  bar.className = 'progress-bar' +
    (!valid || metricNeutral(kind) ? '' : v > 80 ? ' is-err' : v > 60 ? ' is-warn' : '');
}

/* （原异常置顶表 renderDashAbnormal 已下线：异常行由 renderDashServices
   在「异常」筛选下统一展示，告警原因列复用后端 abnormal 同口径文案。） */

/* ── 三态 ── */
function renderDashLoading() {
  setText('dash-refresh-info', '加载中…');
  const ids = ['dash-total-count', 'dash-ok-count', 'dash-warn-count', 'dash-err-count', 'dash-m-cpu', 'dash-m-mem', 'dash-m-disk', 'dash-platform-version', 'dash-platform-uptime', 'dash-host-uptime', 'dash-host-boot', 'dash-net-rx-rate', 'dash-net-tx-rate', 'dash-net-rx-total', 'dash-net-tx-total'];
  ids.forEach(id => setText(id, '—'));
  const tb = el('dash-services-tbody');
  const st = el('dash-services-state');
  if (tb) tb.innerHTML = '';
  if (st) st.innerHTML = loadingState('运行状态加载中…');
}
function renderDashError(msg) {
  const ids = ['dash-total-count', 'dash-ok-count', 'dash-warn-count', 'dash-err-count', 'dash-m-cpu', 'dash-m-mem', 'dash-m-disk', 'dash-host-uptime', 'dash-host-boot', 'dash-net-rx-rate', 'dash-net-tx-rate', 'dash-net-rx-total', 'dash-net-tx-total'];
  ids.forEach(id => setText(id, '—'));
  const tb = el('dash-services-tbody');
  const st = el('dash-services-state');
  if (tb) tb.innerHTML = '';
  if (st) st.innerHTML = errorState('运行状态加载失败：' + msg, 'retryLoadDashboard()');
  setHtml('dash-state', errorState('总览数据加载失败：' + msg, 'retryLoadDashboard()'));
}
function showDashStale() {
  const stale = el('dash-stale');
  stale.classList.remove('d-none');
  setHtml('dash-stale', '<i class="bi bi-exclamation-triangle-fill me-1"></i>数据刷新失败，显示 ' + fmtClock(state.dashboardAt) + ' 快照' +
    ' <button class="btn btn-sm btn-outline-secondary py-0" onclick="retryLoadDashboard()"><i class="bi bi-arrow-clockwise me-1"></i>重试</button>');
}
function retryLoadDashboard() { refreshDashboard(); }

/* 服务状态表「查看」→ 跳管理区块并打开详情 */
function goServiceDetail(id) {
  switchSection('service');
  openServiceModal(id);
}

/* ══════════════ 服务运行状态表（原「运行」页功能合并至此，只读） ══════════════

数据与大盘轮询同源（refreshDashboard → renderDashAll 联动渲染），
无独立请求与独立轮询；点击上方四张统计卡或筛选 chips 切换显示范围，
操作（启停/升级/卸载/配置）统一前往「管理」页。 */

/* setDashFilter 统一筛选入口：统计卡点击与 chips 点击都走这里。
   写 dashFilter 后仅重渲染状态表与选中态，不发请求。 */
function setDashFilter(key) {
  dashFilter = DASH_FILTERS.some(f => f.key === key) ? key : 'all';
  renderDashFilterChips();
  renderDashServices();
}

/* dashFilterCounts 各分类计数：优先取后端 overview（口径见 stateKind，
   disabled 计入 err，与统计卡一致）；overview 缺失时前端按 SERVICE_STATE 兜底。 */
function dashFilterCounts() {
  const o = (state.dashboard && state.dashboard.overview) || {};
  const c = { all: 0, ok: 0, warn: 0, err: 0 };
  if (o.total !== undefined && o.total !== null) {
    c.all = o.total; c.ok = o.ok ?? 0; c.warn = o.warn ?? 0; c.err = o.err ?? 0;
    return c;
  }
  state.services.forEach(s => {
    const meta = SERVICE_STATE[rtState(s)] || SERVICE_STATE.unknown;
    if (meta.kind === 'muted') c.err++; // 停用计异常，与后端 stateKind 口径一致
    else if (c[meta.kind] !== undefined) c[meta.kind]++;
    c.all++;
  });
  return c;
}

/* renderDashFilterChips 筛选条 + 统计卡选中态。
   卡片与 chips 共用 dashFilter 单一状态源，两侧选中态始终一致；
   卡片额外着色（is-err/is-warn）仍由 renderDashOverview 维护。 */
function renderDashFilterChips() {
  const wrap = el('dash-filter-chips');
  if (!wrap) return;
  const c = dashFilterCounts();
  wrap.innerHTML = DASH_FILTERS.map(f =>
    '<button class="chip' + (dashFilter === f.key ? ' chip-active' : '') + '" onclick="setDashFilter(\'' + f.key + '\')">' +
    f.label + '<span class="chip-count">' + (c[f.key] ?? 0) + '</span></button>'
  ).join('');
  ['total', 'ok', 'warn', 'err'].forEach(k => {
    const card = el('dash-card-' + k);
    if (card) card.classList.toggle('summary-card-active', dashFilter === (k === 'total' ? 'all' : k));
  });
}

/* dashServiceFilter 行过滤：all 全量；其余按 SERVICE_STATE.kind 归类。 */
function dashServiceFilter(s) {
  if (dashFilter === 'all') return true;
  const meta = SERVICE_STATE[rtState(s)] || SERVICE_STATE.unknown;
  return meta.kind === dashFilter;
}

/* renderDashServices 主渲染：筛选 → 计数提示 → 行渲染。
   行数据与原运行页同源（state.services 聚合行），保留全部运行态列，
   并补「告警原因」列（复用后端 abnormal 同口径文案）供告警/异常筛选下快速定位。 */
function renderDashServices() {
  const tb = el('dash-services-tbody');
  const st = el('dash-services-state');
  if (!tb) return;

  if (state.servicesError && !state.services.length) {
    tb.innerHTML = '';
    st.innerHTML = errorState('运行状态加载失败：' + state.servicesError, 'retryLoadDashboard()');
    return;
  }
  if (!state.services.length && state.servicesLoading) {
    tb.innerHTML = '';
    st.innerHTML = loadingState('运行状态加载中…');
    return;
  }

  const rows = state.services.filter(dashServiceFilter);
  const info = el('dash-visible-info');
  if (info) info.textContent = rows.length === state.services.length
    ? '共 ' + rows.length + ' 个服务'
    : '显示 ' + rows.length + ' / ' + state.services.length + ' 个服务';

  if (!rows.length) {
    tb.innerHTML = '';
    st.innerHTML = emptyState('bi-funnel', state.services.length ? '该筛选下暂无服务' : '暂无纳管服务');
    return;
  }
  st.innerHTML = '';
  tb.innerHTML = rows.map(s => {
    const meta = SERVICE_STATE[rtState(s)] || SERVICE_STATE.unknown;
    const rt = s.runtime || {};
    const cpuDanger = (rt.cpuPct || 0) > 80 ? ' style="color:var(--danger)"' : '';
    const memDanger = (rt.memMB || 0) > 512 ? ' style="color:var(--danger)"' : '';
    return '<tr>' +
      '<td class="fw-semibold"><span class="led ' + meta.led + '"></span>' + escapeHtml(svcTitle(s)) + '</td>' +
      '<td><span class="badge ' + meta.badge + '">' + meta.label + '</span></td>' +
      '<td class="text-muted" style="min-width:200px;">' + escapeHtml(rt.lastError || ((state.dashboard && state.dashboard.abnormal) || []).find(a => a.serviceId === s.id)?.reason || '—') + '</td>' +
      '<td><code>' + escapeHtml(s.version || '—') + '</code></td>' +
      '<td class="num-cell">' + fmtUptime(rt.uptimeSec) + '</td>' +
      '<td class="num-cell">' + (rt.restartTotal ?? '—') + '</td>' +
      '<td class="num-cell"' + cpuDanger + '>' + fmtPct(rt.cpuPct) + '</td>' +
      '<td class="num-cell"' + memDanger + '>' + fmtMB(rt.memMB) + '</td>' +
      '<td class="num-cell">↓' + fmtKBps(rt.netRxKBps) + ' ↑' + fmtKBps(rt.netTxKBps) + '</td>' +
      '<td class="text-end"><button class="btn btn-outline-secondary btn-sm" onclick="goServiceDetail(' + s.id + ')">查看</button></td>' +
      '</tr>';
  }).join('');
}
