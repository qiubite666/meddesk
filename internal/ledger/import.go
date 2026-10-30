package ledger

// 记账本的导入与恢复：合并（merge）与覆盖（overwrite）。
//
// 【安全底线】
//  1. 每条记录的 user_id 一律改写成当前会话账号——payload 里的归属字段完全不信任，
//     否则一份"别人导出的备份"就能把数据写进他人账号（越权写入）。
//  2. 覆盖导入是破坏性操作，必须 confirm=true 才执行；执行前先把当前数据快照到内存，
//     中途失败就整体回滚，绝不留下"删了一半"的账本。
//  3. 附件只做登记：AttachmentSource 只有读取能力，导入方无法凭它写回文件，
//     因此明细里明确报告"有多少凭证未被恢复"，不让用户误以为恢复完整。

import (
	"context"
	"errors"
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
type ImportResult struct {
	Mode               string   `json:"mode"`
	RecordsBefore      int      `json:"recordsBefore"`
	RecordsAfter       int      `json:"recordsAfter"`
	RecordsInserted    int      `json:"recordsInserted"`
	RecordsSkipped     int      `json:"recordsSkipped"`
	RecordsFailed      int      `json:"recordsFailed"`
	CategoriesCreated  int      `json:"categoriesCreated"`
	AttachmentsTotal   int      `json:"attachmentsTotal"`
	AttachmentsSkipped int      `json:"attachmentsSkipped"`
	Note               string   `json:"note,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

// Import 执行导入。任何结构性错误都在写库之前返回 400，不产生部分写入。
func (s *ExportService) Import(ctx context.Context, userID string, req ImportRequest) (*ImportResult, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	mode := strings.TrimSpace(req.Mode)
	switch mode {
	case "":
		return nil, apierr.New(apierr.CodeBadRequest, "请指定导入模式：merge（合并）或 overwrite（覆盖）")
	case ImportModeMerge, ImportModeOverwrite:
	default:
		return nil, apierr.New(apierr.CodeBadRequest, "导入模式只能是 merge 或 overwrite")
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
