package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// BodyLimit 限制请求体大小，超限返回 payload_too_large。
// 双重校验：先用 Content-Length 快速拒绝（避免无谓读取），
// 再用 http.MaxBytesReader 兜住 chunked / 未声明长度的请求。
// 安全说明：超限请求体直接丢弃，不做解析也不写日志。
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) { applyBodyLimit(c, maxBytes) }
}

// PathLimit 描述「某路径前缀使用某请求体上限」的规则。
type PathLimit struct {
	Prefix string
	Bytes  int64
}

// BodyLimitByPath 按路由前缀区分请求体上限，未命中任何规则时使用 defaultBytes。
// 用途：网盘/文件类接口要收的单文件通常远大于业务附件上限（默认 8 MiB），
// 全局统一小上限会误伤正常上传，因此按前缀放宽。
// 多条规则命中时取前缀最长的一条，保证行为确定。
func BodyLimitByPath(defaultBytes int64, rules []PathLimit) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := defaultBytes
		best := 0
		for _, rule := range rules {
			if rule.Prefix == "" {
				continue
			}
			if strings.HasPrefix(c.Request.URL.Path, rule.Prefix) && len(rule.Prefix) > best {
				limit = rule.Bytes
				best = len(rule.Prefix)
			}
		}
		applyBodyLimit(c, limit)
	}
}

// applyBodyLimit 执行实际的双重校验（见 BodyLimit 说明）并放行到下一环。
func applyBodyLimit(c *gin.Context, maxBytes int64) {
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
