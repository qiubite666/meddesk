package fake

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// userRepo 是 UserRepo 的内存实现。
type userRepo struct{ r *Repos }

// Create 写入新用户。邮箱重复（大小写不敏感，模拟 citext）时返回
// *apierr.Error{Code: apierr.CodeConflict}，与真实仓储一致，
// 这样上层的 auth.Service 才能把它翻译成 HTTP 409。
func (u userRepo) Create(_ context.Context, user *model.User) error {
	if user == nil {
		return apierr.New(apierr.CodeBadRequest, "用户不能为空")
	}
	email := normalizeKey(user.Email)
	if email == "" {
		return apierr.New(apierr.CodeBadRequest, "邮箱不能为空")
	}

	u.r.mu.Lock()
	defer u.r.mu.Unlock()

	if _, ok := u.r.emailIdx[email]; ok {
		return apierr.New(apierr.CodeConflict, "该邮箱已注册")
	}
	cp := *user
	if cp.ID == "" {
		cp.ID = uuid.NewString()
	}
	now := time.Now()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = now
	}
	u.r.users[cp.ID] = &cp
	u.r.emailIdx[email] = cp.ID
	// 回填调用方：真实仓储由数据库 RETURNING 回填。
	*user = cp
	return nil
}

// GetByID 按主键查询，未找到返回 store.ErrNotFound。
func (u userRepo) GetByID(_ context.Context, id string) (*model.User, error) {
	u.r.mu.Lock()
	defer u.r.mu.Unlock()
	got, ok := u.r.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *got
	return &cp, nil
}

// GetByEmail 按邮箱查询（大小写不敏感），未找到返回 store.ErrNotFound。
func (u userRepo) GetByEmail(_ context.Context, email string) (*model.User, error) {
	u.r.mu.Lock()
	defer u.r.mu.Unlock()
	id, ok := u.r.emailIdx[normalizeKey(email)]
	if !ok {
		return nil, store.ErrNotFound
	}
	got := u.r.users[id]
	cp := *got
	return &cp, nil
}

// UpdateCredentials 更新口令哈希与密钥信封；用户不存在返回 store.ErrNotFound。
func (u userRepo) UpdateCredentials(_ context.Context, id, passwordHash string, kdfSalt, wrappedDEK, dekIV []byte) error {
	u.r.mu.Lock()
	defer u.r.mu.Unlock()
	got, ok := u.r.users[id]
	if !ok {
		return store.ErrNotFound
	}
	got.PasswordHash = passwordHash
	got.KDFSalt = cloneBytes(kdfSalt)
	got.WrappedDEK = cloneBytes(wrappedDEK)
	got.DEKIV = cloneBytes(dekIV)
	got.UpdatedAt = time.Now()
	return nil
}

// normalizeKey 邮箱归一化：去空白 + 转小写，等价于 PG 的 citext 比较语义。
func normalizeKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// cloneBytes 拷贝字节切片，避免调用方后续修改影响仓储内部状态。
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
