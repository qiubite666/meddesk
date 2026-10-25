package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/cache"
)

// TestRateLimit429 验证同一 IP 超出突发额度后返回 429、Retry-After 与 rate_limited。
func TestRateLimit429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// rps=1、burst=2 → 窗口 2s、窗口内最多 2 次。
	r.Use(RequestID(), RateLimit(cache.NewNoop(), 1, 2))
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	var last *httptest.ResponseRecorder
	for i := 1; i <= 3; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		last = w
		if i <= 2 && w.Code != http.StatusOK {
			t.Fatalf("第 %d 次应放行: code=%d", i, w.Code)
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次应限流: code=%d body=%s", last.Code, last.Body.String())
	}
	if ra := last.Header().Get("Retry-After"); ra == "" {
		t.Fatal("限流响应缺少 Retry-After 头")
	}
	if got := last.Header().Get(RequestIDHeader); got == "" {
		t.Fatal("响应缺少 X-Request-ID 头")
	}
}
