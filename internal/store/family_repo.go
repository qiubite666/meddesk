package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// FamilyGrantStatus 是授权状态机取值。
const (
	FamilyGrantPending = "pending"
	FamilyGrantActive  = "active"
	FamilyGrantRevoked = "revoked"
)

// FamilyGrant 是 family_grants 的一行。
//
// 注意：这里的 KDFSalt / WrappedDEK / DEKIV 全是**密文材料**，
// 服务端没有任何办法解出 owner 的真实 DEK——解密需要短码，而短码不在库里。
type FamilyGrant struct {
	ID         string
	OwnerID    string
	ViewerID   *string
	CodeLookup []byte
	KDFSalt    []byte
	WrappedDEK []byte
	DEKIV      []byte
	Status     string
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	RedeemedAt *time.Time
	RevokedAt  *time.Time
}

// FamilyRepo 是家庭关联授权仓储。
//
// 设计约束：
//   - 只能按 CodeLookup（HMAC 结果）或主键/用户维度查询，**不允许全表扫描**；
//   - Redeem 必须是单条原子 UPDATE，禁止「先查再改」，否则并发兑换会双人绑定成功。
type FamilyRepo interface {
	Create(ctx context.Context, g *FamilyGrant) error
	GetByID(ctx context.Context, id string) (*FamilyGrant, error)
	GetByLookup(ctx context.Context, lookup []byte) (*FamilyGrant, error)
	Redeem(ctx context.Context, lookup []byte, viewerID string) (*FamilyGrant, error)
	ListByOwner(ctx context.Context, ownerID string) ([]FamilyGrant, error)
	ListActiveForViewer(ctx context.Context, viewerID string) ([]FamilyGrant, error)
	Revoke(ctx context.Context, id, ownerID string) error
	// ListOwnerRecords 只读读取指定用户的密文记录（供授权查看方拉取）。
	// 不触碰 sync_state；调用方必须先完成授权校验。
	ListOwnerRecords(ctx context.Context, ownerID string, sinceSeq int64, limit int) ([]OwnerRecord, int64, bool, error)
}

// OwnerRecord 是 owner 的一条密文同步记录。
//
// 字段类型必须与 sync_records 的实际列类型一致：
//   - updated_at 是 bigint（客户端 Unix 毫秒），**不是** timestamptz。
//     早先这里写成 time.Time，导致 rows.Scan 直接失败、接口回「读取记录失败」。
//     这段代码此前只有桩测试覆盖，真实调用一次就炸。
//   - ver / seq 是 bigint，用 int64 接，避免驱动做窄化转换时报错。
type OwnerRecord struct {
	Table     string
	RecordID  string
	Ver       int64
	UpdatedAt int64 // Unix 毫秒
	Deleted   bool
	IV        []byte
	CT        []byte
	Seq       int64
}

type familyRepo struct{ s *Store }

const familyColumns = `id, owner_id, viewer_id, code_lookup, kdf_salt, wrapped_dek, dek_iv,
	status, expires_at, created_at, redeemed_at, revoked_at`

func scanFamilyGrant(row pgx.Row) (*FamilyGrant, error) {
	var g FamilyGrant
	err := row.Scan(&g.ID, &g.OwnerID, &g.ViewerID, &g.CodeLookup, &g.KDFSalt, &g.WrappedDEK,
		&g.DEKIV, &g.Status, &g.ExpiresAt, &g.CreatedAt, &g.RedeemedAt, &g.RevokedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &g, nil
}

func (r *familyRepo) Create(ctx context.Context, g *FamilyGrant) error {
	if r == nil || r.s == nil || r.s.pool == nil {
		return wrap("创建授权失败", errNoPool)
	}
	_, err := r.s.pool.Exec(ctx, `
INSERT INTO family_grants (id, owner_id, code_lookup, kdf_salt, wrapped_dek, dek_iv, status, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		g.ID, g.OwnerID, g.CodeLookup, g.KDFSalt, g.WrappedDEK, g.DEKIV, FamilyGrantPending, g.ExpiresAt)
	if err != nil {
		// code_lookup 唯一冲突意味着短码撞车，属于可重试的偶发情况
		if isUniqueViolation(err) {
			return newConflict("短码重复，请重新生成")
		}
		return wrap("创建授权失败", err)
	}
	return nil
}

func (r *familyRepo) GetByID(ctx context.Context, id string) (*FamilyGrant, error) {
	if r == nil || r.s == nil || r.s.pool == nil {
		return nil, wrap("查询授权失败", errNoPool)
	}
	return scanFamilyGrant(r.s.pool.QueryRow(ctx,
		`SELECT `+familyColumns+` FROM family_grants WHERE id = $1`, id))
}

func (r *familyRepo) GetByLookup(ctx context.Context, lookup []byte) (*FamilyGrant, error) {
	if r == nil || r.s == nil || r.s.pool == nil {
		return nil, wrap("查询授权失败", errNoPool)
	}
	return scanFamilyGrant(r.s.pool.QueryRow(ctx,
		`SELECT `+familyColumns+` FROM family_grants WHERE code_lookup = $1`, lookup))
}

// revokeActivePair 撤销同一对 owner+viewer 之间的 active 授权。
//
// 只用于「重新兑换」场景：短码已校验通过，说明调用方确实拿到了对方新生成的码，
// 此时让新授权顶替旧的，而不是把用户挡在门外。只动 status='active' 的行，
// 其它 pending 的悬挂码不受影响。
func (r *familyRepo) revokeActivePair(ctx context.Context, ownerID, viewerID string) error {
	if r == nil || r.s == nil || r.s.pool == nil {
		return wrap("撤销旧授权失败", errNoPool)
	}
	owner, err := uuidParam(ownerID)
	if err != nil {
		return wrap("撤销旧授权失败", err)
	}
	viewer, err := uuidParam(viewerID)
	if err != nil {
		return wrap("撤销旧授权失败", err)
	}
	_, err = r.s.pool.Exec(ctx, `
UPDATE family_grants
   SET status = $3, revoked_at = now()
 WHERE owner_id = $1 AND viewer_id = $2 AND status = $4`,
		owner, viewer, FamilyGrantRevoked, FamilyGrantActive)
	if err != nil {
		return wrap("撤销旧授权失败", err)
	}
	return nil
}

// Redeem 原子地把 pending 绑定到 viewer。
// 单条 UPDATE ... WHERE status='pending' 保证并发兑换只有一个能成功。
// 兑换成功后 expires_at 置 NULL —— 按产品决定，授权建立后永久有效。
func (r *familyRepo) Redeem(ctx context.Context, lookup []byte, viewerID string) (*FamilyGrant, error) {
	if r == nil || r.s == nil || r.s.pool == nil {
		return nil, wrap("兑换授权失败", errNoPool)
	}
	g, err := scanFamilyGrant(r.s.pool.QueryRow(ctx, `
UPDATE family_grants
   SET status = $2, viewer_id = $3, redeemed_at = now(), expires_at = NULL
 WHERE code_lookup = $1
   AND status = $4
   AND (expires_at IS NULL OR expires_at > now())
RETURNING `+familyColumns, lookup, FamilyGrantActive, viewerID, FamilyGrantPending))

	if err == nil {
		return g, nil
	}
	// 同一对账号只允许存在一条 active 授权（family_grants_pair_uidx）。
	// 若该查看方此前已关联过这个 owner，再兑换对方新生成的码就会撞这个索引。
	//
	// 【为什么是替换而不是拒绝】
	// 旧授权和新码指向同一份数据，用户重新兑换的意图就是「以这张新码为准」。
	// 直接回 409 会让用户永久卡在「短码无效」，而且他并没有做错什么 ——
	// 对方重新生成码是再正常不过的操作。所以这里先撤销旧的，再让新码生效。
	if isUniqueViolation(err) {
		cur, getErr := r.GetByLookup(ctx, lookup)
		if getErr != nil {
			return nil, getErr
		}
		if revErr := r.revokeActivePair(ctx, cur.OwnerID, viewerID); revErr != nil {
			return nil, revErr
		}
		// 旧记录已让位，重试同一条 UPDATE 即可成功
		g, err = scanFamilyGrant(r.s.pool.QueryRow(ctx, `
UPDATE family_grants
   SET status = $2, viewer_id = $3, redeemed_at = now(), expires_at = NULL
 WHERE code_lookup = $1
   AND status = $4
   AND (expires_at IS NULL OR expires_at > now())
RETURNING `+familyColumns, lookup, FamilyGrantActive, viewerID, FamilyGrantPending))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, wrap("兑换授权失败", err)
		}
		return g, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 没抢到：区分「自己已兑换过（幂等）」与「被别人兑换/已失效」
	cur, getErr := r.GetByLookup(ctx, lookup)
	if getErr != nil {
		return nil, getErr
	}
	if cur.Status == FamilyGrantActive && cur.ViewerID != nil && *cur.ViewerID == viewerID {
		return cur, nil
	}
	if cur.Status == FamilyGrantActive {
		// 已被他人兑换：只回冲突，绝不泄露 owner 信息
		return nil, newConflict("该短码已被使用")
	}
	return nil, ErrNotFound
}

func (r *familyRepo) ListByOwner(ctx context.Context, ownerID string) ([]FamilyGrant, error) {
	if r == nil || r.s == nil || r.s.pool == nil {
		return nil, wrap("查询授权列表失败", errNoPool)
	}
	rows, err := r.s.pool.Query(ctx,
		`SELECT `+familyColumns+` FROM family_grants WHERE owner_id = $1 ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, wrap("查询授权列表失败", err)
	}
	defer rows.Close()

	var out []FamilyGrant
	for rows.Next() {
		var g FamilyGrant
		if err := rows.Scan(&g.ID, &g.OwnerID, &g.ViewerID, &g.CodeLookup, &g.KDFSalt, &g.WrappedDEK,
			&g.DEKIV, &g.Status, &g.ExpiresAt, &g.CreatedAt, &g.RedeemedAt, &g.RevokedAt); err != nil {
			return nil, wrap("读取授权列表失败", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (r *familyRepo) ListActiveForViewer(ctx context.Context, viewerID string) ([]FamilyGrant, error) {
	if r == nil || r.s == nil || r.s.pool == nil {
		return nil, wrap("查询授权列表失败", errNoPool)
	}
	rows, err := r.s.pool.Query(ctx, `
SELECT `+familyColumns+` FROM family_grants
 WHERE viewer_id = $1 AND status = $2 AND (expires_at IS NULL OR expires_at > now())
 ORDER BY redeemed_at DESC`, viewerID, FamilyGrantActive)
	if err != nil {
		return nil, wrap("查询授权列表失败", err)
	}
	defer rows.Close()

	var out []FamilyGrant
	for rows.Next() {
		var g FamilyGrant
		if err := rows.Scan(&g.ID, &g.OwnerID, &g.ViewerID, &g.CodeLookup, &g.KDFSalt, &g.WrappedDEK,
			&g.DEKIV, &g.Status, &g.ExpiresAt, &g.CreatedAt, &g.RedeemedAt, &g.RevokedAt); err != nil {
			return nil, wrap("读取授权列表失败", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Revoke 只允许所有者撤销自己的授权；非本人操作返回 ErrNotFound（不泄露存在性）。
func (r *familyRepo) Revoke(ctx context.Context, id, ownerID string) error {
	if r == nil || r.s == nil || r.s.pool == nil {
		return wrap("撤销授权失败", errNoPool)
	}
	tag, err := r.s.pool.Exec(ctx, `
UPDATE family_grants
   SET status = $3, revoked_at = now()
 WHERE id = $1 AND owner_id = $2 AND status <> $3`, id, ownerID, FamilyGrantRevoked)
	if err != nil {
		return wrap("撤销授权失败", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListOwnerRecords 按 seq 游标读取指定用户的密文记录。
//
// 只读：不加锁、不改 sync_state、不分配 seq。limit+1 用于判断 hasMore。
func (r *familyRepo) ListOwnerRecords(ctx context.Context, ownerID string, sinceSeq int64, limit int) ([]OwnerRecord, int64, bool, error) {
	if r == nil || r.s == nil || r.s.pool == nil {
		return nil, 0, false, wrap("读取记录失败", errNoPool)
	}
	if limit <= 0 {
		limit = 200
	}
	uid, err := uuidParam(ownerID)
	if err != nil {
		return nil, 0, false, wrap("读取记录失败", err)
	}
	rows, err := r.s.pool.Query(ctx, `
SELECT table_name, record_id, ver, updated_at, deleted, iv, ct, seq
  FROM sync_records
 WHERE user_id = $1 AND seq > $2
 ORDER BY seq ASC
 LIMIT $3`, uid, sinceSeq, limit+1)
	if err != nil {
		return nil, 0, false, wrap("读取记录失败", err)
	}
	defer rows.Close()

	var out []OwnerRecord
	for rows.Next() {
		var it OwnerRecord
		if err := rows.Scan(&it.Table, &it.RecordID, &it.Ver, &it.UpdatedAt, &it.Deleted, &it.IV, &it.CT, &it.Seq); err != nil {
			return nil, 0, false, wrap("读取记录失败", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, wrap("读取记录失败", err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	var nextSeq int64 = sinceSeq
	if len(out) > 0 {
		nextSeq = out[len(out)-1].Seq
	}
	return out, nextSeq, hasMore, nil
}
