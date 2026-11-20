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
// ServiceRuntime 读模型。
//
// 安装制定稿：服务只能经包（.cairnpkg）进入平台，统一 managed 模式。
// 覆盖式升级（decisions.md D5）：无多版本目录，Version 记录当前版本，
// Manifest 留存当前版本的安装清单快照（导出与排障用）。
type Service struct {
	ID          int    `gorm:"primaryKey" json:"id"`
	Name        string `gorm:"uniqueIndex;not null" json:"name"` // 同时是安装目录名
	Version     string `gorm:"not null;default:''" json:"version"` // 当前版本（语义化）
	Manifest    datatypes.JSON `json:"manifest"`                    // 当前版本安装清单快照
	InstalledAt time.Time      `json:"-"`
	Summary     string         `gorm:"not null;default:''" json:"summary"` // 卡片展示名（中文摘要）：空则回落到 Name
	ServiceType string         `json:"serviceType"` // native | opensource（纯标注）
	WebUrl      string         `json:"webUrl"`      // 独立 Web 控制台地址（空 = 名称不可穿越）
	ExePath     string         `json:"exePath"`     // services/{os}/{arch}/{name}/{entrypoint}
	Args        datatypes.JSON `json:"args"`
	WorkDir     string         `json:"workDir"` // 固定 data/{name}（数据与产物分离）
	Env         datatypes.JSON `json:"env"`
	AutoStart   bool           `json:"autoStart"`
	Priority    int            `json:"priority"`
	DependsOn   datatypes.JSON `json:"dependsOn"` // 服务 id 数组（存 id 不存 name，防改名破坏引用）
	RestartPolicy datatypes.JSON `json:"restartPolicy"`
	HealthCheck   datatypes.JSON `json:"healthCheck"`
	ConfigFile    datatypes.JSON `json:"configFile"`
	LogPolicy     datatypes.JSON `json:"logPolicy"`
	Enabled       bool           `json:"enabled"`
	CreatedAt     time.Time      `json:"-"`
	UpdatedAt     time.Time      `json:"-"`
}

// ServiceRuntime 运行态快照（内存为主：状态迁移即写，台账字段平台重启不丢；
// 主机指标自 2026-11 决策起仅内存缓存实时值，不再落库）。
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

// EventLog 事件流水（大盘“告警原因”与事件中心的数据源）。
type EventLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Type      string    `json:"type"`  // start|stop|restart|backoff|alert|usb|upgrade|publish|settings
	Level     string    `json:"level"` // info | warn | error
	ServiceID int       `json:"serviceId"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"-"`
}

// UpgradeRecord 升级/安装任务台账（终态才落库；进行中由任务端点内存态呈现，
// 架构 §2.2 定稿：status 维持终态值，「进行中」不落库）。
//
// 与 EventLog(type=upgrade) 的分工：EventLog 是面向大盘/事件中心的事件流水，
// 本表是面向 /api/upgrade/records 的结构化查询台账（operator/durationMs，
// backend-architecture §2.2 增补字段）。
//
// status 终态按 decisions.md D10 只有两值：success（换文件完成且启动指令已发出）/
// failed（任一阶段出错）。rolledback 不再产生——D1 已取消自动回滚（B6：回退走
// 本地上传旧版本包），枚举留作历史数据兼容。
// operator 当前恒为 user（Web 手动）；usb/auto 通道不存在，取值留作扩展。
type UpgradeRecord struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	TaskID      string    `gorm:"index" json:"taskId"`
	ServiceID   int       `json:"serviceId"` // 失败的安装可能尚无对应服务行 → 0
	ServiceName string    `json:"serviceName"`
	Mode        string    `json:"mode"`   // install | upgrade
	Source      string    `json:"source"` // local | ota
	FromVersion string    `json:"fromVersion"`
	ToVersion   string    `json:"toVersion"`
	Status      string    `json:"status"`   // success | failed
	Operator    string    `json:"operator"` // user | usb | auto（当前仅 user）
	DurationMs  int64     `json:"durationMs"` // 任务受理到终态的耗时
	Detail      string    `json:"detail"`     // 成功摘要 / 失败原因
	CreatedAt   time.Time `json:"-"`          // 任务受理时刻
	FinishedAt  time.Time `json:"-"`          // 终态时刻
}
