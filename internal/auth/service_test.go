package auth

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// newTestService 构造使用内存替身的测试服务（argon2 取最小可用参数以加快测试）。
func newTestService() (*Service, *fakeRefresh, *fakeCache) {
	fr, fc := newFakeRefresh(), newFakeCache()
	cfg := config.AuthConfig{
		JWTSecret:         strings.Repeat("s", 32),
		AccessTTL:         15 * time.Minute,
		RefreshTTL:        time.Hour,
		Argon2Memory:      8,
		Argon2Time:        1,
		Argon2Threads:     1,
		SignupEnabled:     true,
		RefreshReuseGrace: 30 * time.Second,
	}
	return NewService(newFakeUsers(), fr, fc, cfg), fr, fc
}

func registerReq() model.RegisterRequest {
	return model.RegisterRequest{
		Email:        " Alice@Example.COM ",
		PasswordHash: "cGFzc3dvcmQtaGFzLWJhc2U2NC12YWx1ZS0xMjM0NTY3ODkw",
		KDFSalt:      "c2FsdHNhbHRzYWx0c2FsdA==",
		WrappedDEK:   "d3JhcHBlZC1kZWstdmFsdWUtMTIz",
		DEKIV:        "aXZpdml2aXY=",
	}
}

// TestPasswordHashRoundTrip 覆盖 argon2id 哈希与校验往返。
func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := HashClientHash("client-pbkdf2-value", 8, 1, 1)
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=8,t=1,p=1$") {
		t.Fatalf("编码格式不符: %s", encoded)
	}
	ok, err := VerifyClientHash(encoded, "client-pbkdf2-value")
	if err != nil || !ok {
		t.Fatalf("正确口令应校验通过: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyClientHash(encoded, "wrong-value"); ok {
		t.Fatal("错误口令不应通过校验")
	}
}

// TestAccessTokenIssueAndAuthorize 覆盖 JWT 签发与校验。
func TestAccessTokenIssueAndAuthorize(t *testing.T) {
	svc, _, _ := newTestService()
	resp, err := svc.Register(context.Background(), registerReq(), RequestMeta{IP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if resp.UserID == "" || resp.AccessToken == "" || resp.KDFSalt == "" {
		t.Fatalf("注册响应缺少字段: %+v", resp)
	}
	uid, err := svc.Authorize(resp.AccessToken)
	if err != nil || uid != resp.UserID {
		t.Fatalf("Authorize 失败: uid=%q err=%v", uid, err)
	}
	if _, err := svc.Authorize(resp.AccessToken + "x"); !apierr.Is(err, apierr.CodeUnauthorized) {
		t.Fatalf("被篡改的令牌应返回 unauthorized: %v", err)
	}
}

// TestRefreshRotationAndReplay 覆盖刷新令牌轮换、sha256 存储与重放检测。
func TestRefreshRotationAndReplay(t *testing.T) {
	ctx := context.Background()
	svc, fr, _ := newTestService()
	resp, err := svc.Register(ctx, registerReq(), RequestMeta{})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	old := resp.RefreshToken
	// 库中只存 sha256。
	sum := sha256.Sum256([]byte(old))
	stored, err := fr.GetByHash(ctx, sum[:])
	if err != nil || stored == nil {
		t.Fatalf("刷新令牌未按 sha256 落库: %v", err)
	}
	if _, err := fr.GetByHash(ctx, []byte(old)); err == nil {
		t.Fatal("明文刷新令牌不应可查")
	}
	// 首次轮换成功。
	r2, err := svc.Refresh(ctx, old, RequestMeta{})
	if err != nil || r2.RefreshToken == old {
		t.Fatalf("首次轮换失败: %v", err)
	}
	// 宽限期内允许旧令牌重用一次。
	if _, err := svc.Refresh(ctx, old, RequestMeta{}); err != nil {
		t.Fatalf("宽限期内应允许重用一次: %v", err)
	}
	// 宽限机会已用尽，再次使用判定为重放并撤销全部会话。
	if _, err := svc.Refresh(ctx, old, RequestMeta{}); !apierr.Is(err, apierr.CodeUnauthorized) {
		t.Fatalf("重放应返回 unauthorized: %v", err)
	}
	if fr.revokeAll != 1 {
		t.Fatalf("重放应撤销全部刷新令牌: revokeAll=%d", fr.revokeAll)
	}
}
