package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

// 与 web/assets/js/crypto.js 严格一致的客户端派生参数。
//
// 客户端：PBKDF2-SHA256(口令, salt, 210000) → 32 字节 authHash；
// KEK = HKDF-SHA256(ikm=authHash, salt, info="meddesk-kek-v1") → AES-GCM-256；
// wrappedDEK = AES-GCM(KEK, iv12, DEK32)。
//
// 服务端在「管理员代建用户 / 重置口令」时需要用同一套参数生成可用的信封，
// 否则用户虽能通过登录校验，却永远解不开数据密钥。
const (
	credentialPBKDF2Iterations = 210000
	credentialPBKDF2KeyLen     = 32
	credentialHKDFInfo         = "meddesk-kek-v1"
	credentialDEKLen           = 32
	credentialIVLen            = 12
	credentialSaltLen          = 16
)

// CredentialMaterial 是一组可直接落库的账号凭据材料。
type CredentialMaterial struct {
	PasswordHash string // argon2id(客户端 PBKDF2 派生值)，与注册路径完全一致
	KDFSalt      []byte // 明文 salt（服务端不解密任何数据，salt 本就不敏感）
	WrappedDEK   []byte // 被 KEK 包裹的 DEK
	DEKIV        []byte // 包裹 DEK 使用的 IV
}

// GenerateCredentials 按客户端口径生成一套全新的账号凭据材料。
//
// 用于管理员在后台创建用户或重置口令：服务端随机生成 DEK，用新口令派生的
// KEK 包裹后落库，用户随后用该口令登录即可正常解包。
func GenerateCredentials(password string, argonMemory, argonTime uint32, argonThreads uint8) (CredentialMaterial, error) {
	var out CredentialMaterial
	if len(password) < 8 {
		return out, errors.New("口令至少 8 位")
	}

	salt := make([]byte, credentialSaltLen)
	dek := make([]byte, credentialDEKLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return out, fmt.Errorf("生成 salt 失败: %w", err)
	}
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return out, fmt.Errorf("生成 DEK 失败: %w", err)
	}

	return credentialsFromDEK(dek, password, salt, argonMemory, argonTime, argonThreads)
}

// GenerateCredentialsWithDEK 用**已有的 DEK** 生成一套新凭据（新 salt + 新口令）。
//
// 这是管理员「直接改口令且不动数据」的关键：DEK 不变，只换包裹它的 KEK，
// 因此服务器上所有密文对新口令依然可解。DEK 由调用方从托管信封里解出。
func GenerateCredentialsWithDEK(dek []byte, password string, argonMemory, argonTime uint32, argonThreads uint8) (CredentialMaterial, error) {
	var out CredentialMaterial
	if len(password) < 8 {
		return out, errors.New("口令至少 8 位")
	}
	if len(dek) != credentialDEKLen {
		return out, fmt.Errorf("DEK 长度异常：%d", len(dek))
	}
	salt := make([]byte, credentialSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return out, fmt.Errorf("生成 salt 失败: %w", err)
	}
	return credentialsFromDEK(dek, password, salt, argonMemory, argonTime, argonThreads)
}

// GeneratePasswordOnly 生成「新 salt + 新口令的 argon2id 哈希」，但**不触碰信封**。
//
// 管理员「直接改口令、保留数据」的默认路径用它：password_hash/kdf_salt 换成新的，
// wrapped_dek 原样留着（旧口令包裹），等持有 DEK 的设备登录后自愈。
func GeneratePasswordOnly(password string, argonMemory, argonTime uint32, argonThreads uint8) (string, []byte, error) {
	if len(password) < 8 {
		return "", nil, errors.New("口令至少 8 位")
	}
	salt := make([]byte, credentialSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", nil, fmt.Errorf("生成 salt 失败: %w", err)
	}
	authHash := pbkdf2.Key([]byte(password), salt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New)
	hash, err := HashClientHash(base64.StdEncoding.EncodeToString(authHash), argonMemory, argonTime, argonThreads)
	if err != nil {
		return "", nil, fmt.Errorf("口令二次哈希失败: %w", err)
	}
	return hash, salt, nil
}

// credentialsFromDEK 是两条生成路径的公共部分：派生 authHash → argon2id → 包裹 DEK。
func credentialsFromDEK(dek []byte, password string, salt []byte, argonMemory, argonTime uint32, argonThreads uint8) (CredentialMaterial, error) {
	var out CredentialMaterial

	authHash := pbkdf2.Key([]byte(password), salt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New)
	passwordHash, err := HashClientHash(base64.StdEncoding.EncodeToString(authHash), argonMemory, argonTime, argonThreads)
	if err != nil {
		return out, fmt.Errorf("口令二次哈希失败: %w", err)
	}

	wrapped, iv, err := wrapDEK(dek, authHash, salt)
	if err != nil {
		return out, err
	}

	out.PasswordHash = passwordHash
	out.KDFSalt = salt
	out.WrappedDEK = wrapped
	out.DEKIV = iv
	return out, nil
}

// wrapDEK 复刻客户端 wrapDEK：HKDF 派生 KEK，AES-GCM 包裹 DEK。
func wrapDEK(dek, authHash, salt []byte) (wrapped, iv []byte, err error) {
	reader := hkdf.New(sha256.New, authHash, salt, []byte(credentialHKDFInfo))
	kek := make([]byte, 32)
	if _, err = io.ReadFull(reader, kek); err != nil {
		return nil, nil, fmt.Errorf("派生 KEK 失败: %w", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	iv = make([]byte, credentialIVLen)
	if _, err = io.ReadFull(rand.Reader, iv); err != nil {
		return nil, nil, fmt.Errorf("生成 IV 失败: %w", err)
	}
	return gcm.Seal(nil, iv, dek, nil), iv, nil
}
