package ledger

// 记账本导出/导入的 HTTP 层。
//
// 【账号归属】一律从 gin 上下文的 user_id 取（由 RequireAuth 中间件写入），
// 请求体/查询串里任何 user_id 都不采信——导入时更是会强制改写成当前账号。
//
// 【空结果】导出 API 在没有命中账单时返回 400「没有符合条件的账单，未生成导出文件」，
// 而不是给一个空壳文件：空文件会被误当成"导出成功但数据丢了"。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// exportHandler 承载导出/导入的 HTTP 处理函数。
type exportHandler struct{ svc *ExportService }

// MountExport 把导出/导入路由挂到 rg（/api/v1）之下，全部要求登录态。
//
// 与 Mount 分开是为了让调用方可以只挂导出（例如备份任务所在进程），
// 也避免两个子任务在同一个文件里互相改冲突。
func MountExport(rg *gin.RouterGroup, svc *ExportService, requireAuth gin.HandlerFunc) {
	if rg == nil || svc == nil {
		return
	}
	h := &exportHandler{svc: svc}
	g := rg.Group("/ledger")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	g.POST("/export/html", h.exportHTML)
	g.POST("/export/zip", h.exportZip)
	g.GET("/export/json", h.exportJSON)
	g.POST("/import", h.importData)
}

// ------------------------------------------------------------------ 筛选条件

// exportRequest 是 POST 导出请求体；IncludeAttachments 用指针以区分「未传」与「显式 false」，
// 未传时默认内嵌附件（备份完整性优先）。
type exportRequest struct {
	ExportFilter
	IncludeAttachments *bool `json:"includeAttachments"`
}

// bindExportFilter 解析筛选条件：POST 走请求体，GET（或无 body）走查询串。
func bindExportFilter(c *gin.Context) (ExportFilter, error) {
	if hasBody(c) {
		var req exportRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			return ExportFilter{}, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误")
		}
		f := req.ExportFilter
		if req.IncludeAttachments != nil {
			f.IncludeAttachments = *req.IncludeAttachments
		} else {
			f.IncludeAttachments = true
		}
		return f, nil
	}
	return filterFromExportQuery(c), nil
}

// hasBody 判断请求是否带了 JSON body（GET /export/json 没有 body，走查询串）。
func hasBody(c *gin.Context) bool {
	if c.Request.Body == nil {
		return false
	}
	if c.Request.ContentLength > 0 {
		return true
	}
	// chunked 传输时 ContentLength 为 -1：只看方法，POST/PUT/PATCH 视为有 body。
	return c.Request.ContentLength < 0 &&
		(c.Request.Method == http.MethodPost || c.Request.Method == http.MethodPut || c.Request.Method == http.MethodPatch)
}

// filterFromExportQuery 解析查询串形式的筛选条件（GET /export/json 用）。
func filterFromExportQuery(c *gin.Context) ExportFilter {
	f := ExportFilter{
		Preset:  strings.TrimSpace(c.Query("preset")),
		Type:    strings.TrimSpace(c.Query("type")),
		Keyword: strings.TrimSpace(c.Query("keyword")),
		From:    strings.TrimSpace(c.Query("from")),
		To:      strings.TrimSpace(c.Query("to")),
		ReimbOnly: c.Query("reimbOnly") == "1" ||
			strings.EqualFold(c.Query("reimbOnly"), "true"),
		IncludeAttachments: true,
	}
	if v := strings.TrimSpace(c.Query("includeAttachments")); v != "" {
		f.IncludeAttachments = !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no"))
	}
	f.CategoryIDs = splitList(c.Query("categoryIds"), c.Query("categoryId"))
	f.IDs = splitList(c.Query("ids"))
	// 兼容既有统计接口的 month=YYYY-MM 写法：转成自定义区间。
	if m := strings.TrimSpace(c.Query("month")); m != "" && f.From == "" && f.To == "" {
		f.From, f.To = MonthRange(m)
	}
	return f
}

// splitList 把逗号分隔的多个参数值合并成一个列表。
func splitList(values ...string) []string {
	out := []string{}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ 导出

func (h *exportHandler) exportHTML(c *gin.Context) {
	f, err := bindExportFilter(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	name, body, err := h.svc.BuildHTML(c.Request.Context(), userIDOf(c), f)
	if err != nil {
		writeErr(c, err)
		return
	}
	sendFile(c, name, "text/html; charset=utf-8", body)
}

func (h *exportHandler) exportZip(c *gin.Context) {
	f, err := bindExportFilter(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	name, body, err := h.svc.BuildZIP(c.Request.Context(), userIDOf(c), f)
	if err != nil {
		writeErr(c, err)
		return
	}
	sendFile(c, name, "application/zip", body)
}

func (h *exportHandler) exportJSON(c *gin.Context) {
	f, err := bindExportFilter(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	name, payload, err := h.svc.BuildJSON(c.Request.Context(), userIDOf(c), f)
	if err != nil {
		writeErr(c, err)
		return
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeInternal, err, "生成备份失败"))
		return
	}
	sendFile(c, name, "application/json; charset=utf-8", body)
}

// ------------------------------------------------------------------ 导入

func (h *exportHandler) importData(c *gin.Context) {
	var req ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	res, err := h.svc.Import(c.Request.Context(), userIDOf(c), req)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ------------------------------------------------------------------ 工具

// sendFile 以附件形式返回文件（文件名全为 ASCII，直接放进 header 即可）。
func sendFile(c *gin.Context, filename, contentType string, body []byte) {
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("Content-Length", strconv.Itoa(len(body)))
	c.Data(http.StatusOK, contentType, body)
}
