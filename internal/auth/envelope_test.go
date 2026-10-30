package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

// unlockForTest 复刻客户端的解包逻辑：PBKDF2 → HKDF → AES-GCM 解 wrappedDEK。
func unlockForTest(t *testing.T, wrapped, iv []byte, password string, salt []byte) []byte {
	t.Helper()
	authHash := pbkdf2.Key([]byte(password), salt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New)
	kek := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, authHash, salt, []byte(credentialHKDFInfo)), kek); err != nil {
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
	plain, err := gcm.Open(nil, iv, wrapped, nil)
	if err != nil {
		t.Fatalf("解包失败（信封与口令不匹配）: %v", err)
	}
	return plain
}

// TestGenerateCredentialsWithDEKKeepsData 验证「沿用既有 DEK 重造凭据」不会换钥。
//
// 这是后台「直接改口令、数据保留」的正确性根基：只要 DEK 不变，
// 服务器上所有旧密文对新口令依然可解。
func TestGenerateCredentialsWithDEKKeepsData(t *testing.T) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatalf("生成 DEK 失败: %v", err)
	}

	mat, err := GenerateCredentialsWithDEK(dek, "new-password-1", 8, 1, 1)
	if err != nil {
		t.Fatalf("生成凭据失败: %v", err)
	}
	got := unlockForTest(t, mat.WrappedDEK, mat.DEKIV, "new-password-1", mat.KDFSalt)
	if !bytes.Equal(got, dek) {
		t.Fatalf("解出的 DEK 与原始不一致：数据会丢失")
	}
	if ok, _ := VerifyClientHash(mat.PasswordHash, base64.StdEncoding.EncodeToString(
		pbkdf2.Key([]byte("new-password-1"), mat.KDFSalt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New))); !ok {
		t.Fatal("新口令应能通过 argon2id 校验")
	}
}

// TestEnvelopeStaleAndRepair 覆盖「管理员改口令 → 设备自愈」这条无损路径。
func TestEnvelopeStaleAndRepair(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	reg, err := svc.Register(ctx, registerReq(), RequestMeta{})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	before, err := svc.users.GetByID(ctx, reg.UserID)
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}

	// 模拟后台「直接改口令（保留数据）」：只换口令与盐，信封原样保留。
	hash, salt, err := GeneratePasswordOnly("brand-new-pwd", 8, 1, 1)
	if err != nil {
		t.Fatalf("生成口令失败: %v", err)
	}
	if err := svc.users.UpdatePasswordKeepEnvelope(ctx, reg.UserID, hash, salt); err != nil {
		t.Fatalf("更新口令失败: %v", err)
	}

	after, err := svc.users.GetByID(ctx, reg.UserID)
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if !after.EnvelopeStale {
		t.Fatal("应标记为「待修复」")
	}
	if !bytes.Equal(after.WrappedDEK, before.WrappedDEK) {
		t.Fatal("保留数据模式下信封必须原样保留")
	}

	// 设备用本地 DEK 重新包裹（长度：32 明文 + 16 tag = 48）。
	wrapped := make([]byte, credentialDEKLen+16)
	iv := make([]byte, credentialIVLen)
	if _, err := rand.Read(wrapped); err != nil {
		t.Fatalf("构造信封失败: %v", err)
	}
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("构造 IV 失败: %v", err)
	}
	if err := svc.RepairEnvelope(ctx, reg.UserID, base64.StdEncoding.EncodeToString(wrapped), base64.StdEncoding.EncodeToString(iv)); err != nil {
		t.Fatalf("修复信封失败: %v", err)
	}
	fixed, err := svc.users.GetByID(ctx, reg.UserID)
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if fixed.EnvelopeStale {
		t.Fatal("修复后应清除待修复标记")
	}
	if !bytes.Equal(fixed.WrappedDEK, wrapped) {
		t.Fatal("修复后信封应替换为新值")
	}

	// 再修一次应被拒绝（没有待修复状态）。
	err = svc.RepairEnvelope(ctx, reg.UserID, base64.StdEncoding.EncodeToString(wrapped), base64.StdEncoding.EncodeToString(iv))
	if err == nil {
		t.Fatal("无待修复状态时应报错")
	}
	if e, ok := err.(*apierr.Error); !ok || e.Code != apierr.CodeConflict {
		t.Fatalf("期望 CodeConflict，实际 %v", err)
	}

	// 长度不合法的信封必须被拒（防脏数据写坏信封）。
	if err := svc.RepairEnvelope(ctx, reg.UserID, base64.StdEncoding.EncodeToString([]byte("short")), base64.StdEncoding.EncodeToString(iv)); err == nil {
		t.Fatal("长度异常的信封应被拒绝")
	}
}

// TestRecoveryEnvelopeRoundTrip 覆盖托管恢复：客户端用公钥包裹 → 服务端用私钥解出。
func TestRecoveryEnvelopeRoundTrip(t *testing.T) {
	key, keyB64, err := GenerateRecoveryKey()
	if err != nil {
		t.Fatalf("生成恢复密钥失败: %v", err)
	}
	parsed, err := ParseRecoveryKey(keyB64)
	if err != nil {
		t.Fatalf("解析恢复密钥失败: %v", err)
	}
	pubB64, err := parsed.PublicKeyB64()
	if err != nil {
		t.Fatalf("导出公钥失败: %v", err)
	}
	if pubB64 == "" {
		t.Fatal("公钥不能为空")
	}

	// 客户端侧：用公钥包裹 DEK。
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatalf("生成 DEK 失败: %v", err)
	}
	// 直接在服务端解出公钥做一次 RSA-OAEP 加密，模拟浏览器 WebCrypto。
	der, _ := base64.StdEncoding.DecodeString(pubB64)
	pubAny, err := parseSPKI(der)
	if err != nil {
		t.Fatalf("解析公钥失败: %v", err)
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pubAny, dek, nil)
	if err != nil {
		t.Fatalf("包裹失败: %v", err)
	}

	got, err := key.Unwrap(ct)
	if err != nil {
		t.Fatalf("解出托管信封失败: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("托管信封解出的 DEK 不一致")
	}

	// 长度异常必须被拒。
	if _, err := key.Unwrap(make([]byte, 16)); err == nil {
		t.Fatal("长度异常应被拒绝")
	}

	// 未配置恢复密钥时不应崩，只是禁用该能力。
	svc, _, _ := newTestService()
	if svc.RecoveryEnabled() {
		t.Fatal("默认不应启用托管恢复")
	}
	if err := svc.SetRecoveryEnvelope(context.Background(), "x", "AAAA", ""); err == nil {
		t.Fatal("未配置密钥时应拒绝开启托管")
	}
}

// TestRepairEnvelopeWithRealKeyMaterial 端到端：改口令 → 重包裹 → 新口令能解出同一个 DEK。
func TestRepairEnvelopeWithRealKeyMaterial(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()

	// 用一个真实可解的初始信封注册。
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	initial, err := GenerateCredentialsWithDEK(dek, "old-password-1", 8, 1, 1)
	if err != nil {
		t.Fatalf("构造初始凭据失败: %v", err)
	}
	req := model.RegisterRequest{
		Email:        "repair@example.com",
		PasswordHash: base64.StdEncoding.EncodeToString(
			pbkdf2.Key([]byte("old-password-1"), initial.KDFSalt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New)),
		KDFSalt:    base64.StdEncoding.EncodeToString(initial.KDFSalt),
		WrappedDEK: base64.StdEncoding.EncodeToString(initial.WrappedDEK),
		DEKIV:      base64.StdEncoding.EncodeToString(initial.DEKIV),
	}
	reg, err := svc.Register(ctx, req, RequestMeta{})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 后台改口令（保留数据）。
	hash, salt, err := GeneratePasswordOnly("new-password-2", 8, 1, 1)
	if err != nil {
		t.Fatalf("生成口令失败: %v", err)
	}
	if err := svc.users.UpdatePasswordKeepEnvelope(ctx, reg.UserID, hash, salt); err != nil {
		t.Fatalf("更新口令失败: %v", err)
	}

	// 设备侧：用本地 DEK + 新口令重新包裹（与 api.js 的 wrapDEK 等价）。
	u, err := svc.users.GetByID(ctx, reg.UserID)
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	newWrapped, newIV, err := wrapDEK(dek, pbkdf2.Key([]byte("new-password-2"), u.KDFSalt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New), u.KDFSalt)
	if err != nil {
		t.Fatalf("重包裹失败: %v", err)
	}
	if err := svc.RepairEnvelope(ctx, reg.UserID, base64.StdEncoding.EncodeToString(newWrapped), base64.StdEncoding.EncodeToString(newIV)); err != nil {
		t.Fatalf("修复失败: %v", err)
	}

	// 修复后：新口令能解出**同一个** DEK（也就是旧密文仍然可读）。
	fixed, err := svc.users.GetByID(ctx, reg.UserID)
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	got := unlockForTest(t, fixed.WrappedDEK, fixed.DEKIV, "new-password-2", fixed.KDFSalt)
	if !bytes.Equal(got, dek) {
		t.Fatal("修复后的信封解出的 DEK 与原来不一致，旧密文会读不出来")
	}

	// 顺带确认登录仍走 argon2id 校验。
	if ok, _ := VerifyClientHash(fixed.PasswordHash, base64.StdEncoding.EncodeToString(
		pbkdf2.Key([]byte("new-password-2"), fixed.KDFSalt, credentialPBKDF2Iterations, credentialPBKDF2KeyLen, sha256.New))); !ok {
		t.Fatal("新口令应能登录")
	}
	_ = time.Now()
}

// 小工具：解析 SPKI 公钥（测试里模拟浏览器 WebCrypto 的 importKey('spki')）。
func parseSPKI(der []byte) (*rsa.PublicKey, error) {
	any, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := any.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("公钥不是 RSA")
	}
	return pub, nil
}
