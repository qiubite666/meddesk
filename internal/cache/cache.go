// Package cache 提供统一的缓存抽象：Redis 实现 + 进程内降级实现（Noop）。
// 所有键名在包内统一加 meddesk: 前缀，调用方只需传业务键（如 "rl:1.2.3.4"）。
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss 表示键不存在（缓存未命中）。调用方应显式区分「未命中」与「后端故障」。
var ErrMiss = errors.New("cache: miss")

// KeyPrefix 是本服务在 Redis / 进程内缓存中的统一键前缀。
const KeyPrefix = "meddesk:"

// Cache 是缓存抽象。Redis 不可用时应降级为进程内实现而不是让请求失败。
type Cache interface {
	// Ping 探测后端可用性。
	Ping(ctx context.Context) error
	// Close 释放资源，幂等。
	Close() error
	// Get 读取字符串值；未命中返回 ErrMiss。
	Get(ctx context.Context, key string) (string, error)
	// GetInt 读取整数值；未命中返回 ErrMiss，非整数内容返回错误。
	GetInt(ctx context.Context, key string) (int64, error)
	// Set 写入字符串值，ttl<=0 表示不过期。
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
	// SetNX 仅在键不存在时写入，返回是否写入成功。
	SetNX(ctx context.Context, key string, value string, ttl time.Duration) (bool, error)
	// Incr 自增计数；首次创建时带上 ttl（ttl<=0 表示不过期）。
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// Del 删除一个或多个键。
	Del(ctx context.Context, keys ...string) error
}

// full 把业务键拼成带前缀的真实键。
func full(key string) string {
	if len(key) > len(KeyPrefix) && key[:len(KeyPrefix)] == KeyPrefix {
		return key // 已带前缀，避免重复
	}
	return KeyPrefix + key
}

// fullAll 批量拼接键名。
func fullAll(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = full(k)
	}
	return out
}
