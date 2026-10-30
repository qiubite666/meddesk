package model

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// errBadAmount 是金额校验失败的哨兵错误；由上层包装成 400 返回给前端。
var errBadAmount = errors.New("金额必须是大于 0 的数字，最多两位小数")

// 记账本（ledger）模型。
//
// 【金额为什么不用 float】
// 金额一旦经过二进制浮点就会出现 0.1+0.2 != 0.3 这类误差，账本是绝对不能接受的。
// 数据库用 numeric(14,2)；Go 侧一律以**字符串**（精确值）与**整数分**（计算值）两种
// 形式传递，全程不出现浮点运算。

// 记账类型与报销状态的取值。
const (
	LedgerTypeIncome  = "income"
	LedgerTypeExpense = "expense"

	LedgerReimbNone    = "none"    // 无需报销（收入的唯一取值）
	LedgerReimbPending = "pending" // 待报销
	LedgerReimbDone    = "done"    // 已报销
)

// amountPattern 约束"最多 12 位整数 + 最多 2 位小数"，且不允许符号、指数、千分位。
var amountPattern = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)

// ParseAmount 校验并归一化金额，返回 (规范化字符串, 整数分, error)。
//
// 归一化示例："12" → "12.00"/1200；"0.5" → "0.50"/50；"007.10" → "7.10"/710。
// 全程整数运算（把元与分拼成一个整数再 ParseInt），不经过任何浮点。
func ParseAmount(raw string) (string, int64, error) {
	s := strings.TrimSpace(raw)
	if !amountPattern.MatchString(s) {
		return "", 0, errBadAmount
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	// 去前导零，避免 "007" 这类输入把整数解析成八进制之外的意外（ParseInt 已按 10 进制，
	// 这里主要是为了得到干净的展示字符串）。
	trimmed := strings.TrimLeft(intPart, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	cents, err := strconv.ParseInt(trimmed+frac, 10, 64)
	if err != nil {
		return "", 0, errBadAmount
	}
	if cents <= 0 {
		return "", 0, errBadAmount
	}
	return trimmed + "." + frac, cents, nil
}

// Money 是金额的对外表示：value 用于展示（精确字符串），cents 用于计算与图表。
type Money struct {
	Value string `json:"value"`
	Cents int64  `json:"cents"`
}

// ZeroMoney 返回 0 元，供"无数据补 0"使用。
func ZeroMoney() Money { return Money{Value: "0.00", Cents: 0} }

// MoneyFromCents 把整数分转成 Money（cents 由 SQL 聚合得到，精确）。
func MoneyFromCents(cents int64) Money {
	if cents == 0 {
		return ZeroMoney()
	}
	neg := cents < 0
	abs := cents
	if neg {
		abs = -abs
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return Money{
		Value: sign + strconv.FormatInt(abs/100, 10) + "." + pad2(abs%100),
		Cents: cents,
	}
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// LedgerCategory 是账本分类。
type LedgerCategory struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sortOrder"`
	Archived  bool      `json:"archived"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LedgerRecord 是账单。AmountCents / AttachmentCount 是派生字段，不落库。
type LedgerRecord struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"-"`
	Type                string    `json:"type"`
	Amount              string    `json:"amount"`
	AmountCents         int64     `json:"amountCents"`
	Currency            string    `json:"currency"`
	CategoryID          string    `json:"categoryId"`
	CategoryName        string    `json:"categoryName"`
	TransactionAt       time.Time `json:"transactionAt"`
	OccurredOn          string    `json:"occurredOn"`
	Note                string    `json:"note"`
	ReimbursementStatus string    `json:"reimbursementStatus"`
	AttachmentCount     int       `json:"attachmentCount"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// LedgerFilter 是账单筛选条件。所有字段都可为空（表示不过滤）。
type LedgerFilter struct {
	Type        string   // income | expense | ""
	CategoryIDs []string // 多选分类
	Keyword     string   // 关键词：覆盖备注与分类名
	From        string   // 起始自然日 YYYY-MM-DD（含）
	To          string   // 结束自然日 YYYY-MM-DD（含）
	ReimbStatus string   // none | pending | done | ""
	// ReimbExcludeNone 在 ReimbStatus 为空时生效：排除「不涉及报销」的记录。
	// 报销管理页的 all 语义是「所有参与报销的记录」（pending + done），而不是
	// 「所有支出」——不涉及报销的记录出现在报销页没有意义，还会被误标成已报销。
	ReimbExcludeNone bool
	IDs              []string // 指定 id（批量导出/批量操作用）
	Page             int
	Size             int
}

// LedgerSummary 是月度（或任意区间）汇总。
type LedgerSummary struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Income  Money  `json:"income"`
	Expense Money  `json:"expense"`
	Balance Money  `json:"balance"` // 结余 = 收入 - 支出
}

// LedgerTrendPoint 是趋势图上的一个点（按自然日）。
type LedgerTrendPoint struct {
	Date    string `json:"date"`
	Income  Money  `json:"income"`
	Expense Money  `json:"expense"`
}

// LedgerCategoryAmount 是分类金额。收入与支出分组返回，不得混算。
type LedgerCategoryAmount struct {
	CategoryID string `json:"categoryId"`
	Name       string `json:"name"`
	Money      Money  `json:"money"`
}

// ErrBadAmount 供上层（服务/接口）判断金额校验失败。
func ErrBadAmount() error { return errBadAmount }
