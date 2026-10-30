package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
)

// 托管恢复（recovery escrow）。
//
// 【要解决的问题】
// DEK 只被「口令派生的 KEK」包裹。管理员改口令时服务端没有明文 DEK，无法为新口令
// 重造信封，只能换一把新 DEK —— 旧密文立刻全部作废（这就是"改密码会清数据"）。
//
// 【做法】
// 账号自愿开启托管后，客户端额外把 DEK 用**服务端恢复公钥**（RSA-OAEP-SHA256）
// 包一份上传。管理员改口令时，服务端用恢复私钥解出 DEK，再用新口令派生的 KEK
// 重新包裹 —— 数据一条不动，任何设备都能立刻用新口令登录。
//
// 【安全代价（必须清楚）】
// 私钥存在服务端配置里，意味着持有该私钥的一方可解密已开启托管账号的数据。
// 因此这是**账号可选**能力，默认关闭；不开托管的账号仍走「设备自愈」路径，
// 服务端永远拿不到它们的 DEK。
const (
	recoveryKeyBits = 2048
	// RSA-OAEP 的密文长度恒等于模长（2048 bit = 256 字节）。解密前先按长度粗筛，
	// 避免把明显不合法的密文送进 RSA 运算。
	recoveryCipherLen = recoveryKeyBits / 8
)

// RecoveryKey 是服务端持有的恢复密钥对。
type RecoveryKey struct {
	priv *rsa.PrivateKey
}

// ParseRecoveryKey 解析 base64(PKCS#8 DER) 形式的私钥。
//
// 空字符串表示未启用托管：调用方应把它当作"该部署不支持托管恢复"处理，
// 而不是报错 —— 未配置时后台改口令仍可走设备自愈路径。
func ParseRecoveryKey(encoded string) (*RecoveryKey, error) {
	if encoded == "" {
		return nil, nil
	}
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("恢复密钥不是合法的 base64: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("恢复密钥解析失败（需要 PKCS#8 DER）: %w", err)
	}
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("恢复密钥必须是 RSA 私钥")
	}
	if priv.N.BitLen() < recoveryKeyBits {
		return nil, fmt.Errorf("恢复密钥长度不足：%d 位（至少 %d）", priv.N.BitLen(), recoveryKeyBits)
	}
	return &RecoveryKey{priv: priv}, nil
}

// PublicKeyB64 返回 base64(SPKI DER)，供前端 crypto.subtle.importKey('spki', ...) 使用。
func (k *RecoveryKey) PublicKeyB64() (string, error) {
	if k == nil || k.priv == nil {
		return "", errors.New("恢复密钥未配置")
	}
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		return "", fmt.Errorf("导出公钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// Unwrap 用恢复私钥解出 DEK。
func (k *RecoveryKey) Unwrap(ciphertext []byte) ([]byte, error) {
	if k == nil || k.priv == nil {
		return nil, errors.New("恢复密钥未配置")
	}
	if len(ciphertext) != recoveryCipherLen {
		return nil, fmt.Errorf("恢复信封长度异常：%d（期望 %d）", len(ciphertext), recoveryCipherLen)
	}
	dek, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, k.priv, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("恢复信封解密失败: %w", err)
	}
	if len(dek) != credentialDEKLen {
		return nil, fmt.Errorf("恢复出的 DEK 长度异常：%d", len(dek))
	}
	return dek, nil
}

// GenerateRecoveryKey 生成一把新的恢复私钥（供 CLI 打印配置用）。
func GenerateRecoveryKey() (*RecoveryKey, string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, recoveryKeyBits)
	if err != nil {
		return nil, "", fmt.Errorf("生成恢复密钥失败: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("编码恢复密钥失败: %w", err)
	}
	return &RecoveryKey{priv: priv}, base64.StdEncoding.EncodeToString(der), nil
}
