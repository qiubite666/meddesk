package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// 安全响应头的取值。CSP 采用 default-src 'self' 基线，
// 仅按一期功能需要放开：data:/blob: 图片（拍照预览）、内联样式（主题变量切换）。
const (
	cspValue = "default-src 'self'; " +
		"img-src 'self' data: blob:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; " +
		"connect-src 'self'; " +
		"form-action 'self'; " +
		"base-uri 'self'; " +
		"frame-ancestors 'none'; " +
		"object-src 'none'"

	permissionsPolicy = "geolocation=(self), camera=(self), microphone=(), " +
		"payment=(), usb=(), interest-cohort=()"

	hstsValue = "max-age=31536000"
)

// SecurityHeaders 输出一组加固响应头。
// 说明：本项目为 HTTPS + Nginx 终止 TLS 部署，HSTS 仅在判定为 HTTPS 时追加，
// 以免本地 http 调试被浏览器强制跳转。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Content-Security-Policy", cspValue)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// 相机用于拍摄药品/附件照片，地理位置在本期暂不需要但保留扩展位。
		h.Set("Permissions-Policy", permissionsPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		if isHTTPS(c) {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		c.Next()
	}
}

// isHTTPS 判断当前请求是否走 HTTPS（含 Nginx 反向代理场景）。
func isHTTPS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}
