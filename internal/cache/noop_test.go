package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNoopTTLExpire 验证进程内缓存的 TTL 过期（惰性清理）与 Incr 首次设置 TTL。
func TestNoopTTLExpire(t *testing.T) {
	ctx := context.Background()
	c := NewNoop()
	defer c.Close()

	if err := c.Set(ctx, "k", "v", 80*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := c.Get(ctx, "k"); err != nil || got != "v" {
		t.Fatalf("过期前应可读: got=%q err=%v", got, err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("过期后应返回 ErrMiss: %v", err)
	}

	// Incr：首次自增为 1，带 TTL；TTL 内继续累加。
	for i := int64(1); i <= 3; i++ {
		n, err := c.Incr(ctx, "counter", time.Second)
		if err != nil || n != i {
			t.Fatalf("Incr 第 %d 次: got=%d err=%v", i, n, err)
		}
	}
	if err := c.Del(ctx, "counter"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := c.GetInt(ctx, "counter"); !errors.Is(err, ErrMiss) {
		t.Fatalf("删除后应返回 ErrMiss: %v", err)
	}

	// 永不过期条目不应被清扫。
	_ = c.Set(ctx, "forever", "1", 0)
	time.Sleep(20 * time.Millisecond)
	if _, err := c.Get(ctx, "forever"); err != nil {
		t.Fatalf("ttl<=0 应永不过期: %v", err)
	}
}
