package fake

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ledgerAttachmentRepo 是 store.LedgerAttachmentRepo 的内存实现，供集成测试使用。
//
// 【为什么数据放在包级表里】
// Repos 结构体（repos.go）与 ledgerStore（ledger.go）都归 lead 所有、不允许修改，
// 因此这里无法给 Repos 增加字段。退而求其次：以 *Repos 指针为键，在包级表里登记
// 一份"影子数据"——每个 Repos 实例的数据仍然互相隔离，语义与真实仓储一致。
// 代价是条目只在进程退出时释放（测试进程生命周期内可忽略）。
// 建议修改点：后续若允许改 repos.go，应把这份 map 直接加进 Repos 结构体。
type ledgerAttachmentRepo struct{ r *Repos }

// 附件数据直接挂在 Repos 上（由 lead 补字段），与 fake 包其他仓储一致：
// 只加 Repos.mu 一把锁，没有第二条锁，也就不存在锁序问题。

// LedgerAttachments 返回凭证附件仓储（内存实现）。
func (r *Repos) LedgerAttachments() store.LedgerAttachmentRepo { return ledgerAttachmentRepo{r} }

var _ store.LedgerAttachmentRepo = ledgerAttachmentRepo{}

// table 取出（必要时创建）该 Repos 实例对应的附件表。调用方需持有 Repos.mu。
func (r ledgerAttachmentRepo) table() map[string]*model.LedgerAttachment {
	if r.r.ledgerAttachments == nil {
		r.r.ledgerAttachments = make(map[string]*model.LedgerAttachment)
	}
	return r.r.ledgerAttachments
}

func (r ledgerAttachmentRepo) Create(ctx context.Context, a *model.LedgerAttachment) error {
	m := r.table()

	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	if a == nil {
		return errors.New("凭证对象为空")
	}
	if a.ID == "" {
		a.ID = newUUIDForTest()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	cp := *a
	m[a.ID] = &cp
	return nil
}

func (r ledgerAttachmentRepo) Get(ctx context.Context, userID, id string) (*model.LedgerAttachment, error) {
	m := r.table()

	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	a, ok := m[id]
	// 越权与不存在都返回 ErrNotFound，与 SQL 版（WHERE user_id = $1）保持一致。
	if !ok || a.UserID != userID {
		return nil, store.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (r ledgerAttachmentRepo) ListByRecord(ctx context.Context, userID, recordID string) ([]model.LedgerAttachment, error) {
	m := r.table()

	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	out := []model.LedgerAttachment{}
	for _, a := range m {
		if a.UserID != userID || a.RecordID != recordID {
			continue
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (r ledgerAttachmentRepo) Delete(ctx context.Context, userID, id string) (string, error) {
	m := r.table()

	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	a, ok := m[id]
	if !ok || a.UserID != userID {
		return "", store.ErrNotFound
	}
	delete(m, id)
	return a.RelPath, nil
}

func (r ledgerAttachmentRepo) DeleteByRecord(ctx context.Context, userID, recordID string) ([]string, error) {
	m := r.table()

	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	out := []string{}
	for id, a := range m {
		if a.UserID != userID || a.RecordID != recordID {
			continue
		}
		out = append(out, a.RelPath)
		delete(m, id)
	}
	sort.Strings(out)
	return out, nil
}
