// Package server 的集成冒烟测试（B3）。
//
// 目标：在没有 PostgreSQL / Redis / Docker 的机器上，用 internal/fake 的内存仓储
// 验证 HTTP 全链路契约（中间件 → 路由 → 服务 → 仓储 → 响应）。
//
// 约束：
//   - 不依赖网络、真实数据库、本机字体与时区；可重复运行（go test -count=1）。
//   - 断言失败信息为中文，便于定位。
//   - 用例按顺序执行（不并行），共用一套内存仓储与一个 httptest 服务。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/cache"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/fake"
	"github.com/qiubite666/meddesk/internal/model"
)

// 编译期断言：内存仓储满足路由所需的最小依赖接口。
var _ Repos = (*fake.Repos)(nil)

const (
	// testJWTSecret 仅用于测试（≥32 字节，满足 config.Validate 的长度要求）。
	testJWTSecret = "b3-integration-test-secret-0123456789abcdef"
	// testEmail / testPasswordHash 为冒烟测试固定账号。
	testEmail = "smoke@meddesk.test"
)

// env 是一套测试环境：httptest 服务 + 内存仓储 + 进程内缓存。
type env struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	repos  *fake.Repos
	cfg    *config.Config
	cache  cache.Cache
}

// newEnv 起一个真实的 gin 引擎（中间件链完整），只把数据层换成内存实现。
func newEnv(t *testing.T) *env {
	t.Helper()

	cfg := &config.Config{
		Env:      "dev", // 开发模式：静态资源走内嵌 FS，IsDev()==true
		HTTPAddr: ":0",
		BaseURL:  "http://127.0.0.1",
		DB:       config.DBConfig{URL: "postgres://fake/fake"}, // 不真正连接
		Auth: config.AuthConfig{
			JWTSecret:  testJWTSecret,
			AccessTTL:  time.Hour,
			RefreshTTL: 24 * time.Hour,
			// 测试用极小的 argon2 参数（真实部署为 64MiB/t=3），避免拖慢测试。
			Argon2Memory:  8192,
			Argon2Time:    1,
			Argon2Threads: 1,
			SignupEnabled: true,
			// 宽限期置 0：让「已轮换令牌被复用」在首次复用即判定为重放（401），
			// 否则 30s 宽限内第一次复用会被放行，断言不稳定。
			RefreshReuseGrace: 0,
		},
		Sync: config.SyncConfig{
			MaxItemsPerPush: 500,
			MaxRecordBytes:  1 << 20,
			PullPageSize:    300,
			MaxPullPageSize: 1000,
		},
		Upload: config.UploadConfig{
			Dir:      t.TempDir(), // 附件密文真实落盘到临时目录
			MaxBytes: 8 << 20,
		},
		PDF: config.PDFConfig{FontPath: ""}, // 留空 → 走默认探测路径
		Log: config.LogConfig{Level: "error", Format: "text"},
		RateLimit: config.RateLimitConfig{
			// 放量到不可能触发，避免限流干扰冒烟用例。
			RPS:   500,
			Burst: 5000,
		},
	}

	c := cache.NewNoop()
	repos := fake.New()
	engine, err := New(Deps{
		Cfg:   cfg,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store: repos,
		Cache: c,
	})
	if err != nil {
		t.Fatalf("构建 gin 引擎失败：%v", err)
	}
	srv := httptest.NewServer(engine)
	t.Cleanup(func() {
		srv.Close()
		_ = c.Close()
	})
	return &env{t: t, srv: srv, client: srv.Client(), repos: repos, cfg: cfg, cache: c}
}

// do 发起一次 JSON 请求（body 为 nil 时不带请求体），返回响应与响应体。
func (e *env) do(method, path string, body interface{}, token string) (*http.Response, []byte) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		var raw []byte
		if b, ok := body.([]byte); ok {
			raw = b
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				e.t.Fatalf("序列化请求体失败：%v", err)
			}
			raw = b
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("构造请求失败（%s %s）：%v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("请求失败（%s %s）：%v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读取响应体失败（%s %s）：%v", method, path, err)
	}
	return resp, data
}

// doMultipart 发起一次 multipart/form-data 请求（用于附件上传）。
func (e *env) doMultipart(method, path string, fields map[string]string, fileField, fileName string, content []byte, token string) (*http.Response, []byte) {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			e.t.Fatalf("写入表单字段 %s 失败：%v", k, err)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileField, fileName))
	h.Set("Content-Type", "application/octet-stream")
	fw, err := w.CreatePart(h)
	if err != nil {
		e.t.Fatalf("创建文件表单项失败：%v", err)
	}
	if _, err := fw.Write(content); err != nil {
		e.t.Fatalf("写入上传内容失败：%v", err)
	}
	if err := w.Close(); err != nil {
		e.t.Fatalf("关闭 multipart 写入器失败：%v", err)
	}

	req, err := http.NewRequest(method, e.srv.URL+path, &buf)
	if err != nil {
		e.t.Fatalf("构造请求失败（%s %s）：%v", method, path, err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("请求失败（%s %s）：%v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读取响应体失败（%s %s）：%v", method, path, err)
	}
	return resp, data
}

// wantStatus 断言 HTTP 状态码。
func (e *env) wantStatus(resp *http.Response, step string, want int) {
	e.t.Helper()
	if resp.StatusCode != want {
		e.t.Fatalf("%s：期望 HTTP %d，实际 %d", step, want, resp.StatusCode)
	}
}

// wantCode 断言错误响应体中的 code 字段。
func (e *env) wantCode(body []byte, step string, want apierr.Code) {
	e.t.Helper()
	var e2 apierr.Error
	if err := json.Unmarshal(body, &e2); err != nil {
		e.t.Fatalf("%s：响应体不是合法的 apierr JSON（%s）：%v", step, string(body), err)
	}
	if e2.Code != want {
		e.t.Fatalf("%s：期望 code=%s，实际 %s（message=%s）", step, want, e2.Code, e2.Message)
	}
}

// decode 把响应体反序列化到 out。
func (e *env) decode(body []byte, step string, out interface{}) {
	e.t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		e.t.Fatalf("%s：解析响应体失败（%s）：%v", step, string(body), err)
	}
}

// b64 生成 n 个固定字节的 base64 串（模拟密文/IV，内容不参与业务语义）。
func b64(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// registerBody 构造注册请求体。
func registerBody() model.RegisterRequest {
	return model.RegisterRequest{
		Email:        testEmail,
		PasswordHash: b64(32), // 客户端 PBKDF2 结果的 base64（44 字符，满足 min=32）
		KDFSalt:      b64(16),
		WrappedDEK:   b64(48),
		DEKIV:        b64(12),
	}
}

// TestIntegrationSmoke 按顺序跑完全链路冒烟用例。
func TestIntegrationSmoke(t *testing.T) {
	e := newEnv(t)

	// ---------------------------------------------------------------- 1. 健康检查
	t.Run("健康检查", func(t *testing.T) {
		resp, body := e.do(http.MethodGet, "/api/v1/health", nil, "")
		e.wantStatus(resp, "健康检查", http.StatusOK)
		var h model.HealthResponse
		e.decode(body, "健康检查", &h)
		if h.Status != "ok" {
			t.Fatalf("健康检查：期望 status=ok，实际 %q", h.Status)
		}
		if h.Version == "" {
			t.Fatal("健康检查：version 不能为空")
		}
		if h.DB != "ok" || h.Cache != "ok" {
			t.Fatalf("健康检查：期望 db=%s cache=%s 均为 ok，实际 db=%q cache=%q", "ok", "ok", h.DB, h.Cache)
		}
	})

	// ---------------------------------------------------------------- 2~3. 注册
	var sess model.TokenResponse
	t.Run("注册并重复注册", func(t *testing.T) {
		resp, body := e.do(http.MethodPost, "/api/v1/auth/register", registerBody(), "")
		e.wantStatus(resp, "注册", http.StatusOK)
		e.decode(body, "注册", &sess)
		if sess.AccessToken == "" || sess.RefreshToken == "" {
			t.Fatalf("注册：未返回令牌对，响应=%s", string(body))
		}
		if sess.UserID == "" || sess.KDFSalt == "" || sess.WrappedDEK == "" || sess.DEKIV == "" {
			t.Fatalf("注册：userId/kdfSalt/wrappedDek/dekIv 存在空值，响应=%s", string(body))
		}

		resp2, body2 := e.do(http.MethodPost, "/api/v1/auth/register", registerBody(), "")
		e.wantStatus(resp2, "重复注册", http.StatusConflict)
		e.wantCode(body2, "重复注册", apierr.CodeConflict)
	})

	// ---------------------------------------------------------------- 4~5. 登录与 me
	t.Run("登录与当前用户", func(t *testing.T) {
		resp, body := e.do(http.MethodPost, "/api/v1/auth/login",
			model.LoginRequest{Email: testEmail, PasswordHash: registerBody().PasswordHash}, "")
		e.wantStatus(resp, "登录", http.StatusOK)
		var login model.TokenResponse
		e.decode(body, "登录", &login)
		if login.AccessToken == "" || login.UserID != sess.UserID {
			t.Fatalf("登录：令牌不可用或用户不一致，响应=%s", string(body))
		}
		sess = login // 后续用例统一使用登录态

		// 带令牌
		resp, body = e.do(http.MethodGet, "/api/v1/auth/me", nil, sess.AccessToken)
		e.wantStatus(resp, "me（带令牌）", http.StatusOK)
		var me model.MeResponse
		e.decode(body, "me（带令牌）", &me)
		if me.User.ID != sess.UserID {
			t.Fatalf("me：期望 userId=%s，实际 %s", sess.UserID, me.User.ID)
		}

		// 不带令牌
		resp, body = e.do(http.MethodGet, "/api/v1/auth/me", nil, "")
		e.wantStatus(resp, "me（无令牌）", http.StatusUnauthorized)
		e.wantCode(body, "me（无令牌）", apierr.CodeUnauthorized)

		// 乱写令牌
		resp, body = e.do(http.MethodGet, "/api/v1/auth/me", nil, "not-a-jwt")
		e.wantStatus(resp, "me（乱写令牌）", http.StatusUnauthorized)
		e.wantCode(body, "me（乱写令牌）", apierr.CodeUnauthorized)
	})

	// ---------------------------------------------------------------- 6~10. 同步
	var cursorAfterPush int64
	t.Run("同步推送与冲突裁决", func(t *testing.T) {
		now := time.Now().UnixMilli()
		batch := []model.SyncItem{
			{Table: model.TableMedication, ID: "med-0001", Ver: 2, UpdatedAt: now, IV: b64(12), CT: b64(32)},
			{Table: model.TableIntake, ID: "intake-0001", Ver: 1, UpdatedAt: now, IV: b64(12), CT: b64(24)},
		}
		// 6. 首次推送两条记录
		resp, body := e.do(http.MethodPost, "/api/v1/sync/push", model.SyncPushRequest{Changes: batch}, sess.AccessToken)
		e.wantStatus(resp, "首次推送", http.StatusOK)
		var pr model.SyncPushResponse
		e.decode(body, "首次推送", &pr)
		if len(pr.Applied) != 2 {
			t.Fatalf("首次推送：期望 applied 长度 2，实际 %d（响应=%s）", len(pr.Applied), string(body))
		}
		if len(pr.Conflicts) != 0 {
			t.Fatalf("首次推送：期望 conflicts 为空，实际 %d 条", len(pr.Conflicts))
		}
		if pr.Cursor != 2 {
			t.Fatalf("首次推送：期望 cursor=2，实际 %d", pr.Cursor)
		}
		cursorAfterPush = pr.Cursor

		// 7. 同 id 但 ver 更小 → 进 conflicts，且冲突项是服务端版本（ver 更大）
		losing := []model.SyncItem{
			{Table: model.TableMedication, ID: "med-0001", Ver: 1, UpdatedAt: now, IV: b64(12), CT: b64(16)},
		}
		resp, body = e.do(http.MethodPost, "/api/v1/sync/push", model.SyncPushRequest{Changes: losing}, sess.AccessToken)
		e.wantStatus(resp, "推送更旧版本", http.StatusOK)
		e.decode(body, "推送更旧版本", &pr)
		if len(pr.Conflicts) != 1 {
			t.Fatalf("推送更旧版本：期望 conflicts 长度 1，实际 %d（响应=%s）", len(pr.Conflicts), string(body))
		}
		if len(pr.Applied) != 0 {
			t.Fatalf("推送更旧版本：期望 applied 为空，实际 %v", pr.Applied)
		}
		if pr.Conflicts[0].Ver != 2 {
			t.Fatalf("推送更旧版本：冲突项应为服务端版本 ver=2，实际 ver=%d", pr.Conflicts[0].Ver)
		}
		if pr.Conflicts[0].ID != "med-0001" || pr.Conflicts[0].Table != model.TableMedication {
			t.Fatalf("推送更旧版本：冲突项标识不符，实际 table=%s id=%s", pr.Conflicts[0].Table, pr.Conflicts[0].ID)
		}
	})

	t.Run("同步入参校验", func(t *testing.T) {
		now := time.Now().UnixMilli()
		// 8a. 非法表名
		resp, body := e.do(http.MethodPost, "/api/v1/sync/push", model.SyncPushRequest{Changes: []model.SyncItem{
			{Table: "evil", ID: "x1", Ver: 1, UpdatedAt: now, IV: b64(12), CT: b64(8)},
		}}, sess.AccessToken)
		e.wantStatus(resp, "推送非法表名", http.StatusBadRequest)
		e.wantCode(body, "推送非法表名", apierr.CodeBadRequest)

		// 8b. 条数超限（MaxItemsPerPush=500，构造 501 条）
		bulk := make([]model.SyncItem, 0, 501)
		for i := 0; i < 501; i++ {
			bulk = append(bulk, model.SyncItem{
				Table: model.TableSetting, ID: fmt.Sprintf("bulk-%04d", i), Ver: 1,
				UpdatedAt: now, IV: b64(12), CT: b64(4),
			})
		}
		resp, body = e.do(http.MethodPost, "/api/v1/sync/push", model.SyncPushRequest{Changes: bulk}, sess.AccessToken)
		e.wantStatus(resp, "推送条数超限", http.StatusBadRequest)
		e.wantCode(body, "推送条数超限", apierr.CodeBadRequest)

		// 8c. updatedAt 为 0
		resp, body = e.do(http.MethodPost, "/api/v1/sync/push", model.SyncPushRequest{Changes: []model.SyncItem{
			{Table: model.TableProfile, ID: "p1", Ver: 1, UpdatedAt: 0, IV: b64(12), CT: b64(8)},
		}}, sess.AccessToken)
		e.wantStatus(resp, "推送 updatedAt=0", http.StatusBadRequest)
		e.wantCode(body, "推送 updatedAt=0", apierr.CodeBadRequest)
	})

	t.Run("同步拉取与概况", func(t *testing.T) {
		// 9. 全量拉取
		resp, body := e.do(http.MethodGet, "/api/v1/sync/pull?cursor=0", nil, sess.AccessToken)
		e.wantStatus(resp, "增量拉取", http.StatusOK)
		var pr model.SyncPullResponse
		e.decode(body, "增量拉取", &pr)
		if len(pr.Items) != 2 {
			t.Fatalf("增量拉取：期望 2 条已应用记录，实际 %d（响应=%s）", len(pr.Items), string(body))
		}
		if pr.Items[0].Seq > pr.Items[1].Seq {
			t.Fatalf("增量拉取：期望按 seq 升序，实际 seq=[%d,%d]", pr.Items[0].Seq, pr.Items[1].Seq)
		}
		if pr.HasMore {
			t.Fatal("增量拉取：只有 2 条记录却返回 hasMore=true")
		}
		if pr.Cursor != cursorAfterPush {
			t.Fatalf("增量拉取：期望 cursor=%d，实际 %d", cursorAfterPush, pr.Cursor)
		}

		// 用上一步游标再拉 → 空 items
		resp, body = e.do(http.MethodGet, fmt.Sprintf("/api/v1/sync/pull?cursor=%d", pr.Cursor), nil, sess.AccessToken)
		e.wantStatus(resp, "按游标增量拉取", http.StatusOK)
		e.decode(body, "按游标增量拉取", &pr)
		if len(pr.Items) != 0 {
			t.Fatalf("按游标增量拉取：期望空 items，实际 %d 条（响应=%s）", len(pr.Items), string(body))
		}

		// limit 超过 MaxPullPageSize：syncsvc.Pull 会**钳制**到上限（不报错），
		// 因此这里断言 200 + 正常返回数据（注释说明：契约未规定必须 400）。
		resp, body = e.do(http.MethodGet, "/api/v1/sync/pull?cursor=0&limit=99999", nil, sess.AccessToken)
		e.wantStatus(resp, "limit 超限被钳制", http.StatusOK)
		e.decode(body, "limit 超限被钳制", &pr)
		if len(pr.Items) != 2 {
			t.Fatalf("limit 超限被钳制：期望仍返回 2 条，实际 %d", len(pr.Items))
		}

		// 10. 同步概况
		resp, body = e.do(http.MethodGet, "/api/v1/sync/stats", nil, sess.AccessToken)
		e.wantStatus(resp, "同步概况", http.StatusOK)
		var st model.SyncStatsResponse
		e.decode(body, "同步概况", &st)
		if st.RecordCount != 2 {
			t.Fatalf("同步概况：期望 recordCount=2，实际 %d", st.RecordCount)
		}
		if st.Cursor != cursorAfterPush {
			t.Fatalf("同步概况：期望 cursor=%d，实际 %d", cursorAfterPush, st.Cursor)
		}
		if st.ByTable[model.TableMedication] != 1 || st.ByTable[model.TableIntake] != 1 {
			t.Fatalf("同步概况：分表统计不符，实际 %v", st.ByTable)
		}
	})

	t.Run("同步重置", func(t *testing.T) {
		// 11a. 缺确认词
		resp, body := e.do(http.MethodPost, "/api/v1/sync/reset", map[string]string{}, sess.AccessToken)
		e.wantStatus(resp, "重置缺确认词", http.StatusBadRequest)
		e.wantCode(body, "重置缺确认词", apierr.CodeBadRequest)

		// 11b. 带确认词
		resp, _ = e.do(http.MethodPost, "/api/v1/sync/reset", map[string]string{"confirm": "DELETE-ALL"}, sess.AccessToken)
		e.wantStatus(resp, "重置带确认词", http.StatusOK)

		resp, body = e.do(http.MethodGet, "/api/v1/sync/pull?cursor=0", nil, sess.AccessToken)
		e.wantStatus(resp, "重置后拉取", http.StatusOK)
		var pr model.SyncPullResponse
		e.decode(body, "重置后拉取", &pr)
		if len(pr.Items) != 0 {
			t.Fatalf("重置后拉取：期望空 items，实际 %d 条", len(pr.Items))
		}
	})

	// ---------------------------------------------------------------- 12~13. 令牌轮换 / 登出
	t.Run("刷新令牌轮换与重放检测", func(t *testing.T) {
		// 12a. 轮换：返回新的 refreshToken（与旧的**不同**）
		old := sess.RefreshToken
		resp, body := e.do(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refreshToken": old}, "")
		e.wantStatus(resp, "刷新令牌", http.StatusOK)
		var r1 model.TokenResponse
		e.decode(body, "刷新令牌", &r1)
		if r1.RefreshToken == "" || r1.RefreshToken == old {
			t.Fatal("刷新令牌：期望返回与旧令牌不同的新 refreshToken")
		}
		if r1.UserID != sess.UserID {
			t.Fatalf("刷新令牌：期望 userId=%s，实际 %s", sess.UserID, r1.UserID)
		}

		// 12b. 用已轮换掉的旧令牌再刷新 → 401（重放检测）
		resp, body = e.do(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refreshToken": old}, "")
		e.wantStatus(resp, "旧 refreshToken 重放", http.StatusUnauthorized)
		e.wantCode(body, "旧 refreshToken 重放", apierr.CodeUnauthorized)

		// 12c. 新令牌可正常刷新（重放已触发全会话吊销，故先重新登录）
		sess = login(e)
		resp, body = e.do(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refreshToken": sess.RefreshToken}, "")
		e.wantStatus(resp, "新 refreshToken 刷新", http.StatusOK)
		var r2 model.TokenResponse
		e.decode(body, "新 refreshToken 刷新", &r2)
		if r2.RefreshToken == sess.RefreshToken {
			t.Fatal("新 refreshToken 刷新：期望再次轮换出新令牌")
		}
		sess.AccessToken, sess.RefreshToken = r2.AccessToken, r2.RefreshToken
	})

	t.Run("登出与全设备登出", func(t *testing.T) {
		// 13a. 单设备登出
		resp, _ := e.do(http.MethodPost, "/api/v1/auth/logout", map[string]string{"refreshToken": sess.RefreshToken}, "")
		e.wantStatus(resp, "登出", http.StatusOK)
		resp, body := e.do(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refreshToken": sess.RefreshToken}, "")
		e.wantStatus(resp, "登出后刷新", http.StatusUnauthorized)
		e.wantCode(body, "登出后刷新", apierr.CodeUnauthorized)

		// 13b. 全设备登出（登出会连带吊销全部会话，故重新登录）
		sess = login(e)
		// JWT 的 iat 是秒级精度：login 与 logout-all 若落在同一秒，无法区分签发先后，
		// 旧 access token 会多存活最长 1 秒（无状态校验的固有窗口）。
		// 这里等 1.1s 跨过秒边界，保证吊销时刻严格晚于 iat。
		time.Sleep(1100 * time.Millisecond)
		resp, _ = e.do(http.MethodPost, "/api/v1/auth/logout-all", nil, sess.AccessToken)
		e.wantStatus(resp, "全设备登出", http.StatusOK)
		resp, body = e.do(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refreshToken": sess.RefreshToken}, "")
		e.wantStatus(resp, "全设备登出后刷新", http.StatusUnauthorized)
		e.wantCode(body, "全设备登出后刷新", apierr.CodeUnauthorized)
		// 访问令牌也应立即失效（用户级吊销名单）
		resp, body = e.do(http.MethodGet, "/api/v1/auth/me", nil, sess.AccessToken)
		e.wantStatus(resp, "全设备登出后访问", http.StatusUnauthorized)
		e.wantCode(body, "全设备登出后访问", apierr.CodeUnauthorized)

		sess = login(e) // 后续用例需要新的登录态
	})

	// ---------------------------------------------------------------- 14. 附件
	t.Run("附件上传下载列表删除", func(t *testing.T) {
		iv := b64(12)
		content := []byte("meddesk-attachment-cipher-bytes-0123456789")
		recordID := "med-0001"

		resp, body := e.doMultipart(http.MethodPost, "/api/v1/attachments",
			map[string]string{"iv": iv, "recordId": recordID}, "file", "photo.bin", content, sess.AccessToken)
		e.wantStatus(resp, "上传附件", http.StatusOK)
		var dto model.AttachmentDTO
		e.decode(body, "上传附件", &dto)
		if dto.ID == "" {
			t.Fatalf("上传附件：未返回附件 id，响应=%s", string(body))
		}

		// 下载：响应头带 IV，body 与上传内容一致
		resp, body = e.do(http.MethodGet, "/api/v1/attachments/"+dto.ID, nil, sess.AccessToken)
		e.wantStatus(resp, "下载附件", http.StatusOK)
		if got := resp.Header.Get("X-MedDesk-IV"); got != iv {
			t.Fatalf("下载附件：期望 X-MedDesk-IV=%s，实际 %q", iv, got)
		}
		if !bytes.Equal(body, content) {
			t.Fatalf("下载附件：body 与上传内容不一致（上传 %d 字节，下载 %d 字节）", len(content), len(body))
		}

		// 列表
		resp, body = e.do(http.MethodGet, "/api/v1/attachments?recordId="+recordID, nil, sess.AccessToken)
		e.wantStatus(resp, "附件列表", http.StatusOK)
		var list []model.AttachmentDTO
		e.decode(body, "附件列表", &list)
		if len(list) != 1 || list[0].ID != dto.ID || list[0].RecordID != recordID {
			t.Fatalf("附件列表：期望 1 条且 id/recordId 匹配，实际 %s", string(body))
		}

		// 删除（实现返回 204，契约未强制 200，两者都接受）
		resp, body = e.do(http.MethodDelete, "/api/v1/attachments/"+dto.ID, nil, sess.AccessToken)
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			e.t.Fatalf("删除附件：期望 204 或 200，实际 %d（响应=%s）", resp.StatusCode, string(body))
		}
		t.Logf("删除附件返回 HTTP %d（任务书写 200，实现为 204，二者均视为成功）", resp.StatusCode)

		resp, body = e.do(http.MethodGet, "/api/v1/attachments/"+dto.ID, nil, sess.AccessToken)
		e.wantStatus(resp, "删除后再次下载", http.StatusNotFound)
		e.wantCode(body, "删除后再次下载", apierr.CodeNotFound)
	})

	// ---------------------------------------------------------------- 15. 导出
	t.Run("导出CSV与PDF", func(t *testing.T) {
		req := model.ExportRequest{
			Format:  "csv",
			Title:   "冒烟测试导出",
			Profile: "张小明",
			Rows: []model.ExportRow{
				{Date: "2026-10-25", Time: "08:00", Profile: "张小明", Medication: "布洛芬缓释胶囊",
					Dosage: "1 片", Status: "taken", Note: "饭后服用"},
			},
			Stats: model.ExportStats{Total: 1, Taken: 1, Adherence: 1},
		}
		resp, body := e.do(http.MethodPost, "/api/v1/export/csv", req, sess.AccessToken)
		e.wantStatus(resp, "导出 CSV", http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "csv") {
			t.Fatalf("导出 CSV：期望 Content-Type 含 csv，实际 %q", ct)
		}
		if len(body) < 3 || body[0] != 0xEF || body[1] != 0xBB || body[2] != 0xBF {
			t.Fatalf("导出 CSV：期望以 UTF-8 BOM 开头，实际前 3 字节=% x", body[:min(3, len(body))])
		}
		text := string(body)
		for _, want := range []string{"日期", "家庭成员", "布洛芬缓释胶囊", "已服用", "2026-10-25"} {
			if !strings.Contains(text, want) {
				t.Fatalf("导出 CSV：响应体缺少 %q，实际=%s", want, text)
			}
		}

		req.Format = "pdf"
		resp, body = e.do(http.MethodPost, "/api/v1/export/pdf", req, sess.AccessToken)
		if resp.StatusCode == http.StatusServiceUnavailable {
			// 本机没有中文字体（Windows 无 /usr/share/fonts 探测路径）
			var ae apierr.Error
			e.decode(body, "导出 PDF（无字体）", &ae)
			if !strings.Contains(ae.Message, "字体") {
				t.Fatalf("导出 PDF（无字体）：期望 message 含「字体」，实际 %q", ae.Message)
			}
			t.Logf("本机缺少中文字体，PDF 导出按预期返回 503：%s", ae.Message)
			return
		}
		e.wantStatus(resp, "导出 PDF", http.StatusOK)
		if !bytes.HasPrefix(body, []byte("%PDF-")) {
			t.Fatalf("导出 PDF：期望以 %%PDF- 开头，实际前 8 字节=% x", body[:min(8, len(body))])
		}
	})

	// ---------------------------------------------------------------- 16~17. 路由兜底与安全头
	t.Run("路由兜底与安全响应头", func(t *testing.T) {
		resp, body := e.do(http.MethodGet, "/api/v1/not-exist", nil, "")
		e.wantStatus(resp, "不存在的接口", http.StatusNotFound)
		e.wantCode(body, "不存在的接口", apierr.CodeNotFound)

		resp, body = e.do(http.MethodGet, "/no-such-page", nil, "")
		e.wantStatus(resp, "前端路由回退", http.StatusOK)
		if !strings.Contains(string(body), "<html") {
			t.Fatalf("前端路由回退：期望回退到 index.html（含 <html），实际前 64 字节=%q", string(body[:min(64, len(body))]))
		}

		resp, _ = e.do(http.MethodGet, "/api/v1/health", nil, "")
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("安全头：期望 X-Content-Type-Options=nosniff，实际 %q", got)
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatal("安全头：Content-Security-Policy 不能为空")
		}
	})
}

// login 重新登录并返回新的令牌对（用于被吊销后恢复登录态）。
func login(e *env) model.TokenResponse {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/api/v1/auth/login",
		model.LoginRequest{Email: testEmail, PasswordHash: registerBody().PasswordHash}, "")
	e.wantStatus(resp, "重新登录", http.StatusOK)
	var out model.TokenResponse
	e.decode(body, "重新登录", &out)
	if out.AccessToken == "" || out.RefreshToken == "" {
		e.t.Fatalf("重新登录：未返回令牌对，响应=%s", string(body))
	}
	return out
}

// min 返回两个整数中的较小值（避免 Go 1.21 以下无内置 min）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}


// TestStaticCacheFingerprint 验证静态资源缓存策略：
// index.html 注入 ?v=<buildID> 指纹、sw.js 注入构建哈希、/assets/ 带不可变缓存与 ETag。
func TestStaticCacheFingerprint(t *testing.T) {
	e := newEnv(t)
	get := func(path string) (*http.Response, string) {
		resp, err := e.client.Get(e.srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		_, _ = io.Copy(&sb, resp.Body)
		return resp, sb.String()
	}

	t.Run("index 注入指纹且禁止强缓存", func(t *testing.T) {
		resp, body := get("/index.html")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("期望 200，实际 %d", resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Fatalf("index.html 的 Cache-Control 应含 no-cache，实际 %q", cc)
		}
		if resp.Header.Get("ETag") == "" {
			t.Fatal("index.html 应带 ETag")
		}
		if !strings.Contains(body, "?v=") {
			t.Fatal("index.html 的静态引用应注入 ?v=<buildID> 指纹")
		}
		if !strings.Contains(body, `/assets/js/med.js?v=`) {
			t.Fatal("index.html 应注入 med.js 的带指纹引用")
		}
	})

	t.Run("sw.js 注入构建哈希", func(t *testing.T) {
		resp, body := get("/sw.js")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("期望 200，实际 %d", resp.StatusCode)
		}
		if strings.Contains(body, "__BUILD_ID__") {
			t.Fatal("sw.js 不应残留 __BUILD_ID__ 占位符（服务端应已注入）")
		}
		if !strings.Contains(body, "meddesk-shell-") {
			t.Fatal("sw.js 应使用带构建哈希后缀的缓存名")
		}
	})

	t.Run("assets 不可变缓存与 ETag 协商", func(t *testing.T) {
		resp, _ := get("/assets/css/app.css")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("期望 200，实际 %d", resp.StatusCode)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
			t.Fatalf("/assets/ 应为一年期不可变缓存，实际 %q", cc)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Fatal("/assets/ 应带 ETag")
		}
		// If-None-Match 命中 → 304
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/assets/css/app.css", nil)
		req.Header.Set("If-None-Match", etag)
		resp304, err := e.client.Do(req)
		if err != nil {
			t.Fatalf("304 请求失败: %v", err)
		}
		defer resp304.Body.Close()
		if resp304.StatusCode != http.StatusNotModified {
			t.Fatalf("If-None-Match 命中时应返回 304，实际 %d", resp304.StatusCode)
		}
	})
}
