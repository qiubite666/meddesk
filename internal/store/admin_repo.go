package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// AdminUserRow 是管理后台用户列表的一行（对外 DTO）。
type AdminUserRow struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	RecordCount int64     `json:"recordCount"` // 服务器上的密文记录数（不含明文，仅计数）
	LastSeq     int64     `json:"lastSeq"`     // 同步游标（可粗略反映活跃度）

	// RecoveryEnabled 表示该账号开启了「托管恢复」（存了被服务端恢复公钥包裹的 DEK）。
	// 为 true 时管理员可以直接改口令且数据无损，任何设备都能立刻用新口令登录。
	RecoveryEnabled bool `json:"recoveryEnabled"`
	// EnvelopeStale 表示口令已被改过但信封待持有 DEK 的设备自愈。
	EnvelopeStale bool `json:"envelopeStale"`
}

// AdminListUsers 分页查询用户，q 为空表示不过滤（按邮箱模糊匹配，大小写不敏感）。
//
// 只用一条 SQL + 窗口函数拿总数，避免 count 与 list 两次往返不一致。
func (s *Store) AdminListUsers(ctx context.Context, q string, page, size int) ([]AdminUserRow, int64, error) {
	if page < 1 {
		page = 1
	}
	if size <= 0 || size > 200 {
		size = 20
	}
	const sql = `
WITH matched AS (
    SELECT u.id, u.email, u.created_at, u.updated_at,
           COALESCE(r.n, 0)   AS record_count,
           COALESCE(r.last_seq, 0) AS last_seq,
           (u.recovery_wrapped_dek IS NOT NULL) AS recovery_enabled,
           u.envelope_stale
    FROM users u
    LEFT JOIN LATERAL (
        SELECT count(*) AS n, max(seq) AS last_seq
        FROM sync_records WHERE user_id = u.id
    ) r ON true
    WHERE $1 = '' OR lower(u.email::text) LIKE '%' || lower($1) || '%'
)
SELECT id, email, created_at, updated_at, record_count, last_seq,
       recovery_enabled, envelope_stale, (SELECT count(*) FROM matched)
FROM matched
ORDER BY created_at DESC
LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, sql, q, size, (page-1)*size)
	if err != nil {
		return nil, 0, wrap("查询用户列表失败", err)
	}
	defer rows.Close()

	out := make([]AdminUserRow, 0, size)
	var total int64
	for rows.Next() {
		var r AdminUserRow
		if err := rows.Scan(&r.ID, &r.Email, &r.CreatedAt, &r.UpdatedAt, &r.RecordCount, &r.LastSeq,
			&r.RecoveryEnabled, &r.EnvelopeStale, &total); err != nil {
			return nil, 0, wrap("读取用户行失败", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, wrap("读取用户列表失败", err)
	}
	return out, total, nil
}

// AdminOverview 是后台首屏统计。
type AdminOverview struct {
	Users       int64 `json:"users"`
	Records     int64 `json:"records"`
	ActiveGrants int64 `json:"activeGrants"`
}

// AdminStats 汇总后台首屏数字（一条 SQL 三个子查询）。
func (s *Store) AdminStats(ctx context.Context) (AdminOverview, error) {
	var o AdminOverview
	err := s.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM users),
       (SELECT count(*) FROM sync_records),
       (SELECT count(*) FROM family_grants WHERE status = 'active')`).
		Scan(&o.Users, &o.Records, &o.ActiveGrants)
	if err != nil {
		return o, wrap("统计后台概况失败", err)
	}
	return o, nil
}

// AdminUpdateEmail 修改用户邮箱。邮箱重复时返回冲突错误。
func (s *Store) AdminUpdateEmail(ctx context.Context, id, email string) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, p, email)
	if err != nil {
		if isUniqueViolation(err) {
			return newConflict("该邮箱已被其他账号使用")
		}
		return wrap("修改邮箱失败", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.New(apierr.CodeNotFound, "用户不存在")
	}
	return nil
}

// AdminDeleteUser 删除用户：先取出附件落盘路径（供调用方清理文件），
// 再删除 users 行——refresh_tokens / sync_records / family_grants / attachments
// 都建了 ON DELETE CASCADE，随主行一并消失。
func (s *Store) AdminDeleteUser(ctx context.Context, id string) ([]string, error) {
	p, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, wrap("开启事务失败", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	paths := make([]string, 0, 8)
	rows, err := tx.Query(ctx, `SELECT path FROM attachments WHERE user_id = $1`, p)
	if err != nil {
		return nil, wrap("读取附件路径失败", err)
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return nil, wrap("读取附件路径失败", err)
		}
		paths = append(paths, path)
	}
	rows.Close()

	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, p)
	if err != nil {
		return nil, wrap("删除用户失败", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, apierr.New(apierr.CodeNotFound, "用户不存在")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrap("提交删除失败", err)
	}
	return paths, nil
}

// AdminWipeSync 清空某用户的全部同步密文与游标。
//
// 口令重置后旧 DEK 无法恢复，服务器上的旧密文对新口令而言是永久噪声；
// 不清掉会让客户端拉取到无法解密的记录并反复报错。
func (s *Store) AdminWipeSync(ctx context.Context, id string) error {
	p, err := uuidParam(id)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wrap("开启事务失败", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `DELETE FROM sync_records WHERE user_id = $1`, p); err != nil {
		return wrap("清空同步记录失败", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sync_state WHERE user_id = $1`, p); err != nil {
		return wrap("清空同步游标失败", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return wrap("提交清空失败", err)
	}
	return nil
}

// AdminUserEmail 按 ID 取邮箱（管理员鉴权中间件用来核对白名单）。
func (s *Store) AdminUserEmail(ctx context.Context, id string) (string, error) {
	p, err := uuidParam(id)
	if err != nil {
		return "", err
	}
	var email string
	err = s.pool.QueryRow(ctx, `SELECT email::text FROM users WHERE id = $1`, p).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apierr.New(apierr.CodeNotFound, "用户不存在")
	}
	if err != nil {
		return "", wrap("查询用户邮箱失败", err)
	}
	return email, nil
}
