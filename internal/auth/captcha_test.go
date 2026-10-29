package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/cache"
)

// newTestCaptcha 构造一个使用进程内缓存的验证码存储。
// 用真实缓存实现而非桩：过期与清扫都在缓存层，桩掉就测不到 TTL 行为。
func newTestCaptcha(t *testing.T) (*captchaStore, cache.Cache) {
	t.Helper()
	c := cache.NewNoop()
	t.Cleanup(func() { _ = c.Close() })
	return newCaptchaStore(c), c
}

// issueAndGet 生成一张验证码，并直接取回正确答案（测试专用捷径，生产代码不暴露明文）。
func issueAndGet(t *testing.T, s *captchaStore, c cache.Cache) (string, string) {
	t.Helper()
	id, svg, err := s.issue(context.Background())
	if err != nil {
		t.Fatalf("生成验证码失败: %v", err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("SVG 不是完整文档: %.60q", svg)
	}
	// 关键安全断言：SVG 里不能出现任何文本节点。
	// 早期版本用 <text> 渲染字符，脚本一条正则就能把答案全提出来 ——
	// 这条断言就是为了防止那个缺陷以任何形式回归。
	if n := strings.Count(svg, "<text"); n != 0 {
		t.Fatalf("SVG 含 %d 个文本节点，会把验证码明文泄露给脚本，必须为 0", n)
	}
	if n := strings.Count(svg, "<rect"); n < captchaLength*5 {
		t.Fatalf("点阵方块过少（%d），字符可能没画出来", n)
	}
	code, err := c.Get(context.Background(), captchaCodeKey(id))
	if err != nil {
		t.Fatalf("读取正确答案失败: %v", err)
	}
	// 正确答案本身也不能以明文形式出现在 SVG 里。
	if strings.Contains(svg, code) {
		t.Fatalf("SVG 中出现了验证码明文 %q", code)
	}
	if len(code) != captchaLength {
		t.Fatalf("验证码长度 = %d，期望 %d", len(code), captchaLength)
	}
	return id, code
}

// TestCaptchaVerifySuccess 生成后立即可校验通过。
func TestCaptchaVerifySuccess(t *testing.T) {
	s, c := newTestCaptcha(t)
	id, code := issueAndGet(t, s, c)
	if !s.verify(context.Background(), id, code) {
		t.Fatal("正确答案校验失败")
	}
}

// TestCaptchaVerifyCaseInsensitive 用户是照着图输入的，大小写不应成为障碍。
func TestCaptchaVerifyCaseInsensitive(t *testing.T) {
	s, c := newTestCaptcha(t)
	id, code := issueAndGet(t, s, c)
	if !s.verify(context.Background(), id, strings.ToLower(code)) {
		t.Fatal("小写输入应通过校验")
	}
}

// TestCaptchaVerifyWrongCode 错误答案必须失败，且不能消耗掉正确条目以外的东西。
func TestCaptchaVerifyWrongCode(t *testing.T) {
	s, c := newTestCaptcha(t)
	id, code := issueAndGet(t, s, c)
	wrong := "ZZZZ"
	if strings.EqualFold(wrong, code) {
		wrong = "YYYY"
	}
	if s.verify(context.Background(), id, wrong) {
		t.Fatal("错误验证码竟然通过校验")
	}
}

// TestCaptchaVerifySingleUse 一次性：用过一次后同一个 id 立即失效（防重放）。
func TestCaptchaVerifySingleUse(t *testing.T) {
	s, c := newTestCaptcha(t)
	id, code := issueAndGet(t, s, c)
	if !s.verify(context.Background(), id, code) {
		t.Fatal("首次校验应通过")
	}
	if s.verify(context.Background(), id, code) {
		t.Fatal("同一验证码被重复使用")
	}
	if _, err := c.Get(context.Background(), captchaCodeKey(id)); err == nil {
		t.Fatal("校验通过后条目未被删除")
	}
}

// TestCaptchaExpired 过期后即使答案正确也失败。
func TestCaptchaExpired(t *testing.T) {
	s, c := newTestCaptcha(t)
	s.ttl = 20 * time.Millisecond
	id, code := issueAndGet(t, s, c)
	time.Sleep(60 * time.Millisecond)
	if s.verify(context.Background(), id, code) {
		t.Fatal("过期验证码仍然通过校验")
	}
}

// TestCaptchaLockedAfterMaxFailures 连错达到上限后作废：此时输对也不放行，
// 防止攻击者在同一张图上反复猜。
func TestCaptchaLockedAfterMaxFailures(t *testing.T) {
	s, c := newTestCaptcha(t)
	id, code := issueAndGet(t, s, c)
	wrong := "ZZZZ"
	if strings.EqualFold(wrong, code) {
		wrong = "YYYY"
	}
	for i := 0; i < captchaMaxFailures; i++ {
		if s.verify(context.Background(), id, wrong) {
			t.Fatalf("第 %d 次错误输入竟然通过", i+1)
		}
	}
	if s.verify(context.Background(), id, code) {
		t.Fatal("连错达到上限后，正确答案仍然通过校验")
	}
}

// TestCaptchaUnknownID 未知 id 返回失败而不是 panic。
func TestCaptchaUnknownID(t *testing.T) {
	s, _ := newTestCaptcha(t)
	if s.verify(context.Background(), "不存在的-id", "ABCD") {
		t.Fatal("未知 captchaId 竟然通过校验")
	}
	if s.verify(context.Background(), "", "") {
		t.Fatal("空 captchaId / 空验证码竟然通过校验")
	}
}

// ---------------------------------------------------------------- 接口层

// newCaptchaRouter 挂载 /api/v1/auth/* 路由，用于验证路由与校验顺序。
func newCaptchaRouter(t *testing.T) (*gin.Engine, *fakeCache) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc, _, fc := newTestService()
	r := gin.New()
	Mount(r.Group("/api/v1"), svc)
	return r, fc
}

// fetchCaptcha 走 HTTP 取一张验证码，并从缓存取出正确答案（模拟"用户看图"）。
func fetchCaptcha(t *testing.T, r *gin.Engine, fc *fakeCache) CaptchaResponse {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/captcha", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("取验证码状态码 = %d，响应 = %s", w.Code, w.Body.String())
	}
	var resp CaptchaResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析验证码响应失败: %v", err)
	}
	if resp.CaptchaID == "" || !strings.HasPrefix(resp.SVG, "<svg") {
		t.Fatalf("验证码响应不完整: %+v", resp)
	}
	if resp.ExpiresIn != int(captchaTTL.Seconds()) {
		t.Fatalf("expiresIn = %d，期望 %d", resp.ExpiresIn, int(captchaTTL.Seconds()))
	}
	code, err := fc.Get(context.Background(), captchaCodeKey(resp.CaptchaID))
	if err != nil {
		t.Fatalf("缓存中找不到验证码答案: %v", err)
	}
	resp.SVG = code // 复用字段把答案带出，避免测试再定义一个结构
	return resp
}

// postRegister 以给定验证码提交注册请求。
func postRegister(t *testing.T, r *gin.Engine, captchaID, captchaCode string) *httptest.ResponseRecorder {
	t.Helper()
	body := registerReq()
	// registerReq 的邮箱带空格（用于测服务层归一化），这里走 JSON 绑定会先被 email 校验拦下。
	body.Email = "captcha@example.com"
	body.CaptchaID = captchaID
	body.CaptchaCode = captchaCode
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化注册请求失败: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestRegisterRequiresCaptcha 注册必须带正确验证码，且成功后同一个码不能再用。
func TestRegisterRequiresCaptcha(t *testing.T) {
	r, fc := newCaptchaRouter(t)
	c := fetchCaptcha(t, r, fc)
	answer := c.SVG
	c.SVG = ""

	if w := postRegister(t, r, c.CaptchaID, answer); w.Code != http.StatusOK {
		t.Fatalf("正确验证码注册状态码 = %d，响应 = %s", w.Code, w.Body.String())
	}
	// 注册成功路径不变：仍要返回令牌对。
	w := postRegister(t, r, c.CaptchaID, answer)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("复用验证码状态码 = %d，期望 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), msgCaptchaInvalid) {
		t.Fatalf("复用验证码响应缺少统一提示: %s", w.Body.String())
	}
}

// TestRegisterWrongCaptchaKeepsCode 单次输错不烧掉验证码：
// 上限是 5 次，正常用户看错一位还有机会改，不必重新取图。
func TestRegisterWrongCaptchaKeepsCode(t *testing.T) {
	r, fc := newCaptchaRouter(t)
	c := fetchCaptcha(t, r, fc)
	answer := c.SVG
	c.SVG = ""

	// 字符集里没有 0，所以 "0000" 一定不是正确答案。
	if w := postRegister(t, r, c.CaptchaID, "0000"); w.Code != http.StatusBadRequest {
		t.Fatalf("错误验证码状态码 = %d，期望 400", w.Code)
	}
	if w := postRegister(t, r, c.CaptchaID, answer); w.Code != http.StatusOK {
		t.Fatalf("输错一次后用正确答案状态码 = %d，期望 200，响应 = %s", w.Code, w.Body.String())
	}
}
