// Package export 实现用药历史的 CSV / PDF 导出。
//
// 【隐私前提 · 必读】MedDesk 是端到端加密（E2EE）系统，服务端没有数据密钥，
// 无法解密任何业务数据。因此导出的唯一可行流程是：**客户端在本地解密后，
// 把明文行数据一次性 POST 给服务端**（请求体上限 4 MiB）。服务端
// **仅在内存中渲染**：不落盘任何临时文件、不把任何明文写入日志、不写审计，
// 响应结束后缓冲区即被丢弃。即便如此，本包仍遵守最小暴露原则——出错时只
// 返回错误码，绝不回显任何行内容。
package export

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// maxExportRows 是单次导出的行数上限，防止超大请求体把内存打爆。
const maxExportRows = 20000

// defaultTitle 是 PDF 默认标题（req.Title 为空时使用）。
const defaultTitle = "用药历史导出"

// csvHeader 是 CSV 列头。
var csvHeader = []string{"日期", "时间", "家庭成员", "药物", "剂量", "状态", "备注"}

// Service 是导出服务，只依赖 PDF 配置（中文字体路径）。
type Service struct {
	cfg config.PDFConfig
}

// NewService 构造导出服务。
func NewService(cfg config.PDFConfig) *Service {
	return &Service{cfg: cfg}
}

// RenderCSV 渲染 CSV：UTF-8 BOM + CRLF 换行，所有字段用双引号包裹并转义内嵌引号。
func (s *Service) RenderCSV(w io.Writer, req model.ExportRequest) error {
	if err := checkRows(req); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM，保证 Excel 正确识别中文
	writeCSVRow(&buf, csvHeader)
	for _, row := range req.Rows {
		writeCSVRow(&buf, []string{
			row.Date, row.Time, row.Profile, row.Medication,
			row.Dosage, statusText(row.Status), row.Note,
		})
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// RenderPDF 渲染 PDF：A4 纵向，标题 + 副标题 + 汇总区 + 表格 + 页脚。
// 找不到可用中文字体时返回 apierr.CodeUnavailable。
func (s *Service) RenderPDF(w io.Writer, req model.ExportRequest) error {
	if err := checkRows(req); err != nil {
		return err
	}
	fontPath := resolveFont(s.cfg.FontPath)
	if fontPath == "" {
		return apierr.New(apierr.CodeUnavailable, "未找到中文字体，请安装字体或设置 MEDDESK_PDF_FONT_PATH")
	}
	var buf bytes.Buffer
	if err := renderPDF(&buf, req, fontPath); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ------------------------------------------------------------------ CSV

// writeCSVRow 写一行 CSV：字段全部加引号，行尾固定 CRLF。
func writeCSVRow(buf *bytes.Buffer, fields []string) {
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(csvField(f))
	}
	buf.WriteString("\r\n")
}

// csvField 转义单个字段：换行折叠为空格（保证一行一条记录），内嵌引号翻倍。
func csvField(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ------------------------------------------------------------------ PDF

// PDF 版式常量（单位 mm，A4 = 210×297）。
const (
	pageW       = 210.0
	pageH       = 297.0
	margin      = 15.0
	footerResv  = 14.0 // 页脚预留高度
	rowH        = 7.0  // 表格行高
	headH       = 8.0  // 表头行高
	fontFamily  = "meddesk-cjk"
	cellPadding = 1.5
)

// pdfHeaders 是表格列头，colWidths 与之对应（合计 180 = 210 - 2×15）。
var (
	pdfHeaders = []string{"日期", "时间", "成员", "药物", "剂量", "状态", "备注"}
	colWidths  = []float64{22, 16, 26, 40, 24, 18, 34}
	colAligns  = []string{"C", "C", "L", "L", "C", "C", "L"}
)

// renderPDF 生成完整 PDF 文档到 w。
func renderPDF(w io.Writer, req model.ExportRequest, fontPath string) error {
	now := time.Now()
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(margin, margin, margin)
	// 关闭自动分页，改由下方手动判断是否换页，以便每页重复表头。
	pdf.SetAutoPageBreak(false, 0)
	pdf.AliasNbPages("{nb}") // 页脚中的总页数占位符
	// AddUTF8Font 内嵌 TTF/OTF 子集，中文直接以 UTF-8 写入即可。
	pdf.AddUTF8Font(fontFamily, "", fontPath)
	if pdf.Err() {
		return apierr.Wrap(apierr.CodeInternal, pdf.Error(), "加载中文字体失败")
	}

	stamp := now.Format("2006-01-02 15:04")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-margin + 1)
		pdf.SetFont(fontFamily, "", 8)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 5, fmt.Sprintf("生成时间：%s        第 %d / {nb} 页", stamp, pdf.PageNo()),
			"", 0, "C", false, 0, "")
	})

	pdf.AddPage()
	pdf.SetTextColor(24, 24, 24)

	// 标题
	pdf.SetFont(fontFamily, "", 16)
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = defaultTitle
	}
	pdf.CellFormat(0, 10, fitCell(pdf, title, pageW-2*margin), "", 1, "L", false, 0, "")

	// 副标题：成员 + 时间范围
	pdf.SetFont(fontFamily, "", 9)
	pdf.SetTextColor(110, 110, 110)
	pdf.CellFormat(0, 6, fitCell(pdf, subtitle(req), pageW-2*margin), "", 1, "L", false, 0, "")
	pdf.Ln(2)

	// 汇总区
	st := summarize(req)
	pdf.SetFont(fontFamily, "", 10)
	pdf.SetTextColor(24, 24, 24)
	pdf.CellFormat(0, 6, fmt.Sprintf("总次数 %d    已服用 %d    已跳过 %d    已漏服 %d    依从率 %.1f%%",
		st.Total, st.Taken, st.Skipped, st.Missed, adherencePct(st)), "", 1, "L", false, 0, "")
	pdf.Ln(3)

	// 表格
	drawTableHeader(pdf)
	limit := pageH - margin - footerResv
	for i, row := range req.Rows {
		if pdf.GetY()+rowH > limit {
			pdf.AddPage()
			pdf.SetTextColor(24, 24, 24)
			drawTableHeader(pdf)
		}
		drawTableRow(pdf, row, i%2 == 1)
	}

	return writePDF(w, pdf)
}

// drawTableHeader 画一页的表头（灰底 + 细边框）。
func drawTableHeader(pdf *fpdf.Fpdf) {
	pdf.SetFont(fontFamily, "", 9)
	pdf.SetFillColor(236, 241, 246)
	pdf.SetDrawColor(190, 200, 210)
	pdf.SetTextColor(60, 60, 60)
	for i, name := range pdfHeaders {
		pdf.CellFormat(colWidths[i], headH, name, "1", 0, "C", true, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetDrawColor(215, 220, 226)
	pdf.SetTextColor(24, 24, 24)
}

// drawTableRow 画一行数据，zebra 为 true 时加浅色底纹。
func drawTableRow(pdf *fpdf.Fpdf, row model.ExportRow, zebra bool) {
	pdf.SetFont(fontFamily, "", 8.5)
	if zebra {
		pdf.SetFillColor(249, 250, 252)
	} else {
		pdf.SetFillColor(255, 255, 255)
	}
	cells := []string{
		row.Date, row.Time, row.Profile, row.Medication,
		row.Dosage, statusText(row.Status), row.Note,
	}
	for i, txt := range cells {
		pdf.CellFormat(colWidths[i], rowH, fitCell(pdf, txt, colWidths[i]-cellPadding),
			"1", 0, colAligns[i], true, 0, "")
	}
	pdf.Ln(-1)
}

// writePDF 收尾并输出到 w；渲染期任何错误统一映射为内部错误。
func writePDF(w io.Writer, pdf *fpdf.Fpdf) error {
	if pdf.Err() {
		if err := pdf.Error(); err != nil {
			return apierr.Wrap(apierr.CodeInternal, err, "PDF 渲染失败")
		}
		return apierr.New(apierr.CodeInternal, "PDF 渲染失败")
	}
	if err := pdf.Output(w); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "PDF 输出失败")
	}
	return nil
}

// fitCell 把文本按显示宽度截断（以 rune 为单位，绝不破坏 UTF-8 编码），
// 超长时追加 ".."。fpdf 内嵌的是 UTF-8 字体，无需再做代码页转换；
// （go-pdf/fpdf v0.9.0 把 UnicodeTranslatorFromDescriptor 作为 *Fpdf 的方法提供，
// 这里直接用 GetStringWidth 手工截断，行为更可控且不会 panic。）
func fitCell(pdf *fpdf.Fpdf, s string, maxW float64) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if pdf.GetStringWidth(s) <= maxW {
		return s
	}
	runes := []rune(s)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		cand := string(runes) + ".."
		if pdf.GetStringWidth(cand) <= maxW {
			return cand
		}
	}
	return ""
}

// subtitle 生成副标题：成员 + 时间范围（缺省则显示「全部」）。
func subtitle(req model.ExportRequest) string {
	var parts []string
	if p := strings.TrimSpace(req.Profile); p != "" {
		parts = append(parts, "成员："+p)
	}
	from := strings.TrimSpace(req.RangeFrom)
	to := strings.TrimSpace(req.RangeTo)
	switch {
	case from != "" && to != "":
		parts = append(parts, fmt.Sprintf("范围：%s ~ %s", from, to))
	case from != "":
		parts = append(parts, "范围："+from+" 起")
	}
	if len(parts) == 0 {
		return "全部记录"
	}
	return strings.Join(parts, "    ")
}

// summarize 汇总统计：客户端未传 stats 时按行数据兜底计算。
func summarize(req model.ExportRequest) model.ExportStats {
	st := req.Stats
	if st.Total <= 0 && len(req.Rows) > 0 {
		st.Total = len(req.Rows)
		for _, r := range req.Rows {
			switch strings.ToLower(strings.TrimSpace(r.Status)) {
			case "taken":
				st.Taken++
			case "skipped":
				st.Skipped++
			case "missed":
				st.Missed++
			}
		}
	}
	return st
}

// adherencePct 依从率百分比：taken / total，保留 1 位小数。
func adherencePct(st model.ExportStats) float64 {
	if st.Total <= 0 {
		return 0
	}
	return float64(st.Taken) / float64(st.Total) * 100
}

// statusText 把状态枚举映射为中文；未知取值原样返回（不丢数据、不 panic）。
func statusText(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "taken":
		return "已服用"
	case "skipped":
		return "已跳过"
	case "snoozed":
		return "已推迟"
	case "pending":
		return "待服用"
	case "missed":
		return "已漏服"
	default:
		return s
	}
}

// checkRows 校验导出行数上限。
func checkRows(req model.ExportRequest) error {
	if len(req.Rows) > maxExportRows {
		return apierr.Newf(apierr.CodeBadRequest, "导出行数 %d 超过上限 %d，请缩小时间范围后重试",
			len(req.Rows), maxExportRows)
	}
	return nil
}

// ------------------------------------------------------------------ 字体探测

// fontCandidates 是 cfg.PDF.FontPath 为空时按顺序探测的中文字体路径。
//
// 注意：go-pdf/fpdf 对 `.ttc` 字体集合的支持有限（只能取集合内的第一个字体，
// 且部分 .ttc 会解析失败）。推荐部署脚本直接下载单个 `.ttf`
// （如 Noto Sans SC Regular）并通过 MEDDESK_PDF_FONT_PATH 指定。
var fontCandidates = []string{
	"/home/ops/service/meddesk/fonts/NotoSansSC-Regular.ttf",
	"/opt/meddesk/fonts/NotoSansSC-Regular.ttf",
	"/usr/share/fonts/truetype/noto/NotoSansSC-Regular.ttf",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
}

var (
	fontMu    sync.Mutex
	fontCache = map[string]string{} // 配置路径 -> 命中的字体路径（"" 表示未找到）
)

// resolveFont 返回可用的中文字体路径，找不到返回空串。
// 探测结果按显式配置做进程内缓存（命中与落空都缓存），避免每次导出都 stat 文件系统。
func resolveFont(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	fontMu.Lock()
	defer fontMu.Unlock()
	if v, ok := fontCache[explicit]; ok {
		return v
	}
	found := probeFont(explicit)
	fontCache[explicit] = found
	return found
}

// probeFont 依次检查显式路径与默认候选路径，返回第一个可读的字体文件。
func probeFont(explicit string) string {
	if explicit != "" {
		if isReadableFile(explicit) {
			return explicit
		}
		return ""
	}
	for _, p := range fontCandidates {
		if isReadableFile(p) {
			return p
		}
	}
	return ""
}

// isReadableFile 判断路径是否为存在的普通文件。
func isReadableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
