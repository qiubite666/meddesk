package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件只测「边界值」：既有 ledger_test.go 已覆盖主干路径，这里专攻账本最容易出错的地方
// ——金额的上下限、非法输入、整数分换算、常量字面值漂移与 JSON 契约。
//
// 与 ledger_test.go 的分工：那边测"能不能用"，这边测"极端输入下是否仍然正确"。

// TestLedgerBoundaryParseAmountValid 覆盖金额上界、下界与归一化。
//
// 【为什么必须测上界】
// amountPattern 限制整数部分最多 12 位，配合 numeric(14,2) 的落库精度。
// 一旦有人放宽正则到 13 位，Go 侧仍能解析出 100000000000000 分，
// 但写库时会 numeric overflow，表现为"保存失败"而非"金额非法"，排查成本极高。
// 【为什么必须测下界】
// 0.01 是账本能表达的最小正数；cents<=0 的兜底分支必须连 0.01 都放行。
func TestLedgerBoundaryParseAmountValid(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		value string
		cents int64
	}{
		{"整数补两位小数", "1", "1.00", 100},
		{"一位小数补零", "0.5", "0.50", 50},
		{"前导零被去掉", "007.10", "7.10", 710},
		{"一位小数归一为两位", "12.3", "12.30", 1230},
		{"全部前导零归零", "000.01", "0.01", 1},
		{"最小正数", "0.01", "0.01", 1},
		{"最大 12 位整数", "999999999999.99", "999999999999.99", 99999999999999},
		{"整数上界无小数", "999999999999", "999999999999.00", 99999999999900},
		{"零整数部分", "0.99", "0.99", 99},
		{"两侧空白被裁剪", "  12.30  ", "12.30", 1230},
		{"仅前导空白", " 0.01", "0.01", 1},
		// TrimSpace 会裁掉制表符与换行：前端从文本框粘贴常带尾随换行，
		// 这里明确记录"会被接受"的行为，避免日后有人误改成拒绝。
		{"制表符与换行被裁剪", "\t12.3\n", "12.30", 1230},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value, cents, err := ParseAmount(c.in)
			if err != nil {
				t.Fatalf("ParseAmount(%q) 应成功，实际报错 %v", c.in, err)
			}
			if value != c.value || cents != c.cents {
				t.Fatalf("ParseAmount(%q) = (%q, %d)，期望 (%q, %d)", c.in, value, cents, c.value, c.cents)
			}
		})
	}
}

// TestLedgerBoundaryParseAmountInvalid 覆盖所有必须被拒绝的形态。
//
// 【为什么这些必须报错而不是"尽量解析"】
// 金额是账本唯一不可出错的字段：多解析出一位小数就是 10 倍的账目偏差，
// 把 "-1" 当正数、把 "1,000" 截成 1，都会静默污染账目。宁可 400 也不能猜。
// 全角/阿拉伯-印度数字也要拒绝：Go 的 \d 只匹配 ASCII 0-9，
// 前端若从输入法带入全角数字，必须让用户改，而不是存成错值。
func TestLedgerBoundaryParseAmountInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"零", "0"},
		{"零点零零", "0.00"},
		{"负一", "-1"},
		{"负小数", "-0.01"},
		{"显式正号", "+1"},
		{"科学计数法", "1e5"},
		{"大写科学计数法", "1E5"},
		{"千分位", "1,000"},
		{"千分位带小数", "1,234.00"},
		{"三位小数", "1.234"},
		{"纯点", "."},
		{"整数后带点", "1."},
		{"点开头", ".5"},
		{"字母", "abc"},
		{"空串", ""},
		{"纯空格", "   "},
		{"十三位整数", "1000000000000"},
		{"十三位整数带小数", "1000000000000.01"},
		{"全角数字", "１２"},
		{"阿拉伯印度数字", "١٢"},
		{"中间空格", "1 2"},
		{"美元符号", "$1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value, cents, err := ParseAmount(c.in)
			if err == nil {
				t.Fatalf("ParseAmount(%q) 应报错，实际返回 (%q, %d)", c.in, value, cents)
			}
			if value != "" || cents != 0 {
				t.Fatalf("ParseAmount(%q) 报错时应返回零值，实际 (%q, %d)", c.in, value, cents)
			}
		})
	}
}

// TestLedgerBoundaryParseAmountNoFloatDrift 直接钉死浮点漂移。
//
// 0.1 + 0.2 在 float64 下是 0.30000000000000004，用分累加必须恰好 30。
// 这是账本"不用浮点"这条设计的唯一可验证证据，改坏了必须立刻失败。
func TestLedgerBoundaryParseAmountNoFloatDrift(t *testing.T) {
	_, c1, err := ParseAmount("0.1")
	if err != nil {
		t.Fatalf("ParseAmount(\"0.1\") 报错：%v", err)
	}
	_, c2, err := ParseAmount("0.2")
	if err != nil {
		t.Fatalf("ParseAmount(\"0.2\") 报错：%v", err)
	}
	if sum := c1 + c2; sum != 30 {
		t.Fatalf("0.1 + 0.2 的分值应为 30，实际 %d（%d + %d）", sum, c1, c2)
	}
	if got := MoneyFromCents(c1 + c2).Value; got != "0.30" {
		t.Fatalf("0.1 + 0.2 的展示值应为 \"0.30\"，实际 %q", got)
	}
}

// TestLedgerBoundaryMoneyFromCents 覆盖分→元的边界，特别是负数与大值。
//
// 【为什么测负数】结余 = 收入 - 支出 可以为负，符号位与"整元/分"拼接顺序错一位
// 就会出现 "-0.45" 或 "-1.-5" 这类脏值。
// 【为什么测 13 位分】MoneyFromCents 的输入来自 SQL 聚合，最大约 1e13 分，
// 必须确认既不走 float 也不溢出 int64。
func TestLedgerBoundaryMoneyFromCents(t *testing.T) {
	cases := []struct {
		name  string
		cents int64
		value string
	}{
		{"零", 0, "0.00"},
		{"一分", 1, "0.01"},
		{"九角九分", 99, "0.99"},
		{"一元", 100, "1.00"},
		{"一元零一分", 101, "1.01"},
		{"一万二千三百四十五分", 12345, "123.45"},
		{"负一分", -1, "-0.01"},
		{"负一元", -100, "-1.00"},
		{"负一万二千三百四十五分", -12345, "-123.45"},
		{"最大 13 位分", 99999999999999, "999999999999.99"},
		{"负最大 13 位分", -99999999999999, "-999999999999.99"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := MoneyFromCents(c.cents)
			if m.Value != c.value {
				t.Fatalf("MoneyFromCents(%d).Value = %q，期望 %q", c.cents, m.Value, c.value)
			}
			if m.Cents != c.cents {
				t.Fatalf("MoneyFromCents(%d).Cents = %d，期望 %d（Cents 必须原样保留）", c.cents, m.Cents, c.cents)
			}
			// 负号的唯一性：负值只能有一个前导 "-"，不能出现 "-0.-5" 这类拼接错误。
			if strings.Count(m.Value, "-") > 1 || (strings.Contains(m.Value, "-") && !strings.HasPrefix(m.Value, "-")) {
				t.Fatalf("MoneyFromCents(%d).Value = %q，负号位置或数量非法", c.cents, m.Value)
			}
		})
	}
}

// TestLedgerBoundaryZeroMoney 确认 ZeroMoney 与 MoneyFromCents(0) 完全一致。
// 两处若各写一套字面值，前端可能拿到 "0" 与 "0.00" 两种零值。
func TestLedgerBoundaryZeroMoney(t *testing.T) {
	z := ZeroMoney()
	if z.Value != "0.00" || z.Cents != 0 {
		t.Fatalf("ZeroMoney() = (%q, %d)，期望 (\"0.00\", 0)", z.Value, z.Cents)
	}
	if m := MoneyFromCents(0); m != z {
		t.Fatalf("MoneyFromCents(0) = %+v，与 ZeroMoney() = %+v 不一致", m, z)
	}
}

// TestLedgerBoundaryMoneyJSON 锁住金额的 JSON 契约。
// 前端按 value 展示、按 cents 画图；字段名一旦改名，前端会静默拿到 undefined。
func TestLedgerBoundaryMoneyJSON(t *testing.T) {
	raw, err := json.Marshal(MoneyFromCents(12345))
	if err != nil {
		t.Fatalf("序列化 Money 失败：%v", err)
	}
	if got, want := string(raw), `{"value":"123.45","cents":12345}`; got != want {
		t.Fatalf("Money JSON = %s，期望 %s", got, want)
	}
	// 零值必须序列化成 0 而不是 null，前端才能直接做数值运算。
	raw, err = json.Marshal(ZeroMoney())
	if err != nil {
		t.Fatalf("序列化 ZeroMoney 失败：%v", err)
	}
	if got, want := string(raw), `{"value":"0.00","cents":0}`; got != want {
		t.Fatalf("ZeroMoney JSON = %s，期望 %s", got, want)
	}
}

// TestLedgerBoundaryConstants 钉住记账类型与报销状态的字面值。
//
// 【为什么这是边界值】
// 这些字符串同时是数据库列值、API 契约与前端映射表的键。任何一次"顺手改成常量名"
// 都会让历史数据读不出来、前端图标错位，而且编译期完全无法发现。
func TestLedgerBoundaryConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"收入类型", LedgerTypeIncome, "income"},
		{"支出类型", LedgerTypeExpense, "expense"},
		{"无需报销", LedgerReimbNone, "none"},
		{"待报销", LedgerReimbPending, "pending"},
		{"已报销", LedgerReimbDone, "done"},
	}
	seen := make(map[string]string, len(cases))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Fatalf("%s 字面值 = %q，期望 %q", c.name, c.got, c.want)
			}
			if other, dup := seen[c.got]; dup {
				t.Fatalf("%s 与 %s 取值重复（都是 %q），前端无法区分", c.name, other, c.got)
			}
			seen[c.got] = c.name
		})
	}
}

// TestLedgerBoundaryParseRoundTrip 校验解析与反解互逆：
// ParseAmount 得到的 cents 交给 MoneyFromCents，必须原样还原成同一个展示串。
// 这条能一次抓住两边对"补零/去前导零"处理不一致的问题。
func TestLedgerBoundaryParseRoundTrip(t *testing.T) {
	raws := []string{"0.01", "1", "0.5", "007.10", "12.3", "999999999999.99", "  8.80  "}
	for _, raw := range raws {
		t.Run(raw, func(t *testing.T) {
			value, cents, err := ParseAmount(raw)
			if err != nil {
				t.Fatalf("ParseAmount(%q) 报错：%v", raw, err)
			}
			if back := MoneyFromCents(cents).Value; back != value {
				t.Fatalf("往返不一致：ParseAmount(%q) 得 %q，MoneyFromCents(%d) 得 %q", raw, value, cents, back)
			}
		})
	}
}
