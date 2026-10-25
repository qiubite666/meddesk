package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// it 构造一个仅含裁决相关字段的同步项。
func it(ver, updatedAt int64, deleted bool) model.SyncItem {
	return model.SyncItem{Ver: ver, UpdatedAt: updatedAt, Deleted: deleted}
}

// TestDecide 覆盖契约 2.4 冲突裁决的全部边界（服务端权威 LWW）。
func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		in, cur model.SyncItem
		exists  bool
		want    bool
	}{
		{"新插入（服务端无记录）", it(1, 100, false), model.SyncItem{}, false, true},
		{"ver 更大胜出", it(2, 100, false), it(1, 200, false), true, true},
		{"ver 相同 updatedAt 更新胜出", it(1, 200, false), it(1, 100, false), true, true},
		{"ver 更小落败", it(1, 200, false), it(2, 100, false), true, false},
		{"同 ver 同时间 删除优先", it(1, 100, true), it(1, 100, false), true, true},
		{"同 ver 同时间 非删除落败", it(1, 100, false), it(1, 100, false), true, false},
		{"同 ver 同时间 双方均删除落败", it(1, 100, true), it(1, 100, true), true, false},
	}
	for _, c := range cases {
		if got := winsOver(c.in, c.cur, c.exists); got != c.want {
			t.Fatalf("%s: 期望胜出=%v，实际=%v", c.name, c.want, got)
		}
	}
}

// TestPaginate 校验 Pull 的分页裁剪与游标推进。
func TestPaginate(t *testing.T) {
	mk := func(seqs ...int64) []model.SyncItem {
		out := make([]model.SyncItem, 0, len(seqs))
		for _, s := range seqs {
			out = append(out, model.SyncItem{Seq: s})
		}
		return out
	}
	if page, cursor, more := paginate(mk(1, 2, 3), 0, 2); len(page) != 2 || cursor != 2 || !more {
		t.Fatalf("首页: page=%d cursor=%d more=%v", len(page), cursor, more)
	}
	if page, cursor, more := paginate(mk(5), 4, 2); len(page) != 1 || cursor != 5 || more {
		t.Fatalf("末页: page=%d cursor=%d more=%v", len(page), cursor, more)
	}
	if page, cursor, more := paginate(nil, 7, 2); len(page) != 0 || cursor != 7 || more {
		t.Fatalf("空页: page=%d cursor=%d more=%v", len(page), cursor, more)
	}
}

// TestStoreWithDB 是数据库端到端用例；未配置 MEDDESK_TEST_DB_URL 时跳过。
func TestStoreWithDB(t *testing.T) {
	url := os.Getenv("MEDDESK_TEST_DB_URL")
	if url == "" {
		t.Skip("跳过数据库用例：未设置 MEDDESK_TEST_DB_URL（需可连的 PostgreSQL 16 实例）")
	}
	ctx := context.Background()
	s, err := Open(ctx, config.DBConfig{URL: url, MaxConns: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	u := &model.User{
		Email:        fmt.Sprintf("t%d@example.com", time.Now().UnixNano()),
		PasswordHash: "argon2id$stub", KDFSalt: []byte("salt"), WrappedDEK: []byte("dek"), DEKIV: []byte("iv"),
	}
	if err := s.Users().Create(ctx, u); err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	if err := s.Users().Create(ctx, u); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复邮箱应返回冲突，实际: %v", err)
	}
	in := model.SyncItem{Table: model.TableMedication, ID: "rec-1", Ver: 1, UpdatedAt: time.Now().UnixMilli(),
		IV: base64.StdEncoding.EncodeToString([]byte("0123456789ab")), CT: base64.StdEncoding.EncodeToString([]byte("ct"))}
	applied, conflicts, cursor, err := s.Sync().Apply(ctx, u.ID, []model.SyncItem{in})
	if err != nil || len(applied) != 1 || len(conflicts) != 0 || cursor != 1 || applied[0].Seq != 1 {
		t.Fatalf("首次 Apply: applied=%v conflicts=%v cursor=%d err=%v", applied, conflicts, cursor, err)
	}
	in.Ver, in.UpdatedAt = 2, time.Now().UnixMilli()
	if _, _, cursor, err = s.Sync().Apply(ctx, u.ID, []model.SyncItem{in}); err != nil || cursor != 2 {
		t.Fatalf("第二次 Apply: cursor=%d err=%v", cursor, err)
	}
	old := in
	old.Ver, old.UpdatedAt = 1, 1
	if _, conflicts, _, err = s.Sync().Apply(ctx, u.ID, []model.SyncItem{old}); err != nil || len(conflicts) != 1 {
		t.Fatalf("旧版本应产生冲突: conflicts=%v err=%v", conflicts, err)
	}
	items, c, more, err := s.Sync().Pull(ctx, u.ID, 0, 1)
	if err != nil || len(items) != 1 || c != 1 || !more {
		t.Fatalf("Pull: items=%d cursor=%d more=%v err=%v", len(items), c, more, err)
	}
	if err := s.Sync().Reset(ctx, u.ID); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if items, _, _, err := s.Sync().Pull(ctx, u.ID, 0, 10); err != nil || len(items) != 0 {
		t.Fatalf("Reset 后应无记录: %d %v", len(items), err)
	}
}
