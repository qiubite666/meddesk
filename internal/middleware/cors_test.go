package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/config"
)

// testHost 模拟线上 API 域名，用于构造同源与跨域两种 Origin。
const testHost = "meddesk.qiubite.fun"

// newCORSRouter 构造只挂 CORS 中间件的测试路由，命中中间件后返回 200。
func newCORSRouter(origins []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS(origins))
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

// doRequest 以 testHost 为 Host 发起带 Origin 的请求。
// 线上由反向代理终结 TLS，故固定带上 X-Forwarded-Proto，
// 否则 sameOrigin 会把本站判成 http，同源用例会失真。
func doRequest(t *testing.T, r *gin.Engine, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/ping", nil)
	req.Host = testHost
	req.Header.Set("X-Forwarded-Proto", "https")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertNoCORS 断言响应没有拿到任何 CORS 放行头。
func assertNoCORS(t *testing.T, w *httptest.ResponseRecorder, origin string) {
	t.Helper()
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("源 %q 不应被放行，却拿到 ACAO=%q", origin, got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("源 %q 不应被放行，却拿到 ACAC=%q", origin, got)
	}
}

// assertAllowed 断言响应放行了 want，且带 Vary 与凭据头。
func assertAllowed(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != want {
		t.Fatalf("ACAO 期望 %q，实际 %q", want, got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("响应缺少 Vary: Origin，实际 %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials 期望 true，实际 %q", got)
	}
}

// TestCORSEmptyWhitelistOnlyAllowsSameOrigin 锁定向后兼容的底线语义：
// 白名单为空时只回显同源，绝不能顺带把 App 容器源放行（否则等于默认放大了放行范围）。
func TestCORSEmptyWhitelistOnlyAllowsSameOrigin(t *testing.T) {
	r := newCORSRouter(nil)

	w := doRequest(t, r, http.MethodGet, "https://evil.example")
	assertNoCORS(t, w, "https://evil.example")
	if w.Code != http.StatusOK {
		t.Fatalf("未放行的跨域请求不应被改写状态码，实际 %d", w.Code)
	}

	// 线上 MEDDESK_CORS_ALLOWED_ORIGINS 为空，App 容器源此时必须仍然被拦，
	// 只有运维显式写入白名单才会放行。
	for _, o := range config.DefaultAppOrigins() {
		assertNoCORS(t, doRequest(t, r, http.MethodGet, o), o)
	}

	assertAllowed(t, doRequest(t, r, http.MethodGet, "https://"+testHost), "https://"+testHost)
}

// TestCORSAllowsAppContainerOrigins 验证契约要求的三个 App 容器源放行，且带 Vary: Origin。
func TestCORSAllowsAppContainerOrigins(t *testing.T) {
	origins := config.DefaultAppOrigins()
	r := newCORSRouter(origins)

	for _, o := range origins {
		w := doRequest(t, r, http.MethodGet, o)
		assertAllowed(t, w, o)
		if w.Code != http.StatusOK {
			t.Fatalf("源 %q 的业务请求应返回 200，实际 %d", o, w.Code)
		}
	}

	// 白名单非空后走纯精确匹配：本站自身的源不在名单里就不再回显，
	// 但同源请求本就不受 CORS 约束，不影响浏览器；若需要回显把它写进名单即可。
	assertNoCORS(t, doRequest(t, r, http.MethodGet, "https://"+testHost), "https://"+testHost)

	// 前缀相同不等于同一源，不能被 localhost 条目顺带放行。
	assertNoCORS(t, doRequest(t, r, http.MethodGet, "https://evil.example"), "https://evil.example")
	assertNoCORS(t, doRequest(t, r, http.MethodGet, "https://localhost.evil.example"), "https://localhost.evil.example")
}

// TestCORSPreflightReturns204 验证 OPTIONS 预检返回 204 且方法清单正确。
func TestCORSPreflightReturns204(t *testing.T) {
	r := newCORSRouter(config.DefaultAppOrigins())

	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Host = testHost
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "capacitor://localhost")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("预检应返回 204，实际 %d body=%q", w.Code, w.Body.String())
	}
	assertAllowed(t, w, "capacitor://localhost")
	if got := w.Header().Get("Access-Control-Allow-Methods"); got != corsAllowMethods {
		t.Fatalf("Allow-Methods 期望 %q，实际 %q", corsAllowMethods, got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != corsAllowHeaders {
		t.Fatalf("Allow-Headers 期望 %q，实际 %q", corsAllowHeaders, got)
	}
	if want := strconv.Itoa(preflightMaxAge); w.Header().Get("Access-Control-Max-Age") != want {
		t.Fatalf("Max-Age 期望 %q，实际 %q", want, w.Header().Get("Access-Control-Max-Age"))
	}
	if w.Body.Len() != 0 {
		t.Fatalf("预检不应返回响应体，实际 %q", w.Body.String())
	}

	// 未放行的源做预检同样拿不到任何 CORS 头。
	req2 := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req2.Host = testHost
	req2.Header.Set("Origin", "https://evil.example")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assertNoCORS(t, w2, "https://evil.example")
}

// TestCORSOriginCaseAndTrailingSlash 验证配置侧与请求侧的 case/尾部斜杠容错。
func TestCORSOriginCaseAndTrailingSlash(t *testing.T) {
	// 白名单里故意写成大写并带尾部斜杠。
	r := newCORSRouter([]string{"HTTPS://Localhost/", "Capacitor://LOCALHOST"})

	assertAllowed(t, doRequest(t, r, http.MethodGet, "https://localhost"), "https://localhost")
	assertAllowed(t, doRequest(t, r, http.MethodGet, "Capacitor://LocalHost"), "capacitor://localhost")
	// 请求侧多带斜杠时也能命中，且回显的是规范化后的取值，保证浏览器能匹配上。
	assertAllowed(t, doRequest(t, r, http.MethodGet, "https://LOCALHOST/"), "https://localhost")
	assertAllowed(t, doRequest(t, r, http.MethodGet, "https://localhost///"), "https://localhost")
}

// TestCORSNeverUsesWildcard 确认 "*" 不会被当成通配，最坏也只是退化为只放行同源。
func TestCORSNeverUsesWildcard(t *testing.T) {
	r := newCORSRouter([]string{"*"})

	assertNoCORS(t, doRequest(t, r, http.MethodGet, "https://evil.example"), "https://evil.example")
	assertNoCORS(t, doRequest(t, r, http.MethodGet, "https://localhost"), "https://localhost")
	assertAllowed(t, doRequest(t, r, http.MethodGet, "https://"+testHost), "https://"+testHost)
}

// TestConfigValidateOrigins 覆盖 CORS 白名单的配置校验。
// ValidateOrigins 属于 config 包，而契约规定 config 包不新增测试文件，故在这里一并覆盖。
func TestConfigValidateOrigins(t *testing.T) {
	// App 容器源与常见合法写法都不应产生告警。
	for _, list := range [][]string{
		config.DefaultAppOrigins(),
		{"https://meddesk.qiubite.fun", "http://localhost:8080", "https://[::1]:8080"},
		nil,
	} {
		if got := config.ValidateOrigins(list); len(got) != 0 {
			t.Fatalf("合法配置 %v 不应产生告警，实际 %v", list, got)
		}
	}

	// 每条明显写错的条目都应产出一条告警。
	bad := []string{
		"*",
		"not-an-origin",
		"http://local host",
		"https://example.com/app",
		"https://example.com?x=1",
		"1http://example.com",
	}
	if got := config.ValidateOrigins(bad); len(got) != len(bad) {
		t.Fatalf("期望 %d 条告警，实际 %d 条：%v", len(bad), len(got), got)
	}
}
