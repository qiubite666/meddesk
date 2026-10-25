package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// slowRequestThreshold 超过该耗时的请求按 warn 级别记录，便于发现慢接口。
const slowRequestThreshold = time.Second

// Logger 输出结构化访问日志。
// 安全约束：本中间件**绝不记录请求体/响应体**（E2EE 项目，禁止日志中出现可能含明文的数据），
// 仅记录 method、path、status、latency_ms、request_id、client_ip、user_agent、error。
// 注意 path 使用路由模板（c.FullPath），避免高基数与隐私参数进入日志。
func Logger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		if tpl := c.FullPath(); tpl != "" {
			path = tpl
		}
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
			"request_id", GetRequestID(c),
			"client_ip", c.ClientIP(),
			"user_agent", c.Request.UserAgent(),
		}
		if err := c.Errors.Last(); err != nil {
			attrs = append(attrs, "error", err.Error())
		}

		if latency > slowRequestThreshold {
			log.Warn("慢请求", attrs...)
			return
		}
		log.Info("访问日志", attrs...)
	}
}
