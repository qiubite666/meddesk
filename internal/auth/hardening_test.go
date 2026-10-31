package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

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

// TestClientIPNormalized 覆盖「IP 归一化缺失导致限流被稀释/失效」。
//
// 三个真实场景：
//   - 带 zone 的 IPv6（fe80::1%eth0）：gin 原样返回，net.ParseIP 解析失败；
//     若不处理，限流键非法或为空，同一广播域内的机器可以无限尝试。
//   - v4-mapped（::ffff:1.2.3.4）：与 1.2.3.4 是同一个客户端，
//     字符串不同则被拆成两个计数桶，阈值实际翻倍。
//   - 非法/空地址：应返回空串，交由调用方按「无法归因」处理。
func TestClientIPNormalized(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct{ remote, want string }{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"[::ffff:1.2.3.4]:80", "1.2.3.4"},
		{"[fe80::1%eth0]:41600", "fe80::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[::1]:80", "::1"},
		{"", ""},
		{"not-an-ip:80", ""},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		c.Request.RemoteAddr = tc.remote
		if got := clientIP(c); got != tc.want {
			t.Errorf("RemoteAddr=%q 归一化后 %q，期望 %q", tc.remote, got, tc.want)
		}
	}

	// 同一条链路：归一化结果必须能当限流键用（非空且稳定）。
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	c.Request.RemoteAddr = "[::ffff:203.0.113.9]:1234"
	first := clientIP(c)
	c.Request.RemoteAddr = "203.0.113.9:9999"
	if second := clientIP(c); first != second || first == "" {
		t.Fatalf("v4-mapped 与点分地址应归一到同一键: %q vs %q", first, second)
	}
}

// TestArgon2GateBusyReturnsError 锁住"排队超时必须失败关闭"。
//
// 【为什么这条不能省】闸门刚加进来时，两个包装函数曾写成 `key, _ := argon2InSlot(...)`：
// 一旦排队超时，它会返回 (nil, nil) —— 调用方拿到空 key 继续走，
// HashClientHash 会把**空哈希**写进库，账号从此无法登录（不可逆）。
// 这类"错误被静默吞掉"的缺陷只能靠测试钉住：占满闸门 → 断言必须返回错误，
// 且**绝不能**返回可用哈希。
func TestArgon2GateBusyReturnsError(t *testing.T) {
	oldTimeout := argon2QueueTimeout
	argon2QueueTimeout = 20 * time.Millisecond
	defer func() { argon2QueueTimeout = oldTimeout }()

	// 占满全部槽位，模拟重载。
	for i := 0; i < argon2MaxConcurrent; i++ {
		argon2Slots <- struct{}{}
	}
	defer func() {
		for i := 0; i < argon2MaxConcurrent; i++ {
			<-argon2Slots
		}
	}()

	hash, err := HashClientHash("client-value", 8, 1, 1)
	if !errors.Is(err, ErrArgon2Busy) {
		t.Fatalf("排队超时应返回 ErrArgon2Busy，实际 %v", err)
	}
	if hash != "" {
		t.Fatalf("失败时不得返回哈希（空哈希落库会永久毁掉账号），实际 %q", hash)
	}

	ok, err := VerifyClientHash(fakePasswordHash, "client-value")
	if !errors.Is(err, ErrArgon2Busy) {
		t.Fatalf("校验排队超时应返回 ErrArgon2Busy，实际 %v", err)
	}
	if ok {
		t.Fatalf("超时时不得返回 ok=true（会被当成口令正确）")
	}

	// 对外必须是 503 而不是 500：500 会让客户端立即重试，加剧排队。
	if code := apierr.FromError(argon2Err(ErrArgon2Busy)).Code; code != apierr.CodeUnavailable {
		t.Fatalf("过载应映射为 unavailable(503)，实际 %s", code)
	}
	if code := apierr.FromError(argon2Err(errors.New("boom"))).Code; code != apierr.CodeInternal {
		t.Fatalf("其它 argon2 错误仍应为 internal，实际 %s", code)
	}
}

// TestRefreshFamilyExpiry 锁住「会话族绝对上限」。
//
// 缺陷回顾：expires_at 每次轮换都被重置为 now+TTL（滑动过期），
// 于是只要每周用一次，令牌就能永久续命——泄露后无法靠时间自然失效。
// 修法是给每个会话族一个不可延长的 family_expires_at，轮换时原样继承。
func TestRefreshFamilyExpiry(t *testing.T) {
	ctx := context.Background()
	svc, fr, _ := newTestService()

	u := &model.User{ID: "11111111-1111-1111-1111-111111111111", Email: "family@example.com"}
	if err := svc.users.Create(ctx, u); err != nil {
		t.Fatalf("预置账号失败: %v", err)
	}

	resp, id, err := svc.issue(ctx, u, RequestMeta{IP: "203.0.113.7"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	first := fr.byID[id]
	if first == nil || first.FamilyExpiresAt.IsZero() {
		t.Fatalf("首签必须写入族寿命")
	}
	if !first.FamilyExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("族寿命(%v)应晚于单次令牌寿命(%v)", first.FamilyExpiresAt, first.ExpiresAt)
	}

	// 轮换一次：新令牌的族寿命必须与旧令牌**完全相同**，不得被 now+TTL 推迟。
	second, err := svc.Refresh(ctx, resp.RefreshToken, RequestMeta{IP: "203.0.113.7"})
	if err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}
	sum := sha256.Sum256([]byte(second.RefreshToken))
	rotated, err := svc.refresh.GetByHash(ctx, sum[:])
	if err != nil {
		t.Fatalf("读取轮换后的令牌失败: %v", err)
	}
	if !rotated.FamilyExpiresAt.Equal(first.FamilyExpiresAt) {
		t.Fatalf("轮换不得延长族寿命：旧 %v，新 %v", first.FamilyExpiresAt, rotated.FamilyExpiresAt)
	}

	// 把族寿命推到过去：即使单次令牌仍在有效期内，也必须拒绝并要求重新登录。
	rotated.FamilyExpiresAt = time.Now().Add(-time.Minute)
	if _, err := svc.Refresh(ctx, second.RefreshToken, RequestMeta{IP: "203.0.113.7"}); !apierr.Is(err, apierr.CodeUnauthorized) {
		t.Fatalf("族寿命已过应返回 401，实际 %v", err)
	}
}
