package fake

import (
	"context"
	"sort"
	"time"

	"github.com/qiubite666/meddesk/internal/model"
)

// syncRepo 是 SyncRepo 的内存实现。
type syncRepo struct{ r *Repos }

// syncState 对应 PG 中的 sync_state 行 + sync_records 集合：
// records 按 (table, id) 唯一（等价联合主键），lastSeq 是已分配的最大 seq。
type syncState struct {
	records map[string]*model.SyncItem
	lastSeq int64
}

// Apply 在一个「逻辑事务」内完成冲突裁决、写入与 seq 分配。
//
// 裁决规则严格照搬契约 2.4（与 store.decide 完全一致）：
//
//	better(in, cur) := in.Ver > cur.Ver
//	                || (in.Ver == cur.Ver && in.UpdatedAt > cur.UpdatedAt)
//	                || (都相等 && in.Deleted && !cur.Deleted)
//
// 胜出 → 分配新的 seq 并覆盖写入，进 applied；落败 → 服务端当前版本（含其 seq）进 conflicts。
// 返回 cursor = 本次分配到的最大 seq（无任何应用时等于当前游标）。
func (s syncRepo) Apply(_ context.Context, userID string, items []model.SyncItem) ([]model.SyncItem, []model.SyncItem, int64, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()

	st := s.r.stateLocked(userID)
	if len(items) == 0 {
		return []model.SyncItem{}, []model.SyncItem{}, st.lastSeq, nil
	}

	applied := make([]model.SyncItem, 0, len(items))
	conflicts := make([]model.SyncItem, 0)
	for i := range items {
		in := items[i]
		key := syncRecordKey(in.Table, in.ID)
		if cur, ok := st.records[key]; ok && !decide(in, *cur) {
			conflicts = append(conflicts, *cur) // 服务端当前版本，供客户端合并
			continue
		}
		st.lastSeq++
		in.Seq = st.lastSeq
		cp := in
		st.records[key] = &cp
		applied = append(applied, cp)
	}
	return applied, conflicts, st.lastSeq, nil
}

// Pull 按 seq > afterSeq 升序取至多 limit 条。
// 实现上多取一条用于判定 hasMore（与真实实现的 paginate 一致）；
// 本页无数据时 cursor 保持 afterSeq。
func (s syncRepo) Pull(_ context.Context, userID string, afterSeq int64, limit int) ([]model.SyncItem, int64, bool, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()

	if limit <= 0 {
		limit = 100 // 与 store.defaultPullLimit 对齐
	}
	st := s.r.stateLocked(userID)
	matched := make([]model.SyncItem, 0, len(st.records))
	for _, it := range st.records {
		if it.Seq > afterSeq {
			matched = append(matched, *it)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Seq < matched[j].Seq })

	hasMore := len(matched) > limit
	page := matched
	if hasMore {
		page = matched[:limit]
	}
	cursor := afterSeq
	if len(page) > 0 {
		cursor = page[len(page)-1].Seq
	}
	return page, cursor, hasMore, nil
}

// Cursor 返回该用户当前最大 seq；从未同步过返回 0。
func (s syncRepo) Cursor(_ context.Context, userID string) (int64, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	return s.r.stateLocked(userID).lastSeq, nil
}

// Stats 返回同步概况：游标、记录总数（含墓碑）、分表统计与服务端时间。
// 与真实实现一致：ByTable 始终包含全部已知表（无数据的为 0），便于前端直接渲染。
func (s syncRepo) Stats(_ context.Context, userID string) (model.SyncStatsResponse, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()

	st := s.r.stateLocked(userID)
	resp := model.SyncStatsResponse{
		Cursor:      st.lastSeq,
		RecordCount: int64(len(st.records)),
		ByTable:     make(map[string]int64, len(model.KnownTables())),
		ServerTime:  time.Now().UnixMilli(),
	}
	for _, t := range model.KnownTables() {
		resp.ByTable[t] = 0
	}
	for _, it := range st.records {
		resp.ByTable[it.Table]++
	}
	return resp, nil
}

// Reset 清空该用户全部同步记录并把游标归零（危险操作，附件不受影响）。
func (s syncRepo) Reset(_ context.Context, userID string) error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	st := s.r.stateLocked(userID)
	st.records = make(map[string]*model.SyncItem)
	st.lastSeq = 0
	return nil
}

// stateLocked 取（必要时创建）用户的同步状态；调用方需已持锁。
func (r *Repos) stateLocked(userID string) *syncState {
	st, ok := r.syncStates[userID]
	if !ok {
		st = &syncState{records: make(map[string]*model.SyncItem)}
		r.syncStates[userID] = st
	}
	return st
}

// syncRecordKey 是 (table, id) 组成的唯一键，用 \x00 分隔避免拼接歧义。
func syncRecordKey(table, id string) string { return table + "\x00" + id }

// decide 判断客户端上传项 in 是否优于服务端当前项 cur（服务端权威，LWW 裁决）。
// 逻辑与 internal/store.decide 逐条一致，保证 fake 与真实实现的裁决结果相同。
func decide(in, cur model.SyncItem) bool {
	if in.Ver > cur.Ver {
		return true
	}
	if in.Ver < cur.Ver {
		return false
	}
	if in.UpdatedAt > cur.UpdatedAt {
		return true
	}
	if in.UpdatedAt < cur.UpdatedAt {
		return false
	}
	// 版本号与时间戳都相同：墓碑优先（删除胜出），避免删除被同版本的修改复活。
	return in.Deleted && !cur.Deleted
}
