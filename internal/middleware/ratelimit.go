package middleware

import (
	"math"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/cache"
)

// rateLimitKeyPrefix 是限流键的前缀（最终键为 meddesk:rl:<clientIP>）。
const rateLimitKeyPrefix = "rl:"

// RateLimit 按客户端 IP 做速率限制，超限返回 429 + Retry-After + rate_limited。
//
// 算法说明：令牌桶的近似实现——基于 cache 的 Incr + TTL 固定计数窗口。
// 取 refill 满 burst 个令牌所需时间 window = burst / rps 作为窗口长度，
// 窗口内允许 burst 次请求，因此长期平均速率收敛到 rps，同时容忍 burst 突发。
// 选它而非双键滑动窗口的原因：Cache 抽象只暴露 Incr/TTL，单键方案对
// Redis 与进程内降级实现都成立，且不依赖 Lua。
func RateLimit(c cache.Cache, rps float64, burst int) gin.HandlerFunc {
	// 参数非法或缓存不可用时直接放行，避免限流组件把接口整体打挂。
	if c == nil || rps <= 0 || burst <= 0 {
		return func(ctx *gin.Context) { ctx.Next() }
	}
	window, limit := limitWindow(rps, burst)

	return func(g *gin.Context) {
		count, err := c.Incr(g.Request.Context(), rateLimitKeyPrefix+g.ClientIP(), window)
		if err != nil {
			// 缓存故障不阻断业务（fail-open），仅由日志/指标观测。
			g.Next()
			return
		}
		if int(count) <= limit {
			g.Next()
			return
		}

		g.Header("Retry-After", strconv.Itoa(retryAfter(count, limit, rps)))
		abortWithAPIError(g, apierr.New(apierr.CodeRateLimited, "请求过于频繁，请稍后再试"))
	}
}

// limitWindow 计算窗口长度与窗口内允许的请求数。
// burst >= rps 时窗口 >= 1s，窗口内上限取 burst；
// burst < rps 时窗口至少 1s（避免亚秒窗口导致计数被过早重置），
// 此时窗口内上限退化为「1 秒内允许的 sustained 请求数」。
func limitWindow(rps float64, burst int) (time.Duration, int) {
	if sec := float64(burst) / rps; sec >= 1 {
		return time.Duration(sec * float64(time.Second)), burst
	}
	return time.Second, int(math.Ceil(rps))
}

// retryAfter 估算需要等待的秒数：按超出量除以补充速率计算，至少 1 秒。
func retryAfter(count int64, limit int, rps float64) int {
	sec := math.Ceil(float64(count-int64(limit)) / rps)
	if sec < 1 {
		return 1
	}
	if sec > 3600 {
		return 3600
	}
	return int(sec)
}
