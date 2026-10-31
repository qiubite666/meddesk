package ledger

// 覆盖导入的凭证保护（B-07）与凭证配额（B-26）的单元测试。
//
// 【为什么单独一个文件】这两条都是"拒绝型"行为：前者挡住会丢凭证的覆盖导入，
// 后者挡住超量上传。它们不依赖真实数据库，也不需要附件服务落盘，
// 因此单独成文件，避免与导出/导入的既有用例互相牵扯。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/fake"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// attachAwareRepo 把账本仓储与凭证仓储拼成一个值。
//
// 生产环境里两者是同一个 ledgerRepo（方法分散在两个文件），因此 ExportService 的
// 类型断言能成功；测试里 fake 把它们拆成了两个类型，这里按生产语义重新组合，
// 才能验证"凭证快照 + 前置保护"这条链路。
type attachAwareRepo struct {
	store.LedgerRepo
	store.LedgerAttachmentRepo
}

// TestOverwriteImportGuardOnAttachments 验证 B-07：
//  1. 快照会把凭证元数据一起带上（修复前只快照分类与账单，回滚后凭证全丢）；
//  2. 账本里已有凭证时，覆盖导入默认 409 拒绝，且一行数据都不动；
//  3. 显式 force=true 才放行。
func TestOverwriteImportGuardOnAttachments(t *testing.T) {
	repos, _, _ := newTestEnv(t, false)
	ctx := context.Background()

	attRepo := repos.LedgerAttachments()
	recID := "22222222-2222-2222-2222-222222222222"
	png := pngBytes(t, 4, 4)
	if err := attRepo.Create(ctx, &model.LedgerAttachment{
		ID: "55555555-5555-5555-5555-555555555555", UserID: testUser, RecordID: recID,
		RelPath: "ledger/" + testUser + "/5555.png", Mime: model.MimePNG, Bytes: int64(len(png)),
	}); err != nil {
		t.Fatalf("播种凭证失败: %v", err)
	}

	svc := NewExportService(attachAwareRepo{LedgerRepo: repos.Ledger(), LedgerAttachmentRepo: attRepo}, nil)
	svc.now = func() time.Time { return fixedNow }

	// 1) 快照必须包含凭证（这是回滚能救回凭证的前提）。
	snap, err := svc.snapshot(ctx, testUser)
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	if len(snap.attachments) != 1 || snap.attachments[0].RelPath == "" {
		t.Fatalf("快照未收集凭证元数据（覆盖导入后无法恢复）：%+v", snap.attachments)
	}

	_, payload, err := svc.BuildJSON(ctx, testUser, ExportFilter{})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	// 2) 默认拒绝：宁可挡住，也不静默删掉不可恢复的凭证。
	_, err = svc.Import(ctx, testUser, ImportRequest{Mode: ImportModeOverwrite, Payload: payload, Confirm: true})
	if err == nil {
		t.Fatal("账本含凭证时，覆盖导入应被拒绝")
	}
	if !apierr.Is(err, apierr.CodeConflict) {
		t.Fatalf("应为 409，实际 %v", err)
	}
	if msg := apierr.FromError(err).Message; !strings.Contains(msg, "凭证") || !strings.Contains(msg, "force") {
		t.Fatalf("错误文案应说明凭证风险与 force 出口：%q", msg)
	}
	// 拒绝必须是"什么都没做"：账单与凭证都还在。
	list, _, err := repos.Ledger().ListRecords(ctx, testUser, model.LedgerFilter{Page: 1, Size: 100})
	if err != nil {
		t.Fatalf("读取账单失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("被拒绝的覆盖导入不应改动账单，实际剩 %d 条", len(list))
	}
	kept, err := attRepo.ListByRecord(ctx, testUser, recID)
	if err != nil || len(kept) != 1 {
		t.Fatalf("被拒绝的覆盖导入不应改动凭证：%v / %+v", err, kept)
	}

	// 3) 显式 force 才放行（用户已自行备份、确认丢弃凭证）。
	res, err := svc.Import(ctx, testUser, ImportRequest{Mode: ImportModeOverwrite, Payload: payload, Confirm: true, Force: true})
	if err != nil {
		t.Fatalf("force=true 时覆盖导入应放行: %v", err)
	}
	if res.RecordsInserted != 2 || res.RecordsAfter != 2 {
		t.Fatalf("force 覆盖导入统计有误: %+v", res)
	}
}

// TestAttachmentQuotaPerRecord 验证 B-26：单条账单最多 9 张凭证。
func TestAttachmentQuotaPerRecord(t *testing.T) {
	repos := fake.New()
	ctx := context.Background()
	repo := repos.Ledger()
	if err := repo.EnsureCategories(ctx, testUser); err != nil {
		t.Fatalf("播种分类失败: %v", err)
	}
	now := time.Now().In(shanghai)
	rec := &model.LedgerRecord{
		ID: "66666666-6666-6666-6666-666666666666", UserID: testUser, Type: model.LedgerTypeExpense,
		Amount: "1.00", AmountCents: 100, Currency: "CNY", CategoryName: "餐饮",
		TransactionAt: now, OccurredOn: now.Format("2006-01-02"), ReimbursementStatus: model.LedgerReimbNone,
	}
	if err := repo.CreateRecord(ctx, rec); err != nil {
		t.Fatalf("播种账单失败: %v", err)
	}

	svc := NewAttachmentService(t.TempDir(), repos.LedgerAttachments(), repo)
	img := pngBytes(t, 4, 4)
	for i := 0; i < maxAttachmentsPerRecord; i++ {
		if _, err := svc.Save(ctx, testUser, rec.ID, img); err != nil {
			t.Fatalf("第 %d 张凭证应允许上传: %v", i+1, err)
		}
	}
	// 第 10 张必须被拒，且不能"先落盘再报错"。
	_, err := svc.Save(ctx, testUser, rec.ID, img)
	if err == nil {
		t.Fatalf("超过 %d 张后仍允许上传", maxAttachmentsPerRecord)
	}
	if !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("超配额应为 400，实际 %v", err)
	}
	if msg := apierr.FromError(err).Message; !strings.Contains(msg, "最多") {
		t.Fatalf("错误文案应说明张数上限：%q", msg)
	}
	items, err := repos.LedgerAttachments().ListByRecord(ctx, testUser, rec.ID)
	if err != nil {
		t.Fatalf("读取凭证失败: %v", err)
	}
	if len(items) != maxAttachmentsPerRecord {
		t.Fatalf("被拒绝的上传不应写入元数据，实际 %d 张", len(items))
	}
}
