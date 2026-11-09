/* ══════════════════════════════════════════════
   常量映射 + 全局 state（加载顺序第 1 位）
   ══════════════════════════════════════════════ */

/* 服务运行态 → LED / badge / 中文标签 / 分类
   分类: ok=正常  warn=告警  err=异常  muted=停用
   状态枚举对齐架构设计 §3.1（9 态 + disabled 由 enabled=false 推导） */
const SERVICE_STATE = {
  running:   { label: '运行中',   led: 'led-on',      badge: 'badge-ok',    kind: 'ok' },
  starting:  { label: '启动中',   led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  waiting:   { label: '等待依赖', led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  stopping:  { label: '停止中',   led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  upgrading: { label: '升级中',   led: 'led-pending', badge: 'badge-plain', kind: 'ok' },
  degraded:  { label: '降级',     led: 'led-warn',    badge: 'badge-r',     kind: 'warn' },
  backoff:   { label: '退避中',   led: 'led-warn',    badge: 'badge-r',     kind: 'warn' },
  failed:    { label: '异常',     led: 'led-err',     badge: 'badge-err',   kind: 'err' },
  stopped:   { label: '已停止',   led: 'led-off',     badge: 'badge-muted', kind: 'err' }, // enabled 但 stopped 计异常
  disabled:  { label: '已停用',   led: 'led-off',     badge: 'badge-muted', kind: 'muted' },
  unknown:   { label: '未知',     led: 'led-off',     badge: 'badge-muted', kind: 'err' },
};

const SERVICE_TYPE_LABEL = { native: '自研', opensource: '开源' };
const APPLY_MODE_LABEL   = { restart: '重启服务', command: '执行热加载' };

/* 日志级别 → 着色类（INFO 用默认色） */
const LOG_LEVEL_META = {
  DEBUG: { cls: 'log-lvl-debug' },
  INFO:  { cls: '' },
  WARN:  { cls: 'log-lvl-warn' },
  ERROR: { cls: 'log-lvl-error' },
};

/* 升级阶段清单（交互说明 5.2.3：下载→验签→预检→停服→安装→切换→健康观察→提交） */
const UPGRADE_PHASES = [
  { key: 'download', label: '下载' },
  { key: 'verify',   label: '验签' },
  { key: 'precheck', label: '预检' },
  { key: 'stop',     label: '停服' },
  { key: 'install',  label: '安装' },
  { key: 'switch',   label: '切换' },
  { key: 'health',   label: '健康观察' },
  { key: 'commit',   label: '提交' },
];
/* 回滚阶段（交互说明 3.5：无下载/验签，直接从停服起） */
const ROLLBACK_PHASES = [
  { key: 'stop',   label: '停服' },
  { key: 'switch', label: '切换旧版' },
  { key: 'health', label: '健康观察' },
  { key: 'commit', label: '完成' },
];

const UPGRADE_SOURCE = { tftp: 'TFTP', usb: 'U盘', manual: '手动', local: '本地', market: '市场安装' };
const RECORD_STATUS = {
  success:    ['badge-ok',  '成功'],
  failed:     ['badge-err', '失败'],
  rolledback: ['badge-r',   '已回滚'],
};

/* 日志环形缓冲上限默认值（交互说明 3.7 步骤 3）。
   实际取值以设置为准（logQuota.bufferLines），由 logBufferMax() 解析。 */
const LOG_BUFFER_MAX_DEFAULT = 2000;

/* 重启策略标签：与后端实际落库的 restartPolicy.mode 取值一致（backoff | periodic）。
   历史键 always/onFailure 来自已废弃的设计，导致服务卡片恒显示兜底文案。 */
const RESTART_MODE_LABEL = { backoff: '退避重启', periodic: '周期重启' };

/* 市场 tab（用户需求：我的=自研 / 发现=全目录 / 已安装 / 已禁用 / 产品=官方套件 / 工具=调试工具） */
const MARKET_TABS = [
  { key: 'mine',     label: '我的',     icon: 'bi-person-workspace' },
  { key: 'discover', label: '发现',     icon: 'bi-compass' },
  { key: 'installed',label: '已安装',   icon: 'bi-check2-circle' },
  { key: 'disabled', label: '已禁用',   icon: 'bi-slash-circle' },
  { key: 'product',  label: '产品',     icon: 'bi-box-seam' },
  { key: 'tools',    label: '工具',     icon: 'bi-tools' },
];

/* 升级来源（管理页：OTA 远程 / OTG U盘 / LOCAL 本地） */
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
    reconnectAttempts: 0, truncated: false, needRebuild: false,
  },
  /* 升级 */
  update: {
    available: {},          // serviceId → check 返回条目
    checkedAt: 0,
    records: [], recordsError: null,
    usb: null, usbError: null,
    taskId: null, taskActive: false,
    checking: false,
  },
  /* 设置 */
  settings: null, settingsLoading: false, settingsError: null,
  /* 市场 */
  market: {
    tab: 'discover',          // 当前 tab：mine|discover|installed|disabled|product
    activeTags: [],           // 标签筛选（交集）
    catalog: null,            // /api/market/catalog 发现目录
    catalogError: null,
    products: null,           // /api/market/products 官方产品套件
    loading: false,
  },
};