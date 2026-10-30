package ledger

// 记账本每日全量备份：打包 → 上传 → 远端校验 → 记录状态。
//
// 【文件结构】（契约第 7 节，Excel 待裁决，本期不生成 ledger.xlsx）
//   ledger-backup-YYYY-MM-DD.zip
//   ├── ledger.json        账单与分类（含附件清单）
//   ├── images/<recordId>/<attachmentId>.<ext>
//   └── manifest.json      {schema, version, generatedAt, recordCount, attachmentCount, checksum}
//
// 【幂等】文件名只由"上海时区的当天日期"决定，上传用 PUT 同名覆盖；
// 运行记录用 ledger_backup_runs 的 UNIQUE(user_id, run_date) 做 upsert。
// 因此同一天重复执行不会产生第二个文件、也不会多出第二条记录。
//
// 【安全】日志只输出方法、相对路径、状态码与条数；凭据明文只在调用栈里出现。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
	"github.com/qiubite666/meddesk/internal/webdav"
)

// backupSchema 是每日备份产出的 ledger.json 的格式标识。
//
// 与 B6 的导出结构同构（分类/账单字段名一致），B6 的导入器（import.go）同时接受
// exportSchema 与本标识，因此用户拿每日备份的 ledger.json 也能直接恢复；
// 版本号沿用 exportSchemaVersion，避免同一个结构出现两套版本线。
const backupSchema = "meddesk-ledger-backup"

// 备份文件名的组成部分：ledger-backup-YYYY-MM-DD.zip（契约第 7 节）。
const (
	backupFilePrefix = "ledger-backup-"
	backupFileSuffix = ".zip"

	// backupPageSize 是分页读取账单的页大小（仓储上限 200）。
	backupPageSize = 200
	// backupRunTimeout 是单个账号一次备份的墙钟上限（含打包与上传）。
	backupRunTimeout = 10 * time.Minute
	// backupStatusLimit 是状态接口默认返回的运行记录条数。
	backupStatusLimit = 10
)

// RemoteStore 是备份所需的远端能力（*webdav.Client 天然实现它）。
//
// 抽成接口的原因：备份服务不关心底层是不是 WebDAV，也便于单测注入替身；
// 生产环境传入 webdav.NewClient()。
type RemoteStore interface {
	Ping(ctx context.Context, creds webdav.Credentials) error
	Mkdir(ctx context.Context, creds webdav.Credentials, rel string) error
	Upload(ctx context.Context, creds webdav.Credentials, relPath string, body io.Reader, size int64) error
	Exists(ctx context.Context, creds webdav.Credentials, relPath string) (bool, error)
}

// LedgerReader 是备份读取账本数据所需的最小能力（store.LedgerRepo 的子集）。
//
// 为什么不用 store.LedgerRepo：备份只需要"列出分类 + 分页列出账单"，
// 收窄成接口后单测可以只实现这两个方法，不必补齐整个账本仓储。
// *store.Store.Ledger() 的返回值天然满足它。
type LedgerReader interface {
	ListCategories(ctx context.Context, userID, kind string, includeArchived bool) ([]model.LedgerCategory, error)
	ListRecords(ctx context.Context, userID string, f model.LedgerFilter) ([]model.LedgerRecord, int64, error)
}

// BackupDeps 组装备份服务所需的依赖。
type BackupDeps struct {
	Ledger LedgerReader           // 生产环境传 st.Ledger()
	Backup store.LedgerBackupRepo // 生产环境传 st.LedgerBackup()
	Remote RemoteStore            // 生产环境传 webdav.NewClient()
	// Attach 是凭证附件读取入口（契约由 attachment_contract.go 冻结）。
	// 为 nil 时备份只包含账单与分类，不含图片——B4 未就绪也能安全跑起来。
	Attach AttachmentSource
	Log    *slog.Logger // 为空则用 slog.Default()
}

// BackupService 是记账本备份服务。
type BackupService struct {
	ledger LedgerReader
	backup store.LedgerBackupRepo
	remote RemoteStore
	attach AttachmentSource
	creds  *credentialStore
	log    *slog.Logger
	cfg    backupConfig

	// runMu 让单个进程内的备份串行执行（打包与上传都不适合并发堆叠）。
	// 注意：它**不能**防止多实例重复执行，见 Scheduler 的已知限制说明。
	runMu sync.Mutex
}

// NewBackupService 构造备份服务；配置（时间/时区/开关）在构造时读取一次。
func NewBackupService(d BackupDeps) *BackupService {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &BackupService{
		ledger: d.Ledger,
		backup: d.Backup,
		remote: d.Remote,
		attach: d.Attach,
		creds:  newCredentialStore(d.Backup),
		log:    log,
		cfg:    loadBackupConfig(),
	}
}

// ------------------------------------------------------------------ 凭据

// CredentialsInput 是保存凭据的请求体（只进不出：任何接口都不回传密码）。
type CredentialsInput struct {
	BaseURL    string `json:"baseURL"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PathPrefix string `json:"pathPrefix"`
}

// SaveCredentials 校验连通性后再加密落库；校验失败**不保存**。
//
// 顺序很重要：先用用户提交的凭据真的连一次 WebDAV（PROPFIND），
// 连不上就直接返回错误，避免把"根本不通"的凭据写进数据库——
// 那样只会在凌晨 02:00 的定时任务里反复失败，用户却浑然不觉。
func (s *BackupService) SaveCredentials(ctx context.Context, userID string, in CredentialsInput) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	// 先确认密钥可用：没有密钥就不该让用户以为"保存成功"。
	if _, err := secretBoxKey(); err != nil {
		return err
	}
	creds, err := normalizeCredentials(in)
	if err != nil {
		return err
	}
	dav := davCredentials(creds)

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := s.remote.Ping(callCtx, dav); err != nil {
		// Ping 返回的是 *apierr.Error（401/403/5xx 已有可读文案），原样透出。
		return err
	}
	// 目标目录不存在就顺手建好：备份时再建目录失败会很晚才暴露。
	if creds.PathPrefix != "" {
		if err := s.remote.Mkdir(callCtx, dav, creds.PathPrefix); err != nil {
			return err
		}
	}
	if err := s.creds.save(ctx, userID, *creds); err != nil {
		return err
	}
	s.log.Info("已保存记账本备份凭据", "userConfigured", true)
	return nil
}

// DeleteCredentials 删除已保存的凭据密文（幂等：本来就未配置也返回 nil）。
func (s *BackupService) DeleteCredentials(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	return s.creds.delete(ctx, userID)
}

// normalizeCredentials 校验并归一化输入。
func normalizeCredentials(in CredentialsInput) (*StoredCredentials, error) {
	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "请填写 WebDAV 服务地址")
	}
	username := strings.TrimSpace(in.Username)
	if username == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "请填写 WebDAV 用户名")
	}
	// 口令不 trim：用户可能故意用首尾空格（虽然少见），schema 校验交给 webdav 包。
	if in.Password == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "请填写 WebDAV 密码")
	}
	prefix, err := cleanPathPrefix(in.PathPrefix)
	if err != nil {
		return nil, err
	}
	return &StoredCredentials{BaseURL: baseURL, Username: username, Password: in.Password, PathPrefix: prefix}, nil
}

// cleanPathPrefix 规范化备份子目录：允许 "a/b"，禁止 ".."、反斜杠、绝对路径与空字节。
func cleanPathPrefix(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if strings.IndexByte(p, 0) >= 0 {
		return "", apierr.New(apierr.CodeBadRequest, "备份目录不得包含空字节")
	}
	if strings.Contains(p, `\`) {
		// 反斜杠在部分服务端会被当作分隔符，是穿越的常见载体。
		return "", apierr.New(apierr.CodeBadRequest, "备份目录不得包含反斜杠")
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			return "", apierr.New(apierr.CodeBadRequest, "备份目录不得包含上级目录引用")
		}
	}
	if strings.Contains(p, "..") {
		return "", apierr.New(apierr.CodeBadRequest, "备份目录不得包含上级目录引用")
	}
	return p, nil
}

// davCredentials 转成 webdav 包使用的凭据结构（明文只在此处短暂出现）。
func davCredentials(c *StoredCredentials) webdav.Credentials {
	return webdav.Credentials{BaseURL: c.BaseURL, User: c.Username, Pass: c.Password}
}

// ------------------------------------------------------------------ 备份执行

// RunForUser 为该账号执行一次全量备份。
//
// 无论成功失败都会把结果写进 ledger_backup_runs（失败保留 message，便于重试与排查）。
func (s *BackupService) RunForUser(ctx context.Context, userID string) (*store.LedgerBackupRun, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	// 单实例串行；跨实例重复执行是已知限制（见 Scheduler 注释）。
	s.runMu.Lock()
	defer s.runMu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, backupRunTimeout)
	defer cancel()

	run := &store.LedgerBackupRun{UserID: userID, RunDate: s.today(), Status: store.BackupRunRunning}
	// 先写一行 running：进程中途被杀时，状态接口仍能显示"今天已启动、未成功"。
	if err := s.backup.UpsertRun(runCtx, run); err != nil {
		s.log.Warn("写入备份运行记录失败", "status", run.Status, "error", err)
	}

	creds, err := s.creds.load(runCtx, userID)
	if err != nil {
		return s.finishRun(ctx, run, err)
	}

	archive, err := s.buildArchive(runCtx, userID)
	if err != nil {
		return s.finishRun(ctx, run, err)
	}

	remote := joinRelPath(creds.PathPrefix, archive.fileName)
	dav := davCredentials(creds)
	if err := s.remote.Upload(runCtx, dav, remote, bytes.NewReader(archive.body), int64(len(archive.body))); err != nil {
		return s.finishRun(ctx, run, err)
	}
	// 上传成功必须回查：PUT 返回 201 但远端没落盘的情况（服务端异步写入、配额失败等）
	// 只有回查才能发现，否则用户会以为"每天都在备份"，实际远端空空如也。
	exists, err := s.remote.Exists(runCtx, dav, remote)
	if err != nil {
		return s.finishRun(ctx, run, err)
	}
	if !exists {
		return s.finishRun(ctx, run, apierr.New(apierr.CodeUnavailable, "上传后未能在远端找到备份文件"))
	}

	run.Status = store.BackupRunSuccess
	run.RemotePath = remote
	run.SizeBytes = int64(len(archive.body))
	run.RecordCount = archive.recordCount
	run.Message = ""
	if err := s.backup.UpsertRun(ctx, run); err != nil {
		s.log.Warn("写入备份运行记录失败", "status", run.Status, "error", err)
	}
	s.log.Info("记账本备份完成", "path", remote, "status", run.Status,
		"records", run.RecordCount, "sizeBytes", run.SizeBytes)
	return run, nil
}

// RunForUserID 是 RunForUser 的别名，供调度器按账号遍历（语义相同）。
func (s *BackupService) RunForUserID(ctx context.Context, userID string) error {
	_, err := s.RunForUser(ctx, userID)
	return err
}

// RunAll 为所有已配置凭据的账号各跑一次备份，返回成功数与失败数。
func (s *BackupService) RunAll(ctx context.Context) (ok, failed int, err error) {
	if !s.cfg.Enabled {
		return 0, 0, nil
	}
	// 没有密钥时整体停用：宁可明确告警，也不要让定时任务天天刷失败。
	if _, keyErr := secretBoxKey(); keyErr != nil {
		return 0, 0, keyErr
	}
	ids, err := s.backup.ListCredentialUserIDs(ctx)
	if err != nil {
		return 0, 0, apierr.Wrap(apierr.CodeInternal, err, "读取已配置备份的账号失败")
	}
	for _, id := range ids {
		if err := s.RunForUserID(ctx, id); err != nil {
			failed++
			s.log.Warn("记账本备份失败", "error", apierr.FromError(err).Message)
			continue
		}
		ok++
	}
	return ok, failed, nil
}

// finishRun 把失败结果落库（成功路径单独处理，语义更清楚）。
//
// 用 context.WithoutCancel：触发备份的 ctx 可能已经超时/取消，
// 但"告知用户这次失败了"这件事必须写进去，否则状态永远停在 running。
func (s *BackupService) finishRun(ctx context.Context, run *store.LedgerBackupRun, cause error) (*store.LedgerBackupRun, error) {
	run.Status = store.BackupRunFailed
	run.Message = trimMessage(apierr.FromError(cause).Message)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.backup.UpsertRun(writeCtx, run); err != nil {
		s.log.Warn("写入备份失败状态失败", "error", err)
	}
	s.log.Warn("记账本备份失败", "path", run.RemotePath, "status", run.Status, "message", run.Message)
	return run, cause
}

// trimMessage 截断错误信息：message 会存进数据库，避免超长堆栈/驱动细节污染。
func trimMessage(msg string) string {
	const limit = 500
	msg = strings.TrimSpace(msg)
	if len([]rune(msg)) <= limit {
		return msg
	}
	return string([]rune(msg)[:limit]) + "…"
}

// ------------------------------------------------------------------ 打包

// backupManifest 是 manifest.json 的结构（契约第 7 节）。
type backupManifest struct {
	Schema          string `json:"schema"`
	Version         int    `json:"version"`
	GeneratedAt     string `json:"generatedAt"`
	RecordCount     int    `json:"recordCount"`
	AttachmentCount int    `json:"attachmentCount"`
	Checksum        string `json:"checksum"`     // sha256(ledger.json)，hex 小写
	ChecksumAlgo    string `json:"checksumAlgo"` // 固定 "sha256"，便于恢复侧校验
	// Files 列出 ZIP 内的全部条目（含 images/…），恢复侧可据此核对完整性。
	Files            []string `json:"files"`
	AttachmentFailed int      `json:"attachmentFailed"` // 读取失败的凭证数量（不阻断备份）
}

// backupArchive 是打包结果。
type backupArchive struct {
	body            []byte
	fileName        string
	recordCount     int
	attachmentCount int
}

// buildArchive 读取该账号的全部账单与分类，生成 ZIP 字节流。
func (s *BackupService) buildArchive(ctx context.Context, userID string) (*backupArchive, error) {
	categories, err := s.ledger.ListCategories(ctx, userID, "", true)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取分类失败")
	}
	records, err := s.listAllRecords(ctx, userID)
	if err != nil {
		return nil, err
	}

	now := time.Now().In(s.cfg.loc)
	date := now.Format("2006-01-02")
	generatedAt := now.Format(time.RFC3339)

	// ledger.json 直接复用导出模块的 BackupPayload（schema 用 backupSchema），
	// 这样每日备份与"设置页导出的 JSON 备份"是同一种格式，导入器只需认一套。
	payload := &BackupPayload{
		Schema:      backupSchema,
		Version:     exportSchemaVersion,
		ExportedAt:  generatedAt,
		Categories:  make([]BackupCategory, 0, len(categories)),
		Records:     make([]BackupRecord, 0, len(records)),
		Attachments: []BackupAttachment{},
	}
	for _, c := range categories {
		payload.Categories = append(payload.Categories, BackupCategory{
			ID: c.ID, Kind: c.Kind, Name: c.Name, SortOrder: c.SortOrder, Archived: c.Archived,
		})
	}
	for _, r := range records {
		payload.Records = append(payload.Records, BackupRecord{
			ID: r.ID, Type: r.Type, Amount: r.Amount, AmountCents: r.AmountCents,
			Currency: r.Currency, CategoryID: r.CategoryID, CategoryName: r.CategoryName,
			TransactionAt:       r.TransactionAt.UTC().Format(time.RFC3339),
			OccurredOn:          r.OccurredOn,
			Note:                r.Note,
			ReimbursementStatus: r.ReimbursementStatus,
			AttachmentCount:     r.AttachmentCount,
			CreatedAt:           r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:           r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}

	// 图片按顺序收集：先算好清单（决定 manifest），再写 ZIP。
	images := []struct {
		path string
		data []byte
	}{}
	var missing int
	if s.attach != nil {
		for _, rec := range records {
			if rec.AttachmentCount <= 0 {
				continue
			}
			refs, err := s.attach.ListByRecord(ctx, userID, rec.ID)
			if err != nil {
				// 单条账单的附件读不出来不该让整次备份失败：记数并继续。
				missing++
				s.log.Warn("读取凭证附件清单失败", "recordId", rec.ID, "error", err)
				continue
			}
			for _, ref := range refs {
				rc, err := s.attach.Open(ctx, userID, ref.ID)
				if err != nil {
					missing++
					s.log.Warn("读取凭证附件失败", "attachmentId", ref.ID, "error", err)
					continue
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					missing++
					s.log.Warn("读取凭证附件内容失败", "attachmentId", ref.ID, "error", err)
					continue
				}
				file := imagePath(rec.ID, ref)
				images = append(images, struct {
					path string
					data []byte
				}{path: file, data: data})
				// 字节不内嵌进 JSON（那会让文件膨胀 33%），只登记清单并指明
				// 字节在 ZIP 内的位置；恢复侧配合 manifest.files 即可还原。
				payload.Attachments = append(payload.Attachments, BackupAttachment{
					ID: ref.ID, RecordID: rec.ID, Mime: ref.Mime, Ext: ref.Ext, Bytes: int64(len(data)),
					Included: false, Reason: "字节见 ZIP 内 " + file,
				})
			}
		}
	}

	payloadBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "生成备份数据失败")
	}
	// 校验和只覆盖 ledger.json：它是备份的全部业务数据，
	// 因此"校验和一致"等价于"账单与分类未被改动"（不受 ZIP 元数据影响）。
	sum := sha256.Sum256(payloadBytes)
	files := []string{"ledger.json", "manifest.json"}
	for _, img := range images {
		files = append(files, img.path)
	}
	manifest := backupManifest{
		Schema:           backupSchema,
		Version:          exportSchemaVersion,
		GeneratedAt:      generatedAt,
		RecordCount:      len(records),
		AttachmentCount:  len(images),
		Checksum:         hex.EncodeToString(sum[:]),
		ChecksumAlgo:     "sha256",
		Files:            files,
		AttachmentFailed: missing,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "生成备份清单失败")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// 所有条目的修改时间统一为生成时刻：同一份数据重复打包得到相同字节，
	// 便于用校验和判断"内容有没有变"。
	if err := writeZipEntry(zw, "ledger.json", payloadBytes, now); err != nil {
		return nil, err
	}
	if err := writeZipEntry(zw, "manifest.json", manifestBytes, now); err != nil {
		return nil, err
	}
	for _, img := range images {
		if err := writeZipEntry(zw, img.path, img.data, now); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "打包备份文件失败")
	}
	return &backupArchive{
		body:            buf.Bytes(),
		fileName:        backupFilePrefix + date + backupFileSuffix,
		recordCount:     len(records),
		attachmentCount: len(images),
	}, nil
}

// listAllRecords 分页读取该账号的全部账单（仓储单页上限 200）。
func (s *BackupService) listAllRecords(ctx context.Context, userID string) ([]model.LedgerRecord, error) {
	out := []model.LedgerRecord{}
	for page := 1; ; page++ {
		f := model.LedgerFilter{Page: page, Size: backupPageSize}
		list, total, err := s.ledger.ListRecords(ctx, userID, f)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeInternal, err, "读取账单失败")
		}
		out = append(out, list...)
		if len(list) == 0 || len(out) >= int(total) {
			break
		}
		// 兜底：total 异常（被并发写入改变）时最多再翻若干页，避免死循环。
		if page > 10000 {
			break
		}
	}
	return out, nil
}

// imagePath 生成图片在 ZIP 内的路径：images/<recordId>/<attachmentId>.<ext>。
//
// id 理论上都是 UUID，但仍用 safePathPart 净化一次：异常数据若含 "../"
// 会让 ZIP 条目逃出 images/ 目录，解压时变成路径穿越。
func imagePath(recordID string, ref AttachmentRef) string {
	ext := strings.TrimPrefix(strings.TrimSpace(ref.Ext), ".")
	if ext == "" {
		ext = extFromMime(ref.Mime)
	}
	return "images/" + safePathPart(recordID) + "/" + safePathPart(ref.ID) + "." + safePathPart(ext)
}

// extFromMime 按 MIME 推断扩展名（附件契约里 ext 缺失时的兜底）。
func extFromMime(mime string) string {
	switch strings.TrimSpace(mime) {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/jpeg":
		return "jpg"
	default:
		return "bin"
	}
}

// writeZipEntry 写入一个 ZIP 条目（Deflate 压缩，修改时间统一）。
func writeZipEntry(zw *zip.Writer, name string, data []byte, at time.Time) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at})
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "写入备份条目失败")
	}
	if _, err := w.Write(data); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "写入备份条目失败")
	}
	return nil
}

// joinRelPath 把目录前缀与文件名拼成相对路径（"a/b" + "f.zip" → "a/b/f.zip"）。
func joinRelPath(prefix, name string) string {
	segs := []string{}
	for _, seg := range strings.Split(strings.Trim(prefix, "/"), "/") {
		if seg != "" && seg != "." {
			segs = append(segs, seg)
		}
	}
	segs = append(segs, name)
	return strings.Join(segs, "/")
}

// today 返回配置时区下的今天（决定备份文件名与 run_date）。
func (s *BackupService) today() string { return time.Now().In(s.cfg.loc).Format("2006-01-02") }

// ------------------------------------------------------------------ 状态

// BackupRunView 是运行记录的对外视图（不含任何凭据信息）。
type BackupRunView struct {
	Date        string `json:"date"`
	Status      string `json:"status"`
	RemotePath  string `json:"remotePath"`
	SizeBytes   int64  `json:"sizeBytes"`
	RecordCount int    `json:"recordCount"`
	Message     string `json:"message"`
	UpdatedAt   string `json:"updatedAt"`
}

// BackupStatus 是备份状态接口的响应。
type BackupStatus struct {
	Enabled      bool            `json:"enabled"`      // 备份功能是否启用（开关 + 密钥是否配置）
	Configured   bool            `json:"configured"`   // 该账号是否已保存 WebDAV 凭据
	Timezone     string          `json:"timezone"`     // 例：Asia/Shanghai
	ScheduleTime string          `json:"scheduleTime"` // 例：02:00
	NextRunAt    string          `json:"nextRunAt"`    // RFC3339；未启用时为空串
	Runs         []BackupRunView `json:"runs"`
}

// Status 返回该账号的备份状态与最近运行记录（**绝不回传凭据内容**）。
func (s *BackupService) Status(ctx context.Context, userID string, limit int) (*BackupStatus, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if limit <= 0 {
		limit = backupStatusLimit
	}
	configured, err := s.creds.configured(ctx, userID)
	if err != nil {
		return nil, err
	}
	runs, err := s.backup.ListRuns(ctx, userID, limit)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取备份运行记录失败")
	}
	views := make([]BackupRunView, 0, len(runs))
	for _, r := range runs {
		views = append(views, BackupRunView{
			Date:        r.RunDate,
			Status:      r.Status,
			RemotePath:  r.RemotePath,
			SizeBytes:   r.SizeBytes,
			RecordCount: r.RecordCount,
			Message:     r.Message,
			UpdatedAt:   r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	// 未配置密钥时 Enabled=false：前端据此提示"服务端未开启备份"。
	_, keyErr := secretBoxKey()
	enabled := s.cfg.Enabled && keyErr == nil
	next := ""
	if enabled {
		next = s.NextRun().Format(time.RFC3339)
	}
	return &BackupStatus{
		Enabled:      enabled,
		Configured:   configured,
		Timezone:     s.cfg.TZ,
		ScheduleTime: s.cfg.scheduleText(),
		NextRunAt:    next,
		Runs:         views,
	}, nil
}

// NextRun 返回下一次计划执行时刻（配置时区下的下一个 HH:MM）。
func (s *BackupService) NextRun() time.Time {
	now := time.Now().In(s.cfg.loc)
	next := time.Date(now.Year(), now.Month(), now.Day(), s.cfg.Hour, s.cfg.Minute, 0, 0, s.cfg.loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1) // 今天已过，则顺延到明天同一时刻
	}
	return next
}

// ErrNotConfigured 供上层判断"尚未配置凭据"。
var ErrNotConfigured = apierr.New(apierr.CodeNotFound, "尚未配置 WebDAV 凭据")

// LoadCredentials 解密读取凭据（仅供内部使用：明文不得离开调用栈）。
func (s *BackupService) LoadCredentials(ctx context.Context, userID string) (*StoredCredentials, error) {
	creds, err := s.creds.load(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotConfigured
		}
		return nil, err
	}
	return creds, nil
}
