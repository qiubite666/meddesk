package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/qiubite666/meddesk/internal/model"
)

// 本文件是 B4（凭证附件）独占文件：它给 internal/store/ledger_repo.go 里已存在的
// ledgerRepo 类型**追加方法**，不修改他人文件。
//
// 为什么可以这样做：Go 允许同一类型的方法分散在多个文件里（只要在同包）。这样既
// 满足了"不得修改 ledger_repo.go"的约束，又让凭证仓储复用同一个 pool 句柄。
//
// 【账号隔离】每个方法都把 user_id 作为首个过滤条件写进 SQL：越权在 SQL 层就被挡住，
// 服务层不需要也不能"先查再判断"（否则就要依赖调用方记得传对账号）。

// LedgerAttachmentRepo 是账单凭证附件所需的数据访问能力（由 *Store 实现）。
type LedgerAttachmentRepo interface {
	// Create 写入凭证元数据；ID 为空时由仓储生成，created_at 由数据库回填。
	Create(ctx context.Context, a *model.LedgerAttachment) error
	// Get 按账号 + 附件 id 查询；越权与不存在都返回 ErrNotFound。
	Get(ctx context.Context, userID, id string) (*model.LedgerAttachment, error)
	// ListByRecord 列出某条账单下的全部凭证，按创建时间升序（与上传顺序一致）。
	ListByRecord(ctx context.Context, userID, recordID string) ([]model.LedgerAttachment, error)
	// Delete 删除单条凭证，返回它的相对存储路径供服务层清理磁盘文件。
	Delete(ctx context.Context, userID, id string) (relPath string, err error)
	// DeleteByRecord 删除某条账单下的全部凭证，返回被删的相对路径列表。
	DeleteByRecord(ctx context.Context, userID, recordID string) ([]string, error)
}

// LedgerAttachments 返回凭证附件仓储。
func (s *Store) LedgerAttachments() LedgerAttachmentRepo { return ledgerRepo{s} }

// 编译期断言：ledgerRepo 满足凭证附件仓储接口。
var _ LedgerAttachmentRepo = ledgerRepo{}

// 凭证查询列（顺序与 scanLedgerAttachment 一致）。
// width/height 在表里可为 NULL，因此用 pgtype.Int4 承载。
const (
	selectLedgerAttachment      = `SELECT id, user_id, record_id, rel_path, mime, bytes, width, height, created_at FROM ledger_attachments WHERE user_id = $1 AND id = $2`
	selectLedgerAttachmentByRec = `SELECT id, user_id, record_id, rel_path, mime, bytes, width, height, created_at FROM ledger_attachments WHERE user_id = $1 AND record_id = $2 ORDER BY created_at, id`
)

// Create 写入一条凭证元数据。
func (r ledgerRepo) Create(ctx context.Context, a *model.LedgerAttachment) error {
	if a == nil {
		return wrap("创建凭证失败", errors.New("凭证对象为空"))
	}
	if a.ID == "" {
		a.ID = newUUID()
	}
	id, err := uuidParam(a.ID)
	if err != nil {
		return err
	}
	uid, err := uuidParam(a.UserID)
	if err != nil {
		return err
	}
	rid, err := uuidParam(a.RecordID)
	if err != nil {
		return err
	}
	// mime 由服务层嗅探后写入，数据库还有 CHECK 兜底（只允许三种图片类型）。
	const q = `INSERT INTO ledger_attachments
(id, user_id, record_id, rel_path, mime, bytes, width, height)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`
	if err := r.s.pool.QueryRow(ctx, q, id, uid, rid, a.RelPath, a.Mime, a.Bytes,
		int4Param(a.Width), int4Param(a.Height)).Scan(&a.CreatedAt); err != nil {
		return wrap("创建凭证失败", err)
	}
	return nil
}

// Get 按账号 + 附件 id 查询凭证。
func (r ledgerRepo) Get(ctx context.Context, userID, id string) (*model.LedgerAttachment, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	aid, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	a, err := scanLedgerAttachment(r.s.pool.QueryRow(ctx, selectLedgerAttachment, uid, aid))
	if err != nil {
		return nil, wrap("查询凭证失败", err)
	}
	return a, nil
}

// ListByRecord 列出某条账单下的全部凭证。
func (r ledgerRepo) ListByRecord(ctx context.Context, userID, recordID string) ([]model.LedgerAttachment, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	rid, err := uuidParam(recordID)
	if err != nil {
		return nil, err
	}
	rows, err := r.s.pool.Query(ctx, selectLedgerAttachmentByRec, uid, rid)
	if err != nil {
		return nil, wrap("列出凭证失败", err)
	}
	defer rows.Close()

	out := []model.LedgerAttachment{}
	for rows.Next() {
		a, err := scanLedgerAttachment(rows)
		if err != nil {
			return nil, wrap("列出凭证失败", err)
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("列出凭证失败", err)
	}
	return out, nil
}

// Delete 删除单条凭证并返回其相对路径（供服务层删磁盘文件）。
//
// 用 RETURNING 而不是"先查后删"：少一次往返，也避免并发下查到已被删的行。
func (r ledgerRepo) Delete(ctx context.Context, userID, id string) (string, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return "", err
	}
	aid, err := uuidParam(id)
	if err != nil {
		return "", err
	}
	const q = `DELETE FROM ledger_attachments WHERE user_id = $1 AND id = $2 RETURNING rel_path`
	var rel string
	if err := r.s.pool.QueryRow(ctx, q, uid, aid).Scan(&rel); err != nil {
		return "", wrap("删除凭证失败", notFound(err))
	}
	return rel, nil
}

// DeleteByRecord 删除某条账单下的全部凭证并返回被删的相对路径。
//
// 外键 ON DELETE CASCADE 也会连带删行，但那时拿不到 rel_path，磁盘文件就成了孤儿——
// 所以删除账单前必须由服务层显式调用本方法。
func (r ledgerRepo) DeleteByRecord(ctx context.Context, userID, recordID string) ([]string, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	rid, err := uuidParam(recordID)
	if err != nil {
		return nil, err
	}
	const q = `DELETE FROM ledger_attachments WHERE user_id = $1 AND record_id = $2 RETURNING rel_path`
	rows, err := r.s.pool.Query(ctx, q, uid, rid)
	if err != nil {
		return nil, wrap("清理凭证失败", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, wrap("清理凭证失败", err)
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("清理凭证失败", err)
	}
	return out, nil
}

// scanLedgerAttachment 把一行扫描成 model.LedgerAttachment；未找到时返回 ErrNotFound。
func scanLedgerAttachment(row pgx.Row) (*model.LedgerAttachment, error) {
	var (
		a model.LedgerAttachment
		w pgtype.Int4
		h pgtype.Int4
	)
	if err := row.Scan(&a.ID, &a.UserID, &a.RecordID, &a.RelPath, &a.Mime, &a.Bytes,
		&w, &h, &a.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	if w.Valid {
		a.Width = int(w.Int32)
	}
	if h.Valid {
		a.Height = int(h.Int32)
	}
	return &a, nil
}

// int4Param 把尺寸转成可空整型参数：0 表示"解码失败/未知"，写 NULL 而不是 0，
// 免得前端以为图片真的是 0 像素宽。
func int4Param(v int) pgtype.Int4 {
	if v <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}
