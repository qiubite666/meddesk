package export

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// sampleRequest 构造一份假数据的导出请求。
func sampleRequest() model.ExportRequest {
	return model.ExportRequest{
		Profile:   "张三",
		RangeFrom: "2026-10-01",
		RangeTo:   "2026-10-25",
		Stats:     model.ExportStats{Total: 2, Taken: 1, Missed: 1},
		Rows: []model.ExportRow{
			{Date: "2026-10-25", Time: "08:00", Profile: `张"三`, Medication: "布洛芬",
				Dosage: "0.5 片", Status: "taken", Note: "饭后\n服用"},
			{Date: "2026-10-25", Time: "20:00", Profile: "李四", Medication: "维生素D",
				Dosage: "1 粒", Status: "missed", Note: ""},
		},
	}
}

// TestRenderCSV 校验 BOM、行数、CRLF、引号转义与状态中文映射。
func TestRenderCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := NewService(config.PDFConfig{}).RenderCSV(&buf, sampleRequest()); err != nil {
		t.Fatalf("RenderCSV: %v", err)
	}
	b := buf.Bytes()
	if !bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("缺少 UTF-8 BOM")
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\r\n"), "\r\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d, 期望 3", len(lines))
	}
	if !strings.Contains(lines[0], `"日期","时间","家庭成员","药物","剂量","状态","备注"`) {
		t.Fatalf("表头不符: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"张""三"`) {
		t.Fatalf("内嵌引号未转义: %s", lines[1])
	}
	if !strings.Contains(lines[1], `"已服用"`) || !strings.Contains(lines[2], `"已漏服"`) {
		t.Fatalf("状态未映射为中文: %s | %s", lines[1], lines[2])
	}
	if strings.Contains(strings.TrimSuffix(string(b), "\r\n"), "\n服用") {
		t.Fatal("备注中的换行未折叠，会破坏行结构")
	}
}

// TestRenderCSVTooManyRows 校验行数上限保护。
func TestRenderCSVTooManyRows(t *testing.T) {
	req := sampleRequest()
	req.Rows = make([]model.ExportRow, maxExportRows+1)
	if err := NewService(config.PDFConfig{}).RenderCSV(&bytes.Buffer{}, req); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("期望 bad_request，实际 %v", err)
	}
}

// TestCSVFormulaInjection 校验 C-06：公式前缀被加单引号，纯数字/日期不被误伤。
func TestCSVFormulaInjection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=1+1", "\"'=1+1\""},
		{"=cmd|'/c calc'!A1", "\"'=cmd|'/c calc'!A1\""},
		{"+cmd", `"'+cmd"`},
		{"-2+3", `"'-2+3"`},
		{"@SUM(A1:A9)", `"'@SUM(A1:A9)"`},
		{"\t=cmd", "\"'\t=cmd\""},
		{"\r=cmd", "\"' =cmd\""}, // 前置单引号后再折叠换行
		{"-12.00", `"-12.00"`},   // 负数金额不能变成文本
		{"+8.5", `"+8.5"`},
		{"2026-10-31", `"2026-10-31"`},
		{"-", `"'-"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := csvSafeField(c.in); got != c.want {
			t.Fatalf("csvSafeField(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestRenderCSVHeaderUntouched 校验表头不做公式防护（固定文案，避免多余前缀）。
func TestRenderCSVHeaderUntouched(t *testing.T) {
	var buf bytes.Buffer
	if err := NewService(config.PDFConfig{}).RenderCSV(&buf, sampleRequest()); err != nil {
		t.Fatalf("RenderCSV: %v", err)
	}
	header := strings.Split(strings.TrimPrefix(buf.String(), "\uFEFF"), "\r\n")[0]
	if strings.Contains(header, "'") {
		t.Fatalf("表头不应被加单引号: %s", header)
	}
	// 数据行里的公式前缀必须被防护。
	req := sampleRequest()
	req.Rows[0].Note = "=1+1"
	req.Rows[0].Dosage = "-12.00"
	buf.Reset()
	if err := NewService(config.PDFConfig{}).RenderCSV(&buf, req); err != nil {
		t.Fatalf("RenderCSV: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\"'=1+1\"") {
		t.Fatalf("公式前缀未防护: %s", out)
	}
	if !strings.Contains(out, `"-12.00"`) || strings.Contains(out, `"'-12.00"`) {
		t.Fatalf("负数金额被误伤: %s", out)
	}
}

// TestFitText 校验 C-07：二分裁剪结果与旧线性实现一致，且不破坏 UTF-8。
func TestFitText(t *testing.T) {
	// 等宽 mock：1 rune = 1 单位，便于精确断言。
	width := func(s string) float64 { return float64(utf8.RuneCountInString(s)) }
	// 旧线性实现（逐 rune 回退）作为对照基准。
	linear := func(s string, maxW float64) string {
		s = strings.TrimSpace(s)
		if s == "" || width(s) <= maxW {
			return s
		}
		runes := []rune(s)
		for len(runes) > 1 {
			runes = runes[:len(runes)-1]
			if cand := string(runes) + ".."; width(cand) <= maxW {
				return cand
			}
		}
		return ""
	}
	inputs := []string{
		"abcdefghij",
		"中文测试文本超长截断",
		"混合 mixed 文本 12345",
		"a",
		"  trimmed  ",
		"",
	}
	for _, in := range inputs {
		for _, maxW := range []float64{0, 1, 3, 5, 8, 12, 100} {
			got := fitText(in, maxW, width)
			want := linear(in, maxW)
			if got != want {
				t.Fatalf("fitText(%q, %v) = %q，期望 %q", in, maxW, got, want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("fitText(%q, %v) 破坏了 UTF-8: %q", in, maxW, got)
			}
		}
	}
	// 宽度恒为 0 的退化函数：整串放得下，不应截断。
	if got := fitText("任意文本", 0, func(string) float64 { return 0 }); got != "任意文本" {
		t.Fatalf("零宽函数应原样返回，实际 %q", got)
	}
}

// TestRenderPDF 在本机存在中文字体时才真正渲染（否则 Skip）。
func TestRenderPDF(t *testing.T) {
	svc := NewService(config.PDFConfig{FontPath: os.Getenv("MEDDESK_PDF_FONT_PATH")})
	if resolveFont(svc.cfg.FontPath) == "" {
		t.Skip("本机未配置/未安装中文字体（设置 MEDDESK_PDF_FONT_PATH 可启用），跳过 PDF 渲染测试")
	}
	var buf bytes.Buffer
	if err := svc.RenderPDF(&buf, sampleRequest()); err != nil {
		t.Fatalf("RenderPDF: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
		t.Fatalf("输出不是合法 PDF（前 8 字节: %q）", buf.Bytes()[:8])
	}
	if buf.Len() < 1000 {
		t.Fatalf("PDF 过小，疑似渲染异常: %d 字节", buf.Len())
	}
}

// TestRenderPDFNoFont 校验字体缺失时返回 CodeUnavailable。
func TestRenderPDFNoFont(t *testing.T) {
	svc := NewService(config.PDFConfig{FontPath: "/nonexistent/meddesk-font.ttf"})
	err := svc.RenderPDF(&bytes.Buffer{}, sampleRequest())
	if !apierr.Is(err, apierr.CodeUnavailable) {
		t.Fatalf("期望 unavailable，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "MEDDESK_PDF_FONT_PATH") {
		t.Fatalf("错误信息应提示字体配置方式: %v", err)
	}
}
