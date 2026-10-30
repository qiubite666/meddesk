package auth

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// newSaltTestService 构造只关心 /auth/salt 的测试服务，并保留 fakeUsers 句柄
// 以便直接塞入账号。复用 fakes_test.go 里的内存实现，不引入新的依赖注入方式。
func newSaltTestService() (*Service, *fakeUsers) {
	users := newFakeUsers()
	cfg := config.AuthConfig{JWTSecret: strings.Repeat("s", 32)}
	return NewService(users, newFakeRefresh(), newFakeCache(), cfg), users
}

// TestAccountSaltNotFoundIsDeterministic 断言不存在的账号多次调用返回完全相同的盐。
//
// 【为什么这条是验收核心】曾经的实现每次返回新随机盐，而真账号的盐恒定，
// 攻击者连查两次就能判定账号是否存在——这条测试正是防这种回退。
func TestAccountSaltNotFoundIsDeterministic(t *testing.T) {
	svc, _ := newSaltTestService()
	ctx := context.Background()

	var got [3]string
	for i := 0; i < 3; i++ {
		salt, err := svc.AccountSalt(ctx, "ghost@example.com")
		if err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
		got[i] = salt
	}
	if got[0] != got[1] || got[1] != got[2] {
		t.Fatalf("同一邮箱应返回一致的假盐，实际为 %q / %q / %q", got[0], got[1], got[2])
	}
}

// TestAccountSaltDiffersByEmail 断言不同邮箱（均不存在）派生出不同的假盐，
// 否则假盐变成全局常量，客户端缓存/派生逻辑会被不同账号串用。
func TestAccountSaltDiffersByEmail(t *testing.T) {
	svc, _ := newSaltTestService()
	ctx := context.Background()

	a, err := svc.AccountSalt(ctx, "alice-ghost@example.com")
	if err != nil {
		t.Fatalf("首个邮箱调用失败: %v", err)
	}
	b, err := svc.AccountSalt(ctx, "bob-ghost@example.com")
	if err != nil {
		t.Fatalf("第二个邮箱调用失败: %v", err)
	}
	if a == b {
		t.Fatalf("不同邮箱不应得到相同假盐，二者均为 %q", a)
	}
}

// TestAccountSaltReturnsRealSaltForExistingUser 断言存在的账号仍返回真实的 KDF 盐。
// 修复只应影响「不存在」分支，真盐下发路径不得被顺手改坏。
func TestAccountSaltReturnsRealSaltForExistingUser(t *testing.T) {
	svc, users := newSaltTestService()
	ctx := context.Background()

	realSalt := make([]byte, 32)
	for i := range realSalt {
		realSalt[i] = byte(i + 1)
	}
	u := &model.User{ID: "u-1", Email: "real@example.com", KDFSalt: realSalt}
	if err := users.Create(ctx, u); err != nil {
		t.Fatalf("写入测试账号失败: %v", err)
	}

	salt, err := svc.AccountSalt(ctx, u.Email)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if want := base64.StdEncoding.EncodeToString(realSalt); salt != want {
		t.Fatalf("应返回真实盐 %q，实际 %q", want, salt)
	}
}

// TestAccountSaltFakeSaltShape 断言假盐的形态与真盐一致：标准 base64 且解码后 32 字节。
// 长度/编码不同同样会泄露「账号不存在」，所以形态本身也是不可区分性的一部分。
func TestAccountSaltFakeSaltShape(t *testing.T) {
	svc, _ := newSaltTestService()

	salt, err := svc.AccountSalt(context.Background(), "shape-ghost@example.com")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		t.Fatalf("假盐不是合法 base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("假盐应为 32 字节，实际 %d 字节", len(raw))
	}
}
