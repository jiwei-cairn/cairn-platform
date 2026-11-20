// trustlist.go — 内容签名信任列表（F4 验签的设备侧入口）。
//
// §12 硬约束：公钥硬编码进二进制（配置文件可被篡改），只增不删；
// 密钥轮换 = 新增 keyId 条目 + 双签过渡，旧条目保留（老设备仍需验旧包）。
//
// 启用流程：
//  1. 服务端首次启动自动生成内容签名密钥（-data 目录），
//     `cairn-hub-admin -data <dir> init` 打印公钥与 keyId；
//  2. 将其加入下方列表并重新编译平台；
//  3. 验签策略（decisions.md §12）：包带签名 → 必须命中本列表并通过验证；
//     无签名 → 放行（过渡态，向客户现场规模交付前应翻转为强制）。
//
// 列表为空时：无签名包照常安装；任何带签名的包都会被拒绝
// （宁可拒绝不可盲信——不知道该信谁就不信）。
package api

import (
	"fmt"

	"github.com/jiwei-cairn/cairn-pkg/pack"
	"github.com/jiwei-cairn/cairn-pkg/trust"
)

// trustedContentKeys 内容公钥信任列表：keyId → base64(ed25519 公钥)。
var trustedContentKeys = map[string]string{
	// 示例（替换为 cairn-hub-admin init 打印的实际值）：
	// "content-1a2b3c4d": "MCowBQYDK2VwAyEAGx9pZ8p...",
}

// contentKeyRing 解码信任列表为验签用 KeyRing。
func contentKeyRing() (trust.KeyRing, error) {
	ring := make(trust.KeyRing, len(trustedContentKeys))
	for id, pubB64 := range trustedContentKeys {
		pub, err := trust.DecodePublicKey(pubB64)
		if err != nil {
			return nil, fmt.Errorf("信任列表条目 %s 公钥无效: %w", id, err)
		}
		ring[id] = pub
	}
	return ring, nil
}

// verifyContentSignature 部署管线的内容验签入口（F4）：
// 结构无效 → 拒绝；无签名 → 过渡态放行；带签名 → 信任列表验证。
func verifyContentSignature(m *pack.Manifest) error {
	sigs, err := trust.ManifestSignatures(m)
	if err != nil {
		return fmt.Errorf("包签名结构无效: %w", err)
	}
	if len(sigs) == 0 {
		return nil
	}
	ring, err := contentKeyRing()
	if err != nil {
		return err
	}
	return trust.VerifyManifest(m, ring)
}
