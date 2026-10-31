package apierr

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// errSentinel 用于验证 errors.Is 能穿透 *Error 的包装链。
var errSentinel = errors.New("底层连接被拒绝")

// TestWrapPreservesCause 覆盖 A-12：Wrap 必须保留 cause，且不泄露到 JSON。
func TestWrapPreservesCause(t *testing.T) {
	wrapped := Wrap(CodeInternal, errSentinel, "登录失败，请稍后重试")

	if !errors.Is(wrapped, errSentinel) {
		t.Fatal("errors.Is 无法追溯到被包装的错误")
	}
	if cause := wrapped.Cause(); cause != errSentinel {
		t.Fatalf("Cause() = %v，期望原错误", cause)
	}
	// 日志里要能看到原因（否则排查时只看到一句用户文案）。
	if !strings.Contains(wrapped.Error(), errSentinel.Error()) {
		t.Fatalf("Error() 未拼接 cause: %q", wrapped.Error())
	}

	// 客户端只能看到 code/message/details —— cause 不得出现在 JSON 中。
	raw, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), errSentinel.Error()) {
		t.Fatalf("JSON 泄露了内部错误细节: %s", raw)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(body) != 2 { // code + message（details 为空被 omitempty 掉）
		t.Fatalf("响应字段与旧行为不一致: %s", raw)
	}
}

// TestWrapNil 无底层错误时不应产生 cause。
func TestWrapNil(t *testing.T) {
	e := Wrap(CodeBadRequest, nil, "参数有误")
	if e.Cause() != nil {
		t.Fatal("err 为 nil 时不应有 cause")
	}
	if errors.Unwrap(e) != nil {
		t.Fatal("err 为 nil 时 Unwrap 应返回 nil")
	}
}

// TestIsStillWorks 包装后仍能按 Code 判定（Is 依赖 errors.As）。
func TestIsStillWorks(t *testing.T) {
	e := Wrap(CodeConflict, errSentinel, "邮箱已注册")
	if !Is(e, CodeConflict) {
		t.Fatal("Is 应识别 Code")
	}
	if Is(e, CodeInternal) {
		t.Fatal("Is 命中了错误 Code")
	}
}
