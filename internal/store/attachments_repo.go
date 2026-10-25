package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/qiubite666/meddesk/internal/model"
)

// attachmentRepo 是 AttachmentRepo 的 PostgreSQL 实现。
type attachmentRepo struct{ s *Store }

// 附件查询列（顺序与 scanAttachment 一致）。
const (
	selectAttachment       = `SELECT id, user_id, record_id, iv, size, mime, path, seq, created_at FROM attachments WHERE user_id = $1 AND id = $2`
	selectAttachmentByUser = `SELECT id, user_id, record_id, iv, size, mime, path, seq, created_at FROM attachments WHERE user_id = $1 ORDER BY created_at DESC, id DESC`
	selectAttachmentByRec  = `SELECT id, user_id, record_id, iv, size, mime, path, seq, created_at FROM attachments WHERE user_id = $1 AND record_id = $2 ORDER BY created_at DESC, id DESC`
)

// Create 写入附件元数据（内容本身由上层以密文落盘，本层只记路径）。
// ID 为空时由仓储生成；created_at 由数据库回填。
func (r *attachmentRepo) Create(ctx context.Context, a *model.Attachment) error {
	if a == nil {
		return wrap("创建附件失败", errors.New("附件对象为空"))
	}
	if a.ID == "" {
		a.ID = newUUID()
	}
	id, err := uuidParam(a.ID)
	if err != nil {
		return err
	}
	userID, err := uuidParam(a.UserID)
	if err != nil {
		return err
	}

	const q = `INSERT INTO attachments (id, user_id, record_id, iv, size, mime, path, seq)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at`
	err = r.s.pool.QueryRow(ctx, q, id, userID, nullText(a.RecordID), a.IV, a.Size, a.Mime, a.Path, a.Seq).
		Scan(&a.CreatedAt)
	if err != nil {
		return wrap("创建附件失败", err)
	}
	return nil
}

// Get 按用户与附件 id 查询，未找到返回 ErrNotFound。
func (r *attachmentRepo) Get(ctx context.Context, userID, id string) (*model.Attachment, error) {
	userParam, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	idParam, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	a, err := scanAttachment(r.s.pool.QueryRow(ctx, selectAttachment, userParam, idParam))
	if err != nil {
		return nil, wrap("查询附件失败", err)
	}
	return a, nil
}

// Delete 删除附件元数据，未找到返回 ErrNotFound，成功返回 nil。
// 注意：密文文件的删除由上层（attachments 服务）负责，本方法只删元数据。
func (r *attachmentRepo) Delete(ctx context.Context, userID, id string) error {
	userParam, err := uuidParam(userID)
	if err != nil {
		return err
	}
	idParam, err := uuidParam(id)
	if err != nil {
		return err
	}
	const q = `DELETE FROM attachments WHERE user_id = $1 AND id = $2`
	tag, err := r.s.pool.Exec(ctx, q, userParam, idParam)
	if err != nil {
		return wrap("删除附件失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("删除附件失败", ErrNotFound)
	}
	return nil
}

// List 列出附件；recordID 为空表示列出该用户全部附件。按创建时间倒序。
func (r *attachmentRepo) List(ctx context.Context, userID, recordID string) ([]model.Attachment, error) {
	userParam, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	var rows pgx.Rows
	if recordID == "" {
		rows, err = r.s.pool.Query(ctx, selectAttachmentByUser, userParam)
	} else {
		rows, err = r.s.pool.Query(ctx, selectAttachmentByRec, userParam, recordID)
	}
	if err != nil {
		return nil, wrap("列出附件失败", err)
	}
	defer rows.Close()

	list := make([]model.Attachment, 0)
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, wrap("列出附件失败", err)
		}
		list = append(list, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("列出附件失败", err)
	}
	return list, nil
}

// scanAttachment 把一行扫描成 model.Attachment，未找到时返回 ErrNotFound。
func scanAttachment(row pgx.Row) (*model.Attachment, error) {
	var (
		a        model.Attachment
		recordID pgtype.Text
	)
	err := row.Scan(&a.ID, &a.UserID, &recordID, &a.IV, &a.Size, &a.Mime, &a.Path, &a.Seq, &a.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	a.RecordID = recordID.String
	return &a, nil
}

// 编译期断言：attachmentRepo 实现 AttachmentRepo 接口。
var _ AttachmentRepo = (*attachmentRepo)(nil)
