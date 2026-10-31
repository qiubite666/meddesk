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
	// captcha 复用 Service 已持有的缓存实例，避免再给 Mount 增加一个依赖参数，
	// 也让验证码在 Redis 可用时天然多实例共享。
	captcha *captchaStore
}

// NewHandler 构造认证 HTTP 处理器。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc, captcha: newCaptchaStore(svc.cache)}
}

// Mount 在传入的路由组（即 /api/v1）下挂载 /auth/* 子路由。
// 需要鉴权的路由在内部自行用 RequireAuth 包装。
func Mount(rg *gin.RouterGroup, svc *Service) {
	h := NewHandler(svc)
	auth := rg.Group("/auth")
	{
		auth.GET("/captcha", h.captchaImage)
		auth.POST("/register", h.register)
		auth.POST("/login", h.login)
		auth.POST("/salt", h.salt)
		auth.POST("/refresh", h.refresh)
		auth.POST("/logout", h.logout)
		auth.POST("/logout-all", RequireAuth(svc), h.logoutAll)
		auth.POST("/change-password", RequireAuth(svc), h.changePassword)
		// 托管恢复：公钥公开（客户端用它包裹 DEK），另外两条需要登录态。
		auth.GET("/recovery-key", h.recoveryKey)
		auth.POST("/recovery-envelope", RequireAuth(svc), h.setRecoveryEnvelope)
		auth.POST("/repair-envelope", RequireAuth(svc), h.repairEnvelope)
		auth.GET("/me", RequireAuth(svc), h.me)
	}
}

// captchaImage 下发一张新图形验证码。
func (h *Handler) captchaImage(c *gin.Context) {
	id, svg, err := h.captcha.issue(c.Request.Context())
	if err != nil {
		writeError(c, apierr.Wrap(apierr.CodeInternal, err, "验证码生成失败，请稍后重试"))
		return
	}
	// 验证码是瞬时凭证，任何中间层缓存都会让用户看到已失效的图。
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, CaptchaResponse{CaptchaID: id, SVG: svg, ExpiresIn: int(captchaTTL.Seconds())})
}

// register 处理用户注册。
func (h *Handler) register(c *gin.Context) {
	var req model.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	// 验证码在业务逻辑之前校验：注册的任何失败路径（邮箱已存在、未开放注册、
	// 内部错误）都会让这张码失效。取舍是「用户需重新取图」换「一码只许试一次」，
	// 否则攻击者可以用一张图反复探测邮箱是否已注册。
	// 【必须与 MetaFromRequest 用同一个归一化函数】否则验证码限流与登录限流
	// 会按两种不同的字符串给同一个客户端计数（IPv6 zone / v4-mapped），
	// 变成两条互不相干的桶。
	if ok, err := h.captcha.verify(c.Request.Context(), req.CaptchaID, req.CaptchaCode, clientIP(c)); err != nil {
		// 目前只有 IP 维度限流会走到这里，需要如实回 429 而不是"验证码错误"。
		writeError(c, err)
		return
	} else if !ok {
		writeError(c, apierr.New(apierr.CodeBadRequest, msgCaptchaInvalid))
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

// recoveryKey 下发托管恢复公钥（base64 SPKI DER）。
//
// 公钥本就是公开材料；未启用托管时返回 enabled=false，前端据此隐藏开关。
func (h *Handler) recoveryKey(c *gin.Context) {
	if !h.svc.RecoveryEnabled() {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "publicKey": ""})
		return
	}
	pub, err := h.svc.RecoveryPublicKey()
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.JSON(http.StatusOK, gin.H{"enabled": true, "publicKey": pub})
}

// setRecoveryEnvelope 开启/关闭账号的「托管恢复」。
func (h *Handler) setRecoveryEnvelope(c *gin.Context) {
	userID, ok := UserID(c)
	if !ok {
		writeError(c, apierr.New(apierr.CodeUnauthorized, msgSessionExpired))
		return
	}
	var req model.EnvelopeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	if err := h.svc.SetRecoveryEnvelope(c.Request.Context(), userID, req.WrappedDEK, req.DEKIV); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "enabled": req.WrappedDEK != ""})
}

// repairEnvelope 用本地 DEK 重新包裹后回填信封（管理员改口令后的自愈）。
func (h *Handler) repairEnvelope(c *gin.Context) {
	userID, ok := UserID(c)
	if !ok {
		writeError(c, apierr.New(apierr.CodeUnauthorized, msgSessionExpired))
		return
	}
	var req model.EnvelopeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	if err := h.svc.RepairEnvelope(c.Request.Context(), userID, req.WrappedDEK, req.DEKIV); err != nil {
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
		ServerTime:      time.Now().UnixMilli(),
		RecoveryEnabled: len(u.RecoveryWrappedDEK) > 0,
		IsAdmin:         h.svc.IsAdminEmail(u.Email),
	})
}

// metaFromRequest 合并 gin 上下文与请求体中的设备信息。
//
// 【为什么不再采信请求体里的 IP】m.IP 是登录失败计数（A-20）与验证码失败计数
// （A-11）的限流键：一旦允许客户端用 device.ip 覆盖，攻击者每次随机填一个 IP
// 就能把 IP 维度限流彻底绕过，两个安全控件同时失效。IP 只能取服务端观测值
// （经 TrustProxy 校准的 c.ClientIP()）。设备自报 IP 若要保留，必须另立字段
// 并标注「不可信、仅审计」，不能用于任何判定。
func metaFromRequest(c *gin.Context, d model.DeviceInfo) RequestMeta {
	m := MetaFromRequest(c)
	if d.DeviceID != "" {
		m.DeviceID = d.DeviceID
	}
	if d.UserAgent != "" {
		m.UserAgent = d.UserAgent
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
	if e.Code == apierr.CodeUnavailable {
		// 注释里承诺"让客户端退避"，就必须真的给出退避信号；
		// 否则调用方的通用重试逻辑会立刻重打，把排队越堆越长。
		c.Header("Retry-After", "3")
	}
	slog.Default().Warn("auth: 请求失败", "code", string(e.Code), "err", err)
	c.JSON(apierr.HTTPStatus(e.Code), e)
}
