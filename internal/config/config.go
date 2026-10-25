// Package config 集中管理 MedDesk 服务端配置。全部来自环境变量（可选 .env 文件）。
package config

import (
	"fmt"
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
			SignupEnabled:     envBool("MEDDESK_SIGNUP_ENABLED", true),
			RefreshReuseGrace: envDuration("MEDDESK_REFRESH_REUSE_GRACE", 30*time.Second),
		},
		Sync: SyncConfig{
			MaxItemsPerPush: envInt("MEDDESK_SYNC_MAX_ITEMS", 500),
			MaxRecordBytes:  envInt64("MEDDESK_SYNC_MAX_RECORD_BYTES", 1<<20), // 1 MiB
			PullPageSize:    envInt("MEDDESK_SYNC_PULL_PAGE", 300),
			MaxPullPageSize: envInt("MEDDESK_SYNC_PULL_PAGE_MAX", 1000),
		},
		Upload: UploadConfig{
			Dir:      envString("MEDDESK_UPLOAD_DIR", "/var/lib/meddesk/uploads"),
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

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

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
