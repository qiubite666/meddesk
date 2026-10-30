package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/qiubite666/meddesk/internal/model"
)

// userRepo 是 UserRepo 的 PostgreSQL 实现。
type userRepo struct{ s *Store }

// 用户查询列（顺序与 scanUser 一致）。
const (
	userColumns       = `id, email, password_hash, kdf_salt, wrapped_dek, dek_iv, envelope_stale, recovery_wrapped_dek, recovery_iv, created_at, updated_at`
	selectUserByID    = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	selectUserByEmail = `SELECT ` + userColumns + ` FROM users WHERE lower(email::text) = lower($1)`
)

// Create 写入新用户。邮箱重复时返回 *apierr.Error（Code=apierr.CodeConflict）。
// ID 为空时由仓储生成；created_at/updated_at 由数据库回填。
func (r *userRepo) Create(ctx context.Context, u *model.User) error {
	if u == nil {
		return wrap("创建用户失败", errors.New("用户对象为空"))
	}
	if u.ID == "" {
		u.ID = newUUID()
	}
	id, err := uuidParam(u.ID)
	if err != nil {
		return err
	}

	const q = `INSERT INTO users (id, email, password_hash, kdf_salt, wrapped_dek, dek_iv)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at`

	err = r.s.pool.QueryRow(ctx, q, id, u.Email, u.PasswordHash, u.KDFSalt, u.WrappedDEK, u.DEKIV).
		Scan(&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return newConflict("该邮箱已被注册")
		}
		return wrap("创建用户失败", err)
	}
	return nil
}

// GetByID 按主键查询用户，未找到返回 ErrNotFound。
func (r *userRepo) GetByID(ctx context.Context, id string) (*model.User, error) {
	p, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	u, err := scanUser(r.s.pool.QueryRow(ctx, selectUserByID, p))
	if err != nil {
		return nil, wrap("查询用户失败", err)
	}
	return u, nil
}

// GetByEmail 按邮箱查询用户（大小写不敏感），未找到返回 ErrNotFound。
// 说明：统一使用 lower(email::text) 比较，使 citext 与 text 两种部署下行为一致，
// 并能命中 users_email_lower_key 唯一索引。
func (r *userRepo) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	u, err := scanUser(r.s.pool.QueryRow(ctx, selectUserByEmail, email))
	if err != nil {
		return nil, wrap("按邮箱查询用户失败", err)
	}
	return u, nil
}

// UpdateCredentials 更新口令哈希与密钥信封（改口令后重新包裹 DEK 时使用）。
// updated_at 由数据库触发器维护；用户不存在时返回 ErrNotFound。
func (r *userRepo) UpdateCredentials(ctx context.Context, id, passwordHash string, kdfSalt, wrappedDEK, dekIV []byte) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	const q = `UPDATE users SET password_hash = $2, kdf_salt = $3, wrapped_dek = $4, dek_iv = $5 WHERE id = $1`
	tag, err := r.s.pool.Exec(ctx, q, p, passwordHash, kdfSalt, wrappedDEK, dekIV)
	if err != nil {
		return wrap("更新用户凭证失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("更新用户凭证失败", ErrNotFound)
	}
	return nil
}

// scanUser 把一行扫描成 model.User，未找到时返回 ErrNotFound。
func scanUser(row pgx.Row) (*model.User, error) {
	var u model.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.KDFSalt, &u.WrappedDEK, &u.DEKIV,
		&u.EnvelopeStale, &u.RecoveryWrappedDEK, &u.RecoveryIV, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

// UpdatePasswordKeepEnvelope 只改口令与盐，**保留原信封**，并标记为待修复。
//
// 这是管理员「直接改口令、不重置数据」的默认路径：
// 服务端没有明文 DEK，无法为新口令重造信封，但它可以把旧信封留住 ——
// 用户下次在仍持有 DEK 的设备上登录时，客户端会用本地 DEK 重新包裹并回填。
func (r *userRepo) UpdatePasswordKeepEnvelope(ctx context.Context, id, passwordHash string, kdfSalt []byte) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	const q = `UPDATE users
SET password_hash = $2, kdf_salt = $3, envelope_stale = true, envelope_stale_at = now()
WHERE id = $1`
	tag, err := r.s.pool.Exec(ctx, q, p, passwordHash, kdfSalt)
	if err != nil {
		return wrap("更新口令失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("更新口令失败", ErrNotFound)
	}
	return nil
}

// RepairEnvelope 用客户端重新包裹好的信封替换旧的，并清除待修复标记。
// 旧信封会留在 wrapped_dek_prev/dek_iv_prev，便于管理员回滚一次。
func (r *userRepo) RepairEnvelope(ctx context.Context, id string, wrappedDEK, dekIV []byte) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	const q = `UPDATE users
SET wrapped_dek_prev = wrapped_dek, dek_iv_prev = dek_iv,
    wrapped_dek = $2, dek_iv = $3,
    envelope_stale = false, envelope_stale_at = NULL
WHERE id = $1`
	tag, err := r.s.pool.Exec(ctx, q, p, wrappedDEK, dekIV)
	if err != nil {
		return wrap("修复密钥信封失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("修复密钥信封失败", ErrNotFound)
	}
	return nil
}

// SetRecoveryEnvelope 写入或清除「托管恢复信封」。wrapped 为空表示关闭托管。
//
// 存的是被服务端恢复公钥包裹的 DEK，服务端不持有私钥时无法解；私钥在
// MEDDESK_RECOVERY_KEY 中，仅管理员改口令路径会用到。
func (r *userRepo) SetRecoveryEnvelope(ctx context.Context, id string, wrapped, iv []byte) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	var w, v any
	if len(wrapped) > 0 {
		w, v = wrapped, iv
	}
	const q = `UPDATE users
SET recovery_wrapped_dek = $2, recovery_iv = $3,
    recovery_at = CASE WHEN $2::bytea IS NULL THEN NULL ELSE now() END
WHERE id = $1`
	tag, err := r.s.pool.Exec(ctx, q, p, w, v)
	if err != nil {
		return wrap("更新托管信封失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("更新托管信封失败", ErrNotFound)
	}
	return nil
}

// 编译期断言：userRepo 实现 UserRepo 接口。
var _ UserRepo = (*userRepo)(nil)
