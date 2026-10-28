package model

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestKnownTablesCoverFrontend 防止「前端新增了表、服务端白名单忘了加」这类漂移。
//
// 【为什么必须存在这条测试】
// 服务端的表名白名单是硬校验：前端推来任何一个不在白名单里的 table，
// 服务端会以 400 拒绝整个批次 —— 不是只丢那一条，而是把同一批里的
// 用药记录、排程、服药记录全部卡住，前端只显示「同步失败」，
// 从界面上完全看不出是白名单缺项。
//
// 历史上真实发生过：v0.3.7 前端加了 family 表，服务端漏加，
// 结果家庭档案从未同步成功，并连带阻塞其它表。
//
// 只做单向校验（前端 ⊆ 服务端）：服务端允许但前端暂未使用的表（如 setting）
// 是合法的保留位，不应判为错误。
func TestKnownTablesCoverFrontend(t *testing.T) {
	jsPath := filepath.Join("..", "..", "web", "assets", "js", "data.js")
	raw, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("读取前端表清单失败（%s）：%v", jsPath, err)
	}

	// 匹配：var TABLES = ['profile', 'medication', ...];
	re := regexp.MustCompile(`var\s+TABLES\s*=\s*\[([^\]]*)\]`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("在 %s 中没找到 TABLES 数组定义，测试需要同步更新", jsPath)
	}

	var frontend []string
	for _, part := range strings.Split(string(m[1]), ",") {
		name := strings.TrimSpace(part)
		name = strings.Trim(name, `'"`)
		if name != "" {
			frontend = append(frontend, name)
		}
	}
	if len(frontend) == 0 {
		t.Fatalf("前端 TABLES 解析结果为空，正则可能失效")
	}

	known := make(map[string]bool, len(KnownTables()))
	for _, k := range KnownTables() {
		known[k] = true
	}

	var missing []string
	for _, name := range frontend {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("前端会同步这些表，但服务端白名单没有：%v\n"+
			"后果：推送含这些表的批次会被整体拒绝（400），并连带阻塞其它表。\n"+
			"修法：在 internal/model/model.go 的 KnownTables() 中补上对应常量。",
			missing)
	}
}
