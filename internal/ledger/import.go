package ledger

// 记账本的导入与恢复：合并（merge）与覆盖（overwrite）。
//
// 【安全底线】
//  1. 每条记录的 user_id 一律改写成当前会话账号——payload 里的归属字段完全不信任，
//     否则一份"别人导出的备份"就能把数据写进他人账号（越权写入）。
//  2. 覆盖导入是破坏性操作，必须 confirm=true 才执行；执行前先把当前数据快照到内存，
//     中途失败就整体回滚，绝不留下"删了一半"的账本。
//  3. JSON 导入只登记附件：AttachmentSource 只有读取能力，导入方无法凭它写回文件，
//     因此明细里明确报告"有多少凭证未被恢复"，不让用户误以为恢复完整。
//     例外是每日备份 ZIP（ImportZIP）：它自带原图字节，附件服务若支持写入
//     （AttachmentService 实现了 Save）就真的落盘还原。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// 导入模式。
const (
	ImportModeMerge     = "merge"
	ImportModeOverwrite = "overwrite"
)

// maxImportRecords 是单次导入的条数上限（与导出上限一致，防止超大 payload 拖垮请求）。
const maxImportRecords = maxExportRecords

// ImportRequest 是导入请求体。
type ImportRequest struct {
	Mode    string         `json:"mode"`    // merge | overwrite
	Payload *BackupPayload `json:"payload"` // 与导出格式一致
	Confirm bool           `json:"confirm"` // 覆盖导入必须显式确认
}

// ImportResult 是导入统计。
//
// Imported / Skipped 是 RecordsInserted / RecordsSkipped 的等价别名：
// 备份包客户端（以及"从 WebDAV 拖回文件直接导入"的脚本）只关心这两项，
// 原有字段全部保留，保持向后兼容。
type ImportResult struct {
	Mode               string `json:"mode"`
	RecordsBefore      int    `json:"recordsBefore"`
	RecordsAfter       int    `json:"recordsAfter"`
	RecordsInserted    int    `json:"recordsInserted"`
	RecordsSkipped     int    `json:"recordsSkipped"`
	RecordsFailed      int    `json:"recordsFailed"`
	CategoriesCreated  int    `json:"categoriesCreated"`
	AttachmentsTotal   int    `json:"attachmentsTotal"`
	AttachmentsSkipped int    `json:"attachmentsSkipped"`
	Imported           int    `json:"imported"`
	Skipped            int    `json:"skipped"`
	// AttachmentsRestored 是真正落盘并登记成功的凭证数量（仅 ZIP 备份包导入可能 > 0）。
	AttachmentsRestored int `json:"attachmentsRestored"`
	// Warnings 是"不影响结果但用户该知道"的提示（ZIP 包结构、被跳过的凭证等）。
	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

// normalizeImportMode 校验导入模式；"" 表示用户没选，需要更明确的提示。
func normalizeImportMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "":
		return "", apierr.New(apierr.CodeBadRequest, "请指定导入模式：merge（合并）或 overwrite（覆盖）")
	case ImportModeMerge, ImportModeOverwrite:
		return strings.TrimSpace(mode), nil
	default:
		return "", apierr.New(apierr.CodeBadRequest, "导入模式只能是 merge 或 overwrite")
	}
}

// Import 执行导入。任何结构性错误都在写库之前返回 400，不产生部分写入。
func (s *ExportService) Import(ctx context.Context, userID string, req ImportRequest) (*ImportResult, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	mode, err := normalizeImportMode(req.Mode)
	if err != nil {
		return nil, err
	}
	if req.Payload == nil {
		return nil, apierr.New(apierr.CodeBadRequest, "缺少 payload（备份内容）")
	}
	// 覆盖是破坏性操作：必须显式确认，防止前端误触或重放请求清空账本。
	if mode == ImportModeOverwrite && !req.Confirm {
		return nil, apierr.New(apierr.CodeBadRequest, "覆盖导入会清空当前账本数据，请确认后重试（confirm=true）")
	}

	cats, recs, err := validatePayload(req.Payload)
	if err != nil {
		return nil, err
	}

	res := &ImportResult{
		Mode:             mode,
		AttachmentsTotal: len(req.Payload.Attachments),
	}
	if len(req.Payload.Attachments) > 0 {
		res.AttachmentsSkipped = len(req.Payload.Attachments)
		res.Note = "附件字节不会通过导入恢复（附件模块只提供读取接口），请在原设备上保留凭证图片"
	}

	before, err := s.listRecords(ctx, userID, model.LedgerFilter{}, 0)
	if err != nil {
		return nil, err
	}
	res.RecordsBefore = len(before)

	if mode == ImportModeMerge {
		if err := s.importMerge(ctx, userID, cats, recs, res); err != nil {
			return nil, err
		}
	} else {
		if err := s.importOverwrite(ctx, userID, cats, recs, res); err != nil {
			return nil, err
		}
	}

	after, err := s.listRecords(ctx, userID, model.LedgerFilter{}, 0)
	if err != nil {
		return nil, err
	}
	res.RecordsAfter = len(after)
	res.Imported = res.RecordsInserted
	res.Skipped = res.RecordsSkipped
	return res, nil
}

// ------------------------------------------------------------------ 校验

// validatePayload 校验备份结构并归一化成可写库的对象。
//
// 为什么整批校验通过后才写库：导入中途报错会留下"导了一半"的脏数据，
// 而校验是纯内存操作，先跑完最稳妥——发现第 3 条有问题就直接 400，一行都不写。
func validatePayload(p *BackupPayload) ([]model.LedgerCategory, []model.LedgerRecord, error) {
	schema := strings.TrimSpace(p.Schema)
	if schema == "" {
		return nil, nil, apierr.New(apierr.CodeBadRequest, "备份文件缺少 schema 标识，无法确认格式")
	}
	// B7 的每日备份与本导出共用同一套结构，都接受；其余一律拒收（宁可拒绝也不瞎猜）。
	if schema != exportSchema && schema != backupSchema {
		return nil, nil, apierr.Newf(apierr.CodeBadRequest, "不支持的备份格式 %q，当前仅支持 %s", schema, exportSchema)
	}
	if p.Version != exportSchemaVersion {
		return nil, nil, apierr.Newf(apierr.CodeBadRequest, "备份版本 %d 不受支持，当前支持版本 %d", p.Version, exportSchemaVersion)
	}
	if len(p.Records) == 0 {
		// 空备份导入没有任何意义，覆盖模式下更是会直接清空账本——明确拒绝。
		return nil, nil, apierr.New(apierr.CodeBadRequest, "备份里没有账单记录，已取消导入")
	}
	if len(p.Records) > maxImportRecords {
		return nil, nil, apierr.Newf(apierr.CodeBadRequest, "备份含 %d 条账单，超过单次导入上限 %d", len(p.Records), maxImportRecords)
	}

	cats := make([]model.LedgerCategory, 0, len(p.Categories))
	seenCat := map[string]bool{}
	for i, c := range p.Categories {
		kind := strings.TrimSpace(c.Kind)
		if kind != model.LedgerTypeIncome && kind != model.LedgerTypeExpense {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 个分类的 kind 非法（应为 income 或 expense）", i+1)
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 个分类缺少名称", i+1)
		}
		if len([]rune(name)) > 32 {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 个分类名称超过 32 个字", i+1)
		}
		key := kind + "|" + name
		if seenCat[key] {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "备份内存在重复分类：%s", name)
		}
		seenCat[key] = true
		cats = append(cats, model.LedgerCategory{
			ID: strings.TrimSpace(c.ID), Kind: kind, Name: name,
			SortOrder: c.SortOrder, Archived: c.Archived,
		})
	}

	// 分类 id → 名称，供"记录只带 categoryId"时兜底取快照名。
	catNameByID := map[string]string{}
	catKindByID := map[string]string{}
	for _, c := range cats {
		if c.ID != "" {
			catNameByID[c.ID] = c.Name
			catKindByID[c.ID] = c.Kind
		}
	}

	recs := make([]model.LedgerRecord, 0, len(p.Records))
	seenRec := map[string]bool{}
	for i, r := range p.Records {
		no := i + 1
		typ := strings.TrimSpace(r.Type)
		if typ != model.LedgerTypeIncome && typ != model.LedgerTypeExpense {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的 type 非法（应为 income 或 expense）", no)
		}
		// 金额：走与新增账单同一套校验（字符串 + 整数分，绝不经过浮点）。
		amount, cents, err := model.ParseAmount(r.Amount)
		if err != nil {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的金额 %q 非法：必须是大于 0 的数字，最多两位小数", no, r.Amount)
		}
		if r.AmountCents != 0 && r.AmountCents != cents {
			// 分值与文本对不上说明文件被改过或在传输中损坏，直接拒收而不是"猜一个"。
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的金额与分值不一致（%s / %d）", no, amount, r.AmountCents)
		}
		currency := strings.ToUpper(strings.TrimSpace(r.Currency))
		if currency == "" {
			currency = "CNY"
		}
		if len(currency) != 3 {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的币种非法（应为 3 位代码，如 CNY）", no)
		}
		categoryName := strings.TrimSpace(r.CategoryName)
		if categoryName == "" {
			categoryName = catNameByID[strings.TrimSpace(r.CategoryID)]
		}
		if categoryName == "" {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录缺少分类名", no)
		}
		if len([]rune(categoryName)) > 32 {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的分类名超过 32 个字", no)
		}
		if kind, ok := catKindByID[strings.TrimSpace(r.CategoryID)]; ok && kind != typ {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的分类（%s）与账单类型（%s）不匹配", no, categoryName, typ)
		}
		transactionAt, err := parseBackupTime(r.TransactionAt, r.OccurredOn)
		if err != nil {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的 transactionAt 无法解析", no)
		}
		status := strings.TrimSpace(r.ReimbursementStatus)
		if status == "" || typ == model.LedgerTypeIncome {
			status = model.LedgerReimbNone // 收入恒为 none（契约 1）
		}
		if status != model.LedgerReimbNone && status != model.LedgerReimbPending && status != model.LedgerReimbDone {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的报销状态非法（应为 none / pending / done）", no)
		}
		note := strings.TrimSpace(r.Note)
		if len([]rune(note)) > 500 {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "第 %d 条记录的备注超过 500 个字", no)
		}
		id := strings.TrimSpace(r.ID)
		if id == "" {
			// 备份里没有 id（例如手工整理的数据）就现场生成一个，保证 merge 去重仍然可用。
			id = uuid.NewString()
		}
		if seenRec[id] {
			return nil, nil, apierr.Newf(apierr.CodeBadRequest, "备份内存在重复账单 id：%s", id)
		}
		seenRec[id] = true

		recs = append(recs, model.LedgerRecord{
			ID:                  id,
			Type:                typ,
			Amount:              amount,
			AmountCents:         cents,
			Currency:            currency,
			CategoryName:        categoryName,
			TransactionAt:       transactionAt,
			OccurredOn:          transactionAt.In(shanghai).Format("2006-01-02"),
			Note:                note,
			ReimbursementStatus: status,
		})
	}
	return cats, recs, nil
}

// parseBackupTime 解析备份中的时间：优先 transactionAt，缺失时退回 occurredOn（当天零点）。
func parseBackupTime(transactionAt, occurredOn string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(transactionAt), shanghai); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(occurredOn), shanghai); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("时间格式无法识别")
}

// ------------------------------------------------------------------ 合并导入

// importMerge 按 id 去重导入：本地已存在同 id 账单则跳过（不覆盖本地修改）。
func (s *ExportService) importMerge(ctx context.Context, userID string, cats []model.LedgerCategory, recs []model.LedgerRecord, res *ImportResult) error {
	cache, err := newCatCache(ctx, s, userID)
	if err != nil {
		return err
	}
	for _, r := range recs {
		rec := r
		rec.UserID = userID // 强制归属当前账号：payload 里的 user_id 从不采信
		// 去重：id 已存在说明本地已有这条（可能已被用户改过），保持本地版本。
		if _, err := s.repo.GetRecord(ctx, userID, rec.ID); err == nil {
			res.RecordsSkipped++
			continue
		}
		catID, created, err := cache.resolve(ctx, rec.Type, rec.CategoryName)
		if err != nil {
			res.RecordsFailed++
			res.addErr(fmtErr("写入分类失败", rec.CategoryName, err))
			continue
		}
		if created {
			res.CategoriesCreated++
		}
		rec.CategoryID = catID
		if err := s.repo.CreateRecord(ctx, &rec); err != nil {
			// 并发 merge 同一份数据时，两个请求可能都通过了上面的"已存在"检查，
			// 随后各自 INSERT，后到的撞主键。这不是数据错误：该条已由另一次导入写入，
			// 结果集里它就是"已存在"，应计 skipped 而不是 failed——否则用户会看到
			// 几十条"写入账单失败"，误以为数据丢了（实测并发 3 次导入 100 条时出现 65 条误报）。
			if errors.Is(err, store.ErrConflict) {
				res.RecordsSkipped++
				continue
			}
			res.RecordsFailed++
			res.addErr(fmtErr("写入账单失败", rec.ID, err))
			continue
		}
		res.RecordsInserted++
	}
	return nil
}

// fmtErr 收敛错误信息：只暴露"哪一步、哪个对象"，不回显底层细节（可能含 SQL 片段）。
func fmtErr(step, object string, err error) string {
	msg := step + "（" + object + "）"
	if e, ok := err.(*apierr.Error); ok {
		return msg + "：" + e.Message
	}
	return msg
}

func (r *ImportResult) addErr(msg string) {
	if len(r.Errors) >= 10 {
		return // 只保留前 10 条，避免错误列表把响应体撑爆
	}
	r.Errors = append(r.Errors, msg)
}

// addWarning 记录一条不影响结果的提示（同样只留前 10 条，避免响应体被撑爆）。
func (r *ImportResult) addWarning(msg string) {
	if len(r.Warnings) >= 10 {
		return
	}
	r.Warnings = append(r.Warnings, msg)
}

// ------------------------------------------------------------------ 覆盖导入

// ledgerSnapshot 是覆盖导入前的内存快照，用于失败回滚。
type ledgerSnapshot struct {
	categories []model.LedgerCategory
	records    []model.LedgerRecord
}

// snapshot 读出当前账号的全部分类与账单。
func (s *ExportService) snapshot(ctx context.Context, userID string) (*ledgerSnapshot, error) {
	cats, err := s.repo.ListCategories(ctx, userID, "", true)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取分类失败")
	}
	recs, err := s.listRecords(ctx, userID, model.LedgerFilter{}, 0)
	if err != nil {
		return nil, err
	}
	return &ledgerSnapshot{categories: cats, records: recs}, nil
}

// importOverwrite 清空当前账本后整批写入备份内容；中途失败则回滚到快照。
func (s *ExportService) importOverwrite(ctx context.Context, userID string, cats []model.LedgerCategory, recs []model.LedgerRecord, res *ImportResult) error {
	snap, err := s.snapshot(ctx, userID)
	if err != nil {
		return err
	}
	// 1) 清空：账单先删（分类被引用时不允许删除），分类后删。
	if err := s.clearAll(ctx, userID, snap); err != nil {
		// 清空就没完成：立刻尝试还原，避免账本停留在"删了一半"的状态。
		if rbErr := s.restore(ctx, userID, snap); rbErr != nil {
			return apierr.Wrap(apierr.CodeInternal, rbErr, "覆盖导入失败，且恢复原数据未完全成功，请核对账本")
		}
		return apierr.Wrap(apierr.CodeInternal, err, "覆盖导入失败，已恢复原数据")
	}

	// 2) 写入备份内容：分类先建（保留备份里的 id），账单随后按名解析分类。
	cache, err := newCatCache(ctx, s, userID)
	if err != nil {
		return s.rollbackOverwrite(ctx, userID, snap, err)
	}
	for _, c := range cats {
		cat := c
		cat.UserID = userID
		if cat.ID == "" {
			cat.ID = uuid.NewString()
		}
		// 已在备份清单里的分类直接建（保留原 id），重复时按名命中即可。
		if err := s.repo.CreateCategory(ctx, &cat); err != nil {
			if isConflict(err) {
				continue // 同名分类已存在（多为清空遗漏），按名解析时会命中
			}
			return s.rollbackOverwrite(ctx, userID, snap, err)
		}
		res.CategoriesCreated++
	}
	for _, r := range recs {
		rec := r
		rec.UserID = userID
		catID, created, err := cache.resolve(ctx, rec.Type, rec.CategoryName)
		if err != nil {
			return s.rollbackOverwrite(ctx, userID, snap, err)
		}
		if created {
			res.CategoriesCreated++
		}
		rec.CategoryID = catID
		if err := s.repo.CreateRecord(ctx, &rec); err != nil {
			return s.rollbackOverwrite(ctx, userID, snap, err)
		}
		res.RecordsInserted++
	}
	return nil
}

// rollbackOverwrite 覆盖导入失败时的统一回滚入口。
func (s *ExportService) rollbackOverwrite(ctx context.Context, userID string, snap *ledgerSnapshot, cause error) error {
	if rbErr := s.restore(ctx, userID, snap); rbErr != nil {
		return apierr.Wrap(apierr.CodeInternal, rbErr,
			"覆盖导入失败（"+errText(cause)+"），且回滚未完全成功，请核对账本数据")
	}
	return apierr.Wrap(apierr.CodeInternal, cause, "覆盖导入失败，已回滚到导入前的状态")
}

// errText 取出对外可展示的错误文案。
func errText(err error) string {
	if e, ok := err.(*apierr.Error); ok {
		return e.Message
	}
	return "数据写入失败"
}

// clearAll 删除快照中登记的全部账单与分类。
func (s *ExportService) clearAll(ctx context.Context, userID string, snap *ledgerSnapshot) error {
	for _, r := range snap.records {
		if err := s.repo.DeleteRecord(ctx, userID, r.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return apierr.Wrap(apierr.CodeInternal, err, "清空原账单失败")
		}
	}
	for _, c := range snap.categories {
		if err := s.repo.DeleteCategory(ctx, userID, c.ID); err != nil {
			// 分类删除失败（多半是还有账单引用它）不阻断流程：
			// 账单已经先删掉了，残留的空分类在后续按名解析时会被复用，不影响正确性。
			continue
		}
	}
	return nil
}

// restore 把快照原样写回（先清空当前状态，再按原 id 重建）。
func (s *ExportService) restore(ctx context.Context, userID string, snap *ledgerSnapshot) error {
	cur, err := s.snapshot(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.clearAll(ctx, userID, cur); err != nil {
		return err
	}
	for _, c := range snap.categories {
		cat := c
		cat.UserID = userID
		if err := s.repo.CreateCategory(ctx, &cat); err != nil && !isConflict(err) {
			return apierr.Wrap(apierr.CodeInternal, err, "恢复原分类失败")
		}
	}
	for _, r := range snap.records {
		rec := r
		rec.UserID = userID
		if err := s.repo.CreateRecord(ctx, &rec); err != nil {
			return apierr.Wrap(apierr.CodeInternal, err, "恢复原账单失败")
		}
	}
	return nil
}

// isConflict 判断是否为"同名/同 id 已存在"这类冲突（可安全忽略）。
func isConflict(err error) bool {
	return errors.Is(err, store.ErrConflict) || apierr.Is(err, apierr.CodeConflict)
}

// ------------------------------------------------------------------ 分类解析

// catCache 缓存账号下的分类，避免逐条账单都查一次库。
type catCache struct {
	svc    *ExportService
	userID string
	byKey  map[string]string // kind|name -> id
}

// newCatCache 建立分类缓存（含已停用的分类：历史账单的分类名可能已经停用）。
func newCatCache(ctx context.Context, s *ExportService, userID string) (*catCache, error) {
	c := &catCache{svc: s, userID: userID, byKey: map[string]string{}}
	if err := c.reload(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *catCache) reload(ctx context.Context) error {
	list, err := c.svc.repo.ListCategories(ctx, c.userID, "", true)
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "读取分类失败")
	}
	c.byKey = map[string]string{}
	for _, cat := range list {
		c.byKey[c.key(cat.Kind, cat.Name)] = cat.ID
	}
	return nil
}

func (c *catCache) key(kind, name string) string { return kind + "|" + strings.TrimSpace(name) }

// resolve 返回分类 id；不存在时新建（created=true）。
func (c *catCache) resolve(ctx context.Context, kind, name string) (id string, created bool, err error) {
	if id, ok := c.byKey[c.key(kind, name)]; ok {
		return id, false, nil
	}
	cat := &model.LedgerCategory{
		UserID: c.userID, Kind: kind, Name: strings.TrimSpace(name), SortOrder: 100 + len(c.byKey),
	}
	if err := c.svc.repo.CreateCategory(ctx, cat); err != nil {
		// 冲突说明库里其实已经有了（缓存未覆盖到，例如刚被并发创建）：重新加载再取一次。
		if isConflict(err) {
			if rErr := c.reload(ctx); rErr != nil {
				return "", false, rErr
			}
			if id, ok := c.byKey[c.key(kind, name)]; ok {
				return id, false, nil
			}
		}
		return "", false, err
	}
	c.byKey[c.key(kind, name)] = cat.ID
	return cat.ID, true, nil
}

// ------------------------------------------------------------------ 备份 ZIP 包导入

// ZIP 备份包导入的规模上限。
//
// 【为什么必须限制】备份包是用户上传的**不受信任输入**，一个 1 KiB 的压缩包可以
// 解压出十几 GB，因此不能只信 manifest 声明的大小，必须按实际读到的字节数记账：
//   - maxZipEntries 挡"条目数爆炸"（每条目都要建 Reader、做路径校验）；
//   - maxZipUncompressed 挡 zip bomb（解压后总量）；
//   - maxZipLedgerBytes 单独限制 ledger.json，避免一个超大 JSON 直接吃光内存；
//   - maxZipAttachmentBytes 与附件模块的单张上限一致（2 MiB），超限的凭证跳过并计数。
const (
	maxZipEntries         = 5000
	maxZipUncompressed    = 200 << 20 // 200 MiB
	maxZipLedgerBytes     = 32 << 20  // 32 MiB
	maxZipAttachmentBytes = maxAttachmentBytes
)

// zipEntryPrefixImages 是备份包里附件目录的前缀（backup.go: imagePath）。
const zipEntryPrefixImages = "images/"

var (
	// errUnsafeZipEntry 表示条目名像路径穿越（zip slip），整个包一律拒收。
	errUnsafeZipEntry = errors.New("备份包内存在非法路径条目")
	// errZipTooLarge 表示解压后的字节数超出预算（zip bomb 防护）。
	errZipTooLarge = errors.New("备份包解压后超出大小上限")
)

// zipBudget 是解压字节预算：每次读取都从预算里扣，扣不动就拒绝。
//
// 为什么不用 zip.File.UncompressedSize64 判断：那是打包方写进头部的值，
// 可以被伪造；只有实际读出的字节数才是真的。
type zipBudget struct{ left int64 }

func (b *zipBudget) read(f *zip.File, limit int64) ([]byte, error) {
	if b.left <= 0 {
		return nil, errZipTooLarge
	}
	if limit > b.left {
		limit = b.left
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	// 多读 1 字节用于判定超限：既不把超大条目整块读进内存，也能区分"正好达上限"。
	buf, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, errZipTooLarge
	}
	b.left -= int64(len(buf))
	return buf, nil
}

// zipEntryName 校验并规范化 ZIP 条目名（zip slip 的第一道闸）。
//
// 判定顺序：先拒空名/空字节/反斜杠/绝对路径，再 path.Clean 后逐段检查 ".."，
// 最后要求规范化结果不以 "../" 开头。任何一条不满足就拒收整个包——
// 因为备份包由我们自己生成，出现这类条目只可能是构造出来的攻击载荷。
func zipEntryName(raw string) (string, error) {
	if raw == "" || strings.IndexByte(raw, 0) >= 0 {
		return "", errUnsafeZipEntry
	}
	// 反斜杠在 Windows 解压器里是分隔符，是最常见的穿越载体，直接拒绝。
	if strings.Contains(raw, `\`) {
		return "", errUnsafeZipEntry
	}
	if strings.HasPrefix(raw, "/") || path.IsAbs(raw) {
		return "", errUnsafeZipEntry
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", errUnsafeZipEntry
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", errUnsafeZipEntry
	}
	return clean, nil
}

// zipManifest 只取恢复所需的 manifest.json 字段（两种形态共用）。
type zipManifest struct {
	Schema       string          `json:"schema"`
	Checksum     string          `json:"checksum"`
	ChecksumAlgo string          `json:"checksumAlgo"`
	Files        json.RawMessage `json:"files"`
}

// manifestFileNames 解析 manifest.files。
//
// 两种形态都要认：每日备份（backup.go）是字符串数组，
// 导出包（export.go 的 exportManifest）是 {path,bytes,sha256} 对象数组。
func manifestFileNames(raw []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(trimmed, &names); err == nil {
		out := make([]string, 0, len(names))
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
		return out, nil
	}
	var items []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Path = strings.TrimSpace(it.Path); it.Path != "" {
			out = append(out, it.Path)
		}
	}
	return out, nil
}

// ImportZIP 直接导入一份每日备份 ZIP（ledger-backup-YYYY-MM-DD.zip）。
//
// 设计目标就是用户那句"服务挂了，从 WebDAV 拖下备份文件直接导入就能用"：
// 解包 → 取 ledger.json → 复用 Import 的全部合并/覆盖与回滚逻辑 →
// 再把 images/<recordId>/<attachmentId>.<ext> 还原成附件记录并落盘。
//
// 顺序刻意是"先全量解析校验、再写库"：ZIP 结构、路径、体积、JSON 都在写库之前判定，
// 任何一项不合格都返回 400 且一行数据都不写；写库之后的失败仍由 Import 的快照回滚兜住。
func (s *ExportService) ImportZIP(ctx context.Context, userID string, body []byte, mode string, confirm bool) (*ImportResult, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	mode, err := normalizeImportMode(mode)
	if err != nil {
		return nil, err
	}
	if mode == ImportModeOverwrite && !confirm {
		return nil, apierr.New(apierr.CodeBadRequest, "覆盖导入会清空当前账本数据，请确认后重试（confirm=true）")
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeBadRequest, err, "备份压缩包无法解析，请确认上传的是 ledger-backup-YYYY-MM-DD.zip")
	}
	if len(zr.File) > maxZipEntries {
		return nil, apierr.Newf(apierr.CodeBadRequest, "备份包含 %d 个条目，超过上限 %d", len(zr.File), maxZipEntries)
	}

	budget := &zipBudget{left: maxZipUncompressed}
	files := map[string]*zip.File{} // 规范化条目名 -> 条目
	declared := map[string]bool{}   // manifest.files 列出的条目
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		name, nameErr := zipEntryName(f.Name)
		if nameErr != nil {
			return nil, apierr.Newf(apierr.CodeBadRequest, "备份包内存在非法路径条目（疑似路径穿越）：%q", f.Name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if _, dup := files[name]; dup {
			return nil, apierr.Newf(apierr.CodeBadRequest, "备份包内存在重复条目：%s", name)
		}
		files[name] = f
		names = append(names, name)
	}

	// manifest.json 是"允许清单"：只有列在 files 里的条目才会被读取。
	mf, ok := files["manifest.json"]
	if !ok {
		return nil, apierr.New(apierr.CodeBadRequest, "备份包缺少 manifest.json，无法确认内容清单")
	}
	manifestBytes, err := budget.read(mf, maxZipLedgerBytes)
	if err != nil {
		return nil, zipReadErr(err, "manifest.json")
	}
	var manifest zipManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, apierr.Wrap(apierr.CodeBadRequest, err, "备份包的 manifest.json 无法解析")
	}
	listed, err := manifestFileNames(manifest.Files)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeBadRequest, err, "备份包的 manifest.json 中 files 字段无法解析")
	}
	for _, n := range listed {
		if clean, nameErr := zipEntryName(n); nameErr == nil {
			declared[clean] = true
		}
	}

	lj, ok := files["ledger.json"]
	if !ok {
		return nil, apierr.New(apierr.CodeBadRequest,
			"备份包不含 ledger.json（如果这是导出包，请改用 JSON 备份导入）")
	}
	if len(declared) > 0 && !declared["ledger.json"] {
		return nil, apierr.New(apierr.CodeBadRequest, "manifest.json 未登记 ledger.json，备份包可能不完整")
	}
	ledgerBytes, err := budget.read(lj, maxZipLedgerBytes)
	if err != nil {
		return nil, zipReadErr(err, "ledger.json")
	}

	warnings := []string{}
	// 校验和只覆盖 ledger.json；对不上说明文件被手工改过或传输损坏。
	// 不直接拒收：结构校验已经能挡住非法数据，手工整理后再导入是合理需求，只提示。
	if sum := strings.TrimSpace(manifest.Checksum); sum != "" {
		algo := strings.ToLower(strings.TrimSpace(manifest.ChecksumAlgo))
		if algo == "" || algo == "sha256" {
			got := sha256.Sum256(ledgerBytes)
			if !strings.EqualFold(hex.EncodeToString(got[:]), sum) {
				warnings = append(warnings,
					"ledger.json 的 sha256 与 manifest.json 不一致（文件可能被手工修改或传输损坏），已按结构校验继续")
			}
		}
	}

	var payload BackupPayload
	if err := json.Unmarshal(ledgerBytes, &payload); err != nil {
		return nil, apierr.Wrap(apierr.CodeBadRequest, err, "备份包的 ledger.json 无法解析")
	}

	known := make(map[string]bool, len(payload.Records))
	for _, r := range payload.Records {
		if id := strings.TrimSpace(r.ID); id != "" {
			known[id] = true
		}
	}
	images := imageEntries(names, declared)

	// merge 模式下"本地已存在"的账单不会重写，其凭证也不重复恢复（否则会越导越多）。
	// 必须在 Import 之前取快照：导入之后就分不清哪些是本地原有的了。
	existing := map[string]bool{}
	if mode == ImportModeMerge && len(images) > 0 {
		if before, listErr := s.listRecords(ctx, userID, model.LedgerFilter{}, 0); listErr == nil {
			for _, r := range before {
				existing[r.ID] = true
			}
		}
	}

	// 账单与分类走与 JSON 导入完全相同的路径（校验、强制归属、覆盖回滚都在里面）。
	res, err := s.Import(ctx, userID, ImportRequest{Mode: mode, Payload: &payload, Confirm: confirm})
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		res.addWarning(w)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, zipEntryPrefixImages) {
			continue
		}
		if len(declared) > 0 && !declared[name] {
			res.addWarning("备份包内存在 manifest.json 未登记的条目，已忽略：" + name)
		}
	}

	// 附件必须等账单落库之后再还原：附件服务要用账单归属做越权校验。
	// Import 里把附件一律记成"跳过"（JSON 的附件只有清单没有字节），这里重算。
	res.AttachmentsSkipped = 0
	s.restoreZipAttachments(ctx, userID, images, files, budget, known, existing, res)
	if len(images) > 0 {
		res.Note = fmt.Sprintf("凭证恢复 %d 个，跳过 %d 个", res.AttachmentsRestored, res.AttachmentsSkipped)
	}
	return res, nil
}

// imageEntries 取出"已登记、且位于 images/ 下"的条目名（排序保证结果稳定）。
func imageEntries(names []string, declared map[string]bool) []string {
	out := []string{}
	for _, name := range names {
		if !strings.HasPrefix(name, zipEntryPrefixImages) {
			continue
		}
		if len(declared) > 0 && !declared[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// attachmentRestorer 是"能写回凭证"的附件服务能力。
//
// 为什么用类型断言而不是加接口方法：*AttachmentService 已实现 Save
// （落盘 + 嗅探真实类型 + 归属校验 + 写元数据 + 失败回收），它就是我们要复用的实现；
// 而 ExportService 只依赖只读的 AttachmentSource，加一个字段就得改导出模块，
// 断言让"只读替身"（测试、未接入附件模块的部署）照旧工作。
type attachmentRestorer interface {
	Save(ctx context.Context, userID, recordID string, data []byte) (*model.LedgerAttachment, error)
}

// restoreZipAttachments 把 images/ 下的条目写回附件存储并登记元数据。
//
// 归属规则：条目路径第二段是 recordId，必须能在本备份的 ledger.json 里找到对应账单，
// 找不到的图片一律不落地（防止往他人或已删除的账单上挂凭证），只计数并提示。
func (s *ExportService) restoreZipAttachments(ctx context.Context, userID string, images []string,
	files map[string]*zip.File, budget *zipBudget, known, existing map[string]bool, res *ImportResult) {
	if len(images) == 0 {
		return
	}
	restorer, ok := s.attachments.(attachmentRestorer)
	if !ok {
		res.AttachmentsSkipped += len(images)
		res.addWarning("当前部署未接入可写入的附件服务，凭证图片未恢复（账单数据已恢复）")
		return
	}
	for _, name := range images {
		segs := strings.Split(strings.TrimPrefix(name, zipEntryPrefixImages), "/")
		if len(segs) != 2 {
			res.AttachmentsSkipped++
			res.addWarning("凭证条目路径不符合 images/<recordId>/<file> 规则，已跳过：" + name)
			continue
		}
		recordID := segs[0]
		if !known[recordID] {
			res.AttachmentsSkipped++
			res.addWarning("凭证无法归属到备份内的任何账单，已跳过：" + name)
			continue
		}
		if existing[recordID] {
			// merge：账单在本地已存在（用户可能已改过），凭证不重复恢复。
			res.AttachmentsSkipped++
			continue
		}
		f := files[name]
		data, err := budget.read(f, maxZipAttachmentBytes)
		if err != nil {
			res.AttachmentsSkipped++
			if errors.Is(err, errZipTooLarge) && budget.left <= 0 {
				res.addWarning("备份包解压后总量超出上限，剩余凭证未恢复")
				return
			}
			res.addWarning("单张凭证超过上限或无法读取，已跳过：" + name)
			continue
		}
		if _, err := restorer.Save(ctx, userID, recordID, data); err != nil {
			res.AttachmentsSkipped++
			res.addWarning(fmtErr("恢复凭证失败", name, err))
			continue
		}
		res.AttachmentsRestored++
	}
}

// zipReadErr 把解压期错误翻译成对外可读的 400（结构错误一律是用户可修正的输入问题）。
func zipReadErr(err error, name string) error {
	if errors.Is(err, errZipTooLarge) {
		return apierr.Newf(apierr.CodeBadRequest, "备份包解压后超出大小上限（%d 字节），已拒绝导入", int64(maxZipUncompressed))
	}
	return apierr.Wrap(apierr.CodeBadRequest, err, "读取备份包内的 "+name+" 失败")
}
