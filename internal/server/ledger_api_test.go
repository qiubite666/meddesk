// 记账本 API 的契约与异常输入测试（B5）。
//
// 与 ledger_integration_test.go 的分工：
//   - 那份是「一条主线跑到底」的冒烟测试，覆盖正常路径与基本越权；
//   - 本文件按 A/B/C/D 四组独立用例展开，重点放在三件事上：
//     1) HTTP 契约的具体错误码（不能只断言"非 200"，否则错误码漂移不会被发现）；
//     2) 跨账号越权必须返回 404 而不是 403/200（不能泄露资源是否存在）；
//     3) 报销状态流转——尤其「标记已报销后 status=all 仍能看到」，这是本轮修复的核心，
//     没有这条断言，回归时会静默退回"标记完就消失"的旧行为。
//
// 全部走 httptest + internal/fake 内存仓储，不连 PostgreSQL、不打真实服务器。
// 每个 t.Run 内部自建 env 与独立账号：用例之间没有共享的可变状态，执行顺序无关。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
)

// apiRecPath 是账单集合的路径前缀（详情/修改/删除都拼 id）。
const apiRecPath = "/api/v1/ledger/records"

// apiEmailSeq 保证同一进程内生成的测试邮箱不重复
// （fake 的用户表按邮箱唯一，重复注册会 409）。
var apiEmailSeq int64

// apiUniqueEmail 生成全局唯一的测试邮箱。
func apiUniqueEmail(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&apiEmailSeq, 1)
	return fmt.Sprintf("ledger-api-%d-%d@meddesk.test", time.Now().UnixNano(), n)
}

// apiNewAccount 注册并登录一个新账号，返回 access token。
// 每个用例都用新账号，等价于「独立的 fake 数据」，避免用例互相污染。
func apiNewAccount(t *testing.T, e *env) string {
	t.Helper()
	return e.ledgerAccount(apiUniqueEmail(t))
}

// apiCategories 取分类列表（首次访问会触发内置分类播种）。
func apiCategories(t *testing.T, e *env, token string) []model.LedgerCategory {
	t.Helper()
	resp, raw := e.do(http.MethodGet, "/api/v1/ledger/categories", nil, token)
	e.wantStatus(resp, "取分类列表", http.StatusOK)
	var out struct {
		Items []model.LedgerCategory `json:"items"`
	}
	e.decode(raw, "取分类列表", &out)
	if len(out.Items) == 0 {
		t.Fatal("取分类列表：应至少播种内置分类，实际为空")
	}
	return out.Items
}

// apiFindCategory 按 kind + name 找内置分类。
func apiFindCategory(t *testing.T, e *env, token, kind, name string) model.LedgerCategory {
	t.Helper()
	list := apiCategories(t, e, token)
	for _, c := range list {
		if c.Kind == kind && c.Name == name {
			return c
		}
	}
	names := make([]string, 0, len(list))
	for _, c := range list {
		names = append(names, c.Kind+"/"+c.Name)
	}
	t.Fatalf("内置分类里找不到 kind=%s name=%s，现有分类：%v", kind, name, names)
	return model.LedgerCategory{}
}

// apiExpenseBody 构造一笔支出的请求体（不带报销状态）。
func apiExpenseBody(catID, amount, note string, at time.Time) map[string]interface{} {
	return apiExpenseBodyStatus(catID, amount, note, at, "")
}

// apiExpenseBodyStatus 同上，但可指定报销状态。
func apiExpenseBodyStatus(catID, amount, note string, at time.Time, status string) map[string]interface{} {
	b := map[string]interface{}{
		"type":          model.LedgerTypeExpense,
		"amount":        amount,
		"categoryId":    catID,
		"transactionAt": at.Format(time.RFC3339),
		"note":          note,
	}
	if status != "" {
		b["reimbursementStatus"] = status
	}
	return b
}

// apiNewRecord 新增一笔账单并断言 201，返回落库后的账单。
func apiNewRecord(t *testing.T, e *env, token string, body map[string]interface{}) model.LedgerRecord {
	t.Helper()
	resp, raw := e.do(http.MethodPost, apiRecPath, body, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("新增账单应 201，实际 %d，响应=%s", resp.StatusCode, string(raw))
	}
	var rec model.LedgerRecord
	e.decode(raw, "新增账单", &rec)
	return rec
}

// apiGetRecord 取账单详情并断言 200。
func apiGetRecord(t *testing.T, e *env, token, id string) model.LedgerRecord {
	t.Helper()
	resp, raw := e.do(http.MethodGet, apiRecPath+"/"+id, nil, token)
	e.wantStatus(resp, "取账单详情", http.StatusOK)
	var rec model.LedgerRecord
	e.decode(raw, "取账单详情", &rec)
	return rec
}

// apiPage 是 GET /ledger/records 的分页响应。
type apiPage struct {
	Items []model.LedgerRecord `json:"items"`
	Total int64                `json:"total"`
	Page  int                  `json:"page"`
	Size  int                  `json:"size"`
}

// apiListRecords 按查询串取账单列表并断言 200。
func apiListRecords(t *testing.T, e *env, token, query string) apiPage {
	t.Helper()
	resp, raw := e.do(http.MethodGet, apiRecPath+query, nil, token)
	e.wantStatus(resp, "取账单列表", http.StatusOK)
	var out apiPage
	e.decode(raw, "取账单列表", &out)
	return out
}

// apiFindRecord 在结果集中按 id 查找。
func apiFindRecord(items []model.LedgerRecord, id string) (model.LedgerRecord, bool) {
	for _, r := range items {
		if r.ID == id {
			return r, true
		}
	}
	return model.LedgerRecord{}, false
}

// apiRecordIDs 返回 id 集合，仅用于失败信息。
func apiRecordIDs(items []model.LedgerRecord) []string {
	ids := make([]string, 0, len(items))
	for _, r := range items {
		ids = append(ids, r.ID)
	}
	return ids
}

// apiMark 调用批量改报销状态并断言 200，返回 updated 条数。
func apiMark(t *testing.T, e *env, token string, ids []string, status string) int64 {
	t.Helper()
	resp, raw := e.do(http.MethodPost, "/api/v1/ledger/reimbursements/mark",
		map[string]interface{}{"ids": ids, "status": status}, token)
	e.wantStatus(resp, "批量改报销状态", http.StatusOK)
	var out struct {
		Updated int64 `json:"updated"`
	}
	e.decode(raw, "批量改报销状态", &out)
	return out.Updated
}

// apiReimbPage 是 GET /ledger/reimbursements 的完整响应（含兼容字段与 summary）。
type apiReimbPage struct {
	Items        []model.LedgerRecord `json:"items"`
	Total        int64                `json:"total"`
	Page         int                  `json:"page"`
	Size         int                  `json:"size"`
	PendingTotal model.Money          `json:"pendingTotal"`
	Summary      struct {
		PendingTotal model.Money `json:"pendingTotal"`
		DoneTotal    model.Money `json:"doneTotal"`
		Counts       struct {
			Pending int64 `json:"pending"`
			Done    int64 `json:"done"`
			None    int64 `json:"none"`
		} `json:"counts"`
	} `json:"summary"`
	Range struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"range"`
}

// apiReimb 取报销列表并断言 200。
func apiReimb(t *testing.T, e *env, token, query string) apiReimbPage {
	t.Helper()
	resp, raw := e.do(http.MethodGet, "/api/v1/ledger/reimbursements"+query, nil, token)
	e.wantStatus(resp, "取报销列表", http.StatusOK)
	var out apiReimbPage
	e.decode(raw, "取报销列表", &out)
	return out
}

// apiSignToken 用指定密钥签一张 access 令牌，用于验证鉴权中间件对
// 过期令牌 / 错签令牌的拒绝行为。
func apiSignToken(t *testing.T, secret string, exp time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": uuid.NewString(),
		"typ": "access",
		"jti": uuid.NewString(),
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": exp.Unix(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("签发测试令牌失败：%v", err)
	}
	return signed
}

// ------------------------------------------------------------------ A. CRUD 全生命周期

// TestLedgerAPICRUD 覆盖单账号的正常路径：分类、账单、详情、修改、列表、删除。
func TestLedgerAPICRUD(t *testing.T) {
	// A1：一条完整链路跑通，确保各接口的"接力"没错位
	// （新建的分类能被账单引用、改完能立刻在列表里看到、删完立刻查不到）。
	t.Run("分类与账单全生命周期", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)

		// 0) 必须先取一次内置分类触发播种。
		// 已知缺陷（见汇报，未改代码）：EnsureCategories 的判据是"该账号是否已有
		// 任意分类"，因此若第一步就是"新建自定义分类"，内置分类将永远不再播种。
		salary := apiFindCategory(t, e, token, model.LedgerTypeIncome, "工资")

		// 1) 新建一个自定义支出分类
		resp, raw := e.do(http.MethodPost, "/api/v1/ledger/categories", map[string]interface{}{
			"kind": model.LedgerTypeExpense, "name": "宠物",
		}, token)
		e.wantStatus(resp, "新建分类", http.StatusCreated)
		var pet model.LedgerCategory
		e.decode(raw, "新建分类", &pet)
		if pet.ID == "" || pet.Kind != model.LedgerTypeExpense || pet.Name != "宠物" {
			t.Fatalf("新建分类返回不符合预期：%+v", pet)
		}

		// 2) 用该分类建一笔支出，用内置分类建一笔收入
		at := time.Date(2026, 10, 20, 12, 30, 0, 0, time.FixedZone("CST", 8*3600))
		exp := apiNewRecord(t, e, token, apiExpenseBody(pet.ID, "12.34", "猫粮", at))

		income := apiNewRecord(t, e, token, map[string]interface{}{
			"type": model.LedgerTypeIncome, "amount": "5000", "categoryId": salary.ID,
			"transactionAt": at.Format(time.RFC3339), "note": "十月工资",
		})
		if income.Amount != "5000.00" {
			t.Fatalf("收入金额应归一化为 5000.00，实际 %q", income.Amount)
		}
		if income.ReimbursementStatus != model.LedgerReimbNone {
			t.Fatalf("收入报销状态必须归一为 none，实际 %q", income.ReimbursementStatus)
		}

		// 3) 查详情：金额、分类快照、备注都要原样回来
		got := apiGetRecord(t, e, token, exp.ID)
		if got.Amount != "12.34" || got.AmountCents != 1234 {
			t.Fatalf("详情金额应为 12.34 / 1234 分，实际 %q / %d", got.Amount, got.AmountCents)
		}
		if got.CategoryName != "宠物" || got.Note != "猫粮" {
			t.Fatalf("详情分类快照/备注不符：name=%q note=%q", got.CategoryName, got.Note)
		}

		// 4) 改金额与备注（PATCH 是整体覆盖，前端总是提交完整对象）
		resp, raw = e.do(http.MethodPatch, apiRecPath+"/"+exp.ID,
			apiExpenseBody(pet.ID, "99.90", "猫粮+猫砂", at), token)
		e.wantStatus(resp, "改金额与备注", http.StatusOK)
		var updated model.LedgerRecord
		e.decode(raw, "改金额与备注", &updated)
		if updated.Amount != "99.90" || updated.AmountCents != 9990 || updated.Note != "猫粮+猫砂" {
			t.Fatalf("修改未生效：amount=%q cents=%d note=%q", updated.Amount, updated.AmountCents, updated.Note)
		}

		// 5) 列表能看到两笔，且金额是新值（不能在列表里读到缓存旧值）
		page := apiListRecords(t, e, token, "?size=50")
		if page.Total != 2 || len(page.Items) != 2 {
			t.Fatalf("列表应有 2 条，实际 total=%d len=%d", page.Total, len(page.Items))
		}
		listed, ok := apiFindRecord(page.Items, exp.ID)
		if !ok {
			t.Fatalf("列表里找不到刚改过的账单 %s，实际有 %v", exp.ID, apiRecordIDs(page.Items))
		}
		if listed.Amount != "99.90" || listed.Note != "猫粮+猫砂" {
			t.Fatalf("列表里的金额/备注不是最新值：%q / %q", listed.Amount, listed.Note)
		}

		// 6) 删除后再查必须 404（删除是幂等语义上的"查不到"，不是 500）
		resp, _ = e.do(http.MethodDelete, apiRecPath+"/"+exp.ID, nil, token)
		e.wantStatus(resp, "删除账单", http.StatusOK)
		resp, raw = e.do(http.MethodGet, apiRecPath+"/"+exp.ID, nil, token)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("删除后详情应 404，实际 %d", resp.StatusCode)
		}
		e.wantCode(raw, "删除后详情", apierr.CodeNotFound)

		// 7) 列表里也不能再有它
		page = apiListRecords(t, e, token, "?size=50")
		if page.Total != 1 {
			t.Fatalf("删除后列表应剩 1 条，实际 %d", page.Total)
		}
		if _, found := apiFindRecord(page.Items, exp.ID); found {
			t.Fatalf("删除后列表仍能查到 %s", exp.ID)
		}
	})

	// A2：锁定金额字段的实际 JSON 形态。
	//
	// 契约是**平铺**的：amount 为字符串（精确值），amountCents 为整数分。
	// 若哪天有人把它改成 {value,cents} 嵌套对象，前端与导出会同时崩，
	// 所以这里直接对原始 JSON 做类型断言，而不是只看结构体反序列化结果
	// （结构体对嵌套对象的字段缺失是静默零值，测不出来）。
	t.Run("金额字段JSON形态", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		cat := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")
		rec := apiNewRecord(t, e, token, apiExpenseBody(cat.ID, "12.34", "午饭", time.Now()))

		resp, raw := e.do(http.MethodGet, apiRecPath+"/"+rec.ID, nil, token)
		e.wantStatus(resp, "取账单详情", http.StatusOK)

		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("响应不是合法 JSON：%v", err)
		}
		amount, ok := obj["amount"].(string)
		if !ok {
			t.Fatalf("amount 必须是 JSON 字符串（平铺形态），实际类型 %T，响应=%s", obj["amount"], string(raw))
		}
		if amount != "12.34" {
			t.Fatalf("amount 应为 %q，实际 %q", "12.34", amount)
		}
		cents, ok := obj["amountCents"].(float64)
		if !ok {
			t.Fatalf("amountCents 必须是平铺的数字，实际类型 %T，响应=%s", obj["amountCents"], string(raw))
		}
		if cents != 1234 {
			t.Fatalf("amountCents 应为 1234，实际 %v", cents)
		}
		if _, bad := obj["amount"].(map[string]interface{}); bad {
			t.Fatal("amount 不能是 {value,cents} 嵌套对象")
		}
		if _, has := obj["occurredOn"]; !has {
			t.Fatal("响应应包含 occurredOn（自然日口径）")
		}
	})

	// A3：分页必须真正切片，page=1 与 page=2 不得重叠（重叠说明 size 被忽略）。
	t.Run("分页两页不重叠", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		cat := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")

		// 5 笔，发生时间递增：倒序后顺序稳定，便于断言不重叠。
		for i := 0; i < 5; i++ {
			at := time.Date(2026, 10, 1+i, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
			apiNewRecord(t, e, token, apiExpenseBody(cat.ID, fmt.Sprintf("%d.00", i+1), fmt.Sprintf("第%d笔", i+1), at))
		}

		first := apiListRecords(t, e, token, "?page=1&size=2")
		second := apiListRecords(t, e, token, "?page=2&size=2")
		if len(first.Items) != 2 || len(second.Items) != 2 {
			t.Fatalf("每页应 2 条，实际 %d / %d", len(first.Items), len(second.Items))
		}
		if first.Total != 5 || second.Total != 5 {
			t.Fatalf("total 应为 5，实际 %d / %d", first.Total, second.Total)
		}
		seen := map[string]bool{}
		for _, r := range first.Items {
			seen[r.ID] = true
		}
		for _, r := range second.Items {
			if seen[r.ID] {
				t.Fatalf("第 1 页与第 2 页出现重复账单 %s（分页未生效）", r.ID)
			}
		}
	})

	// A4：size 超过 200 被夹取到 200；size 非正数回落默认 20。
	// 上限是防"一次拉全库"的保护，夹取而非报错，前端不需要感知。
	t.Run("size被夹取到上限", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)

		if p := apiListRecords(t, e, token, "?size=500"); p.Size != 200 {
			t.Fatalf("size=500 应被夹取为 200，实际 %d", p.Size)
		}
		if p := apiListRecords(t, e, token, "?size=0"); p.Size != 20 {
			t.Fatalf("size=0 应回落默认 20，实际 %d", p.Size)
		}
	})

	// A5：page 为 0 或负数不得 500，应归一为第 1 页。
	// 前端在筛选条件联动时很容易算出 0，这条保证不会白屏。
	t.Run("page非法值归一为第一页", func(t *testing.T) {
		for _, q := range []string{"?page=0&size=2", "?page=-3&size=2"} {
			q := q
			t.Run(q, func(t *testing.T) {
				e := newEnv(t)
				token := apiNewAccount(t, e)
				p := apiListRecords(t, e, token, q)
				if p.Page != 1 {
					t.Fatalf("%s 的 page 应归一为 1，实际 %d", q, p.Page)
				}
			})
		}
	})
}

// ------------------------------------------------------------------ B. 异常输入

// TestLedgerAPIInvalidInput 逐条锁定异常输入的**具体错误码**。
//
// 只断言"非 200"是不够的：400 与 500 对前端是完全不同的处理路径
// （前者提示用户改输入，后者只能报"服务器异常"）。
func TestLedgerAPIInvalidInput(t *testing.T) {
	nowStr := time.Now().Format(time.RFC3339)

	type badCase struct {
		name string
		// method/path 固定；body 由每个用例按需构造（需要分类 id）。
		method string
		path   string
		body   func(t *testing.T, exp, inc model.LedgerCategory) interface{}
		want   int
		code   apierr.Code
	}

	cases := []badCase{
		// ---- 非法 UUID：fake 仓储按 map 查不到即 404，不做 UUID 语法校验；
		// 真实 PostgreSQL 实现会在驱动层报 invalid input syntax for type uuid。
		// 契约上这两种都应落到"4xx 且不泄露存在性"，因此这里按实测断 404，
		// 具体偏差见汇报（未改代码）。
		{
			name: "非法UUID_undefined", method: http.MethodGet, path: apiRecPath + "/undefined",
			want: http.StatusNotFound, code: apierr.CodeNotFound,
		},
		{
			name: "非法UUID_纯数字", method: http.MethodGet, path: apiRecPath + "/123",
			want: http.StatusNotFound, code: apierr.CodeNotFound,
		},

		// ---- 缺必填字段
		{
			name: "缺type", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return map[string]interface{}{"amount": "1.00", "categoryId": exp.ID, "transactionAt": nowStr}
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "缺amount", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return map[string]interface{}{
					"type": model.LedgerTypeExpense, "categoryId": exp.ID, "transactionAt": nowStr,
				}
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "缺categoryId", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, _, _ model.LedgerCategory) interface{} {
				return map[string]interface{}{
					"type": model.LedgerTypeExpense, "amount": "1.00", "transactionAt": nowStr,
				}
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},

		// ---- 非法金额：0 不是"合法零元"，负数、三位小数、非数字全部拒绝。
		{
			name: "金额为零", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(exp.ID, "0", "", time.Now())
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "金额为负", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(exp.ID, "-1", "", time.Now())
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "金额三位小数", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(exp.ID, "1.234", "", time.Now())
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "金额非数字", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(exp.ID, "abc", "", time.Now())
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},

		// ---- 备注长度：service 的上限是 500 个字符（按 rune 计）。
		// 超长必须 400；恰好 500 必须放行（否则中文用户会被无端拦下）。
		{
			name: "备注超长1000字", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(exp.ID, "1.00", strings.Repeat("账", 1000), time.Now())
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "备注恰好500字", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(exp.ID, "1.00", strings.Repeat("账", 500), time.Now())
			},
			want: http.StatusCreated,
		},

		// ---- 非法报销状态
		{
			name: "报销状态为foo", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, exp, _ model.LedgerCategory) interface{} {
				return apiExpenseBodyStatus(exp.ID, "1.00", "", time.Now(), "foo")
			},
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "报销列表status非法", method: http.MethodGet,
			path: "/api/v1/ledger/reimbursements?status=foo",
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},

		// ---- 非法日期区间
		{
			name: "日期不存在", method: http.MethodGet,
			path: "/api/v1/ledger/reimbursements?from=2026-13-99&to=2026-12-01",
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},
		{
			name: "起始日期晚于结束日期", method: http.MethodGet,
			path: "/api/v1/ledger/reimbursements?from=2026-05-10&to=2026-05-01",
			want: http.StatusBadRequest, code: apierr.CodeBadRequest,
		},

		// ---- 分类 id 合法但不存在：按 404 处理，与越权保持同一语义。
		{
			name: "分类不存在", method: http.MethodPost, path: apiRecPath,
			body: func(_ *testing.T, _, _ model.LedgerCategory) interface{} {
				return apiExpenseBody(uuid.NewString(), "1.00", "", time.Now())
			},
			want: http.StatusNotFound, code: apierr.CodeNotFound,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			token := apiNewAccount(t, e)
			exp := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")
			inc := apiFindCategory(t, e, token, model.LedgerTypeIncome, "工资")

			var body interface{}
			if tc.body != nil {
				body = tc.body(t, exp, inc)
			}
			resp, raw := e.do(tc.method, tc.path, body, token)
			if resp.StatusCode != tc.want {
				t.Fatalf("期望 HTTP %d，实际 %d，响应=%s", tc.want, resp.StatusCode, string(raw))
			}
			if tc.code != "" {
				e.wantCode(raw, tc.name, tc.code)
			}
		})
	}
}

// ------------------------------------------------------------------ C. 跨账号越权

// TestLedgerAPICrossAccount 验证账号隔离。
//
// 安全口径：越权访问必须返回 **404**，不能是 403 —— 403 等于告诉攻击者
// "这个 id 确实存在，只是不属于你"，可用于资源枚举。
func TestLedgerAPICrossAccount(t *testing.T) {
	// C1~C3：B 拿到 A 的账单 id，读/改/删都必须 404，且不能产生副作用。
	t.Run("B对A的账单读改删均为404", func(t *testing.T) {
		e := newEnv(t)
		tokenA := apiNewAccount(t, e)
		tokenB := apiNewAccount(t, e)

		catA := apiFindCategory(t, e, tokenA, model.LedgerTypeExpense, "餐饮")
		recA := apiNewRecord(t, e, tokenA, apiExpenseBody(catA.ID, "66.00", "A 的私账", time.Now()))

		patchBody := apiExpenseBody(catA.ID, "0.01", "越权篡改", time.Now())
		steps := []struct {
			name   string
			method string
			body   interface{}
		}{
			{"读", http.MethodGet, nil},
			{"改", http.MethodPatch, patchBody},
			{"删", http.MethodDelete, nil},
		}
		for _, s := range steps {
			s := s
			t.Run("B"+s.name+"A的账单", func(t *testing.T) {
				resp, raw := e.do(s.method, apiRecPath+"/"+recA.ID, s.body, tokenB)
				// 先显式排除 200（最危险的假通过：越权成功却 200 返回了别人的数据）。
				if resp.StatusCode == http.StatusOK {
					t.Fatalf("%s越权必须失败，不能返回 200", s.name)
				}
				// 再显式要求 404：403 会泄露"资源存在"这一事实。
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("%s越权应返回 404（不泄露存在性），实际 %d，响应=%s",
						s.name, resp.StatusCode, string(raw))
				}
				e.wantCode(raw, "越权"+s.name, apierr.CodeNotFound)
			})
		}

		// A 的账单必须毫发无损（越权请求不能有副作用）
		after := apiGetRecord(t, e, tokenA, recA.ID)
		if after.Amount != "66.00" || after.Note != "A 的私账" {
			t.Fatalf("越权请求产生了副作用：amount=%q note=%q", after.Amount, after.Note)
		}
	})

	// C4：A 拿 B 的分类 id 建账单 —— 分类归属校验必须拦住，否则能往别人分类里记账。
	t.Run("A用B的分类建账单被拒", func(t *testing.T) {
		e := newEnv(t)
		tokenA := apiNewAccount(t, e)
		tokenB := apiNewAccount(t, e)

		catB := apiFindCategory(t, e, tokenB, model.LedgerTypeExpense, "餐饮")
		resp, raw := e.do(http.MethodPost, apiRecPath,
			apiExpenseBody(catB.ID, "9.99", "借用别人的分类", time.Now()), tokenA)
		if resp.StatusCode == http.StatusCreated {
			t.Fatal("用他人分类建账单必须失败，不能 201")
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("用他人分类建账单应 404，实际 %d，响应=%s", resp.StatusCode, string(raw))
		}
		e.wantCode(raw, "用他人分类建账单", apierr.CodeNotFound)

		// 且 A 名下不能多出任何账单
		if p := apiListRecords(t, e, tokenA, "?size=50"); p.Total != 0 {
			t.Fatalf("失败的创建不应落库，A 名下实际有 %d 条", p.Total)
		}
	})

	// C5：列表查询必须按账号过滤，A 的 items 里不能出现 B 的任何 id。
	t.Run("A的列表不含B的任何记录", func(t *testing.T) {
		e := newEnv(t)
		tokenA := apiNewAccount(t, e)
		tokenB := apiNewAccount(t, e)

		catA := apiFindCategory(t, e, tokenA, model.LedgerTypeExpense, "餐饮")
		catB := apiFindCategory(t, e, tokenB, model.LedgerTypeExpense, "餐饮")

		recA := apiNewRecord(t, e, tokenA, apiExpenseBody(catA.ID, "1.00", "A1", time.Now()))
		bIDs := map[string]bool{}
		for i := 0; i < 2; i++ {
			recB := apiNewRecord(t, e, tokenB,
				apiExpenseBody(catB.ID, "2.00", fmt.Sprintf("B%d", i+1), time.Now()))
			bIDs[recB.ID] = true
		}

		page := apiListRecords(t, e, tokenA, "?size=50")
		if page.Total != 1 || len(page.Items) != 1 {
			t.Fatalf("A 只应有自己的 1 条账单，实际 total=%d len=%d", page.Total, len(page.Items))
		}
		if page.Items[0].ID != recA.ID {
			t.Fatalf("A 的列表出现了不属于自己的账单：%v", apiRecordIDs(page.Items))
		}
		for _, r := range page.Items {
			if bIDs[r.ID] {
				t.Fatalf("A 的列表泄露了 B 的账单 id=%s", r.ID)
			}
		}
		// 报销看板同样是按账号聚合的，顺带确认不会混算
		reimbA := apiReimb(t, e, tokenA, "?status=all")
		for _, r := range reimbA.Items {
			if bIDs[r.ID] {
				t.Fatalf("A 的报销列表泄露了 B 的账单 id=%s", r.ID)
			}
		}
	})

	// C6：鉴权层必须先于业务层拦下无令牌 / 过期 / 伪造令牌。
	t.Run("无令牌与无效令牌均为401", func(t *testing.T) {
		e := newEnv(t)
		_ = apiNewAccount(t, e) // 至少存在一个账号，证明 401 不是"库空"导致的

		steps := []struct {
			name  string
			token string
		}{
			{"无令牌", ""},
			{"非JWT串", "not-a-jwt"},
			{"已过期", apiSignToken(t, testJWTSecret, time.Now().Add(-time.Hour))},
			{"错误密钥签名", apiSignToken(t, "wrong-secret-0123456789abcdefghijklmn", time.Now().Add(time.Hour))},
		}
		for _, s := range steps {
			s := s
			t.Run(s.name, func(t *testing.T) {
				resp, raw := e.do(http.MethodGet, apiRecPath, nil, s.token)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("%s 应 401，实际 %d，响应=%s", s.name, resp.StatusCode, string(raw))
				}
				e.wantCode(raw, s.name, apierr.CodeUnauthorized)
			})
		}
	})
}

// ------------------------------------------------------------------ D. 报销状态流转

// TestLedgerAPIReimbursement 覆盖报销看板的状态机与汇总口径。
func TestLedgerAPIReimbursement(t *testing.T) {
	// D1【核心回归】：标记已报销后，status=all 仍必须能看到这条。
	// 旧实现固定查 pending，标记 done 后记录直接从列表消失，用户以为账没了。
	t.Run("标记已报销后all仍可见", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		cat := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")

		rec := apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "45.60", "打车报销", time.Now(), model.LedgerReimbPending))

		// 标记前：all 里是 pending
		before := apiReimb(t, e, token, "?status=all")
		got, ok := apiFindRecord(before.Items, rec.ID)
		if !ok {
			t.Fatalf("标记前 status=all 应包含该条，实际 items=%v", apiRecordIDs(before.Items))
		}
		if got.ReimbursementStatus != model.LedgerReimbPending {
			t.Fatalf("标记前状态应为 pending，实际 %q", got.ReimbursementStatus)
		}

		if n := apiMark(t, e, token, []string{rec.ID}, model.LedgerReimbDone); n != 1 {
			t.Fatalf("标记已报销应更新 1 条，实际 %d", n)
		}

		// 标记后：all 里仍要有它，且状态为 done —— 这是本轮修复的核心断言
		after := apiReimb(t, e, token, "?status=all")
		got, ok = apiFindRecord(after.Items, rec.ID)
		if !ok {
			t.Fatalf("标记已报销后 status=all 必须仍能看到该条（旧缺陷：标记即消失），实际 items=%v",
				apiRecordIDs(after.Items))
		}
		if got.ReimbursementStatus != model.LedgerReimbDone {
			t.Fatalf("标记后状态应为 done，实际 %q", got.ReimbursementStatus)
		}
	})

	// D2：done 退回 pending 后状态必须真的恢复（不是只能单向流转）。
	t.Run("已报销可退回待报销", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		cat := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")

		rec := apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "8.00", "误标", time.Now(), model.LedgerReimbPending))
		if n := apiMark(t, e, token, []string{rec.ID}, model.LedgerReimbDone); n != 1 {
			t.Fatalf("先标记已报销应更新 1 条，实际 %d", n)
		}
		if n := apiMark(t, e, token, []string{rec.ID}, model.LedgerReimbPending); n != 1 {
			t.Fatalf("退回待报销应更新 1 条，实际 %d", n)
		}

		if got := apiGetRecord(t, e, token, rec.ID); got.ReimbursementStatus != model.LedgerReimbPending {
			t.Fatalf("退回后详情状态应为 pending，实际 %q", got.ReimbursementStatus)
		}
		page := apiReimb(t, e, token, "?status=pending")
		if _, ok := apiFindRecord(page.Items, rec.ID); !ok {
			t.Fatalf("退回后应重新出现在 pending 列表，实际 items=%v", apiRecordIDs(page.Items))
		}
	})

	// D3：三种筛选互斥 —— pending 不含 done，done 只含 done。
	t.Run("按状态筛选互斥", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		cat := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")

		p := apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "10.00", "待报", time.Now(), model.LedgerReimbPending))
		d := apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "20.00", "已报", time.Now(), model.LedgerReimbPending))
		if n := apiMark(t, e, token, []string{d.ID}, model.LedgerReimbDone); n != 1 {
			t.Fatalf("标记已报销应更新 1 条，实际 %d", n)
		}
		n := apiNewRecord(t, e, token, apiExpenseBody(cat.ID, "30.00", "不用报", time.Now()))

		pendingPage := apiReimb(t, e, token, "?status=pending")
		if pendingPage.Total != 1 || len(pendingPage.Items) != 1 {
			t.Fatalf("pending 应只有 1 条，实际 total=%d len=%d", pendingPage.Total, len(pendingPage.Items))
		}
		if pendingPage.Items[0].ID != p.ID {
			t.Fatalf("pending 列表应只含待报销那条，实际 %v", apiRecordIDs(pendingPage.Items))
		}
		for _, r := range pendingPage.Items {
			if r.ReimbursementStatus == model.LedgerReimbDone {
				t.Fatalf("pending 列表混入了 done 项 id=%s", r.ID)
			}
		}

		donePage := apiReimb(t, e, token, "?status=done")
		if donePage.Total != 1 || len(donePage.Items) != 1 {
			t.Fatalf("done 应只有 1 条，实际 total=%d len=%d", donePage.Total, len(donePage.Items))
		}
		if donePage.Items[0].ID != d.ID {
			t.Fatalf("done 列表应只含已报销那条，实际 %v", apiRecordIDs(donePage.Items))
		}

		nonePage := apiReimb(t, e, token, "?status=none")
		if nonePage.Total != 1 || nonePage.Items[0].ID != n.ID {
			t.Fatalf("none 列表应只含不报销那条，实际 %v", apiRecordIDs(nonePage.Items))
		}
	})

	// D4：summary.counts 三个数必须与实际条数一致，金额合计走全量聚合。
	t.Run("汇总计数与金额准确", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		cat := apiFindCategory(t, e, token, model.LedgerTypeExpense, "餐饮")

		apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "10.00", "待报1", time.Now(), model.LedgerReimbPending))
		apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "20.00", "待报2", time.Now(), model.LedgerReimbPending))
		r3 := apiNewRecord(t, e, token,
			apiExpenseBodyStatus(cat.ID, "40.00", "已报", time.Now(), model.LedgerReimbPending))
		notReimb := apiNewRecord(t, e, token, apiExpenseBody(cat.ID, "80.00", "不报", time.Now()))
		if n := apiMark(t, e, token, []string{r3.ID}, model.LedgerReimbDone); n != 1 {
			t.Fatalf("标记已报销应更新 1 条，实际 %d", n)
		}

		page := apiReimb(t, e, token, "?status=all")
		if page.Summary.Counts.Pending != 2 {
			t.Fatalf("counts.pending 应为 2，实际 %d", page.Summary.Counts.Pending)
		}
		if page.Summary.Counts.Done != 1 {
			t.Fatalf("counts.done 应为 1，实际 %d", page.Summary.Counts.Done)
		}
		if page.Summary.Counts.None != 1 {
			t.Fatalf("counts.none 应为 1，实际 %d", page.Summary.Counts.None)
		}
		// all 的语义是「所有参与报销的记录」= pending + done，不含 none。
		// 「不涉及」的记录出现在报销页没有意义，还能被误标成已报销（用户已反馈）。
		if page.Total != 3 {
			t.Fatalf("status=all 的 total 应为 3（pending 2 + done 1），实际 %d", page.Total)
		}
		for _, it := range page.Items {
			if it.ID == notReimb.ID {
				t.Fatalf("status=all 不应包含「不涉及报销」的记录 %s", notReimb.ID)
			}
			if it.ReimbursementStatus == model.LedgerReimbNone {
				t.Fatalf("status=all 返回了 none 状态的记录 %s", it.ID)
			}
		}
		if page.Summary.PendingTotal.Cents != 3000 {
			t.Fatalf("待报销合计应为 3000 分，实际 %d", page.Summary.PendingTotal.Cents)
		}
		if page.Summary.DoneTotal.Cents != 4000 {
			t.Fatalf("已报销合计应为 4000 分，实际 %d", page.Summary.DoneTotal.Cents)
		}
		// 兼容字段必须与 summary 内的一致，否则旧前端显示 ¥0.00
		if page.PendingTotal.Cents != page.Summary.PendingTotal.Cents {
			t.Fatalf("顶层 pendingTotal(%d) 与 summary.pendingTotal(%d) 不一致",
				page.PendingTotal.Cents, page.Summary.PendingTotal.Cents)
		}
	})

	// D5：空 ids 是用户"没勾选就点提交"，必须 400 给出可读提示，不能静默 200。
	t.Run("空ids返回400", func(t *testing.T) {
		steps := []struct {
			name string
			body interface{}
		}{
			{"空数组", map[string]interface{}{"ids": []string{}, "status": model.LedgerReimbDone}},
			{"仅空白字符", map[string]interface{}{"ids": []string{"  "}, "status": model.LedgerReimbDone}},
			{"缺ids字段", map[string]interface{}{"status": model.LedgerReimbDone}},
		}
		for _, s := range steps {
			s := s
			t.Run(s.name, func(t *testing.T) {
				e := newEnv(t)
				tk := apiNewAccount(t, e)
				resp, raw := e.do(http.MethodPost, "/api/v1/ledger/reimbursements/mark", s.body, tk)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s 应 400，实际 %d，响应=%s", s.name, resp.StatusCode, string(raw))
				}
				e.wantCode(raw, s.name, apierr.CodeBadRequest)
			})
		}
	})

	// D6：批量改状态的 status 只能是 none/pending/done；all 是查询专用，不能写入。
	t.Run("批量改状态非法值返回400", func(t *testing.T) {
		for _, st := range []string{"all", "foo", ""} {
			st := st
			t.Run("status="+st, func(t *testing.T) {
				e := newEnv(t)
				token := apiNewAccount(t, e)
				resp, raw := e.do(http.MethodPost, "/api/v1/ledger/reimbursements/mark",
					map[string]interface{}{"ids": []string{uuid.NewString()}, "status": st}, token)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status=%q 应 400，实际 %d，响应=%s", st, resp.StatusCode, string(raw))
				}
				e.wantCode(raw, "非法报销状态", apierr.CodeBadRequest)
			})
		}
	})

	// D7：收入不进入报销流程 —— 传它的 id 也不能被改成 pending/done。
	t.Run("收入不进报销流程", func(t *testing.T) {
		e := newEnv(t)
		token := apiNewAccount(t, e)
		salary := apiFindCategory(t, e, token, model.LedgerTypeIncome, "工资")

		inc := apiNewRecord(t, e, token, map[string]interface{}{
			"type": model.LedgerTypeIncome, "amount": "100.00", "categoryId": salary.ID,
			"transactionAt": time.Now().Format(time.RFC3339), "note": "红包",
		})
		if n := apiMark(t, e, token, []string{inc.ID}, model.LedgerReimbDone); n != 0 {
			t.Fatalf("收入不应被改报销状态，实际更新了 %d 条", n)
		}
		if got := apiGetRecord(t, e, token, inc.ID); got.ReimbursementStatus != model.LedgerReimbNone {
			t.Fatalf("收入报销状态必须保持 none，实际 %q", got.ReimbursementStatus)
		}
		// 报销看板只统计支出，收入不能出现在里面
		page := apiReimb(t, e, token, "?status=all")
		if _, ok := apiFindRecord(page.Items, inc.ID); ok {
			t.Fatal("报销列表不应包含收入账单")
		}
	})
}
