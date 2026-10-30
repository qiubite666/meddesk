package fake

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ledgerRepo 是 store.LedgerRepo 的内存实现，用于集成冒烟测试。
//
// 目的与边界（与真实实现对齐的部分）：
//   - 语义尽量贴近 PostgreSQL 版本：账号隔离、分页按时间倒序、金额为字符串+整数分、
//     趋势按自然日补 0、分类金额按 type 分组不混算；
//   - 不具备事务与并发控制（测试串行执行，且真实实现由 PG 事务保证）。
//
// 这样在没有 PostgreSQL 的机器上也能跑完整的 HTTP 链路与越权用例。
type ledgerRepo struct{ r *Repos }

// Ledger 返回记账本仓储（内存实现）。
func (r *Repos) Ledger() store.LedgerRepo { return ledgerRepo{r} }

var _ store.LedgerRepo = ledgerRepo{}

// ledgerStore 是内存中的账本数据，由 Repos 的互斥锁保护。
type ledgerStore struct {
	categories map[string]*model.LedgerCategory // id -> 分类
	records    map[string]*model.LedgerRecord   // id -> 账单
	order      []string                         // 账单写入顺序（稳定排序兜底）
}

func (r ledgerRepo) ls() *ledgerStore {
	if r.r.ledger == nil {
		r.r.ledger = &ledgerStore{
			categories: make(map[string]*model.LedgerCategory),
			records:    make(map[string]*model.LedgerRecord),
		}
	}
	return r.r.ledger
}

// ------------------------------------------------------------------ 分类

func (r ledgerRepo) EnsureCategories(ctx context.Context, userID string) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	ls := r.ls()
	for _, c := range ls.categories {
		if c.UserID == userID {
			return nil // 已有分类，不重复播种
		}
	}
	builtin := []struct {
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
	now := time.Now()
	for i, b := range builtin {
		c := &model.LedgerCategory{
			ID:        newUUIDForTest(),
			UserID:    userID,
			Kind:      b.kind,
			Name:      b.name,
			SortOrder: i,
			CreatedAt: now,
			UpdatedAt: now,
		}
		ls.categories[c.ID] = c
	}
	return nil
}

func (r ledgerRepo) ListCategories(ctx context.Context, userID, kind string, includeArchived bool) ([]model.LedgerCategory, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	out := []model.LedgerCategory{}
	for _, c := range r.ls().categories {
		if c.UserID != userID {
			continue
		}
		if kind != "" && c.Kind != kind {
			continue
		}
		if !includeArchived && c.Archived {
			continue
		}
		cp := *c
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (r ledgerRepo) CreateCategory(ctx context.Context, c *model.LedgerCategory) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	if c.ID == "" {
		c.ID = newUUIDForTest()
	}
	// 同名冲突（对齐 UNIQUE(user_id, kind, name)）
	for _, exist := range r.ls().categories {
		if exist.UserID == c.UserID && exist.Kind == c.Kind && exist.Name == c.Name {
			return store.ErrConflict
		}
	}
	now := time.Now()
	c.CreatedAt, c.UpdatedAt = now, now
	r.ls().categories[c.ID] = c
	return nil
}

func (r ledgerRepo) GetCategory(ctx context.Context, userID, id string) (*model.LedgerCategory, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	c, ok := r.ls().categories[id]
	if !ok || c.UserID != userID {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (r ledgerRepo) UpdateCategory(ctx context.Context, c *model.LedgerCategory) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	old, ok := r.ls().categories[c.ID]
	if !ok || old.UserID != c.UserID {
		return store.ErrNotFound
	}
	for _, exist := range r.ls().categories {
		if exist.ID != c.ID && exist.UserID == c.UserID && exist.Kind == c.Kind && exist.Name == c.Name {
			return store.ErrConflict
		}
	}
	c.UpdatedAt = time.Now()
	c.CreatedAt = old.CreatedAt
	r.ls().categories[c.ID] = c
	return nil
}

func (r ledgerRepo) DeleteCategory(ctx context.Context, userID, id string) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	c, ok := r.ls().categories[id]
	if !ok || c.UserID != userID {
		return store.ErrNotFound
	}
	// 已被账单引用则拒绝删除（对齐 SQL 里的 NOT EXISTS 条件）
	for _, rec := range r.ls().records {
		if rec.UserID == userID && rec.CategoryID == id {
			return store.ErrNotFound
		}
	}
	delete(r.ls().categories, id)
	return nil
}

func (r ledgerRepo) CategoryUsage(ctx context.Context, userID, id string) (int64, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	var n int64
	for _, rec := range r.ls().records {
		if rec.UserID == userID && rec.CategoryID == id {
			n++
		}
	}
	return n, nil
}

// ------------------------------------------------------------------ 账单

func (r ledgerRepo) CreateRecord(ctx context.Context, rec *model.LedgerRecord) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	if rec.ID == "" {
		rec.ID = newUUIDForTest()
	}
	now := time.Now()
	rec.CreatedAt, rec.UpdatedAt = now, now
	ls := r.ls()
	cp := *rec
	ls.records[rec.ID] = &cp
	ls.order = append(ls.order, rec.ID)
	return nil
}

func (r ledgerRepo) GetRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	rec, ok := r.ls().records[id]
	if !ok || rec.UserID != userID {
		return nil, store.ErrNotFound
	}
	cp := *rec
	return &cp, nil
}

func (r ledgerRepo) UpdateRecord(ctx context.Context, rec *model.LedgerRecord) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	old, ok := r.ls().records[rec.ID]
	if !ok || old.UserID != rec.UserID {
		return store.ErrNotFound
	}
	rec.UpdatedAt = time.Now()
	rec.CreatedAt = old.CreatedAt
	cp := *rec
	r.ls().records[rec.ID] = &cp
	return nil
}

func (r ledgerRepo) DeleteRecord(ctx context.Context, userID, id string) error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	if _, ok := r.ls().records[id]; !ok {
		return store.ErrNotFound
	}
	if r.ls().records[id].UserID != userID {
		return store.ErrNotFound
	}
	delete(r.ls().records, id)
	ls := r.ls()
	for i, rid := range ls.order {
		if rid == id {
			ls.order = append(ls.order[:i], ls.order[i+1:]...)
			break
		}
	}
	return nil
}

// matchFilter 判断账单是否命中筛选条件（与 SQL 版 ledgerWhere 一一对应）。
func matchFilter(rec *model.LedgerRecord, f model.LedgerFilter, from, to string) bool {
	if f.Type != "" && rec.Type != f.Type {
		return false
	}
	if f.ReimbStatus != "" && rec.ReimbursementStatus != f.ReimbStatus {
		return false
	}
	// 与 SQL 版一致：ReimbStatus 为空时，ReimbExcludeNone 排除「不涉及」。
	if f.ReimbStatus == "" && f.ReimbExcludeNone && rec.ReimbursementStatus == model.LedgerReimbNone {
		return false
	}
	if from != "" && rec.OccurredOn < from {
		return false
	}
	if to != "" && rec.OccurredOn > to {
		return false
	}
	if len(f.CategoryIDs) > 0 && !containsString(f.CategoryIDs, rec.CategoryID) {
		return false
	}
	if len(f.IDs) > 0 && !containsString(f.IDs, rec.ID) {
		return false
	}
	if f.Keyword != "" {
		kw := strings.ToLower(strings.TrimSpace(f.Keyword))
		if !strings.Contains(strings.ToLower(rec.Note), kw) &&
			!strings.Contains(strings.ToLower(rec.CategoryName), kw) {
			return false
		}
	}
	return true
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (r ledgerRepo) ListRecords(ctx context.Context, userID string, f model.LedgerFilter) ([]model.LedgerRecord, int64, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	matched := []*model.LedgerRecord{}
	for _, rec := range r.ls().records {
		if rec.UserID != userID {
			continue
		}
		if !matchFilter(rec, f, f.From, f.To) {
			continue
		}
		matched = append(matched, rec)
	}
	// 发生时间倒序，id 兜底保证稳定
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].TransactionAt.Equal(matched[j].TransactionAt) {
			return matched[i].TransactionAt.After(matched[j].TransactionAt)
		}
		return matched[i].ID > matched[j].ID
	})

	size := f.Size
	if size <= 0 {
		size = 20
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	total := int64(len(matched))
	start := (page - 1) * size
	if start > len(matched) {
		start = len(matched)
	}
	end := start + size
	if end > len(matched) {
		end = len(matched)
	}
	out := make([]model.LedgerRecord, 0, end-start)
	for _, rec := range matched[start:end] {
		out = append(out, *rec)
	}
	return out, total, nil
}

func (r ledgerRepo) SumRecords(ctx context.Context, userID string, f model.LedgerFilter, from, to string) (int64, int64, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	var income, expense int64
	for _, rec := range r.ls().records {
		if rec.UserID != userID || !matchFilter(rec, f, from, to) {
			continue
		}
		if rec.Type == model.LedgerTypeIncome {
			income += rec.AmountCents
		} else {
			expense += rec.AmountCents
		}
	}
	return income, expense, nil
}

func (r ledgerRepo) DailyTrend(ctx context.Context, userID string, f model.LedgerFilter, from, to string) ([]model.LedgerTrendPoint, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	// 逐日补 0：与 SQL 的 generate_series 行为一致
	days := []string{}
	cur, err := time.ParseInLocation("2006-01-02", from, time.UTC)
	if err != nil {
		return nil, err
	}
	end, err := time.ParseInLocation("2006-01-02", to, time.UTC)
	if err != nil {
		return nil, err
	}
	for !cur.After(end) {
		days = append(days, cur.Format("2006-01-02"))
		cur = cur.AddDate(0, 0, 1)
	}
	incomeByDay := map[string]int64{}
	expenseByDay := map[string]int64{}
	for _, rec := range r.ls().records {
		if rec.UserID != userID || !matchFilter(rec, f, from, to) {
			continue
		}
		if rec.Type == model.LedgerTypeIncome {
			incomeByDay[rec.OccurredOn] += rec.AmountCents
		} else {
			expenseByDay[rec.OccurredOn] += rec.AmountCents
		}
	}
	out := make([]model.LedgerTrendPoint, 0, len(days))
	for _, d := range days {
		out = append(out, model.LedgerTrendPoint{
			Date:    d,
			Income:  model.MoneyFromCents(incomeByDay[d]),
			Expense: model.MoneyFromCents(expenseByDay[d]),
		})
	}
	return out, nil
}

func (r ledgerRepo) CategoryBreakdown(ctx context.Context, userID string, f model.LedgerFilter, from, to string) ([]model.LedgerCategoryAmount, []model.LedgerCategoryAmount, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	type key struct {
		typ string
		id  string
	}
	sums := map[key]int64{}
	names := map[key]string{}
	for _, rec := range r.ls().records {
		if rec.UserID != userID || !matchFilter(rec, f, from, to) {
			continue
		}
		k := key{typ: rec.Type, id: rec.CategoryID}
		sums[k] += rec.AmountCents
		names[k] = rec.CategoryName
	}
	income := []model.LedgerCategoryAmount{}
	expense := []model.LedgerCategoryAmount{}
	for k, cents := range sums {
		item := model.LedgerCategoryAmount{CategoryID: k.id, Name: names[k], Money: model.MoneyFromCents(cents)}
		if k.typ == model.LedgerTypeIncome {
			income = append(income, item)
		} else {
			expense = append(expense, item)
		}
	}
	sortByAmountDesc(income)
	sortByAmountDesc(expense)
	return income, expense, nil
}

func sortByAmountDesc(list []model.LedgerCategoryAmount) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Money.Cents != list[j].Money.Cents {
			return list[i].Money.Cents > list[j].Money.Cents
		}
		return list[i].Name < list[j].Name
	})
}

func (r ledgerRepo) MarkReimbursement(ctx context.Context, userID string, ids []string, status string) (int64, error) {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	var n int64
	for _, id := range ids {
		rec, ok := r.ls().records[id]
		if !ok || rec.UserID != userID {
			continue
		}
		// 收入不进入报销流程（与 SQL 的 type='expense' 条件一致）
		if rec.Type != model.LedgerTypeExpense {
			continue
		}
		rec.ReimbursementStatus = status
		rec.UpdatedAt = time.Now()
		n++
	}
	return n, nil
}

// newUUIDForTest 生成测试用的 id（不引入额外依赖，直接用 google/uuid）。
func newUUIDForTest() string {
	return uuid.NewString()
}
