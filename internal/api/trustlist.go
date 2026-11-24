// trustlist.go — 内容签名信任列表（F4 验签的设备侧入口）。
//
// 信任来源两档：
//  1. 编译内置列表（§12 硬约束：公钥硬编码进二进制，只增不删；
//     密钥轮换 = 新增 keyId 条目 + 双签过渡）——生产形态；
//  2. TOFU（Trust On First Use）：列表为空时从已配置的 OTA 服务器
//     /healthz 动态获取内容公钥（仅内存，重启失效，不落盘）——
//     验证期/开发形态，免「每台 hub 都要重编译平台」的循环依赖。
//
// TOFU 的安全边界（有意为之，须知情）：
//   - 公钥取自「当前配置的 OTA 服务器」的 healthz——信任锚是服务器本身
//     （与下载包的传输通道同源）。它解决的是「无签名 vs 有签名」的空窗，
//     不解决「服务器被入侵」——那仍需编译内置 + 轮换（§12 全量方案）。
//   - 每次平台进程内只获取一次（幂等缓存）；获取失败按空列表处理
//     （带签名包将被拒绝，宁可拒绝不可盲信）。
//
// 启用编译内置的流程：
//  1. 服务端首次启动自动生成内容签名密钥（-data 目录），
//     `cairn-hub-admin -data <dir> init` 打印公钥与 keyId；
//  2. 将其加入下方 trustedContentKeys 列表并重新编译平台。
//
// 验签策略（decisions.md §12）：包带签名 → 必须命中信任列表（内置+TOFU）
// 并通过验证；无签名 → 放行（过渡态，向客户现场规模交付前应翻转为强制）。
package api

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-pkg/pack"
	"github.com/jiwei-cairn/cairn-pkg/trust"

	"gorm.io/gorm"
)

// trustedContentKeys 编译内置内容公钥信任列表：keyId → base64(ed25519 公钥)。
var trustedContentKeys = map[string]string{
	// 示例（替换为 cairn-hub-admin init 打印的实际值）：
	// "content-1a2b3c4d": "MCowBQYDK2VwAyEAGx9pZ8p...",
}

// tofuContentKey 进程内 TOFU 缓存（列表为空时从 OTA 服务器获取一次）。
type tofuKey struct {
	mu      sync.Mutex
	fetched bool
	keyID   string
	pubB64  string
}

var tofu = tofuKey{}

// contentKeyRing 解码信任列表（内置 + TOFU 缓存）为验签用 KeyRing。
//
// db 仅用于读取 OTA 服务器地址（Settings）；可以为 nil（测试形态：
// 只有内置列表生效）。
func contentKeyRing(db *gorm.DB) (trust.KeyRing, error) {
	ring := make(trust.KeyRing, len(trustedContentKeys))
	for id, pubB64 := range trustedContentKeys {
		pub, err := trust.DecodePublicKey(pubB64)
		if err != nil {
			return nil, fmt.Errorf("信任列表条目 %s 公钥无效: %w", id, err)
		}
		ring[id] = pub
	}
	// 内置列表已非空：TOFU 不参与（生产形态以编译内置为准）。
	if len(ring) > 0 {
		return ring, nil
	}
	// TOFU：从已配置的 OTA 服务器获取内容公钥（进程内一次）。
	if id, pubB64, ok := fetchTOFUKey(db); ok {
		if pub, err := trust.DecodePublicKey(pubB64); err == nil {
			ring[id] = pub
		}
	}
	return ring, nil
}

// fetchTOFUKey 获取（并缓存）OTA 服务器的内容公钥。
// db 为 nil 或未配置服务器 → false；失败 → false 且标记已尝试（不重试轰炸）。
func fetchTOFUKey(db *gorm.DB) (keyID, pubB64 string, ok bool) {
	tofu.mu.Lock()
	defer tofu.mu.Unlock()
	if tofu.fetched {
		if tofu.keyID != "" {
			return tofu.keyID, tofu.pubB64, true
		}
		return "", "", false
	}
	tofu.fetched = true // 无论成败只试一次（进程生命周期内）
	if db == nil {
		return "", "", false
	}
	var rec struct{ ConfigJSON []byte }
	if err := db.Table("settings").Select("config_json").First(&rec).Error; err != nil {
		return "", "", false
	}
	// 解析 ota.serverUrl（最小结构，避免 import store 依赖环）。
	var cfg struct {
		OTA struct {
			ServerURL string `json:"serverUrl"`
		} `json:"ota"`
	}
	if err := jsonUnmarshalLenient(rec.ConfigJSON, &cfg); err != nil || cfg.OTA.ServerURL == "" {
		return "", "", false
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cleanOtaBase(cfg.OTA.ServerURL) + "/healthz")
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	var health struct {
		Server           string `json:"server"`
		ContentKeyID     string `json:"contentKeyId"`
		ContentPublicKey string `json:"contentPublicKey"`
	}
	if err := jsonUnmarshalLenient(readBodyLimited(resp.Body), &health); err != nil {
		return "", "", false
	}
	if health.Server != "cairn-hub" || health.ContentKeyID == "" || health.ContentPublicKey == "" {
		return "", "", false
	}
	tofu.keyID, tofu.pubB64 = health.ContentKeyID, health.ContentPublicKey
	return tofu.keyID, tofu.pubB64, true
}

// verifyContentSignature 部署管线的内容验签入口（F4）：
// 结构无效 → 拒绝；无签名 → 过渡态放行；带签名 → 信任列表（内置+TOFU）验证。
//
// db 传入用于 TOFU 读取服务器地址（deploy 管线持有 s.DB）。
func verifyContentSignatureDB(db *gorm.DB, m *pack.Manifest) error {
	sigs, err := trust.ManifestSignatures(m)
	if err != nil {
		return fmt.Errorf("包签名结构无效: %w", err)
	}
	if len(sigs) == 0 {
		return nil
	}
	ring, err := contentKeyRing(db)
	if err != nil {
		return err
	}
	return trust.VerifyManifest(m, ring)
}

// readBodyLimited 读取响应体（上限 64KB，防异常响应打满内存）。
func readBodyLimited(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	return b
}
