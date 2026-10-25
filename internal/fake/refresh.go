package fake

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// refreshRepo 是 RefreshRepo 的内存实现。
//
// 与真实实现一致，库中只保存令牌的 sha256（TokenHash 字段），
// 明文只出现在客户端与 HTTP 响应里。
type refreshRepo struct{ r *Repos }

// Create 写入新的刷新令牌记录。
func (t refreshRepo) Create(_ context.Context, token *model.RefreshToken) error {
	if token == nil {
		return store.ErrNotFound
	}
	t.r.mu.Lock()
	defer t.r.mu.Unlock()

	cp := *token
	cp.TokenHash = cloneBytes(token.TokenHash)
	if cp.ID == "" {
		cp.ID = uuid.NewString()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	t.r.refresh[cp.ID] = &cp
	if len(cp.TokenHash) > 0 {
		t.r.refreshHashIdx[hex.EncodeToString(cp.TokenHash)] = cp.ID
	}
	*token = cp
	return nil
}

// GetByID 按主键查询，未找到返回 store.ErrNotFound。
func (t refreshRepo) GetByID(_ context.Context, id string) (*model.RefreshToken, error) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	return t.lookupLocked(id)
}

// GetByHash 按 sha256 哈希查询，未找到返回 store.ErrNotFound。
func (t refreshRepo) GetByHash(_ context.Context, hash []byte) (*model.RefreshToken, error) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	id, ok := t.r.refreshHashIdx[hex.EncodeToString(hash)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return t.lookupLocked(id)
}

// MarkReplaced 标记令牌已被 newID 轮换掉，并同时置 revoked_at。
// 与 A1 的真实实现保持一致（见 auth/service.go 中 Refresh 的判断顺序注释）。
func (t refreshRepo) MarkReplaced(_ context.Context, id, replacedBy string) error {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	got, ok := t.r.refresh[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	got.ReplacedBy = &replacedBy
	got.RevokedAt = &now
	return nil
}

// Revoke 吊销单个令牌；已吊销或不存在均返回 nil（幂等）。
func (t refreshRepo) Revoke(_ context.Context, id string) error {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	got, ok := t.r.refresh[id]
	if !ok || got.RevokedAt != nil {
		return nil
	}
	now := time.Now()
	got.RevokedAt = &now
	return nil
}

// RevokeAllForUser 吊销该用户全部尚未吊销的令牌。
func (t refreshRepo) RevokeAllForUser(_ context.Context, userID string) error {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	now := time.Now()
	for _, got := range t.r.refresh {
		if got.UserID == userID && got.RevokedAt == nil {
			ts := now
			got.RevokedAt = &ts
		}
	}
	return nil
}

// DeleteExpired 物理删除 expires_at 早于 before 的令牌，返回删除行数。
func (t refreshRepo) DeleteExpired(_ context.Context, before time.Time) (int64, error) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	var n int64
	for id, got := range t.r.refresh {
		if !got.ExpiresAt.IsZero() && got.ExpiresAt.Before(before) {
			delete(t.r.refresh, id)
			delete(t.r.refreshHashIdx, hex.EncodeToString(got.TokenHash))
			n++
		}
	}
	return n, nil
}

// lookupLocked 在已持锁的前提下按 id 取一份令牌副本。
func (t refreshRepo) lookupLocked(id string) (*model.RefreshToken, error) {
	got, ok := t.r.refresh[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *got
	cp.TokenHash = cloneBytes(got.TokenHash)
	if got.RevokedAt != nil {
		ts := *got.RevokedAt
		cp.RevokedAt = &ts
	}
	if got.ReplacedBy != nil {
		v := *got.ReplacedBy
		cp.ReplacedBy = &v
	}
	return &cp, nil
}
