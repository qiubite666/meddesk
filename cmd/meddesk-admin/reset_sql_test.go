package main

import (
	"strings"
	"testing"
)

// TestResetPasswordSQLClearsEnvelopeStale 覆盖 A-30。
//
// 【为什么用 SQL 文本断言】reset-password 只在有真实 Postgres 时才执行
// （本仓库的单元测试不连库，集成测试在 internal/server），因此这里退而锁定
// 语句形状：一旦有人再把 envelope_stale 两列漏掉，测试立即失败。
// 这条缺陷的代价很高——残留的 stale 标记会让客户端用旧 DEK 覆盖刚写好的
// 新信封，账号永久解不开数据，值得用一条"笨"但稳定的断言守住。
func TestResetPasswordSQLClearsEnvelopeStale(t *testing.T) {
	q := resetPasswordSQL
	for _, want := range []string{
		"kdf_salt=$1",
		"wrapped_dek=$2",
		"dek_iv=$3",
		"password_hash=$4",
		"envelope_stale = false",
		"envelope_stale_at = NULL",
		"WHERE id=$5",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("resetPasswordSQL 缺少 %q:\n%s", want, q)
		}
	}
	// 五个占位符必须与调用方的实参个数一致（salt/wrapped/iv/hash/id）。
	if n := strings.Count(q, "$"); n != 5 {
		t.Fatalf("占位符数量 = %d，期望 5:\n%s", n, q)
	}
}
