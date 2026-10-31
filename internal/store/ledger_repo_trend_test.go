package store

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
)

// placeholderRe 匹配 SQL 里的 $N 占位符。
var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// placeholderNos 按出现顺序返回 SQL 中的占位符编号。
func placeholderNos(q string) []int {
	ms := placeholderRe.FindAllStringSubmatch(q, -1)
	out := make([]int, 0, len(ms))
	for _, m := range ms {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// TestLedgerWhereFromTrendStartIdx 复现 P1-4：
// DailyTrend 里 $1=user_id、$2/$3=generate_series 起止日期，筛选条件必须从 $4 起编。
// 若退回从 $2 起编，条件会绑到起始日期上，真实 PG 报 42883，趋势接口 500。
func TestLedgerWhereFromTrendStartIdx(t *testing.T) {
	cond, args, err := ledgerWhereFrom(model.LedgerFilter{Type: model.LedgerTypeExpense}, "", "", 3)
	if err != nil {
		t.Fatalf("无条件筛选不应报错：%v", err)
	}
	if want := " AND r.type = $4"; cond != want {
		t.Fatalf("startIdx=3 时条件应为 %q，实际 %q", want, cond)
	}
	if len(args) != 1 || args[0] != model.LedgerTypeExpense {
		t.Fatalf("参数应为 [expense]，实际 %v", args)
	}

	// 拼进真实形状的 DailyTrend 语句后，占位符必须是 $1..$4 各出现一次、无撞号。
	q := `SELECT d::date::text
FROM generate_series($2::date, $3::date, '1 day') AS d
LEFT JOIN ledger_records r ON r.user_id = $1 AND r.occurred_on = d::date` + cond
	got := placeholderNos(q)
	want := []int{2, 3, 1, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DailyTrend 占位符顺序应为 %v，实际 %v（$2/$3 被日期占用，条件不能再用）", want, got)
	}
}

// TestLedgerWhereFromLegacyStartIdx 回归保护：startIdx=1 时占位符编号与条件顺序不变（从 $2 起编）。
//
// 【旧断言锁定的是 bug，已改写】原用例要求条件里出现 `r.category_id::text = ANY($4)`、
// 参数为 []string——那正是 B-09：列上做 ::text 转换会让 uuid 索引彻底失效。
// 现在改为 uuid[] 参数与 uuid 列直接比较，断言同步更新为"参数是 pgtype.UUID 切片"。
func TestLedgerWhereFromLegacyStartIdx(t *testing.T) {
	f := model.LedgerFilter{
		Type:        model.LedgerTypeExpense,
		ReimbStatus: "pending",
		CategoryIDs: []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"},
		IDs:         []string{"33333333-3333-3333-3333-333333333333"},
		Keyword:     " Food ",
	}
	cond, args, err := ledgerWhereFrom(f, "", "", 1)
	if err != nil {
		t.Fatalf("合法 uuid 不应报错：%v", err)
	}

	wantCond := " AND r.type = $2" +
		" AND r.reimbursement_status = $3" +
		" AND r.category_id = ANY($4::uuid[])" +
		" AND r.id = ANY($5::uuid[])" +
		" AND (lower(r.note) LIKE $6 OR lower(r.category_name_snapshot) LIKE $7)"
	if cond != wantCond {
		t.Fatalf("startIdx=1 条件与旧行为不一致：\n期望 %q\n实际 %q", wantCond, cond)
	}
	if strings.Contains(cond, "::text") {
		t.Fatalf("条件里不应再出现 ::text（会让索引失效）：%q", cond)
	}
	wantArgs := []any{
		model.LedgerTypeExpense, "pending",
		[]pgtype.UUID{
			mustPgUUID(t, "11111111-1111-1111-1111-111111111111"),
			mustPgUUID(t, "22222222-2222-2222-2222-222222222222"),
		},
		[]pgtype.UUID{mustPgUUID(t, "33333333-3333-3333-3333-333333333333")},
		"%food%", "%food%",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("startIdx=1 参数与预期不一致：\n期望 %v\n实际 %v", wantArgs, args)
	}
}

// TestLedgerWhereFromRejectsInvalidUUID 非法 id 必须在构造参数阶段报 400，
// 不能把字符串丢给 PG 触发 22P02（那会被包成 500）。
func TestLedgerWhereFromRejectsInvalidUUID(t *testing.T) {
	cases := []model.LedgerFilter{
		{CategoryIDs: []string{"c1"}},
		{IDs: []string{"not-a-uuid"}},
	}
	for _, f := range cases {
		_, _, err := ledgerWhereFrom(f, "", "", 1)
		if err == nil {
			t.Fatalf("非法 uuid 应报错：%+v", f)
		}
		if !apierr.Is(err, apierr.CodeBadRequest) {
			t.Fatalf("非法 uuid 应为 bad_request，实际 %v", err)
		}
	}
}

// mustPgUUID 构造期望的 uuid 参数（独立于被测代码，避免用实现验证实现）。
func mustPgUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("测试用 uuid 非法 %q：%v", s, err)
	}
	return pgtype.UUID{Bytes: [16]byte(u), Valid: true}
}

// TestDailyTrendSQLPlaceholders 直接校验 DailyTrend 真实组装的语句：
// 占位符必须恰好是 $1..$N 各一次，N == len(args)。
// 只要有人把起始下标写回 1，条件就会产出重复的 $2（与起始日期撞号），本用例立即失败。
func TestDailyTrendSQLPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		f    model.LedgerFilter
		args int // 期望参数总数 = 3（uid/from/to）+ 筛选参数
	}{
		{"无筛选（v0.3.51 已修好的路径）", model.LedgerFilter{}, 3},
		{"仅 type（线上 500 的路径）", model.LedgerFilter{Type: model.LedgerTypeExpense}, 4},
		{"全筛选", model.LedgerFilter{
			Type:        model.LedgerTypeIncome,
			ReimbStatus: "done",
			CategoryIDs: []string{"11111111-1111-1111-1111-111111111111"},
			IDs:         []string{"33333333-3333-3333-3333-333333333333"},
			Keyword:     "工资",
		}, 9}, // 3 + type/reimb/categoryIDs/ids/keyword×2
	}
	for _, c := range cases {
		q, args, err := dailyTrendSQL(c.f, "uid", "2026-10-01", "2026-10-31")
		if err != nil {
			t.Fatalf("%s：不应报错 %v", c.name, err)
		}
		if len(args) != c.args {
			t.Fatalf("%s：参数数量期望 %d，实际 %d（%v）", c.name, c.args, len(args), args)
		}
		// 占位符在 SQL 文本里不是按编号顺序出现的（$2/$3 在 generate_series 里），
		// 这里按编号排序后比对：既是集合相等，重复编号（撞号）也会暴露出来。
		got := placeholderNos(q)
		slices.Sort(got)
		want := make([]int, 0, len(args))
		for i := 1; i <= len(args); i++ {
			want = append(want, i)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s：占位符应为 %v，实际 %v\nSQL=%s", c.name, want, got, q)
		}
	}
}

// TestLedgerWhereFromNumbering 校验条件里的占位符从 startIdx+1 起严格连续，
// 且数量与 args 一致——这是"参数化 + 编号正确"的直接证据。
func TestLedgerWhereFromNumbering(t *testing.T) {
	f := model.LedgerFilter{
		Type:        model.LedgerTypeIncome,
		ReimbStatus: "done",
		CategoryIDs: []string{"11111111-1111-1111-1111-111111111111"},
		Keyword:     "工资",
	}
	for _, start := range []int{1, 3} {
		cond, args, err := ledgerWhereFrom(f, "2026-10-01", "2026-10-31", start)
		if err != nil {
			t.Fatalf("startIdx=%d：不应报错 %v", start, err)
		}
		got := placeholderNos(cond)
		want := make([]int, 0, len(args))
		for i := 0; i < len(args); i++ {
			want = append(want, start+1+i)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("startIdx=%d：占位符应为 %v，实际 %v（条件=%q）", start, want, got, cond)
		}
		if len(args) != 7 { // type / reimb / from / to / categoryIDs / keyword×2
			t.Fatalf("startIdx=%d：参数数量应为 7，实际 %d（%v）", start, len(args), args)
		}
	}
}
