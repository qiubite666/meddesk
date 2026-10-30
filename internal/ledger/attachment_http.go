package ledger

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// maxUploadBodyBytes 是整个 multipart 请求体的上限：单张凭证上限 + 给边界与
// 其他表单字段预留的余量。
//
// 为什么不是"正好 2 MiB"：MaxBytesReader 限制的是**整个请求体**（含 boundary、
// recordId 字段等），卡死在 2 MiB 会让刚好 2 MiB 的图片因为多出几十字节而被拒。
// 余量之外仍有服务层的精确校验（按文件字节数判断），两道闸缺一不可。
const maxUploadBodyBytes = maxAttachmentBytes + 64<<10

// attachmentHandler 承载凭证附件的 HTTP 处理函数。
type attachmentHandler struct{ svc *AttachmentService }

// MountAttachments 把凭证附件路由挂到 rg 之下（rg 通常是 /api/v1）。
//
// 路由：POST|GET /api/v1/ledger/attachments、GET|DELETE /api/v1/ledger/attachments/:id。
// 与 Mount 一致，账号一律取自会话，不接受请求体里的 user_id。
func MountAttachments(rg *gin.RouterGroup, svc *AttachmentService, requireAuth gin.HandlerFunc) {
	h := &attachmentHandler{svc: svc}
	g := rg.Group("/ledger/attachments")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	g.POST("", h.upload)
	g.GET("", h.listAttachments)
	g.GET("/:id", h.previewAttachment)
	g.DELETE("/:id", h.deleteAttachment)
}

// upload 处理 POST /api/v1/ledger/attachments（multipart：file + recordId）。
func (h *attachmentHandler) upload(c *gin.Context) {
	userID := userIDOf(c)
	if userID == "" {
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期"))
		return
	}
	// 在读阶段就截断请求体：先把超大请求挡在内存之外，再谈业务校验。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBodyBytes)
	if err := c.Request.ParseMultipartForm(1 << 20); err != nil {
		writeErr(c, mapUploadErr(err))
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "缺少 file 字段"))
		return
	}
	recordID := strings.TrimSpace(c.Request.FormValue("recordId"))
	if recordID == "" {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "缺少 recordId"))
		return
	}
	src, err := fh.Open()
	if err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeInternal, err, "打开上传内容失败"))
		return
	}
	defer src.Close()

	// 多读 1 字节用于发现"超过上限"，避免为判断大小多读整个文件到内存。
	data, err := io.ReadAll(io.LimitReader(src, maxAttachmentBytes+1))
	if err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "读取上传内容失败"))
		return
	}
	if int64(len(data)) > maxAttachmentBytes {
		writeErr(c, apierr.Newf(apierr.CodePayloadTooLarge, "单张凭证不能超过 %d 字节", maxAttachmentBytes))
		return
	}

	// 真实类型由服务层按魔数嗅探，这里不读也不信客户端声明的 Content-Type。
	out, err := h.svc.Save(c.Request.Context(), userID, recordID, data)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

// listAttachments 处理 GET /api/v1/ledger/attachments?recordId=xxx。
func (h *attachmentHandler) listAttachments(c *gin.Context) {
	items, err := h.svc.List(c.Request.Context(), userIDOf(c), strings.TrimSpace(c.Query("recordId")))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// previewAttachment 处理 GET /api/v1/ledger/attachments/:id（图片预览/下载）。
func (h *attachmentHandler) previewAttachment(c *gin.Context) {
	a, rc, err := h.svc.OpenMeta(c.Request.Context(), userIDOf(c), c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	defer rc.Close()

	// 凭证属于个人财务隐私：禁止任何中间层与浏览器缓存。
	// nosniff 防止浏览器把图片当 HTML 猜类型（历史上 CVE-2016-3718 一类的绕过）。
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", `inline; filename="`+a.ID+"."+extOfMime(a.Mime)+`"`)
	c.DataFromReader(http.StatusOK, a.Bytes, a.Mime, rc, nil)
}

// deleteAttachment 处理 DELETE /api/v1/ledger/attachments/:id。
func (h *attachmentHandler) deleteAttachment(c *gin.Context) {
	if err := h.svc.Delete(c.Request.Context(), userIDOf(c), c.Param("id")); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// mapUploadErr 把 multipart 解析错误归一化：超限 → 413，其余 → 400。
func mapUploadErr(err error) error {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return apierr.Newf(apierr.CodePayloadTooLarge, "上传内容超过上限 %d 字节", maxAttachmentBytes)
	}
	return apierr.Wrap(apierr.CodeBadRequest, err, "解析上传表单失败")
}
