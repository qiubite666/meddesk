package model

import "testing"

// TestParseAmount 覆盖金额解析：定点数语义，全程整数运算，不允许浮点。
func TestParseAmount(t *testing.T) {
	cases := []struct {
		in    string
		value string
		cents int64
		ok    bool
	}{
		{"12.34", "12.34", 1234, true},
		{"12", "12.00", 1200, true},
		{"0.5", "0.50", 50, true},
		{"0.05", "0.05", 5, true},
		{"007.10", "7.10", 710, true},
		{"999999999999.99", "999999999999.99", 99999999999999, true},
		{"0", "", 0, false},          // 金额必须大于 0
		{"0.00", "", 0, false},
		{"-1", "", 0, false},         // 负数用 type 表达，不允许负金额
		{"12.345", "", 0, false},     // 超过两位小数
		{"1e3", "", 0, false},        // 科学计数法
		{"abc", "", 0, false},
		{"", "", 0, false},
		{"12.", "", 0, false},
		{"1,234.00", "", 0, false},   // 千分位
	}
	for _, c := range cases {
		value, cents, err := ParseAmount(c.in)
		if c.ok {
			if err != nil {
				t.Fatalf("ParseAmount(%q) 不应报错，实际 %v", c.in, err)
			}
			if value != c.value || cents != c.cents {
				t.Fatalf("ParseAmount(%q) = (%q,%d)，期望 (%q,%d)", c.in, value, cents, c.value, c.cents)
			}
			continue
		}
		if err == nil {
			t.Fatalf("ParseAmount(%q) 应报错，实际返回 (%q,%d)", c.in, value, cents)
		}
	}
}

// TestMoneyFromCents 覆盖整数分 → 展示字符串的换算（含负数结余）。
func TestMoneyFromCents(t *testing.T) {
	cases := []struct {
		cents int64
		value string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{50, "0.50"},
		{1234, "12.34"},
		{-1234, "-12.34"},
		{100000, "1000.00"},
	}
	for _, c := range cases {
		m := MoneyFromCents(c.cents)
		if m.Value != c.value {
			t.Fatalf("MoneyFromCents(%d).Value = %q，期望 %q", c.cents, m.Value, c.value)
		}
		if m.Cents != c.cents {
			t.Fatalf("MoneyFromCents(%d).Cents = %d，期望 %d", c.cents, m.Cents, c.cents)
		}
	}
}

// TestAmountNoFloatDrift 验证"累加不漂移"：0.1 + 0.2 在浮点下是 0.30000000000000004，
// 用整数分累加必须得到精确的 30 分。
func TestAmountNoFloatDrift(t *testing.T) {
	var total int64
	for _, raw := range []string{"0.10", "0.20", "0.30"} {
		_, cents, err := ParseAmount(raw)
		if err != nil {
			t.Fatalf("解析 %q 失败：%v", raw, err)
		}
		total += cents
	}
	if total != 60 {
		t.Fatalf("0.1+0.2+0.3 应为 60 分，实际 %d", total)
	}
	if got := MoneyFromCents(total).Value; got != "0.60" {
		t.Fatalf("合计应为 0.60，实际 %q", got)
	}
}
