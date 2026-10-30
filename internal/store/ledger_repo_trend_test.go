package store

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"testing"

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
	cond, args := ledgerWhereFrom(model.LedgerFilter{Type: model.LedgerTypeExpense}, "", "", 3)
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

// TestLedgerWhereFromLegacyStartIdx 回归保护：startIdx=1 时必须与旧实现（从 $2 起编）逐字节一致。
func TestLedgerWhereFromLegacyStartIdx(t *testing.T) {
	f := model.LedgerFilter{
		Type:        model.LedgerTypeExpense,
		ReimbStatus: "pending",
		CategoryIDs: []string{"c1", "c2"},
		IDs:         []string{"r1"},
		Keyword:     " Food ",
	}
	cond, args := ledgerWhereFrom(f, "", "", 1)

	wantCond := " AND r.type = $2" +
		" AND r.reimbursement_status = $3" +
		" AND r.category_id::text = ANY($4)" +
		" AND r.id::text = ANY($5)" +
		" AND (lower(r.note) LIKE $6 OR lower(r.category_name_snapshot) LIKE $7)"
	if cond != wantCond {
		t.Fatalf("startIdx=1 条件与旧行为不一致：\n期望 %q\n实际 %q", wantCond, cond)
	}
	wantArgs := []any{model.LedgerTypeExpense, "pending", []string{"c1", "c2"}, []string{"r1"}, "%food%", "%food%"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("startIdx=1 参数与旧行为不一致：\n期望 %v\n实际 %v", wantArgs, args)
	}
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
			CategoryIDs: []string{"c1"},
			IDs:         []string{"r1"},
			Keyword:     "工资",
		}, 9}, // 3 + type/reimb/categoryIDs/ids/keyword×2
	}
	for _, c := range cases {
		q, args := dailyTrendSQL(c.f, "uid", "2026-10-01", "2026-10-31")
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
		CategoryIDs: []string{"c1"},
		Keyword:     "工资",
	}
	for _, start := range []int{1, 3} {
		cond, args := ledgerWhereFrom(f, "2026-10-01", "2026-10-31", start)
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
