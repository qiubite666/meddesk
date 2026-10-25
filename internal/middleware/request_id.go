package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestIDHeader 是请求 ID 的请求/响应头名称。
const RequestIDHeader = "X-Request-ID"

// requestIDLength 是生成的请求 ID 长度（uuid 去掉连字符后截取前 12 位，够短且碰撞概率极低）。
const requestIDLength = 12

// RequestID 保证每个请求都有 ID：
// 上游（如 Nginx）已注入则沿用，否则生成；同时写入响应头与 gin 上下文（键 "request_id"）。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		c.Set(ContextKeyRequestID, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// newRequestID 生成一个短请求 ID。
func newRequestID() string {
	raw := strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(raw) > requestIDLength {
		return raw[:requestIDLength]
	}
	return raw
}
