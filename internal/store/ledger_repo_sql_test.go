package store

import (
	"strings"
	"testing"
)

// TestUpdateCategoryIncludesKind 锁住 B-08 的回归。
//
// 缺陷回顾：`UPDATE ledger_categories` 只写 name/sort_order/archived，漏了 kind。
// 服务层把 kind 改在内存对象上、接口返回 200，但数据库里没变，前端刷新即回到旧类型。
// 这类"少一列"的缺陷在没有真实 PG 的 CI 里跑不出来（store 集成用例默认 Skip），
// 只能用语句断言兜住：任何把 kind 从 UPDATE 里删掉的改动都会让这条用例变红。
func TestUpdateCategoryIncludesKind(t *testing.T) {
	const q = updateLedgerCategorySQL

	if !strings.Contains(q, "kind = $6") {
		t.Fatalf("UPDATE 语句必须写回 kind（缺列会导致 PATCH 静默失效）：\n%s", q)
	}
	if !strings.HasPrefix(strings.TrimSpace(q), "UPDATE ledger_categories SET") {
		t.Fatalf("语句目标表/动作不对：\n%s", q)
	}

	// 占位符必须连续且只到 $6：参数顺序是 (uid, cid, name, sortOrder, archived, kind)，
	// 多一个或少一个都会在运行时变成"参数个数不匹配"的 500。
	for _, p := range []string{"$1", "$2", "$3", "$4", "$5", "$6"} {
		if !strings.Contains(q, p) {
			t.Fatalf("缺少占位符 %s：\n%s", p, q)
		}
	}
	if strings.Contains(q, "$7") {
		t.Fatalf("占位符超过 $6，与 Exec 的参数个数不符：\n%s", q)
	}

	// WHERE 必须带 user_id：分类更新是跨账号越权的高危点。
	if !strings.Contains(q, "WHERE user_id = $1") {
		t.Fatalf("UPDATE 必须以 user_id 过滤，否则可跨账号改分类：\n%s", q)
	}
}
