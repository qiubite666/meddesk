package cache

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// sweepInterval 是后台清扫协程的周期。
const sweepInterval = 60 * time.Second

// noopEntry 是进程内缓存的一个条目。expireAt 为零值表示永不过期。
type noopEntry struct {
	value    string
	expireAt time.Time
}

// noopCache 是进程内的 Cache 降级实现：
// map + sync.RWMutex + 惰性过期清理 + 后台 goroutine 每 60s 全量清扫。
// 仅用于 Redis 不可用时的降级，多实例部署下不共享状态（限流等会退化为单实例口径）。
type noopCache struct {
	mu      sync.RWMutex
	items   map[string]noopEntry
	stop    chan struct{}
	closeOn sync.Once
}

// NewNoop 创建进程内缓存。调用方应在进程退出前调用 Close 停止清扫协程。
func NewNoop() Cache {
	c := &noopCache{
		items: make(map[string]noopEntry),
		stop:  make(chan struct{}),
	}
	go c.sweep()
	return c
}

// sweep 每 60s 清扫一次过期键，直到 Close 被调用。
func (n *noopCache) sweep() {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.deleteExpired(time.Now())
		}
	}
}

// deleteExpired 删除所有已过期条目。
func (n *noopCache) deleteExpired(now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, e := range n.items {
		if !e.expireAt.IsZero() && !now.Before(e.expireAt) {
			delete(n.items, k)
		}
	}
}

// cleanExpired 惰性清理：命中已过期条目时顺手删除。
func (n *noopCache) cleanExpired(key string) {
	n.mu.Lock()
	delete(n.items, key)
	n.mu.Unlock()
}

func (n *noopCache) Ping(context.Context) error { return nil }

// Close 停止后台清扫协程，可重复调用。
func (n *noopCache) Close() error {
	n.closeOn.Do(func() { close(n.stop) })
	return nil
}

// Get 读取值；不存在或已过期返回 ErrMiss。
func (n *noopCache) Get(_ context.Context, key string) (string, error) {
	k := full(key)
	now := time.Now()
	n.mu.RLock()
	e, ok := n.items[k]
	n.mu.RUnlock()
	if !ok {
		return "", ErrMiss
	}
	if !e.expireAt.IsZero() && !now.Before(e.expireAt) {
		n.cleanExpired(k)
		return "", ErrMiss
	}
	return e.value, nil
}

// GetInt 读取整数值；未命中返回 ErrMiss，非整数内容返回错误。
func (n *noopCache) GetInt(ctx context.Context, key string) (int64, error) {
	v, err := n.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	return i, nil
}

// Set 写入值；ttl<=0 表示不过期。
func (n *noopCache) Set(_ context.Context, key string, value string, ttl time.Duration) error {
	k := full(key)
	e := noopEntry{value: value}
	if ttl > 0 {
		e.expireAt = time.Now().Add(ttl)
	}
	n.mu.Lock()
	n.items[k] = e
	n.mu.Unlock()
	return nil
}

// SetNX 仅在键不存在（或未过期）时写入，返回是否写入成功。
func (n *noopCache) SetNX(ctx context.Context, key string, value string, ttl time.Duration) (bool, error) {
	k := full(key)
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if e, ok := n.items[k]; ok && (e.expireAt.IsZero() || now.Before(e.expireAt)) {
		return false, nil
	}
	e := noopEntry{value: value}
	if ttl > 0 {
		e.expireAt = now.Add(ttl)
	}
	n.items[k] = e
	return true, nil
}

// Incr 自增计数；首次创建时带上 TTL（ttl<=0 表示不过期）。
func (n *noopCache) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	k := full(key)
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	var next int64
	if e, ok := n.items[k]; ok && (e.expireAt.IsZero() || now.Before(e.expireAt)) {
		if v, err := strconv.ParseInt(e.value, 10, 64); err == nil {
			next = v + 1
		} else {
			next = 1
		}
	} else {
		next = 1
	}
	n.items[k] = noopEntry{value: strconv.FormatInt(next, 10), expireAt: expireAtFrom(now, ttl)}
	return next, nil
}

// Del 批量删除键。
func (n *noopCache) Del(_ context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	n.mu.Lock()
	for _, k := range keys {
		delete(n.items, full(k))
	}
	n.mu.Unlock()
	return nil
}

// expireAtFrom 计算过期时刻；ttl<=0 返回零值（永不过期）。
func expireAtFrom(now time.Time, ttl time.Duration) time.Time {
	if ttl > 0 {
		return now.Add(ttl)
	}
	return time.Time{}
}
