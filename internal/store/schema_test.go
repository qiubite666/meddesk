package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOwnerRecordMatchesSchema 校验 OwnerRecord 的字段类型与 sync_records 的真实列类型兼容。
//
// 【为什么需要这条测试】
// family 相关的测试几乎都用内存桩，从不真正执行 SQL。结果出现过一个只有上线才炸的缺陷：
// 结构体把 updated_at 声明成 time.Time，而表里是 bigint（客户端 Unix 毫秒），
// rows.Scan 直接失败，接口对用户只显示「读取记录失败」。
// 桩测试永远抓不到这种问题，所以在这里做一次「结构体 ↔ 建表语句」的对齐检查。
func TestOwnerRecordMatchesSchema(t *testing.T) {
	mig := filepath.Join("migrations", "0001_init.sql")
	raw, err := os.ReadFile(mig)
	if err != nil {
		t.Fatalf("读取迁移文件失败（%s）：%v", mig, err)
	}

	// 截出 sync_records 的建表语句
	sql := string(raw)
	start := strings.Index(sql, "CREATE TABLE IF NOT EXISTS sync_records")
	if start < 0 {
		t.Fatalf("迁移里找不到 sync_records 建表语句")
	}
	end := strings.Index(sql[start:], ");")
	if end < 0 {
		t.Fatalf("sync_records 建表语句未正常结束")
	}
	body := sql[start : start+end]

	// 解析「列名 类型」
	colType := map[string]string{}
	lineRe := regexp.MustCompile(`(?m)^\s*([a-z_]+)\s+([a-z]+)`)
	for _, m := range lineRe.FindAllStringSubmatch(body, -1) {
		colType[m[1]] = m[2]
	}

	cases := []struct {
		column   string
		wantType string
		goField  string
	}{
		{"updated_at", "bigint", "UpdatedAt"},
		{"ver", "bigint", "Ver"},
		{"seq", "bigint", "Seq"},
		{"deleted", "boolean", "Deleted"},
		{"table_name", "text", "Table"},
		{"record_id", "text", "RecordID"},
	}
	for _, c := range cases {
		got, ok := colType[c.column]
		if !ok {
			t.Fatalf("sync_records 里找不到列 %s（建表语句可能变了）", c.column)
		}
		if got != c.wantType {
			t.Fatalf("列 %s 类型为 %s，期望 %s：OwnerRecord.%s 的类型要跟着改",
				c.column, got, c.wantType, c.goField)
		}
	}

	// 结构体侧：这些列都不该用 time.Time 接（表里没有 timestamptz 列）
	srcPath := "family_repo.go"
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", srcPath, err)
	}
	code := string(src)
	rs := strings.Index(code, "type OwnerRecord struct {")
	if rs < 0 {
		t.Fatalf("找不到 OwnerRecord 结构体定义")
	}
	re := strings.Index(code[rs:], "}")
	if re < 0 {
		t.Fatalf("OwnerRecord 结构体未正常结束")
	}
	structBody := code[rs : rs+re]
	if strings.Contains(structBody, "time.Time") {
		t.Fatalf("OwnerRecord 含 time.Time 字段，但 sync_records 没有 timestamptz 列："+
			"这会让 rows.Scan 失败并对外表现为「读取记录失败」\n%s", structBody)
	}
}
