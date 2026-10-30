// Package ledger 实现记账本：账单与分类的增删改查、收支统计与报销管理。
//
// 【架构说明】
// 与本项目的健康数据不同，记账数据**明文建表**——服务端需要做金额聚合、分类统计
// 与每日自动备份，端到端加密下这些都无从谈起。这是用户明确确认过的取舍：
// 账本明文落库，健康数据仍保持端到端加密。设置页与文档必须向用户明示这一点。
//
// 【金额口径】
// 数据库 numeric(14,2)，Go 侧以字符串（精确值）与整数分（计算值）传递，
// 全程不出现浮点运算（见 model.ParseAmount / model.MoneyFromCents）。
package ledger

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// shanghai 是账本统一使用的时区（所有自然日、月度口径都按它计算）。
//
// 容器镜像常常不带 tzdata，LoadLocation 会失败，此时退化为固定的 +08:00 偏移——
// 中国目前不使用夏令时，固定偏移与 Asia/Shanghai 等价，不会算错日期。
var shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}()

// RecordFileCleanup 是「删除账单前清理磁盘凭证」的钩子。
//
// 【为什么必须在删除记录之前调用】
// ledger_attachments 对 ledger_records 是 ON DELETE CASCADE，一旦记录被删，
// rel_path 就取不到了，磁盘上的图片会变成永久孤儿。因此附件服务要求：
// 先按 record_id 取出路径并删文件，再删记录。
type RecordFileCleanup func(ctx context.Context, userID, recordID string) error

// Service 组装记账本依赖。
type Service struct {
	repo store.LedgerRepo
	// cleanup 由附件模块注入（B4）。未注入时删除账单只删记录，不碰磁盘。
	cleanup RecordFileCleanup
}

// NewService 构造记账本服务。
func NewService(repo store.LedgerRepo) *Service {
	return &Service{repo: repo}
}

// SetFileCleanup 注入凭证清理钩子（由路由在附件模块就绪后调用）。
func (s *Service) SetFileCleanup(fn RecordFileCleanup) { s.cleanup = fn }

// ------------------------------------------------------------------ 输入

// RecordInput 是新增/编辑账单的请求体。
type RecordInput struct {
	Type                string `json:"type"`
	Amount              string `json:"amount"`
	Currency            string `json:"currency"`
	CategoryID          string `json:"categoryId"`
	CategoryName        string `json:"categoryName"` // 分类缺失时的兜底快照（导入场景）
	TransactionAt       string `json:"transactionAt"`
	Note                string `json:"note"`
	ReimbursementStatus string `json:"reimbursementStatus"`
}

// CategoryInput 是新增/编辑分类的请求体。
type CategoryInput struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
	Archived  bool   `json:"archived"`
}

// CategoryPatch 是分类的**局部更新**入参，仅 PATCH /ledger/categories/:id 使用。
//
// 为什么不用 CategoryInput：值类型无法区分「没传」与「显式传零值」。
// 若直接 `c.SortOrder = in.SortOrder`，前端只想改名字时（body 里没带 sortOrder），
// 排序会被静默清零、archived 被静默置回 false —— 属于不易察觉的数据丢失。
// 这里对 sortOrder / archived 用指针，nil 即表示"本次不改动"。
type CategoryPatch struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	SortOrder *int   `json:"sortOrder"`
	Archived  *bool  `json:"archived"`
}

// ------------------------------------------------------------------ 分类

// ListCategories 返回分类列表；首次访问自动播种内置分类。
func (s *Service) ListCategories(ctx context.Context, userID, kind string, includeArchived bool) ([]model.LedgerCategory, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if kind != "" && kind != model.LedgerTypeIncome && kind != model.LedgerTypeExpense {
		return nil, apierr.New(apierr.CodeBadRequest, "kind 只能是 income 或 expense")
	}
	if err := s.repo.EnsureCategories(ctx, userID); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "初始化分类失败")
	}
	list, err := s.repo.ListCategories(ctx, userID, kind, includeArchived)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取分类失败")
	}
	return list, nil
}

func (s *Service) CreateCategory(ctx context.Context, userID string, in CategoryInput) (*model.LedgerCategory, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	kind, name, err := normalizeCategory(in.Kind, in.Name)
	if err != nil {
		return nil, err
	}
	c := &model.LedgerCategory{
		UserID: userID, Kind: kind, Name: name,
		SortOrder: in.SortOrder, Archived: in.Archived,
	}
	if err := s.repo.CreateCategory(ctx, c); err != nil {
		return nil, wrapRepoErr(err, "创建分类失败")
	}
	return c, nil
}

// UpdateCategory 部分更新：只有**显式传入**的字段才覆盖（零值也认，nil 才跳过）。
func (s *Service) UpdateCategory(ctx context.Context, userID, id string, in CategoryPatch) (*model.LedgerCategory, error) {
	c, err := s.mustOwnCategory(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if in.Kind != "" {
		if in.Kind != model.LedgerTypeIncome && in.Kind != model.LedgerTypeExpense {
			return nil, apierr.New(apierr.CodeBadRequest, "kind 只能是 income 或 expense")
		}
		c.Kind = in.Kind
	}
	if strings.TrimSpace(in.Name) != "" {
		c.Name = strings.TrimSpace(in.Name)
		if len([]rune(c.Name)) > 32 {
			return nil, apierr.New(apierr.CodeBadRequest, "分类名最多 32 个字")
		}
	}
	// 指针字段：nil = 本次不改动，非 nil = 按传入值写（含 0 / false）。
	if in.SortOrder != nil {
		c.SortOrder = *in.SortOrder
	}
	if in.Archived != nil {
		c.Archived = *in.Archived
	}
	if err := s.repo.UpdateCategory(ctx, c); err != nil {
		return nil, wrapRepoErr(err, "更新分类失败")
	}
	return c, nil
}

// DeleteCategory 删除分类；已被账单引用的不允许删除（应改用"停用"）。
func (s *Service) DeleteCategory(ctx context.Context, userID, id string) error {
	if _, err := s.mustOwnCategory(ctx, userID, id); err != nil {
		return err
	}
	if err := s.repo.DeleteCategory(ctx, userID, id); err != nil {
		// 已被引用的情况在 SQL 层被挡掉（RowsAffected=0 → ErrNotFound）。
		return wrapRepoErr(err, "删除分类失败；若该分类已被账单使用，请改用「停用」")
	}
	return nil
}

func (s *Service) mustOwnCategory(ctx context.Context, userID, id string) (*model.LedgerCategory, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	c, err := s.repo.GetCategory(ctx, userID, id)
	if err != nil {
		// 越权与不存在都返回 404：不向调用方透露资源是否存在。
		return nil, notFoundOrInternal(err, "分类不存在")
	}
	return c, nil
}

func normalizeCategory(kind, name string) (string, string, error) {
	if kind != model.LedgerTypeIncome && kind != model.LedgerTypeExpense {
		return "", "", apierr.New(apierr.CodeBadRequest, "kind 只能是 income 或 expense")
	}
	n := strings.TrimSpace(name)
	if n == "" {
		return "", "", apierr.New(apierr.CodeBadRequest, "分类名不能为空")
	}
	if len([]rune(n)) > 32 {
		return "", "", apierr.New(apierr.CodeBadRequest, "分类名最多 32 个字")
	}
	return kind, n, nil
}

// ------------------------------------------------------------------ 账单

// CreateRecord 新增账单。
func (s *Service) CreateRecord(ctx context.Context, userID string, in RecordInput) (*model.LedgerRecord, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if err := s.repo.EnsureCategories(ctx, userID); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "初始化分类失败")
	}
	rec, err := s.buildRecord(userID, in, nil)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateRecord(ctx, rec); err != nil {
		return nil, wrapRepoErr(err, "保存账单失败")
	}
	return rec, nil
}

// UpdateRecord 编辑账单（整体覆盖，未提供的字段沿用原值以外的值会被清空，
// 因此前端总是提交完整对象）。
func (s *Service) UpdateRecord(ctx context.Context, userID, id string, in RecordInput) (*model.LedgerRecord, error) {
	old, err := s.mustOwnRecord(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	rec, err := s.buildRecord(userID, in, old)
	if err != nil {
		return nil, err
	}
	rec.ID = id
	if err := s.repo.UpdateRecord(ctx, rec); err != nil {
		return nil, wrapRepoErr(err, "更新账单失败")
	}
	return rec, nil
}

func (s *Service) GetRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error) {
	return s.mustOwnRecord(ctx, userID, id)
}

func (s *Service) DeleteRecord(ctx context.Context, userID, id string) error {
	if _, err := s.mustOwnRecord(ctx, userID, id); err != nil {
		return err
	}
	// 先清磁盘凭证再删记录：记录一删，外键 CASCADE 会把附件元数据一起带走，
	// 到时候就再也拿不到 rel_path，磁盘图片只能变孤儿。
	if s.cleanup != nil {
		if err := s.cleanup(ctx, userID, id); err != nil {
			// 清理失败不阻断删除：元数据删除是用户明确要求的动作，
			// 残留文件由后续清理任务处理，这里只记日志。
			slog.Warn("清理账单凭证失败", "record", id, "error", err)
		}
	}
	if err := s.repo.DeleteRecord(ctx, userID, id); err != nil {
		return wrapRepoErr(err, "删除账单失败")
	}
	return nil
}

func (s *Service) mustOwnRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	rec, err := s.repo.GetRecord(ctx, userID, id)
	if err != nil {
		return nil, notFoundOrInternal(err, "账单不存在")
	}
	return rec, nil
}

// buildRecord 校验输入并组装账单；old 不为 nil 时表示编辑，用于兜底分类快照。
func (s *Service) buildRecord(userID string, in RecordInput, old *model.LedgerRecord) (*model.LedgerRecord, error) {
	typ := strings.TrimSpace(in.Type)
	if typ != model.LedgerTypeIncome && typ != model.LedgerTypeExpense {
		return nil, apierr.New(apierr.CodeBadRequest, "type 只能是 income 或 expense")
	}

	amount, cents, err := model.ParseAmount(in.Amount)
	if err != nil {
		return nil, apierr.New(apierr.CodeBadRequest, "金额必须是大于 0 的数字，最多两位小数")
	}

	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = "CNY"
	}
	if len(currency) != 3 {
		return nil, apierr.New(apierr.CodeBadRequest, "币种需为 3 位代码（默认 CNY）")
	}

	// 分类：优先按 id 查（校验归属与类型），取当前名称作为快照；
	// 没有 id 时退回用户提交的名称（导入场景），但不允许两者都空。
	categoryID := ""
	categoryName := strings.TrimSpace(in.CategoryName)
	if id := strings.TrimSpace(in.CategoryID); id != "" {
		c, err := s.repo.GetCategory(context.Background(), userID, id)
		if err != nil {
			return nil, notFoundOrInternal(err, "分类不存在")
		}
		if c.Kind != typ {
			return nil, apierr.New(apierr.CodeBadRequest, "分类与账单类型不匹配")
		}
		categoryID = c.ID
		categoryName = c.Name
	}
	if categoryName == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "请选择分类")
	}
	if len([]rune(categoryName)) > 32 {
		return nil, apierr.New(apierr.CodeBadRequest, "分类名最多 32 个字")
	}

	transactionAt, err := parseTransactionAt(in.TransactionAt)
	if err != nil {
		return nil, err
	}

	note := strings.TrimSpace(in.Note)
	if len([]rune(note)) > 500 {
		return nil, apierr.New(apierr.CodeBadRequest, "备注最多 500 个字")
	}

	status := strings.TrimSpace(in.ReimbursementStatus)
	if status == "" {
		status = model.LedgerReimbNone
	}
	if typ == model.LedgerTypeIncome {
		// 收入不进入报销流程：直接归一为 none，而不是报错（前端可能沿用上一个值）。
		status = model.LedgerReimbNone
	}
	if status != model.LedgerReimbNone && status != model.LedgerReimbPending && status != model.LedgerReimbDone {
		return nil, apierr.New(apierr.CodeBadRequest, "报销状态只能是 none / pending / done")
	}

	rec := &model.LedgerRecord{
		UserID:              userID,
		Type:                typ,
		Amount:              amount,
		AmountCents:         cents,
		Currency:            currency,
		CategoryID:          categoryID,
		CategoryName:        categoryName,
		TransactionAt:       transactionAt,
		OccurredOn:          transactionAt.In(shanghai).Format("2006-01-02"),
		Note:                note,
		ReimbursementStatus: status,
	}
	return rec, nil
}

// parseTransactionAt 接受 RFC3339 或 "2006-01-02T15:04"；留空取当前时刻。
func parseTransactionAt(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Now().In(shanghai), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, shanghai); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, shanghai); err == nil {
		return t, nil
	}
	return time.Time{}, apierr.New(apierr.CodeBadRequest, "发生时间格式有误（建议 YYYY-MM-DDTHH:mm）")
}

// ------------------------------------------------------------------ 列表与统计

func (s *Service) ListRecords(ctx context.Context, userID string, f model.LedgerFilter) ([]model.LedgerRecord, int64, error) {
	if userID == "" {
		return nil, 0, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	list, total, err := s.repo.ListRecords(ctx, userID, f)
	if err != nil {
		return nil, 0, apierr.Wrap(apierr.CodeInternal, err, "读取账单失败")
	}
	return list, total, nil
}

// Summary 返回区间汇总（结余 = 收入 - 支出）。
func (s *Service) Summary(ctx context.Context, userID string, f model.LedgerFilter, from, to string) (*model.LedgerSummary, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	income, expense, err := s.repo.SumRecords(ctx, userID, f, from, to)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "统计收支失败")
	}
	return &model.LedgerSummary{
		From:    from,
		To:      to,
		Income:  model.MoneyFromCents(income),
		Expense: model.MoneyFromCents(expense),
		Balance: model.MoneyFromCents(income - expense),
	}, nil
}

// Trend 返回 [from, to] 区间内逐日收支（无数据的日子补 0）。
func (s *Service) Trend(ctx context.Context, userID string, f model.LedgerFilter, from, to string) ([]model.LedgerTrendPoint, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	points, err := s.repo.DailyTrend(ctx, userID, f, from, to)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "统计趋势失败")
	}
	return points, nil
}

// Breakdown 返回分类金额，收入与支出分组，不得混算。
func (s *Service) Breakdown(ctx context.Context, userID string, f model.LedgerFilter, from, to string) (income, expense []model.LedgerCategoryAmount, err error) {
	if userID == "" {
		return nil, nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	income, expense, err = s.repo.CategoryBreakdown(ctx, userID, f, from, to)
	if err != nil {
		return nil, nil, apierr.Wrap(apierr.CodeInternal, err, "统计分类金额失败")
	}
	return income, expense, nil
}

// ------------------------------------------------------------------ 报销

// 报销列表的默认值与上限。
//
// 为什么默认只查近 30 天而不是"全部"：报销场景是"这个月的账要报"，而全量扫描
// 会随账本增长越来越慢；需要更早的数据时前端显式传 from/to 或 days 即可。
const (
	reimbDefaultDays = 30  // days 缺省/非法时的默认区间天数
	reimbDefaultSize = 50  // 报销列表每页条数（比账单列表的 20 大，一次能看到"要报的一批"）
	reimbMaxSize     = 200 // 与仓储层 ListRecords 的硬上限保持一致
)

// ReimbQuery 是报销列表的查询条件，由 HTTP 层的查询参数解析而来。
type ReimbQuery struct {
	Status string // pending（默认）| done | none | all
	From   string // 起始自然日 YYYY-MM-DD（含），可选
	To     string // 结束自然日 YYYY-MM-DD（含），可选
	Days   int    // 仅在 From/To 都未提供时生效，默认 reimbDefaultDays
	Page   int
	Size   int
}

// ReimbCounts 是三种报销状态在**同一时间区间**内的条数。
type ReimbCounts struct {
	Pending int64 `json:"pending"`
	Done    int64 `json:"done"`
	None    int64 `json:"none"`
}

// ReimbSummary 是报销看板的汇总数字：两个金额 + 三个计数。
type ReimbSummary struct {
	PendingTotal model.Money `json:"pendingTotal"`
	DoneTotal    model.Money `json:"doneTotal"`
	Counts       ReimbCounts `json:"counts"`
}

// ReimbRange 回显本次查询实际生效的自然日区间，前端据此显示"近 30 天"之类的文案。
type ReimbRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ReimbPage 是 GET /ledger/reimbursements 的完整响应。
type ReimbPage struct {
	Items []model.LedgerRecord `json:"items"`
	Total int64                `json:"total"`
	Page  int                  `json:"page"`
	Size  int                  `json:"size"`
	// PendingTotal 是**兼容字段**，不能删：旧版前端只读顶层的 pendingTotal
	// 来渲染"待报销合计"，字段一旦挪进 summary，旧客户端会读到零值并显示 ¥0.00，
	// 用户会以为账目出错。它的语义是"当前时间区间内 status=pending 的支出合计"，
	// 与 summary.pendingTotal 恒等，等旧版本客户端全部退场后才能移除。
	PendingTotal model.Money  `json:"pendingTotal"`
	Summary      ReimbSummary `json:"summary"`
	Range        ReimbRange   `json:"range"`
}

// ListReimbursements 按状态/时间区间分页返回报销账单，并附带汇总。
//
// 【为什么不复用旧的待报销查询】
// 旧实现固定 ReimbStatus=pending、Size=200、无时间条件：用户把记录标成「已报销」
// 之后它就从这个列表里消失，看起来像"列表被清空了"，也无法查历史。现在状态、
// 区间、分页全部由调用方决定，默认仍是 pending + 近 30 天，行为对老用户最接近。
//
// 【为什么只统计支出】
// 收入不进入报销流程（buildRecord 会把收入强制归一为 none）。若不过滤 type，
// status=none 的计数会把所有工资、红包都算进去，与"报销"语义无关。
func (s *Service) ListReimbursements(ctx context.Context, userID string, q ReimbQuery) (*ReimbPage, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	status, err := reimbStatusFilter(q.Status)
	if err != nil {
		return nil, err
	}
	from, to, err := reimbRange(q.From, q.To, q.Days)
	if err != nil {
		return nil, err
	}
	page, size := normalizeReimbPaging(q.Page, q.Size)

	// base 是"支出 + 区间"的公共条件，状态由各查询自己覆盖。
	base := model.LedgerFilter{
		Type: model.LedgerTypeExpense,
		From: from,
		To:   to,
	}

	// 明细：分页 + 状态筛选。
	listFilter := base
	listFilter.ReimbStatus = status
	listFilter.Page, listFilter.Size = page, size
	list, total, err := s.repo.ListRecords(ctx, userID, listFilter)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取报销列表失败")
	}
	if list == nil {
		list = []model.LedgerRecord{} // 保证 JSON 输出 []，不输出 null
	}

	// 金额合计必须走 SQL 聚合求全量，不能累加当前页：分页后首页金额与 total
	// 口径不一致，条数越多误差越大（原实现注释里已经踩过这个坑）。
	sumOf := func(st string) (model.Money, error) {
		f := base
		f.ReimbStatus = st
		_, expenseCents, err := s.repo.SumRecords(ctx, userID, f, from, to)
		if err != nil {
			return model.ZeroMoney(), err
		}
		return model.MoneyFromCents(expenseCents), nil
	}
	pendingTotal, err := sumOf(model.LedgerReimbPending)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "统计待报销金额失败")
	}
	doneTotal, err := sumOf(model.LedgerReimbDone)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "统计已报销金额失败")
	}

	// 三个状态计数各查一次（Size=1 只取 total，不取数据）。
	// 取舍：SUM(CASE WHEN ...) 一次查完确实更省，但仓储层不在本次改动范围，
	// 而这三个计数是报销页的首屏数据、区间通常只有 30 天，三条 count 的代价
	// 远小于为此新增一个仓储方法与一次 SQL 评审；真正的性能瓶颈在明细查询。
	countOf := func(st string) (int64, error) {
		f := base
		f.ReimbStatus = st
		f.Page, f.Size = 1, 1
		_, n, err := s.repo.ListRecords(ctx, userID, f)
		return n, err
	}
	var counts ReimbCounts
	for _, it := range []struct {
		status string
		dst    *int64
	}{
		{model.LedgerReimbPending, &counts.Pending},
		{model.LedgerReimbDone, &counts.Done},
		{model.LedgerReimbNone, &counts.None},
	} {
		n, err := countOf(it.status)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeInternal, err, "统计报销条数失败")
		}
		*it.dst = n
	}

	return &ReimbPage{
		Items:        list,
		Total:        total,
		Page:         page,
		Size:         size,
		PendingTotal: pendingTotal,
		Summary: ReimbSummary{
			PendingTotal: pendingTotal,
			DoneTotal:    doneTotal,
			Counts:       counts,
		},
		Range: ReimbRange{From: from, To: to},
	}, nil
}

// reimbStatusFilter 把 status 参数映射成仓储的 ReimbStatus 过滤值。
// 空值默认 pending（保持旧接口"进来就是看待报销"的语义）；all 表示不过滤。
func reimbStatusFilter(status string) (string, error) {
	s := strings.TrimSpace(status)
	switch s {
	case "":
		return model.LedgerReimbPending, nil
	case model.LedgerReimbPending, model.LedgerReimbDone, model.LedgerReimbNone:
		return s, nil
	case "all":
		return "", nil
	default:
		return "", apierr.New(apierr.CodeBadRequest, "status 只能是 pending / done / none / all")
	}
}

// reimbRange 计算生效的自然日区间。
//
// 只要 from/to 传了任意一个就以它们为准（另一端留空表示不设边界），
// 两者都没传时按 days 反推：from = 今天 - (days-1)、to = 今天。
// 日期一律按上海时区算——账本的"今天"必须与用户看到的一致，不能用 UTC。
func reimbRange(fromRaw, toRaw string, days int) (string, string, error) {
	from := strings.TrimSpace(fromRaw)
	to := strings.TrimSpace(toRaw)
	for _, v := range []string{from, to} {
		if v == "" {
			continue
		}
		// datePattern 与导出模块共用：用户输入的自然日必须先校验再进 SQL。
		if !datePattern.MatchString(v) {
			return "", "", apierr.New(apierr.CodeBadRequest, "日期格式应为 YYYY-MM-DD")
		}
		if _, err := time.ParseInLocation("2006-01-02", v, shanghai); err != nil {
			return "", "", apierr.New(apierr.CodeBadRequest, "日期不是有效日期")
		}
	}
	// YYYY-MM-DD 是定长格式，字符串比较即时间先后。
	if from != "" && to != "" && from > to {
		return "", "", apierr.New(apierr.CodeBadRequest, "开始日期不能晚于结束日期")
	}
	if from == "" && to == "" {
		if days <= 0 {
			days = reimbDefaultDays
		}
		today := time.Now().In(shanghai)
		from = today.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
		to = today.Format("2006-01-02")
	}
	return from, to, nil
}

// normalizeReimbPaging 夹取分页参数，规则与 /records 保持一致。
func normalizeReimbPaging(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = reimbDefaultSize
	}
	if size > reimbMaxSize {
		size = reimbMaxSize
	}
	return page, size
}

// MarkReimbursement 批量改报销状态。只处理该账号名下、且确实是支出的记录。
func (s *Service) MarkReimbursement(ctx context.Context, userID string, ids []string, status string) (int64, error) {
	if userID == "" {
		return 0, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if status != model.LedgerReimbPending && status != model.LedgerReimbDone && status != model.LedgerReimbNone {
		return 0, apierr.New(apierr.CodeBadRequest, "报销状态只能是 none / pending / done")
	}
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			clean = append(clean, strings.TrimSpace(id))
		}
	}
	if len(clean) == 0 {
		return 0, apierr.New(apierr.CodeBadRequest, "请先选择要操作的账单")
	}
	n, err := s.repo.MarkReimbursement(ctx, userID, clean, status)
	if err != nil {
		return 0, apierr.Wrap(apierr.CodeInternal, err, "更新报销状态失败")
	}
	return n, nil
}

// ------------------------------------------------------------------ 工具

// MonthRange 返回 month（YYYY-MM）在上海时区下的 [首日, 末日]。
func MonthRange(month string) (string, string) {
	now := time.Now().In(shanghai)
	m := strings.TrimSpace(month)
	if m == "" {
		m = now.Format("2006-01")
	}
	start, err := time.ParseInLocation("2006-01", m, shanghai)
	if err != nil {
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, shanghai)
	}
	end := start.AddDate(0, 1, -1)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// RecentDaysRange 返回最近 n 个自然日（含今天）的 [起, 止]。
func RecentDaysRange(n int) (string, string) {
	if n <= 0 {
		n = 7
	}
	today := time.Now().In(shanghai)
	from := today.AddDate(0, 0, -(n - 1))
	return from.Format("2006-01-02"), today.Format("2006-01-02")
}

// Today 返回上海时区的今天（YYYY-MM-DD），供默认区间使用。
func Today() string { return time.Now().In(shanghai).Format("2006-01-02") }

// wrapRepoErr 把仓储错误统一成对外错误：未找到 → 404，冲突 → 409，其余 → 500。
func wrapRepoErr(err error, msg string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return apierr.Wrap(apierr.CodeNotFound, err, msg)
	}
	// 必须用 errors.As 而不是类型断言：仓储层的冲突错误是 store 包内部的
	// conflictError 类型（它 Unwrap 出内层的 *apierr.Error）。直接断言
	// err.(*apierr.Error) 永远为 false，会把"同名分类已存在"这类冲突
	// 错误错误地映射成 500，前端收到 internal 无法给出可读提示。
	var e *apierr.Error
	if errors.As(err, &e) {
		return e
	}
	return apierr.Wrap(apierr.CodeInternal, err, msg)
}

// notFoundOrInternal 越权与不存在统一按 404 处理，避免泄露资源是否存在。
func notFoundOrInternal(err error, msg string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apierr.New(apierr.CodeNotFound, msg)
	}
	return apierr.Wrap(apierr.CodeInternal, err, msg)
}
