// Package config 集中管理 MedDesk 服务端配置。全部来自环境变量（可选 .env 文件）。
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config 是运行期只读配置。
type Config struct {
	Env        string
	HTTPAddr   string
	BaseURL    string
	TrustProxy bool
	WebDir     string // 非空则从此目录读取静态资源（开发模式），否则使用内嵌 FS

	DB        DBConfig
	Redis     RedisConfig
	Auth      AuthConfig
	Admin     AdminConfig
	Sync      SyncConfig
	Upload    UploadConfig
	PDF       PDFConfig
	Log       LogConfig
	RateLimit RateLimitConfig
	CORS      CORSConfig
}

type DBConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MigrateOnBoot   bool
}

type RedisConfig struct {
	URL      string // redis://[:password@]host:port/db 或 rediss://；为空表示禁用（使用进程内降级实现）
	Enabled  bool
	Password string
	DB       int
}

type AuthConfig struct {
	JWTSecret      string
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	Argon2Memory   uint32 // KiB
	Argon2Time     uint32 // iterations
	Argon2Threads  uint8
	SignupEnabled  bool
	RefreshReuseGrace time.Duration // 刷新令牌重用宽限期（并发刷新容错）

	// RecoveryKey 是「托管恢复」的 RSA 私钥（base64 PKCS#8 DER）。
	// 为空表示本部署不支持托管恢复：管理员改口令仍可走「设备自愈」的无损路径，
	// 但需要用户在仍持有 DEK 的设备上登录一次才能恢复完整访问。
	RecoveryKey string
}

// AdminConfig 是管理后台的配置。
//
// 管理员身份 = 白名单邮箱 + 该账号自身的口令（复用用户表凭据，不另存密码）。
// 后台 token 与业务 token 分离（不同 scope），即使业务 token 泄露也无法调用后台接口。
type AdminConfig struct {
	Emails []string      // 允许登录后台的邮箱白名单（大小写不敏感）
	TTL    time.Duration // 后台会话有效期

	// 独立管理员凭据（可选）。设置后，白名单邮箱除账号口令外，
	// 还可以用这组凭据登录后台。
	//
	// 【为什么需要】meddesk 是端到端加密：重置账号口令会让旧 KEK 无法解包 DEK，
	// 该账号已同步的密文与家庭关联全部失效。为了"能进后台"而重置真实账号口令，
	// 代价过大。因此后台允许配置一组独立凭据：密码只用于后台，账号本身不受影响。
	Salt         string // KDF salt（base64），前端派生 PBKDF2 用
	PasswordHash string // argon2id(PBKDF2(管理员口令, salt))，与用户表同格式
}

type SyncConfig struct {
	MaxItemsPerPush int   // 单次 push 最大条数
	MaxRecordBytes  int64 // 单条密文最大字节数
	PullPageSize    int   // 单次 pull 默认条数
	MaxPullPageSize int
}

type UploadConfig struct {
	Dir      string
	MaxBytes int64
}

type PDFConfig struct {
	FontPath string // CJK TTF 字体路径；为空则自动探测，探测失败时 PDF 导出不可用
}

type LogConfig struct {
	Level  string // debug|info|warn|error
	Format string // json|text
}

type RateLimitConfig struct {
	RPS   float64
	Burst int
}

type CORSConfig struct {
	AllowedOrigins []string
}

// Load 读取配置。优先级：环境变量 > .env 文件 > 默认值。
func Load() (*Config, error) {
	_ = godotenv.Load() // 无 .env 时静默忽略

	c := &Config{
		Env:        envString("MEDDESK_ENV", "prod"),
		HTTPAddr:   envString("MEDDESK_HTTP_ADDR", ":8080"),
		BaseURL:    envString("MEDDESK_BASE_URL", "http://localhost:8080"),
		TrustProxy: envBool("MEDDESK_TRUST_PROXY", false),
		WebDir:     envString("MEDDESK_WEB_DIR", ""),

		DB: DBConfig{
			URL:             envString("MEDDESK_DB_URL", "postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable"),
			MaxConns:        int32(envInt("MEDDESK_DB_MAX_CONNS", 10)),
			MinConns:        int32(envInt("MEDDESK_DB_MIN_CONNS", 1)),
			MaxConnLifetime: envDuration("MEDDESK_DB_MAX_CONN_LIFETIME", 30*time.Minute),
			MigrateOnBoot:   envBool("MEDDESK_DB_MIGRATE_ON_BOOT", true),
		},
		Redis: RedisConfig{
			URL: envString("MEDDESK_REDIS_URL", "redis://127.0.0.1:6379/0"),
			// 默认关闭：单实例部署时进程内 Noop 缓存即可满足限流/失败计数/吊销名单，
			// 少维护一个中间件；多副本部署或需要跨进程共享时再显式开启。
			Enabled: envBool("MEDDESK_REDIS_ENABLED", false),
		},
		Auth: AuthConfig{
			JWTSecret:         envString("MEDDESK_JWT_SECRET", ""),
			AccessTTL:         envDuration("MEDDESK_ACCESS_TTL", 15*time.Minute),
			RefreshTTL:        envDuration("MEDDESK_REFRESH_TTL", 30*24*time.Hour),
			Argon2Memory:      uint32(envInt("MEDDESK_ARGON2_MEMORY", 65536)),
			Argon2Time:        uint32(envInt("MEDDESK_ARGON2_ITERATIONS", 3)),
			Argon2Threads:     uint8(envInt("MEDDESK_ARGON2_PARALLELISM", 2)),
			// 默认关闭：避免"部署到公网却忘了关注册"变成公开的匿名账本。
			// 需要开户时临时置 true → 重启 → 注册 → 改回 false → 重启。
			SignupEnabled:     envBool("MEDDESK_SIGNUP_ENABLED", false),
			RefreshReuseGrace: envDuration("MEDDESK_REFRESH_REUSE_GRACE", 30*time.Second),
			RecoveryKey:       envString("MEDDESK_RECOVERY_KEY", ""),
		},
		Admin: AdminConfig{
			// 默认**空名单**：不在代码里内置任何个人邮箱。
			// 生产环境必须在 .meddesk.env 显式配置，否则 /admin 无人可登录（启动时会有 WARN）。
			Emails:       splitEmails(envString("MEDDESK_ADMIN_EMAILS", "")),
			TTL:          envDuration("MEDDESK_ADMIN_TTL", 8*time.Hour),
			Salt:         envString("MEDDESK_ADMIN_SALT", ""),
			PasswordHash: envString("MEDDESK_ADMIN_PASSWORD_HASH", ""),
		},
		Sync: SyncConfig{
			MaxItemsPerPush: envInt("MEDDESK_SYNC_MAX_ITEMS", 500),
			MaxRecordBytes:  envInt64("MEDDESK_SYNC_MAX_RECORD_BYTES", 1<<20), // 1 MiB
			PullPageSize:    envInt("MEDDESK_SYNC_PULL_PAGE", 300),
			MaxPullPageSize: envInt("MEDDESK_SYNC_PULL_PAGE_MAX", 1000),
		},
		Upload: UploadConfig{
			Dir:      envString("MEDDESK_UPLOAD_DIR", "/home/ops/service/meddesk/data"),
			MaxBytes: envInt64("MEDDESK_UPLOAD_MAX_BYTES", 8<<20), // 8 MiB
		},
		PDF: PDFConfig{
			FontPath: envString("MEDDESK_PDF_FONT_PATH", ""),
		},
		Log: LogConfig{
			Level:  envString("MEDDESK_LOG_LEVEL", "info"),
			Format: envString("MEDDESK_LOG_FORMAT", "json"),
		},
		RateLimit: RateLimitConfig{
			RPS:   envFloat("MEDDESK_RATE_LIMIT_RPS", 20),
			Burst: envInt("MEDDESK_RATE_LIMIT_BURST", 60),
		},
		CORS: CORSConfig{
			AllowedOrigins: splitList(envString("MEDDESK_CORS_ALLOWED_ORIGINS", "")),
		},
	}

	// CORS 白名单只告警不阻断：一次误配不应该让服务起不来，
	// 否则线上一改环境变量就可能被挡在门外，问题反而更难发现。
	for _, w := range ValidateOrigins(c.CORS.AllowedOrigins) {
		log.Printf("[warn] CORS 配置: %s", w)
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// DefaultAppOrigins 返回 Capacitor 打包客户端的容器源清单（每次返回新切片，避免调用方改动共享状态）。
//
// App 内的页面由设备本地 WebView 提供，页面 Origin 由打包配置决定、与 API 域名无关：
//   - Android：契约固定 androidScheme=https，页面源为 https://localhost；
//   - iOS：Capacitor 默认 capacitor://localhost（本次不做 iOS，列出仅为兼容默认行为）；
//   - 自定义 scheme 或本地联调时会出现 http://localhost。
//
// 安全含义：localhost 是整台机器共享的源，放行它等于允许本机运行的任意网页
// 携带用户凭据读取本 API 的数据。因此这里**不**做内置自动放行，
// 必须由运维显式写入 MEDDESK_CORS_ALLOWED_ORIGINS；
// 白名单为空时 middleware.CORS 仍维持「只放行同源」的默认语义。
func DefaultAppOrigins() []string {
	return []string{"capacitor://localhost", "https://localhost", "http://localhost"}
}

// ValidateOrigins 逐个校验 CORS 白名单条目，返回人类可读的告警文案列表（无问题则返回 nil）。
// 约定：只告警不返回 error，调用方自行决定如何记录。
func ValidateOrigins(origins []string) []string {
	var warnings []string
	for _, raw := range origins {
		o := strings.TrimSpace(raw)
		if o == "" {
			continue
		}
		// 带凭据的 CORS 里通配会被浏览器直接拒绝，放行它只会造成「配了却以为生效」的错觉。
		if o == "*" {
			warnings = append(warnings, `CORS 白名单中的 "*" 已忽略：本服务带凭据，不支持通配，请逐条列出源`)
			continue
		}
		scheme, rest, ok := strings.Cut(o, "://")
		if !ok || scheme == "" || rest == "" {
			warnings = append(warnings, fmt.Sprintf("CORS 白名单条目 %q 不是合法 origin，应为 scheme://host[:port]，例如 https://localhost", o))
			continue
		}
		if !validOriginScheme(scheme) {
			warnings = append(warnings, fmt.Sprintf("CORS 白名单条目 %q 的 scheme 不合法（只允许字母开头，可含字母、数字、+、-、.）", o))
			continue
		}
		// origin 不允许带路径或查询串：带了说明填的是站点地址而非源，匹配时永远不会命中。
		// 先于字符校验判断，好让告警文案指向真正的问题。
		if strings.ContainsAny(rest, "/?#") {
			warnings = append(warnings, fmt.Sprintf("CORS 白名单条目 %q 含路径或查询串，origin 只应写到 host[:port]", o))
			continue
		}
		if !validOriginHostPort(rest) {
			warnings = append(warnings, fmt.Sprintf("CORS 白名单条目 %q 的 host[:port] 含非法字符（只允许字母、数字、.、-、:、[、]）", o))
			continue
		}
	}
	return warnings
}

// validOriginScheme 判断 scheme 是否形如字母开头、后续为字母/数字/+/-/.。
func validOriginScheme(s string) bool {
	if s == "" {
		return false
	}
	if !isAlpha(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !isAlphaNum(c) && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// validOriginHostPort 判断 host[:port] 部分（可含 IPv6 方括号）是否只含合法字符。
func validOriginHostPort(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlphaNum(c) && c != '.' && c != '-' && c != ':' && c != '[' && c != ']' {
			return false
		}
	}
	return s != ""
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isAlphaNum(c byte) bool { return isAlpha(c) || (c >= '0' && c <= '9') }

// Validate 校验必填项与取值范围，失败时给出可操作的错误信息。
func (c *Config) Validate() error {
	if c.Auth.JWTSecret == "" {
		return fmt.Errorf("MEDDESK_JWT_SECRET 不能为空（生成：openssl rand -base64 48）")
	}
	if len(c.Auth.JWTSecret) < 32 {
		return fmt.Errorf("MEDDESK_JWT_SECRET 长度需 >= 32 字节")
	}
	if c.DB.URL == "" {
		return fmt.Errorf("MEDDESK_DB_URL 不能为空")
	}
	if c.Upload.Dir == "" {
		return fmt.Errorf("MEDDESK_UPLOAD_DIR 不能为空")
	}
	if c.Sync.MaxItemsPerPush <= 0 || c.Sync.PullPageSize <= 0 {
		return fmt.Errorf("同步参数必须为正数")
	}
	return nil
}

// IsDev 返回是否为开发环境。
func (c *Config) IsDev() bool { return c.Env == "dev" || c.Env == "development" }

// AdminWarnings 返回管理后台的**非致命**配置告警。
//
// 之所以不塞进 Validate()：管理员白名单为空只是"后台没人能登录"，
// 不足以让整个健康服务起不来——线上已经有用户在用主功能了。
// 但也不能静默，否则排障时很难把"登不上后台"和"没配白名单"联系起来，
// 所以由 main 在启动后逐条打 WARN。
func (c *Config) AdminWarnings() []string {
	var out []string
	if len(c.Admin.Emails) == 0 {
		out = append(out, "MEDDESK_ADMIN_EMAILS 未配置：管理后台 /admin 将无人可登录（如需启用请填入管理员邮箱，多个用逗号分隔）")
	}
	if c.Admin.PasswordHash == "" || c.Admin.Salt == "" {
		out = append(out, "MEDDESK_ADMIN_PASSWORD_HASH / MEDDESK_ADMIN_SALT 未配置：管理后台无法完成口令校验")
	}
	return out
}

// splitEmails 解析逗号分隔的邮箱白名单，统一小写并去空。
func splitEmails(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 10)
	if err != nil {
		return def
	}
	return f
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// splitList 解析逗号分隔列表，忽略空项。
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
