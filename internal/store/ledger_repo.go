package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/qiubite666/meddesk/internal/model"
)

// ledgerRepo 是记账本仓储的 PostgreSQL 实现。
//
// 【账号隔离】每个方法都把 user_id 作为首个过滤条件写进 SQL，
// 服务端不信任请求体里的任何归属字段——越权在 SQL 层就被挡住。
type ledgerRepo struct{ s *Store }

// LedgerRepo 是记账本所需的数据访问能力（由 *Store 实现）。
type LedgerRepo interface {
	// 分类
	EnsureCategories(ctx context.Context, userID string) error
	ListCategories(ctx context.Context, userID, kind string, includeArchived bool) ([]model.LedgerCategory, error)
	CreateCategory(ctx context.Context, c *model.LedgerCategory) error
	GetCategory(ctx context.Context, userID, id string) (*model.LedgerCategory, error)
	UpdateCategory(ctx context.Context, c *model.LedgerCategory) error
	DeleteCategory(ctx context.Context, userID, id string) error
	CategoryUsage(ctx context.Context, userID, id string) (int64, error)

	// 账单
	CreateRecord(ctx context.Context, r *model.LedgerRecord) error
	GetRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error)
	UpdateRecord(ctx context.Context, r *model.LedgerRecord) error
	DeleteRecord(ctx context.Context, userID, id string) error
	ListRecords(ctx context.Context, userID string, f model.LedgerFilter) ([]model.LedgerRecord, int64, error)

	// 统计（金额聚合在 SQL 内用 numeric 完成，不经过浮点）
	SumRecords(ctx context.Context, userID string, f model.LedgerFilter, from, to string) (incomeCents, expenseCents int64, err error)
	DailyTrend(ctx context.Context, userID string, f model.LedgerFilter, from, to string) ([]model.LedgerTrendPoint, error)
	CategoryBreakdown(ctx context.Context, userID string, f model.LedgerFilter, from, to string) (income, expense []model.LedgerCategoryAmount, err error)

	// 报销
	MarkReimbursement(ctx context.Context, userID string, ids []string, status string) (int64, error)
}

// Ledger 返回记账本仓储。
func (s *Store) Ledger() LedgerRepo { return ledgerRepo{s} }

// 账单查询列：amount 转成 text 保证精度；occurred_on 转 text 便于直接返回；
// attachment_count 用左连接子查询，避免逐条再查。
const selectLedgerRecord = `SELECT r.id, r.user_id, r.type, r.amount::text, r.currency,
       COALESCE(r.category_id::text, ''), r.category_name_snapshot, r.transaction_at,
       r.occurred_on::text, r.note, r.reimbursement_status, COALESCE(a.n, 0),
       r.created_at, r.updated_at
FROM ledger_records r
LEFT JOIN (SELECT record_id, count(*) AS n FROM ledger_attachments GROUP BY record_id) a
       ON a.record_id = r.id`

func scanLedgerRecord(row pgx.Row) (*model.LedgerRecord, error) {
	var r model.LedgerRecord
	var categoryID string
	err := row.Scan(&r.ID, &r.UserID, &r.Type, &r.Amount, &r.Currency, &categoryID,
		&r.CategoryName, &r.TransactionAt, &r.OccurredOn, &r.Note, &r.ReimbursementStatus,
		&r.AttachmentCount, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	r.CategoryID = categoryID
	// 金额派生：PostgreSQL 的 numeric 已保证精度，这里只做字符串→整数分。
	_, cents, err := model.ParseAmount(r.Amount)
	if err != nil {
		return nil, wrap("账单金额异常", err)
	}
	r.AmountCents = cents
	return &r, nil
}

// 内置分类（首次使用时播种，按用户隔离）。
var defaultLedgerCategories = []struct {
	kind string
	name string
}{
	{model.LedgerTypeIncome, "工资"},
	{model.LedgerTypeIncome, "奖金"},
	{model.LedgerTypeIncome, "投资收益"},
	{model.LedgerTypeIncome, "兼职"},
	{model.LedgerTypeIncome, "其他收入"},
	{model.LedgerTypeExpense, "餐饮"},
	{model.LedgerTypeExpense, "交通"},
	{model.LedgerTypeExpense, "购物"},
	{model.LedgerTypeExpense, "住房"},
	{model.LedgerTypeExpense, "水电"},
	{model.LedgerTypeExpense, "医疗"},
	{model.LedgerTypeExpense, "教育"},
	{model.LedgerTypeExpense, "娱乐"},
	{model.LedgerTypeExpense, "数码"},
	{model.LedgerTypeExpense, "日用品"},
	{model.LedgerTypeExpense, "其他支出"},
}

// EnsureCategories 首次访问时为该账号写入内置分类；已有分类则原样返回。
//
// 幂等：ON CONFLICT DO NOTHING，重复执行不会产生重复行。
func (r ledgerRepo) EnsureCategories(ctx context.Context, userID string) error {
	p, err := uuidParam(userID)
	if err != nil {
		return err
	}
	var count int
	if err := r.s.pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_categories WHERE user_id = $1`, p).Scan(&count); err != nil {
		return wrap("查询分类失败", err)
	}
	if count > 0 {
		return nil
	}
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return wrap("开启事务失败", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const q = `INSERT INTO ledger_categories (user_id, kind, name, sort_order)
VALUES ($1, $2, $3, $4) ON CONFLICT (user_id, kind, name) DO NOTHING`
	for i, c := range defaultLedgerCategories {
		if _, err := tx.Exec(ctx, q, p, c.kind, c.name, i); err != nil {
			return wrap("写入内置分类失败", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return wrap("提交内置分类失败", err)
	}
	return nil
}

func (r ledgerRepo) ListCategories(ctx context.Context, userID, kind string, includeArchived bool) ([]model.LedgerCategory, error) {
	p, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, user_id, kind, name, sort_order, archived, created_at, updated_at
FROM ledger_categories WHERE user_id = $1`
	args := []any{p}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	if !includeArchived {
		q += " AND archived = false"
	}
	q += " ORDER BY kind, sort_order, name"

	rows, err := r.s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, wrap("查询分类失败", err)
	}
	defer rows.Close()

	out := make([]model.LedgerCategory, 0, len(defaultLedgerCategories))
	for rows.Next() {
		var c model.LedgerCategory
		if err := rows.Scan(&c.ID, &c.UserID, &c.Kind, &c.Name, &c.SortOrder, &c.Archived, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, wrap("读取分类失败", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r ledgerRepo) CreateCategory(ctx context.Context, c *model.LedgerCategory) error {
	if c == nil {
		return wrap("创建分类失败", errors.New("分类对象为空"))
	}
	if c.ID == "" {
		c.ID = newUUID()
	}
	id, err := uuidParam(c.ID)
	if err != nil {
		return err
	}
	uid, err := uuidParam(c.UserID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO ledger_categories (id, user_id, kind, name, sort_order, archived)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at, updated_at`
	err = r.s.pool.QueryRow(ctx, q, id, uid, c.Kind, c.Name, c.SortOrder, c.Archived).
		Scan(&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return newConflict("同名分类已存在")
		}
		return wrap("创建分类失败", err)
	}
	return nil
}

func (r ledgerRepo) GetCategory(ctx context.Context, userID, id string) (*model.LedgerCategory, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	cid, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	var c model.LedgerCategory
	const q = `SELECT id, user_id, kind, name, sort_order, archived, created_at, updated_at
FROM ledger_categories WHERE user_id = $1 AND id = $2`
	if err := r.s.pool.QueryRow(ctx, q, uid, cid).Scan(&c.ID, &c.UserID, &c.Kind, &c.Name,
		&c.SortOrder, &c.Archived, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (r ledgerRepo) UpdateCategory(ctx context.Context, c *model.LedgerCategory) error {
	if c == nil {
		return wrap("更新分类失败", errors.New("分类对象为空"))
	}
	uid, err := uuidParam(c.UserID)
	if err != nil {
		return err
	}
	cid, err := uuidParam(c.ID)
	if err != nil {
		return err
	}
	const q = `UPDATE ledger_categories SET name = $3, sort_order = $4, archived = $5
WHERE user_id = $1 AND id = $2`
	tag, err := r.s.pool.Exec(ctx, q, uid, cid, c.Name, c.SortOrder, c.Archived)
	if err != nil {
		if isUniqueViolation(err) {
			return newConflict("同名分类已存在")
		}
		return wrap("更新分类失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("更新分类失败", ErrNotFound)
	}
	return nil
}

func (r ledgerRepo) DeleteCategory(ctx context.Context, userID, id string) error {
	uid, err := uuidParam(userID)
	if err != nil {
		return err
	}
	cid, err := uuidParam(id)
	if err != nil {
		return err
	}
	// 只允许删除"没有被任何账单引用"的分类；已被引用的请改用 archived（停用）。
	const q = `DELETE FROM ledger_categories c
WHERE c.user_id = $1 AND c.id = $2
  AND NOT EXISTS (SELECT 1 FROM ledger_records r WHERE r.category_id = c.id)`
	tag, err := r.s.pool.Exec(ctx, q, uid, cid)
	if err != nil {
		return wrap("删除分类失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("删除分类失败（分类不存在或已被账单引用，请改用「停用」）", ErrNotFound)
	}
	return nil
}

func (r ledgerRepo) CategoryUsage(ctx context.Context, userID, id string) (int64, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return 0, err
	}
	cid, err := uuidParam(id)
	if err != nil {
		return 0, err
	}
	var n int64
	const q = `SELECT count(*) FROM ledger_records WHERE user_id = $1 AND category_id = $2`
	if err := r.s.pool.QueryRow(ctx, q, uid, cid).Scan(&n); err != nil {
		return 0, wrap("统计分类引用失败", err)
	}
	return n, nil
}

func (r ledgerRepo) CreateRecord(ctx context.Context, rec *model.LedgerRecord) error {
	if rec == nil {
		return wrap("创建账单失败", errors.New("账单对象为空"))
	}
	if rec.ID == "" {
		rec.ID = newUUID()
	}
	id, err := uuidParam(rec.ID)
	if err != nil {
		return err
	}
	uid, err := uuidParam(rec.UserID)
	if err != nil {
		return err
	}
	var catID any
	if rec.CategoryID != "" {
		v, err := uuidParam(rec.CategoryID)
		if err != nil {
			return err
		}
		catID = v
	}
	occurred, err := parseDate(rec.OccurredOn)
	if err != nil {
		return err
	}
	const q = `INSERT INTO ledger_records
(id, user_id, type, amount, currency, category_id, category_name_snapshot,
 transaction_at, occurred_on, note, reimbursement_status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING created_at, updated_at`
	err = r.s.pool.QueryRow(ctx, q, id, uid, rec.Type, rec.Amount, rec.Currency, catID,
		rec.CategoryName, rec.TransactionAt, occurred, rec.Note, rec.ReimbursementStatus).
		Scan(&rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		// 主键冲突 = 该账单已存在，不是"写入失败"。
		// 并发导入同一份数据时，"先查后插"之间存在竞态：两个请求都判断"不存在"，
		// 于是都去 INSERT，后到的必然撞主键。若这里不区分，上层会把它们记成 failed，
		// 用户看到一堆"写入账单失败"却查不出丢了哪条数据（实际一条没丢）。
		// 转成 ErrConflict 后，上层可准确计为 skipped。
		if isUniqueViolation(err) {
			return newConflict("账单已存在")
		}
		return wrap("创建账单失败", err)
	}
	return nil
}

func (r ledgerRepo) GetRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	rid, err := uuidParam(id)
	if err != nil {
		return nil, err
	}
	row := r.s.pool.QueryRow(ctx, selectLedgerRecord+" WHERE r.user_id = $1 AND r.id = $2", uid, rid)
	rec, err := scanLedgerRecord(row)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (r ledgerRepo) UpdateRecord(ctx context.Context, rec *model.LedgerRecord) error {
	if rec == nil {
		return wrap("更新账单失败", errors.New("账单对象为空"))
	}
	uid, err := uuidParam(rec.UserID)
	if err != nil {
		return err
	}
	rid, err := uuidParam(rec.ID)
	if err != nil {
		return err
	}
	var catID any
	if rec.CategoryID != "" {
		v, err := uuidParam(rec.CategoryID)
		if err != nil {
			return err
		}
		catID = v
	}
	occurred, err := parseDate(rec.OccurredOn)
	if err != nil {
		return err
	}
	const q = `UPDATE ledger_records SET type = $3, amount = $4, currency = $5, category_id = $6,
category_name_snapshot = $7, transaction_at = $8, occurred_on = $9, note = $10,
reimbursement_status = $11 WHERE user_id = $1 AND id = $2`
	tag, err := r.s.pool.Exec(ctx, q, uid, rid, rec.Type, rec.Amount, rec.Currency, catID,
		rec.CategoryName, rec.TransactionAt, occurred, rec.Note, rec.ReimbursementStatus)
	if err != nil {
		return wrap("更新账单失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("更新账单失败", ErrNotFound)
	}
	return nil
}

func (r ledgerRepo) DeleteRecord(ctx context.Context, userID, id string) error {
	uid, err := uuidParam(userID)
	if err != nil {
		return err
	}
	rid, err := uuidParam(id)
	if err != nil {
		return err
	}
	// 凭证附件由外键 ON DELETE CASCADE 连带删除（元数据）；磁盘文件由服务层清理。
	tag, err := r.s.pool.Exec(ctx, `DELETE FROM ledger_records WHERE user_id = $1 AND id = $2`, uid, rid)
	if err != nil {
		return wrap("删除账单失败", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("删除账单失败", ErrNotFound)
	}
	return nil
}

// ledgerWhereFrom 构造筛选条件。user_id 永远是调用方的第一个参数。
//
// startIdx 是"调用方 SQL 里已被占用的最大占位符编号"，本函数生成的占位符从
// startIdx+1 起顺延：
//   - ListRecords / SumRecords / CategoryBreakdown 只占用 $1=user_id，传 1，条件从 $2 起；
//   - DailyTrend 的 $1=user_id、$2/$3=generate_series 的起止日期，必须传 3，条件从 $4 起。
//
// 【占位符起点这个坑】曾经无条件从 $2 起编，导致 DailyTrend 带任何筛选
// （type/reimbStatus/categoryIDs/ids/keyword）时条件撞上 $2（起始日期），
// PostgreSQL 报参数类型不匹配（42883），接口直接 500。所以起点必须由调用方声明，
// 不能默认假设只有 user_id 一个参数。
//
// 【为什么全参数化】关键词、分类 id 都来自用户输入，拼接进 SQL 就是注入入口；
// 这里只把占位符下标拼进去，值一律走 args。
func ledgerWhereFrom(f model.LedgerFilter, from, to string, startIdx int) (string, []any) {
	var sb strings.Builder
	args := []any{}
	idx := startIdx
	next := func(v any) string {
		args = append(args, v)
		idx++
		return fmt.Sprintf("$%d", idx)
	}
	if f.Type != "" {
		sb.WriteString(" AND r.type = " + next(f.Type))
	}
	if f.ReimbStatus != "" {
		sb.WriteString(" AND r.reimbursement_status = " + next(f.ReimbStatus))
	}
	if from != "" {
		sb.WriteString(" AND r.occurred_on >= " + next(from) + "::date")
	}
	if to != "" {
		sb.WriteString(" AND r.occurred_on <= " + next(to) + "::date")
	}
	if len(f.CategoryIDs) > 0 {
		sb.WriteString(" AND r.category_id::text = ANY(" + next(f.CategoryIDs) + ")")
	}
	if len(f.IDs) > 0 {
		sb.WriteString(" AND r.id::text = ANY(" + next(f.IDs) + ")")
	}
	if f.Keyword != "" {
		kw := "%" + strings.ToLower(strings.TrimSpace(f.Keyword)) + "%"
		sb.WriteString(" AND (lower(r.note) LIKE " + next(kw) +
			" OR lower(r.category_name_snapshot) LIKE " + next(kw) + ")")
	}
	return sb.String(), args
}

func (r ledgerRepo) ListRecords(ctx context.Context, userID string, f model.LedgerFilter) ([]model.LedgerRecord, int64, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, 0, err
	}
	cond, args := ledgerWhereFrom(f, f.From, f.To, 1) // $1=user_id，条件从 $2 起

	size := f.Size
	if size <= 0 || size > 200 {
		size = 20
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * size

	var total int64
	countArgs := append([]any{uid}, args...)
	if err := r.s.pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_records r WHERE r.user_id = $1`+cond, countArgs...).Scan(&total); err != nil {
		return nil, 0, wrap("统计账单数量失败", err)
	}

	q := selectLedgerRecord + ` WHERE r.user_id = $1` + cond +
		` ORDER BY r.transaction_at DESC, r.id DESC LIMIT ` +
		fmt.Sprintf("$%d", len(args)+2) + ` OFFSET ` + fmt.Sprintf("$%d", len(args)+3)
	qArgs := append([]any{uid}, args...)
	qArgs = append(qArgs, size, offset)

	rows, err := r.s.pool.Query(ctx, q, qArgs...)
	if err != nil {
		return nil, 0, wrap("查询账单失败", err)
	}
	defer rows.Close()

	out := make([]model.LedgerRecord, 0, size)
	for rows.Next() {
		rec, err := scanLedgerRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, wrap("读取账单失败", err)
	}
	return out, total, nil
}

func (r ledgerRepo) SumRecords(ctx context.Context, userID string, f model.LedgerFilter, from, to string) (int64, int64, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return 0, 0, err
	}
	// 关键：聚合在 numeric 上完成（精确），再乘 100 转 bigint 给前端使用。
	// 若直接 SUM 后转 float 就会出现 0.1+0.2 的误差，账本不能接受。
	const q = `SELECT
  COALESCE((SUM(amount) FILTER (WHERE type = 'income')  * 100)::bigint, 0),
  COALESCE((SUM(amount) FILTER (WHERE type = 'expense') * 100)::bigint, 0)
FROM ledger_records r WHERE r.user_id = $1`
	cond, args := ledgerWhereFrom(f, from, to, 1) // $1=user_id，条件从 $2 起
	qArgs := append([]any{uid}, args...)
	var income, expense int64
	if err := r.s.pool.QueryRow(ctx, q+cond, qArgs...).Scan(&income, &expense); err != nil {
		return 0, 0, wrap("统计收支失败", err)
	}
	return income, expense, nil
}

// dailyTrendSQL 组装趋势查询。$1=user_id、$2/$3=generate_series 的起止日期，
// 因此筛选条件必须从 $4 起编（见 ledgerWhereFrom 的起点说明）。
//
// 注意：这里**不能**把 from/to 再交给 ledgerWhereFrom——区间已由 generate_series 表达，
// 再传一次会让日期条件与序列参数互相错位；传空串即可。
// generate_series 保证"没有数据的日子补 0"，前端不必自己补齐。
func dailyTrendSQL(f model.LedgerFilter, uid any, from, to string) (string, []any) {
	// 起始下标必须是 3（不是 1）：否则 type/keyword 等条件会生成 $2 并撞上起始日期，
	// PostgreSQL 报 42883，趋势接口 500。
	cond, args := ledgerWhereFrom(f, "", "", 3)
	q := `SELECT d::date::text,
       COALESCE((SUM(r.amount) FILTER (WHERE r.type = 'income')  * 100)::bigint, 0),
       COALESCE((SUM(r.amount) FILTER (WHERE r.type = 'expense') * 100)::bigint, 0)
FROM generate_series($2::date, $3::date, '1 day') AS d
LEFT JOIN ledger_records r
  ON r.user_id = $1 AND r.occurred_on = d::date` + cond + `
GROUP BY d ORDER BY d`
	return q, append([]any{uid, from, to}, args...)
}

func (r ledgerRepo) DailyTrend(ctx context.Context, userID string, f model.LedgerFilter, from, to string) ([]model.LedgerTrendPoint, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, err
	}
	q, qArgs := dailyTrendSQL(f, uid, from, to)
	rows, err := r.s.pool.Query(ctx, q, qArgs...)
	if err != nil {
		return nil, wrap("统计趋势失败", err)
	}
	defer rows.Close()

	out := []model.LedgerTrendPoint{}
	for rows.Next() {
		var date string
		var income, expense int64
		if err := rows.Scan(&date, &income, &expense); err != nil {
			return nil, wrap("读取趋势失败", err)
		}
		out = append(out, model.LedgerTrendPoint{
			Date:    date,
			Income:  model.MoneyFromCents(income),
			Expense: model.MoneyFromCents(expense),
		})
	}
	return out, rows.Err()
}

func (r ledgerRepo) CategoryBreakdown(ctx context.Context, userID string, f model.LedgerFilter, from, to string) ([]model.LedgerCategoryAmount, []model.LedgerCategoryAmount, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return nil, nil, err
	}
	cond, args := ledgerWhereFrom(f, from, to, 1) // $1=user_id，条件从 $2 起
	// 收入与支出分组统计：按 type 分组，两个结果集合并成一个查询再由 Go 拆分，
	// 保证口径一致（同一 WHERE、同一聚合方式），绝不会把收入算进支出。
	q := `SELECT r.type,
       COALESCE(r.category_id::text, ''),
       COALESCE(r.category_name_snapshot, ''),
       COALESCE((SUM(r.amount) * 100)::bigint, 0)
FROM ledger_records r WHERE r.user_id = $1` + cond + `
GROUP BY r.type, r.category_id, r.category_name_snapshot
ORDER BY 4 DESC`
	qArgs := append([]any{uid}, args...)
	rows, err := r.s.pool.Query(ctx, q, qArgs...)
	if err != nil {
		return nil, nil, wrap("统计分类金额失败", err)
	}
	defer rows.Close()

	income := []model.LedgerCategoryAmount{}
	expense := []model.LedgerCategoryAmount{}
	for rows.Next() {
		var typ, catID, name string
		var cents int64
		if err := rows.Scan(&typ, &catID, &name, &cents); err != nil {
			return nil, nil, wrap("读取分类金额失败", err)
		}
		item := model.LedgerCategoryAmount{CategoryID: catID, Name: name, Money: model.MoneyFromCents(cents)}
		if typ == model.LedgerTypeIncome {
			income = append(income, item)
		} else {
			expense = append(expense, item)
		}
	}
	return income, expense, rows.Err()
}

func (r ledgerRepo) MarkReimbursement(ctx context.Context, userID string, ids []string, status string) (int64, error) {
	uid, err := uuidParam(userID)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// 只更新该账号名下、且确实是支出的记录：收入不允许进入报销流程。
	const q = `UPDATE ledger_records SET reimbursement_status = $3
WHERE user_id = $1 AND id::text = ANY($2) AND type = 'expense'`
	tag, err := r.s.pool.Exec(ctx, q, uid, ids, status)
	if err != nil {
		return 0, wrap("更新报销状态失败", err)
	}
	return tag.RowsAffected(), nil
}

// parseDate 解析 YYYY-MM-DD（账单的 occurred_on 与查询区间都用它）。
func parseDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), time.UTC)
	if err != nil {
		return time.Time{}, wrap("日期格式有误（应为 YYYY-MM-DD）", err)
	}
	return t, nil
}
