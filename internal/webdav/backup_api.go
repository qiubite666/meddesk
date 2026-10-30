package webdav

// 记账本每日自动备份所需的客户端能力（B7）。
//
// 【与 webdav.go 的关系】
// 原文件的定位是"浏览器无法直连 WebDAV，由服务端代理转发，凭据随单次请求带来、不落盘"。
// 备份是另一种场景：凭据由用户**加密持久化**在数据库里（用户明确批准），服务端定时
// 取出解密后使用。两者的安全要求其实一致——凭据只在调用栈里出现、不写全局状态、不进日志——
// 因此这里不重复造轮子，也不改动 webdav.go，而是在同包新文件里给 *Client 加导出方法，
// 直接复用已有的 do / ensureDir / ensureBase / buildTarget / statusError，
// 从而完整继承 https 强制、SSRF 拦截、路径穿越防护与 15s 超时。

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// maxUploadBytes 是单次上传的内存上限（256 MiB）。
//
// 为什么先把内容读进内存：do() 最终调用 http.NewRequestWithContext，只有 *bytes.Reader
// 这类具名类型才能算出 Content-Length，否则请求会退化成 chunked 编码，
// 而部分 WebDAV 服务（典型如 nginx 的 dav 模块）会直接拒绝 chunked PUT。
// 代价是内存占用，因此这里设上限并在超限时明确报错（而不是 OOM）。
const maxUploadBytes = 256 << 20

// NewClient 返回使用默认安全策略的客户端（强制 https、拦截内网地址、禁止跟随重定向）。
//
// 客户端本身**不持有任何凭据**：凭据由每次调用显式传入（备份场景下来自解密后的用户配置）。
func NewClient() *Client { return newClient() }

// NewClientForTest 仅供单元测试：注入 http.Client（用于信任 httptest 的自签证书）
// 与 IP 拦截策略（放行 127.0.0.1）。
//
// 【不要在业务代码里调用它】denyIP 传 nil 才回落到生产策略；放行内网等于拆掉 SSRF 防护。
func NewClientForTest(hc *http.Client, denyIP func(net.IP) bool) *Client {
	c := newClient()
	if hc != nil && hc.Transport != nil {
		// 只替换 Transport：保留 newClient 设置的超时与"禁止跟随重定向"策略。
		c.http.Transport = hc.Transport
	}
	if denyIP != nil {
		c.denyIP = denyIP
	}
	return c
}

// Ping 校验凭据是否有效、根目录是否可达（PROPFIND Depth: 0）。
//
// 404（根目录尚不存在）**不算失败**：备份时会先 MKCOL 逐级创建，
// 因此"连得上且认证通过"就足够了；401/403/5xx 才是凭据或服务的真实问题。
func (c *Client) Ping(ctx context.Context, creds Credentials) error {
	tgt, err := c.buildTarget(ctx, creds, "")
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, "PROPFIND", tgt.URL, creds, strings.NewReader(propfindBody),
		map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	drain(resp.Body)
	switch {
	case code >= 200 && code < 300, code == http.StatusNotFound:
		slog.Info("WebDAV 连通性校验通过", "method", "PROPFIND", "path", tgt.Rel, "status", code)
		return nil
	default:
		return statusError(code)
	}
}

// Mkdir 逐级创建目录（相对 baseURL）；已存在视为成功（幂等）。
func (c *Client) Mkdir(ctx context.Context, creds Credentials, rel string) error {
	// buildTarget 先跑一遍：拒绝穿越/反斜杠等非法路径，避免拿到 MKCOL 才报错。
	if _, err := c.buildTarget(ctx, creds, rel); err != nil {
		return err
	}
	return c.ensureDir(ctx, creds, rel)
}

// Exists 判断远程路径是否存在（PROPFIND Depth: 0）。
func (c *Client) Exists(ctx context.Context, creds Credentials, rel string) (bool, error) {
	tgt, err := c.buildTarget(ctx, creds, rel)
	if err != nil {
		return false, err
	}
	resp, err := c.do(ctx, "PROPFIND", tgt.URL, creds, strings.NewReader(propfindBody),
		map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	drain(resp.Body)
	switch {
	case code == http.StatusNotFound:
		return false, nil
	case code >= 200 && code < 300:
		return true, nil
	default:
		return false, statusError(code)
	}
}

// Upload 把 body 上传到 relPath（相对 baseURL），父目录自动创建，同名文件直接覆盖。
//
// size 是预期字节数（已知则传入，用于提前拦截超限；未知传 0 或负数）。
func (c *Client) Upload(ctx context.Context, creds Credentials, relPath string, body io.Reader, size int64) error {
	tgt, err := c.buildTarget(ctx, creds, relPath)
	if err != nil {
		return err
	}
	rel := tgt.Rel
	if parent := path.Dir(rel); parent != "." && parent != "/" {
		if err := c.ensureDir(ctx, creds, parent); err != nil {
			return err
		}
	}
	limit := size
	if limit <= 0 || limit > maxUploadBytes {
		limit = maxUploadBytes
	}
	// 多读一个字节才能区分"正好等于上限"与"超过上限"。
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "读取待上传内容失败")
	}
	if int64(len(buf)) > limit {
		return apierr.Newf(apierr.CodePayloadTooLarge, "备份文件超过上传上限 %d 字节", limit)
	}
	resp, err := c.do(ctx, http.MethodPut, tgt.URL, creds, bytes.NewReader(buf), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	drain(resp.Body)
	if code/100 != 2 {
		return statusError(code)
	}
	// 日志只输出方法、相对路径与状态码：不含主机、不含凭据。
	slog.Info("WebDAV 上传完成", "method", http.MethodPut, "path", rel, "status", code)
	return nil
}
