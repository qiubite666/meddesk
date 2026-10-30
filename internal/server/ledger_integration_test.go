package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/model"
)

// TestLedgerIntegration 用内存仓储跑记账本 HTTP 全链路，重点是**两个账号之间的越权**。
//
// 覆盖：内置分类播种、账单增删查改、金额定点数口径、校验规则、报销状态、
// 汇总/趋势/分类金额，以及账号 A 与账号 B 互不可见（含按 id 直接访问与批量操作）。
func TestLedgerIntegration(t *testing.T) {
	e := newEnv(t)

	// 两个独立账号：A 建数据，B 试图访问 A 的数据。
	tokenA := e.ledgerAccount("ledger-a@meddesk.test")
	tokenB := e.ledgerAccount("ledger-b@meddesk.test")
	if tokenA == "" || tokenB == "" || tokenA == tokenB {
		t.Fatalf("两个账号应拿到各自不同的令牌：A=%q B=%q", tokenA, tokenB)
	}

	today := time.Now().Format("2006-01-02")

	// ---------------------------------------------------------------- 分类
	var catResp struct {
		Items []model.LedgerCategory `json:"items"`
		Total int                    `json:"total"`
	}
	resp, raw := e.do(http.MethodGet, "/api/v1/ledger/categories", nil, tokenA)
	e.wantStatus(resp, "A 取分类", http.StatusOK)
	e.decode(raw, "A 取分类", &catResp)
	if len(catResp.Items) != 16 {
		t.Fatalf("内置分类应播种 16 条（5 收入 + 11 支出），实际 %d", len(catResp.Items))
	}

	var food model.LedgerCategory
	for _, c := range catResp.Items {
		if c.Kind == model.LedgerTypeExpense && c.Name == "餐饮" {
			food = c
		}
	}
	if food.ID == "" {
		t.Fatal("内置分类应包含支出「餐饮」")
	}

	// B 的分类是独立的一份（同样是 16 条，但 id 不同）
	var catRespB struct {
		Items []model.LedgerCategory `json:"items"`
	}
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/categories", nil, tokenB)
	e.wantStatus(resp, "B 取分类", http.StatusOK)
	e.decode(raw, "B 取分类", &catRespB)
	if len(catRespB.Items) == 0 {
		t.Fatal("B 应拿到自己的分类")
	}
	for _, c := range catRespB.Items {
		if c.ID == food.ID {
			t.Fatalf("分类必须按账号隔离：B 看到了 A 的分类 id=%s", food.ID)
		}
	}

	// ---------------------------------------------------------------- 新增账单
	newRec := map[string]interface{}{
		"type":          model.LedgerTypeExpense,
		"amount":        "12.34",
		"categoryId":    food.ID,
		"transactionAt": today + "T12:00:00+08:00",
		"note":          "午饭",
	}
	resp, raw = e.do(http.MethodPost, "/api/v1/ledger/records", newRec, tokenA)
	e.wantStatus(resp, "A 新增支出", http.StatusCreated)
	var recA model.LedgerRecord
	e.decode(raw, "A 新增支出", &recA)

	if recA.Amount != "12.34" {
		t.Fatalf("金额应按定点数原样返回 12.34，实际 %q", recA.Amount)
	}
	if recA.AmountCents != 1234 {
		t.Fatalf("amountCents 应为 1234，实际 %d（不能用浮点换算）", recA.AmountCents)
	}
	if recA.CategoryName != "餐饮" {
		t.Fatalf("应写入分类名快照「餐饮」，实际 %q", recA.CategoryName)
	}
	if recA.ReimbursementStatus != model.LedgerReimbNone {
		t.Fatalf("默认报销状态应为 none，实际 %q", recA.ReimbursementStatus)
	}
	if recA.OccurredOn != today {
		t.Fatalf("occurredOn 应为发生时间所在的自然日 %s，实际 %s", today, recA.OccurredOn)
	}

	// ---------------------------------------------------------------- 金额校验
	badAmounts := []string{"0", "-1", "12.345", "abc", "1e3", ""}
	for _, bad := range badAmounts {
		body := map[string]interface{}{
			"type": model.LedgerTypeExpense, "amount": bad, "categoryId": food.ID,
			"transactionAt": today + "T12:00:00+08:00",
		}
		resp, _ := e.do(http.MethodPost, "/api/v1/ledger/records", body, tokenA)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("金额 %q 应被拒绝（400），实际 %d", bad, resp.StatusCode)
		}
	}

	// 类型与分类不匹配
	mismatch := map[string]interface{}{
		"type": model.LedgerTypeIncome, "amount": "1.00", "categoryId": food.ID,
		"transactionAt": today + "T12:00:00+08:00",
	}
	resp, _ = e.do(http.MethodPost, "/api/v1/ledger/records", mismatch, tokenA)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("收入使用支出分类应被拒绝（400），实际 %d", resp.StatusCode)
	}

	// 收入不进入报销流程：即使提交 pending 也归一为 none
	var incomeCat model.LedgerCategory
	for _, c := range catResp.Items {
		if c.Kind == model.LedgerTypeIncome && c.Name == "工资" {
			incomeCat = c
		}
	}
	incomeRec := map[string]interface{}{
		"type": model.LedgerTypeIncome, "amount": "5000", "categoryId": incomeCat.ID,
		"transactionAt": today + "T09:00:00+08:00", "note": "十月工资",
		"reimbursementStatus": model.LedgerReimbPending,
	}
	resp, raw = e.do(http.MethodPost, "/api/v1/ledger/records", incomeRec, tokenA)
	e.wantStatus(resp, "A 新增收入", http.StatusCreated)
	var recIncome model.LedgerRecord
	e.decode(raw, "A 新增收入", &recIncome)
	if recIncome.ReimbursementStatus != model.LedgerReimbNone {
		t.Fatalf("收入的报销状态必须归一为 none，实际 %q", recIncome.ReimbursementStatus)
	}
	if recIncome.Amount != "5000.00" {
		t.Fatalf("金额应归一化到两位小数 5000.00，实际 %q", recIncome.Amount)
	}

	// ---------------------------------------------------------------- 越权
	var listB struct {
		Items []model.LedgerRecord `json:"items"`
		Total int64                `json:"total"`
	}
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/records", nil, tokenB)
	e.wantStatus(resp, "B 取账单列表", http.StatusOK)
	e.decode(raw, "B 取账单列表", &listB)
	if listB.Total != 0 || len(listB.Items) != 0 {
		t.Fatalf("B 不应看到 A 的任何账单，实际 total=%d", listB.Total)
	}

	// 按 id 直取 / 改 / 删，一律 404（不泄露资源是否存在）
	resp, _ = e.do(http.MethodGet, "/api/v1/ledger/records/"+recA.ID, nil, tokenB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B 直取 A 的账单应 404，实际 %d", resp.StatusCode)
	}
	resp, _ = e.do(http.MethodPatch, "/api/v1/ledger/records/"+recA.ID, newRec, tokenB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B 修改 A 的账单应 404，实际 %d", resp.StatusCode)
	}
	resp, _ = e.do(http.MethodDelete, "/api/v1/ledger/records/"+recA.ID, nil, tokenB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B 删除 A 的账单应 404，实际 %d", resp.StatusCode)
	}

	// 批量改报销状态：B 传 A 的 id 应一条都没更新
	markBody := map[string]interface{}{
		"ids": []string{recA.ID}, "status": model.LedgerReimbDone,
	}
	resp, raw = e.do(http.MethodPost, "/api/v1/ledger/reimbursements/mark", markBody, tokenB)
	e.wantStatus(resp, "B 批量改报销", http.StatusOK)
	var marked struct {
		Updated int64 `json:"updated"`
	}
	e.decode(raw, "B 批量改报销", &marked)
	if marked.Updated != 0 {
		t.Fatalf("B 不能改 A 的报销状态，实际更新了 %d 条", marked.Updated)
	}

	// 请求体里夹带 user_id 也不起作用（服务端只认会话）
	spoof := map[string]interface{}{
		"type": model.LedgerTypeExpense, "amount": "9.99", "categoryId": food.ID,
		"transactionAt": today + "T12:00:00+08:00", "userId": "anything",
	}
	resp, _ = e.do(http.MethodPost, "/api/v1/ledger/records", spoof, tokenA)
	e.wantStatus(resp, "A 带无关 userId 新增", http.StatusCreated)

	// ---------------------------------------------------------------- 统计口径
	var sum model.LedgerSummary
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/summary", nil, tokenA)
	e.wantStatus(resp, "A 月度汇总", http.StatusOK)
	e.decode(raw, "A 月度汇总", &sum)
	// A 本月：收入 5000.00 + 支出 12.34 + 9.99 = 22.33
	if sum.Income.Cents != 500000 {
		t.Fatalf("本月收入应为 500000 分，实际 %d", sum.Income.Cents)
	}
	if sum.Expense.Cents != 2233 {
		t.Fatalf("本月支出应为 2233 分（12.34 + 9.99），实际 %d", sum.Expense.Cents)
	}
	if sum.Balance.Cents != 500000-2233 {
		t.Fatalf("结余应为 收入-支出=%d，实际 %d", 500000-2233, sum.Balance.Cents)
	}
	if sum.Expense.Value != "22.33" {
		t.Fatalf("支出字符串应为 22.33，实际 %q（说明走了浮点）", sum.Expense.Value)
	}

	// B 的汇总必须是 0（不能算到 A 的数据）
	var sumB model.LedgerSummary
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/summary", nil, tokenB)
	e.wantStatus(resp, "B 月度汇总", http.StatusOK)
	e.decode(raw, "B 月度汇总", &sumB)
	if sumB.Income.Cents != 0 || sumB.Expense.Cents != 0 || sumB.Balance.Cents != 0 {
		t.Fatalf("B 的汇总应全为 0，实际 %+v", sumB)
	}

	// 趋势：7 天，无数据的日子补 0
	var trend struct {
		From string                   `json:"from"`
		To   string                   `json:"to"`
		Days []model.LedgerTrendPoint `json:"days"`
	}
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/trend", nil, tokenA)
	e.wantStatus(resp, "A 趋势", http.StatusOK)
	e.decode(raw, "A 趋势", &trend)
	if len(trend.Days) != 7 {
		t.Fatalf("趋势应返回 7 个自然日，实际 %d", len(trend.Days))
	}
	if trend.Days[len(trend.Days)-1].Date != today {
		t.Fatalf("趋势最后一天应为今天 %s，实际 %s", today, trend.Days[len(trend.Days)-1].Date)
	}

	// 分类金额：收入与支出分组，不混算
	var breakdown struct {
		Income  []model.LedgerCategoryAmount `json:"income"`
		Expense []model.LedgerCategoryAmount `json:"expense"`
	}
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/breakdown", nil, tokenA)
	e.wantStatus(resp, "A 分类金额", http.StatusOK)
	e.decode(raw, "A 分类金额", &breakdown)
	var expenseSum int64
	for _, it := range breakdown.Expense {
		expenseSum += it.Money.Cents
		if it.Name == "" {
			t.Fatal("分类金额必须带分类名（快照）")
		}
	}
	if expenseSum != 2233 {
		t.Fatalf("支出分类金额合计应为 2233 分，实际 %d", expenseSum)
	}
	for _, it := range breakdown.Income {
		if it.Name == "餐饮" {
			t.Fatalf("分类金额混算了：收入分组里出现了支出分类 %q", it.Name)
		}
	}

	// ---------------------------------------------------------------- 报销流程
	// 把 A 的那笔支出标记为待报销，再批量标为已报销
	resp, _ = e.do(http.MethodPatch, "/api/v1/ledger/records/"+recA.ID, map[string]interface{}{
		"type": model.LedgerTypeExpense, "amount": "12.34", "categoryId": food.ID,
		"transactionAt": today + "T12:00:00+08:00", "note": "午饭（可报销）",
		"reimbursementStatus": model.LedgerReimbPending,
	}, tokenA)
	e.wantStatus(resp, "A 标记待报销", http.StatusOK)

	var pending struct {
		Items        []model.LedgerRecord `json:"items"`
		Total        int64                `json:"total"`
		PendingTotal model.Money          `json:"pendingTotal"`
	}
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/reimbursements", nil, tokenA)
	e.wantStatus(resp, "A 待报销列表", http.StatusOK)
	e.decode(raw, "A 待报销列表", &pending)
	if pending.Total != 1 {
		t.Fatalf("A 应有 1 条待报销，实际 %d", pending.Total)
	}
	if pending.PendingTotal.Cents != 1234 {
		t.Fatalf("待报销合计应为 1234 分，实际 %d", pending.PendingTotal.Cents)
	}

	resp, raw = e.do(http.MethodPost, "/api/v1/ledger/reimbursements/mark",
		map[string]interface{}{"ids": []string{recA.ID}, "status": model.LedgerReimbDone}, tokenA)
	e.wantStatus(resp, "A 批量标记已报销", http.StatusOK)
	e.decode(raw, "A 批量标记已报销", &marked)
	if marked.Updated != 1 {
		t.Fatalf("A 批量标记应更新 1 条，实际 %d", marked.Updated)
	}

	// ---------------------------------------------------------------- 删除
	resp, _ = e.do(http.MethodDelete, "/api/v1/ledger/records/"+recIncome.ID, nil, tokenA)
	e.wantStatus(resp, "A 删除收入账单", http.StatusOK)
	resp, raw = e.do(http.MethodGet, "/api/v1/ledger/records/"+recIncome.ID, nil, tokenA)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后应查不到，实际 %d", resp.StatusCode)
	}
	_ = json.RawMessage{}
	_ = fmt.Sprint(strings.TrimSpace(""))
}

// ledgerAccount 注册并登录一个账号，返回 access token。
//
// 口令派生值必须用**与注册时相同**的字符串，否则登录校验不过（b64 是确定性的）。
func (e *env) ledgerAccount(email string) string {
	e.t.Helper()
	req := e.registerBodyWithCaptcha()
	req.Email = email
	resp, _ := e.do(http.MethodPost, "/api/v1/auth/register", req, "")
	e.wantStatus(resp, "注册 "+email, http.StatusOK)

	loginReq := map[string]string{
		"email":        email,
		"passwordHash": req.PasswordHash,
	}
	resp, raw := e.do(http.MethodPost, "/api/v1/auth/login", loginReq, "")
	e.wantStatus(resp, "登录 "+email, http.StatusOK)
	var sess model.TokenResponse
	e.decode(raw, "登录 "+email, &sess)
	if sess.AccessToken == "" {
		e.t.Fatalf("登录 %s 未返回 accessToken，响应=%s", email, string(raw))
	}
	return sess.AccessToken
}
