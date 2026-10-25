package syncsvc

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// 上下文键，与 middleware/auth 约定保持一致。
const (
	ctxKeyUserID    = "user_id"
	ctxKeyRequestID = "request_id"
)

// Handler 承载同步相关 HTTP 接口。
type Handler struct {
	svc *Service
}

// NewHandler 构造同步 HTTP 处理器。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Mount 将同步路由挂到 /api/v1 路由组下，所有接口强制经过传入的鉴权中间件。
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc) {
	h := NewHandler(svc)
	g := rg.Group("/sync")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	g.POST("/push", h.push)
	g.GET("/pull", h.pull)
	g.GET("/stats", h.stats)
	g.POST("/reset", h.reset)
}

// resetRequest 是清空同步数据的请求体。
type resetRequest struct {
	Confirm string `json:"confirm"`
}

// push 处理 POST /api/v1/sync/push。
func (h *Handler) push(c *gin.Context) {
	userID := c.GetString(ctxKeyUserID)
	if userID == "" {
		writeError(c, apierr.New(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req model.SyncPushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.New(apierr.CodeBadRequest, "请求体格式错误"))
		return
	}
	resp, err := h.svc.Push(c.Request.Context(), userID, req)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// pull 处理 GET /api/v1/sync/pull?cursor=&limit=。
func (h *Handler) pull(c *gin.Context) {
	userID := c.GetString(ctxKeyUserID)
	if userID == "" {
		writeError(c, apierr.New(apierr.CodeUnauthorized, "未认证"))
		return
	}
	cursor, limit, err := parsePullParams(c, h.svc.cfg)
	if err != nil {
		writeError(c, err)
		return
	}
	resp, err := h.svc.Pull(c.Request.Context(), userID, cursor, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// stats 处理 GET /api/v1/sync/stats。
func (h *Handler) stats(c *gin.Context) {
	userID := c.GetString(ctxKeyUserID)
	if userID == "" {
		writeError(c, apierr.New(apierr.CodeUnauthorized, "未认证"))
		return
	}
	resp, err := h.svc.Stats(c.Request.Context(), userID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// reset 处理 POST /api/v1/sync/reset，必须提交确认词 DELETE-ALL。
func (h *Handler) reset(c *gin.Context) {
	userID := c.GetString(ctxKeyUserID)
	if userID == "" {
		writeError(c, apierr.New(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req resetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.New(apierr.CodeBadRequest, "请求体格式错误"))
		return
	}
	if req.Confirm != resetConfirmWord {
		writeError(c, apierr.Newf(apierr.CodeBadRequest, "清空云端同步数据需提交确认词 %s", resetConfirmWord))
		return
	}
	if err := h.svc.Reset(c.Request.Context(), userID); err != nil {
		writeError(c, err)
		return
	}
	slog.Warn("已清空用户云端同步数据",
		"user_id", userID,
		"request_id", c.GetString(ctxKeyRequestID),
	)
	c.JSON(http.StatusOK, gin.H{"ok": true, "cursor": 0})
}

// parsePullParams 解析 pull 查询参数：cursor 缺省 0，limit 缺省 cfg.PullPageSize；
// 非法值返回 CodeBadRequest（limit 超上限由服务层钳制）。
func parsePullParams(c *gin.Context, cfg config.SyncConfig) (int64, int, error) {
	var cursor int64
	if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			return 0, 0, apierr.New(apierr.CodeBadRequest, "cursor 必须是非负整数")
		}
		cursor = v
	}
	limit := cfg.PullPageSize
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, apierr.New(apierr.CodeBadRequest, "limit 必须是正整数")
		}
		limit = v
	}
	return cursor, limit, nil
}

// writeError 统一输出错误响应：内部错误只留通用文案并写日志，细节不外泄。
func writeError(c *gin.Context, err error) {
	ae := apierr.FromError(err)
	if ae.Code == apierr.CodeInternal {
		slog.Error("同步接口内部错误",
			"request_id", c.GetString(ctxKeyRequestID),
			"error", err,
		)
		ae.Details = nil
	}
	c.JSON(apierr.HTTPStatus(ae.Code), ae)
}
