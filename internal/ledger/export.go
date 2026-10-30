package ledger

// 记账本的导出：自包含 HTML 报告、最小合法 XLSX、ZIP 包与 JSON 备份。
//
// 【为什么不引入 excelize】
// 契约 8 的待裁决项明确「当前 go.mod 未包含 excelize」，本模块不新增第三方依赖：
// xlsx 本质是一个 ZIP + 若干 XML 部件，用标准库 archive/zip + encoding/xml 就能
// 产出 Excel/WPS/Numbers 均可打开的最小合法工作簿（字符串走 inlineStr，省掉
// sharedStrings 部件）。
//
// 【金额口径】全程 string + int64 分，不出现任何浮点运算；写进 xlsx 的也是
// decimal 文本（由整数分还原的精确字符串），不存在二进制浮点误差。
//
// 【附件】字节一律通过 AttachmentSource 取（契约由 lead 冻结，B4 实现）。
// 注入 nil 时导出仍必须可用，并在报告/清单里明确标注「不含附件」。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// 导出/备份格式标识与版本。导入时据此判断兼容性，不认识的 schema/version 一律拒收。
//
// backupSchema 由 B7（每日备份）声明在 backup.go：它的 ledger.json 与本导出结构同构
// （分类/账单字段名一致），因此导入侧直接接受它，用户拿每日备份也能恢复。
const (
	exportSchema        = "meddesk-ledger-export"
	exportSchemaVersion = 1
)

// 单次导出的规模上限：仓储单页上限 200，导出时按页翻到底；总量超过上限则提示用户缩小范围，
// 避免一次性把几十万行读进内存。
const (
	exportPageSize      = 200
	maxExportRecords    = 20000
	maxInlineFileBytes  = 3 << 20  // 单个附件内嵌上限 3 MiB
	maxInlineTotalBytes = 32 << 20 // 单次导出内嵌字节总量上限 32 MiB
)

// mimeAllow 是可内嵌进 HTML（data: URI）的图片类型：非白名单的附件不内嵌，
// 避免把 text/html、svg 之类可直接执行脚本的内容塞进 <img>/<a href>。
var mimeAllow = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/gif":  "gif",
	"image/webp": "webp",
	"image/bmp":  "bmp",
}

// extAllow 是扩展名 → mime 的兜底映射（AttachmentRef.Mime 为空时用）。
var extAllow = map[string]string{
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"gif":  "image/gif",
	"webp": "image/webp",
	"bmp":  "image/bmp",
}

// ------------------------------------------------------------------ 筛选条件

// ExportFilter 是导出请求支持的筛选条件，覆盖：本月 / 上月 / 自定义时间范围 /
// 分类多选 / 仅待报销 / 当前勾选记录（ids）。
type ExportFilter struct {
	Preset      string   `json:"preset"`      // thisMonth | lastMonth | custom | all（默认 all）
	From        string   `json:"from"`        // 自定义起始自然日 YYYY-MM-DD（含）
	To          string   `json:"to"`          // 自定义结束自然日 YYYY-MM-DD（含）
	Type        string   `json:"type"`        // income | expense | ""
	CategoryIDs []string `json:"categoryIds"` // 分类多选
	Keyword     string   `json:"keyword"`     // 备注/分类名关键词
	ReimbOnly   bool     `json:"reimbOnly"`   // 仅待报销项目
	IDs         []string `json:"ids"`         // 当前勾选记录（多选）

	// IncludeAttachments 仅对 JSON 备份生效：false 时只登记附件清单，不内嵌 base64。
	IncludeAttachments bool `json:"includeAttachments"`
}

// datePattern 约束自然日格式（自定义区间来自用户输入，必须先校验再进 SQL）。
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// toModel 把导出筛选翻译成仓储查询条件，并解析出统计区间 [from, to]。
func (f ExportFilter) toModel() (model.LedgerFilter, string, string, error) {
	from, to, err := f.resolveRange()
	if err != nil {
		return model.LedgerFilter{}, "", "", err
	}
	typ := strings.TrimSpace(f.Type)
	if typ != "" && typ != model.LedgerTypeIncome && typ != model.LedgerTypeExpense {
		return model.LedgerFilter{}, "", "", apierr.New(apierr.CodeBadRequest, "type 只能是 income 或 expense")
	}
	filter := model.LedgerFilter{
		Type:    typ,
		Keyword: strings.TrimSpace(f.Keyword),
		From:    from,
		To:      to,
	}
	// 「仅待报销」等价于报销状态 = pending（收入恒为 none，天然被排除）。
	if f.ReimbOnly {
		filter.ReimbStatus = model.LedgerReimbPending
		filter.Type = model.LedgerTypeExpense
	}
	for _, id := range f.CategoryIDs {
		if id = strings.TrimSpace(id); id != "" {
			filter.CategoryIDs = append(filter.CategoryIDs, id)
		}
	}
	for _, id := range f.IDs {
		if id = strings.TrimSpace(id); id != "" {
			filter.IDs = append(filter.IDs, id)
		}
	}
	if len(filter.IDs) > 1000 {
		return model.LedgerFilter{}, "", "", apierr.New(apierr.CodeBadRequest, "一次最多导出 1000 条勾选记录")
	}
	return filter, from, to, nil
}

// resolveRange 解析时间范围。缺省（preset 为空且没传 from/to）视为「全部」。
func (f ExportFilter) resolveRange() (string, string, error) {
	preset := strings.TrimSpace(f.Preset)
	from := strings.TrimSpace(f.From)
	to := strings.TrimSpace(f.To)
	// 只传一端（如"某日之后"）是常见需求，按不限处理；两端都给了才算自定义区间。
	if preset == "" && from != "" && to != "" {
		preset = "custom"
	}
	switch preset {
	case "", "all":
		// 允许只传一端（"某日之后" 这种需求很常见）
		return checkDate(from), checkDate(to), nil
	case "thisMonth", "month":
		f, t := MonthRange("")
		return f, t, nil
	case "lastMonth":
		f, t := lastMonthRange(time.Now().In(shanghai))
		return f, t, nil
	case "custom":
		if from == "" || to == "" {
			return "", "", apierr.New(apierr.CodeBadRequest, "自定义时间范围需要同时提供 from 与 to")
		}
		f2, t2 := checkDate(from), checkDate(to)
		if f2 == "" || t2 == "" {
			return "", "", apierr.New(apierr.CodeBadRequest, "时间范围格式有误，应为 YYYY-MM-DD")
		}
		if f2 > t2 {
			return "", "", apierr.New(apierr.CodeBadRequest, "开始日期不能晚于结束日期")
		}
		return f2, t2, nil
	default:
		return "", "", apierr.New(apierr.CodeBadRequest, "未知的导出范围 preset，可用值：thisMonth / lastMonth / custom / all")
	}
}

// lastMonthRange 返回 now 所在月份的「上一个自然月」[首日, 末日]。
//
// 【为什么不直接 now.AddDate(0, -1, 0)】AddDate 对不存在的日期会做归一化：
// 2026-10-31 减一个月得到 2026-09-31 → 被归一化成 2026-10-01，于是「上月」
// 被算成了「本月」，用户点「导出上月账单」拿到的却是本月账单（每月 31 日、
// 3 月 30/31 日等约每年 7 天会触发）。改成先取当月 1 号再减一个月：日恒为 1，
// 任何月份都存在，天然不会触发归一化。
//
// now 按可注入参数传入，测试可以固定日期而不依赖真实时钟（见 export_test.go）。
func lastMonthRange(now time.Time) (string, string) {
	cur := now.In(shanghai)
	first := time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, shanghai)
	return MonthRange(first.AddDate(0, -1, 0).Format("2006-01"))
}

// checkDate 校验自然日格式：合法返回原值，非法或为空返回空串（表示不限）。
//
// 为什么不直接报错：导出是「尽量给结果」的操作，手输一个带空格的日期就整次失败体验很差，
// 而格式错误在 SQL 层也只会被当成不限，不会造成安全问题（值不进 SQL，只作为占位符参数）。
func checkDate(v string) string {
	if v == "" {
		return ""
	}
	if !datePattern.MatchString(v) {
		return ""
	}
	if _, err := time.ParseInLocation("2006-01-02", v, shanghai); err != nil {
		return ""
	}
	return v
}

// ------------------------------------------------------------------ 服务

// ExportService 组装导出/导入所需的依赖。
//
// attachments 允许为 nil：此时导出正常工作，只是不含附件字节（会明确标注）。
type ExportService struct {
	repo        store.LedgerRepo
	attachments AttachmentSource

	// now 是时钟注入点，仅测试使用（固定导出时间与文件名）；生产为 time.Now。
	now func() time.Time
}

// NewExportService 构造导出服务。
func NewExportService(repo store.LedgerRepo, attachments AttachmentSource) *ExportService {
	return &ExportService{repo: repo, attachments: attachments}
}

// nowIn 返回上海时区下的当前时刻（用于导出时间与文件名）。
func (s *ExportService) nowIn() time.Time {
	if s.now != nil {
		return s.now().In(shanghai)
	}
	return time.Now().In(shanghai)
}

// hasAttachments 判断是否接入了附件来源。
func (s *ExportService) hasAttachments() bool { return s.attachments != nil }

// errEmptyExport 是「空结果」的统一错误：宁可 400 也不生成看似有效、实则空的文件。
func errEmptyExport() error {
	return apierr.New(apierr.CodeBadRequest, "没有符合条件的账单，未生成导出文件")
}

// ------------------------------------------------------------------ 取数

// exportData 是一次导出的完整素材。
type exportData struct {
	Filter      ExportFilter
	From        string
	To          string
	GeneratedAt time.Time

	Records    []model.LedgerRecord
	Categories []model.LedgerCategory

	Income  model.Money
	Expense model.Money
	Balance model.Money

	IncomeByCat  []model.LedgerCategoryAmount
	ExpenseByCat []model.LedgerCategoryAmount

	Pending      []model.LedgerRecord // 报销明细（待报销支出）
	PendingTotal model.Money

	// Attachments 是 recordID -> 附件引用；Bytes 是 attachmentID -> 字节（仅可取到时存在）。
	Attachments map[string][]AttachmentRef
	Bytes       map[string][]byte
	Missing     map[string]string // attachmentID -> 未内嵌原因
	AttachCount int
	// AttachmentsIncluded 表示本次导出是否真的带上了附件字节。
	AttachmentsIncluded bool
	// AttachmentsNote 说明附件缺失原因（会写进 JSON 备份与 HTML）。
	AttachmentsNote string
}

// collect 按筛选条件取出全部素材；结果为空返回 400，调用方不得继续生成文件。
//
// withBytes 为 true 时读取附件字节（HTML / ZIP 内嵌，或 JSON 明确要求内嵌）。
func (s *ExportService) collect(ctx context.Context, userID string, f ExportFilter, withBytes bool) (*exportData, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	filter, from, to, err := f.toModel()
	if err != nil {
		return nil, err
	}
	records, err := s.allRecords(ctx, userID, filter)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errEmptyExport()
	}
	cats, err := s.repo.ListCategories(ctx, userID, "", true)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取分类失败")
	}

	d := &exportData{
		Filter:      f,
		From:        from,
		To:          to,
		GeneratedAt: s.nowIn(),
		Records:     records,
		Categories:  cats,
		Attachments: map[string][]AttachmentRef{},
		Bytes:       map[string][]byte{},
		Missing:     map[string]string{},
	}
	summarize(d)
	if err := s.loadAttachments(ctx, userID, d, withBytes); err != nil {
		return nil, err
	}
	return d, nil
}

// allRecords 分页取回全部命中账单（仓储单页上限 200，这里翻到底），并套用导出上限。
func (s *ExportService) allRecords(ctx context.Context, userID string, f model.LedgerFilter) ([]model.LedgerRecord, error) {
	return s.listRecords(ctx, userID, f, maxExportRecords)
}

// listRecords 分页取回全部命中账单；limit > 0 时超过条数上限返回 400（导出场景）。
//
// 导入/回滚场景传 0：这里必须拿到真实全量，否则快照不全就回滚不干净。
func (s *ExportService) listRecords(ctx context.Context, userID string, f model.LedgerFilter, limit int64) ([]model.LedgerRecord, error) {
	out := []model.LedgerRecord{}
	for page := 1; ; page++ {
		f.Page = page
		f.Size = exportPageSize
		list, total, err := s.repo.ListRecords(ctx, userID, f)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeInternal, err, "读取账单失败")
		}
		if limit > 0 && total > limit {
			return nil, apierr.Newf(apierr.CodeBadRequest,
				"符合条件的账单共 %d 条，超过单次处理上限 %d，请缩小时间范围后重试", total, limit)
		}
		out = append(out, list...)
		if len(list) == 0 || int64(len(out)) >= total {
			break
		}
	}
	return out, nil
}

// summarize 在内存里汇总：金额用整数分累加（精确），再转成 Money 展示。
//
// 为什么不直接调 SumRecords：导出文件里的汇总必须和文件里的明细完全一致，
// 用同一批 records 现算可避免「两次查询之间数据变动导致对不上」。
func summarize(d *exportData) {
	var income, expense int64
	type key struct {
		typ, id string
	}
	sum := map[key]int64{}
	name := map[key]string{}
	count := map[key]int{}
	for _, r := range d.Records {
		k := key{typ: r.Type, id: r.CategoryID}
		sum[k] += r.AmountCents
		name[k] = r.CategoryName
		count[k]++
		if r.Type == model.LedgerTypeIncome {
			income += r.AmountCents
		} else {
			expense += r.AmountCents
		}
		if r.ReimbursementStatus == model.LedgerReimbPending && r.Type == model.LedgerTypeExpense {
			d.Pending = append(d.Pending, r)
		}
	}
	d.Income = model.MoneyFromCents(income)
	d.Expense = model.MoneyFromCents(expense)
	d.Balance = model.MoneyFromCents(income - expense)
	for k, cents := range sum {
		item := model.LedgerCategoryAmount{CategoryID: k.id, Name: name[k], Money: model.MoneyFromCents(cents)}
		if k.typ == model.LedgerTypeIncome {
			d.IncomeByCat = append(d.IncomeByCat, item)
		} else {
			d.ExpenseByCat = append(d.ExpenseByCat, item)
		}
	}
	sortByCentsDesc(d.IncomeByCat)
	sortByCentsDesc(d.ExpenseByCat)

	var pending int64
	for _, r := range d.Pending {
		pending += r.AmountCents
	}
	d.PendingTotal = model.MoneyFromCents(pending)
}

func sortByCentsDesc(list []model.LedgerCategoryAmount) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Money.Cents != list[j].Money.Cents {
			return list[i].Money.Cents > list[j].Money.Cents
		}
		return list[i].Name < list[j].Name
	})
}

// loadAttachments 登记附件清单，按需读取字节。
//
// 未接入附件来源（attachments == nil）时只标注「不含附件」，绝不影响账单数据导出。
func (s *ExportService) loadAttachments(ctx context.Context, userID string, d *exportData, withBytes bool) error {
	if !s.hasAttachments() {
		// 账单上仍可能有凭证计数，报告里如实写「有 N 个凭证未随本文件导出」。
		for _, r := range d.Records {
			d.AttachCount += r.AttachmentCount
		}
		d.AttachmentsIncluded = false
		d.AttachmentsNote = "本次导出未接入附件来源，不含凭证图片"
		return nil
	}
	var used int64
	for _, r := range d.Records {
		refs, err := s.attachments.ListByRecord(ctx, userID, r.ID)
		if err != nil {
			// 单条附件读取失败不影响整次导出：记一条原因继续。
			d.Missing[r.ID] = "读取附件清单失败"
			continue
		}
		if len(refs) == 0 {
			continue
		}
		d.Attachments[r.ID] = refs
		for _, ref := range refs {
			d.AttachCount++
			if !withBytes {
				continue
			}
			if _, ok := mimeOf(ref.Mime, ref.Ext); !ok {
				d.Missing[ref.ID] = "附件类型不支持内嵌"
				continue
			}
			if used >= maxInlineTotalBytes {
				d.Missing[ref.ID] = "超出单次导出内嵌字节上限"
				continue
			}
			rc, err := s.attachments.Open(ctx, userID, ref.ID)
			if err != nil {
				d.Missing[ref.ID] = "附件已丢失或无权访问"
				continue
			}
			// LimitReader 多读 1 字节用于判定超限，避免把超大文件整块读进内存。
			buf, err := io.ReadAll(io.LimitReader(rc, maxInlineFileBytes+1))
			_ = rc.Close()
			if err != nil {
				d.Missing[ref.ID] = "附件读取失败"
				continue
			}
			if int64(len(buf)) > maxInlineFileBytes {
				d.Missing[ref.ID] = "附件过大，未内嵌"
				continue
			}
			d.Bytes[ref.ID] = buf
			used += int64(len(buf))
		}
	}
	d.AttachmentsIncluded = len(d.Bytes) > 0
	if d.AttachmentsIncluded && len(d.Missing) > 0 {
		d.AttachmentsNote = fmt.Sprintf("已内嵌 %d 个附件，另有 %d 个未能内嵌", len(d.Bytes), len(d.Missing))
	}
	return nil
}

// mimeOf 归一 mime：优先用 AttachmentRef.Mime，缺省按扩展名兜底；不在白名单内返回 false。
func mimeOf(mime, ext string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(mime))
	if _, ok := mimeAllow[m]; ok {
		return m, true
	}
	if m2, ok := extAllow[strings.ToLower(strings.TrimSpace(ext))]; ok {
		return m2, true
	}
	return "", false
}

// ------------------------------------------------------------------ HTML

// htmlRow 是报告里的一行账单；Images 为已校验过的 data: URI（template.URL 走白名单）。
type htmlRow struct {
	Date       string
	TypeText   string
	Category   string
	Amount     string
	Currency   string
	Note       string
	ReimbText  string
	AttachText string
	Images     []template.URL
}

type htmlCat struct {
	Name   string
	Amount string
	Count  int
}

type htmlData struct {
	Title          string
	GeneratedAt    string
	FilterDesc     string
	RecordCount    int
	Income         string
	Expense        string
	Balance        string
	Currency       string
	HasAttachments bool
	AttachNote     string
	Rows           []htmlRow
	IncomeCats     []htmlCat
	ExpenseCats    []htmlCat
	HasPending     bool
	PendingRows    []htmlRow
	PendingTotal   string
	PendingCount   int
}

// htmlReport 是报告的模板。
//
// 【为什么用 html/template】所有用户输入（备注、分类名、筛选描述）都经过模板自动转义，
// 手写 fmt.Sprintf 拼接 HTML 迟早会漏掉一处造成存储型 XSS。唯一绕过转义的是
// template.URL 类型的 data: URI——它由本文件生成（base64 + 白名单 mime），不含用户输入。
var htmlReport = template.Must(template.New("ledger-report").Funcs(template.FuncMap{
	"safeURL": func(u template.URL) template.URL { return u },
}).Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;margin:24px;color:#1f2328;background:#fff}
h1{font-size:20px;margin:0 0 4px}
h2{font-size:16px;margin:24px 0 8px;padding-left:8px;border-left:3px solid #3b82f6}
.meta{color:#6b7280;font-size:12px;margin-bottom:16px}
.cards{display:flex;gap:12px;flex-wrap:wrap;margin:12px 0}
.card{flex:1;min-width:140px;border:1px solid #e5e7eb;border-radius:8px;padding:10px 12px}
.card .k{font-size:12px;color:#6b7280}
.card .v{font-size:18px;font-weight:600;margin-top:4px}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{border:1px solid #e5e7eb;padding:6px 8px;text-align:left;vertical-align:top}
th{background:#f3f4f6;font-weight:600}
td.num{text-align:right;white-space:nowrap}
.income{color:#15803d}.expense{color:#b91c1c}
.note{color:#6b7280;font-size:12px}
.shots{display:flex;gap:8px;flex-wrap:wrap;margin:6px 0 0}
.shots img{max-width:220px;max-height:220px;border:1px solid #e5e7eb;border-radius:6px}
</style>
</head>
<body>
<h1>{{.Title}}</h1>
<div class="meta">导出时间：{{.GeneratedAt}}（Asia/Shanghai）　筛选条件：{{.FilterDesc}}</div>
<div class="meta">共 {{.RecordCount}} 条账单{{if not .HasAttachments}}　·　<strong>本文件不含附件</strong>{{end}}{{if .AttachNote}}　·　{{.AttachNote}}{{end}}</div>

<h2>收支结余</h2>
<div class="cards">
  <div class="card"><div class="k">收入</div><div class="v income">{{.Income}} {{.Currency}}</div></div>
  <div class="card"><div class="k">支出</div><div class="v expense">{{.Expense}} {{.Currency}}</div></div>
  <div class="card"><div class="k">结余</div><div class="v">{{.Balance}} {{.Currency}}</div></div>
  <div class="card"><div class="k">账单笔数</div><div class="v">{{.RecordCount}}</div></div>
</div>

<h2>账单明细</h2>
<table>
<thead><tr><th style="width:96px">日期</th><th style="width:56px">类型</th><th style="width:96px">分类</th>
<th style="width:96px">金额</th><th style="width:56px">币种</th><th style="width:80px">报销</th><th>备注 / 凭证</th></tr></thead>
<tbody>
{{range .Rows}}<tr>
<td>{{.Date}}</td><td>{{.TypeText}}</td><td>{{.Category}}</td>
<td class="num">{{.Amount}}</td><td>{{.Currency}}</td><td>{{.ReimbText}}</td>
<td>{{.Note}}<div class="note">{{.AttachText}}</div>{{if .Images}}<div class="shots">{{range .Images}}<img alt="凭证" src="{{.}}">{{end}}</div>{{end}}</td>
</tr>{{end}}
</tbody>
</table>

<h2>分类汇总</h2>
<table>
<thead><tr><th style="width:64px">类型</th><th>分类</th><th style="width:120px">金额</th></tr></thead>
<tbody>
{{range .IncomeCats}}<tr><td>收入</td><td>{{.Name}}</td><td class="num income">{{.Amount}}</td></tr>{{end}}
{{range .ExpenseCats}}<tr><td>支出</td><td>{{.Name}}</td><td class="num expense">{{.Amount}}</td></tr>{{end}}
</tbody>
</table>

{{if .HasPending}}
<h2>报销明细（待报销）</h2>
<div class="meta">共 {{.PendingCount}} 笔，合计 {{.PendingTotal}} {{.Currency}}</div>
<table>
<thead><tr><th style="width:96px">日期</th><th style="width:96px">分类</th><th style="width:96px">金额</th><th>备注</th></tr></thead>
<tbody>
{{range .PendingRows}}<tr><td>{{.Date}}</td><td>{{.Category}}</td><td class="num">{{.Amount}}</td><td>{{.Note}}</td></tr>{{end}}
</tbody>
</table>
{{end}}
</body>
</html>
`))

// BuildHTML 生成自包含 HTML 报告（图片 base64 内嵌，可离线打开）。
func (s *ExportService) BuildHTML(ctx context.Context, userID string, f ExportFilter) (filename string, body []byte, err error) {
	d, err := s.collect(ctx, userID, f, true)
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	if err := htmlReport.Execute(&buf, toHTMLData(d)); err != nil {
		return "", nil, apierr.Wrap(apierr.CodeInternal, err, "生成 HTML 报告失败")
	}
	return fmt.Sprintf("ledger-export-%s.html", d.GeneratedAt.Format("20060102")), buf.Bytes(), nil
}

// toHTMLData 把导出素材转成模板数据（含附件 data: URI）。
func toHTMLData(d *exportData) htmlData {
	out := htmlData{
		Title:          "记账本导出报告",
		GeneratedAt:    d.GeneratedAt.Format("2006-01-02 15:04:05"),
		FilterDesc:     filterDesc(d),
		RecordCount:    len(d.Records),
		Income:         d.Income.Value,
		Expense:        d.Expense.Value,
		Balance:        d.Balance.Value,
		Currency:       currencyOf(d.Records),
		HasAttachments: d.AttachmentsIncluded,
		AttachNote:     d.AttachmentsNote,
	}
	for _, r := range d.Records {
		row := htmlRow{
			Date:      r.OccurredOn,
			TypeText:  typeText(r.Type),
			Category:  r.CategoryName,
			Amount:    r.Amount,
			Currency:  r.Currency,
			Note:      displayNote(r.Note),
			ReimbText: reimbText(r.ReimbursementStatus),
		}
		refs := d.Attachments[r.ID]
		switch {
		case r.AttachmentCount == 0:
			row.AttachText = "无凭证"
		case !d.AttachmentsIncluded && len(refs) == 0:
			row.AttachText = fmt.Sprintf("有 %d 个凭证，未随本文件导出", r.AttachmentCount)
		default:
			row.AttachText = fmt.Sprintf("凭证 %d 个", len(refs))
		}
		for _, ref := range refs {
			if _, ok := mimeOf(ref.Mime, ref.Ext); !ok {
				continue
			}
			raw, ok := d.Bytes[ref.ID]
			if !ok {
				continue
			}
			mime, _ := mimeOf(ref.Mime, ref.Ext)
			row.Images = append(row.Images, template.URL("data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(raw)))
		}
		out.Rows = append(out.Rows, row)
	}
	for _, c := range d.IncomeByCat {
		out.IncomeCats = append(out.IncomeCats, htmlCat{Name: c.Name, Amount: c.Money.Value})
	}
	for _, c := range d.ExpenseByCat {
		out.ExpenseCats = append(out.ExpenseCats, htmlCat{Name: c.Name, Amount: c.Money.Value})
	}
	out.HasPending = len(d.Pending) > 0
	out.PendingCount = len(d.Pending)
	out.PendingTotal = d.PendingTotal.Value
	for _, r := range d.Pending {
		out.PendingRows = append(out.PendingRows, htmlRow{
			Date: r.OccurredOn, Category: r.CategoryName,
			Amount: r.Amount, Note: displayNote(r.Note),
		})
	}
	return out
}

func typeText(t string) string {
	if t == model.LedgerTypeIncome {
		return "收入"
	}
	return "支出"
}

func reimbText(s string) string {
	switch s {
	case model.LedgerReimbPending:
		return "待报销"
	case model.LedgerReimbDone:
		return "已报销"
	default:
		return "—"
	}
}

// displayNote 备注为空时给一个占位，避免表格里出现无法分辨的空格。
func displayNote(note string) string {
	if strings.TrimSpace(note) == "" {
		return "—"
	}
	return note
}

// currencyOf 取币种展示值；多币种混排时统一显示「多币种」以免误导。
func currencyOf(records []model.LedgerRecord) string {
	cur := ""
	for _, r := range records {
		c := strings.ToUpper(strings.TrimSpace(r.Currency))
		if c == "" {
			c = "CNY"
		}
		if cur == "" {
			cur = c
			continue
		}
		if cur != c {
			return "多币种"
		}
	}
	if cur == "" {
		return "CNY"
	}
	return cur
}

// filterDesc 生成人类可读的筛选条件描述（会经模板转义，无需在这里处理 XSS）。
func filterDesc(d *exportData) string {
	var parts []string
	switch {
	case d.From != "" && d.To != "":
		parts = append(parts, fmt.Sprintf("%s ~ %s", d.From, d.To))
	case d.From != "":
		parts = append(parts, d.From+" 起")
	case d.To != "":
		parts = append(parts, "截至 "+d.To)
	default:
		parts = append(parts, "全部时间")
	}
	if t := strings.TrimSpace(d.Filter.Type); t != "" {
		parts = append(parts, typeText(t))
	}
	if d.Filter.ReimbOnly {
		parts = append(parts, "仅待报销")
	}
	if n := len(d.Filter.CategoryIDs); n > 0 {
		parts = append(parts, fmt.Sprintf("指定 %d 个分类", n))
	}
	if k := strings.TrimSpace(d.Filter.Keyword); k != "" {
		parts = append(parts, "关键词："+k)
	}
	if n := len(d.Filter.IDs); n > 0 {
		parts = append(parts, fmt.Sprintf("勾选 %d 条", n))
	}
	return strings.Join(parts, "；")
}

// ------------------------------------------------------------------ XLSX（最小合法工作簿）

// xCell 是工作表里的一个单元格：字符串用 inlineStr（省掉 sharedStrings 部件），
// 数值直接写 <v>（金额是 decimal 文本，由整数分还原，无浮点误差）。
type xCell struct {
	XMLName xml.Name `xml:"c"`
	Ref     string   `xml:"r,attr"`
	Type    string   `xml:"t,attr,omitempty"`
	Inline  *xInline `xml:"is,omitempty"`
	Value   string   `xml:"v,omitempty"`
}

type xInline struct {
	Text string `xml:"t"`
}

type xRow struct {
	XMLName xml.Name `xml:"row"`
	R       int      `xml:"r,attr"`
	Cells   []xCell  `xml:"c"`
}

type xSheetData struct {
	Rows []xRow `xml:"row"`
}

type xWorksheet struct {
	XMLName   xml.Name   `xml:"http://schemas.openxmlformats.org/spreadsheetml/2006/main worksheet"`
	SheetData xSheetData `xml:"sheetData"`
}

// buildXLSX 生成最小合法 xlsx 字节流（账单明细 + 分类汇总两个工作表）。
func buildXLSX(d *exportData) ([]byte, error) {
	sheet1, err := marshalWorksheet(recordsSheet(d))
	if err != nil {
		return nil, err
	}
	sheet2, err := marshalWorksheet(categorySheet(d))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, body []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: d.GeneratedAt,
		})
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}
	parts := [][2]string{
		{"[Content_Types].xml", xlsxContentTypes},
		{"_rels/.rels", xlsxRootRels},
		{"xl/workbook.xml", fmt.Sprintf(xlsxWorkbook, "账单明细", "分类汇总")},
		{"xl/_rels/workbook.xml.rels", xlsxWorkbookRels},
	}
	for _, p := range parts {
		if err := write(p[0], []byte(p[1])); err != nil {
			return nil, apierr.Wrap(apierr.CodeInternal, err, "生成 xlsx 失败")
		}
	}
	if err := write("xl/worksheets/sheet1.xml", sheet1); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "生成 xlsx 失败")
	}
	if err := write("xl/worksheets/sheet2.xml", sheet2); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "生成 xlsx 失败")
	}
	if err := zw.Close(); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "生成 xlsx 失败")
	}
	return buf.Bytes(), nil
}

// marshalWorksheet 把行数据序列化成 worksheet XML（文本转义交给 encoding/xml）。
func marshalWorksheet(rows []xRow) ([]byte, error) {
	body, err := xml.Marshal(xWorksheet{SheetData: xSheetData{Rows: rows}})
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "生成工作表失败")
	}
	return append([]byte(xml.Header), body...), nil
}

// recordsSheet 构造"账单明细"工作表。
func recordsSheet(d *exportData) []xRow {
	header := []string{"日期", "类型", "分类", "金额", "币种", "报销状态", "备注", "凭证数", "记录ID", "发生时间"}
	rows := []xRow{newRow(1, textCells(header))}
	r := 2
	for _, rec := range d.Records {
		cells := textCells([]string{
			rec.OccurredOn, typeText(rec.Type), rec.CategoryName, "", rec.Currency,
			reimbText(rec.ReimbursementStatus), rec.Note, strconv.Itoa(rec.AttachmentCount),
			rec.ID, rec.TransactionAt.In(shanghai).Format("2006-01-02 15:04:05"),
		})
		// 金额与凭证数写成数值（无 t 属性），便于在 Excel 里直接求和/排序。
		cells[3] = xCell{Ref: colName(3) + strconv.Itoa(r), Value: rec.Amount}
		cells[7] = xCell{Ref: colName(7) + strconv.Itoa(r), Value: strconv.Itoa(rec.AttachmentCount)}
		for i := range cells {
			cells[i].Ref = colName(i) + strconv.Itoa(r)
		}
		rows = append(rows, xRow{R: r, Cells: cells})
		r++
	}
	return rows
}

// categorySheet 构造"分类汇总"工作表（收入、支出分开，不混算）。
func categorySheet(d *exportData) []xRow {
	rows := []xRow{newRow(1, textCells([]string{"类型", "分类", "金额", "笔数"}))}
	r := 2
	appendCat := func(typ string, list []model.LedgerCategoryAmount) {
		for _, c := range list {
			cells := textCells([]string{typeText(typ), c.Name, "", strconv.Itoa(countOf(d, typ, c.CategoryID))})
			cells[2] = xCell{Ref: colName(2) + strconv.Itoa(r), Value: c.Money.Value}
			for i := range cells {
				cells[i].Ref = colName(i) + strconv.Itoa(r)
			}
			rows = append(rows, xRow{R: r, Cells: cells})
			r++
		}
	}
	appendCat(model.LedgerTypeIncome, d.IncomeByCat)
	appendCat(model.LedgerTypeExpense, d.ExpenseByCat)
	return rows
}

// countOf 统计某分类在本批账单里的笔数（与汇总同源，保证对得上）。
func countOf(d *exportData, typ, categoryID string) int {
	var n int
	for _, rec := range d.Records {
		if rec.Type == typ && rec.CategoryID == categoryID {
			n++
		}
	}
	return n
}

func newRow(r int, cells []xCell) xRow {
	for i := range cells {
		cells[i].Ref = colName(i) + strconv.Itoa(r)
	}
	return xRow{R: r, Cells: cells}
}

func textCells(values []string) []xCell {
	cells := make([]xCell, 0, len(values))
	for _, v := range values {
		cells = append(cells, xCell{Type: "inlineStr", Inline: &xInline{Text: v}})
	}
	return cells
}

// colName 把 0 基列号转成 Excel 列名：0→A，25→Z，26→AA。
func colName(i int) string {
	s := ""
	i++
	for i > 0 {
		i--
		s = string(rune('A'+i%26)) + s
		i /= 26
	}
	return s
}

// xlsx 的静态部件：内容类型、包关系、工作簿与工作表关系。
// （这些部件不含用户输入，写成常量最直观；含账单数据的 sheet 走 encoding/xml。）
const xlsxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
<Override PartName="/xl/worksheets/sheet2.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`

const xlsxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

const xlsxWorkbook = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets>
<sheet name="%s" sheetId="1" r:id="rId1"/>
<sheet name="%s" sheetId="2" r:id="rId2"/>
</sheets>
</workbook>`

const xlsxWorkbookRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/>
</Relationships>`

// ------------------------------------------------------------------ ZIP

// exportManifest 是 ZIP 包内的清单（对齐契约 7 的 manifest.json 结构）。
type exportManifest struct {
	Schema              string         `json:"schema"`
	Version             int            `json:"version"`
	GeneratedAt         string         `json:"generatedAt"`
	RecordCount         int            `json:"recordCount"`
	AttachmentCount     int            `json:"attachmentCount"`
	AttachmentsIncluded bool           `json:"attachmentsIncluded"`
	Checksum            string         `json:"checksum"`
	Range               map[string]any `json:"range,omitempty"`
	Files               []manifestFile `json:"files"`
}

type manifestFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// BuildZIP 生成导出压缩包：ledger.xlsx + images/<recordId>/<attachmentId>.<ext> + manifest.json。
func (s *ExportService) BuildZIP(ctx context.Context, userID string, f ExportFilter) (filename string, body []byte, err error) {
	d, err := s.collect(ctx, userID, f, true)
	if err != nil {
		return "", nil, err
	}
	xlsx, err := buildXLSX(d)
	if err != nil {
		return "", nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []manifestFile{}
	put := func(path string, data []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     path,
			Method:   zip.Deflate,
			Modified: d.GeneratedAt,
		})
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, manifestFile{
			Path:   path,
			Bytes:  int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]),
		})
		return nil
	}

	if err := put("ledger.xlsx", xlsx); err != nil {
		return "", nil, apierr.Wrap(apierr.CodeInternal, err, "写入表格失败")
	}
	// 附件目录：路径中的 id 只保留安全字符，避免异常数据造成 ZIP 路径穿越。
	recordIDs := make([]string, 0, len(d.Attachments))
	for rid := range d.Attachments {
		recordIDs = append(recordIDs, rid)
	}
	sort.Strings(recordIDs)
	for _, rid := range recordIDs {
		for _, ref := range d.Attachments[rid] {
			raw, ok := d.Bytes[ref.ID]
			if !ok {
				continue
			}
			ext := strings.ToLower(strings.TrimSpace(ref.Ext))
			if ext == "" {
				if m, ok := mimeAllow[strings.ToLower(ref.Mime)]; ok {
					ext = m
				} else {
					ext = "bin"
				}
			}
			path := "images/" + safePathPart(rid) + "/" + safePathPart(ref.ID) + "." + safePathPart(ext)
			if err := put(path, raw); err != nil {
				return "", nil, apierr.Wrap(apierr.CodeInternal, err, "写入附件失败")
			}
		}
	}

	m := exportManifest{
		Schema:              exportSchema,
		Version:             exportSchemaVersion,
		GeneratedAt:         d.GeneratedAt.Format(time.RFC3339),
		RecordCount:         len(d.Records),
		AttachmentCount:     len(d.Bytes),
		AttachmentsIncluded: d.AttachmentsIncluded,
		Files:               files,
	}
	if d.From != "" || d.To != "" {
		m.Range = map[string]any{"from": d.From, "to": d.To}
	}
	// 整体校验和：把（sorted）"sha256  路径" 逐行再算一次 sha256。
	// 这样只改一个字节、或删掉/多加一个文件，都会被发现。
	m.Checksum = overallChecksum(files)
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", nil, apierr.Wrap(apierr.CodeInternal, err, "生成清单失败")
	}
	if err := put("manifest.json", manifest); err != nil {
		return "", nil, apierr.Wrap(apierr.CodeInternal, err, "写入清单失败")
	}
	if err := zw.Close(); err != nil {
		return "", nil, apierr.Wrap(apierr.CodeInternal, err, "生成压缩包失败")
	}
	return fmt.Sprintf("ledger-export-%s.zip", d.GeneratedAt.Format("20060102")), buf.Bytes(), nil
}

// overallChecksum 计算整包校验和（对已排序的 "摘要  路径" 行再取 sha256）。
func overallChecksum(files []manifestFile) string {
	sorted := make([]manifestFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s  %s\n", f.SHA256, f.Path)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// safePathPart 只保留 [A-Za-z0-9._-]，其余替换为下划线；空值回退 "unknown"。
//
// 为什么需要：附件/账单 id 来自数据库，理论上都是 UUID，但导出路径一旦被异常数据
// 污染就可能写出 ../ 之类的穿越路径，这里做一次兜底净化。
func safePathPart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "unknown"
	}
	return strings.Trim(sb.String(), ".")
}

// ------------------------------------------------------------------ JSON 备份

// BackupCategory / BackupRecord / BackupAttachment 是 JSON 备份里的元素。
type BackupCategory struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
	Archived  bool   `json:"archived"`
}

type BackupRecord struct {
	ID                  string `json:"id"`
	Type                string `json:"type"`
	Amount              string `json:"amount"`
	AmountCents         int64  `json:"amountCents"`
	Currency            string `json:"currency"`
	CategoryID          string `json:"categoryId"`
	CategoryName        string `json:"categoryName"`
	TransactionAt       string `json:"transactionAt"`
	OccurredOn          string `json:"occurredOn"`
	Note                string `json:"note"`
	ReimbursementStatus string `json:"reimbursementStatus"`
	AttachmentCount     int    `json:"attachmentCount"`
	CreatedAt           string `json:"createdAt,omitempty"`
	UpdatedAt           string `json:"updatedAt,omitempty"`
}

type BackupAttachment struct {
	ID       string `json:"id"`
	RecordID string `json:"recordId"`
	Mime     string `json:"mime"`
	Ext      string `json:"ext"`
	Bytes    int64  `json:"bytes"`
	// Data 是 base64 编码的附件字节；Included 为 false 时为空（缺失原因见 Reason）。
	Data     string `json:"data,omitempty"`
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
}

// BackupPayload 是 JSON 备份的完整结构（导出与导入共用同一套 schema）。
type BackupPayload struct {
	Schema      string             `json:"schema"`
	Version     int                `json:"version"`
	ExportedAt  string             `json:"exported_at"`
	Categories  []BackupCategory   `json:"categories"`
	Records     []BackupRecord     `json:"records"`
	Attachments []BackupAttachment `json:"attachments"`
	// AttachmentsIncluded 明确标记本次备份是否真的带了附件字节；缺失时必为 false，
	// 避免恢复时误以为「恢复成功」而实际丢了凭证。
	AttachmentsIncluded bool   `json:"attachmentsIncluded"`
	AttachmentsNote     string `json:"attachmentsNote,omitempty"`
}

// BuildJSON 生成 JSON 备份；withAttachments 控制是否内嵌附件 base64。
func (s *ExportService) BuildJSON(ctx context.Context, userID string, f ExportFilter) (filename string, payload *BackupPayload, err error) {
	d, err := s.collect(ctx, userID, f, f.IncludeAttachments)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("ledger-backup-%s.json", d.GeneratedAt.Format("20060102")), toBackupPayload(d, f.IncludeAttachments), nil
}

// toBackupPayload 把导出素材转成备份结构。
func toBackupPayload(d *exportData, withAttachments bool) *BackupPayload {
	p := &BackupPayload{
		Schema:     exportSchema,
		Version:    exportSchemaVersion,
		ExportedAt: d.GeneratedAt.Format(time.RFC3339),
		Categories: []BackupCategory{},
		Records:    []BackupRecord{},
		// 附件切片始终非 nil：导入方可以直接 range，且空数组比 null 语义更明确。
		Attachments:         []BackupAttachment{},
		AttachmentsIncluded: d.AttachmentsIncluded,
		AttachmentsNote:     d.AttachmentsNote,
	}
	for _, c := range d.Categories {
		p.Categories = append(p.Categories, BackupCategory{
			ID: c.ID, Kind: c.Kind, Name: c.Name, SortOrder: c.SortOrder, Archived: c.Archived,
		})
	}
	for _, r := range d.Records {
		p.Records = append(p.Records, BackupRecord{
			ID: r.ID, Type: r.Type, Amount: r.Amount, AmountCents: r.AmountCents,
			Currency: r.Currency, CategoryID: r.CategoryID, CategoryName: r.CategoryName,
			TransactionAt:       r.TransactionAt.Format(time.RFC3339),
			OccurredOn:          r.OccurredOn,
			Note:                r.Note,
			ReimbursementStatus: r.ReimbursementStatus,
			AttachmentCount:     r.AttachmentCount,
			CreatedAt:           r.CreatedAt.Format(time.RFC3339),
			UpdatedAt:           r.UpdatedAt.Format(time.RFC3339),
		})
	}
	if !withAttachments {
		// 开关关闭：仍登记清单（included=false），让恢复方知道有多少凭证没带过来。
		for _, r := range d.Records {
			for _, ref := range d.Attachments[r.ID] {
				p.Attachments = append(p.Attachments, BackupAttachment{
					ID: ref.ID, RecordID: ref.RecordID, Mime: ref.Mime, Ext: ref.Ext, Bytes: ref.Bytes,
					Included: false, Reason: "本次备份未包含附件字节",
				})
			}
		}
		return p
	}
	for _, r := range d.Records {
		for _, ref := range d.Attachments[r.ID] {
			item := BackupAttachment{
				ID: ref.ID, RecordID: ref.RecordID, Mime: ref.Mime, Ext: ref.Ext, Bytes: ref.Bytes,
			}
			raw, ok := d.Bytes[ref.ID]
			if !ok {
				reason := d.Missing[ref.ID]
				if reason == "" {
					reason = "附件缺失"
				}
				item.Included = false
				item.Reason = reason
			} else {
				item.Included = true
				item.Data = base64.StdEncoding.EncodeToString(raw)
			}
			p.Attachments = append(p.Attachments, item)
		}
	}
	return p
}
