/* ══════════════════════════════════════════════
   ④ market 市场：顶部 tab（我的/发现/已安装/已禁用/产品）
      + 标签筛选 + CSS 瀑布流卡片
      卡片：名称 / 作者 / 版本 / 发布时间 / 描述 / 依赖标签
   数据：目录与产品套件来自 /api/market/*（后端待实现）。
      已安装/已禁用由 state.services 运行时推导 join。
   ══════════════════════════════════════════════ */

const MARKET_TAB_HINT = {
  mine:      '自研 / 本地微服务（serviceType = native）',
  discover:  '浏览目录中的全部微服务',
  installed: '当前已纳管安装的微服务',
  disabled:  '已停用（enabled = false）的微服务',
  product:   '官方产品套件：多微服务组合，一键部署',
  tools:     '物联网辅助调试工具（扩展预留，即将上线）',
};

/* ── 进入区块（switchSection 钩子） ── */
async function marketEnter() {
  if (!state.market.catalog && !state.market.catalogError) {
    await loadMarket();
  }
  renderMarketTabs();
  renderMarketTagChips();
  renderMarketGrid();
}
function marketLeave() { /* 无轮询，无需清理 */ }

function setMarketTab(key) {
  state.market.tab = key;
  state.market.activeTags = []; /* 切 tab 清空标签筛选 */
  renderMarketTabs();
  renderMarketTagChips();
  renderMarketGrid();
}
function toggleMarketTag(tag) {
  const t = state.market.activeTags;
  const i = t.indexOf(tag);
  if (i >= 0) t.splice(i, 1); else t.push(tag);
  renderMarketTagChips();
  renderMarketGrid();
}

/* ── 目录 join 运行时（name 对齐） ── */
function marketJoined() {
  const cat = state.market.catalog || [];
  const byName = {};
  state.services.forEach(s => { byName[s.name] = s; });
  return cat.map(c => {
    const svc = byName[c.name];
    return Object.assign({}, c, {
      installed: !!svc,
      disabled: !!(svc && !svc.enabled),
      installedVersion: svc ? (svc.version || '') : '',
      svcId: svc ? svc.id : null,
    });
  });
}

/* ── 当前 tab 数据集（数组） ── */
function marketItems() {
  const tab = state.market.tab;
  if (tab === 'product') return state.market.products || [];
  const joined = marketJoined();
  if (tab === 'mine') return joined.filter(c => c.serviceType === 'native');
  if (tab === 'installed') return joined.filter(c => c.installed);
  if (tab === 'disabled') return joined.filter(c => c.disabled);
  return joined; /* discover */
}

/* ── tab 栏 ── */
function renderMarketTabs() {
  const wrap = el('market-tabs');
  if (!wrap) return;
  const counts = {
    mine: marketJoined().filter(c => c.serviceType === 'native').length,
    discover: (state.market.catalog || []).length,
    installed: state.services.filter(s => s.enabled).length,
    disabled: state.services.filter(s => !s.enabled).length,
    product: (state.market.products || []).length,
  };
  wrap.innerHTML = MARKET_TABS.map(t =>
    '<button class="market-tab' + (state.market.tab === t.key ? ' market-tab-active' : '') + '" onclick="setMarketTab(\'' + t.key + '\')">' +
    '<i class="bi ' + t.icon + ' me-1"></i>' + t.label +
    '<span class="market-tab-count">' + (counts[t.key] ?? 0) + '</span></button>'
  ).join('') +
  '<span class="market-tab-hint ms-auto">' + escapeHtml(MARKET_TAB_HINT[state.market.tab] || '') + '</span>';
}

/* ── 标签筛选（交集 AND） ── */
function marketTagPool() {
  const pool = {};
  marketItems().forEach(it => (it.tags || []).forEach(t => { pool[t] = (pool[t] || 0) + 1; }));
  return Object.keys(pool).sort((a, b) => pool[b] - pool[a] || a.localeCompare(b, 'zh')).map(t => ({ tag: t, count: pool[t] }));
}
function renderMarketTagChips() {
  const wrap = el('market-tag-chips');
  if (!wrap) return;
  const pool = marketTagPool();
  const active = state.market.activeTags;
  let html = '<button class="chip' + (!active.length ? ' chip-active' : '') + '" onclick="clearMarketTags()">全部</button>';
  html += pool.map(t =>
    '<button class="chip' + (active.includes(t.tag) ? ' chip-active' : '') + '" onclick="toggleMarketTag(\'' + escapeHtml(t.tag) + '\')">' +
    escapeHtml(t.tag) + '<span class="chip-count">' + t.count + '</span></button>'
  ).join('');
  if (active.length) html += '<button class="chip chip-clear" onclick="clearMarketTags()" title="清除标签筛选"><i class="bi bi-x-lg"></i></button>';
  wrap.innerHTML = html;
}
function clearMarketTags() {
  state.market.activeTags = [];
  renderMarketTagChips();
  renderMarketGrid();
}

/* ── 时间格式 ── */
function fmtAgo(ts) {
  if (!ts) return '—';
  const diff = Date.now() - ts;
  const d = Math.floor(diff / 86400000);
  if (d <= 0) return '今天';
  if (d < 30) return d + ' 天前';
  if (d < 365) return Math.floor(d / 30) + ' 个月前';
  return Math.floor(d / 365) + ' 年前';
}

/* ── 瀑布流渲染 ── */
function renderMarketGrid() {
  const grid = el('market-grid');
  const st = el('market-state');
  if (!grid) return;

  /* 工具 tab：独立渲染（不走目录数据，占位说明卡片）。 */
  if (state.market.tab === 'tools') {
    grid.innerHTML = IOT_TOOLS.map(toolCardHtml).join('');
    st.innerHTML = '';
    el('market-tag-chips').innerHTML = '';
    return;
  }

  if (state.market.catalogError && !state.market.catalog) {
    grid.innerHTML = '';
    st.innerHTML = errorState('市场目录加载失败：' + state.market.catalogError + ('（后端暂未提供 /api/market 接口）'), 'marketRetry()');
    return;
  }
  if (state.market.loading && !state.market.catalog) {
    grid.innerHTML = '';
    st.innerHTML = loadingState('市场目录加载中…');
    return;
  }

  const tab = state.market.tab;
  const active = state.market.activeTags;
  let items = marketItems().filter(it => active.every(t => (it.tags || []).includes(t)));

  if (!items.length) {
    grid.innerHTML = '';
    st.innerHTML = emptyState('bi-shop', active.length ? '该标签组合下暂无内容' : '该分类下暂无内容');
    return;
  }
  st.innerHTML = '';
  grid.innerHTML = items.map(it => tab === 'product' ? productCardHtml(it) : marketCardHtml(it)).join('');
}
function marketRetry() { loadMarket().then(() => marketEnter()); }

/* ── 微服务卡片（瀑布流单元） ── */
function marketCardHtml(c) {
  const stMeta = c.installed
    ? (c.disabled ? '<span class="badge badge-muted">已禁用</span>' : '<span class="badge badge-ok">已安装</span>')
    : '<span class="badge badge-plain">未安装</span>';
  const depTags = (c.deps || []).length
    ? '<div class="market-card-deps"><i class="bi bi-arrow-repeat me-1"></i>' +
      c.deps.map(d => '<span class="mc-tag mc-tag-dep">' + escapeHtml(d) + '</span>').join('') + '</div>'
    : '';
  let actions;
  if (!c.installed) {
    actions = '<button class="btn btn-primary btn-sm" onclick="openInstallModal(\'' + escapeHtml(c.name) + '\')"><i class="bi bi-download me-1"></i>安装</button>';
  } else {
    actions = '<button class="btn btn-outline-primary btn-sm" onclick="goServiceDetail(' + c.svcId + ')"><i class="bi bi-sliders me-1"></i>管理</button>' +
      '<button class="btn btn-outline-secondary btn-sm" onclick="switchSection(\'service\')"><i class="bi bi-cloud-arrow-up me-1"></i>升级</button>';
  }
  return '<div class="market-card">' +
    '<div class="market-card-head">' +
      '<div class="model-card-icon"><i class="bi ' + (c.serviceType === 'opensource' ? 'bi-box-seam' : 'bi-app-window') + '"></i></div>' +
      '<div class="min-w-0">' +
        '<div class="market-card-name" title="' + escapeHtml(c.display || c.name) + '">' + escapeHtml(c.display || c.name) + '</div>' +
        '<div class="market-card-meta"><i class="bi bi-person me-1"></i>' + escapeHtml(c.author || '—') + '</div>' +
      '</div>' +
      stMeta +
    '</div>' +
    '<div class="market-card-version"><code>' + escapeHtml(c.installed ? (c.installedVersion || c.version) : c.version) + '</code>' +
      (c.installed && c.installedVersion !== c.version ? ' <span class="badge badge-r">目录 ' + escapeHtml(c.version) + '</span>' : '') +
      '<span class="ms-auto small" style="color:var(--muted-color)"><i class="bi bi-clock me-1"></i>' + fmtAgo(c.publishedAt) + '发布</span></div>' +
    '<div class="market-card-desc">' + escapeHtml(c.description || '') + '</div>' +
    '<div class="market-card-tags">' + (c.tags || []).map(t => '<span class="mc-tag">' + escapeHtml(t) + '</span>').join('') + '</div>' +
    depTags +
    '<div class="market-card-actions">' + actions + '</div>' +
    '</div>';
}

/* ── 产品套件卡（组合形态） ── */
function productCardHtml(p) {
  const joined = marketJoined();
  const byName = {};
  joined.forEach(c => { byName[c.name] = c; });
  const svcRows = (p.services || []).map(sv => {
    const j = byName[sv.name];
    const stateBadge = !j ? '<span class="badge badge-plain">未装</span>'
      : (j.disabled ? '<span class="badge badge-muted">已禁用</span>' : '<span class="badge badge-ok">' + escapeHtml(j.installedVersion || '已装') + '</span>');
    return '<li><span class="fw-semibold">' + escapeHtml(sv.name) + '</span><code class="ms-2">' + escapeHtml(sv.version) + '</code>' +
      '<span class="ms-auto">' + stateBadge + '</span></li>';
  }).join('');
  return '<div class="market-card market-card-product">' +
    '<div class="market-card-head">' +
      '<div class="model-card-icon"><i class="bi bi-box-seam"></i></div>' +
      '<div class="min-w-0">' +
        '<div class="market-card-name">' + escapeHtml(p.name) + '</div>' +
        '<div class="market-card-meta"><i class="bi bi-person me-1"></i>' + escapeHtml(p.author || '—') + ' · <code>' + escapeHtml(p.version) + '</code></div>' +
      '</div>' +
    '</div>' +
    '<div class="market-card-version"><span class="small" style="color:var(--muted-color)"><i class="bi bi-clock me-1"></i>' + fmtAgo(p.publishedAt) + '发布</span></div>' +
    '<div class="market-card-desc">' + escapeHtml(p.description || '') + '</div>' +
    '<div class="market-card-tags">' + (p.tags || []).map(t => '<span class="mc-tag">' + escapeHtml(t) + '</span>').join('') + '</div>' +
    '<ul class="market-card-svc-list">' + svcRows + '</ul>' +
    '<div class="market-card-actions">' +
      '<button class="btn btn-primary btn-sm" onclick="openInstallModal(\'' + escapeHtml(p.name) + '\', true)"><i class="bi bi-box-seam me-1"></i>安装套件</button>' +
    '</div>' +
    '</div>';
}

/* ══════════════ 市场安装流（manifest 说明文档 → 预检 → 安装） ══════════════ */
let installCtx = null; /* { manifest, suite:bool } */

function openInstallModal(name, isProduct) {
  const entry = isProduct
    ? (state.market.products || []).find(p => p.name === name)
    : (state.market.catalog || []).find(c => c.name === name);
  if (!entry) { showToast('warning', '未找到该条目的说明文档'); return; }
  if (isProduct) { openSuiteInstall(entry); return; }
  /* 目录条目即 manifest（同一 schema 两处消费） */
  const manifest = deepCopy(entry);
  installCtx = { manifest, suite: false };
  setText('im-name', manifest.displayName || manifest.name);
  setText('im-version', manifest.version || '—');
  setText('im-desc', manifest.description || '—');
  setText('im-author', manifest.author || '—');
  setText('im-tags', (manifest.tags || []).join(' · ') || '—');
  /* 依赖说明 */
  const depBanner = el('im-dep-banner');
  const deps = manifest.deps || [];
  const missing = deps.filter(d => !state.services.some(s => s.name === d));
  if (!deps.length) depBanner.innerHTML = '<i class="bi bi-check-circle me-2" style="color:var(--success)"></i>无前置依赖，可直接安装';
  else if (missing.length) depBanner.innerHTML = '<i class="bi bi-exclamation-triangle-fill me-2" style="color:var(--warning)"></i>依赖 <b>' + missing.map(escapeHtml).join('、') + '</b> 尚未安装，预检将阻止安装（请先安装依赖）';
  else depBanner.innerHTML = '<i class="bi bi-check-circle me-2" style="color:var(--success)"></i>依赖已就绪：' + deps.map(escapeHtml).join('、');
  setVal('im-args', (manifest.args || []).join('\n'));
  setVal('im-env', Object.entries(manifest.env || {}).map(([k, v]) => k + '=' + v).join('\n'));
  setChecked('im-autoStart', true);
  setChecked('im-enabled', true);
  setVal('im-priority', 100);
  setHtml('im-precheck', '<div class="text-muted small">点击「预检并安装」后显示检查结果</div>');
  btnIdle(el('im-confirm'));
  installModal.show();
}

async function doInstall() {
  if (!installCtx) return;
  const m = installCtx.manifest;
  const btn = el('im-confirm');
  btnBusy(btn, '安装中…');
  setHtml('im-precheck', loadingState('预检并安装中…'));
  try {
    const env = {};
    String(val('im-env')).split('\n').map(s => s.trim()).filter(Boolean).forEach(line => {
      const i = line.indexOf('=');
      if (i > 0) env[line.slice(0, i)] = line.slice(i + 1);
    });
    const r = await apiPost('/services/install', {
      manifest: m,
      overrides: {
        args: String(val('im-args')).split('\n').map(s => s.trim()).filter(Boolean),
        env,
        enabled: checked('im-enabled'),
        autoStart: checked('im-autoStart'),
        priority: toNum(val('im-priority'), 100),
      },
    });
    renderInstallIssues(r.issues || [], true);
    showToast('success', '安装完成：' + (m.displayName || m.name) + ' ' + m.version);
    await loadServices(true);
    renderServiceCards();
    renderMarketGrid();
    setTimeout(() => { installModal.hide(); switchSection('service'); const card = el('svc-card-' + r.id); if (card) card.classList.add('svc-card-fade-in'); }, 900);
  } catch (e) {
    renderInstallIssues(e.issues, false, e.message);
    showToast('danger', '安装失败：' + e.message);
    btnIdle(btn);
    return;
  }
  btnIdle(btn);
}

function renderInstallIssues(issues, okFlag, errMsg) {
  const wrap = el('im-precheck');
  if (errMsg && !(issues && issues.length)) {
    wrap.innerHTML = '<div class="um-result-banner err"><i class="bi bi-x-octagon-fill me-2"></i>' + escapeHtml(errMsg) + '</div>';
    return;
  }
  const list = issues || [];
  const errs = list.filter(i => i.level === 'error');
  const warns = list.filter(i => i.level === 'warn');
  let html = '';
  if (okFlag) html += '<div class="um-result-banner ok mb-2"><i class="bi bi-check-circle-fill me-2"></i>安装成功' + (warns.length ? '（' + warns.length + ' 条提示）' : '') + '</div>';
  else if (errs.length) html += '<div class="um-result-banner err mb-2"><i class="bi bi-x-octagon-fill me-2"></i>预检未通过（' + errs.length + ' 项阻断）</div>';
  if (list.length) {
    html += '<ul class="mb-0 ps-3" style="font-size:13px;">' + list.map(i =>
      '<li class="mt-1"><i class="bi ' + (i.level === 'error' ? 'bi-x-circle-fill' : 'bi-exclamation-triangle-fill') + ' me-1" style="color:var(--' + (i.level === 'error' ? 'danger' : 'warning') + ')"></i>' +
      '<b>' + escapeHtml(i.item) + '</b>：' + escapeHtml(i.msg) + '</li>').join('') + '</ul>';
  } else if (!okFlag) html += '<div class="text-muted small">无预检问题</div>';
  wrap.innerHTML = html;
}

/* ── 产品套件安装：按清单顺序逐个安装（依赖顺序由套件定义保证） ── */
function openSuiteInstall(suite) {
  const catalog = state.market.catalog || [];
  const items = (suite.services || []).map(sv => catalog.find(c => c.name === sv.name)).filter(Boolean);
  if (!items.length) { showToast('warning', '套件清单为空'); return; }
  const rows = items.map(m => {
    const svc = state.services.find(s => s.name === m.name);
    const st = svc ? (svc.enabled ? '<span class="badge badge-ok">已装 ' + escapeHtml(svc.version || '') + '</span>' : '<span class="badge badge-muted">已禁用</span>') : '<span class="badge badge-plain">未装</span>';
    return '<li class="mt-1 d-flex align-items-center"><span class="fw-semibold">' + escapeHtml(m.displayName || m.name) + '</span><code class="ms-2">' + escapeHtml(m.version) + '</code><span class="ms-auto">' + st + '</span></li>';
  }).join('');
  confirmDialog({
    title: '安装产品套件 · ' + suite.name,
    html: '将按依赖顺序安装以下 <b>' + items.length + '</b> 个微服务（manifest 默认参数，可安装后逐个调整）：<ul class="mt-2 mb-0 ps-3" style="font-size:13px;">' + rows + '</ul>' +
      '<div class="mt-2 text-muted" style="font-size:12.5px;">已安装的条目将跳过；任一安装失败会停止后续安装。</div>',
    level: 'L1', confirmText: '开始安装',
  }).then(ok => { if (ok) installSuite(items); });
}

async function installSuite(items) {
  let done = 0, skipped = 0, failed = null;
  for (const m of items) {
    if (state.services.some(s => s.name === m.name)) { skipped++; continue; }
    try {
      await apiPost('/services/install', { manifest: deepCopy(m), overrides: { autoStart: true, enabled: true, priority: 100 } });
      done++;
    } catch (e) { failed = { name: m.name, msg: e.message }; break; }
  }
  await loadServices(true);
  renderServiceCards();
  renderMarketGrid();
  if (failed) showToast('warning', '套件安装中断于「' + failed.name + '」：' + failed.msg + '（已装 ' + done + ' 个）');
  else showToast('success', '套件安装完成：新装 ' + done + ' 个，跳过已装 ' + skipped + ' 个');
  switchSection('service');
}

/* ══════════════════════════════════════════════
   工具 tab：物联网辅助调试工具（扩展预留，当前占位）
   未来集成 cantest / nats-cli / modbus 等调试工具，
   以微服务形式安装运行或内嵌 Web 终端。
   ══════════════════════════════════════════════ */

const IOT_TOOLS = [
  { key: 'cantest',    name: 'CAN Test',         icon: 'bi-broadcast',   desc: 'CAN 总线调试：发送/接收报文、DBC 解析、总线监控、错误帧统计', tags: ['CAN', '总线', '调试'] },
  { key: 'modbus',     name: 'Modbus Poll',      icon: 'bi-ethernet',    desc: 'Modbus RTU/TCP 主从站模拟：寄存器读写、批量轮询、报文抓取', tags: ['Modbus', 'RTU', 'TCP'] },
  { key: 'nats-cli',   name: 'NATS CLI',         icon: 'bi-arrow-left-right', desc: 'NATS 消息总线命令行：pub/sub/request、队列监控、JetStream 管理', tags: ['NATS', '消息'] },
  { key: 'mqtt',       name: 'MQTT Client',      icon: 'bi-wifi-router', desc: 'MQTT 连接测试：主题订阅/发布、遗嘱消息、QoS 验证、报文日志', tags: ['MQTT', 'IoT'] },
  { key: 'serial',     name: 'Serial Terminal',  icon: 'bi-usb-symbol',  desc: '串口终端：RS-485/232 波特率扫描、十六进制/ASCII 视图、脚本回放', tags: ['串口', 'RS-485'] },
  { key: 'sniffer',    name: 'Protocol Sniffer', icon: 'bi-activity',    desc: '多协议抓包分析：自动识别 Modbus/MQTT/HTTP/自定义帧，时间线视图', tags: ['抓包', '分析'] },
];

function toolCardHtml(t) {
  return '<div class="market-card" style="opacity:.75">' +
    '<div class="market-card-head">' +
      '<div class="model-card-icon"><i class="bi ' + t.icon + '"></i></div>' +
      '<div class="min-w-0">' +
        '<div class="market-card-name">' + escapeHtml(t.name) + '</div>' +
        '<div class="market-card-meta">辅助调试工具</div>' +
      '</div>' +
      '<span class="badge badge-muted">即将上线</span>' +
    '</div>' +
    '<div class="market-card-desc">' + escapeHtml(t.desc) + '</div>' +
    '<div class="market-card-tags">' + t.tags.map(tag => '<span class="mc-tag">' + escapeHtml(tag) + '</span>').join('') + '</div>' +
    '<div class="market-card-actions">' +
      '<button class="btn btn-outline-secondary btn-sm" disabled><i class="bi bi-hourglass-split me-1"></i>开发中</button>' +
    '</div>' +
    '</div>';
}
