package auth

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/cache"
	"github.com/qiubite666/meddesk/internal/model"
)

// TestTruncateRuneSafe 覆盖 A-17：截断必须按 rune，且不产生非法 UTF-8。
func TestTruncateRuneSafe(t *testing.T) {
	cn := strings.Repeat("中", 10)
	got := truncate(cn, 4)
	if utf8.ValidString(got) == false {
		t.Fatalf("中文截断后不是合法 UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("中文截断后 rune 数 = %d，期望 4", n)
	}

	emoji := strings.Repeat("👨‍👩‍👧‍👦", 6)
	got = truncate(emoji, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("emoji 截断后不是合法 UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("emoji 截断后 rune 数 = %d，期望 5", n)
	}

	// 未超长时原样返回（避免无谓的 []rune 分配与内容变化）。
	if short := "abc"; truncate(short, 256) != short {
		t.Fatal("未超长字符串不应被改写")
	}
	if truncate(cn, 0) != "" {
		t.Fatal("n<=0 应返回空串")
	}
}

// TestDecodeKeyMaterialLength 覆盖 A-18：键材长度不符必须 400，而不是落库成坏信封。
func TestDecodeKeyMaterialLength(t *testing.T) {
	okSalt := base64.StdEncoding.EncodeToString(make([]byte, credentialSaltLen))
	okWrap := base64.StdEncoding.EncodeToString(make([]byte, credentialDEKLen+16))
	okIV := base64.StdEncoding.EncodeToString(make([]byte, credentialIVLen))

	if _, _, _, err := decodeKeyMaterial(okSalt, okWrap, okIV); err != nil {
		t.Fatalf("合法长度不应报错: %v", err)
	}
	cases := []struct{ name, salt, wrap, iv string }{
		{"salt 过短", base64.StdEncoding.EncodeToString(make([]byte, 8)), okWrap, okIV},
		{"wrappedDek 过短", okSalt, base64.StdEncoding.EncodeToString(make([]byte, 32)), okIV},
		{"dekIv 过长", okSalt, okWrap, base64.StdEncoding.EncodeToString(make([]byte, 16))},
	}
	for _, c := range cases {
		_, _, _, err := decodeKeyMaterial(c.salt, c.wrap, c.iv)
		if !apierr.Is(err, apierr.CodeBadRequest) {
			t.Fatalf("%s 应返回 bad_request，得到 %v", c.name, err)
		}
	}
}

// TestLoginUnknownAccountCountsFailure 覆盖 A-19：账户不存在也走假哈希并计入失败，
// 对外文案与「口令错误」完全一致（不泄露账号是否存在）。
func TestLoginUnknownAccountCountsFailure(t *testing.T) {
	ctx := context.Background()
	svc, _, fc := newTestService()

	_, err := svc.Login(ctx, model.LoginRequest{Email: "ghost@example.com", PasswordHash: strings.Repeat("x", 32)},
		RequestMeta{IP: "10.0.0.1"})
	if !apierr.Is(err, apierr.CodeUnauthorized) {
		t.Fatalf("不存在账号应返回 unauthorized，得到 %v", err)
	}
	if msg := apierr.FromError(err).Message; msg != msgInvalidCredential {
		t.Fatalf("文案泄露了账号存在性: %q", msg)
	}
	// 假哈希路径也要计入邮箱维度，否则枚举者可以无限试探某个邮箱是否存在。
	if n, _ := fc.GetInt(ctx, cacheKeyFail+hashKey("ghost@example.com")); n != 1 {
		t.Fatalf("不存在账号的失败次数 = %d，期望 1", n)
	}
}

// TestLoginIPDimensions 覆盖 A-20：IP 总失败数与「同 IP 多邮箱」两条独立阈值，
// 以及「成功后只清邮箱维度」。
func TestLoginIPDimensions(t *testing.T) {
	ctx := context.Background()
	svc, _, fc := newTestService()

	// 1) 同 IP 多邮箱：11 个不同邮箱（阈值 10）+ 第 12 次请求应被限流。
	for i := 0; i <= maxEmailsPerIP; i++ {
		_, err := svc.Login(ctx, model.LoginRequest{
			Email:        "spray" + string(rune('a'+i)) + "@example.com",
			PasswordHash: strings.Repeat("x", 32),
		}, RequestMeta{IP: "203.0.113.7"})
		if !apierr.Is(err, apierr.CodeUnauthorized) {
			t.Fatalf("第 %d 次撞库尝试应返回 unauthorized，得到 %v", i+1, err)
		}
	}
	_, err := svc.Login(ctx, model.LoginRequest{
		Email: "spray-again@example.com", PasswordHash: strings.Repeat("x", 32),
	}, RequestMeta{IP: "203.0.113.7"})
	if !apierr.Is(err, apierr.CodeRateLimited) {
		t.Fatalf("同 IP 多邮箱应触发限流，得到 %v", err)
	}

	// 2) IP 总失败数：直接置到阈值，验证判定逻辑（无需真跑 30 次 argon2）。
	const ip2 = "203.0.113.8"
	if err := fc.Set(ctx, cacheKeyFailIP+hashKey(ip2), "30", time.Minute); err != nil {
		t.Fatalf("预置计数失败: %v", err)
	}
	_, err = svc.Login(ctx, model.LoginRequest{Email: "someone@example.com", PasswordHash: strings.Repeat("x", 32)},
		RequestMeta{IP: ip2})
	if !apierr.Is(err, apierr.CodeRateLimited) {
		t.Fatalf("IP 失败数达上限应限流，得到 %v", err)
	}

	// 3) 登录成功只清邮箱维度：IP 计数保留，防止用有效账号洗白来源。
	const ip3 = "203.0.113.9"
	reg, err := svc.Register(ctx, registerReq(), RequestMeta{IP: ip3})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := fc.Set(ctx, cacheKeyFailIP+hashKey(ip3), "5", time.Minute); err != nil {
		t.Fatalf("预置计数失败: %v", err)
	}
	if err := fc.Set(ctx, cacheKeyFail+hashKey("alice@example.com"), "3", time.Minute); err != nil {
		t.Fatalf("预置计数失败: %v", err)
	}
	if _, err := svc.Login(ctx, model.LoginRequest{
		Email: "alice@example.com", PasswordHash: registerReq().PasswordHash,
	}, RequestMeta{IP: ip3}); err != nil {
		t.Fatalf("正常登录失败: %v（user=%s）", err, reg.UserID)
	}
	if n, _ := fc.GetInt(ctx, cacheKeyFail+hashKey("alice@example.com")); n != 0 {
		t.Fatalf("登录成功应清零邮箱维度计数，实际 %d", n)
	}
	if n, _ := fc.GetInt(ctx, cacheKeyFailIP+hashKey(ip3)); n != 5 {
		t.Fatalf("登录成功不应清 IP 维度计数，实际 %d", n)
	}
}

// TestParseArgon2RejectsOversizedParams 覆盖 A-27：超界参数必须在分配内存前失败。
func TestParseArgon2RejectsOversizedParams(t *testing.T) {
	const salt = "YWFhYWFhYWFhYWFhYWFhYQ"
	const key = "YmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmI"

	cases := []struct{ name, encoded string }{
		{"内存 4TiB", "$argon2id$v=19$m=4294967295,t=3,p=2$" + salt + "$" + key},
		{"迭代过大", "$argon2id$v=19$m=65536,t=1000000,p=2$" + salt + "$" + key},
		{"线程过大", "$argon2id$v=19$m=65536,t=3,p=99$" + salt + "$" + key},
		{"版本不符", "$argon2id$v=16$m=65536,t=3,p=2$" + salt + "$" + key},
		{"内存不足以支撑线程", "$argon2id$v=19$m=64,t=3,p=16$" + salt + "$" + key},
	}
	for _, c := range cases {
		start := time.Now()
		if _, err := VerifyClientHash(c.encoded, "client-value"); err == nil {
			t.Fatalf("%s 应被拒绝", c.name)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%s 校验耗时 %v，说明已按攻击者参数分配内存", c.name, d)
		}
	}
}

// TestCaptchaIPThrottle 覆盖 A-11：验证码失败必须按 IP 累计，超过阈值即 429。
func TestCaptchaIPThrottle(t *testing.T) {
	c := cache.NewNoop()
	t.Cleanup(func() { _ = c.Close() })
	s := newCaptchaStore(c)
	ctx := context.Background()
	const ip = "198.51.100.11"

	// 每次都换一张新图，模拟「失败即刷新」的攻击者：单图容错（5 次）拦不住他。
	for i := 0; i < captchaIPFailLimit; i++ {
		id, _, err := s.issue(ctx)
		if err != nil {
			t.Fatalf("生成验证码失败: %v", err)
		}
		ok, err := s.verify(ctx, id, "0000", ip) // 字符集无 0，必错
		if ok || err != nil {
			t.Fatalf("第 %d 次错误验证码: ok=%v err=%v", i+1, ok, err)
		}
	}
	id, _, err := s.issue(ctx)
	if err != nil {
		t.Fatalf("生成验证码失败: %v", err)
	}
	ok, err := s.verify(ctx, id, "0000", ip)
	if ok || !apierr.Is(err, apierr.CodeRateLimited) {
		t.Fatalf("超过 IP 阈值应返回 rate_limited: ok=%v err=%v", ok, err)
	}

	// 换一个 IP 不受影响（阈值是 per-IP 而非全站）。
	id2, _, err := s.issue(ctx)
	if err != nil {
		t.Fatalf("生成验证码失败: %v", err)
	}
	code2, err := c.Get(ctx, captchaCodeKey(id2))
	if err != nil {
		t.Fatalf("读取正确答案失败: %v", err)
	}
	if ok, err := s.verify(ctx, id2, code2, "198.51.100.12"); !ok || err != nil {
		t.Fatalf("其他 IP 不应被牵连: ok=%v err=%v", ok, err)
	}
}
