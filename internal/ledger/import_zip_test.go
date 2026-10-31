package ledger

// 备份 ZIP 导入（ImportZIP）与 /ledger/import 双格式入口的测试。
//
// 覆盖四件事：
//  1. JSON 导入路径的行为与响应字段保持兼容（新字段只增不改）；
//  2. 备份 ZIP 能把账单 + 分类 + 凭证图片一起还原（附件真的落盘并登记元数据）；
//  3. zip slip（含 ../ 的条目）被整体拒绝，返回 400 而不是写文件；
//  4. 伪造/损坏的 ZIP 返回 400，不 panic。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/fake"
	"github.com/qiubite666/meddesk/internal/model"
)

// zipItem 是一个待写入的 ZIP 条目。
type zipItem struct {
	name string
	data []byte
}

// buildZip 按给定顺序打包（顺序即条目顺序，便于验证路径校验与排序逻辑）。
func buildZip(t *testing.T, items []zipItem) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, it := range items {
		w, err := zw.Create(it.name)
		if err != nil {
			t.Fatalf("写入 ZIP 条目 %s 失败: %v", it.name, err)
		}
		if _, err := w.Write(it.data); err != nil {
			t.Fatalf("写入 ZIP 条目内容 %s 失败: %v", it.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 ZIP 失败: %v", err)
	}
	return buf.Bytes()
}

// buildBackupZip 按 backup.go 的真实结构造包：ledger.json + manifest.json + images/…。
// images 的 key 是 images/ 下的相对路径（如 "images/<rec>/<att>.png"）。
func buildBackupZip(t *testing.T, payload *BackupPayload, images map[string][]byte) []byte {
	t.Helper()
	ledgerJSON, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("序列化 ledger.json 失败: %v", err)
	}
	paths := make([]string, 0, len(images))
	for p := range images {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files := append([]string{"ledger.json", "manifest.json"}, paths...)
	sum := sha256.Sum256(ledgerJSON)
	manifest, err := json.MarshalIndent(map[string]any{
		"schema":       backupSchema,
		"version":      exportSchemaVersion,
		"recordCount":  len(payload.Records),
		"checksum":     hex.EncodeToString(sum[:]),
		"checksumAlgo": "sha256",
		"files":        files,
	}, "", "  ")
	if err != nil {
		t.Fatalf("序列化 manifest.json 失败: %v", err)
	}
	items := []zipItem{
		{name: "ledger.json", data: ledgerJSON},
		{name: "manifest.json", data: manifest},
	}
	for _, p := range paths {
		items = append(items, zipItem{name: p, data: images[p]})
	}
	return buildZip(t, items)
}

// newImportRouter 搭一个把 user_id 固定成指定账号的路由。
func newImportRouter(t *testing.T, svc *ExportService, userID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	MountExport(r.Group("/api/v1"), svc, func(c *gin.Context) { c.Set(ctxUserIDKey, userID) })
	return r
}

// postImport 发一次导入请求，返回状态码与响应体。
func postImport(t *testing.T, r *gin.Engine, target, contentType string, body []byte) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// ------------------------------------------------------------------ 1. JSON 行为未变

func TestHTTPImportJSONUnchanged(t *testing.T) {
	repos, svc, _ := newTestEnv(t, false)
	r := newImportRouter(t, svc, testUser)

	_, payload, err := svc.BuildJSON(context.Background(), testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出 JSON 失败: %v", err)
	}
	body, _ := json.Marshal(ImportRequest{Mode: ImportModeMerge, Payload: payload})

	code, resp := postImport(t, r, "/api/v1/ledger/import", "application/json", body)
	if code != http.StatusOK {
		t.Fatalf("JSON 导入状态码 %d：%s", code, resp)
	}
	var res ImportResult
	if err := json.Unmarshal([]byte(resp), &res); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 重复导入：全部跳过（与历史行为一致），新增字段只是别名。
	if res.RecordsSkipped != 2 || res.RecordsInserted != 0 {
		t.Fatalf("重复导入统计有误: %+v", res)
	}
	if res.Imported != 0 || res.Skipped != 2 {
		t.Fatalf("imported/skipped 未与 recordsInserted/Skipped 对齐: %+v", res)
	}
	if res.AttachmentsRestored != 0 {
		t.Fatalf("JSON 导入不应恢复凭证: %+v", res)
	}

	// 空 body / 非法 JSON 仍然是 400「请求参数有误」，不能被 ZIP 分支吞掉。
	code, resp = postImport(t, r, "/api/v1/ledger/import", "application/json", []byte(`{`))
	if code != http.StatusBadRequest || !strings.Contains(resp, "请求参数有误") {
		t.Fatalf("非法 JSON 应 400 且提示请求参数有误，实际 %d：%s", code, resp)
	}

	// 没有 mode 的旧客户端仍会收到同一条提示。
	body, _ = json.Marshal(ImportRequest{Payload: payload})
	code, resp = postImport(t, r, "/api/v1/ledger/import", "application/json", body)
	if code != http.StatusBadRequest || !strings.Contains(resp, "请指定导入模式") {
		t.Fatalf("缺 mode 应 400，实际 %d：%s", code, resp)
	}
	if repos == nil {
		t.Fatal("unreachable")
	}
}

// ------------------------------------------------------------------ 2. ZIP 还原账单 + 凭证

func TestImportZIPRestoresRecordsAndAttachments(t *testing.T) {
	repos, cfgSvc, _ := newTestEnv(t, false)
	ctx := context.Background()

	_, payload, err := cfgSvc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出 JSON 失败: %v", err)
	}
	// 备份包里带一张凭证：recordId 用真实账单 id，路径形状与 backup.go 一致。
	recID := "22222222-2222-2222-2222-222222222222"
	img := pngBytes(t, 4, 4)
	zipBody := buildBackupZip(t, payload, map[string][]byte{
		"images/" + recID + "/33333333-3333-3333-3333-333333333333.png": img,
	})

	// 导入到另一个账号（模拟"服务挂了，从备份恢复"）。
	uploadDir := t.TempDir()
	attach := NewAttachmentService(uploadDir, repos.LedgerAttachments(), repos.Ledger())
	svc := NewExportService(repos.Ledger(), attach)
	target := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	r := newImportRouter(t, svc, target)

	code, resp := postImport(t, r, "/api/v1/ledger/import?mode=merge", "application/zip", zipBody)
	if code != http.StatusOK {
		t.Fatalf("ZIP 导入状态码 %d：%s", code, resp)
	}
	var res ImportResult
	if err := json.Unmarshal([]byte(resp), &res); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if res.RecordsInserted != 2 || res.Imported != 2 {
		t.Fatalf("账单未全部还原: %+v", res)
	}
	if res.AttachmentsRestored != 1 || res.AttachmentsSkipped != 0 {
		t.Fatalf("凭证恢复统计有误: %+v", res)
	}

	// 账单确实落到目标账号。
	if _, err := repos.Ledger().GetRecord(ctx, target, recID); err != nil {
		t.Fatalf("还原后读不到账单: %v", err)
	}
	// 凭证元数据 + 磁盘文件都必须在。
	items, err := repos.LedgerAttachments().ListByRecord(ctx, target, recID)
	if err != nil {
		t.Fatalf("读取凭证失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应恢复 1 个凭证，实际 %d", len(items))
	}
	if items[0].Mime != model.MimePNG || items[0].Bytes != int64(len(img)) {
		t.Fatalf("凭证元数据有误: %+v", items[0])
	}
	disk := filepath.Join(uploadDir, filepath.FromSlash(items[0].RelPath))
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatalf("凭证文件未落盘: %v", err)
	}
	if !bytes.Equal(got, img) {
		t.Fatalf("落盘的凭证字节与备份包不一致")
	}
}

// ------------------------------------------------------------------ 3. zip slip 必须被拒

func TestImportZIPRejectsZipSlip(t *testing.T) {
	repos, cfgSvc, _ := newTestEnv(t, false)
	_, payload, err := cfgSvc.BuildJSON(context.Background(), testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出 JSON 失败: %v", err)
	}
	ledgerJSON, _ := json.Marshal(payload)
	manifest, _ := json.Marshal(map[string]any{
		"schema": backupSchema, "version": exportSchemaVersion,
		// 故意把穿越条目也登记进清单：校验必须靠路径规则拦住它，而不是靠清单。
		"files": []string{"ledger.json", "manifest.json", "../evil.txt"},
	})
	body := buildZip(t, []zipItem{
		{name: "ledger.json", data: ledgerJSON},
		{name: "manifest.json", data: manifest},
		{name: "../evil.txt", data: []byte("pwned")},
	})

	uploadDir := t.TempDir()
	attach := NewAttachmentService(uploadDir, repos.LedgerAttachments(), repos.Ledger())
	svc := NewExportService(repos.Ledger(), attach)
	target := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	r := newImportRouter(t, svc, target)

	code, resp := postImport(t, r, "/api/v1/ledger/import?mode=merge", "application/zip", body)
	if code != http.StatusBadRequest {
		t.Fatalf("含路径穿越的备份包应 400，实际 %d：%s", code, resp)
	}
	if !strings.Contains(resp, "路径穿越") {
		t.Fatalf("错误文案未说明路径穿越: %s", resp)
	}
	// 拒绝要彻底：一条都不写，也不产生任何落盘文件。
	list, _, err := repos.Ledger().ListRecords(context.Background(), target, model.LedgerFilter{Page: 1, Size: 100})
	if err != nil {
		t.Fatalf("读取账单失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("被拒绝的备份包不得写入任何账单，实际 %d 条", len(list))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(uploadDir), "evil.txt")); err == nil {
		t.Fatalf("路径穿越条目不应被写出")
	}
}

// ------------------------------------------------------------------ 4. 损坏 ZIP → 400

func TestImportZIPRejectsBrokenArchive(t *testing.T) {
	repos, _, _ := newTestEnv(t, false)
	svc := NewExportService(repos.Ledger(), nil)
	target := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	r := newImportRouter(t, svc, target)

	cases := []struct {
		name string
		body []byte
		ct   string
	}{
		{"伪造魔数", []byte("PK\x03\x04not-a-real-zip"), "application/zip"},
		{"空 ZIP（无 ledger.json）", buildZip(t, []zipItem{{name: "readme.txt", data: []byte("hi")}}), "application/zip"},
		{"octet-stream 乱字节", []byte("\x00\x01\x02\x03"), "application/octet-stream"},
	}
	for _, tc := range cases {
		code, resp := postImport(t, r, "/api/v1/ledger/import?mode=merge", tc.ct, tc.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s：应 400，实际 %d：%s", tc.name, code, resp)
		}
	}
}

// ------------------------------------------------------------------ 5. B-07：ZIP 放行 / JSON 拦截

// seedAttachedRecord 给已存在的账单挂一张凭证，让 guardAttachments 在覆盖导入时必然命中。
//
// 为什么必须预置：guardAttachments 靠"快照里有多少凭证元数据"判定，账本里没有凭证时
// 它会直接放行——那样两条用例都会假绿（测不到"放行"也测不到"拦截"）。
// 凭证元数据直接写仓储而不走 AttachmentService.Save，是为了不依赖磁盘落盘。
func seedAttachedRecord(t *testing.T, repos *fake.Repos, recordID string) *model.LedgerAttachment {
	t.Helper()
	img := pngBytes(t, 4, 4)
	a := &model.LedgerAttachment{
		ID: "77777777-7777-7777-7777-777777777777", UserID: testUser, RecordID: recordID,
		RelPath: "ledger/" + testUser + "/7777.png", Mime: model.MimePNG, Bytes: int64(len(img)),
	}
	if err := repos.LedgerAttachments().Create(context.Background(), a); err != nil {
		t.Fatalf("预置凭证失败: %v", err)
	}
	return a
}

// TestImportZIPOverwriteBypassesAttachmentGuard 锁定 ZIP 覆盖导入**不被** guardAttachments 挡住。
//
// 语义来源：team-lead 在 ImportZIP 里把 confirm 兼作 force（ZIP 自带 images/ 凭证，
// 覆盖属于"完整还原"；若再要求 force，会把用户逼向不带图片的 JSON 覆盖）。
// 这条用例的价值在于"注释挡不住回退"：如果哪天有人把 Force 改回 false，
// 这里会立刻变红，而不是等线上发现"导出 ZIP 再还原"被 409 挡住。
func TestImportZIPOverwriteBypassesAttachmentGuard(t *testing.T) {
	repos, cfgSvc, _ := newTestEnv(t, false)
	ctx := context.Background()
	recID := "11111111-1111-1111-1111-111111111111"
	seeded := seedAttachedRecord(t, repos, recID)

	// 凭证仓储必须真的挂在 ExportService.repo 上（生产环境两者是同一个值），
	// 否则 snapshotAttachments 走降级分支返回 nil，guard 永不命中，用例等于没测。
	svc := NewExportService(
		attachAwareRepo{LedgerRepo: repos.Ledger(), LedgerAttachmentRepo: repos.LedgerAttachments()},
		NewAttachmentService(t.TempDir(), repos.LedgerAttachments(), repos.Ledger()),
	)
	snap, err := svc.snapshot(ctx, testUser)
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	if len(snap.attachments) != 1 {
		t.Fatalf("快照未读到预置凭证，guard 不会命中，本用例无意义: %+v", snap.attachments)
	}

	_, payload, err := cfgSvc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出 JSON 失败: %v", err)
	}
	// ZIP 内的 recordId 必须与预置凭证的账单一致，才能测到"凭证被还原"。
	zipBody := buildBackupZip(t, payload, map[string][]byte{
		"images/" + recID + "/" + seeded.ID + ".png": pngBytes(t, 4, 4),
	})

	r := newImportRouter(t, svc, testUser)
	code, resp := postImport(t, r, "/api/v1/ledger/import?mode=overwrite&confirm=true", "application/zip", zipBody)
	if code != http.StatusOK {
		t.Fatalf("ZIP 覆盖导入应被放行（confirm 兼作 force），实际 %d：%s", code, resp)
	}
	var res ImportResult
	if err := json.Unmarshal([]byte(resp), &res); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if res.AttachmentsRestored < 1 {
		t.Fatalf("凭证未从 ZIP 还原（只放行不还原等于什么都没做）: %+v", res)
	}
	// 覆盖必须真的生效：账本条数等于 ZIP 内条数，且不是"两条旧的还在 + 新的又加一遍"。
	list, total, err := repos.Ledger().ListRecords(ctx, testUser, model.LedgerFilter{Page: 1, Size: 100})
	if err != nil {
		t.Fatalf("读取账单失败: %v", err)
	}
	if len(list) != len(payload.Records) || int(total) != len(payload.Records) {
		t.Fatalf("覆盖后账单条数应为 %d，实际 %d（total=%d）", len(payload.Records), len(list), total)
	}
}

// TestOverwriteJSONWithoutForceStillBlocked 是上一条的负向对照：JSON 覆盖**仍然**被拦截。
//
// 两条一起才锁得住语义：ZIP 放行（凭证字节随包还原）、JSON 拦截（只有清单、覆盖即永久丢图）。
// 只加正向用例的话，有人把 guardAttachments 整个删掉也不会被发现。
func TestOverwriteJSONWithoutForceStillBlocked(t *testing.T) {
	repos, cfgSvc, _ := newTestEnv(t, false)
	ctx := context.Background()
	recID := "11111111-1111-1111-1111-111111111111"
	seedAttachedRecord(t, repos, recID)

	svc := NewExportService(
		attachAwareRepo{LedgerRepo: repos.Ledger(), LedgerAttachmentRepo: repos.LedgerAttachments()},
		nil,
	)
	_, payload, err := cfgSvc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出 JSON 失败: %v", err)
	}
	body, err := json.Marshal(ImportRequest{Mode: ImportModeOverwrite, Payload: payload, Confirm: true})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}

	r := newImportRouter(t, svc, testUser)
	code, resp := postImport(t, r, "/api/v1/ledger/import", "application/json", body)
	if code != http.StatusConflict {
		t.Fatalf("JSON 覆盖且未传 force 应为 409，实际 %d：%s", code, resp)
	}
	if !strings.Contains(resp, "凭证") {
		t.Fatalf("409 文案应说明凭证风险：%s", resp)
	}
	// 被拒绝 = 什么都没做：账单与凭证元数据都必须在。
	list, _, err := repos.Ledger().ListRecords(ctx, testUser, model.LedgerFilter{Page: 1, Size: 100})
	if err != nil {
		t.Fatalf("读取账单失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("被拒绝的覆盖导入不应删除账单，实际剩 %d 条", len(list))
	}
	kept, err := repos.LedgerAttachments().ListByRecord(ctx, testUser, recID)
	if err != nil || len(kept) != 1 {
		t.Fatalf("被拒绝的覆盖导入不应删除凭证：%v / %+v", err, kept)
	}
}
