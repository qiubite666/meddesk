// Package auth 实现 MedDesk 的认证与授权：注册、登录、令牌轮换与吊销、口令修改。
//
// 安全模型（端到端加密）：服务端永远收不到明文口令，客户端送来的是 PBKDF2
// 派生结果的 base64，服务端再用 argon2id 做二次哈希后落库；数据密钥（DEK）
// 的包裹材料（kdfSalt/wrappedDek/dekIv）由客户端保管，服务端只做原样回显。
package auth

import (
	"time"

	"github.com/qiubite666/meddesk/internal/config"
)

// cache 键前缀与限流阈值。
const (
	// cacheKeyFail 登录失败计数键前缀，后接 sha256(email) 的十六进制串。
	cacheKeyFail = "meddesk:auth:fail:"
	// cacheKeyRefreshRevoked 刷新令牌吊销名单键前缀，后接刷新令牌 ID。
	cacheKeyRefreshRevoked = "meddesk:auth:revoked:"
	// cacheKeyGrace 刷新令牌重用宽限标记键前缀，后接被替换的旧令牌 ID。
	cacheKeyGrace = "meddesk:auth:grace:"
	// cacheKeyUserRevoked 用户级吊销标记，使已签发的访问令牌立即失效。
	cacheKeyUserRevoked = "meddesk:auth:revoked-user:"

	// maxLoginFailures 同一邮箱在 loginFailTTL 内允许的最大失败次数。
	maxLoginFailures = 10
	// loginFailTTL 登录失败计数的保留时长。
	loginFailTTL = 15 * time.Minute

	// refreshTokenBytes 刷新令牌随机字节数（32 字节 → base64url 后发给客户端）。
	refreshTokenBytes = 32
	// maxMetaLen 设备信息字段落库前的最大长度，避免超长 UA 污染存储。
	maxMetaLen = 256
)

// 用户可见文案（简体中文）。
const (
	msgInvalidCredential = "邮箱或口令不正确"
	msgSessionExpired    = "登录已失效，请重新登录"
	msgRateLimited       = "登录尝试过于频繁，请稍后再试"
	msgInternal          = "服务器内部错误"
)

// RequestMeta 记录一次认证请求的客户端环境，用于刷新令牌绑定与异常登录审计。
type RequestMeta struct {
	IP        string // 客户端 IP
	UserAgent string // 浏览器 UA
	DeviceID  string // 客户端设备标识（可选）
}

// normalizeConfig 补齐为零的配置项，避免调用方漏填时产生零值 TTL。
func normalizeConfig(cfg config.AuthConfig) config.AuthConfig {
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = 15 * time.Minute
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.Argon2Memory == 0 {
		cfg.Argon2Memory = 65536
	}
	if cfg.Argon2Time == 0 {
		cfg.Argon2Time = 3
	}
	if cfg.Argon2Threads == 0 {
		cfg.Argon2Threads = 2
	}
	return cfg
}

// truncate 按字符截断字符串，防止超长输入落库。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
