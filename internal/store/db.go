package store

import (
	"os"
	"path/filepath"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Open 打开（或创建）SQLite 配置库并自动迁移全部表（幂等，旧库升级可重跑）。
//
// PRAGMA 说明：管理面天然并发（API 保存配置 vs 演示引擎状态落库），
// 默认 rollback journal + 无 busy_timeout 时一次瞬时「database is locked」
// 就足以让读侧拿到空数据。WAL 让读写互不阻塞，busy_timeout 提供重试窗口。
func Open(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logx.NewGormLogger(gormlogger.Warn),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(
		&Service{}, &ServiceRuntime{}, &MetricPoint{},
		&EventLog{}, &Setting{},
	); err != nil {
		return nil, err
	}
	return db, nil
}

// Close 关闭底层数据库连接（进程退出或测试清理时调用）。
func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
