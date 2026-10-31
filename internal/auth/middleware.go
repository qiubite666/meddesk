package auth

import (
	"net"
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
// clientIP 返回用于限流与审计的客户端 IP，并做两处归一化。
//
// 【为什么不能直接用 c.ClientIP()】
//  1. 带 zone 的 IPv6（链路本地常见，如 fe80::1%eth0）经 SplitHostPort 后仍带
//     "%eth0"，net.ParseIP 解析失败 —— 限流键要么变成非法字符串，要么变空，
//     结果是「同一台机器可以无限次尝试」，而它就在同一广播域里。
//  2. v4-mapped（::ffff:1.2.3.4）需要与 1.2.3.4 归一。**注意分裂的真实来源**：
//     直连路径上 gin 会先 net.ParseIP 再 .String()，此时 Go 已做 To4() 归一，
//     不会分裂；只有**代理路径**（gin 直接返回 X-Forwarded-For 里的原始字符串）
//     才可能出现 "::ffff:1.2.3.4" 与 "1.2.3.4" 并存，把同一客户端拆成两个计数桶。
//     这里统一 To4() 是把两种来源都收敛，不依赖调用路径。
//
// 归一化后：去掉 zone、IPv4（含 v4-mapped）统一成点分四段。解析失败返回空串，
// 由调用方按「无法归因」处理（当前策略：不做 IP 维度判定，而不是把所有人
// 塞进同一个 unknown 桶——后者会让一次攻击锁死全部用户）。
//
// 【部署硬约束】不得启用 gin 的 TrustedPlatform：一旦设置，gin 会用该平台专用头
// 覆盖 ClientIP 的返回值（例如某些 PaaS 的自定义头），而本函数拿到的就是被覆盖后
// 的值——limit key 会变成头里的任意字符串，IP 限流等于没有。信任的代理只能通过
// TrustedProxies 声明，且不能是 unix socket（net.IP 判空后整条 IP 限流会静默跳过）。
func clientIP(c *gin.Context) string {
	raw := strings.TrimSpace(c.ClientIP())
	if raw == "" && c.Request != nil {
		// gin 的 ClientIP() 内部先 SplitHostPort 再 net.ParseIP，
		// 而 net.ParseIP 不认 "%eth0" 这种 zone —— 于是带 zone 的链路本地地址
		// 会被它直接判成""（不是"没有 IP"，是"解析失败"）。
		// 这里自己剥一层再解析，把这类客户端捞回来。
		host, _, err := net.SplitHostPort(strings.TrimSpace(c.Request.RemoteAddr))
		if err != nil {
			host = strings.TrimSpace(c.Request.RemoteAddr)
		}
		raw = host
	}
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, '%'); i >= 0 {
		raw = raw[:i]
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// MetaFromRequest 由 gin 上下文构造请求元信息（IP / UA / 设备号）。
func MetaFromRequest(c *gin.Context) RequestMeta {
	return RequestMeta{
		IP:        clientIP(c),
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
