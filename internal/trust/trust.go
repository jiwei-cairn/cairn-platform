// Package trust 是内容签名和密钥编码。登录账户不走这里。
package trust

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/jiwei-cairn/cairn-platform/internal/pack"
)

// Signature 一条 ed25519 签名。
type Signature struct {
	Algo  string `json:"algo"`
	KeyID string `json:"keyId"`
	Sig   string `json:"sig"`
}

// KeyRing keyId → 公钥。
type KeyRing map[string]ed25519.PublicKey

// DecodePublicKey 解析 PKIX 公钥的 base64。也接受 32 字节原始公钥的 base64。
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	k, err := x509.ParsePKIXPublicKey(raw)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("不是 ed25519 公钥")
	}
	return pub, nil
}

func signJSON(v any, priv ed25519.PrivateKey, keyID string) (json.RawMessage, error) {
	msg, err := canonical(v)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, msg)
	raw, err := json.Marshal(Signature{Algo: "ed25519", KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(sig)})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func verifyJSON(v any, raw json.RawMessage, ring KeyRing) error {
	sigs, err := parseSignatures(raw)
	if err != nil {
		return err
	}
	if len(sigs) == 0 {
		return fmt.Errorf("缺少签名")
	}
	msgs, err := signedPayloads(v)
	if err != nil {
		return err
	}
	for _, s := range sigs {
		pub, ok := ring[s.KeyID]
		if !ok {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			if ed25519.Verify(pub, msg, blob) {
				return nil
			}
		}
	}
	return fmt.Errorf("签名验证失败")
}

// VerifyManifest 用信任列表验证服务包清单。至少一条签名命中即可。
func VerifyManifest(m *pack.Manifest, ring KeyRing) error {
	if m == nil {
		return fmt.Errorf("清单为空")
	}
	return verifyJSON(m, m.Signature, ring)
}

// signedPayloads 给出验签时要尝试的两种字节。
// 新签名剔除 signature 后按键名重排；已发布的包则保留 "signature":null，并递归重排嵌套对象。
func signedPayloads(v any) ([][]byte, error) {
	current, err := canonical(v)
	if err != nil {
		return nil, err
	}
	legacy, err := canonicalWithNullSignature(v)
	if err != nil {
		return nil, err
	}
	return [][]byte{current, legacy}, nil
}

func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "signature")
	return json.Marshal(m)
}

// canonicalWithNullSignature 复现已发布包的签名字节：嵌套对象键名排序，signature 置为 null。
func canonicalWithNullSignature(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("签名对象不是 JSON 对象")
	}
	m["signature"] = nil
	return json.Marshal(m)
}

func parseSignatures(raw json.RawMessage) ([]Signature, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var list []Signature
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("签名不是合法 JSON")
		}
		for _, s := range list {
			if err := checkSig(s); err != nil {
				return nil, err
			}
		}
		return list, nil
	}
	var one Signature
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("签名不是合法 JSON")
	}
	if err := checkSig(one); err != nil {
		return nil, err
	}
	return []Signature{one}, nil
}

func checkSig(s Signature) error {
	if s.Algo != "ed25519" || s.KeyID == "" || s.Sig == "" {
		return fmt.Errorf("签名字段不完整")
	}
	return nil
}

// ManifestSignatures 读出服务包清单上的签名。没有签名时返回空列表。结构损坏则报错。
func ManifestSignatures(m *pack.Manifest) ([]Signature, error) {
	if m == nil {
		return nil, fmt.Errorf("清单为空")
	}
	return parseSignatures(m.Signature)
}
