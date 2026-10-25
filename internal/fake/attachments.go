package fake

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// attachmentRepo 是 AttachmentRepo 的内存实现。
//
// 关于「内容放哪儿」的取舍说明：
//   - 真实实现（A1）只在库里存元数据（含相对路径 path），密文字节由 attachments.Service
//     负责真实落盘到 {Upload.Dir}/{userID}/{yy}/{id}.bin；
//   - 因此本 fake 也**只存元数据**（map），内容沿用生产路径：集成测试把
//     config.UploadConfig.Dir 指向 t.TempDir()，由 Service 真实写盘/读盘，
//     这样下载接口的字节一致性校验才有意义（而不是自欺欺人地回读内存 buffer）。
type attachmentRepo struct{ r *Repos }

// Create 写入附件元数据；id 为空时生成 UUID，创建时间回填。
func (a attachmentRepo) Create(_ context.Context, att *model.Attachment) error {
	if att == nil {
		return store.ErrNotFound
	}
	a.r.mu.Lock()
	defer a.r.mu.Unlock()

	cp := *att
	cp.IV = cloneBytes(att.IV)
	if cp.ID == "" {
		cp.ID = uuid.NewString()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	bucket, ok := a.r.attachments[cp.UserID]
	if !ok {
		bucket = make(map[string]*model.Attachment)
		a.r.attachments[cp.UserID] = bucket
	}
	bucket[cp.ID] = &cp
	*att = cp
	return nil
}

// Get 按用户与附件 id 查询，未找到返回 store.ErrNotFound。
func (a attachmentRepo) Get(_ context.Context, userID, id string) (*model.Attachment, error) {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	got, ok := a.r.attachments[userID][id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *got
	cp.IV = cloneBytes(got.IV)
	return &cp, nil
}

// Delete 删除附件元数据，未找到返回 store.ErrNotFound，成功返回 nil。
func (a attachmentRepo) Delete(_ context.Context, userID, id string) error {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	bucket, ok := a.r.attachments[userID]
	if !ok {
		return store.ErrNotFound
	}
	if _, ok := bucket[id]; !ok {
		return store.ErrNotFound
	}
	delete(bucket, id)
	return nil
}

// List 列出附件；recordID 为空表示列出该用户全部附件，按创建时间倒序
// （与真实实现的 ORDER BY created_at DESC 保持一致）。
func (a attachmentRepo) List(_ context.Context, userID, recordID string) ([]model.Attachment, error) {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	out := make([]model.Attachment, 0)
	for _, got := range a.r.attachments[userID] {
		if recordID != "" && got.RecordID != recordID {
			continue
		}
		cp := *got
		cp.IV = cloneBytes(got.IV)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
