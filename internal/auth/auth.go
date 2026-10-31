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
	// cacheKeyFailIP 登录失败计数的 IP 维度键前缀，后接 sha256(ip)。
	cacheKeyFailIP = "meddesk:auth:fail:ip:"
	// cacheKeyIPEmails 同一 IP 已尝试过的不同邮箱计数键前缀。
	cacheKeyIPEmails = "meddesk:auth:fail:ip-emails:"
	// cacheKeyIPEmailSeen 标记「该 IP 已尝试过该邮箱」，保证每对只计一次。
	cacheKeyIPEmailSeen = "meddesk:auth:fail:ip-email:"
	// cacheKeyRefreshRevoked 刷新令牌吊销名单键前缀，后接刷新令牌 ID。
	cacheKeyRefreshRevoked = "meddesk:auth:revoked:"
	// cacheKeyGrace 刷新令牌重用宽限标记键前缀，后接被替换的旧令牌 ID。
	cacheKeyGrace = "meddesk:auth:grace:"
	// cacheKeyUserRevoked 用户级吊销标记，使已签发的访问令牌立即失效。
	cacheKeyUserRevoked = "meddesk:auth:revoked-user:"

	// maxLoginFailures 同一邮箱在 loginFailTTL 内允许的最大失败次数。
	maxLoginFailures = 10
	// maxLoginFailuresPerIP 同一 IP 在 loginFailTTL 内允许的最大失败次数。
	//
	// 【为什么单靠邮箱维度不够】攻击者换邮箱名撞库时，每个邮箱只失败一两次，
	// 邮箱计数器永远到不了阈值；同一台主机却在持续试探，必须按来源 IP 兜底。
	// 取 30（约邮箱阈值的 3 倍）：家庭/办公室 NAT 下多人共用一个出口 IP 属常态，
	// 阈值过低会误伤正常用户；30 次/15 分钟仍远高于人类手误速率。
	maxLoginFailuresPerIP = 30
	// maxEmailsPerIP 同一 IP 在 loginFailTTL 内允许尝试的不同邮箱数。
	// 撞库的典型特征是「一个 IP 撒很多账号」；正常用户只在少数几个账号间切换。
	//
	// 取 20 而不是 10：家庭宽带、小型办公室、校园/企业 NAT 后面常常有多人共用一个
	// 出口 IP，10 会让「一家三口 + 两台设备切换账号」就触顶，把正常用户锁在门外。
	// 20 仍远低于撞库所需量级（每目标试 9 次的策略在 20 个邮箱内最多覆盖 180 次尝试）。
	maxEmailsPerIP = 20
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

// truncate 按 rune 截断字符串，防止超长输入落库。
//
// 【为什么按 rune 而不是字节】旧实现是 s[:n]，中文/emoji 被拦腰截断后会产生
// 非法 UTF-8，写入 Postgres 的 text 列会直接报错（22P05），把一次登录变成 500。
// 按 rune 截断最坏占 4n 字节（maxMetaLen=256 → ≤1KiB），元数据列完全可以承受。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
