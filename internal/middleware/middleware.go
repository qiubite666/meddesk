// Package middleware 提供与业务无关的通用 HTTP 中间件。
// 注意：本包禁止依赖 internal/auth、internal/store、internal/syncsvc，避免循环依赖；
// 也禁止记录任何请求体内容（E2EE 项目，明文只存在于客户端）。
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// ContextKeyRequestID 是请求 ID 在 gin 上下文中的键名。
const ContextKeyRequestID = "request_id"

// GetRequestID 从 gin 上下文取出请求 ID；未注入时返回空字符串。
func GetRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(ContextKeyRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// abortWithAPIError 以统一错误结构终止请求：HTTP 状态码由 apierr.HTTPStatus 决定。
func abortWithAPIError(c *gin.Context, e *apierr.Error) {
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}
