package logx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// gormLogger 把 GORM 的日志接口转发到 logx（mod=gorm 分支）。
type gormLogger struct {
	log           *slog.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

// NewGormLogger 返回接入 logx 的 GORM 日志器。level 采用 GORM 的级别语义
// （Silent/Error/Warn/Info）；默认 Warn，仅在出错或慢查询时输出。
func NewGormLogger(level gormlogger.LogLevel) gormlogger.Interface {
	return &gormLogger{log: Module("gorm"), level: level, slowThreshold: 200 * time.Millisecond}
}

func (l *gormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	n := *l
	n.level = level
	return &n
}

func (l *gormLogger) Info(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Info {
		l.log.InfoContext(ctx, fmt.Sprintf(msg, data...))
	}
}

func (l *gormLogger) Warn(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Warn {
		l.log.WarnContext(ctx, fmt.Sprintf(msg, data...))
	}
}

func (l *gormLogger) Error(ctx context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Error {
		l.log.ErrorContext(ctx, fmt.Sprintf(msg, data...))
	}
}

func (l *gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)
	sql, rows := fc()
	switch {
	case err != nil && l.level >= gormlogger.Error && !errors.Is(err, gorm.ErrRecordNotFound):
		l.log.ErrorContext(ctx, "SQL 执行出错", "err", err, "elapsed", elapsed.String(), "rows", rows, "sql", sql)
	case elapsed > l.slowThreshold && l.level >= gormlogger.Warn:
		l.log.WarnContext(ctx, "SQL 慢查询", "threshold", l.slowThreshold.String(), "elapsed", elapsed.String(), "rows", rows, "sql", sql)
	case l.level >= gormlogger.Info:
		l.log.InfoContext(ctx, "SQL", "elapsed", elapsed.String(), "rows", rows, "sql", sql)
	}
}
