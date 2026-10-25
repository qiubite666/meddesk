package syncsvc

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// fakeRepo 是按契约 2.4 实现的内存假仓储，仅用于本包单测；items 按 seq 升序。
type fakeRepo struct {
	items []model.SyncItem
	seq   int64
	reset int
}

var _ store.SyncRepo = (*fakeRepo)(nil)

func (f *fakeRepo) find(it model.SyncItem) int {
	for i, cur := range f.items {
		if cur.Table == it.Table && cur.ID == it.ID {
			return i
		}
	}
	return -1
}

func (f *fakeRepo) Apply(_ context.Context, _ string, in []model.SyncItem) (a, c []model.SyncItem, cur int64, err error) {
	for _, it := range in {
		if i := f.find(it); i >= 0 {
			o := f.items[i]
			if !(it.Ver > o.Ver || (it.Ver == o.Ver && it.UpdatedAt > o.UpdatedAt) ||
				(it.Ver == o.Ver && it.UpdatedAt == o.UpdatedAt && it.Deleted && !o.Deleted)) {
				c = append(c, o)
				continue
			}
			f.items = append(f.items[:i], f.items[i+1:]...)
		}
		f.seq++
		it.Seq = f.seq
		f.items, a = append(f.items, it), append(a, it)
	}
	return a, c, f.seq, nil
}

func (f *fakeRepo) Pull(_ context.Context, _ string, after int64, limit int) ([]model.SyncItem, int64, bool, error) {
	var out []model.SyncItem
	for _, it := range f.items {
		if it.Seq > after {
			out = append(out, it)
		}
	}
	more, cur := len(out) > limit, after
	if more {
		out = out[:limit]
	}
	if n := len(out); n > 0 {
		cur = out[n-1].Seq
	}
	return out, cur, more, nil
}

func (f *fakeRepo) Cursor(_ context.Context, _ string) (int64, error) { return f.seq, nil }
func (f *fakeRepo) Stats(_ context.Context, _ string) (model.SyncStatsResponse, error) {
	return model.SyncStatsResponse{Cursor: f.seq, RecordCount: int64(len(f.items)), ServerTime: 1}, nil
}

func (f *fakeRepo) Reset(_ context.Context, _ string) error {
	f.items, f.seq, f.reset = nil, 0, f.reset+1
	return nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func testCfg() config.SyncConfig {
	return config.SyncConfig{MaxItemsPerPush: 2, MaxRecordBytes: 16, PullPageSize: 2, MaxPullPageSize: 5}
}

func goodItem(id string, ver, at int64) model.SyncItem {
	return model.SyncItem{Table: model.TableMedication, ID: id, Ver: ver, UpdatedAt: at, IV: b64("123456789012"), CT: b64("cipher")}
}

func TestValidatePush(t *testing.T) {
	now, cfg, at := time.Now(), testCfg(), time.Now().UnixMilli()
	mk := func(fn func(*model.SyncItem)) model.SyncItem {
		it := goodItem("a1", 1, at)
		fn(&it)
		return it
	}
	bad := []struct {
		name string
		it   model.SyncItem
	}{
		{"未知表", mk(func(i *model.SyncItem) { i.Table = "evil" })},
		{"空id", mk(func(i *model.SyncItem) { i.ID = "" })},
		{"超长id", mk(func(i *model.SyncItem) { i.ID = strings.Repeat("a", 65) })},
		{"非法id字符", mk(func(i *model.SyncItem) { i.ID = "id with space" })},
		{"ver为0", mk(func(i *model.SyncItem) { i.Ver = 0 })},
		{"时间为0", mk(func(i *model.SyncItem) { i.UpdatedAt = 0 })},
		{"时间超前", mk(func(i *model.SyncItem) { i.UpdatedAt = now.Add(time.Hour).UnixMilli() })},
		{"iv缺失", mk(func(i *model.SyncItem) { i.IV = "" })},
		{"iv非法", mk(func(i *model.SyncItem) { i.IV = "!!!" })},
		{"ct非法", mk(func(i *model.SyncItem) { i.CT = "@@@" })},
		{"ct过大", mk(func(i *model.SyncItem) { i.CT = b64(strings.Repeat("x", 32)) })},
		{"非墓碑空ct", mk(func(i *model.SyncItem) { i.CT = "" })},
	}
	for _, tc := range bad {
		err := validatePush(cfg, now, []model.SyncItem{tc.it})
		if !apierr.Is(err, apierr.CodeBadRequest) {
			t.Fatalf("%s: 期望 bad_request，实际 %v", tc.name, err)
		}
		if d, _ := err.(*apierr.Error).Details.(validationDetails); len(d.Items) != 1 || d.Items[0].Index != 0 {
			t.Fatalf("%s: details 缺少条目索引: %+v", tc.name, d)
		}
	}
	if err := validatePush(cfg, now, []model.SyncItem{goodItem("a1", 1, at)}); err != nil {
		t.Fatalf("正常条目应通过: %v", err)
	}
	tomb := goodItem("a1", 1, at)
	tomb.Deleted, tomb.CT = true, ""
	if err := validatePush(cfg, now, []model.SyncItem{tomb}); err != nil {
		t.Fatalf("墓碑允许空 ct: %v", err)
	}
	it := goodItem("a1", 1, at)
	if err := validatePush(cfg, now, []model.SyncItem{it, it}); err == nil {
		t.Fatal("同批次重复 (table,id) 应报错")
	}
	if err := validatePush(cfg, now, []model.SyncItem{it, it, it}); err == nil {
		t.Fatal("超过 MaxItemsPerPush 应报错")
	}
}

func TestPushAppliedAndConflict(t *testing.T) {
	ctx, repo, cfg := context.Background(), &fakeRepo{}, testCfg()
	svc, at := NewService(repo, cfg), time.Now().UnixMilli()
	r1, err := svc.Push(ctx, "u1", model.SyncPushRequest{Changes: []model.SyncItem{goodItem("a1", 2, at)}})
	if err != nil || len(r1.Applied) != 1 || r1.Applied[0] != "medication:a1" || r1.Cursor != 1 || r1.SyncedAt <= 0 {
		t.Fatalf("正常 push: %+v err=%v", r1, err)
	}
	old := goodItem("a1", 2, at-1000)
	old.CT = b64("old-cipher")
	r2, err := svc.Push(ctx, "u1", model.SyncPushRequest{Changes: []model.SyncItem{old}})
	if err != nil || len(r2.Applied) != 0 || len(r2.Conflicts) != 1 {
		t.Fatalf("冲突 push: %+v err=%v", r2, err)
	}
	if r2.Conflicts[0].Seq != 1 || r2.Conflicts[0].CT != b64("cipher") {
		t.Fatalf("冲突项应回传服务端版本(含 seq): %+v", r2.Conflicts[0])
	}
}

func TestPullParams(t *testing.T) {
	ctx, repo, cfg := context.Background(), &fakeRepo{}, testCfg()
	svc, at := NewService(repo, cfg), time.Now().UnixMilli()
	var batch []model.SyncItem
	for i := 0; i < 6; i++ {
		batch = append(batch, goodItem("a"+string(rune('0'+i)), 1, at))
	}
	if _, _, _, err := repo.Apply(ctx, "u1", batch); err != nil {
		t.Fatal(err)
	}
	p1, _ := svc.Pull(ctx, "u1", 0, 0) // 缺省 → PullPageSize=2
	if len(p1.Items) != 2 || !p1.HasMore || p1.Cursor != 2 {
		t.Fatalf("默认分页: %+v", p1)
	}
	p2, _ := svc.Pull(ctx, "u1", 0, 999) // 超限 → 钳制到 MaxPullPageSize=5
	if len(p2.Items) != 5 || !p2.HasMore {
		t.Fatalf("上限钳制: %+v", p2)
	}
	p3, _ := svc.Pull(ctx, "u1", 5, 0)
	if len(p3.Items) != 1 || p3.HasMore || p3.Cursor != 6 {
		t.Fatalf("游标增量: %+v", p3)
	}
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/sync/pull?cursor=abc", nil)
	if _, _, err := parsePullParams(c, cfg); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("非法 cursor 应返回 400: %v", err)
	}
}

func TestResetConfirm(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &fakeRepo{}
	h := NewHandler(NewService(repo, testCfg()))
	do := func(body string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sync/reset", strings.NewReader(body))
		c.Set(ctxKeyUserID, "u1")
		h.reset(c)
		return w.Code
	}
	if code := do(`{"confirm":"NO"}`); code != http.StatusBadRequest {
		t.Fatalf("确认词错误应返回 400，实际 %d", code)
	}
	if code := do(`{"confirm":"DELETE-ALL"}`); code != http.StatusOK || repo.reset != 1 {
		t.Fatalf("确认词正确应清空并返回 200，实际 %d reset=%d", code, repo.reset)
	}
}
