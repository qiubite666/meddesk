package auth

import (
	"bytes"
	"context"
	"strconv"
	"time"

	"github.com/qiubite666/meddesk/internal/cache"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ---------------------------------------------------------------- 测试替身

// fakeUsers 是 store.UserRepo 的内存实现。
type fakeUsers struct {
	byID    map[string]*model.User
	byEmail map[string]*model.User
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byID: map[string]*model.User{}, byEmail: map[string]*model.User{}}
}

func (f *fakeUsers) Create(_ context.Context, u *model.User) error {
	if _, ok := f.byEmail[u.Email]; ok {
		return store.ErrConflict
	}
	f.byID[u.ID], f.byEmail[u.Email] = u, u
	return nil
}

func (f *fakeUsers) GetByID(_ context.Context, id string) (*model.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*model.User, error) {
	if u, ok := f.byEmail[email]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeUsers) UpdateCredentials(_ context.Context, id, passwordHash string, kdfSalt, wrappedDEK, dekIV []byte) error {
	u, ok := f.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash, u.KDFSalt, u.WrappedDEK, u.DEKIV = passwordHash, kdfSalt, wrappedDEK, dekIV
	return nil
}

// fakeRefresh 是 store.RefreshRepo 的内存实现。
type fakeRefresh struct {
	byID       map[string]*model.RefreshToken
	revokeAll  int
	revokeCall int
}

func newFakeRefresh() *fakeRefresh {
	return &fakeRefresh{byID: map[string]*model.RefreshToken{}}
}

func (f *fakeRefresh) Create(_ context.Context, t *model.RefreshToken) error {
	f.byID[t.ID] = t
	return nil
}
func (f *fakeRefresh) GetByID(_ context.Context, id string) (*model.RefreshToken, error) {
	if t, ok := f.byID[id]; ok {
		return t, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeRefresh) GetByHash(_ context.Context, hash []byte) (*model.RefreshToken, error) {
	for _, t := range f.byID {
		if bytes.Equal(t.TokenHash, hash) {
			return t, nil
		}
	}
	return nil, store.ErrNotFound
}
func (f *fakeRefresh) MarkReplaced(_ context.Context, id, replacedBy string) error {
	t, ok := f.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	// 与 A1 行为一致：标记被轮换的同时置 revoked_at。
	now := time.Now()
	t.ReplacedBy, t.RevokedAt = &replacedBy, &now
	return nil
}
func (f *fakeRefresh) Revoke(_ context.Context, id string) error {
	if t, ok := f.byID[id]; ok {
		now := time.Now()
		t.RevokedAt = &now
		f.revokeCall++
	}
	return nil
}
// RevokeAllForUser 忠实复刻真实仓储语义：吊销该用户全部未吊销令牌，
// 使后续 Refresh 走到 ReplacedBy/RevokedAt 分支。仅计数会让上层行为的
// 断言失真（改密后旧令牌看似仍能续期），故此处必须真实改写状态。
func (f *fakeRefresh) RevokeAllForUser(_ context.Context, userID string) error {
	f.revokeAll++
	now := time.Now()
	for _, t := range f.byID {
		if t.UserID == userID && t.RevokedAt == nil {
			t.RevokedAt = &now
		}
	}
	return nil
}
func (f *fakeRefresh) DeleteExpired(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// fakeCache 是 cache.Cache 的内存实现（忽略 TTL）。
type fakeCache struct{ m map[string]string }

func newFakeCache() *fakeCache { return &fakeCache{m: map[string]string{}} }

func (f *fakeCache) Ping(context.Context) error { return nil }
func (f *fakeCache) Close() error               { return nil }
func (f *fakeCache) Get(_ context.Context, key string) (string, error) {
	if v, ok := f.m[key]; ok {
		return v, nil
	}
	return "", cache.ErrMiss
}
func (f *fakeCache) GetInt(ctx context.Context, key string) (int64, error) {
	v, err := f.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}
func (f *fakeCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	f.m[key] = value
	return nil
}
func (f *fakeCache) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	if _, ok := f.m[key]; ok {
		return false, nil
	}
	f.m[key] = value
	return true, nil
}
func (f *fakeCache) Incr(_ context.Context, key string, _ time.Duration) (int64, error) {
	n, _ := strconv.ParseInt(f.m[key], 10, 64)
	n++
	f.m[key] = strconv.FormatInt(n, 10)
	return n, nil
}
func (f *fakeCache) Del(_ context.Context, keys ...string) error {
	for _, k := range keys {
		delete(f.m, k)
	}
	return nil
}
