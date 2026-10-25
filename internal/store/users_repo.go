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
	selectUserByID    = `SELECT id, email, password_hash, kdf_salt, wrapped_dek, dek_iv, created_at, updated_at FROM users WHERE id = $1`
	selectUserByEmail = `SELECT id, email, password_hash, kdf_salt, wrapped_dek, dek_iv, created_at, updated_at FROM users WHERE lower(email::text) = lower($1)`
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
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.KDFSalt, &u.WrappedDEK, &u.DEKIV, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

// 编译期断言：userRepo 实现 UserRepo 接口。
var _ UserRepo = (*userRepo)(nil)
