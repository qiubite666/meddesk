package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// BodyLimit 限制请求体大小，超限返回 payload_too_large。
// 双重校验：先用 Content-Length 快速拒绝（避免无谓读取），
// 再用 http.MaxBytesReader 兜住 chunked / 未声明长度的请求。
// 安全说明：超限请求体直接丢弃，不做解析也不写日志。
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 {
			if c.Request.ContentLength > maxBytes {
				abortWithAPIError(c, apierr.Newf(apierr.CodePayloadTooLarge,
					"请求体过大，上限 %d 字节", maxBytes))
				return
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}

		c.Next()

		// 读取阶段才触发超限（如 chunked 传输）：若下游尚未写出响应，补一个统一错误。
		if maxBytes > 0 && !c.Writer.Written() && hasMaxBytesError(c) {
			abortWithAPIError(c, apierr.Newf(apierr.CodePayloadTooLarge,
				"请求体过大，上限 %d 字节", maxBytes))
		}
	}
}

// hasMaxBytesError 判断处理链上是否出现了请求体超限错误。
func hasMaxBytesError(c *gin.Context) bool {
	var target *http.MaxBytesError
	for _, e := range c.Errors {
		if errors.As(e.Err, &target) {
			return true
		}
	}
	return false
}
