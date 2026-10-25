package attachments

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// ctxUserIDKey 是鉴权中间件写入 gin 上下文的用户标识键（见契约 2.7）。
const ctxUserIDKey = "user_id"

// maxRecordIDLen 是关联记录 id 的最大长度，与同步契约保持一致。
const maxRecordIDLen = 64

// allowedMIME 是客户端可声明的附件类型白名单。
//
// 注意：服务端只持有密文，**无法嗅探真实文件类型**，因此这里完全信任客户端
// 声明值，白名单只用于收敛取值范围（防止把任意字符串写库），不代表已校验内容。
var allowedMIME = map[string]bool{
	"image/jpeg":               true,
	"image/png":                true,
	"image/webp":               true,
	"application/octet-stream": true,
}

// Handler 承载附件相关的 HTTP 处理函数。
type Handler struct {
	svc *Service
}

// Mount 把附件路由挂到 rg（通常是 /api/v1）之下，并统一用 requireAuth 包装。
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc) {
	h := &Handler{svc: svc}
	g := rg.Group("/attachments")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	g.POST("", h.upload)
	g.GET("", h.list)
	g.GET("/:id", h.download)
	g.DELETE("/:id", h.delete)
}

// upload 处理 POST /api/v1/attachments：multipart/form-data 上传密文附件。
// 字段：file（二进制密文，必填）、iv（base64，12 字节，必填）、recordId（可选）、mime（可选）。
func (h *Handler) upload(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString(ctxUserIDKey))
	if userID == "" {
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期"))
		return
	}
	// 用 MaxBytesReader 在读取阶段就截断超长请求体，避免把超大内容读进内存。
	if max := h.svc.maxBytes(); max > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
	}
	if err := c.Request.ParseMultipartForm(1 << 20); err != nil {
		writeErr(c, mapBodyErr(err, "解析上传表单失败"))
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "缺少 file 字段"))
		return
	}
	ivB64 := strings.TrimSpace(c.Request.FormValue("iv"))
	if ivB64 == "" {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "缺少 iv 字段"))
		return
	}
	iv, err := decodeBase64(ivB64)
	if err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "iv 不是合法 base64"))
		return
	}
	if len(iv) != ivLen {
		writeErr(c, apierr.Newf(apierr.CodeBadRequest, "iv 必须为 %d 字节", ivLen))
		return
	}
	recordID := strings.TrimSpace(c.Request.FormValue("recordId"))
	if len(recordID) > maxRecordIDLen {
		writeErr(c, apierr.Newf(apierr.CodeBadRequest, "recordId 长度不能超过 %d", maxRecordIDLen))
		return
	}
	mime := pickMIME(c.Request.FormValue("mime"), fh.Header.Get("Content-Type"))
	if !allowedMIME[mime] {
		writeErr(c, apierr.Newf(apierr.CodeBadRequest, "不支持的附件类型：%s", mime))
		return
	}

	src, err := fh.Open()
	if err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeInternal, err, "打开上传内容失败"))
		return
	}
	defer src.Close()

	dto, err := h.svc.Save(c.Request.Context(), userID, recordID, iv, mime, src, fh.Size)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, dto)
}

// download 处理 GET /api/v1/attachments/:id：返回密文字节与 IV。
func (h *Handler) download(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString(ctxUserIDKey))
	if userID == "" {
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期"))
		return
	}
	a, rc, err := h.svc.Open(c.Request.Context(), userID, c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	defer rc.Close()

	name := pctEncode(a.ID + ".bin")
	c.DataFromReader(http.StatusOK, a.Size, "application/octet-stream", rc, map[string]string{
		"X-MedDesk-IV":           base64.StdEncoding.EncodeToString(a.IV),
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition": fmt.Sprintf(`attachment; filename="%s.bin"; filename*=UTF-8''%s`,
			a.ID, name),
	})
}

// delete 处理 DELETE /api/v1/attachments/:id。
func (h *Handler) delete(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString(ctxUserIDKey))
	if userID == "" {
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期"))
		return
	}
	if err := h.svc.Delete(c.Request.Context(), userID, c.Param("id")); err != nil {
		writeErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// list 处理 GET /api/v1/attachments?recordId=。
func (h *Handler) list(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString(ctxUserIDKey))
	if userID == "" {
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期"))
		return
	}
	items, err := h.svc.List(c.Request.Context(), userID, strings.TrimSpace(c.Query("recordId")))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, items)
}

// pickMIME 取客户端声明的类型：优先表单 mime 字段，其次文件部分 Content-Type，
// 都没有则退回 application/octet-stream。参数（;charset=...）会被去掉。
func pickMIME(formValue, contentType string) string {
	m := strings.TrimSpace(formValue)
	if m == "" {
		m = strings.TrimSpace(contentType)
	}
	if m == "" {
		return "application/octet-stream"
	}
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}

// decodeBase64 依次尝试标准、无填充与 URL 安全三种 base64 编码。
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// mapBodyErr 把请求体解析错误归一化为 API 错误：超限 → payload_too_large。
func mapBodyErr(err error, msg string) error {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return apierr.Newf(apierr.CodePayloadTooLarge, "请求体超过上限 %d 字节", mb.Limit)
	}
	return apierr.Wrap(apierr.CodeBadRequest, err, msg)
}

// writeErr 统一错误响应：HTTP 状态码由 apierr.HTTPStatus 决定。
func writeErr(c *gin.Context, err error) {
	e := apierr.FromError(err)
	c.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}

// pctEncode 按 RFC 5987 对 filename* 做百分号编码（只保留 unreserved 字符）。
func pctEncode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
			continue
		}
		for _, bb := range []byte(string(r)) {
			fmt.Fprintf(&b, "%%%02X", bb)
		}
	}
	return b.String()
}
