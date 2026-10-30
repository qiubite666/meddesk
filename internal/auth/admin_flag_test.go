package auth

import "testing"

// TestAdminEmailWhitelist 覆盖「是否管理员」的判定。
//
// 这个标记只用于前端决定要不要显示「网站后台」入口；
// 真正的权限校验在 /api/v1/admin/* 的 RequireAdmin 中间件里，两者互不替代。
func TestAdminEmailWhitelist(t *testing.T) {
	svc, _, _ := newTestService()

	// 未注入白名单时，任何人都不是管理员（默认安全）。
	if svc.IsAdminEmail("a975707253@qq.com") {
		t.Fatal("未配置白名单时不应判定为管理员")
	}

	svc.SetAdminEmails([]string{" A975707253@QQ.com ", "", "ops@example.com"})

	cases := []struct {
		email string
		want  bool
	}{
		{"a975707253@qq.com", true},   // 大小写不敏感
		{"  A975707253@QQ.com ", true}, // 前后空白不影响
		{"ops@example.com", true},
		{"someone@example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := svc.IsAdminEmail(c.email); got != c.want {
			t.Fatalf("IsAdminEmail(%q) = %v，期望 %v", c.email, got, c.want)
		}
	}

	// 重复注入应当覆盖而不是累加（配置变更后旧白名单必须失效）。
	svc.SetAdminEmails([]string{"ops@example.com"})
	if svc.IsAdminEmail("a975707253@qq.com") {
		t.Fatal("重新注入后旧白名单应失效")
	}
	if !svc.IsAdminEmail("ops@example.com") {
		t.Fatal("重新注入后新白名单应生效")
	}
}
