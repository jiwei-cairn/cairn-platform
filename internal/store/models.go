// Package store 集中定义全部 GORM 模型（架构设计 §2.2 数据模型定稿）。
//
// 通用约定（协作规范）：时间字段 `json:"-"`（API 层按需以毫秒时间戳输出）、
// 灵活 schema 字段用 datatypes.JSON、全链路 camelCase。
package store

import (
	"time"

	"gorm.io/datatypes"
)

// Service 服务定义（纯期望态）。监管引擎据此差量调谐，运行结果落在
// ServiceRuntime / MetricPoint 读模型。
//
// 安装制定稿：服务只能经 /api/services/install（manifest 说明文档）进入
// 平台，统一 managed 模式（平台全权定义与守护）；adopt/unitName 已退役。
type Service struct {
	ID            int            `gorm:"primaryKey" json:"id"`
	// Name 唯一索引：服务名同时是安装目录名与依赖引用键。仅靠预检的
	// check-then-act 在并发双击安装/保存时会双双通过，产出重复行。
	Name          string         `gorm:"uniqueIndex;not null" json:"name"`
	Summary       string         `gorm:"not null;default:''" json:"summary"` // 卡片展示名（中文摘要）：空则回落到 Name
	ServiceType   string         `json:"serviceType"` // native | opensource（纯标注）
	WebUrl        string         `json:"webUrl"`      // 独立 Web 控制台地址（空 = 名称不可穿越）
	ExePath       string         `json:"exePath"`     // 安装产物路径（services/{os}/{arch}/{name}/{version}/…，运行目录相对）
	Args          datatypes.JSON `json:"args"`
	WorkDir       string         `json:"workDir"`
	Env           datatypes.JSON `json:"env"`
	AutoStart     bool           `json:"autoStart"`
	Priority      int            `json:"priority"`
	DependsOn     datatypes.JSON `json:"dependsOn"` // 服务 id 数组（存 id 不存 name，防改名破坏引用）
	RestartPolicy datatypes.JSON `json:"restartPolicy"`
	HealthCheck   datatypes.JSON `json:"healthCheck"`
	ConfigFile    datatypes.JSON `json:"configFile"`
	LogPolicy     datatypes.JSON `json:"logPolicy"`
	Enabled       bool           `json:"enabled"`
	CreatedAt     time.Time      `json:"-"`
	UpdatedAt     time.Time      `json:"-"`
}

// ServiceRuntime 运行态快照（内存为主，两级存储节奏：状态迁移即写 +
// 低频快照周期写；高频指标移出本表由 MetricPoint 承载，避免写穿 SD 卡）。
type ServiceRuntime struct {
	ID                  uint      `gorm:"primaryKey" json:"-"`
	ServiceID           int       `gorm:"uniqueIndex;not null" json:"serviceId"`
	State               string    `json:"state"` // 对齐架构 §3.1 状态机九态
	Pid                 int       `json:"pid"`
	StartedAt           time.Time `json:"startedAt"`
	RestartTotal        int       `json:"restartTotal"` // 持久累计（平台重启不丢台账）
	RestartFailures     int       `json:"restartFailures"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	HealthState         string    `json:"healthState"` // ok | fail | n/a
	LastExitCode        int       `json:"lastExitCode"`
	LastError           string    `json:"lastError"` // D4 崩溃现场留存
	UpdatedAt           time.Time `json:"-"`
}

// MetricPoint 指标时序点：serviceId=0 表示主机自身（大盘主机概览复用本表）。
// 主机行当前只保留 2h 原始点（覆盖大盘「近 1 小时趋势」），由 api 层定时
// 采样与过期清理；更长的降采样层级（1min×24h → 5min×7d）留待需要长窗口
// 报表时再启用。
//
// 当前实际写入：主机行（serviceId=0）只用 CpuPct/MemPct/DiskPct 三条使用率
// 与 NetRxKBps/NetTxKBps 速率。服务行落库（M3 ServiceStats）尚未实现，其
// CpuTimeSec/MemMB/NetRxKB/NetTxKB 四列目前无人写入也无人读取——保留列以
// 免后续 AutoMigrate 改表，但不要误以为它们在生效。
type MetricPoint struct {
	ID         uint    `gorm:"primaryKey" json:"-"`
	ServiceID  int     `gorm:"index:idx_metric_svc_ts;not null" json:"serviceId"`
	Ts         int64   `gorm:"index:idx_metric_svc_ts" json:"ts"` // 毫秒时间戳
	CpuPct     float64 `json:"cpuPct"`
	CpuTimeSec float64 `json:"cpuTimeSec"` // M3 服务行用，当前未写入
	MemMB      float64 `json:"memMB"`      // M3 服务行用，当前未写入
	MemPct     float64 `json:"memPct"`     // 内存使用率（主机行专用）
	DiskPct    float64 `json:"diskPct"`    // 磁盘使用率（主机行专用）
	NetRxKBps  float64 `json:"netRxKBps"`
	NetTxKBps  float64 `json:"netTxKBps"`
	NetRxKB    float64 `json:"netRxKB"` // M3 服务行用，当前未写入
	NetTxKB    float64 `json:"netTxKB"` // M3 服务行用，当前未写入
}

// ServiceVersion 本地保留版本（回滚水位与防回滚判断依据）。
//
// 当前安装只填 Version/DirPath/SizeBytes/Manifest/Active；Checksum 未计算、
// 无人读取（M2 签名校验落地后启用）。DirPath 写而不读——安装目录总由
// installRoot() 重新推导。SizeBytes 存的是 manifest.json 字节数而非产物
// 大小，语义待 M2 修正（届时记录实际产物占用）。
type ServiceVersion struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	ServiceID   int            `gorm:"index;not null" json:"serviceId"`
	Version     string         `json:"version"`
	DirPath     string         `json:"dirPath"`
	Checksum    string         `json:"checksum"` // M2 验签用，当前未填充
	SizeBytes   int64          `json:"sizeBytes"`
	Manifest    datatypes.JSON `json:"manifest"` // 包内清单原样留存（组件、sha256、签名指纹、来源）
	InstalledAt time.Time      `json:"-"`
	Active      bool           `json:"active"`
}

// EventLog 事件流水（大盘“告警原因”与事件中心的数据源）。
type EventLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Type      string    `json:"type"`  // start|stop|restart|backoff|alert|usb|upgrade|settings
	Level     string    `json:"level"` // info | warn | error
	ServiceID int       `json:"serviceId"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"-"`
}
