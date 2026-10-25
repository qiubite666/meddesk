package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// preflightMaxAge 是预检结果的浏览器缓存时长（秒）。
const preflightMaxAge = 600

// CORS 处理跨域请求。origins 为白名单，采用**精确匹配**（scheme+host[:port]）；
// 白名单为空表示只允许同源：仅回显与本站同源的 Origin，绝不返回通配符 "*"，
// 未通过校验的跨域请求拿不到任何 CORS 响应头。
func CORS(origins []string) gin.HandlerFunc {
	allow := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(strings.TrimSuffix(o, "/"))
		if o != "" {
			allow[strings.ToLower(o)] = struct{}{}
		}
	}
	allowAny := len(allow) == 0

	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		// 无 Origin 头（同源请求、服务端调用等）无需处理。
		if origin == "" {
			c.Next()
			return
		}

		if _, ok := allow[strings.ToLower(strings.TrimSuffix(origin, "/"))]; !ok {
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
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID, X-Device-ID")
		c.Header("Access-Control-Expose-Headers", "X-Request-ID")
		c.Header("Access-Control-Max-Age", strconv.Itoa(preflightMaxAge))

		if c.Request.Method == http.MethodOptions {
			// 预检请求不进入业务处理链。
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
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
