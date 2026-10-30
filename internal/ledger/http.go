package ledger

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
)

// ctxUserIDKey 与鉴权中间件写入 gin 上下文的键保持一致（见契约 2.7）。
const ctxUserIDKey = "user_id"

// Handler 承载记账本的 HTTP 处理函数。
type Handler struct{ svc *Service }

// Mount 把记账本路由挂到 rg（/api/v1）之下，全部要求登录态。
//
// 账号一律取自会话（RequireAuth 写入上下文），**不接受请求体里的 user_id**。
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc) {
	h := &Handler{svc: svc}
	g := rg.Group("/ledger")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	// 分类
	g.GET("/categories", h.listCategories)
	g.POST("/categories", h.createCategory)
	g.PATCH("/categories/:id", h.updateCategory)
	g.DELETE("/categories/:id", h.deleteCategory)
	// 账单
	g.GET("/records", h.listRecords)
	g.POST("/records", h.createRecord)
	g.GET("/records/:id", h.getRecord)
	g.PATCH("/records/:id", h.updateRecord)
	g.DELETE("/records/:id", h.deleteRecord)
	// 统计
	g.GET("/summary", h.summary)
	g.GET("/trend", h.trend)
	g.GET("/breakdown", h.breakdown)
	// 报销
	g.GET("/reimbursements", h.listReimbursements)
	g.POST("/reimbursements/mark", h.markReimbursement)
}

func userIDOf(c *gin.Context) string { return strings.TrimSpace(c.GetString(ctxUserIDKey)) }

func writeErr(c *gin.Context, err error) {
	e := apierr.FromError(err)
	c.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}

// ------------------------------------------------------------------ 分类

func (h *Handler) listCategories(c *gin.Context) {
	userID := userIDOf(c)
	kind := strings.TrimSpace(c.Query("kind"))
	includeArchived := c.Query("archived") == "1" || c.Query("archived") == "true"
	list, err := h.svc.ListCategories(c.Request.Context(), userID, kind, includeArchived)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list, "total": len(list)})
}

func (h *Handler) createCategory(c *gin.Context) {
	var in CategoryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	out, err := h.svc.CreateCategory(c.Request.Context(), userIDOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) updateCategory(c *gin.Context) {
	var in CategoryPatch
	if err := c.ShouldBindJSON(&in); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	out, err := h.svc.UpdateCategory(c.Request.Context(), userIDOf(c), c.Param("id"), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) deleteCategory(c *gin.Context) {
	if err := h.svc.DeleteCategory(c.Request.Context(), userIDOf(c), c.Param("id")); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ------------------------------------------------------------------ 账单

// filterFromQuery 解析筛选参数。区间缺省为"本月"。
func filterFromQuery(c *gin.Context) model.LedgerFilter {
	f := model.LedgerFilter{
		Type:        strings.TrimSpace(c.Query("type")),
		Keyword:     strings.TrimSpace(c.Query("keyword")),
		ReimbStatus: strings.TrimSpace(c.Query("reimbStatus")),
		From:        strings.TrimSpace(c.Query("from")),
		To:          strings.TrimSpace(c.Query("to")),
	}
	// 分类多选同时支持 categoryId=a,b,c 与 categoryIds=a,b,c（后者语义更明确）。
	for _, key := range []string{"categoryId", "categoryIds"} {
		for _, part := range strings.Split(c.Query(key), ",") {
			if strings.TrimSpace(part) != "" {
				f.CategoryIDs = append(f.CategoryIDs, strings.TrimSpace(part))
			}
		}
	}
	if v := strings.TrimSpace(c.Query("ids")); v != "" {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) != "" {
				f.IDs = append(f.IDs, strings.TrimSpace(part))
			}
		}
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	if f.Page < 1 {
		f.Page = 1
	}
	f.Size, _ = strconv.Atoi(c.Query("size"))
	if f.Size <= 0 {
		f.Size = 20
	}
	if f.Size > 200 {
		f.Size = 200
	}
	return f
}

// rangeFromQuery 返回统计区间：默认本月；传了 from/to 则以它们为准。
func rangeFromQuery(c *gin.Context) (from, to string) {
	f := filterFromQuery(c)
	if f.From != "" && f.To != "" {
		return f.From, f.To
	}
	if m := strings.TrimSpace(c.Query("month")); m != "" {
		return MonthRange(m)
	}
	return MonthRange("")
}

func (h *Handler) listRecords(c *gin.Context) {
	f := filterFromQuery(c)
	list, total, err := h.svc.ListRecords(c.Request.Context(), userIDOf(c), f)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list, "total": total, "page": f.Page, "size": f.Size})
}

func (h *Handler) createRecord(c *gin.Context) {
	var in RecordInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	out, err := h.svc.CreateRecord(c.Request.Context(), userIDOf(c), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) getRecord(c *gin.Context) {
	out, err := h.svc.GetRecord(c.Request.Context(), userIDOf(c), c.Param("id"))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) updateRecord(c *gin.Context) {
	var in RecordInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	out, err := h.svc.UpdateRecord(c.Request.Context(), userIDOf(c), c.Param("id"), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) deleteRecord(c *gin.Context) {
	if err := h.svc.DeleteRecord(c.Request.Context(), userIDOf(c), c.Param("id")); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ------------------------------------------------------------------ 统计

func (h *Handler) summary(c *gin.Context) {
	from, to := rangeFromQuery(c)
	out, err := h.svc.Summary(c.Request.Context(), userIDOf(c), filterFromQuery(c), from, to)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) trend(c *gin.Context) {
	// 趋势固定"最近 7 个自然日（含今天）"，与需求一致；也允许显式传 days。
	days, _ := strconv.Atoi(c.Query("days"))
	if days <= 0 {
		days = 7
	}
	from, to := RecentDaysRange(days)
	out, err := h.svc.Trend(c.Request.Context(), userIDOf(c), filterFromQuery(c), from, to)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"from": from, "to": to, "days": out})
}

func (h *Handler) breakdown(c *gin.Context) {
	from, to := rangeFromQuery(c)
	income, expense, err := h.svc.Breakdown(c.Request.Context(), userIDOf(c), filterFromQuery(c), from, to)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"from": from, "to": to, "income": income, "expense": expense})
}

// ------------------------------------------------------------------ 报销

func (h *Handler) listReimbursements(c *gin.Context) {
	list, total, sum, err := h.svc.ListPending(c.Request.Context(), userIDOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list, "total": total, "pendingTotal": sum})
}

// markRequest 是批量改报销状态的请求体。
type markRequest struct {
	IDs    []string `json:"ids"`
	Status string   `json:"status"`
}

func (h *Handler) markReimbursement(c *gin.Context) {
	var req markRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	n, err := h.svc.MarkReimbursement(c.Request.Context(), userIDOf(c), req.IDs, strings.TrimSpace(req.Status))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": n})
}

// ErrNoRows 供上层判断"空结果"（导出时用于提示用户而非生成空文件）。
var ErrNoRows = errors.New("没有符合条件的账单")
