package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// Recovery 捕获 panic：记录堆栈到日志，并向客户端返回 internal 错误的统一 JSON。
// 绝不把堆栈或内部错误原文返回给客户端。
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler 是 net/http 的内部信号，必须继续向上抛，
				// 否则连接无法被正确终止。
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.Error("请求处理发生 panic",
					"request_id", GetRequestID(c),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"client_ip", c.ClientIP(),
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				_ = c.Error(apierr.New(apierr.CodeInternal, "服务器内部错误"))
				if c.Writer.Written() {
					c.Abort()
					return
				}
				abortWithAPIError(c, apierr.New(apierr.CodeInternal, "服务器内部错误"))
			}
		}()
		c.Next()
	}
}
