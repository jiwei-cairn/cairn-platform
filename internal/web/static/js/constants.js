/* ══════════════════════════════════════════════
   常量映射 + 全局 state（加载顺序第 1 位）
   ══════════════════════════════════════════════ */

/* 服务运行态 → LED / badge / 中文标签 / 分类
   分类: ok=正常  warn=告警  err=异常  muted=停用
   监管八态 + disabled（enabled=false 时由接口覆盖） */
const SERVICE_STATE = {
  running:   { label: '运行中',   led: 'led-on',      badge: 'badge-ok',    kind: 'ok' },
  starting:  { label: '启动中',   led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  waiting:   { label: '等待依赖', led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  stopping:  { label: '停止中',   led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  degraded:  { label: '降级',     led: 'led-warn',    badge: 'badge-r',     kind: 'warn' },
  backoff:   { label: '退避中',   led: 'led-warn',    badge: 'badge-r',     kind: 'warn' },
  failed:    { label: '异常',     led: 'led-err',     badge: 'badge-err',   kind: 'err' },
  stopped:   { label: '已停止',   led: 'led-off',     badge: 'badge-muted', kind: 'err' },
  disabled:  { label: '已停用',   led: 'led-off',     badge: 'badge-muted', kind: 'muted' },
  unknown:   { label: '未知',     led: 'led-off',     badge: 'badge-muted', kind: 'err' },
};

const SERVICE_TYPE_LABEL = { native: '自研', opensource: '开源' };

/* 日志级别 → 着色类（INFO 用默认色） */
const LOG_LEVEL_META = {
  DEBUG: { cls: 'log-lvl-debug' },
  INFO:  { cls: '' },
  WARN:  { cls: 'log-lvl-warn' },
  ERROR: { cls: 'log-lvl-error' },
};

/* 日志环形缓冲上限默认值。实际取值以设置为准（logQuota.bufferLines）。 */
const LOG_BUFFER_MAX_DEFAULT = 2000;

const RESTART_MODE_LABEL = { backoff: '退避重启', periodic: '周期重启' };

const MARKET_TABS = [
  { key: 'mine',     label: '我的',     icon: 'bi-person-workspace' },
  { key: 'discover', label: '发现',     icon: 'bi-compass' },
  { key: 'installed',label: '已安装',   icon: 'bi-check2-circle' },
  { key: 'disabled', label: '已禁用',   icon: 'bi-slash-circle' },
  { key: 'product',  label: '产品',     icon: 'bi-box-seam' },
];

/* 全局 state —— 前端唯一数据源（交互说明 §1.2） */
const state = {
  version: null,            // 平台自身版本（只读）
  /* 大盘 */
  dashboard: null,
  dashboardError: false,
  dashboardAt: 0,           // 最后一次成功刷新时间戳
  /* 微服务 */
  services: [],             // /api/services 聚合行（定义 + runtime）
  servicesLoading: false,
  servicesError: null,
  editingServiceId: null,   // null=列表态；0=新建；>0=编辑
  optimistic: {},           // id → 乐观过渡态（starting/stopping）
  /* 日志中心 */
  log: {
    es: null, lines: [], paused: false, pendingCount: 0,
    serviceFilter: '', keyword: '', levelFilter: '', connected: false,
    reconnectAttempts: 0, truncated: false, needRebuild: false, lastTs: 0,
  },
  /* 升级 */
  update: {
    available: {},          // serviceId → check 返回条目
    checkedAt: 0,
    records: [], recordsError: null,
    taskId: null, taskActive: false,
    checking: false,
  },
  /* 设置 */
  settings: null, settingsLoading: false, settingsError: null,
  /* 市场 */
  market: {
    tab: 'discover',          // mine|discover|installed|disabled|product
    activeTags: [],           // 标签筛选（交集）
    catalog: null,            // /api/market/catalog 发现目录
    catalogError: null,
    products: null,           // /api/market/products 官方产品套件
    loading: false,
  },
};