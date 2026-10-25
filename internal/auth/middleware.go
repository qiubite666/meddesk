package auth

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// ctxKeyUserID 是 gin 上下文中保存当前用户 ID 的键名。
const ctxKeyUserID = "user_id"

// RequireAuth 校验 Authorization: Bearer <jwt>，成功后把用户 ID 写入上下文键 "user_id"。
func RequireAuth(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			writeError(c, apierr.New(apierr.CodeUnauthorized, msgSessionExpired))
			c.Abort()
			return
		}
		userID, err := svc.Authorize(token)
		if err != nil {
			writeError(c, err)
			c.Abort()
			return
		}
		c.Set(ctxKeyUserID, userID)
		c.Next()
	}
}

// UserID 从 gin 上下文取出当前用户 ID。
func UserID(c *gin.Context) (string, bool) {
	v, ok := c.Get(ctxKeyUserID)
	if !ok {
		return "", false
	}
	id, ok := v.(string)
	return id, ok && id != ""
}

// MetaFromRequest 从 gin 上下文抽取客户端 IP、UA 与设备标识。
func MetaFromRequest(c *gin.Context) RequestMeta {
	return RequestMeta{
		IP:        c.ClientIP(),
		UserAgent: c.GetHeader("User-Agent"),
		DeviceID:  c.GetHeader("X-Device-Id"),
	}
}

// bearerToken 从 Authorization 头提取 Bearer 令牌。
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
