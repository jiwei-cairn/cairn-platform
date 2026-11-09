package logx

import (
	"context"
	"io"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// newRotator 构造滚动文件写入器：
//   - 大小轮转：单文件超过 MaxSizeMB 自动切割；
//   - 保留策略：最多 MaxBackups 份、MaxAgeDays 天，可选 gzip 压缩；
//   - 每日轮转：可选，在每天本地 00:00 触发一次切割。
func newRotator(opt Options) (io.Writer, func()) {
	lj := &lumberjack.Logger{
		Filename:   opt.File,
		MaxSize:    orDefault(opt.MaxSizeMB, 20),  // MB
		// 兜底 20 与 config.Validate 的上限（1–20）保持一致：
		// 兜底若取 30，会造出一个校验不允许、运行时却生效的取值。
		MaxBackups: orDefault(opt.MaxBackups, 20), // 份
		MaxAge:     orDefault(opt.MaxAgeDays, 30), // 天
		Compress:   opt.Compress,
		LocalTime:  true,
	}
	if opt.DailyRotate {
		return lj, startDailyRotate(lj)
	}
	return lj, nil
}

// startDailyRotate starts the daily worker and returns a function that waits for it to exit.
func startDailyRotate(lj *lumberjack.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		dailyRotate(ctx, lj)
	}()
	return func() {
		cancel()
		<-done
	}
}

// dailyRotate 在每天本地 00:00 调用一次 Rotate() 实现按天切割。
// 与 lumberjack 的大小轮转互不冲突：任一条件满足即切出新文件。
func dailyRotate(ctx context.Context, lj *lumberjack.Logger) {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		_ = lj.Rotate()
	}
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
