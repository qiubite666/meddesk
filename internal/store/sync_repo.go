package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/qiubite666/meddesk/internal/model"
)

// syncRepo 是 SyncRepo 的 PostgreSQL 实现。
type syncRepo struct{ s *Store }

// defaultPullLimit 是调用方传入非正 limit 时使用的兜底分页大小。
const defaultPullLimit = 100

// syncColumns 是 sync_records 的查询列（顺序与 scanSync 一致）。
const syncColumns = `table_name, record_id, ver, updated_at, deleted, iv, ct, device_id, seq`

// syncKey 标识一条同步记录（用户维度下由 表名 + 记录ID 唯一确定）。
type syncKey struct {
	table string
	id    string
}

// winsOver 综合「服务端是否存在当前版本」与 decide 的裁决结果：
// 服务端没有该记录（首次上传）时客户端版本直接胜出。
func winsOver(in, cur model.SyncItem, exists bool) bool { return !exists || decide(in, cur) }

// paginate 对已按 seq 升序排列的记录做分页裁剪，判定 hasMore 并计算本页游标。
// 约定：调用方多取一条（limit+1）用于判定；本页无数据时游标保持 afterSeq。
// 这是纯函数，可直接单测（见 sync_repo_test.go）。
func paginate(items []model.SyncItem, afterSeq int64, limit int) (page []model.SyncItem, cursor int64, hasMore bool) {
	if limit <= 0 {
		limit = defaultPullLimit
	}
	hasMore = len(items) > limit
	page = items
	if hasMore {
		page = items[:limit]
	}
	cursor = afterSeq
	if len(page) > 0 {
		cursor = page[len(page)-1].Seq
	}
	return page, cursor, hasMore
}

// pendingItem 是已解码密文的待裁决项（解码在事务外完成，避免事务内出现可失败的转换）。
type pendingItem struct {
	item model.SyncItem
	iv   []byte
	ct   []byte
}

// appliedItem 是胜出待写入项（含解码后的密文字节）。
type appliedItem struct {
	item model.SyncItem
	iv   []byte
	ct   []byte
}

// decide 判断客户端上传项 in 是否优于服务端当前项 cur（服务端权威，LWW 裁决）。
//
// 规则（契约 2.4，严格实现）：
//
//	better(in, cur) := in.Ver > cur.Ver
//	                || (in.Ver == cur.Ver && in.UpdatedAt > cur.UpdatedAt)
//	                || (in.Ver == cur.Ver && in.UpdatedAt == cur.UpdatedAt && in.Deleted && !cur.Deleted)
//
// 这是纯函数，可直接单测（见 sync_repo_test.go）。
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

// Apply 在单个事务内完成冲突裁决、写入与 seq 分配。
//
// 流程：
//  1. 兜底确保 sync_state 行存在（INSERT ... ON CONFLICT DO NOTHING）；
//  2. 一次性读出本批次涉及记录的服务端当前版本；
//  3. 按输入顺序裁决：胜出进 applied（同批次重复 key 以最后一次为准），否则服务端版本进 conflicts；
//  4. 事务内 UPDATE sync_state SET last_seq = last_seq + n RETURNING last_seq 取号，
//     再倒序为 applied 分配 seq，保持与输入顺序一致的单调递增；
//  5. 单条语句批量 upsert。
//
// 返回的 cursor 是本次分配的最大 seq（即 sync_state.last_seq 的最新值）；
// items 为空时不分配 seq，直接返回当前游标。
func (r *syncRepo) Apply(ctx context.Context, userID string, items []model.SyncItem) ([]model.SyncItem, []model.SyncItem, int64, error) {
	userParam, err := uuidParam(userID)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(items) == 0 {
		cursor, err := r.Cursor(ctx, userID)
		if err != nil {
			return nil, nil, 0, err
		}
		return []model.SyncItem{}, []model.SyncItem{}, cursor, nil
	}

	// 预解码密文：非法 base64 在进入事务前就返回，避免污染事务。
	pendings := make([]pendingItem, 0, len(items))
	for _, it := range items {
		iv, err := decodeBase64("iv", it.IV)
		if err != nil {
			return nil, nil, 0, err
		}
		ct, err := decodeBase64("ct", it.CT)
		if err != nil {
			return nil, nil, 0, err
		}
		pendings = append(pendings, pendingItem{item: it, iv: iv, ct: ct})
	}

	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, 0, wrap("开启同步事务失败", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) 兜底：用户首次同步时没有 sync_state 行。
	if _, err := tx.Exec(ctx, `INSERT INTO sync_state (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, userParam); err != nil {
		return nil, nil, 0, wrap("初始化同步状态失败", err)
	}

	// 2) 读取服务端当前版本（一次查询取回整批）。
	current, err := loadCurrent(ctx, tx, userParam, pendings)
	if err != nil {
		return nil, nil, 0, err
	}

	// 3) 按输入顺序裁决。
	var applied []appliedItem
	var conflicts []model.SyncItem
	appliedIdx := make(map[syncKey]int, len(pendings))
	for _, p := range pendings {
		key := syncKey{table: p.item.Table, id: p.item.ID}
		cur, exists := current[key]
		if !winsOver(p.item, cur, exists) {
			conflicts = append(conflicts, cur) // 服务端当前版本，供客户端合并
			continue
		}
		win := appliedItem{item: p.item, iv: p.iv, ct: p.ct}
		if idx, ok := appliedIdx[key]; ok {
			applied[idx] = win // 同批次重复 key：以最后一次胜出结果为准，位置不变
		} else {
			appliedIdx[key] = len(applied)
			applied = append(applied, win)
		}
		current[key] = p.item // 后续重复项与刚写入的结果比较
	}

	// 4) 分配 seq：事务内原子自增，倒序回填，使 applied 的 seq 升序。
	var cursor int64
	if len(applied) == 0 {
		// 全部落败（全是冲突）：不消耗 seq，返回当前游标。
		if cursor, err = readSeq(ctx, tx, userParam); err != nil {
			return nil, nil, 0, err
		}
	}
	if len(applied) > 0 {
		var lastSeq int64
		err := tx.QueryRow(ctx,
			`UPDATE sync_state SET last_seq = last_seq + $2 WHERE user_id = $1 RETURNING last_seq`,
			userParam, int64(len(applied))).Scan(&lastSeq)
		if err != nil {
			return nil, nil, 0, wrap("分配同步序列号失败", err)
		}
		for i := len(applied) - 1; i >= 0; i-- {
			applied[i].item.Seq = lastSeq
			lastSeq--
		}
		cursor = applied[len(applied)-1].item.Seq

		// 5) 批量写入（单条语句，避免 N 次往返）。
		if err := upsertSyncRecords(ctx, tx, userParam, applied); err != nil {
			return nil, nil, 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, 0, wrap("提交同步事务失败", err)
	}

	out := make([]model.SyncItem, 0, len(applied))
	for _, a := range applied {
		out = append(out, a.item)
	}
	return out, conflicts, cursor, nil
}

// readSeq 读取该用户当前的 last_seq（调用方需保证 sync_state 行已存在）。
func readSeq(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, userParam pgtype.UUID) (int64, error) {
	var seq int64
	if err := db.QueryRow(ctx, `SELECT last_seq FROM sync_state WHERE user_id = $1`, userParam).Scan(&seq); err != nil {
		return 0, wrap("读取同步游标失败", err)
	}
	return seq, nil
}

// loadCurrent 一次取出本批次涉及记录的服务端当前版本，返回 key → 当前项映射。
func loadCurrent(ctx context.Context, tx pgx.Tx, userParam pgtype.UUID, pendings []pendingItem) (map[syncKey]model.SyncItem, error) {
	tables := make([]string, 0, len(pendings))
	ids := make([]string, 0, len(pendings))
	seen := make(map[syncKey]struct{}, len(pendings))
	for _, p := range pendings {
		key := syncKey{table: p.item.Table, id: p.item.ID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		tables = append(tables, key.table)
		ids = append(ids, key.id)
	}

	current := make(map[syncKey]model.SyncItem, len(tables))
	if len(tables) == 0 {
		return current, nil
	}
	// (table_name, record_id) 成对匹配：FROM 中的多参数 unnest 按行展开。
	const q = `SELECT ` + syncColumns + ` FROM sync_records
WHERE user_id = $1 AND (table_name, record_id) IN (SELECT * FROM unnest($2::text[], $3::text[]))`
	rows, err := tx.Query(ctx, q, userParam, tables, ids)
	if err != nil {
		return nil, wrap("读取服务端当前版本失败", err)
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanSync(rows)
		if err != nil {
			return nil, wrap("读取服务端当前版本失败", err)
		}
		current[syncKey{table: it.Table, id: it.ID}] = *it
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("读取服务端当前版本失败", err)
	}
	return current, nil
}

// upsertSyncRecords 用单条语句批量写入胜出记录（含 seq）。
func upsertSyncRecords(ctx context.Context, tx pgx.Tx, userParam pgtype.UUID, applied []appliedItem) error {
	n := len(applied)
	tables := make([]string, 0, n)
	ids := make([]string, 0, n)
	vers := make([]int64, 0, n)
	updatedAt := make([]int64, 0, n)
	deleted := make([]bool, 0, n)
	ivs := make([][]byte, 0, n)
	cts := make([][]byte, 0, n)
	devices := make([]string, 0, n)
	seqs := make([]int64, 0, n)
	for _, a := range applied {
		tables = append(tables, a.item.Table)
		ids = append(ids, a.item.ID)
		vers = append(vers, a.item.Ver)
		updatedAt = append(updatedAt, a.item.UpdatedAt)
		deleted = append(deleted, a.item.Deleted)
		ivs = append(ivs, a.iv)
		cts = append(cts, a.ct)
		devices = append(devices, a.item.DeviceID)
		seqs = append(seqs, a.item.Seq)
	}

	const q = `INSERT INTO sync_records
    (user_id, table_name, record_id, ver, updated_at, deleted, iv, ct, device_id, seq)
SELECT $1, u.table_name, u.record_id, u.ver, u.updated_at, u.deleted, u.iv, u.ct, u.device_id, u.seq
FROM unnest($2::text[], $3::text[], $4::bigint[], $5::bigint[], $6::boolean[], $7::bytea[], $8::bytea[], $9::text[], $10::bigint[])
  AS u(table_name, record_id, ver, updated_at, deleted, iv, ct, device_id, seq)
ON CONFLICT (user_id, table_name, record_id) DO UPDATE SET
    ver = EXCLUDED.ver,
    updated_at = EXCLUDED.updated_at,
    deleted = EXCLUDED.deleted,
    iv = EXCLUDED.iv,
    ct = EXCLUDED.ct,
    device_id = EXCLUDED.device_id,
    seq = EXCLUDED.seq,
    updated_seq = now()`

	if _, err := tx.Exec(ctx, q, userParam, tables, ids, vers, updatedAt, deleted, ivs, cts, devices, seqs); err != nil {
		return wrap("写入同步记录失败", err)
	}
	return nil
}

// Pull 按 seq > afterSeq 升序拉取至多 limit 条。
// 实现上多取一条用于判定 hasMore；cursor 为本页最大 seq（无数据时等于 afterSeq）。
func (r *syncRepo) Pull(ctx context.Context, userID string, afterSeq int64, limit int) ([]model.SyncItem, int64, bool, error) {
	userParam, err := uuidParam(userID)
	if err != nil {
		return nil, 0, false, err
	}
	if limit <= 0 {
		limit = defaultPullLimit
	}

	// 多取一条用于判定 hasMore。LIMIT 参数是 bigint，必须显式转成 int64（pgx 不支持 Go 的 int）。
	const q = `SELECT ` + syncColumns + ` FROM sync_records
WHERE user_id = $1 AND seq > $2 ORDER BY seq ASC LIMIT $3`
	rows, err := r.s.pool.Query(ctx, q, userParam, afterSeq, int64(limit+1))
	if err != nil {
		return nil, 0, false, wrap("拉取同步记录失败", err)
	}
	defer rows.Close()

	items := make([]model.SyncItem, 0, limit+1)
	for rows.Next() {
		it, err := scanSync(rows)
		if err != nil {
			return nil, 0, false, wrap("拉取同步记录失败", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, wrap("拉取同步记录失败", err)
	}

	page, cursor, hasMore := paginate(items, afterSeq, limit)
	return page, cursor, hasMore, nil
}

// Cursor 返回该用户已分配的最大 seq；用户尚无 sync_state 行时返回 0。
func (r *syncRepo) Cursor(ctx context.Context, userID string) (int64, error) {
	userParam, err := uuidParam(userID)
	if err != nil {
		return 0, err
	}
	var seq int64
	err = r.s.pool.QueryRow(ctx, `SELECT last_seq FROM sync_state WHERE user_id = $1`, userParam).Scan(&seq)
	if err != nil {
		if errors.Is(notFound(err), ErrNotFound) {
			return 0, nil // 用户尚未同步过
		}
		return 0, wrap("读取同步游标失败", err)
	}
	return seq, nil
}

// Stats 返回同步概况：游标、记录总数、分表统计与服务端时间（Unix 毫秒）。
func (r *syncRepo) Stats(ctx context.Context, userID string) (model.SyncStatsResponse, error) {
	userParam, err := uuidParam(userID)
	if err != nil {
		return model.SyncStatsResponse{}, err
	}
	resp := model.SyncStatsResponse{
		ByTable:    make(map[string]int64, len(model.KnownTables())),
		ServerTime: time.Now().UnixMilli(),
	}
	for _, t := range model.KnownTables() {
		resp.ByTable[t] = 0 // 保证前端始终拿到完整表清单
	}

	// 游标取自 sync_state.last_seq，与 Apply/Pull 的游标语义保持一致。
	resp.Cursor, err = r.Cursor(ctx, userID)
	if err != nil {
		return model.SyncStatsResponse{}, err
	}
	err = r.s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sync_records WHERE user_id = $1`, userParam,
	).Scan(&resp.RecordCount)
	if err != nil {
		return model.SyncStatsResponse{}, wrap("统计同步概况失败", err)
	}

	rows, err := r.s.pool.Query(ctx,
		`SELECT table_name, COUNT(*) FROM sync_records WHERE user_id = $1 GROUP BY table_name`, userParam)
	if err != nil {
		return model.SyncStatsResponse{}, wrap("统计分表记录数失败", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		var n int64
		if err := rows.Scan(&table, &n); err != nil {
			return model.SyncStatsResponse{}, wrap("统计分表记录数失败", err)
		}
		resp.ByTable[table] = n
	}
	if err := rows.Err(); err != nil {
		return model.SyncStatsResponse{}, wrap("统计分表记录数失败", err)
	}
	return resp, nil
}

// Reset 清空该用户的全部同步记录并把游标归零（危险操作，附件不受影响）。
func (r *syncRepo) Reset(ctx context.Context, userID string) error {
	userParam, err := uuidParam(userID)
	if err != nil {
		return err
	}
	if _, err := r.s.pool.Exec(ctx, `DELETE FROM sync_records WHERE user_id = $1`, userParam); err != nil {
		return wrap("清空同步记录失败", err)
	}
	const q = `INSERT INTO sync_state (user_id, last_seq) VALUES ($1, 0)
ON CONFLICT (user_id) DO UPDATE SET last_seq = 0`
	if _, err := r.s.pool.Exec(ctx, q, userParam); err != nil {
		return wrap("重置同步游标失败", err)
	}
	return nil
}

// scanSync 把一行扫描成 model.SyncItem（iv/ct 由 bytea 还原为标准 base64）。
func scanSync(row pgx.Row) (*model.SyncItem, error) {
	var (
		it       model.SyncItem
		iv       []byte
		ct       []byte
		deviceID pgtype.Text
	)
	err := row.Scan(&it.Table, &it.ID, &it.Ver, &it.UpdatedAt, &it.Deleted, &iv, &ct, &deviceID, &it.Seq)
	if err != nil {
		return nil, notFound(err)
	}
	it.IV = encodeBase64(iv)
	it.CT = encodeBase64(ct)
	it.DeviceID = deviceID.String
	return &it, nil
}

// 编译期断言：syncRepo 实现 SyncRepo 接口。
var _ SyncRepo = (*syncRepo)(nil)
