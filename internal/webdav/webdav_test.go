package webdav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// 测试专用凭据与地址：全部为占位值，不含任何真实服务信息。
const (
	testUser = "tester"
	testPass = "placeholder-not-a-real-secret"
)

// fakeDav 是内存实现的极简 WebDAV 服务，覆盖 PROPFIND/PUT/GET/MKCOL/DELETE。
//
// 目录与文件分开存放：dirs 用相对路径为键（"" 表示根目录），files 用相对路径为键。
type fakeDav struct {
	mu       sync.Mutex
	dirs     map[string]bool
	files    map[string][]byte
	modified time.Time
}

// fakeEntry 是 PROPFIND 的返回条目。
type fakeEntry struct {
	rel   string
	isDir bool
	size  int64
}

func newFakeDav() *fakeDav {
	return &fakeDav{
		dirs:     map[string]bool{"": true},
		files:    map[string][]byte{},
		modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// ServeHTTP 实现最小 WebDAV 语义；凭据不符一律 401。
func (d *fakeDav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != testUser || pass != testPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rel, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/dav"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rel = strings.Trim(rel, "/")

	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Method {
	case "PROPFIND":
		d.propfind(w, r, rel)
	case http.MethodGet:
		if rel == "redir.txt" {
			// 用于验证代理不跟随重定向
			w.Header().Set("Location", "https://example.com/elsewhere.txt")
			w.WriteHeader(http.StatusFound)
			return
		}
		b, ok := d.files[rel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	case http.MethodPut:
		if !d.exists(parentOf(rel)) {
			w.WriteHeader(http.StatusConflict) // 父目录不存在
			return
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		d.files[rel] = buf.Bytes()
		w.WriteHeader(http.StatusCreated)
	case "MKCOL":
		if d.dirs[rel] || d.fileExists(rel) {
			w.WriteHeader(http.StatusMethodNotAllowed) // 已存在
			return
		}
		if !d.exists(parentOf(rel)) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		d.dirs[rel] = true
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if !d.dirs[rel] && !d.fileExists(rel) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(d.dirs, rel)
		delete(d.files, rel)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// propfind 返回 207 多状态响应：目录自身 + （Depth: 1 时）直接子项。
func (d *fakeDav) propfind(w http.ResponseWriter, r *http.Request, rel string) {
	var entries []fakeEntry
	switch {
	case d.dirs[rel]:
		entries = append(entries, fakeEntry{rel: rel, isDir: true})
		if r.Header.Get("Depth") == "1" {
			entries = append(entries, d.children(rel)...)
		}
	case d.fileExists(rel):
		entries = append(entries, fakeEntry{rel: rel, size: int64(len(d.files[rel]))})
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
	for _, e := range entries {
		b.WriteString(`<D:response><D:href>`)
		b.WriteString(davHref(e))
		b.WriteString(`</D:href><D:propstat><D:prop>`)
		if e.isDir {
			b.WriteString(`<D:resourcetype><D:collection/></D:resourcetype>`)
		} else {
			b.WriteString(`<D:resourcetype/><D:getcontentlength>`)
			b.WriteString(fmt.Sprintf("%d", e.size))
			b.WriteString(`</D:getcontentlength>`)
		}
		b.WriteString(`<D:getlastmodified>`)
		b.WriteString(d.modified.UTC().Format(http.TimeFormat))
		b.WriteString(`</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	b.WriteString(`</D:multistatus>`)
	_, _ = w.Write([]byte(b.String()))
}

// davHref 生成条目的 href（含 /dav 前缀，目录带尾斜杠）。
func davHref(e fakeEntry) string {
	p := "/dav/"
	if e.rel != "" {
		p += e.rel
	}
	if e.isDir && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}

// children 返回目录的直接子项（顺序不定，客户端侧会排序）。
func (d *fakeDav) children(rel string) []fakeEntry {
	var out []fakeEntry
	for k, v := range d.dirs {
		if k != "" && parentOf(k) == rel {
			out = append(out, fakeEntry{rel: k, isDir: v})
		}
	}
	for k, v := range d.files {
		if parentOf(k) == rel {
			out = append(out, fakeEntry{rel: k, size: int64(len(v))})
		}
	}
	return out
}

func (d *fakeDav) exists(rel string) bool     { return d.dirs[rel] || d.fileExists(rel) }
func (d *fakeDav) fileExists(rel string) bool { _, ok := d.files[rel]; return ok }

// parentOf 返回相对路径的父目录（顶层返回 ""）。
func parentOf(rel string) string {
	p := path.Dir(rel)
	if p == "." || p == "/" {
		return ""
	}
	return p
}

// ---- 测试脚手架 ----

// env 是单测环境：假 WebDAV 服务 + 已挂载的 gin 路由 + 测试凭据。
type env struct {
	router *gin.Engine
	client *Client
	dav    *fakeDav
	creds  Credentials
}

// newEnv 起一个 httptest TLS 服务（https 满足 scheme 校验），
// 并把客户端的 IP 拦截策略替换为放行，以便访问 127.0.0.1 上的测试服务。
func newEnv(t *testing.T) *env {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dav := newFakeDav()
	srv := httptest.NewTLSServer(dav)
	t.Cleanup(srv.Close)

	c := newClient()
	c.denyIP = func(net.IP) bool { return false } // 仅测试放行本地回环
	c.http.Transport = srv.Client().Transport     // 信任 httptest 自签证书

	r := gin.New()
	mount(r.Group("/api/v1/webdav"), c)
	return &env{
		router: r,
		client: c,
		dav:    dav,
		creds:  Credentials{BaseURL: srv.URL + "/dav/", User: testUser, Pass: testPass},
	}
}

// postJSON 发起 JSON 请求并返回响应记录器。
func (e *env) postJSON(endpoint string, body interface{}) *httptest.ResponseRecorder {
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webdav"+endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// upload 以 multipart/form-data 上传（config 为 JSON 字符串 + file 字段）。
func (e *env) upload(rel, name string, content []byte) *httptest.ResponseRecorder {
	cfg, _ := json.Marshal(pathRequest{Credentials: e.creds, Path: rel})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("config", string(cfg)); err != nil {
		panic(err)
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		panic(err)
	}
	if _, err := fw.Write(content); err != nil {
		panic(err)
	}
	if err := mw.Close(); err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webdav/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// ---- 功能用例 ----

// TestList 覆盖目录列出：子目录、文件、大小与修改时间。
func TestList(t *testing.T) {
	e := newEnv(t)
	e.dav.dirs["docs"] = true
	e.dav.files["docs/a.txt"] = []byte("hello")
	e.dav.files["readme.md"] = []byte("hi")

	rec := e.postJSON("/list", pathRequest{Credentials: e.creds, Path: "docs"})
	if rec.Code != http.StatusOK {
		t.Fatalf("list docs: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析 list 响应: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Name != "a.txt" || got.Items[0].Path != "docs/a.txt" {
		t.Fatalf("docs 子项不符: %+v", got.Items)
	}
	if got.Items[0].IsDir || got.Items[0].Size != 5 {
		t.Fatalf("a.txt 属性不符: %+v", got.Items[0])
	}
	if got.Items[0].Modified != "2026-01-02T03:04:05Z" {
		t.Fatalf("修改时间应为 RFC3339 UTC，实际 %q", got.Items[0].Modified)
	}

	// 根目录：应包含 docs 目录与 readme.md，且不含目录自身。
	rec = e.postJSON("/list", pathRequest{Credentials: e.creds})
	if rec.Code != http.StatusOK {
		t.Fatalf("list root: code=%d body=%s", rec.Code, rec.Body.String())
	}
	got = listResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Items) != 2 || got.Items[0].Name != "docs" || !got.Items[0].IsDir || got.Items[1].Name != "readme.md" {
		t.Fatalf("根目录条目不符: %+v", got.Items)
	}
}

// TestUploadDownloadDelete 覆盖上传（自动建父目录）、下载、删除全链路。
func TestUploadDownloadDelete(t *testing.T) {
	e := newEnv(t)
	content := []byte("encrypted-payload")

	rec := e.upload("x/y/z.txt", "z.txt", content)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var up uploadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	if !up.OK || up.Path != "x/y/z.txt" {
		t.Fatalf("upload 响应不符: %+v", up)
	}
	// 父目录应被自动创建
	if !e.dav.dirs["x"] || !e.dav.dirs["x/y"] {
		t.Fatalf("父目录未自动创建: dirs=%v", e.dav.dirs)
	}

	rec = e.postJSON("/download", pathRequest{Credentials: e.creds, Path: "x/y/z.txt"})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("download: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "z.txt") {
		t.Fatalf("缺少 Content-Disposition 文件名: %q", cd)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatalf("缺少 Content-Type")
	}

	rec = e.postJSON("/delete", pathRequest{Credentials: e.creds, Path: "x/y/z.txt"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := e.dav.files["x/y/z.txt"]; ok {
		t.Fatalf("删除后文件仍存在")
	}
}

// TestMkdirIdempotent 覆盖逐级建目录与重复创建的幂等性。
func TestMkdirIdempotent(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		rec := e.postJSON("/mkdir", pathRequest{Credentials: e.creds, Path: "a/b/c"})
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 mkdir: code=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if !e.dav.dirs["a"] || !e.dav.dirs["a/b"] || !e.dav.dirs["a/b/c"] {
		t.Fatalf("目录未逐级创建: %v", e.dav.dirs)
	}
}

// TestTestEndpoint 覆盖连通性探测：根存在、根不存在、凭据错误。
func TestTestEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.postJSON("/test", e.creds)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got testResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.OK || !got.Exists {
		t.Fatalf("根路径应存在: %+v", got)
	}

	// 根不存在：ok=true，exists=false
	delete(e.dav.dirs, "")
	rec = e.postJSON("/test", e.creds)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.OK || got.Exists {
		t.Fatalf("根不存在时应 ok=true exists=false，实际 %+v", got)
	}

	// 凭据错误：401，且响应不得回显密码
	rec = e.postJSON("/test", Credentials{BaseURL: e.creds.BaseURL, User: testUser, Pass: "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误口令应返回 401，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "wrong") || strings.Contains(rec.Body.String(), testPass) {
		t.Fatalf("错误响应泄漏凭据: %s", rec.Body.String())
	}
}

// ---- 安全用例 ----

// TestPathTraversal 覆盖路径穿越：上级引用、绝对路径、反斜杠、URL 编码变体、空字节。
func TestPathTraversal(t *testing.T) {
	e := newEnv(t)
	bad := []string{
		"../secret.txt",
		"..",
		"a/../../b",
		"/abs/path",
		`..\windows`,
		"%2e%2e/%2e%2e/etc/passwd",
		"%252e%252e/x",
		"a\x00b",
		"docs/../../outside",
	}
	for _, p := range bad {
		for _, ep := range []string{"/list", "/mkdir", "/delete", "/download"} {
			rec := e.postJSON(ep, pathRequest{Credentials: e.creds, Path: p})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("路径 %q 经 %s 应被拒绝(400)，实际 %d body=%s", p, ep, rec.Code, rec.Body.String())
			}
		}
		// 上传同样要拒绝
		if rec := e.upload(p, "f.txt", []byte("x")); rec.Code != http.StatusBadRequest {
			t.Fatalf("上传路径 %q 应被拒绝(400)，实际 %d", p, rec.Code)
		}
	}
}

// TestSSRF 覆盖 SSRF 防护：内网 IP 字面量与 http scheme。
func TestSSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	mount(r.Group("/api/v1/webdav"), newClient()) // 生产策略：拦截内网地址

	cases := []struct {
		name    string
		baseURL string
		want    int
	}{
		{"回环 127.0.0.1", "https://127.0.0.1/dav/", http.StatusForbidden},
		{"回环 ::1", "https://[::1]/dav/", http.StatusForbidden},
		{"私有 10/8", "https://10.0.0.1/dav/", http.StatusForbidden},
		{"私有 172.16/12", "https://172.16.0.1/dav/", http.StatusForbidden},
		{"私有 192.168/16", "https://192.168.1.10/dav/", http.StatusForbidden},
		{"链路本地 169.254", "https://169.254.169.254/latest/", http.StatusForbidden},
		{"未指定 0.0.0.0", "https://0.0.0.0/dav/", http.StatusForbidden},
		{"http scheme", "http://example.com/dav/", http.StatusBadRequest},
		{"地址内嵌凭据", "https://user:pw@example.com/dav/", http.StatusBadRequest},
	}
	for _, c := range cases {
		body, _ := json.Marshal(pathRequest{
			Credentials: Credentials{BaseURL: c.baseURL, User: testUser, Pass: testPass},
			Path:        "x",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webdav/list", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s: 期望 %d，实际 %d body=%s", c.name, c.want, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), testPass) {
			t.Fatalf("%s: 错误响应泄漏口令", c.name)
		}
	}
}

// TestDenyIP 直接覆盖 IP 黑名单本身。
func TestDenyIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "127.255.255.254", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.255",
		"192.168.0.1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "fc00::1", "fd12:3456::1", "::ffff:127.0.0.1"}
	for _, s := range blocked {
		if !defaultDenyIP(net.ParseIP(s)) {
			t.Fatalf("%s 应被拦截", s)
		}
	}
	// 203.0.113.0/24 是 RFC 5737 文档保留地址（非真实主机），用于表示公网地址。
	for _, s := range []string{"203.0.113.10", "198.51.100.7", "2400:cb00::1"} {
		if defaultDenyIP(net.ParseIP(s)) {
			t.Fatalf("%s 不应被拦截", s)
		}
	}
}

// TestNoRedirect 验证代理不跟随重定向（防止被跳转到内网地址）。
func TestNoRedirect(t *testing.T) {
	e := newEnv(t)
	rec := e.postJSON("/download", pathRequest{Credentials: e.creds, Path: "redir.txt"})
	if rec.Code == http.StatusOK {
		t.Fatalf("重定向不应被跟随: %s", rec.Body.String())
	}
}

// TestEmptyCredentials 覆盖缺失地址/用户名。
func TestEmptyCredentials(t *testing.T) {
	e := newEnv(t)
	cases := []Credentials{
		{BaseURL: "", User: testUser, Pass: testPass},
		{BaseURL: e.creds.BaseURL, User: "", Pass: testPass},
		{BaseURL: "   ", User: "  ", Pass: ""},
	}
	for i, c := range cases {
		rec := e.postJSON("/list", pathRequest{Credentials: c})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("用例 %d 应返回 400，实际 %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}

// TestLimits 覆盖下载体上限与列目录条目上限。
func TestLimits(t *testing.T) {
	e := newEnv(t)
	e.client.maxDownload = 4
	if rec := e.upload("big.bin", "big.bin", []byte("0123456789")); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.postJSON("/download", pathRequest{Credentials: e.creds, Path: "big.bin"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限下载应返回 413，实际 %d", rec.Code)
	}

	e.client.maxItems = 1
	e.dav.files["one.txt"] = []byte("1")
	e.dav.files["two.txt"] = []byte("2")
	rec = e.postJSON("/list", pathRequest{Credentials: e.creds})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("条目超限应返回 413，实际 %d", rec.Code)
	}
}

// TestCleanRelPath 覆盖相对路径规范化与拒绝规则。
func TestCleanRelPath(t *testing.T) {
	ok := map[string]string{
		"":            "",
		"/":           "",
		"a.txt":       "a.txt",
		"docs/a.txt":  "docs/a.txt",
		"docs//a.txt": "docs/a.txt",
		"docs/":       "docs",
		"a..b/c":      "a..b/c",
	}
	for in, want := range ok {
		got, err := cleanRelPath(in)
		if err != nil || got != want {
			t.Fatalf("cleanRelPath(%q) = %q, %v（期望 %q）", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../a", "a/..", "/a", `\a`, "a\\b", "%2e%2e/a", "%252e%252e/a", "a\x00b"} {
		if got, err := cleanRelPath(in); err == nil {
			t.Fatalf("cleanRelPath(%q) 应被拒绝，实际返回 %q", in, got)
		}
	}
}
