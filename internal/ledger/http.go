package ledger

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
)

// ctxUserIDKey 与鉴权中间件写入 gin 上下文的 key 保持一致（见契约 2.7）。
const ctxUserIDKey = "user_id"

// maxTrendDays 是 /ledger/trend 允许的最大天数（含闰年在内的整年）。
const maxTrendDays = 366

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
//
// 【为什么这里必须校验日期】from/to 会被仓储拼进 SQL 的 ::date 表达式，
// 非法值交给 PostgreSQL 只会得到 22007（invalid input syntax for type date），
// 再被包成 500；用户看到的不是"日期写错了"而是"服务器内部错误"。
// 因此统一按 2006-01-02 严格解析（time.Parse 会连带校验 2 月 30 日这类假日期），
// 并保证 from <= to。
//
// 【为什么 id 也在这里校验】分类/账单 id 会变成 uuid[] 参数；非法 UUID 在仓储层
// 报 22P02 同样会变成 500。在入口就转成 400，错误信息也更明确。
func filterFromQuery(c *gin.Context) (model.LedgerFilter, error) {
	f := model.LedgerFilter{
		Type:        strings.TrimSpace(c.Query("type")),
		Keyword:     strings.TrimSpace(c.Query("keyword")),
		ReimbStatus: strings.TrimSpace(c.Query("reimbStatus")),
		From:        strings.TrimSpace(c.Query("from")),
		To:          strings.TrimSpace(c.Query("to")),
	}
	if f.From != "" {
		if _, err := time.Parse("2006-01-02", f.From); err != nil {
			return model.LedgerFilter{}, apierr.Newf(apierr.CodeBadRequest, "from 应为 YYYY-MM-DD，收到 %q", f.From)
		}
	}
	if f.To != "" {
		if _, err := time.Parse("2006-01-02", f.To); err != nil {
			return model.LedgerFilter{}, apierr.Newf(apierr.CodeBadRequest, "to 应为 YYYY-MM-DD，收到 %q", f.To)
		}
	}
	if f.From != "" && f.To != "" && f.From > f.To {
		return model.LedgerFilter{}, apierr.New(apierr.CodeBadRequest, "开始日期不能晚于结束日期")
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
	// 校验后顺手规范化成标准 36 位形式：google/uuid 也接受 urn:{...} 等变体，
	// 而仓储侧的 uuid 解析只认标准写法，规范化后两边的判断一定一致。
	for i, id := range f.CategoryIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			return model.LedgerFilter{}, apierr.Newf(apierr.CodeBadRequest, "分类 id %q 不是合法 UUID", id)
		}
		f.CategoryIDs[i] = u.String()
	}
	for i, id := range f.IDs {
		u, err := uuid.Parse(id)
		if err != nil {
			return model.LedgerFilter{}, apierr.Newf(apierr.CodeBadRequest, "账单 id %q 不是合法 UUID", id)
		}
		f.IDs[i] = u.String()
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
	return f, nil
}

// rangeFromQuery 返回统计区间：默认本月；传了 from/to 则以它们为准。
func rangeFromQuery(c *gin.Context) (from, to string, err error) {
	f, err := filterFromQuery(c)
	if err != nil {
		return "", "", err
	}
	if f.From != "" && f.To != "" {
		return f.From, f.To, nil
	}
	if m := strings.TrimSpace(c.Query("month")); m != "" {
		from, to = MonthRange(m)
		return from, to, nil
	}
	from, to = MonthRange("")
	return from, to, nil
}

func (h *Handler) listRecords(c *gin.Context) {
	f, err := filterFromQuery(c)
	if err != nil {
		writeErr(c, err)
		return
	}
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
	from, to, err := rangeFromQuery(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	f, err := filterFromQuery(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	out, err := h.svc.Summary(c.Request.Context(), userIDOf(c), f, from, to)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) trend(c *gin.Context) {
	// 趋势固定"最近 7 个自然日（含今天）"，与需求一致；也允许显式传 days。
	//
	// days 必须夹取上限：一个 GET 就能让 generate_series 展开上百万行，
	// 属于无需登录成本的低成本资源耗尽。366 天（含闰年整年）足够业务使用。
	days, _ := strconv.Atoi(c.Query("days"))
	if days <= 0 {
		days = 7
	}
	if days > maxTrendDays {
		days = maxTrendDays
	}
	f, err := filterFromQuery(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	from, to := RecentDaysRange(days)
	out, err := h.svc.Trend(c.Request.Context(), userIDOf(c), f, from, to)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"from": from, "to": to, "days": out})
}

func (h *Handler) breakdown(c *gin.Context) {
	from, to, err := rangeFromQuery(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	f, err := filterFromQuery(c)
	if err != nil {
		writeErr(c, err)
		return
	}
	income, expense, err := h.svc.Breakdown(c.Request.Context(), userIDOf(c), f, from, to)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"from": from, "to": to, "income": income, "expense": expense})
}

// ------------------------------------------------------------------ 报销

// reimbQueryFromQuery 解析报销列表参数。
//
// status 取值、日期格式、from<=to 的校验统一放在 service 层，这样无论入口是
// HTTP 还是后台任务，规则都只有一份；这里只做数字解析（解析失败即视为未传，
// 由 service 的默认值兜底，与 /records 的夹取风格一致）。
func reimbQueryFromQuery(c *gin.Context) ReimbQuery {
	q := ReimbQuery{
		Status: strings.TrimSpace(c.Query("status")),
		From:   strings.TrimSpace(c.Query("from")),
		To:     strings.TrimSpace(c.Query("to")),
	}
	q.Days, _ = strconv.Atoi(c.Query("days"))
	q.Page, _ = strconv.Atoi(c.Query("page"))
	q.Size, _ = strconv.Atoi(c.Query("size"))
	return q
}

func (h *Handler) listReimbursements(c *gin.Context) {
	out, err := h.svc.ListReimbursements(c.Request.Context(), userIDOf(c), reimbQueryFromQuery(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
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
