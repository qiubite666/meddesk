package store

// 记账本每日自动备份的仓储（迁移 0005_ledger_backup）。
//
// 【为什么单独一个文件】LedgerRepo 接口（ledger_repo.go）由他人所有、已冻结，
// 备份是后来追加的能力，因此这里另开一个接口 LedgerBackupRepo，并在**已有的
// ledgerRepo 类型**上实现方法（Go 允许在同包的其他文件里继续给同一类型加方法），
// 这样既不改动原文件，也复用了同一个 *Store。
//
// 【安全约定】
//   - user_webdav_credentials 里只有密文与 nonce，本层**永远看不到明文口令**；
//     加解密由上层（internal/ledger/credstore.go）用环境变量中的密钥完成。
//   - ledger_backup_runs.message 只存错误摘要，不得包含凭据、完整 URL 或令牌。

import (
	"context"
	"errors"
	"time"
)

// 备份运行状态。
const (
	BackupRunRunning = "running" // 已开始，尚未出结果（进程被杀时可用于发现中断的备份）
	BackupRunSuccess = "success"
	BackupRunFailed  = "failed"
)

// WebdavCredentialRow 是用户 WebDAV 凭据的**密文**行。
//
// IV 是 4 个 12 字节 GCM nonce 的拼接（共 48 字节），顺序为
// base_url / username / password / path_prefix；拆分与使用见 internal/ledger/credstore.go。
type WebdavCredentialRow struct {
	UserID     string
	BaseURL    []byte // base_url_ct
	Username   []byte // username_ct
	Password   []byte // password_ct
	PathPrefix []byte // path_prefix_ct（空串也会被加密，便于统一处理）
	IV         []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// LedgerBackupRun 是一次备份运行的记录。RunDate 是上海时区的自然日（YYYY-MM-DD）。
type LedgerBackupRun struct {
	ID          string
	UserID      string
	RunDate     string
	Status      string
	RemotePath  string
	SizeBytes   int64
	RecordCount int
	Message     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LedgerBackupRepo 是每日备份所需的数据访问能力（由 *Store 实现）。
type LedgerBackupRepo interface {
	// SaveCredentials 写入或覆盖该账号的凭据密文（一个账号最多一行）。
	SaveCredentials(ctx context.Context, c *WebdavCredentialRow) error
	// GetCredentials 读取凭据密文；未配置返回 ErrNotFound。
	GetCredentials(ctx context.Context, userID string) (*WebdavCredentialRow, error)
	// DeleteCredentials 删除凭据密文；不存在时返回 nil（幂等）。
	DeleteCredentials(ctx context.Context, userID string) error
	// ListCredentialUserIDs 返回全部已配置凭据的账号 id，供调度器遍历。
	ListCredentialUserIDs(ctx context.Context) ([]string, error)
	// UpsertRun 记录一次运行：同一 (user_id, run_date) 只保留一行，重复执行为覆盖。
	// 传入的 run.ID 为空时由仓储生成；created_at/updated_at 由数据库回填。
	UpsertRun(ctx context.Context, run *LedgerBackupRun) error
	// ListRuns 返回该账号最近 limit 次运行记录（按日期倒序）。
	ListRuns(ctx context.Context, userID string, limit int) ([]LedgerBackupRun, error)
}

// LedgerBackup 返回记账本备份仓储。
func (s *Store) LedgerBackup() LedgerBackupRepo { return ledgerRepo{s} }

// ------------------------------------------------------------------ 凭据

const selectCredentials = `SELECT user_id::text, base_url_ct, username_ct, password_ct,
       path_prefix_ct, iv, created_at, updated_at
FROM user_webdav_credentials`

func (r ledgerRepo) SaveCredentials(ctx context.Context, c *WebdavCredentialRow) error {
	if c == nil {
		return wrap("保存 WebDAV 凭据失败", errors.New("凭据对象为空"))
	}
	uid, err := uuidParam(c.UserID)
	if err != nil {
		return err
	}
	// 一个账号一行：重复保存即覆盖（用户换网盘/改口令时直接 PUT 覆盖即可）。
	const q = `INSERT INTO user_webdav_credentials
(user_id, base_url_ct, username_ct, password_ct, path_prefix_ct, iv)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id) DO UPDATE SET
  base_url_ct = EXCLUDED.base_url_ct,
  username_ct = EXCLUDED.username_ct,
  password_ct = EXCLUDED.password_ct,
  path_prefix_ct = EXCLUDED.path_prefix_ct,
  iv = EXCLUDED.iv
RETURNING created_at, updated_at`
	err = r.s.pool.QueryRow(ctx, q, uid, c.BaseURL, c.Username, c.Password, c.PathPrefix, c.IV).
		Scan(&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return wrap("保存 WebDAV 凭据失败", err)
	}
	return nil
}

func (r ledgerRepo) GetCredentials(ctx context.Context, userID string) (*WebdavCredentialRow, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	var c WebdavCredentialRow
	err = r.s.pool.QueryRow(ctx, selectCredentials+" WHERE user_id = $1", uid).
		Scan(&c.UserID, &c.BaseURL, &c.Username, &c.Password, &c.PathPrefix, &c.IV,
			&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (r ledgerRepo) DeleteCredentials(ctx context.Context, userID string) error {
	uid, err := uuidParam(userID)
	if err != nil {
		return err
	}
	// 不存在也返回 nil：删除接口应当是幂等的（用户连点两次不该报错）。
	if _, err := r.s.pool.Exec(ctx, `DELETE FROM user_webdav_credentials WHERE user_id = $1`, uid); err != nil {
		return wrap("删除 WebDAV 凭据失败", err)
	}
	return nil
}

func (r ledgerRepo) ListCredentialUserIDs(ctx context.Context) ([]string, error) {
	rows, err := r.s.pool.Query(ctx,
		`SELECT user_id::text FROM user_webdav_credentials ORDER BY updated_at`)
	if err != nil {
		return nil, wrap("查询已配置备份的账号失败", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrap("读取已配置备份的账号失败", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ 运行记录

func (r ledgerRepo) UpsertRun(ctx context.Context, run *LedgerBackupRun) error {
	if run == nil {
		return wrap("记录备份运行失败", errors.New("运行记录为空"))
	}
	uid, err := uuidParam(run.UserID)
	if err != nil {
		return err
	}
	day, err := parseDate(run.RunDate)
	if err != nil {
		return err
	}
	if run.ID == "" {
		run.ID = newUUID()
	}
	id, err := uuidParam(run.ID)
	if err != nil {
		return err
	}
	// 一天一行：无论上一次是成功还是失败，本次运行都覆盖同一行，
	// 这样"重试"不会产生第二条记录，前端展示的最近运行列表也不会被刷屏。
	const q = `INSERT INTO ledger_backup_runs
(id, user_id, run_date, status, remote_path, size_bytes, record_count, message)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id, run_date) DO UPDATE SET
  status = EXCLUDED.status,
  remote_path = EXCLUDED.remote_path,
  size_bytes = EXCLUDED.size_bytes,
  record_count = EXCLUDED.record_count,
  message = EXCLUDED.message
RETURNING created_at, updated_at`
	err = r.s.pool.QueryRow(ctx, q, id, uid, day, run.Status, run.RemotePath,
		run.SizeBytes, run.RecordCount, run.Message).Scan(&run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return wrap("记录备份运行失败", err)
	}
	return nil
}

func (r ledgerRepo) ListRuns(ctx context.Context, userID string, limit int) ([]LedgerBackupRun, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	const q = `SELECT id::text, user_id::text, run_date::text, status, remote_path,
       size_bytes, record_count, message, created_at, updated_at
FROM ledger_backup_runs WHERE user_id = $1 ORDER BY run_date DESC LIMIT $2`
	rows, err := r.s.pool.Query(ctx, q, uid, limit)
	if err != nil {
		return nil, wrap("查询备份运行记录失败", err)
	}
	defer rows.Close()
	out := []LedgerBackupRun{}
	for rows.Next() {
		var run LedgerBackupRun
		if err := rows.Scan(&run.ID, &run.UserID, &run.RunDate, &run.Status, &run.RemotePath,
			&run.SizeBytes, &run.RecordCount, &run.Message, &run.CreatedAt, &run.UpdatedAt); err != nil {
			return nil, wrap("读取备份运行记录失败", err)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
