# 架构设计：后端平台框架（积微 Cairn 微服务管理平台 V1）

> 输入文档：[需求梳理 V1](需求梳理-积微 Cairn 微服务管理平台-V1.md)（第 5/6/7 节基线）、
> [frontend-backend-collaboration.md](frontend-backend-collaboration.md)（五件套骨架与协作契约）、
> [功能范围与需求决策](功能范围与需求决策-积微 Cairn 微服务管理平台.md)（架构红线）。
>
> **本轮目标**：搭后端平台框架，支撑前端 UI 原型先行；本文给出包结构、契约冻结清单、
> 状态机与 Runtime 抽象、mock 切换策略四个决策包，作为开发基线。

---

## 0. 设计原则与红线落位

| 红线/原则 | 本文落位 |
|-----------|---------|
| 管理面不依赖 NATS | 平台全仓库 **零 nats import**（CI grep 检查，见 §5）；被管服务指标走本地 gopsutil、日志走本地文件，不经消息总线回传 |
| 升级原子切换 + 回滚 | update 管线独占 staging→fsync→rename 语义，tftp/usb/manual 只是"获取通道"差异（§1.4） |
| 日志/指标配额轮转 | 平台日志 lumberjack（logx 既有）；指标表两级降采样 + 行数配额（§2.2 MetricPoint）；被管服务日志按 logPolicy 轮转 |
| 单体二进制、无构建工具、纯 Go 无 CGO | 沿用五件套；进程管理用 os/exec 直接 fork（M1 决策修订，见 §3.4 与 M1 方案 §0），零第三方依赖，交叉编译不受影响 |
| 接口化解耦、facade 可替换 | api 层三个 facade（Supervisor/Update/Stats）+ runtime 抽象，**mock 切换的根基**（§4） |

---

## 1. 模块划分、包结构与依赖方向

### 1.1 顶层模块命名

与既有兄弟模块（ThingsModel / Gateway-Modbus / AlarmEngine）保持同风格：

- **现行：目录 `cairn-platform`，go.mod module 名 `github.com/jiwei-cairn/cairn-platform`**，systemd unit 名 `cairn-console.service`。
  理由：产品定位是"管理平台/控制台"，与被管的业务微服务在仓库层级一眼可区分；
  避免与内部 `supervisor` 包重名，也避免与社区 supervisord 概念混淆。
  （前身为单目录 `Console` / module `console`，平台化整合时更名。）
- 备选：`Platform`（对应功能范围文档 F 节"平台基础"），语义更泛，作次选。
- 不推荐：`Supervisor`（与 internal/supervisor 重名，import 路径丑陋）。

### 1.2 目录结构

```text
cairn-platform/
├── main.go                       # 装配：唯一知道所有具体实现的层
├── go.mod                        # module github.com/jiwei-cairn/cairn-platform
└── internal/
    ├── config/                   # 五件套①：平台设置类型+默认值回落+校验
    ├── store/                    # 五件套②：GORM 模型集中（models.go）+ AutoMigrate
    ├── api/                      # 五件套③：REST 处理器 + facade 接口定义
    │   ├── api.go                #   Server + SupervisorFacade/UpdateFacade/StatsSource + ok/fail
    │   ├── service.go            #   服务定义 CRUD + 启停操作 + configfile
    │   ├── dashboard.go          #   大盘聚合
    │   ├── logs.go               #   日志快照/SSE/导出
    │   ├── upgrade.go            #   升级检查/应用/进度/历史/回滚
    │   ├── usb.go                #   U盘状态
    │   ├── events.go             #   事件流水
    │   └── settings.go           #   平台配置
    ├── web/                      # 五件套④：chi 路由 + go:embed 静态 SPA
    │   └── static/               #   js/{dash,service,log,update,settings}.js 五区块
    ├── logx/                     # 五件套⑤：slog 三路 fanout（直接复用）
    ├── buildinfo/                # 版本注入（复用）
    ├── supervisor/               # ★ 监管引擎（对标 Gateway 的 internal/engine）
    │   ├── supervisor.go         #   差量调谐（指纹策略，复用 Gateway 模式）
    │   ├── worker.go             #   单服务监管循环：探针调度+重启决策+状态机
    │   ├── graph.go              #   dependsOn DAG 校验（拒绝环）+ 拓扑序/逆序
    │   └── health/               #   L2 探针实现：http / tcp
    ├── runtime/                  # ★ 进程管理器抽象（平台级，非 supervisor 私有）
    │   ├── runtime.go            #   接口 + UnitState 中立枚举 + 可选 ExitWatcher 能力接口
    │   ├── fork/                 #   M1 实现：os/exec 直接 fork + Wait 事件 + 孤儿认领（含 Windows 降级分文件）
    │   ├── systemd/              #   备选实现（未排期：需 cgroup 硬限等场景再评估）
    │   └── fake/                 #   内存实现：单测桩 + UI 原型 mock（一物两用）
    ├── update/                   # ★ 升级管线
    │   ├── pipeline.go           #   验签→预检→暂停监管→staging→原子切换→验证→commit/回滚
    │   ├── pack/                 #   统一包格式解析 + ed25519 验签 + sha256 校验
    │   └── fetch/                #   获取通道（策略模式）：tftp.go / usb.go / manual.go
    ├── collect/                  # ★ 采集统计：gopsutil 采样 + 环形缓冲 + 降采样落库
    └── svcfg/                    #   被管服务配置文件管理：读/校验/备份/生效/历史
```

命名说明：`supervisor` 对应需求 B 节（看门狗守护）；`update` 对应 E 节（OTA/OTG 同一管线）；
`collect` 对应 C 节统计口径（Q1 已确认是性能指标而非能耗）；`svcfg` 是 service-config 缩写，对应 H 节。

### 1.3 依赖方向（强约束）

```text
main.go ──► 装配（store/logx/runtime 实现/supervisor/update/collect → 注入 api → web）

web ──► api ──┬─► store（读写定义与记录）
              ├─(SupervisorFacade)──► supervisor ──► runtime（抽象）+ health + store
              ├─(UpdateFacade)─────► update ──► pack + fetch + store
              └─(StatsSource)──────► collect ──► runtime（PID 注册表）+ gopsutil

update ──(窄接口 LifecycleCtrl)──► supervisor     # 单向：升级时暂停/恢复监管、启停受控
所有包 ──► logx、buildinfo
```

规则清单（进 CI 检查，见 §5）：

1. **严格单向**：web → api → 领域层（supervisor/update/collect/svcfg）→ store/runtime；
   领域层严禁反向 import api/web。
2. **api 只依赖 facade**：三个 facade 接口定义在 `api` 包内（Go 消费者接口惯例，同 Gateway
   的 EngineFacade），具体实现由 main 装配注入——编译期保证 api 不知道引擎细节。
3. **update → supervisor 单向**，且 update 内部以窄接口 `LifecycleCtrl`
   （Pause/Resume/RestartAndWait/HealthWait）引用，便于 update 单测用桩。
4. **collect 独立于 supervisor**：PID 从 runtime 的注册表取（fake 实现同样填充注册表），
   采集周期调谐不触碰监管状态机；两者仅共享 store 与 runtime。
5. **runtime 不含业务**：健康探针、重启策略、依赖门控全在 supervisor；
   进程 fork/信号/认领细节全在 runtime/fork。互不越界。
6. **零 nats import**（红线）；web/static 不引入构建工具、不内置 mock（§4）。

### 1.4 各引擎如何挂进五件套

核心洞察：**supervisor 就是这个平台的 "engine"**，Gateway 的热重载模式可以原样平移：

| Gateway（参照） | 本平台 | 说明 |
|----------------|--------|------|
| engine.Engine + worker per channel | supervisor + worker per service | 每服务一个监管循环，独占自己的探针/退避计时器 |
| ChannelPlan + planFingerprint 差量调谐 | Service 定义指纹（unitFingerprint） | Service 定义变更 → api 写库 → `reloadSupervisor()` → 指纹比对 → 无/增/删/改(重启) |
| EngineFacade | SupervisorFacade / UpdateFacade / StatsSource | api 层零锁、引擎收敛并发 |
| 写命令 writeCh 非阻塞投递 | 启停请求提交至 worker 命令队列 | 队列满返回 503，语义一致 |

指纹设计两档（继承 Gateway "排除显示字段防抖动"的经验）：

- **unitFingerprint**（变化→重启进程）：exePath、args、workDir、env、unitName、autoStart、enabled；
- **监管参数**（变化→只更新 worker 配置，不重启）：restartPolicy、healthCheck、logPolicy。
  尤其 healthCheck 调参不能引发服务重启——现场调探针阈值是高频操作。

update 管线阶段（获取通道只是第一步的策略差异，后续同一流水线）：

```text
fetch(tftp / usb / manual) → pack 解析+ed25519 验签 → 预检(磁盘/最低兼容版本/版本防回滚)
  → LifecycleCtrl.Pause(目标+受影响依赖方) → staging 安装 + fsync
  → 原子 rename 切换 → 重启 + 健康验证期 → commit(保留最近 N=2 版) / 失败自动回滚
  → Resume 监管 → UpgradeRecord 落库 → (usb 来源) report.txt 写回 U盘
```

平台自升级（E7）：平台 unit 由 systemd 拉起，走同一管线的 staging+rename，二进制切换后
平台 exit(0)，由 systemd Restart=always 拉起新版本——不需要特殊通道。

---

## 2. API 契约冻结清单与数据模型定稿

### 2.1 端点分级（"冻结"= 路径/方法/JSON 形状一经确定，变更只允许增量）

统一 `{code, data}` 封装、资源名复数、camelCase（协作契约照旧）。分级：

**A 类：本轮即真实实现**（无 OS 依赖，直接套三件套模板）：

| 端点 | 说明 |
|------|------|
| GET/POST/DELETE `/api/services[/{id}]` | 定义 CRUD 落库；列表响应**附运行态摘要**（见 §2.2） |
| GET/POST `/api/settings` | Setting 单行表照搬 ThingsModel 模式 |
| GET `/api/version` | buildinfo；响应含 `runtime` 与 `mock` 标识（演示角标用） |
| GET `/api/upgrade/records` | 历史表真实查询（原型期由 fake 预置种子数据） |

**B 类：形状冻结 + fake 实现**（UI 原型立即可用，里程碑替换）：

| 端点 | fake 行为 | 替换里程碑 |
|------|-----------|-----------|
| POST `/api/services/{id}/start\|stop\|restart` | fake runtime 状态机可演示迁移 | M1 |
| GET `/api/dashboard` | 聚合假数据（host/summary/abnormal/services） | M1 |
| GET `/api/events`（**新增**） | 假事件流（含 level） | M1 |
| GET `/api/services/{id}/stats` | 假曲线序列（时间桶数组） | M3 |
| POST `/api/upgrade/check` | 按 TFTP 配置返回假 index.json 清单 | M2 |
| POST `/api/upgrade/apply` | 模拟进度（互斥：并发返回 409） | M2 |
| GET `/api/upgrade/tasks/{taskId}`（**新增**） | 任务状态：phase/progress/receivedBytes/totalBytes/detail/result（apply 受理返回 taskId） | M2 |
| POST `/api/upgrade/records/{id}/rollback` | 假回滚 | M2 |
| GET `/api/usb/status` | 假挂载状态+扫描结果 | M2 |
| GET/PUT `/api/services/{id}/configfile`(+history) | 假内容/假历史 | M3 |
| GET `/api/logs` `/api/logs/stream` `/api/logs/export` | 平台自身日志**真实**（logx SSE 复用）；被管服务日志源 fake；export 返回假 zip | M3 |

**对第 6 节端点清单的增改汇总**：

1. 新增 `GET /api/events`（EventLog 流水；大盘"告警原因"与事件中心的数据源，第 6 节漏列）；
2. 新增 `GET /api/upgrade/tasks/{taskId}`（apply 是长任务：POST 受理返回 taskId + 任务轮询，不做 SSE；主理人裁决：任务式取代全局 progress 快照，与交互说明/前端方案对齐）；
3. `configfile` 拆分：PUT=保存(校验+备份)、**新增 POST `.../configfile/apply`**=按 applyMode 生效、
   **新增 POST `.../configfile/history/{bid}/restore`**=恢复指定版本（保存≠生效，现场更安全）；
4. `GET /api/logs` 统一入口：`service=platform` 即平台自身日志（logx ring），被管服务日志按
   logPolicy 的 file/stdout 接入（M3）；
5. `/api/upgrade/apply` 定义互斥语义：同一时刻仅一个升级任务，并发 409；
6. `/api/services` 列表响应为"定义+运行态"聚合视图（前端列表一次拿全，不用两跳拼装）。

注：功能范围文档 F3 的本地认证未进入需求梳理 V1 冻结基线（页面结构无登录页），
本轮不做，Setting 不设账号线；如恢复需求走增量端点。

### 2.2 数据模型定稿（对第 5 节雏形的修正）

通用约定照协作规范：时间字段 `json:"-"`、JSON Blob 用 `datatypes.JSON`、camelCase。

**Service（服务定义 = 纯期望态）**——修正点：

| 字段 | 修正 | 理由 |
|------|------|------|
| + `serviceType` | 新增 `native\|opensource` | 展示分组与默认模板；A1 的类型信息保留为纯标注 |
| + `unitName` | 新增，可空 | **adopt 模式**：纳管系统既有 systemd 单元（如包管理器装的 nats-server.service）时指定，平台只控制不生成 unit；留空 = managed 模式，平台按 `Cairn-{id}.service` 规则生成。这是"统一纳管自研+开源"的关键二态 |
| `restartPolicy.mode` | `always\|onFailure\|limited` → **`always\|onFailure`** | `limited` 不是模式而是配额：拆出独立的 `maxRetries` + `windowSec`（0=不限）。模式管"什么退出触发重启"，配额管"窗口内重启几次熔断"，两维正交 |
| `healthCheck.type` | `http\|tcp\|process` → **`none\|http\|tcp`** | 进程存活是 L1，由 runtime 保证，不需要用户配"process 探针"；`none`=仅 L1。补 `failuresThreshold`（连续失败几次判 degraded）与 `degradedRestartSec`（degraded 持续多久按策略重启）；L3 心跳字段名预留、V1 不启用 |
| `dependsOn` | 明确**存服务 id 数组** | 存 name 会被改名破坏引用；API 响应附带 name 供展示 |
| `configFile` | + `maxHistory`（默认 10） | ConfigBackup 条数配额（红线：配额轮转） |
| exePath/args/workDir | 语义按 managed/adopt 二态 | adopt 模式仅需 unitName，路径组可空 |

**ServiceRuntime（运行态读模型）**——修正点：

- `state` 枚举对齐 §3.1 状态机（9 态），不再是不明确定义的字符串；
- 明确**两级存储节奏**：状态迁移即写 + 低频快照周期写（约 30s）；高频指标（cpuPct 等）
  **移出本表**，由新增 MetricPoint 承载——避免高频落库写穿 SD 卡；
- + `pid`、`healthState`（ok\|fail\|n/a）、`lastExitCode`、`lastError`（D4 崩溃现场留存：
  退出码 + 最后 N 行日志由 worker 存内存并在事件中引用）；
- `restartTotal/restartFailures` 保留为持久累计值（平台重启后不丢台账）。

**MetricPoint（新增表）**：`serviceId(0=主机)`、`ts`、`cpuPct、cpuTimeSec、memMB、netRxKBps、
netTxKBps、netRxKB、netTxKB`。降采样起步两级：1min 粒度×24h → 5min 粒度×7d，行数配额触发
删除（Setting 可配）——第 5 节缺统计曲线的时序载体，且"主机资源概览"（大盘）用
serviceId=0 复用同一张表。

**ServiceVersion**：+ `manifest(JSON)`（包内清单原样留存：组件、sha256、签名指纹，排障与
防回滚水位判断都要用）、+ `sizeBytes`。

**UpgradeRecord**：+ `operator(user\|usb\|auto)`、+ `durationMs`；status 维持终态三值
（success\|failed\|rolledback），"进行中"由 progress 端点的内存态呈现，不落库。

**EventLog**：+ `level(info\|warn\|error)`（大盘异常原因按 level 过滤）、+ `detail(JSON)`
（结构化补充：退出码、版本号等）；配额（保留条数/天数，Setting 可配）。

**ConfigBackup**：+ `sizeBytes`；条数配额用 `configFile.maxHistory`。

**Setting**：字段清单建议（默认值回落）：`tftp{server,port,timeoutSec,retries}`、
`usb{enabled,reportName}`、`collect{intervalSec,retentionShort,retentionLong}`、
`logQuota{platformMaxMB,platformMaxBackups}`、`upgrade{keepVersions(默认2),quotaGB,
publicKey,healthVerifySec,minSafeVersion}`、`web{port}`。

---

## 3. Supervisor 状态机与 Runtime 抽象

### 3.1 状态枚举（每服务一个实例）与 LED 映射

| 状态 | 含义 | LED/前端 |
|------|------|----------|
| `stopped` | 已停止，定义仍在 | 灰 |
| `waiting` | 等待依赖就绪（就绪门控排队） | 蓝 |
| `starting` | 已发起启动，L1+L2+readyHold 验证中 | 蓝（可带进度） |
| `running` | L1 存活且 L2 通过 | 绿 |
| `degraded` | 进程活着但 L2 连续失败≥阈值（"活而不死"） | 黄 |
| `backoff` | 崩溃/启动失败后的退避等待（下次重启已排定） | 黄（显示倒计时） |
| `failed` | 熔断：窗口内失败超限或 maxRetries 用尽，需人工介入 | 红 |
| `stopping` | 优雅停机中（SIGTERM→宽限→强杀在 runtime 层） | 蓝灰 |
| `upgrading` | 升级管线接管，监管暂停 | 蓝（显示升级进度） |

### 3.2 迁移条件（全表）

| 当前态 | 事件/条件 | 目标态 | 关键动作 |
|--------|-----------|--------|----------|
| stopped | 手动 Start，或平台启动时 autoStart 且 enabled | waiting | 依赖检查入队 |
| waiting | 依赖全部 running（拓扑层就绪） | starting | runtime.Start + 就绪计时 |
| waiting | 依赖进入 failed/backoff | waiting（保持） | 告警事件"依赖不可用"，不空转重启 |
| starting | L1 active + L2 通过 + readyHoldSec 稳定 | running | 计入成功启动，事件 |
| starting | 启动超时 / L2 超时失败 | backoff | 失败计数+1，按退避排定，留存现场 |
| running | 进程退出（L1 失效） | backoff（策略允许时） | 退出码+尾部日志留存（D4），计数 |
| running | L2 连续失败 ≥ failuresThreshold | degraded | 告警事件 |
| degraded | L2 恢复 | running | 恢复事件 |
| degraded | 持续超 degradedRestartSec 且策略允许 | backoff | 发起重启（经 runtime） |
| backoff | 退避时间到（指数 backoffBase→backoffMax 封顶 60s） | starting | runtime.Start |
| backoff | 滑动窗口内失败超限 / maxRetries 用尽 | failed | 熔断告警（红），停止自动重启 |
| failed | 人工"重置并启动" | waiting | 计数清零 |
| running 等活动态 | 手动 Stop / enabled=false / 定义删除 | stopping | **先停依赖方**（逆序传播） |
| stopping | 优雅退出成功，或宽限（默认 10s）后强杀 | stopped | 停止失败在 UI 显式暴露（B3） |
| 活动态 | update 管线接管 | upgrading | LifecycleCtrl.Pause |
| upgrading | 新版本健康验证期通过 | running | commit：ServiceVersion.active 切换，清理超配额旧版 |
| upgrading | 验证期失败自动回滚 | starting | rename 回退上一版再走 starting 验证 |

平台自身崩溃恢复：状态在内存，平台重启时按**孤儿认领**对齐（fork 模型，M1 方案 §3.3——读登记
文件扫 /proc 匹配 pid/starttime/exePath，命中→running 零中断、退出感知降级轮询；未命中→stopped），
enabled+autoStart 的按拓扑序拉起；restartTotal 等累计值从 ServiceRuntime 表恢复——**平台不追认
历史、只对齐现状**。

### 3.3 决策：重启逻辑归属（ADR 简式）

- **背景**：OS 服务管理器（如 systemd 的 Restart=/StartLimit）自带重启能力，平台也有重启策略
  状态机，谁做主？
- **方案 A（选定）**：重启决策全在平台 worker——指数退避、滑动窗口、熔断、事件、UI 口径全部
  收敛一处；L2 探针失败重启、degraded 处理、依赖门控这些 OS 层本就无法表达。平台自身由 systemd
  `Restart=always` 保护，守护链完整（fork 模型下被管服务无 unit 级重启，决策天然单层）。
- **方案 B（弃）**：把策略翻译成 unit 的 Restart=/StartLimitBurst——退避非指数、探针语义
  缺失、计数分裂在两层，UI 展示口径不可收拾。
- **补充**：fork 模型下"平台失联期间的最低保底"由孤儿存活语义自然承担（服务不重启但也不死，
  平台恢复后认领）；systemd 备选实现时可再评估 Tier0 unit 级 `Restart=on-failure` 兜底。

### 3.4 Runtime 抽象接口边界

**原则：runtime 只回答"OS 服务管理器能做什么"**——进程生命周期原语；不懂健康、不懂策略、
不懂依赖、不懂版本。

> **M1 决策修订（ADR，详见 [M1 方案 §0](M1实现方案-supervisor监管引擎-fork模型-V1.md)）**：
> 执行模型从 systemd 托管（godbus 控制 unit）改为 **fork/exec 父子模型**（参考 supervisord）。
> 被管服务是平台的直接子进程：`cmd.Wait()` 事件驱动感知退出、退出码精确、stdout/stderr 管道
> 天然接管（M3 日志聚合成本大降）、Windows 开发机可真跑、零第三方依赖。唯一代价（平台崩溃时
> 父子关系断裂）以**孤儿认领**补齐：落盘 (pid, starttime, resolved exePath)，平台重启扫 /proc
> 匹配找回，零中断对齐 running。接口（8 原语）保持不变，systemd 降为备选实现。

方法集（中立命名，禁 systemd 词汇），fork 实现语义：

| 方法 | 语义 | fork 实现 |
|------|------|-----------|
| EnsureUnit(def) | 幂等登记期望态并生效 | 内存台账 + 落盘登记（data/runtime/{id}.json，供认领与开机对齐） |
| RemoveUnit(name) | 删除单元 | 停止（若活）+ 清除登记 |
| SetEnabled(name,bool) | 开机自启开关 | 登记位翻转（开机自启统一经平台按拓扑拉起） |
| Start(name,timeout) | 发起启动 | exec.Cmd：setsid+进程组（Win: CREATE_NEW_PROCESS_GROUP）+ env 合并 + stdout/stderr 管道接管；fork 即活，就绪判定归 worker |
| Stop(name,timeout) | 优雅停，含宽限与强杀语义 | 信号序列：SIGTERM（Win: taskkill /T）→ 宽限 → SIGKILL 进程组（Win: taskkill /F /T） |
| Restart(name,timeout) | 重启 | Stop + Start |
| Status(name)→UnitStatus | 中立状态 + MainPID + 退出码 | 内存态：Wait 未返回=running；登记不存在=notFound（fork 下 UnitState 仅出现 running/stopped/failed/notFound） |
| Pids(name)[]int | 主进程+子进程 | 主 PID + /proc 子孙扫描（ppid 链；Win: toolhelp32 或仅主 PID） |

中立枚举 `UnitState`：`running | starting | stopping | stopped | failed | notFound`。
平台状态机（§3.1）与 UnitState 是**两个层次**：前者是监管语义（含 degraded/backoff 等
runtime 无从得知的状态），后者是进程事实——worker 负责把进程事实折叠进监管语义。

新增**可选能力接口**（消费者按能力探测，fake 亦可提供）：

```go
type ExitWatcher interface{ Exits() <-chan UnitExit }  // 事件驱动退出感知（含退出码/时刻/是否认领进程）
```

进程生成无模板翻译（不产 unit 文件）：期望态变更即按 unitFingerprint 差量调谐决定重 fork（§1.4）。
停止序列、孤儿认领与 Windows 降级分文件清单详见 [M1 方案](M1实现方案-supervisor监管引擎-fork模型-V1.md) §3。

---

## 4. 前端 mock 与真实后端切换策略

### 4.1 方案对比

| 方案 | 描述 | 结论 |
|------|------|------|
| A 前端拦截 mock | JS 层拦截 apiReq 返回假数据 | 弃：契约双事实源，漂移必然发生；前端写两套 |
| B **后端 fake facade** | 真实后端进程 + 引擎层 fake 实现，前端零感知 | **选定**：契约冻结点在 API 层（最接近真实），切换=装配替换，前端代码永不改 |
| C 独立 mock server | json-server 之类 | 弃：额外设施 + 无 {code,data} 封装 + 形状漂移更大 |

方案 B 的支点正是协作文档既有的 facade 模式：接口定义在 api 包，`runtime/fake`、
collect fake、update fake 三个实现同时是**单测桩与 UI 原型数据源（一物两用）**。

### 4.2 fake 的要求（不是静态 stub，要"可演示"）

1. **种子数据对齐产品**：fake 启动预置首批纳管五服务（ThingsModel、Gateway-Modbus、
   AlarmEngine、nats-server、nginx），原型一打开就是真实形态的满页数据；
2. **内置行为脚本**：Start→starting 约 2s→running；Stop→stopping→stopped；随机让一个
   服务进入 degraded 并出告警事件；upgrade apply 走 10 段假进度；fake runtime 同样填充
   PID 注册表，collect fake 产出平滑随机游走曲线——大盘、曲线、事件全链路可演示；
3. **与真实实现同构**：fake 实现的状态枚举、事件类型与真实一致，替换时前端无感。

### 4.3 切换机制

- 装配选择在 main：默认**自动探测**（Linux → fork 真实引擎；Windows → fake 演示），
  `--runtime=fake|fork` 显式覆盖，`--mock` 等价于强制 fake——开发机（Windows）默认进
  演示模式（`--runtime=fork` 可显式真跑），目标板自动真实，无需改代码或配置；
- `/api/version` 返回 `{runtime:"fake", mock:true}` → 前端顶部黄条"演示数据"角标，
  杜绝把演示环境误当生产；
- **前端唯一纪律：永不内置 mock**。所有页面从第一天起就连真实后端（fake 数据）。

### 4.4 分阶段替换矩阵（fake → real，逐端点演进）

| 阶段 | 替换内容 | fake 保留 |
|------|----------|-----------|
| 本轮（骨架） | services CRUD / settings / version / records 查询 | 其余全部 fake |
| M1 | runtime/fork（直接 fork/exec + Wait 事件 + 孤儿认领）+ supervisor 状态机 + dashboard/启停/事件 | 升级域、指标曲线（stdout 日志管道 M1 已接管，聚合呈现归 M3） |
| M2 | update 管线 + pack 验签；获取通道按 **manual → tftp → usb** 递进（manual 先验签名+切换+回滚主链路，再加通道） | usb 检测、曲线 |
| M3 | 被管服务日志聚合 + svcfg 配置编辑 + collect 真实采样 | —（fake 仅存于单测） |

### 4.5 契约回归（防两端漂移）

对每个 B 类端点写**形状断言测试**：同一断言跑 fake 与真实 handler（字段名/类型/枚举值），
fake 的返回值即"活契约"；契约文档（本文 §2.1）与断言测试共同构成冻结机制，改动必须
双侧同步 + 增量式演进。

---

## 5. 架构适应度函数（红线进 CI）

```text
1. grep -r "nats" internal/ → 必须为 0（管理面不依赖 NATS）
2. grep "internal/api\|internal/web" internal/supervisor|update|collect|runtime|svcfg → 必须为 0（依赖单向）
3. api 包 import 列表不得出现 supervisor/update/collect 具体包（只允许本包 facade 接口）
4. AutoMigrate 幂等测试：旧库升级路径可迁移（glebarez/sqlite 内存库跑两遍）
5. 契约形状断言全绿（§4.5）
```

---

## 6. 落地顺序对齐（与需求梳理第 8 节映射）

1. **本轮**：Console 骨架（五件套 + 四领域包 + 三 facade + runtime/fake）+ 全部端点
   （A 类真实 + B 类 fake）+ 前端五区块 → 可点击、可演示的完整原型；
2. **M1**：runtime/fork（直接 fork/exec 父子模型 + Wait 事件驱动 + 孤儿认领，详见 [M1 方案](M1实现方案-supervisor监管引擎-fork模型-V1.md)）+ supervisor 状态机 + 就绪门控；dashboard/启停/事件转真；
3. **M2**：update 管线（统一包格式 + 验签 + 原子切换 + 双版本回滚），manual→tftp→usb；
4. **M3**：日志 SSE 聚合、configfile 编辑生效、collect 指标曲线转真。
