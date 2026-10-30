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

// TestNoopIncrFixedWindow 锁死「固定窗口」语义，防止回退成滑动窗口。
//
// 背景（真实故障）：早期实现每次 Incr 都重设 TTL，等价于滑动窗口。只要请求间隔
// 始终小于窗口长度，key 就永不过期、计数只增不减，累计到上限后**永久 429**，
// 且被拒的请求还会把 TTL 续上形成自锁。生产（Redis 关闭、走本实现）表现为
// 「连续操作一会儿之后所有请求都 429，必须停手满一个窗口才恢复」。
//
// 本用例用「间隔 < TTL 的连续自增」来区分两种语义：
//   - 固定窗口：窗口到期后计数归零，序列里会出现 1（重置）
//   - 滑动窗口：TTL 被不断续期，序列单调递增，永远不会回到 1
func TestNoopIncrFixedWindow(t *testing.T) {
	ctx := context.Background()
	c := NewNoop()
	defer c.Close()

	const (
		ttl   = 80 * time.Millisecond
		gap   = 30 * time.Millisecond // 严格小于 ttl：滑动窗口下会被无限续期
		times = 8
	)

	var seq []int64
	for i := 0; i < times; i++ {
		n, err := c.Incr(ctx, "rl:1.2.3.4", ttl)
		if err != nil {
			t.Fatalf("Incr: %v", err)
		}
		seq = append(seq, n)
		time.Sleep(gap)
	}

	// 断言 1：窗口必须重置过——序列里除首个元素外应再次出现 1。
	reset := false
	for _, n := range seq[1:] {
		if n == 1 {
			reset = true
			break
		}
	}
	if !reset {
		t.Fatalf("固定窗口未重置，计数一直累加：seq=%v（滑动窗口症状：TTL 被每次请求续期）", seq)
	}

	// 断言 2：单窗口内的计数不得超过「一个窗口能容纳的请求数」，说明 TTL 没有被续期放大。
	maxPerWindow := int(ttl/gap) + 2
	for i, n := range seq {
		if n > int64(maxPerWindow) {
			t.Fatalf("第 %d 次自增计数 %d 超出单窗口上限 %d，TTL 疑似被刷新：seq=%v",
				i+1, n, maxPerWindow, seq)
		}
	}
}

// TestNoopIncrExpiresToZero 验证窗口彻底过期后从 1 重新计数。
func TestNoopIncrExpiresToZero(t *testing.T) {
	ctx := context.Background()
	c := NewNoop()
	defer c.Close()

	if n, err := c.Incr(ctx, "rl:expire", 40*time.Millisecond); err != nil || n != 1 {
		t.Fatalf("首次 Incr 应为 1: n=%d err=%v", n, err)
	}
	if n, _ := c.Incr(ctx, "rl:expire", 40*time.Millisecond); n != 2 {
		t.Fatalf("窗口内第二次应为 2: n=%d", n)
	}
	time.Sleep(70 * time.Millisecond) // 超过窗口
	if n, _ := c.Incr(ctx, "rl:expire", 40*time.Millisecond); n != 1 {
		t.Fatalf("窗口过期后应重新从 1 开始: n=%d", n)
	}
}
