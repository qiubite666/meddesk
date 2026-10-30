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

// ListPending 返回当前账号全部待报销支出（不受月份筛选限制）。
func (s *Service) ListPending(ctx context.Context, userID string) ([]model.LedgerRecord, int64, model.Money, error) {
	if userID == "" {
		return nil, 0, model.ZeroMoney(), apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	f := model.LedgerFilter{
		Type:        model.LedgerTypeExpense,
		ReimbStatus: model.LedgerReimbPending,
		Page:        1,
		Size:        200,
	}
	list, total, err := s.repo.ListRecords(ctx, userID, f)
	if err != nil {
		return nil, 0, model.ZeroMoney(), apierr.Wrap(apierr.CodeInternal, err, "读取待报销失败")
	}
	// 合计必须走 SQL 聚合求**全量**，不能只累加当前页：
	// 列表最多取 200 条，而 total 是全量条数。若用首页金额当合计，
	// 待报销超过 200 笔时前端会出现"共 350 笔，合计 ¥1,200"（实为前 200 笔金额），
	// 条数与金额口径自相矛盾，且误差随分页放大。
	_, expenseCents, err := s.repo.SumRecords(ctx, userID, model.LedgerFilter{
		Type:        model.LedgerTypeExpense,
		ReimbStatus: model.LedgerReimbPending,
	}, "", "")
	if err != nil {
		return nil, 0, model.ZeroMoney(), apierr.Wrap(apierr.CodeInternal, err, "统计待报销金额失败")
	}
	return list, total, model.MoneyFromCents(expenseCents), nil
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
