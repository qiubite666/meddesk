// Package webdav 实现 WebDAV 代理：把用户在设置页填写的自建 WebDAV 凭据随请求带来，
// 由服务端代为转发（目标服务通常不返回 CORS 头，浏览器无法直连）。
//
// 【安全前提】凭据只存在于单次请求的内存中：不落盘、不入缓存、不写全局状态、不进日志；
// 错误响应与 slog 日志只输出方法、相对路径与状态码，绝不回显 user/pass。
//
// 【威胁模型】用户提供的 baseURL 与 path 均视为不可信输入：
//  1. SSRF：强制 https，解析后的目标 IP 不得落在回环/私有/链路本地/未指定网段，且禁止跟随重定向；
//  2. 路径穿越：path 必须是相对路径，拒绝任何 "."/".." 段（含多层 URL 编码变体），
//     规范化后仍需落在 baseURL 的 path 前缀内；
//  3. 资源限制：15s 超时、下载体上限、列目录条目上限。
package webdav

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

const (
	// requestTimeout 是单次远程请求的超时（含连接、发送、读取响应头与响应体）。
	requestTimeout = 15 * time.Second

	// maxDownloadBytes 是下载响应体的上限（200 MiB），超出直接报错，避免把超大响应读进内存。
	maxDownloadBytes = 200 << 20

	// maxListItems 是单次列目录返回的最大条目数。
	maxListItems = 2000

	// maxListBodyBytes 是 PROPFIND 207 响应体的字节上限（4 MiB）。
	// 理由：一个条目约 200~300 字节，2000 条约 0.5 MiB，4 MiB 已留足 8 倍余量；
	// 若不加限制，恶意/异常服务端可以返回任意大的 XML，在 xml.Decoder 反序列化
	// 阶段就把内存吃光（此时还来得及返回错误，不用等解析完成再判条目数）。
	maxListBodyBytes = 4 << 20

	// maxPathBytes 是相对路径的字节上限。
	maxPathBytes = 1024

	// maxDrainBytes 是错误响应体丢弃时的最大读取量（仅用于释放连接，不返回给客户端）。
	maxDrainBytes = 8 << 10
)

// propfindBody 是 PROPFIND 请求体：只索取列目录需要的三个属性，
// 避免服务端返回无关属性（部分实现对空请求体返回 400）。
const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<d:propfind xmlns:d="DAV:"><d:prop>` +
	`<d:resourcetype/><d:getcontentlength/><d:getlastmodified/>` +
	`</d:prop></d:propfind>`

// ---- 数据契约（与前端约定，字段签名不可更改）----

// Credentials 是用户随请求带来的 WebDAV 凭据，仅存在于单次请求内存中。
type Credentials struct {
	BaseURL string `json:"baseURL"` // 例：https://example.com/meddesk/
	User    string `json:"user"`
	Pass    string `json:"pass"`
}

// pathRequest 是带相对路径的请求体；path 相对 baseURL，空或 "/" 表示根目录。
type pathRequest struct {
	Credentials
	Path string `json:"path"`
}

// Item 是列目录返回的单条目。
type Item struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"` // RFC3339（UTC）
}

type listResponse struct {
	Items []Item `json:"items"`
}

type okResponse struct {
	OK bool `json:"ok"`
}

type testResponse struct {
	OK     bool `json:"ok"`
	Exists bool `json:"exists"`
}

type uploadResponse struct {
	OK   bool   `json:"ok"`
	Path string `json:"path"`
}

// ---- PROPFIND 207 响应解析 ----

type davMultistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	ContentLength string          `xml:"getcontentlength"`
	LastModified  string          `xml:"getlastmodified"`
	ResourceType  davResourceType `xml:"resourcetype"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

// ---- 客户端 ----

// denyIPFunc 判定某个目标 IP 是否禁止访问。
type denyIPFunc func(ip net.IP) bool

// Client 是无状态的 WebDAV 转发客户端：不持有任何凭据。
type Client struct {
	http *http.Client
	// denyIP 为目标 IP 的拦截策略；生产使用 defaultDenyIP，测试可替换为放行回环地址。
	denyIP denyIPFunc
	// maxDownload 与 maxItems 是资源上限，拆成字段便于测试注入小值。
	maxDownload int64
	maxItems    int
}

// newClient 构造使用默认安全策略的客户端。
func newClient() *Client {
	return &Client{
		http: &http.Client{
			Timeout: requestTimeout,
			// 重定向可能把请求带到内网地址，一律拒绝（SSRF 防护的一部分）。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("禁止跟随重定向")
			},
		},
		denyIP:      defaultDenyIP,
		maxDownload: maxDownloadBytes,
		maxItems:    maxListItems,
	}
}

// RegisterWebDAV 在给定的 gin 路由组上挂载 WebDAV 代理端点。
// 由 internal/server 在 /api/v1/webdav 组上调用，鉴权沿用上层中间件。
func RegisterWebDAV(group *gin.RouterGroup) { mount(group, newClient()) }

// mount 在路由组上挂载端点（拆分出来便于测试注入客户端）。
func mount(group *gin.RouterGroup, c *Client) {
	group.POST("/test", c.handleTest)
	group.POST("/list", c.handleList)
	group.POST("/mkdir", c.handleMkdir)
	group.POST("/delete", c.handleDelete)
	group.POST("/upload", c.handleUpload)
	group.POST("/download", c.handleDownload)
}

// ---- 处理函数 ----

// handleTest 校验连通性并判断根路径是否存在。
func (c *Client) handleTest(g *gin.Context) {
	var creds Credentials
	if err := g.ShouldBindJSON(&creds); err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "请求体格式非法"))
		return
	}
	tgt, err := c.buildTarget(g.Request.Context(), creds, "")
	if err != nil {
		writeErr(g, err)
		return
	}
	resp, err := c.do(g.Request.Context(), "PROPFIND", tgt.URL, creds, strings.NewReader(propfindBody),
		map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		writeErr(g, err)
		return
	}
	defer resp.Body.Close()
	drain(resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		g.JSON(http.StatusOK, testResponse{OK: true, Exists: false})
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		g.JSON(http.StatusOK, testResponse{OK: true, Exists: true})
	default:
		writeErr(g, statusError(resp.StatusCode))
	}
}

// handleList 列出目录内容（PROPFIND Depth: 1）。
func (c *Client) handleList(g *gin.Context) {
	var req pathRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "请求体格式非法"))
		return
	}
	tgt, err := c.buildTarget(g.Request.Context(), req.Credentials, req.Path)
	if err != nil {
		writeErr(g, err)
		return
	}
	resp, err := c.do(g.Request.Context(), "PROPFIND", tgt.URL, req.Credentials, strings.NewReader(propfindBody),
		map[string]string{"Depth": "1", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		writeErr(g, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		drain(resp.Body)
		writeErr(g, statusError(resp.StatusCode))
		return
	}
	// 先套字节上限再解析：避免恶意服务端用超大 XML 在 Decode 阶段撑爆内存。
	lr := &io.LimitedReader{R: resp.Body, N: maxListBodyBytes + 1}
	items, err := parseMultiStatus(lr, tgt.Base, tgt.Rel)
	if err != nil {
		if lr.N <= 0 {
			writeErr(g, apierr.Newf(apierr.CodePayloadTooLarge, "目录响应超过上限 %d 字节", maxListBodyBytes))
			return
		}
		writeErr(g, err)
		return
	}
	// Decoder 可能在根元素结束后提前停止，需把剩余字节读掉才能确认是否超限。
	_, _ = io.Copy(io.Discard, lr)
	if lr.N <= 0 {
		writeErr(g, apierr.Newf(apierr.CodePayloadTooLarge, "目录响应超过上限 %d 字节", maxListBodyBytes))
		return
	}
	if len(items) > c.maxItems {
		writeErr(g, apierr.Newf(apierr.CodePayloadTooLarge, "目录条目过多（上限 %d）", c.maxItems))
		return
	}
	g.JSON(http.StatusOK, listResponse{Items: items})
}

// handleMkdir 逐级创建目录；已存在视为成功（幂等）。
func (c *Client) handleMkdir(g *gin.Context) {
	var req pathRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "请求体格式非法"))
		return
	}
	if _, err := c.buildTarget(g.Request.Context(), req.Credentials, req.Path); err != nil {
		writeErr(g, err)
		return
	}
	if err := c.ensureDir(g.Request.Context(), req.Credentials, req.Path); err != nil {
		writeErr(g, err)
		return
	}
	g.JSON(http.StatusOK, okResponse{OK: true})
}

// handleDelete 删除文件或目录。
func (c *Client) handleDelete(g *gin.Context) {
	var req pathRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "请求体格式非法"))
		return
	}
	tgt, err := c.buildTarget(g.Request.Context(), req.Credentials, req.Path)
	if err != nil {
		writeErr(g, err)
		return
	}
	resp, err := c.do(g.Request.Context(), http.MethodDelete, tgt.URL, req.Credentials, nil, nil)
	if err != nil {
		writeErr(g, err)
		return
	}
	defer resp.Body.Close()
	drain(resp.Body)
	if resp.StatusCode/100 != 2 {
		writeErr(g, statusError(resp.StatusCode))
		return
	}
	g.JSON(http.StatusOK, okResponse{OK: true})
}

// handleUpload 接收 multipart/form-data：config（JSON 字符串）+ file，PUT 到远程。
func (c *Client) handleUpload(g *gin.Context) {
	raw := g.Request.FormValue("config")
	if raw == "" {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "缺少 config 字段"))
		return
	}
	var req pathRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "config 字段不是合法 JSON"))
		return
	}
	f, fh, err := g.Request.FormFile("file")
	if err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "缺少 file 字段"))
		return
	}
	defer f.Close()
	want := req.Path
	if strings.TrimSpace(want) == "" {
		want = fh.Filename // 未指定路径时落在上传文件名对应的相对路径
	}
	tgt, err := c.buildTarget(g.Request.Context(), req.Credentials, want)
	rel := tgt.Rel
	if err != nil {
		writeErr(g, err)
		return
	}
	// 上传前确保父目录存在（逐级 MKCOL，已存在则跳过）。
	if parent := path.Dir(rel); parent != "." && parent != "/" {
		if err := c.ensureDir(g.Request.Context(), req.Credentials, parent); err != nil {
			writeErr(g, err)
			return
		}
	}

	// 直接把 multipart 的文件视图作为 PUT 请求体流式发送：FormFile 返回的
	// multipart.File 支持 Seek，小文件是内存 section，大文件是已落盘的临时文件，
	// 两条路径都不需要再整份读进内存。ContentLength 显式取 fh.Size，
	// 保证请求带确定的 Content-Length（部分 WebDAV 服务拒绝 chunked 编码）。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeErr(g, apierr.Wrap(apierr.CodeBadRequest, err, "无法读取上传内容"))
		return
	}
	resp, err := c.doWithLength(g.Request.Context(), http.MethodPut, tgt.URL, req.Credentials, f, fh.Size, nil)
	if err != nil {
		writeErr(g, err)
		return
	}
	defer resp.Body.Close()
	drain(resp.Body)
	if resp.StatusCode/100 != 2 {
		writeErr(g, statusError(resp.StatusCode))
		return
	}
	g.JSON(http.StatusOK, uploadResponse{OK: true, Path: rel})
}

// handleDownload 下载文件并以附件形式返回。
func (c *Client) handleDownload(g *gin.Context) {
	var req pathRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		writeErr(g, apierr.New(apierr.CodeBadRequest, "请求体格式非法"))
		return
	}
	tgt, err := c.buildTarget(g.Request.Context(), req.Credentials, req.Path)
	rel := tgt.Rel
	if err != nil {
		writeErr(g, err)
		return
	}
	resp, err := c.do(g.Request.Context(), http.MethodGet, tgt.URL, req.Credentials, nil, nil)
	if err != nil {
		writeErr(g, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		drain(resp.Body)
		writeErr(g, statusError(resp.StatusCode))
		return
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	// 流式转发，不再整份读进内存。
	//
	// 上限处理分两种：
	//  1. Content-Length 已知：此时响应头尚未发出，超限可以干净地返回 413；
	//  2. Content-Length 未知（chunked / 上游谎报）：只能边读边写。此时若静默截断，
	//     客户端会拿到一个"HTTP 200 + 长度不足"的损坏文件，比直接失败更糟。
	//     因此超过上限时**主动断开连接**（hijack 关闭 TCP；HTTP/2 等不支持
	//     hijack 的场景退回 panic(http.ErrAbortHandler)），让客户端看到传输中断。
	src := io.LimitReader(resp.Body, c.maxDownload+1)
	contentLength := int64(-1)
	if resp.ContentLength >= 0 {
		if resp.ContentLength > c.maxDownload {
			writeErr(g, apierr.Newf(apierr.CodePayloadTooLarge, "文件超过下载上限 %d 字节", c.maxDownload))
			return
		}
		contentLength = resp.ContentLength
		// 按声明长度精确转发，避免上游多发的字节与 Content-Length 不一致。
		src = io.LimitReader(resp.Body, resp.ContentLength)
	}
	headers := map[string]string{
		"Content-Disposition":    attachmentDisposition(path.Base(rel)),
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	}
	if contentLength >= 0 {
		g.DataFromReader(http.StatusOK, contentLength, ctype, src, headers)
		return
	}

	// 未知长度：先发头，再手动拷贝，以便在超限时中断而不是"成功返回半个文件"。
	for k, v := range headers {
		g.Writer.Header().Set(k, v)
	}
	g.Writer.Header().Set("Content-Type", ctype)
	g.Status(http.StatusOK)
	n, copyErr := io.Copy(g.Writer, src)
	if n > c.maxDownload {
		abortResponse(g)
		return
	}
	if copyErr != nil {
		// 头已发出，无法改状态码；断开连接，避免调用方把半截响应当成功。
		abortResponse(g)
		return
	}
	g.Writer.Flush()
}

// abortResponse 立即中断当前响应。
//
// 优先 hijack 关闭底层 TCP：这是 HTTP/1.1 下唯一能让客户端明确感知"传输未完成"
// 的方式（仅设置状态码已经太晚）。HTTP/2 不支持 hijack，退回 net/http 的
// ErrAbortHandler 语义——由 server 直接断开该 stream。
func abortResponse(g *gin.Context) {
	if hj, ok := g.Writer.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

// ---- 远程调用 ----

// do 发起一次带 Basic Auth 的远程请求；凭据只在本次调用中使用，不写入任何状态。
func (c *Client) do(ctx context.Context, method, target string, creds Credentials, body io.Reader, header map[string]string) (*http.Response, error) {
	return c.doWithLength(ctx, method, target, creds, body, 0, header)
}

// doWithLength 是 do 的流式版本：contentLength > 0 时显式设置请求体长度，
// 用于把 multipart 文件直接作为请求体转发（否则未知长度的 io.Reader 会走 chunked）。
func (c *Client) doWithLength(ctx context.Context, method, target string, creds Credentials,
	body io.Reader, contentLength int64, header map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeBadRequest, err, "构造远程请求失败")
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	req.SetBasicAuth(creds.User, creds.Pass)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// 只记录方法与路径，不记录凭据；底层错误可能含主机地址，但不含 user/pass。
		slog.Warn("WebDAV 代理请求失败", "method", method, "error", err)
		return nil, apierr.Wrap(apierr.CodeUnavailable, err, "无法连接 WebDAV 服务（请检查地址与网络）")
	}
	return resp, nil
}

// ensureBase 逐级创建 baseURL 指向的根目录本身。
// 场景：用户配置的根目录（如服务下的 meddesk/）可能尚不存在，此时直接在其下
// MKCOL 子目录会被服务端以 409 Conflict 拒绝，因此建子目录前先把根补齐。
// 各级已存在时 WebDAV 返回 405 Method Not Allowed，同样视为成功。
func (c *Client) ensureBase(ctx context.Context, creds Credentials) error {
	base, err := c.parseBase(ctx, creds)
	if err != nil {
		return err
	}
	cur := ""
	for _, seg := range strings.Split(strings.Trim(base.Path, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		target := *base
		target.Path = cur
		target.RawPath = ""
		resp, err := c.do(ctx, "MKCOL", target.String(), creds, nil, nil)
		if err != nil {
			return err
		}
		code := resp.StatusCode
		drain(resp.Body)
		resp.Body.Close()
		switch code {
		case http.StatusCreated, http.StatusOK, http.StatusNoContent, http.StatusMethodNotAllowed:
			// 新建成功或已存在
		default:
			return statusError(code)
		}
	}
	return nil
}

// ensureDir 逐级 MKCOL 创建目录；已存在（405）视为成功。
func (c *Client) ensureDir(ctx context.Context, creds Credentials, rel string) error {
	// 先确保根目录存在，否则首个子目录会因父级缺失而 409。
	if err := c.ensureBase(ctx, creds); err != nil {
		return err
	}
	segs := strings.Split(strings.Trim(rel, "/"), "/")
	cur := ""
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		if cur == "" {
			cur = seg
		} else {
			cur += "/" + seg
		}
		tgt, err := c.buildTarget(ctx, creds, cur)
		if err != nil {
			return err
		}
		resp, err := c.do(ctx, "MKCOL", tgt.URL, creds, nil, nil)
		if err != nil {
			return err
		}
		code := resp.StatusCode
		drain(resp.Body)
		resp.Body.Close()
		switch code {
		case http.StatusCreated, http.StatusOK, http.StatusNoContent: // 新建成功
		case http.StatusMethodNotAllowed: // 已存在，幂等视为成功
		default:
			return statusError(code)
		}
	}
	return nil
}

// ---- 输入校验 ----

// target 是校验通过后的请求目标。
type davTarget struct {
	URL  string // 完整请求 URL
	Rel  string // 相对 baseURL 的规范化路径（根目录为 ""）
	Base string // baseURL 的 path 前缀（已转义，以 "/" 结尾）
}

// buildTarget 校验凭据与路径，返回最终请求目标。
func (c *Client) buildTarget(ctx context.Context, creds Credentials, rawPath string) (davTarget, error) {
	base, err := c.parseBase(ctx, creds)
	if err != nil {
		return davTarget{}, err
	}
	rel, err := cleanRelPath(rawPath)
	if err != nil {
		return davTarget{}, err
	}
	prefix := base.EscapedPath()
	u := base
	if rel != "" {
		u = base.JoinPath(rel)
		// 兜底断言：规范化后的路径必须仍在 baseURL 的 path 前缀内，绝不逃出根目录。
		if !strings.HasPrefix(u.EscapedPath(), prefix) {
			return davTarget{}, apierr.New(apierr.CodeBadRequest, "路径越出 WebDAV 根目录")
		}
	}
	return davTarget{URL: u.String(), Rel: rel, Base: prefix}, nil
}

// parseBase 校验并规范化 baseURL：强制 https、禁止内嵌用户信息、拦截内网目标。
func (c *Client) parseBase(ctx context.Context, creds Credentials) (*url.URL, error) {
	raw := strings.TrimSpace(creds.BaseURL)
	if raw == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "缺少 WebDAV 服务地址")
	}
	if strings.TrimSpace(creds.User) == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "缺少 WebDAV 用户名")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, apierr.New(apierr.CodeBadRequest, "WebDAV 服务地址格式非法")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, apierr.New(apierr.CodeBadRequest, "WebDAV 服务地址必须使用 https")
	}
	if u.User != nil {
		// 形如 https://user:pass@host/ 的地址会把凭据写进 URL（易被代理日志记录）。
		return nil, apierr.New(apierr.CodeBadRequest, "WebDAV 服务地址不得内嵌用户名与密码")
	}
	host := u.Hostname()
	if host == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "WebDAV 服务地址缺少主机名")
	}
	if err := c.checkHost(ctx, host); err != nil {
		return nil, err
	}
	bp := u.Path
	if bp == "" {
		bp = "/"
	}
	if !strings.HasSuffix(bp, "/") {
		bp += "/" // 保证后续拼接都在该目录之内
	}
	out := *u
	out.User = nil
	out.Path = bp
	out.RawPath = ""
	out.RawQuery = ""
	out.Fragment = ""
	return &out, nil
}

// checkHost 解析 host（域名走 DNS）并逐个校验 IP，任一命中黑名单即拒绝。
//
// 说明：DNS rebinding（解析—连接之间记录被改写）无法在应用层完全杜绝，
// 这里挡住的是「域名/字面量直接指向内网」这一类最常见的 SSRF。
func (c *Client) checkHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if c.denyIP(ip) {
			return apierr.New(apierr.CodeForbidden, "目标地址属于内网或保留网段，禁止访问")
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return apierr.Wrap(apierr.CodeUnavailable, err, "无法解析 WebDAV 服务地址")
	}
	for _, a := range addrs {
		if c.denyIP(a.IP) {
			return apierr.New(apierr.CodeForbidden, "目标地址解析到内网或保留网段，禁止访问")
		}
	}
	return nil
}

// blockedNets 是需要显式拦截的保留 / 特殊用途网段（生产策略）。
//
// 说明：net.IP.IsPrivate() 已覆盖 RFC1918（10/8、172.16/12、192.168/16）与
// fc00::/7（ULA），回环 / 链路本地 / 未指定由 IsLoopback / IsLinkLocalUnicast /
// IsUnspecified 覆盖，这里只补它们没覆盖到的部分，不重复实现也不删掉前者。
// 预解析成 IPNet 切片（进程启动时一次），避免每次请求都做字符串解析。
var blockedNets = mustParseCIDRs(
	"0.0.0.0/8",       // 本网络（含 0.0.0.0）
	"100.64.0.0/10",   // RFC 6598 运营商级 NAT（CGNAT）
	"192.0.0.0/24",    // IETF 协议分配（含 192.0.0.170/171 NAT64）
	"192.0.2.0/24",    // RFC 5737 TEST-NET-1
	"198.18.0.0/15",   // RFC 2544 网络设备基准测试
	"198.51.100.0/24", // RFC 5737 TEST-NET-2
	"203.0.113.0/24",  // RFC 5737 TEST-NET-3
	"224.0.0.0/4",     // IPv4 多播
	"240.0.0.0/4",     // 保留位（含 255.255.255.255 广播）
	"ff00::/8",        // IPv6 多播
)

// mustParseCIDRs 在初始化阶段解析网段；常量写错属于编码错误，直接 panic 暴露。
func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("webdav: 非法 CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// defaultDenyIP 是生产环境的 IP 拦截策略：回环、私有、链路本地、未指定，
// 以及 blockedNets 中列出的保留 / 特殊用途网段。
func defaultDenyIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// cleanRelPath 校验相对路径并返回规范化结果（不含前导 "/"，根目录返回 ""）。
func cleanRelPath(p string) (string, error) {
	if p == "" || p == "/" {
		return "", nil // 根目录
	}
	if len(p) > maxPathBytes {
		return "", apierr.New(apierr.CodeBadRequest, "路径过长")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", apierr.New(apierr.CodeBadRequest, "路径包含空字节")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", apierr.New(apierr.CodeBadRequest, "路径必须是相对路径")
	}
	if strings.Contains(p, `\`) {
		// 反斜杠在部分服务端会被当作分隔符，是穿越的常见载体，一律拒绝。
		return "", apierr.New(apierr.CodeBadRequest, "路径不得包含反斜杠")
	}
	// 叠加解码两次：既拦 %2e%2e，也拦 %252e%252e 这类多层编码穿越。
	cur := p
	for i := 0; i < 2; i++ {
		dec, err := url.PathUnescape(cur)
		if err != nil {
			return "", apierr.New(apierr.CodeBadRequest, "路径包含非法转义序列")
		}
		for _, seg := range strings.FieldsFunc(dec, isPathSep) {
			if seg == ".." {
				return "", apierr.New(apierr.CodeBadRequest, "路径不得包含上级目录引用")
			}
		}
		cur = dec
	}
	return strings.TrimPrefix(path.Clean("/"+p), "/"), nil
}

func isPathSep(r rune) bool { return r == '/' || r == '\\' }

// ---- 响应解析与辅助 ----

// parseMultiStatus 解析 207 响应：href 解码后取相对路径，并跳过请求目录自身。
// basePrefix 是 baseURL 的 path 前缀（已转义，以 "/" 结尾），selfRel 是被列目录的相对路径。
func parseMultiStatus(r io.Reader, basePrefix, selfRel string) ([]Item, error) {
	var ms davMultistatus
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, apierr.Wrap(apierr.CodeUnavailable, err, "无法解析 WebDAV 目录响应")
	}
	items := make([]Item, 0, len(ms.Responses))
	for _, resp := range ms.Responses {
		rel, err := relFromHref(resp.Href, basePrefix)
		if err != nil || rel == selfRel {
			continue // 跳过目录自身与无法解析的条目
		}
		var size int64
		modified := ""
		isDir := false
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			size, _ = strconv.ParseInt(strings.TrimSpace(ps.Prop.ContentLength), 10, 64)
			isDir = ps.Prop.ResourceType.Collection != nil
			if t, err := http.ParseTime(strings.TrimSpace(ps.Prop.LastModified)); err == nil {
				modified = t.UTC().Format(time.RFC3339)
			}
			break
		}
		name := path.Base(rel)
		if rel == "" {
			name = ""
		}
		items = append(items, Item{Name: name, Path: rel, IsDir: isDir, Size: size, Modified: modified})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// relFromHref 把 href 转成相对于 baseURL path 的相对路径。
func relFromHref(href, prefix string) (string, error) {
	raw := strings.TrimSpace(href)
	if raw == "" {
		return "", errors.New("空 href")
	}
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil {
			raw = u.EscapedPath()
		}
	}
	if prefix != "" && prefix != "/" {
		if !strings.HasPrefix(raw, prefix) {
			return "", errors.New("href 越出根目录")
		}
		raw = strings.TrimPrefix(raw, prefix)
	}
	dec, err := url.PathUnescape(strings.TrimPrefix(raw, "/"))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(dec, "/"), nil
}

// attachmentDisposition 生成 RFC 5987 的 Content-Disposition（支持非 ASCII 文件名）。
func attachmentDisposition(name string) string {
	return `attachment; filename*=UTF-8''` + url.PathEscape(name)
}

// statusError 把远程状态码映射为统一错误；错误信息不回显地址与凭据。
func statusError(status int) *apierr.Error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return apierr.New(apierr.CodeUnauthorized, "WebDAV 凭据无效或权限不足")
	case status == http.StatusNotFound:
		return apierr.New(apierr.CodeNotFound, "远程路径不存在")
	case status == http.StatusMethodNotAllowed:
		return apierr.New(apierr.CodeConflict, "远程路径类型冲突")
	case status == http.StatusConflict:
		return apierr.New(apierr.CodeConflict, "远程路径冲突（父目录可能不存在）")
	case status == http.StatusRequestEntityTooLarge:
		return apierr.New(apierr.CodePayloadTooLarge, "远程存储拒绝：文件过大")
	case status >= 500:
		return apierr.New(apierr.CodeUnavailable, "WebDAV 服务不可用")
	default:
		return apierr.Newf(apierr.CodeInternal, "WebDAV 服务返回异常状态 %d", status)
	}
}

// drain 丢弃响应体以便连接复用。
func drain(r io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(r, maxDrainBytes)) }

// writeErr 统一错误输出。
func writeErr(g *gin.Context, err error) {
	e := apierr.FromError(err)
	g.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}
