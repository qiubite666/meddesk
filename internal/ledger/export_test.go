package ledger

// 导出/导入的单元测试。
//
// 【为什么不用真实数据库】导出与导入的逻辑全部在内存里完成（取数 → 渲染 → 打包 → 写回），
// 数据访问只需要 store.LedgerRepo 接口，因此用 internal/fake 的内存仓储 +
// net/http/httptest 就能覆盖完整链路，不需要 PostgreSQL。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/fake"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ------------------------------------------------------------------ 测试脚手架

// fixedNow 固定导出时间，保证文件名与生成时间可断言。
var fixedNow = time.Date(2026, 10, 30, 14, 5, 6, 0, shanghai)

// stubAttachments 是 AttachmentSource 的内存替身（B4 的实现尚未接入时也能跑测试）。
type stubAttachments struct {
	refs   map[string][]AttachmentRef // recordID -> refs
	data   map[string][]byte          // attachmentID -> bytes
	broken map[string]bool            // attachmentID -> 打开失败
}

func (s *stubAttachments) Open(ctx context.Context, userID, attachmentID string) (io.ReadCloser, error) {
	if s.broken[attachmentID] {
		return nil, errors.New("附件已丢失")
	}
	raw, ok := s.data[attachmentID]
	if !ok {
		return nil, errors.New("附件不存在")
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (s *stubAttachments) ListByRecord(ctx context.Context, userID, recordID string) ([]AttachmentRef, error) {
	return s.refs[recordID], nil
}

// newTestEnv 准备一套内存仓储 + 导出服务（带附件替身），并播种几条账单。
func newTestEnv(t *testing.T, withAttachments bool) (*fake.Repos, *ExportService, *stubAttachments) {
	t.Helper()
	repos := fake.New()
	repo := repos.Ledger()
	ctx := context.Background()
	if err := repo.EnsureCategories(ctx, testUser); err != nil {
		t.Fatalf("播种分类失败: %v", err)
	}
	cats, err := repo.ListCategories(ctx, testUser, "", true)
	if err != nil {
		t.Fatalf("读取分类失败: %v", err)
	}
	catByName := map[string]string{}
	for _, c := range cats {
		catByName[c.Kind+"/"+c.Name] = c.ID
	}
	seed := []model.LedgerRecord{
		{
			ID: "11111111-1111-1111-1111-111111111111", UserID: testUser, Type: model.LedgerTypeIncome,
			Amount: "1000.00", AmountCents: 100000, Currency: "CNY",
			CategoryID: catByName["income/工资"], CategoryName: "工资",
			TransactionAt: time.Date(2026, 10, 3, 9, 0, 0, 0, shanghai), OccurredOn: "2026-10-03",
			Note: "十月工资", ReimbursementStatus: model.LedgerReimbNone,
		},
		{
			ID: "22222222-2222-2222-2222-222222222222", UserID: testUser, Type: model.LedgerTypeExpense,
			Amount: "12.34", AmountCents: 1234, Currency: "CNY",
			CategoryID: catByName["expense/餐饮"], CategoryName: "餐饮",
			TransactionAt: time.Date(2026, 10, 5, 12, 30, 0, 0, shanghai), OccurredOn: "2026-10-05",
			Note: "<script>alert('xss')</script>", ReimbursementStatus: model.LedgerReimbPending,
			AttachmentCount: 1,
		},
	}
	for i := range seed {
		rec := seed[i]
		if err := repo.CreateRecord(ctx, &rec); err != nil {
			t.Fatalf("播种账单失败: %v", err)
		}
	}

	var src *stubAttachments
	if withAttachments {
		recID := seed[1].ID
		attID := "33333333-3333-3333-3333-333333333333"
		src = &stubAttachments{
			refs:   map[string][]AttachmentRef{recID: {{ID: attID, RecordID: recID, Mime: "image/png", Ext: "png", Bytes: 4}}},
			data:   map[string][]byte{attID: []byte("\x89PNG")},
			broken: map[string]bool{},
		}
	}
	svc := NewExportService(repo, nil)
	if src != nil {
		svc = NewExportService(repo, src)
	}
	svc.now = func() time.Time { return fixedNow }
	return repos, svc, src
}

const testUser = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

// newTestRouter 搭一个带鉴权桩的 gin 路由（user_id 直接写进上下文）。
func newTestRouter(t *testing.T, repos *fake.Repos, svc *ExportService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	MountExport(v1, svc, func(c *gin.Context) { c.Set(ctxUserIDKey, testUser) })
	return r
}

// ------------------------------------------------------------------ HTML

func TestBuildHTMLEscapesUserInput(t *testing.T) {
	_, svc, _ := newTestEnv(t, false)
	name, body, err := svc.BuildHTML(context.Background(), testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("生成 HTML 失败: %v", err)
	}
	if name != "ledger-export-20261030.html" {
		t.Fatalf("文件名不符合 Asia/Shanghai 口径: %s", name)
	}
	html := string(body)
	// 备注里的脚本必须被转义：这是离线报告最典型的存储型 XSS 入口。
	if strings.Contains(html, "<script>") {
		t.Fatalf("HTML 中存在未转义的 <script>：\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("HTML 未对用户输入做转义：\n%s", html)
	}
	// 关键区块都要在（导出时间、筛选、汇总、明细、分类汇总、报销明细）。
	for _, want := range []string{"2026-10-30 14:05:06", "收支结余", "账单明细", "分类汇总", "报销明细", "1000.00", "12.34"} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML 缺少 %q", want)
		}
	}
	// 未接入附件来源时必须明确标注"不含附件"。
	if !strings.Contains(html, "不含附件") {
		t.Fatalf("HTML 未标注不含附件")
	}
}

func TestBuildHTMLEmbedsImageAsDataURI(t *testing.T) {
	_, svc, _ := newTestEnv(t, true)
	_, body, err := svc.BuildHTML(context.Background(), testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("生成 HTML 失败: %v", err)
	}
	// iVBORw0 是 "PNG" 前 4 字节的 base64 前缀；出现 ZgotmplZ 说明被 html/template 过滤了。
	if !strings.Contains(string(body), "data:image/png;base64,iVBORw==") {
		t.Fatalf("附件未以 data URI 内嵌（可能被模板过滤）：\n%s", string(body))
	}
	if strings.Contains(string(body), "ZgotmplZ") {
		t.Fatalf("data URI 被 html/template 判定为不安全")
	}
}

// ------------------------------------------------------------------ ZIP / XLSX

func TestBuildZIPEntriesAndChecksum(t *testing.T) {
	_, svc, _ := newTestEnv(t, true)
	name, body, err := svc.BuildZIP(context.Background(), testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("生成 ZIP 失败: %v", err)
	}
	if name != "ledger-export-20261030.zip" {
		t.Fatalf("ZIP 文件名不符合契约: %s", name)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("ZIP 不可读: %v", err)
	}
	got := map[string]bool{}
	var manifest exportManifest
	for _, f := range zr.File {
		got[f.Name] = true
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("打开 manifest 失败: %v", err)
			}
			if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
				t.Fatalf("解析 manifest 失败: %v", err)
			}
			rc.Close()
		}
	}
	want := []string{
		"ledger.xlsx", "manifest.json",
		"images/22222222-2222-2222-2222-222222222222/33333333-3333-3333-3333-333333333333.png",
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("ZIP 缺少条目 %s，实际：%v", w, got)
		}
	}
	if manifest.Schema != exportSchema || manifest.Version != exportSchemaVersion {
		t.Fatalf("manifest 格式标识有误: %+v", manifest)
	}
	if manifest.RecordCount != 2 || manifest.AttachmentCount != 1 || !manifest.AttachmentsIncluded {
		t.Fatalf("manifest 统计有误: %+v", manifest)
	}
	// 校验和一致性：逐条重算 + 整体校验和重算。
	for _, f := range manifest.Files {
		sum := sha256.Sum256(readZipEntry(t, zr, f.Path))
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Fatalf("条目 %s 的 sha256 与 manifest 不一致", f.Path)
		}
	}
	if overallChecksum(manifest.Files) != manifest.Checksum {
		t.Fatalf("整体校验和不一致")
	}
}

func readZipEntry(t *testing.T, zr *zip.Reader, path string) []byte {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != path {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", path, err)
		}
		defer rc.Close()
		raw, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		return raw
	}
	t.Fatalf("ZIP 内没有 %s", path)
	return nil
}

// TestXLSXPartsParseable 校验最小 xlsx 的必备部件齐全且 sheet XML 合法可解析。
func TestXLSXPartsParseable(t *testing.T) {
	_, svc, _ := newTestEnv(t, true)
	_, body, err := svc.BuildZIP(context.Background(), testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("生成 ZIP 失败: %v", err)
	}
	outer, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("ZIP 不可读: %v", err)
	}
	// ledger.xlsx 本身就是一个 zip（OOXML 包）：把它再打开一次才能看到内部部件。
	xlsx := readZipEntry(t, outer, "ledger.xlsx")
	zr, err := zip.NewReader(bytes.NewReader(xlsx), int64(len(xlsx)))
	if err != nil {
		t.Fatalf("ledger.xlsx 不是合法 zip: %v", err)
	}
	for _, part := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml",
	} {
		if !hasZipEntry(zr, part) {
			t.Fatalf("xlsx 缺少部件 %s", part)
		}
	}
	// 工作表必须是合法 XML，且含表头与金额数值单元格（金额不带 inlineStr，便于 Excel 求和）。
	var sheet xWorksheet
	if err := xml.Unmarshal(readZipEntry(t, zr, "xl/worksheets/sheet1.xml"), &sheet); err != nil {
		t.Fatalf("sheet1 不是合法 XML: %v", err)
	}
	if len(sheet.SheetData.Rows) != 3 { // 表头 + 2 条账单
		t.Fatalf("sheet1 行数应为 3，实际 %d", len(sheet.SheetData.Rows))
	}
	var foundAmount bool
	for _, row := range sheet.SheetData.Rows {
		for _, c := range row.Cells {
			if c.Value == "12.34" {
				foundAmount = true
			}
		}
	}
	if !foundAmount {
		t.Fatalf("sheet1 未找到金额数值单元格")
	}
}

func hasZipEntry(zr *zip.Reader, name string) bool {
	for _, f := range zr.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ JSON 导出 → 导入 round-trip

func TestJSONExportImportRoundTrip(t *testing.T) {
	repos, svc, _ := newTestEnv(t, true)
	ctx := context.Background()

	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{IncludeAttachments: true})
	if err != nil {
		t.Fatalf("导出 JSON 失败: %v", err)
	}
	if payload.Schema != exportSchema || payload.Version != exportSchemaVersion {
		t.Fatalf("备份标识有误: %+v", payload)
	}
	if len(payload.Records) != 2 || len(payload.Categories) != 16 {
		t.Fatalf("备份内容条数有误: 账单 %d / 分类 %d", len(payload.Records), len(payload.Categories))
	}
	if !payload.AttachmentsIncluded || len(payload.Attachments) != 1 || !payload.Attachments[0].Included {
		t.Fatalf("附件未内嵌或标记错误: %+v", payload.Attachments)
	}
	if payload.Attachments[0].Data == "" {
		t.Fatalf("附件 base64 为空")
	}

	// 清空当前账号的账单，再用备份恢复——验证 round-trip 后数据一致。
	svc2 := NewExportService(repos.Ledger(), nil)
	snap, err := svc2.snapshot(ctx, testUser)
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	if err := svc2.clearAll(ctx, testUser, snap); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	res, err := svc2.Import(ctx, testUser, ImportRequest{Mode: ImportModeOverwrite, Payload: payload, Confirm: true})
	if err != nil {
		t.Fatalf("覆盖导入失败: %v", err)
	}
	if res.RecordsInserted != 2 || res.RecordsAfter != 2 {
		t.Fatalf("导入统计有误: %+v", res)
	}
	// 归属必须被强制改写为当前账号（payload 里的 user_id 不可信）。
	got, err := repos.Ledger().GetRecord(ctx, testUser, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("恢复后读不到原记录: %v", err)
	}
	if got.Amount != "1000.00" || got.AmountCents != 100000 || got.CategoryName != "工资" {
		t.Fatalf("恢复后的账单与备份不一致: %+v", got)
	}
}

// TestImportForcesCurrentUser 验证 payload 里的 user_id 不会被采信。
func TestImportForcesCurrentUser(t *testing.T) {
	repos, svc, _ := newTestEnv(t, false)
	ctx := context.Background()
	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	other := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	res, err := svc.Import(ctx, other, ImportRequest{Mode: ImportModeMerge, Payload: payload})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if res.RecordsInserted != 2 {
		t.Fatalf("导入条数有误: %+v", res)
	}
	list, _, err := repos.Ledger().ListRecords(ctx, other, model.LedgerFilter{Page: 1, Size: 100})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("导入到新账号的条数应为 2，实际 %d", len(list))
	}
	for _, r := range list {
		if r.UserID != other {
			t.Fatalf("记录归属未被强制改写: %+v", r)
		}
	}
}

// ------------------------------------------------------------------ 导入校验

func TestImportRejectsInvalidPayload(t *testing.T) {
	_, svc, _ := newTestEnv(t, false)
	ctx := context.Background()
	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(p *BackupPayload)
		wantMsg string
	}{
		{"版本不支持", func(p *BackupPayload) { p.Version = 99 }, "版本"},
		{"schema 不认识", func(p *BackupPayload) { p.Schema = "someone-else" }, "不支持的备份格式"},
		{"金额非法", func(p *BackupPayload) { p.Records[0].Amount = "12.345" }, "金额"},
		{"金额与分值不一致", func(p *BackupPayload) { p.Records[0].AmountCents = 1 }, "分值不一致"},
		{"类型非法", func(p *BackupPayload) { p.Records[0].Type = "transfer" }, "type"},
		// 分类名缺失且无法从备份的分类清单里兜底时才算非法（单缺 categoryName 会走 id 兜底）。
		{"缺分类名", func(p *BackupPayload) {
			p.Records[0].CategoryName = ""
			p.Records[0].CategoryID = "not-in-list"
		}, "分类名"},
		// 只对支出改状态：收入的报销状态会被归一成 none（契约要求），改它测不出问题。
		{"报销状态非法", func(p *BackupPayload) {
			for i := range p.Records {
				if p.Records[i].Type == model.LedgerTypeExpense {
					p.Records[i].ReimbursementStatus = "later"
					break
				}
			}
		}, "报销状态"},
		{"空备份", func(p *BackupPayload) { p.Records = nil }, "没有账单"},
	}
	for _, tc := range cases {
		p := *payload
		cp := payload // 深拷贝：直接改切片元素会污染后续用例
		copied := *cp
		copied.Records = append([]BackupRecord(nil), cp.Records...)
		copied.Categories = append([]BackupCategory(nil), cp.Categories...)
		p = copied
		tc.mutate(&p)
		_, err := svc.Import(ctx, testUser, ImportRequest{Mode: ImportModeMerge, Payload: &p})
		if err == nil {
			t.Fatalf("%s：应返回错误", tc.name)
		}
		if !apierr.Is(err, apierr.CodeBadRequest) {
			t.Fatalf("%s：应为 400，实际 %v", tc.name, err)
		}
		if !strings.Contains(apierr.FromError(err).Message, tc.wantMsg) {
			t.Fatalf("%s：错误信息应包含 %q，实际 %q", tc.name, tc.wantMsg, apierr.FromError(err).Message)
		}
	}
}

func TestImportMergeSkipsExisting(t *testing.T) {
	repos, svc, _ := newTestEnv(t, false)
	ctx := context.Background()
	// 本地改一下已有账单的备注，merge 不得覆盖。
	rec, err := repos.Ledger().GetRecord(ctx, testUser, "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	rec.Note = "本地已修改"
	if err := repos.Ledger().UpdateRecord(ctx, rec); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	res, err := svc.Import(ctx, testUser, ImportRequest{Mode: ImportModeMerge, Payload: payload})
	if err != nil {
		t.Fatalf("合并导入失败: %v", err)
	}
	if res.RecordsSkipped != 2 || res.RecordsInserted != 0 {
		t.Fatalf("merge 应全部跳过: %+v", res)
	}
	after, err := repos.Ledger().GetRecord(ctx, testUser, "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if after.Note != "本地已修改" {
		t.Fatalf("merge 覆盖了本地修改: %q", after.Note)
	}
}

func TestImportOverwriteRequiresConfirm(t *testing.T) {
	_, svc, _ := newTestEnv(t, false)
	ctx := context.Background()
	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	_, err = svc.Import(ctx, testUser, ImportRequest{Mode: ImportModeOverwrite, Payload: payload})
	if err == nil {
		t.Fatal("覆盖导入未要求确认")
	}
	if !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("应为 400，实际 %v", err)
	}
	if !strings.Contains(apierr.FromError(err).Message, "confirm") {
		t.Fatalf("错误应提示 confirm：%q", apierr.FromError(err).Message)
	}
}

// failCreateRepo 在前 failRemaining 次写账单时报错，用来验证覆盖导入失败会回滚。
//
// 为什么是"前 N 次"而不是"总是失败"：回滚本身也要写回原数据，
// 恒失败的替身会连回滚一起搞挂，测不出"数据是否恢复"。
type failCreateRepo struct {
	store.LedgerRepo
	failRemaining int
}

func (f *failCreateRepo) CreateRecord(ctx context.Context, r *model.LedgerRecord) error {
	if f.failRemaining > 0 {
		f.failRemaining--
		return errors.New("数据库写入失败")
	}
	return f.LedgerRepo.CreateRecord(ctx, r)
}

func TestImportOverwriteRollsBackOnFailure(t *testing.T) {
	repos, svc, _ := newTestEnv(t, false)
	ctx := context.Background()
	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	// 备份内容与当前数据完全相同，覆盖导入必然"先删后建"；
	// 让建的过程失败，验证删除能被回滚。
	broken := &failCreateRepo{LedgerRepo: repos.Ledger(), failRemaining: 1}
	svc2 := NewExportService(broken, nil)
	_, err = svc2.Import(ctx, testUser, ImportRequest{Mode: ImportModeOverwrite, Payload: payload, Confirm: true})
	if err == nil {
		t.Fatal("写入失败时应返回错误")
	}
	if !strings.Contains(apierr.FromError(err).Message, "回滚") {
		t.Fatalf("错误应说明已回滚：%q", apierr.FromError(err).Message)
	}
	// 原数据必须还在（回滚生效）。
	left, _, err := broken.ListRecords(ctx, testUser, model.LedgerFilter{Page: 1, Size: 100})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("回滚后应有 2 条账单，实际 %d", len(left))
	}
	for _, r := range left {
		if r.ID != "11111111-1111-1111-1111-111111111111" && r.ID != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("回滚后出现了非原始记录: %+v", r)
		}
	}
	cats, err := broken.ListCategories(ctx, testUser, "", true)
	if err != nil {
		t.Fatalf("读取分类失败: %v", err)
	}
	if len(cats) != 16 {
		t.Fatalf("回滚后分类应恢复为 16 个，实际 %d", len(cats))
	}
}

// ------------------------------------------------------------------ 空结果

func TestEmptyResultReturns400(t *testing.T) {
	repos := fake.New()
	svc := NewExportService(repos.Ledger(), nil)
	ctx := context.Background()

	if _, _, err := svc.BuildHTML(ctx, testUser, ExportFilter{}); err == nil {
		t.Fatal("空结果仍生成了 HTML")
	} else if !apierr.Is(err, apierr.CodeBadRequest) || !strings.Contains(apierr.FromError(err).Message, "没有符合条件的账单") {
		t.Fatalf("空结果应返回 400 并提示，实际 %v", err)
	}
	if _, _, err := svc.BuildZIP(ctx, testUser, ExportFilter{}); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("空结果 ZIP 应返回 400，实际 %v", err)
	}
	if _, _, err := svc.BuildJSON(ctx, testUser, ExportFilter{}); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("空结果 JSON 应返回 400，实际 %v", err)
	}
	// 筛选到没有命中的区间同样是 400（不得产出空壳文件）。
	_, svc2, _ := newTestEnv(t, false)
	if _, _, err := svc2.BuildHTML(ctx, testUser, ExportFilter{Preset: "custom", From: "2020-01-01", To: "2020-01-31"}); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("区间内无账单应返回 400，实际 %v", err)
	}
}

// ------------------------------------------------------------------ HTTP 层

func TestHTTPExportEndpoints(t *testing.T) {
	repos, svc, _ := newTestEnv(t, true)
	r := newTestRouter(t, repos, svc)

	// HTML：POST + JSON body
	w := httptest.NewRecorder()
	body, _ := json.Marshal(ExportFilter{Preset: "all"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ledger/export/html", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HTML 导出状态码 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "ledger-export-20261030.html") {
		t.Fatalf("HTML 缺少下载文件名: %q", w.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatalf("HTTP 返回的 HTML 未转义用户输入")
	}

	// ZIP：POST
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/ledger/export/zip", bytes.NewReader([]byte(`{"preset":"thisMonth"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ZIP 导出状态码 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "ledger-export-20261030.zip") {
		t.Fatalf("ZIP 文件名不符合契约: %q", w.Header().Get("Content-Disposition"))
	}

	// JSON：GET（查询串）
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/ledger/export/json?preset=all&includeAttachments=1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("JSON 导出状态码 %d：%s", w.Code, w.Body.String())
	}
	var payload BackupPayload
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(payload.Records) != 2 || !payload.AttachmentsIncluded {
		t.Fatalf("JSON 备份内容有误: %+v", payload)
	}

	// 导入：POST
	w = httptest.NewRecorder()
	importBody, _ := json.Marshal(ImportRequest{Mode: ImportModeMerge, Payload: &payload})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/ledger/import", bytes.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("导入状态码 %d：%s", w.Code, w.Body.String())
	}
	var res ImportResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("导入响应解析失败: %v", err)
	}
	if res.RecordsSkipped != 2 || res.RecordsInserted != 0 {
		t.Fatalf("重复导入应全部跳过: %+v", res)
	}
}

func TestHTTPEmptyResultReturns400(t *testing.T) {
	repos := fake.New()
	svc := NewExportService(repos.Ledger(), nil)
	r := newTestRouter(t, repos, svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ledger/export/html", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空结果应为 400，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "没有符合条件的账单") {
		t.Fatalf("错误提示不符合要求: %s", w.Body.String())
	}
}

// TestExportFilterPresets 覆盖本月 / 上月 / 自定义 / 勾选 ids / 仅待报销 五种筛选。
func TestExportFilterPresets(t *testing.T) {
	_, svc, _ := newTestEnv(t, false)
	ctx := context.Background()
	cases := []struct {
		name      string
		filter    ExportFilter
		wantCount int
		wantErr   bool
	}{
		{"本月", ExportFilter{Preset: "thisMonth"}, 2, false},
		{"上月（无数据）", ExportFilter{Preset: "lastMonth"}, 0, true},
		{"自定义命中", ExportFilter{Preset: "custom", From: "2026-10-01", To: "2026-10-31"}, 2, false},
		{"仅待报销", ExportFilter{ReimbOnly: true}, 1, false},
		{"勾选 ids", ExportFilter{IDs: []string{"11111111-1111-1111-1111-111111111111"}}, 1, false},
		{"非法 preset", ExportFilter{Preset: "nextYear"}, 0, true},
		{"自定义缺 to", ExportFilter{Preset: "custom", From: "2026-10-01"}, 0, true},
		{"起止倒置", ExportFilter{Preset: "custom", From: "2026-10-31", To: "2026-10-01"}, 0, true},
	}
	for _, tc := range cases {
		_, payload, err := svc.BuildJSON(ctx, testUser, tc.filter)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s：应返回错误", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s：导出失败 %v", tc.name, err)
		}
		if len(payload.Records) != tc.wantCount {
			t.Fatalf("%s：命中 %d 条，期望 %d", tc.name, len(payload.Records), tc.wantCount)
		}
	}
}

// TestAttachmentMissingMarked 验证附件读不出来时会被明确标记（而不是静默丢掉）。
func TestAttachmentMissingMarked(t *testing.T) {
	_, svc, src := newTestEnv(t, true)
	src.data = map[string][]byte{} // 清单还在，字节没了
	ctx := context.Background()
	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{IncludeAttachments: true})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if payload.AttachmentsIncluded {
		t.Fatalf("附件缺失时 attachmentsIncluded 应为 false")
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].Included || payload.Attachments[0].Reason == "" {
		t.Fatalf("缺失附件未明确标记: %+v", payload.Attachments)
	}
}
