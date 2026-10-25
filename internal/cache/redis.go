package cache

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/qiubite666/meddesk/internal/config"
)

// incrLua 原子自增并设置过期时间。
// 说明：不用 SetNX + Incr 两条命令，是因为两者之间存在竞态——若键恰好在
// SetNX 返回 false 之后、Incr 之前过期，Incr 会重建一个永不过期的键。
// Lua 脚本在 Redis 中单线程执行，可保证「自增」与「首次设置 TTL」是原子的。
const incrLua = `
local cur = redis.call('INCR', KEYS[1])
if tonumber(ARGV[1]) > 0 then
  if cur == 1 then
    redis.call('PEXPIRE', KEYS[1], ARGV[1])
  end
else
  redis.call('PERSIST', KEYS[1])
end
return cur
`

// redisCache 是基于 go-redis/v9 的 Cache 实现。
type redisCache struct {
	client *redis.Client
	incr   *redis.Script
}

// Open 根据配置创建缓存实例。
// Enabled==false 或 URL 为空时返回进程内降级实现；Redis 连接失败同样降级，
// 仅打印 warn 日志并返回 nil error（缓存不是强依赖，不能拖垮启动流程）。
func Open(ctx context.Context, cfg config.RedisConfig) (Cache, error) {
	if !cfg.Enabled || strings.TrimSpace(cfg.URL) == "" {
		slog.Default().Warn("缓存已禁用或 Redis 地址为空，使用进程内降级实现",
			"enabled", cfg.Enabled)
		return NewNoop(), nil
	}

	opt, err := redis.ParseURL(cfg.URL)
	if err != nil {
		slog.Default().Warn("Redis 连接串解析失败，使用进程内降级实现", "url", cfg.URL, "error", err)
		return NewNoop(), nil
	}
	// 配置中的显式密码 / 库号优先于 URL 中的值。
	if cfg.Password != "" {
		opt.Password = cfg.Password
	}
	if cfg.DB != 0 {
		opt.DB = cfg.DB
	}
	// 生产环境不要让一次抖动拖垮整个请求链路。
	opt.DialTimeout = 3 * time.Second
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = 2 * time.Second
	}
	if opt.WriteTimeout == 0 {
		opt.WriteTimeout = 2 * time.Second
	}

	client := redis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Default().Warn("Redis 连接失败，使用进程内降级实现", "url", cfg.URL, "error", err)
		_ = client.Close()
		return NewNoop(), nil
	}
	return &redisCache{client: client, incr: redis.NewScript(incrLua)}, nil
}

func (r *redisCache) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func (r *redisCache) Close() error { return r.client.Close() }

// Get 读取字符串；键不存在时统一转换为 ErrMiss。
func (r *redisCache) Get(ctx context.Context, key string) (string, error) {
	v, err := r.client.Get(ctx, full(key)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrMiss
		}
		return "", err
	}
	return v, nil
}

// GetInt 读取整数；未命中返回 ErrMiss。
func (r *redisCache) GetInt(ctx context.Context, key string) (int64, error) {
	v, err := r.client.Get(ctx, full(key)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, ErrMiss
		}
		return 0, err
	}
	return v, nil
}

// Set 写入字符串值。
func (r *redisCache) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	return r.client.Set(ctx, full(key), value, ttl).Err()
}

// SetNX 仅在键不存在时写入，返回是否写入成功。
func (r *redisCache) SetNX(ctx context.Context, key string, value string, ttl time.Duration) (bool, error) {
	return r.client.SetNX(ctx, full(key), value, ttl).Result()
}

// Incr 原子自增，首次创建（结果==1）时设置 TTL。ttl<=0 表示不过期。
func (r *redisCache) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	ms := int64(0)
	if ttl > 0 {
		ms = ttl.Milliseconds()
		if ms <= 0 {
			ms = 1 // 亚毫秒 TTL 退化为 1ms，避免 PEXPIRE 被当成 0 而立即过期
		}
	}
	return r.incr.Run(ctx, r.client, []string{full(key)}, ms).Int64()
}

// Del 批量删除键。
func (r *redisCache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return r.client.Del(ctx, fullAll(keys)...).Err()
}
