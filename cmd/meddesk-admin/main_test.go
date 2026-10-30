package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

// 客户端/服务端一致认可的派生参数（与 web/assets/js/crypto.js、
// internal/auth/credential.go 严格对齐）。测试里刻意独立写一份，
// 避免「被测代码说什么就用什么」导致口径漂移发现不了。
const (
	testPBKDF2Iterations = 210000
	testPBKDF2KeyLen     = 32
	testHKDFInfo         = "meddesk-kek-v1"
	testKEKLen           = 32
	testDEKLen           = 32
)

// TestResetPasswordEnvelopeUsesRawAuthHash 拦住 A-07：CLI 生成的信封必须能被
// **客户端口径**解开。
//
// 历史缺陷：CLI 自己复刻派生时把 HKDF 的 ikm 写成了 PBKDF2 结果的 base64 文本
// （44 字节），而正确口径是原始 32 字节。结果是 wrapped_dek 用错误 KEK 包裹，
// 用户在新的 reset-password 流程下能登录但永远解不出 DEK，而旧密文已被清空。
func TestResetPasswordEnvelopeUsesRawAuthHash(t *testing.T) {
	const password = "reset-me-please-2026"

	mat, err := buildResetCredentials(password, 8, 1, 1)
	if err != nil {
		t.Fatalf("生成凭据材料失败: %v", err)
	}
	if len(mat.KDFSalt) == 0 || len(mat.WrappedDEK) == 0 || len(mat.DEKIV) == 0 || mat.PasswordHash == "" {
		t.Fatal("凭据材料不完整")
	}

	// 客户端口径复刻（crypto.js deriveBytes → deriveKEK → unwrapDEK）：
	// authHash 是原始字节，**不是** base64 文本。
	authHash := pbkdf2.Key([]byte(password), mat.KDFSalt, testPBKDF2Iterations, testPBKDF2KeyLen, sha256.New)
	kek := make([]byte, testKEKLen)
	if _, err := io.ReadFull(hkdf.New(sha256.New, authHash, mat.KDFSalt, []byte(testHKDFInfo)), kek); err != nil {
		t.Fatalf("派生 KEK 失败: %v", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		t.Fatalf("初始化 AES 失败: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("初始化 GCM 失败: %v", err)
	}
	dek, err := gcm.Open(nil, mat.DEKIV, mat.WrappedDEK, nil)
	if err != nil {
		t.Fatalf("解包 DEK 失败（HKDF 的 ikm 口径不对，或 salt/iv 不匹配）: %v", err)
	}
	if len(dek) != testDEKLen {
		t.Fatalf("解出的 DEK 长度异常：期望 %d，实际 %d", testDEKLen, len(dek))
	}
}

// TestResetPasswordEnvelopeRejectsBase64IKM 反向钉子：证明用 base64 文本当 ikm
// 一定会解包失败。这条用例存在的意义是防止有人"修复"成错误的口径而测试还全绿。
func TestResetPasswordEnvelopeRejectsBase64IKM(t *testing.T) {
	const password = "reset-me-please-2026"

	mat, err := buildResetCredentials(password, 8, 1, 1)
	if err != nil {
		t.Fatalf("生成凭据材料失败: %v", err)
	}

	wrongIKM := []byte(base64.StdEncoding.EncodeToString(
		pbkdf2.Key([]byte(password), mat.KDFSalt, testPBKDF2Iterations, testPBKDF2KeyLen, sha256.New)))
	kek := make([]byte, testKEKLen)
	if _, err := io.ReadFull(hkdf.New(sha256.New, wrongIKM, mat.KDFSalt, []byte(testHKDFInfo)), kek); err != nil {
		t.Fatalf("派生 KEK 失败: %v", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		t.Fatalf("初始化 AES 失败: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("初始化 GCM 失败: %v", err)
	}
	if _, err := gcm.Open(nil, mat.DEKIV, mat.WrappedDEK, nil); err == nil {
		t.Fatal("base64 文本 ikm 不应能解开信封：说明凭据材料按错误口径生成了")
	}
}

// TestBuildResetCredentialsSaltIsFresh 每次重置必须换一套新的 salt 与 DEK，
// 否则多账号共用材料会在服务端形成可关联/可重放的凭据。
func TestBuildResetCredentialsSaltIsFresh(t *testing.T) {
	a, err := buildResetCredentials("same-password-1", 8, 1, 1)
	if err != nil {
		t.Fatalf("生成凭据材料失败: %v", err)
	}
	b, err := buildResetCredentials("same-password-1", 8, 1, 1)
	if err != nil {
		t.Fatalf("生成凭据材料失败: %v", err)
	}
	if base64.StdEncoding.EncodeToString(a.KDFSalt) == base64.StdEncoding.EncodeToString(b.KDFSalt) {
		t.Fatal("两次重置不应复用同一个 kdf_salt")
	}
	if base64.StdEncoding.EncodeToString(a.WrappedDEK) == base64.StdEncoding.EncodeToString(b.WrappedDEK) {
		t.Fatal("两次重置不应生成同一个 wrapped_dek")
	}
}

// TestBuildResetCredentialsRejectsShortPassword 短口令必须被拒，
// 绝不能把弱口令材料写进库里。
func TestBuildResetCredentialsRejectsShortPassword(t *testing.T) {
	for _, pw := range []string{"", "short", "1234567"} {
		if _, err := buildResetCredentials(pw, 8, 1, 1); err == nil {
			t.Fatalf("口令 %q（长度 %d）应被拒绝", pw, len(pw))
		}
	}
}
