package auth

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
)

// Handler 承载 /api/v1/auth/* 的 HTTP 处理函数。
type Handler struct {
	svc *Service
}

// NewHandler 构造认证 HTTP 处理器。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Mount 在传入的路由组（即 /api/v1）下挂载 /auth/* 子路由。
// 需要鉴权的路由在内部自行用 RequireAuth 包装。
func Mount(rg *gin.RouterGroup, svc *Service) {
	h := NewHandler(svc)
	auth := rg.Group("/auth")
	{
		auth.POST("/register", h.register)
		auth.POST("/login", h.login)
		auth.POST("/salt", h.salt)
		auth.POST("/refresh", h.refresh)
		auth.POST("/logout", h.logout)
		auth.POST("/logout-all", RequireAuth(svc), h.logoutAll)
		auth.POST("/change-password", RequireAuth(svc), h.changePassword)
		auth.GET("/me", RequireAuth(svc), h.me)
	}
}

// register 处理用户注册。
func (h *Handler) register(c *gin.Context) {
	var req model.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	resp, err := h.svc.Register(c.Request.Context(), req, metaFromRequest(c, req.Device))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// login 处理用户登录。
func (h *Handler) login(c *gin.Context) {
	var req model.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	resp, err := h.svc.Login(c.Request.Context(), req, metaFromRequest(c, req.Device))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// salt 处理新设备登录的盐值握手（防枚举：不存在也返回随机盐）。
func (h *Handler) salt(c *gin.Context) {
	var req model.SaltRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	salt, err := h.svc.AccountSalt(c.Request.Context(), req.Email)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, model.SaltResponse{KDFSalt: salt})
}

// refresh 处理令牌刷新（轮换）。
func (h *Handler) refresh(c *gin.Context) {
	var req model.RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	resp, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken, metaFromRequest(c, req.Device))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// logout 处理单设备登出。
func (h *Handler) logout(c *gin.Context) {
	var req model.LogoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	if err := h.svc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// logoutAll 处理全部设备登出。
func (h *Handler) logoutAll(c *gin.Context) {
	userID, ok := UserID(c)
	if !ok {
		writeError(c, apierr.New(apierr.CodeUnauthorized, msgSessionExpired))
		return
	}
	if err := h.svc.LogoutAll(c.Request.Context(), userID); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// changePassword 处理修改口令与重新包裹 DEK。
func (h *Handler) changePassword(c *gin.Context) {
	userID, ok := UserID(c)
	if !ok {
		writeError(c, apierr.New(apierr.CodeUnauthorized, msgSessionExpired))
		return
	}
	var req model.ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	if err := h.svc.ChangePassword(c.Request.Context(), userID, req); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// me 返回当前用户信息。
func (h *Handler) me(c *gin.Context) {
	userID, ok := UserID(c)
	if !ok {
		writeError(c, apierr.New(apierr.CodeUnauthorized, msgSessionExpired))
		return
	}
	u, err := h.svc.users.GetByID(c.Request.Context(), userID)
	if err != nil {
		writeError(c, apierr.Wrap(apierr.CodeInternal, err, msgInternal))
		return
	}
	c.JSON(http.StatusOK, model.MeResponse{
		User: model.UserDTO{
			ID:        u.ID,
			Email:     u.Email,
			CreatedAt: u.CreatedAt,
		},
		ServerTime: time.Now().UnixMilli(),
	})
}

// metaFromRequest 合并 gin 上下文与请求体中的设备信息（请求体优先）。
func metaFromRequest(c *gin.Context, d model.DeviceInfo) RequestMeta {
	m := MetaFromRequest(c)
	if d.DeviceID != "" {
		m.DeviceID = d.DeviceID
	}
	if d.UserAgent != "" {
		m.UserAgent = d.UserAgent
	}
	if d.IP != "" {
		m.IP = d.IP
	}
	return m
}

// writeError 统一输出错误：状态码由 apierr.HTTPStatus 决定，
// 仅回传 code/message（内部错误细节只写日志，不下发客户端）。
func writeError(c *gin.Context, err error) {
	e := apierr.FromError(err)
	if e.Code == apierr.CodeInternal {
		e = apierr.New(apierr.CodeInternal, msgInternal)
	}
	if e.Code != apierr.CodeBadRequest {
		e.Details = nil
	}
	slog.Default().Warn("auth: 请求失败", "code", string(e.Code), "err", err)
	c.JSON(apierr.HTTPStatus(e.Code), e)
}
