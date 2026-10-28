package family

import (
	"context"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ------------------------------------------------------------------ 测试替身

// stubFamilyRepo 只实现 AppendRecords 会用到的方法，其余留空。
// 故意不做「全能内存实现」：替身越像真实仓储，测试越容易掩盖接线错误。
type stubFamilyRepo struct {
	grant *store.FamilyGrant
	err   error
}

func (r *stubFamilyRepo) Create(context.Context, *store.FamilyGrant) error { return nil }
func (r *stubFamilyRepo) GetByID(context.Context, string) (*store.FamilyGrant, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.grant == nil {
		return nil, store.ErrNotFound
	}
	return r.grant, nil
}
func (r *stubFamilyRepo) GetByLookup(context.Context, []byte) (*store.FamilyGrant, error) {
	return nil, store.ErrNotFound
}
func (r *stubFamilyRepo) Redeem(context.Context, []byte, string) (*store.FamilyGrant, error) {
	return nil, store.ErrNotFound
}
func (r *stubFamilyRepo) ListByOwner(context.Context, string) ([]store.FamilyGrant, error) {
	return nil, nil
}
func (r *stubFamilyRepo) ListActiveForViewer(context.Context, string) ([]store.FamilyGrant, error) {
	return nil, nil
}
func (r *stubFamilyRepo) Revoke(context.Context, string, string) error { return nil }
func (r *stubFamilyRepo) ListOwnerRecords(context.Context, string, int64, int) ([]store.OwnerRecord, int64, bool, error) {
	return nil, 0, false, nil
}

// stubSyncRepo 记录 Apply 收到的是「谁的」账号空间 —— 这是本功能的命门。
type stubSyncRepo struct {
	calls     int
	lastUser  string
	lastItems []model.SyncItem
}

func (r *stubSyncRepo) Apply(_ context.Context, userID string, items []model.SyncItem) ([]model.SyncItem, []model.SyncItem, int64, error) {
	r.calls++
	r.lastUser = userID
	r.lastItems = items
	return items, nil, 42, nil
}
func (r *stubSyncRepo) Pull(context.Context, string, int64, int) ([]model.SyncItem, int64, bool, error) {
	return nil, 0, false, nil
}
func (r *stubSyncRepo) Cursor(context.Context, string) (int64, error) { return 0, nil }
func (r *stubSyncRepo) Stats(context.Context, string) (model.SyncStatsResponse, error) {
	return model.SyncStatsResponse{}, nil
}
func (r *stubSyncRepo) Reset(context.Context, string) error { return nil }

// ------------------------------------------------------------------ 辅助

func activeGrant(owner, viewer string) *store.FamilyGrant {
	v := viewer
	return &store.FamilyGrant{
		ID:       "grant-1",
		OwnerID:  owner,
		ViewerID: &v,
		Status:   store.FamilyGrantActive,
	}
}

func medChange(id string, ver int64) model.SyncItem {
	return model.SyncItem{
		Table: model.TableMedication,
		ID:    id,
		Ver:   ver,
		IV:    "aXY=",
		CT:    "Y3Q=",
	}
}

// ------------------------------------------------------------------ 用例

// 核心断言：写入必须落在 owner 的账号空间，而不是 viewer 自己的。
// 写错这一点会导致「改了但对方收不到」，而且界面上看起来一切正常。
func TestAppendRecordsWritesIntoOwnerSpace(t *testing.T) {
	fr := &stubFamilyRepo{grant: activeGrant("owner-1", "viewer-1")}
	sr := &stubSyncRepo{}
	svc := NewService(fr, sr, "pepper")

	resp, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1",
		[]model.SyncItem{medChange("med-1", 3)})
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if sr.calls != 1 {
		t.Fatalf("Apply 应调用一次，实际 %d", sr.calls)
	}
	if sr.lastUser != "owner-1" {
		t.Fatalf("必须写到 owner 的账号空间，实际写到了 %q", sr.lastUser)
	}
	if len(resp.Applied) != 1 || resp.Applied[0] != "medication:med-1" {
		t.Fatalf("applied 不符合预期：%v", resp.Applied)
	}
	if resp.Cursor != 42 {
		t.Fatalf("游标应透传仓储返回值，实际 %d", resp.Cursor)
	}
}

// 非本授权的查看方不得写入（且不得触达同步内核）。
func TestAppendRecordsRejectsOtherViewer(t *testing.T) {
	fr := &stubFamilyRepo{grant: activeGrant("owner-1", "viewer-1")}
	sr := &stubSyncRepo{}
	svc := NewService(fr, sr, "pepper")

	_, err := svc.AppendRecords(context.Background(), "viewer-2", "grant-1",
		[]model.SyncItem{medChange("med-1", 1)})
	if !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("越权写入应返回 NotFound，实际 %v", err)
	}
	if sr.calls != 0 {
		t.Fatalf("越权请求不得触达同步内核，实际调用 %d 次", sr.calls)
	}
}

// 未兑换（viewer 为空）或已撤销的授权同样不能写。
func TestAppendRecordsRejectsInactiveGrant(t *testing.T) {
	pending := &store.FamilyGrant{ID: "grant-1", OwnerID: "owner-1", Status: store.FamilyGrantPending}
	svc := NewService(&stubFamilyRepo{grant: pending}, &stubSyncRepo{}, "pepper")
	if _, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1",
		[]model.SyncItem{medChange("med-1", 1)}); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("未兑换的授权不应允许写入，实际 %v", err)
	}

	revoked := activeGrant("owner-1", "viewer-1")
	revoked.Status = store.FamilyGrantRevoked
	svc2 := NewService(&stubFamilyRepo{grant: revoked}, &stubSyncRepo{}, "pepper")
	if _, err := svc2.AppendRecords(context.Background(), "viewer-1", "grant-1",
		[]model.SyncItem{medChange("med-1", 1)}); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("已撤销的授权不应允许写入，实际 %v", err)
	}
}

// 表白名单：除 medication 外一律拒绝，且不做「过滤后照写」。
func TestAppendRecordsRejectsNonMedicationTables(t *testing.T) {
	for _, table := range []string{
		model.TableSchedule, model.TableIntake, model.TableProfile, model.TableFamily, model.TableSetting,
	} {
		fr := &stubFamilyRepo{grant: activeGrant("owner-1", "viewer-1")}
		sr := &stubSyncRepo{}
		svc := NewService(fr, sr, "pepper")

		_, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1",
			[]model.SyncItem{{Table: table, ID: "x-1", Ver: 1}})
		if !apierr.Is(err, apierr.CodeForbidden) {
			t.Fatalf("表 %s 应被拒绝（Forbidden），实际 %v", table, err)
		}
		if sr.calls != 0 {
			t.Fatalf("表 %s 被拒后不得触达同步内核，实际调用 %d 次", table, sr.calls)
		}
	}
}

// 空变更与缺 id 属于入参错误，必须早于仓储调用被拦下。
func TestAppendRecordsValidatesInput(t *testing.T) {
	fr := &stubFamilyRepo{grant: activeGrant("owner-1", "viewer-1")}
	sr := &stubSyncRepo{}
	svc := NewService(fr, sr, "pepper")

	if _, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1", nil); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("空变更应返回 BadRequest，实际 %v", err)
	}
	if _, err := svc.AppendRecords(context.Background(), "viewer-1", "", []model.SyncItem{medChange("m", 1)}); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("缺 grantId 应返回 BadRequest，实际 %v", err)
	}
	if _, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1",
		[]model.SyncItem{{Table: model.TableMedication, ID: "", Ver: 1}}); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("缺记录 id 应返回 BadRequest，实际 %v", err)
	}
	if sr.calls != 0 {
		t.Fatalf("入参非法时不得触达同步内核，实际 %d 次", sr.calls)
	}
}

// 过期授权不能写（过期判定必须在服务层也成立，不能只依赖库里的 WHERE）。
func TestAppendRecordsRejectsExpired(t *testing.T) {
	g := activeGrant("owner-1", "viewer-1")
	past := time.Now().Add(-time.Hour)
	g.ExpiresAt = &past
	sr := &stubSyncRepo{}
	svc := NewService(&stubFamilyRepo{grant: g}, sr, "pepper")

	if _, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1",
		[]model.SyncItem{medChange("med-1", 1)}); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("过期授权应返回 NotFound，实际 %v", err)
	}
	if sr.calls != 0 {
		t.Fatalf("过期授权不得触达同步内核")
	}
}

// 同步内核缺失时明确报未实现，而不是静默丢弃（那样用户会以为改成功了）。
func TestAppendRecordsWithoutSyncRepo(t *testing.T) {
	svc := NewService(&stubFamilyRepo{grant: activeGrant("owner-1", "viewer-1")}, nil, "pepper")
	_, err := svc.AppendRecords(context.Background(), "viewer-1", "grant-1",
		[]model.SyncItem{medChange("med-1", 1)})
	if !apierr.Is(err, apierr.CodeNotImplemented) {
		t.Fatalf("缺同步内核应返回 NotImplemented，实际 %v", err)
	}
}
