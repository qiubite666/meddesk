package ledger

// 记账本每日备份的单元测试。
//
// 不需要真实 WebDAV：用 httptest.NewTLSServer 起一个只处理 PROPFIND / PUT / MKCOL
// 的假服务（TLS 是因为 webdav 包强制 https；IP 拦截策略在测试里放行回环地址）。
// 这样"上传成功 / 远端校验存在 / 重复执行幂等 / 凭据加解密 / 未配置密钥时拒绝 /
// manifest 校验和一致"都能在离线环境里验证。

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
	"github.com/qiubite666/meddesk/internal/webdav"
)

// 测试专用标识：全是占位值，不含任何真实凭据。
const (
	bkUserID  = "11111111-2222-3333-4444-555555555555"
	bkOtherID = "99999999-8888-7777-6666-555555555555"
	bkDavUser = "bk-tester"
	bkDavPass = "placeholder-not-a-real-secret"
	bkSecret  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // base64(32 个 0x00)
)

// ------------------------------------------------------------------ 假的 WebDAV 服务

// bkFakeDav 是内存实现的极简 WebDAV 服务（Basic Auth + PROPFIND/PUT/MKCOL）。
type bkFakeDav struct {
	mu    sync.Mutex
	dirs  map[string]bool
	files map[string][]byte
	puts  map[string]int // 路径 -> PUT 次数（用于验证幂等覆盖而不是新建）
}

func newBkFakeDav() *bkFakeDav {
	return &bkFakeDav{
		dirs:  map[string]bool{"": true, "dav": true},
		files: map[string][]byte{},
		puts:  map[string]int{},
	}
}

func (d *bkFakeDav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != bkDavUser || pass != bkDavPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dav"), "/")

	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Method {
	case "PROPFIND":
		if !d.dirs[rel] && !d.exists(rel) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`+
			`<D:response><D:href>/dav/`+rel+`</D:href><D:propstat><D:prop>`+
			`<D:resourcetype/><D:getcontentlength>`+
			itoa(int64(len(d.files[rel])))+`</D:getcontentlength>`+
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`+
			`</D:multistatus>`)
	case "MKCOL":
		if d.dirs[rel] || d.exists(rel) {
			w.WriteHeader(http.StatusMethodNotAllowed) // 已存在
			return
		}
		if !d.dirs[parentOfBk(rel)] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		d.dirs[rel] = true
		w.WriteHeader(http.StatusCreated)
	case http.MethodPut:
		if !d.dirs[parentOfBk(rel)] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		d.files[rel] = buf.Bytes()
		d.puts[rel]++
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (d *bkFakeDav) exists(rel string) bool { _, ok := d.files[rel]; return ok }

func parentOfBk(rel string) string {
	p := path.Dir(rel)
	if p == "." || p == "/" {
		return ""
	}
	return p
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ------------------------------------------------------------------ 假的仓储与附件

// bkFakeLedger 实现 LedgerReader（只覆盖备份用到的两个方法）。
type bkFakeLedger struct {
	categories []model.LedgerCategory
	records    []model.LedgerRecord
}

func (f *bkFakeLedger) ListCategories(ctx context.Context, userID, kind string, includeArchived bool) ([]model.LedgerCategory, error) {
	return f.categories, nil
}

func (f *bkFakeLedger) ListRecords(ctx context.Context, userID string, flt model.LedgerFilter) ([]model.LedgerRecord, int64, error) {
	size := flt.Size
	if size <= 0 {
		size = 20
	}
	page := flt.Page
	if page < 1 {
		page = 1
	}
	start := (page - 1) * size
	if start > len(f.records) {
		start = len(f.records)
	}
	end := start + size
	if end > len(f.records) {
		end = len(f.records)
	}
	return f.records[start:end], int64(len(f.records)), nil
}

// bkFakeRepo 实现 store.LedgerBackupRepo（内存版，保留 UNIQUE(user_id, run_date) 语义）。
type bkFakeRepo struct {
	mu    sync.Mutex
	creds map[string]*store.WebdavCredentialRow
	runs  []store.LedgerBackupRun
}

func newBkFakeRepo() *bkFakeRepo {
	return &bkFakeRepo{creds: map[string]*store.WebdavCredentialRow{}}
}

func (r *bkFakeRepo) SaveCredentials(ctx context.Context, c *store.WebdavCredentialRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *c
	cp.CreatedAt = time.Now()
	cp.UpdatedAt = cp.CreatedAt
	r.creds[c.UserID] = &cp
	return nil
}

func (r *bkFakeRepo) GetCredentials(ctx context.Context, userID string) (*store.WebdavCredentialRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[userID]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *bkFakeRepo) DeleteCredentials(ctx context.Context, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.creds, userID)
	return nil
}

func (r *bkFakeRepo) ListCredentialUserIDs(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.creds))
	for id := range r.creds {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (r *bkFakeRepo) UpsertRun(ctx context.Context, run *store.LedgerBackupRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run.ID == "" {
		run.ID = "run-" + itoa(int64(len(r.runs)+1))
	}
	now := time.Now()
	run.UpdatedAt = now
	for i := range r.runs {
		// 同一账号同一天只保留一行：与数据库的 UNIQUE(user_id, run_date) 语义一致。
		if r.runs[i].UserID == run.UserID && r.runs[i].RunDate == run.RunDate {
			run.CreatedAt = r.runs[i].CreatedAt
			r.runs[i] = *run
			return nil
		}
	}
	run.CreatedAt = now
	r.runs = append(r.runs, *run)
	return nil
}

func (r *bkFakeRepo) ListRuns(ctx context.Context, userID string, limit int) ([]store.LedgerBackupRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []store.LedgerBackupRun{}
	for _, run := range r.runs {
		if run.UserID == userID {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunDate > out[j].RunDate })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// bkFakeAttach 实现 AttachmentSource：按附件 id 返回内存字节。
type bkFakeAttach struct {
	refs map[string][]AttachmentRef // recordID -> 清单
	data map[string][]byte          // attachmentID -> 字节
}

func (a *bkFakeAttach) Open(ctx context.Context, userID, attachmentID string) (io.ReadCloser, error) {
	b, ok := a.data[attachmentID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (a *bkFakeAttach) ListByRecord(ctx context.Context, userID, recordID string) ([]AttachmentRef, error) {
	return a.refs[recordID], nil
}

// ------------------------------------------------------------------ 测试脚手架

type bkEnv struct {
	svc  *BackupService
	dav  *bkFakeDav
	repo *bkFakeRepo
	base string // WebDAV 根地址
}

// newBkEnv 起一个假 WebDAV 服务并组装备份服务（密钥通过环境变量注入）。
func newBkEnv(t *testing.T, records []model.LedgerRecord, attach *bkFakeAttach) *bkEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("MEDDESK_SECRETBOX_KEY", bkSecret)
	t.Setenv("MEDDESK_LEDGER_BACKUP_TZ", "Asia/Shanghai")
	t.Setenv("MEDDESK_LEDGER_BACKUP_TIME", "02:00")
	t.Setenv("MEDDESK_LEDGER_BACKUP_ENABLED", "1")

	dav := newBkFakeDav()
	srv := httptest.NewTLSServer(dav)
	t.Cleanup(srv.Close)

	// 测试专用客户端：放行回环地址、信任 httptest 的自签证书。
	client := webdav.NewClientForTest(srv.Client(), func(net.IP) bool { return false })
	repo := newBkFakeRepo()
	svc := NewBackupService(BackupDeps{
		Ledger: &bkFakeLedger{
			categories: []model.LedgerCategory{{ID: "c1", UserID: bkUserID, Kind: model.LedgerTypeExpense, Name: "餐饮"}},
			records:    records,
		},
		Backup: repo,
		Remote: client,
		Attach: attach,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &bkEnv{svc: svc, dav: dav, repo: repo, base: srv.URL + "/dav/"}
}

func bkRecord(id string) model.LedgerRecord {
	return model.LedgerRecord{
		ID: id, UserID: bkUserID, Type: model.LedgerTypeExpense, Amount: "12.34", AmountCents: 1234,
		Currency: "CNY", CategoryID: "c1", CategoryName: "餐饮",
		TransactionAt: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), OccurredOn: "2026-10-29",
		Note: "午饭", ReimbursementStatus: model.LedgerReimbNone,
		CreatedAt: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	}
}

func bkSave(t *testing.T, e *bkEnv, pass string) error {
	t.Helper()
	return e.svc.SaveCredentials(context.Background(), bkUserID, CredentialsInput{
		BaseURL: e.base, Username: bkDavUser, Password: pass, PathPrefix: "meddesk/ledger",
	})
}

// ------------------------------------------------------------------ 用例

// TestBackupRunUploadsAndIsIdempotent 覆盖：上传成功、远端校验存在、重复执行不产生第二个文件。
func TestBackupRunUploadsAndIsIdempotent(t *testing.T) {
	e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
	if err := bkSave(t, e, bkDavPass); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}

	first, err := e.svc.RunForUser(context.Background(), bkUserID)
	if err != nil {
		t.Fatalf("首次备份失败: %v", err)
	}
	if first.Status != store.BackupRunSuccess {
		t.Fatalf("首次备份状态应为 success，实际 %s（%s）", first.Status, first.Message)
	}
	wantPath := "meddesk/ledger/ledger-backup-" + e.svc.today() + ".zip"
	if first.RemotePath != wantPath {
		t.Fatalf("远端路径不符：%q != %q", first.RemotePath, wantPath)
	}
	if _, ok := e.dav.files[wantPath]; !ok {
		t.Fatalf("远端没有落盘文件：%v", e.dav.files)
	}

	// 同一天再跑一次：必须覆盖同名文件，而不是新增一个。
	second, err := e.svc.RunForUser(context.Background(), bkUserID)
	if err != nil {
		t.Fatalf("第二次备份失败: %v", err)
	}
	if second.RemotePath != wantPath {
		t.Fatalf("第二次备份路径变了：%q", second.RemotePath)
	}
	if len(e.dav.files) != 1 {
		t.Fatalf("重复执行产生了额外文件：%v", keysOf(e.dav.files))
	}
	if e.dav.puts[wantPath] != 2 {
		t.Fatalf("PUT 次数应为 2（两次覆盖），实际 %d", e.dav.puts[wantPath])
	}
	// 一天一行：运行记录同样幂等。
	runs, err := e.repo.ListRuns(context.Background(), bkUserID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("运行记录应只有一行，实际 %d（err=%v）", len(runs), err)
	}
	if runs[0].Status != store.BackupRunSuccess || runs[0].RecordCount != 1 {
		t.Fatalf("运行记录内容不符: %+v", runs[0])
	}
}

// TestBackupArchiveContent 覆盖：ZIP 内条目、manifest 校验和与图片内容。
func TestBackupArchiveContent(t *testing.T) {
	rec := bkRecord("r1")
	rec.AttachmentCount = 1
	att := &bkFakeAttach{
		refs: map[string][]AttachmentRef{"r1": {{ID: "a1", RecordID: "r1", Mime: "image/jpeg", Ext: "jpg", Bytes: 4}}},
		data: map[string][]byte{"a1": []byte("JPEG")},
	}
	e := newBkEnv(t, []model.LedgerRecord{rec}, att)
	if err := bkSave(t, e, bkDavPass); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}
	run, err := e.svc.RunForUser(context.Background(), bkUserID)
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}

	raw := e.dav.files[run.RemotePath]
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("打开 ZIP 失败: %v", err)
	}
	ledgerJSON := bkReadZip(t, zr, "ledger.json")
	manifestRaw := bkReadZip(t, zr, "manifest.json")

	var mf backupManifest
	if err := json.Unmarshal(manifestRaw, &mf); err != nil {
		t.Fatalf("解析 manifest.json 失败: %v", err)
	}
	sum := sha256.Sum256(ledgerJSON)
	if mf.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest 校验和不一致：%s != %s", mf.Checksum, hex.EncodeToString(sum[:]))
	}
	if mf.RecordCount != 1 || mf.AttachmentCount != 1 {
		t.Fatalf("manifest 计数不符: %+v", mf)
	}
	if mf.Schema != backupSchema || mf.Version != exportSchemaVersion {
		t.Fatalf("manifest schema/version 不符: %s/%d", mf.Schema, mf.Version)
	}
	if mf.GeneratedAt == "" {
		t.Fatalf("manifest 缺少 generatedAt")
	}

	var payload BackupPayload
	if err := json.Unmarshal(ledgerJSON, &payload); err != nil {
		t.Fatalf("解析 ledger.json 失败: %v", err)
	}
	if payload.Schema != backupSchema || payload.Version != exportSchemaVersion {
		t.Fatalf("ledger.json schema/version 不符（应与导入器兼容）: %s/%d", payload.Schema, payload.Version)
	}
	if len(payload.Records) != 1 || len(payload.Categories) != 1 {
		t.Fatalf("ledger.json 内容不符: records=%d categories=%d", len(payload.Records), len(payload.Categories))
	}
	// 图片：路径 images/<recordId>/<attachmentId>.<ext>，字节一致。
	img := bkReadZip(t, zr, "images/r1/a1.jpg")
	if !bytes.Equal(img, []byte("JPEG")) {
		t.Fatalf("图片内容不符: %q", img)
	}
}

// TestCredentialEncryptionRoundTrip 覆盖：加密落库、解密还原、密文不含明文、跨账号不可用。
func TestCredentialEncryptionRoundTrip(t *testing.T) {
	e := newBkEnv(t, nil, nil)
	in := CredentialsInput{BaseURL: e.base, Username: bkDavUser, Password: bkDavPass, PathPrefix: "meddesk/ledger"}
	if err := e.svc.SaveCredentials(context.Background(), bkUserID, in); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}

	row, err := e.repo.GetCredentials(context.Background(), bkUserID)
	if err != nil {
		t.Fatalf("读取凭据密文失败: %v", err)
	}
	if len(row.IV) != 48 {
		t.Fatalf("iv 应为 4×12=48 字节，实际 %d", len(row.IV))
	}
	// 密文里绝不能出现明文口令/用户名。
	for name, ct := range map[string][]byte{"base_url": row.BaseURL, "username": row.Username, "password": row.Password, "prefix": row.PathPrefix} {
		if bytes.Contains(ct, []byte(bkDavPass)) || bytes.Contains(ct, []byte(bkDavUser)) {
			t.Fatalf("%s 密文中出现明文", name)
		}
	}

	got, err := e.svc.LoadCredentials(context.Background(), bkUserID)
	if err != nil {
		t.Fatalf("解密凭据失败: %v", err)
	}
	if got.BaseURL != in.BaseURL || got.Username != in.Username || got.Password != in.Password || got.PathPrefix != in.PathPrefix {
		t.Fatalf("解密结果与原文不符: %+v", got)
	}

	// 把同一份密文挪到另一个账号：AAD 不匹配，必须解密失败（而不是解出别人的凭据）。
	e.repo.creds[bkOtherID] = row
	if _, err := e.svc.creds.load(context.Background(), bkOtherID); err == nil {
		t.Fatalf("密文被挪到别的账号后仍能解密，AAD 绑定失效")
	}

	// 删除后应读不到。
	if err := e.svc.DeleteCredentials(context.Background(), bkUserID); err != nil {
		t.Fatalf("删除凭据失败: %v", err)
	}
	if ok, err := e.svc.creds.configured(context.Background(), bkUserID); err != nil || ok {
		t.Fatalf("删除后 configured 应为 false: ok=%v err=%v", ok, err)
	}
}

// TestSaveCredentialsRequiresConnectivity 覆盖：连通性校验失败时不落库。
func TestSaveCredentialsRequiresConnectivity(t *testing.T) {
	e := newBkEnv(t, nil, nil)
	err := e.svc.SaveCredentials(context.Background(), bkUserID, CredentialsInput{
		BaseURL: e.base, Username: bkDavUser, Password: "wrong-password", PathPrefix: "meddesk/ledger",
	})
	if err == nil {
		t.Fatalf("错误口令应保存失败")
	}
	if !apierr.Is(err, apierr.CodeUnauthorized) {
		t.Fatalf("错误口令应映射为 401，实际 %v", apierr.FromError(err).Code)
	}
	if len(e.repo.creds) != 0 {
		t.Fatalf("校验失败不应落库，实际 %d 行", len(e.repo.creds))
	}
	// 非法路径前缀同样要在保存前被拒。
	if err := e.svc.SaveCredentials(context.Background(), bkUserID, CredentialsInput{
		BaseURL: e.base, Username: bkDavUser, Password: bkDavPass, PathPrefix: "../escape",
	}); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("非法前缀应返回 400，实际 %v", err)
	}
}

// TestBackupDisabledWithoutKey 覆盖：未配置 MEDDESK_SECRETBOX_KEY 时拒绝并提示。
func TestBackupDisabledWithoutKey(t *testing.T) {
	e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
	t.Setenv("MEDDESK_SECRETBOX_KEY", "")

	err := e.svc.SaveCredentials(context.Background(), bkUserID, CredentialsInput{
		BaseURL: e.base, Username: bkDavUser, Password: bkDavPass,
	})
	if !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("缺少密钥时应返回 400，实际 %v", err)
	}
	if strings.Contains(apierr.FromError(err).Message, bkDavPass) {
		t.Fatalf("错误信息泄漏凭据: %s", apierr.FromError(err).Message)
	}

	if _, err := e.svc.RunForUser(context.Background(), bkUserID); !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("缺少密钥时备份应返回 400，实际 %v", err)
	}
	st, err := e.svc.Status(context.Background(), bkUserID, 5)
	if err != nil {
		t.Fatalf("状态查询失败: %v", err)
	}
	if st.Enabled {
		t.Fatalf("缺少密钥时 Enabled 应为 false")
	}
	if st.NextRunAt != "" {
		t.Fatalf("未启用时不应给出下次运行时间: %q", st.NextRunAt)
	}
	// 开关关闭时调度器不启动（这里只验证配置解析）。
	if _, _, err := e.svc.RunAll(context.Background()); err == nil {
		t.Fatalf("缺少密钥时 RunAll 应报错")
	}
}

// TestBackupStatusAndHTTP 覆盖：状态接口返回运行记录，且绝不回传凭据。
func TestBackupStatusAndHTTP(t *testing.T) {
	e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
	if err := bkSave(t, e, bkDavPass); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}
	if _, err := e.svc.RunForUser(context.Background(), bkUserID); err != nil {
		t.Fatalf("备份失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	MountBackup(r.Group("/api/v1"), e.svc, func(c *gin.Context) { c.Set(ctxUserIDKey, bkUserID) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ledger/backup/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, bkDavPass) || strings.Contains(body, bkDavUser) {
		t.Fatalf("状态接口回传了凭据: %s", body)
	}
	var st BackupStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析状态失败: %v", err)
	}
	if !st.Enabled || !st.Configured {
		t.Fatalf("状态不符: enabled=%v configured=%v", st.Enabled, st.Configured)
	}
	if st.ScheduleTime != "02:00" || st.Timezone != "Asia/Shanghai" {
		t.Fatalf("排程配置不符: %+v", st)
	}
	if st.NextRunAt == "" {
		t.Fatalf("缺少下次运行时间")
	}
	if len(st.Runs) != 1 || st.Runs[0].Status != store.BackupRunSuccess {
		t.Fatalf("运行记录不符: %+v", st.Runs)
	}

	// 手动触发
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/ledger/backup/run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("run: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 删除凭据
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/ledger/backup/credentials", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ok, _ := e.svc.creds.configured(context.Background(), bkUserID); ok {
		t.Fatalf("删除后仍显示已配置")
	}
}

// bkLieDav 包装真实客户端：上传照做，但校验时谎称"文件不存在"。
// 用来模拟 PUT 返回 201 而远端实际没落盘（服务端异步写入、配额失败等）的情形。
type bkLieDav struct{ inner RemoteStore }

func (l bkLieDav) Ping(ctx context.Context, creds webdav.Credentials) error {
	return l.inner.Ping(ctx, creds)
}
func (l bkLieDav) Mkdir(ctx context.Context, creds webdav.Credentials, rel string) error {
	return l.inner.Mkdir(ctx, creds, rel)
}
func (l bkLieDav) Upload(ctx context.Context, creds webdav.Credentials, relPath string, body io.Reader, size int64) error {
	return l.inner.Upload(ctx, creds, relPath, body, size)
}
func (l bkLieDav) Exists(ctx context.Context, creds webdav.Credentials, relPath string) (bool, error) {
	return false, nil
}

// TestBackupFailureRecorded 覆盖：远端校验失败要写 failed 并保留可重试的 message。
func TestBackupFailureRecorded(t *testing.T) {
	e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
	if err := bkSave(t, e, bkDavPass); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}
	// 换掉远端：让"上传后校验"这一步失败。
	e.svc.remote = bkLieDav{inner: e.svc.remote}

	if _, err := e.svc.RunForUser(context.Background(), bkUserID); err == nil {
		t.Fatalf("远端校验不到文件时应判失败")
	}
	runs, _ := e.repo.ListRuns(context.Background(), bkUserID, 10)
	if len(runs) != 1 || runs[0].Status != store.BackupRunFailed {
		t.Fatalf("失败状态未落库: %+v", runs)
	}
	if runs[0].Message == "" || strings.Contains(runs[0].Message, bkDavPass) {
		t.Fatalf("message 应保留失败原因且不含凭据: %q", runs[0].Message)
	}

	// 未配置凭据时同样要落 failed（便于前端提示"请先配置"）。
	if err := e.svc.DeleteCredentials(context.Background(), bkUserID); err != nil {
		t.Fatalf("删除凭据失败: %v", err)
	}
	if _, err := e.svc.RunForUser(context.Background(), bkUserID); err == nil {
		t.Fatalf("未配置凭据时备份应失败")
	}
	runs, _ = e.repo.ListRuns(context.Background(), bkUserID, 10)
	if len(runs) != 1 || runs[0].Status != store.BackupRunFailed {
		t.Fatalf("未配置凭据的失败状态未落库: %+v", runs)
	}
}

// TestSchedulerNextRun 覆盖下一次触发时刻的计算（今天已过则顺延到明天）。
func TestSchedulerNextRun(t *testing.T) {
	e := newBkEnv(t, nil, nil)
	t.Setenv("MEDDESK_LEDGER_BACKUP_TIME", "02:00")
	svc := NewBackupService(BackupDeps{
		Ledger: &bkFakeLedger{}, Backup: e.repo, Remote: webdav.NewClient(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	next := svc.NextRun()
	if next.Location().String() != "Asia/Shanghai" && next.Format("2006-01-02") == "" {
		t.Fatalf("时区解析异常: %v", next)
	}
	if next.Hour() != 2 || next.Minute() != 0 {
		t.Fatalf("触发时刻不符: %v", next)
	}
	if !next.After(time.Now()) {
		t.Fatalf("下一次触发必须在未来: %v", next)
	}
	if next.Sub(time.Now()) > 25*time.Hour {
		t.Fatalf("下一次触发不应超过 24 小时: %v", next)
	}
}

// ------------------------------------------------------------------ 工具

func bkReadZip(t *testing.T, zr *zip.Reader, name string) []byte {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", name, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		return b
	}
	t.Fatalf("ZIP 内缺少条目 %s", name)
	return nil
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------------ 并发 / 容错（P1-5、P1-12）

// bkGateDav 在 Upload 处阻塞：每个进入上传阶段的调用先报到（entered），
// 再等内容被放行（release）。用于验证"不同账号能同时进入上传阶段"——
// 如果仍是一把全局锁，第二个账号根本走不到这里。
type bkGateDav struct {
	inner    RemoteStore
	entered  chan string
	release  chan struct{}
	releaseO sync.Once
}

func (g *bkGateDav) open() { g.releaseO.Do(func() { close(g.release) }) }

func (g *bkGateDav) Ping(ctx context.Context, creds webdav.Credentials) error {
	return g.inner.Ping(ctx, creds)
}

func (g *bkGateDav) Mkdir(ctx context.Context, creds webdav.Credentials, rel string) error {
	return g.inner.Mkdir(ctx, creds, rel)
}

func (g *bkGateDav) Upload(ctx context.Context, creds webdav.Credentials, relPath string, body io.Reader, size int64) error {
	g.entered <- relPath
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.inner.Upload(ctx, creds, relPath, body, size)
}

func (g *bkGateDav) Exists(ctx context.Context, creds webdav.Credentials, relPath string) (bool, error) {
	return g.inner.Exists(ctx, creds, relPath)
}

// bkConcurrencyDav 统计 Upload 阶段的并发进入数（当前值与历史峰值）。
type bkConcurrencyDav struct {
	inner RemoteStore
	hold  time.Duration
	cur   atomic.Int32
	max   atomic.Int32
}

func (d *bkConcurrencyDav) Ping(ctx context.Context, creds webdav.Credentials) error {
	return d.inner.Ping(ctx, creds)
}

func (d *bkConcurrencyDav) Mkdir(ctx context.Context, creds webdav.Credentials, rel string) error {
	return d.inner.Mkdir(ctx, creds, rel)
}

func (d *bkConcurrencyDav) Upload(ctx context.Context, creds webdav.Credentials, relPath string, body io.Reader, size int64) error {
	n := d.cur.Add(1)
	for {
		m := d.max.Load()
		if n <= m || d.max.CompareAndSwap(m, n) {
			break
		}
	}
	defer d.cur.Add(-1)
	time.Sleep(d.hold)
	return d.inner.Upload(ctx, creds, relPath, body, size)
}

func (d *bkConcurrencyDav) Exists(ctx context.Context, creds webdav.Credentials, relPath string) (bool, error) {
	return d.inner.Exists(ctx, creds, relPath)
}

// bkPanicDav 在 Upload 处 panic：模拟打包/上传路径中的未预期崩溃。
type bkPanicDav struct{ inner RemoteStore }

func (p bkPanicDav) Ping(ctx context.Context, creds webdav.Credentials) error {
	return p.inner.Ping(ctx, creds)
}

func (p bkPanicDav) Mkdir(ctx context.Context, creds webdav.Credentials, rel string) error {
	return p.inner.Mkdir(ctx, creds, rel)
}

func (p bkPanicDav) Upload(ctx context.Context, creds webdav.Credentials, relPath string, body io.Reader, size int64) error {
	panic("模拟远端驱动 panic")
}

func (p bkPanicDav) Exists(ctx context.Context, creds webdav.Credentials, relPath string) (bool, error) {
	return p.inner.Exists(ctx, creds, relPath)
}

// bkSaveOther 为第二个账号保存一份独立凭据（复用同一台假 WebDAV）。
func bkSaveOther(t *testing.T, e *bkEnv) {
	t.Helper()
	if err := e.svc.SaveCredentials(context.Background(), bkOtherID, CredentialsInput{
		BaseURL: e.base, Username: bkDavUser, Password: bkDavPass, PathPrefix: "meddesk/ledger",
	}); err != nil {
		t.Fatalf("保存第二个账号凭据失败: %v", err)
	}
}

// TestBackupRunForUserParallelAcrossUsers 覆盖 P1-5：不同账号的备份必须能并行。
//
// 做法：两个账号各自并发 RunForUser，远端在 Upload 处"报到后等放行"。
// 若锁仍是进程级全局锁，第二个账号会卡在锁上，报到的只会有一个人 → 超时失败。
func TestBackupRunForUserParallelAcrossUsers(t *testing.T) {
	e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
	if err := bkSave(t, e, bkDavPass); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}
	bkSaveOther(t, e)

	gate := &bkGateDav{inner: e.svc.remote, entered: make(chan string, 2), release: make(chan struct{})}
	e.svc.remote = gate
	defer gate.open() // 失败路径也放行，避免 goroutine 泄漏

	started := time.Now()
	done := make(chan error, 2)
	for _, id := range []string{bkUserID, bkOtherID} {
		userID := id
		go func() {
			_, err := e.svc.RunForUser(context.Background(), userID)
			done <- err
		}()
	}

	deadline := time.After(10 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-gate.entered:
		case <-deadline:
			t.Fatalf("只有 %d 个账号进入上传阶段：不同账号仍被串行化（锁仍是全局锁）", i)
		}
	}
	elapsed := time.Since(started)
	gate.open()

	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并行备份失败: %v", err)
		}
	}
	// 两者都在对方被放行前就进入了上传，总耗时必然远小于"串行两次上传"。
	if elapsed > 5*time.Second {
		t.Fatalf("两个账号进入上传耗时 %v，疑似串行", elapsed)
	}
}

// TestBackupRunForUserSerialPerUser 覆盖 P1-5 的另一半：同一账号必须严格互斥。
//
// 计数器记录 Upload 阶段的最大并发数：同一 userID 并发两次，峰值必须为 1。
func TestBackupRunForUserSerialPerUser(t *testing.T) {
	e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
	if err := bkSave(t, e, bkDavPass); err != nil {
		t.Fatalf("保存凭据失败: %v", err)
	}
	cnt := &bkConcurrencyDav{inner: e.svc.remote, hold: 120 * time.Millisecond}
	e.svc.remote = cnt

	begin := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-begin
			_, err := e.svc.RunForUser(context.Background(), bkUserID)
			done <- err
		}()
	}
	close(begin) // 尽量让两次调用同时起跑

	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("同账号备份失败: %v", err)
		}
	}
	if got := cnt.max.Load(); got != 1 {
		t.Fatalf("同一账号上传阶段并发峰值应为 1，实际 %d（分片锁未生效）", got)
	}
	// 两次都成功且都真的上传了（串行不代表被吞掉一次）。
	runs, err := e.repo.ListRuns(context.Background(), bkUserID, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != store.BackupRunSuccess {
		t.Fatalf("运行记录不符: %+v（err=%v）", runs, err)
	}
}

// TestSchedulerRecoversFromPanic 覆盖 P1-12：备份 panic 不能杀掉进程，且下一轮仍可执行。
func TestSchedulerRecoversFromPanic(t *testing.T) {
	t.Run("单次备份 panic 后调度继续", func(t *testing.T) {
		e := newBkEnv(t, []model.LedgerRecord{bkRecord("r1")}, nil)
		if err := bkSave(t, e, bkDavPass); err != nil {
			t.Fatalf("保存凭据失败: %v", err)
		}
		working := e.svc.remote
		s := NewScheduler(e.svc, e.svc.log)

		// 注入会 panic 的远端：runAllSafe 必须兜住，测试进程不能崩。
		e.svc.remote = bkPanicDav{inner: working}
		s.runAllSafe(context.Background())

		// 关键：panic 之后同一账号的锁必须已被释放，下一轮备份仍能正常跑完。
		e.svc.remote = working
		s.runAllSafe(context.Background())

		runs, err := e.repo.ListRuns(context.Background(), bkUserID, 10)
		if err != nil {
			t.Fatalf("读取运行记录失败: %v", err)
		}
		if len(runs) != 1 || runs[0].Status != store.BackupRunSuccess {
			t.Fatalf("panic 后下一轮备份未成功执行: %+v", runs)
		}
	})

	t.Run("调度协程入口 panic 被兜住", func(t *testing.T) {
		e := newBkEnv(t, nil, nil)
		s := NewScheduler(e.svc, e.svc.log)
		// 让 loop 的第一步（NextRun）就 panic：loc 为 nil 时 time.In 会 panic。
		// safeLoop 未做 recover 的话，这里会直接终止整个测试进程。
		e.svc.cfg.loc = nil
		s.safeLoop(context.Background())
	})
}
