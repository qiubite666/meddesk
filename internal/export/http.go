package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
)

// ctxUserIDKey 是鉴权中间件写入 gin 上下文的用户标识键（见契约 2.7）。
const ctxUserIDKey = "user_id"

// maxExportBody 是导出请求体上限：4 MiB（明文行数据由客户端一次性 POST 过来）。
const maxExportBody = 4 << 20

// Handler 承载导出相关的 HTTP 处理函数。
type Handler struct {
	svc *Service
}

// Mount 把导出路由挂到 rg（通常是 /api/v1）之下，并统一用 requireAuth 包装。
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc) {
	h := &Handler{svc: svc}
	g := rg.Group("/export")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	g.POST("/csv", h.csv)
	g.POST("/pdf", h.pdf)
}

// csv 处理 POST /api/v1/export/csv。
func (h *Handler) csv(c *gin.Context) {
	h.render(c, "csv", h.svc.RenderCSV)
}

// pdf 处理 POST /api/v1/export/pdf。
func (h *Handler) pdf(c *gin.Context) {
	h.render(c, "pdf", h.svc.RenderPDF)
}

// render 是两种格式的公共流程：限流大小的请求体 → 内存渲染 → 带附件头输出。
// 全过程不落盘、不记日志（见包注释的隐私说明）。
func (h *Handler) render(c *gin.Context, format string, renderFn func(io.Writer, model.ExportRequest) error) {
	if strings.TrimSpace(c.GetString(ctxUserIDKey)) == "" {
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期"))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxExportBody)

	// 这里用 json.Decoder 而不是 c.ShouldBindJSON：model.ExportRequest 的 Format
	// 带 binding:"required"，而格式已由路由决定，不应强制客户端再传一次。
	var req model.ExportRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		writeErr(c, mapBodyErr(err, "请求体不是合法的 JSON"))
		return
	}
	req.Format = format

	var buf bytes.Buffer
	if err := renderFn(&buf, req); err != nil {
		writeErr(c, err)
		return
	}

	stamp := time.Now().Format("20060102-1504")
	ascii := "meddesk-" + stamp + "." + format
	utf8 := "用药历史导出-" + stamp + "." + format
	ctype := contentType(format)
	c.Header("Content-Type", ctype)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		ascii, pctEncode(utf8)))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, ctype, buf.Bytes())
}

// contentType 返回导出格式的响应类型。
func contentType(format string) string {
	if format == "csv" {
		return "text/csv; charset=utf-8"
	}
	return "application/pdf"
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
