package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/config"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Setting 是平台配置的单行持久化表，ConfigJSON 保存完整的 config.App 快照。
type Setting struct {
	ID         int       `gorm:"primaryKey" json:"-"`
	ConfigJSON []byte    `gorm:"not null" json:"-"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

const settingsRowID = 1

// SettingsStore 以数据库为唯一配置源的平台设置存储。
// 启动时 OpenSettings 创建表结构并加载已有配置；不存在时写入默认配置。
type SettingsStore struct {
	db  *gorm.DB
	mu  sync.RWMutex
	app config.App
}

// OpenSettings 加载或初始化平台配置。首次启动（表中无记录）时持久化默认配置；
// 已有记录缺失的字段回落到默认值。
func OpenSettings(db *gorm.DB) (*SettingsStore, error) {
	var record Setting
	// 首次启动查不到记录属预期路径，丢弃 ORM 日志避免误报 record not found 错误
	err := db.Session(&gorm.Session{Logger: logger.Discard}).First(&record, "id = ?", settingsRowID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		app := config.Default()
		raw, err := json.Marshal(app)
		if err != nil {
			return nil, fmt.Errorf("序列化默认配置失败: %w", err)
		}
		record = Setting{ID: settingsRowID, ConfigJSON: raw, UpdatedAt: time.Now().UTC()}
		if err := db.Create(&record).Error; err != nil {
			return nil, fmt.Errorf("写入默认配置失败: %w", err)
		}
		return &SettingsStore{db: db, app: app}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取平台配置失败: %w", err)
	}
	app, err := decodeSettings(record.ConfigJSON)
	if err != nil {
		return nil, fmt.Errorf("解析平台配置失败: %w", err)
	}
	return &SettingsStore{db: db, app: app}, nil
}

// Get 返回当前配置（App 为纯值类型，直接拷贝返回）。
func (s *SettingsStore) Get() config.App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.app
}

// Save 校验并持久化配置到数据库，同时更新内存副本。
func (s *SettingsStore) Save(app config.App) error {
	if issues := config.Validate(app); len(issues) > 0 {
		return fmt.Errorf("配置校验失败: %s", issues[0].Message)
	}
	raw, err := json.Marshal(app)
	if err != nil {
		return fmt.Errorf("序列化平台配置失败: %w", err)
	}
	record := Setting{ID: settingsRowID, ConfigJSON: raw, UpdatedAt: time.Now().UTC()}
	if err := s.db.Save(&record).Error; err != nil {
		return fmt.Errorf("写入平台配置失败: %w", err)
	}
	s.mu.Lock()
	s.app = app
	s.mu.Unlock()
	return nil
}

// decodeSettings 在默认值基础上反序列化，缺失字段保持默认。
func decodeSettings(raw []byte) (config.App, error) {
	app := config.Default()
	if err := json.Unmarshal(raw, &app); err != nil {
		return config.App{}, err
	}
	return app, nil
}
