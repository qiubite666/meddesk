package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/qiubite666/meddesk/internal/model"
)

// refreshRepo 是 RefreshRepo 的 PostgreSQL 实现。
type refreshRepo struct{ s *Store }

// 刷新令牌查询列（顺序与 scanRefresh 一致）。
const (
	selectRefreshByID   = `SELECT id, user_id, token_hash, device_id, user_agent, ip, expires_at, created_at, revoked_at, replaced_by FROM refresh_tokens WHERE id = $1`
	selectRefreshByHash = `SELECT id, user_id, token_hash, device_id, user_agent, ip, expires_at, created_at, revoked_at, replaced_by FROM refresh_tokens WHERE token_hash = $1`
)

// Create 写入刷新令牌记录（库中只保存令牌原文的 sha256，绝不落明文）。
// ID 为空时由仓储生成；created_at 由数据库回填。
func (r *refreshRepo) Create(ctx context.Context, t *model.RefreshToken) error {
	if t == nil {
		return wrap("创建刷新令牌失败", errors.New("令牌对象为空"))
	}
	if t.ID == "" {
		t.ID = newUUID()
	}
	id, err := uuidParam(t.ID)
	if err != nil {
		return err
	}
	userID, err := uuidParam(t.UserID)
	if err != nil {
		return err
	}
	replacedBy, err := uuidPtrParam(t.ReplacedBy)
	if err != nil {
		return err
	}

	const q = `INSERT INTO refresh_tokens
    (id, user_id, token_hash, device_id, user_agent, ip, expires_at, revoked_at, replaced_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING created_at`

	err = r.s.pool.QueryRow(ctx, q,
		id, userID, t.TokenHash, nullText(t.DeviceID), nullText(t.UserAgent), nullText(t.IP),
		t.ExpiresAt, timePtrParam(t.RevokedAt), replacedBy,
	).Scan(&t.CreatedAt)
	if err != nil {
		return wrap("创建刷新令牌失败", err)
	}
	return nil
}

// GetByID 按主键查询刷新令牌，未找到返回 ErrNotFound。
func (r *refreshRepo) GetByID(ctx context.Context, id string) (*model.RefreshToken, error) {
	p, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	t, err := scanRefresh(r.s.pool.QueryRow(ctx, selectRefreshByID, p))
	if err != nil {
		return nil, wrap("查询刷新令牌失败", err)
	}
	return t, nil
}

// GetByHash 按令牌哈希查询，未找到返回 ErrNotFound。
func (r *refreshRepo) GetByHash(ctx context.Context, hash []byte) (*model.RefreshToken, error) {
	t, err := scanRefresh(r.s.pool.QueryRow(ctx, selectRefreshByHash, hash))
	if err != nil {
		return nil, wrap("按哈希查询刷新令牌失败", err)
	}
	return t, nil
}

// MarkReplaced 标记令牌已被 replacedBy 轮换掉，同时置 revoked_at（轮换即吊销）。
// 令牌不存在时返回 ErrNotFound。
func (r *refreshRepo) MarkReplaced(ctx context.Context, id, replacedBy string) error {
	idParam, err := uuidParam(id)
	if err != nil {
		return err
	}
	newParam, err := uuidParam(replacedBy)
	if err != nil {
		return err
	}
	const q = `UPDATE refresh_tokens SET replaced_by = $2, revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	tag, err := r.s.pool.Exec(ctx, q, idParam, newParam)
	if err != nil {
		return wrap("标记令牌轮换失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("标记令牌轮换失败", ErrNotFound)
	}
	return nil
}

// Revoke 吊销单个刷新令牌。令牌不存在或已吊销时返回 nil（幂等，便于登出流程重复调用）。
func (r *refreshRepo) Revoke(ctx context.Context, id string) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	if _, err := r.s.pool.Exec(ctx, q, p); err != nil {
		return wrap("吊销刷新令牌失败", err)
	}
	return nil
}

// RevokeAllForUser 吊销该用户的全部未吊销令牌。无令牌时返回 nil（幂等）。
func (r *refreshRepo) RevokeAllForUser(ctx context.Context, userID string) error {
	p, err := uuidParam(userID)
	if err != nil {
		return err
	}
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	if _, err := r.s.pool.Exec(ctx, q, p); err != nil {
		return wrap("吊销用户全部刷新令牌失败", err)
	}
	return nil
}

// DeleteExpired 物理删除 expires_at 早于 before 的令牌，返回删除行数。
func (r *refreshRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	const q = `DELETE FROM refresh_tokens WHERE expires_at < $1`
	tag, err := r.s.pool.Exec(ctx, q, before)
	if err != nil {
		return 0, wrap("清理过期刷新令牌失败", err)
	}
	return tag.RowsAffected(), nil
}

// scanRefresh 把一行扫描成 model.RefreshToken，未找到时返回 ErrNotFound。
func scanRefresh(row pgx.Row) (*model.RefreshToken, error) {
	var (
		t        model.RefreshToken
		deviceID pgtype.Text
		ua       pgtype.Text
		ip       pgtype.Text
		revoked  pgtype.Timestamptz
		replaced pgtype.UUID
	)
	err := row.Scan(&t.ID, &t.UserID, &t.TokenHash, &deviceID, &ua, &ip, &t.ExpiresAt, &t.CreatedAt, &revoked, &replaced)
	if err != nil {
		return nil, notFound(err)
	}
	t.DeviceID = deviceID.String
	t.UserAgent = ua.String
	t.IP = ip.String
	if revoked.Valid {
		ts := revoked.Time
		t.RevokedAt = &ts
	}
	if replaced.Valid {
		id := replaced.String()
		t.ReplacedBy = &id
	}
	return &t, nil
}

// nullText 把空字符串转成 NULL，避免把「未采集」写成空串。
func nullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// 编译期断言：refreshRepo 实现 RefreshRepo 接口。
var _ RefreshRepo = (*refreshRepo)(nil)
