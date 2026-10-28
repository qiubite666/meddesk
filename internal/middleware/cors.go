package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// preflightMaxAge 是预检结果的浏览器缓存时长（秒）。
const preflightMaxAge = 600

// 预检回给浏览器的能力清单收成常量，避免与业务代码里散落的同名字符串产生分歧。
const (
	corsAllowMethods  = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, X-Request-ID, X-Device-ID"
	corsExposeHeaders = "X-Request-ID"
)

// CORS 处理跨域请求。origins 为白名单，采用**精确匹配**（scheme+host[:port]）；
// 白名单为空表示只允许同源：仅回显与本站同源的 Origin，绝不返回通配符 "*"，
// 未通过校验的跨域请求拿不到任何 CORS 响应头。
//
// 放行哪些源完全由 origins 决定，中间件不对 scheme 做任何限制，
// 因此 App 容器源（capacitor://localhost、https://localhost、http://localhost）
// 只要出现在白名单里就能通过；但它们**不会**自动生效——
// 空白名单必须维持「只放行同源」的语义，避免默认行为被无意放大。
func CORS(origins []string) gin.HandlerFunc {
	allow := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		o = normalizeOrigin(o)
		if o == "" {
			continue
		}
		// "*" 永不当作通配：本中间件带凭据，浏览器本身就会拒绝通配源，
		// 若照单收下只会让人误以为配置已生效，故直接忽略（config 层会额外告警）。
		if o == "*" {
			continue
		}
		allow[o] = struct{}{}
	}
	allowAny := len(allow) == 0

	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader("Origin"))
		// 无 Origin 头（同源请求、服务端调用等）无需处理。
		if raw == "" {
			c.Next()
			return
		}
		origin := normalizeOrigin(raw)

		if _, ok := allow[origin]; !ok {
			// 未命中白名单：白名单为空时允许「与本站同源」的回显，其余一律不放行。
			if !allowAny || !sameOrigin(c, origin) {
				c.Next()
				return
			}
		}

		// 响应随 Origin 变化，必须声明 Vary，避免被中间缓存串号。
		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", corsAllowMethods)
		c.Header("Access-Control-Allow-Headers", corsAllowHeaders)
		c.Header("Access-Control-Expose-Headers", corsExposeHeaders)
		c.Header("Access-Control-Max-Age", strconv.Itoa(preflightMaxAge))

		if c.Request.Method == http.MethodOptions {
			// 预检请求不进入业务处理链。
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// normalizeOrigin 归一化 origin：去首尾空白、去结尾多余斜杠、统一小写。
// scheme 与 host 在 RFC 中不区分大小写，小写后才能做精确比较；
// 个别 WebView 会在 Origin 末尾多带斜杠，这里一并容忍，避免 App 请求被静默拦掉。
func normalizeOrigin(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
}

// sameOrigin 判断 Origin 是否与当前请求同源（scheme 与 host 均相等）。
func sameOrigin(c *gin.Context, origin string) bool {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if h := c.GetHeader("X-Forwarded-Host"); h != "" {
		host = h
	}
	return strings.EqualFold(origin, scheme+"://"+host)
}
