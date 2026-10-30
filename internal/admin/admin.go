// Package admin 实现管理后台：管理员登录、用户管理（列表/新增/删除/改邮箱/重置口令）。
//
// 【身份模型】
// 管理员 = 配置里白名单邮箱 + 该账号自身的口令。后台不另存密码，避免多一套凭据；
// 台账 identity 直接复用 users 表，登录校验走与业务登录相同的 argon2id 验证。
//
// 【为什么后台 token 与业务 token 分开】
// 业务 access token 由客户端长期持有并频繁刷新，泄露面大；后台 token 单独签发、
// 带 scope=admin 声明、默认 8 小时有效，即使业务 token 泄露也无法调用后台接口。
//
// 【端到端加密的硬约束】
// 重置口令会让旧 KEK 无法解包旧 DEK，服务器上的旧密文对新口令而言永久不可读，
// 因此重置口令必须连带清空该用户的同步密文（wipeData=true 显式确认），
// 账号从空状态开始；旧设备本地数据不受影响，可先导出备份再导入。
package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/auth"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// 后台登录的失败锁定参数：同一 IP + 邮箱连续失败 5 次，锁 60 秒。
const (
	loginMaxFailures = 5
	loginLockWindow  = 60 * time.Second
)

// Repo 是后台所需的数据访问能力（由 *store.Store 实现）。
type Repo interface {
	AdminListUsers(ctx context.Context, q string, page, size int) ([]store.AdminUserRow, int64, error)
	AdminStats(ctx context.Context) (store.AdminOverview, error)
	AdminUpdateEmail(ctx context.Context, id, email string) error
	AdminDeleteUser(ctx context.Context, id string) ([]string, error)
	AdminWipeSync(ctx context.Context, id string) error
	AdminUserEmail(ctx context.Context, id string) (string, error)
}

// Service 组装后台依赖。
type Service struct {
	users   store.UserRepo
	refresh store.RefreshRepo
	repo    Repo
	cfg     config.Config
	log     *slog.Logger
	// recovery 是托管恢复私钥；nil 表示本部署未启用托管恢复（改口令只能走设备自愈）。
	recovery *auth.RecoveryKey

	mu    sync.Mutex
	fails map[string]failState
}

type failState struct {
	count int
	until time.Time
}

// NewService 构造后台服务。
func NewService(users store.UserRepo, refresh store.RefreshRepo, repo Repo, cfg config.Config, log *slog.Logger) *Service {
	recovery, err := auth.ParseRecoveryKey(cfg.Auth.RecoveryKey)
	if err != nil {
		log.Warn("后台：托管恢复密钥不可用，改口令只能走设备自愈", "error", err)
		recovery = nil
	}
	return &Service{users: users, refresh: refresh, repo: repo, cfg: cfg, log: log,
		recovery: recovery, fails: make(map[string]failState)}
}

// AdminClaims 是后台 token 的声明。
type AdminClaims struct {
	Email string `json:"email"`
	Scope string `json:"scope"`
	jwt.RegisteredClaims
}

// mountPath 统一前缀。
const mountPath = "/api/v1/admin"

// Mount 挂载后台路由。
func Mount(v1 *gin.RouterGroup, sc *Service) {
	g := v1.Group("/admin")
	g.GET("/salt", sc.Salt)
	g.POST("/login", sc.Login)

	authed := g.Group("")
	authed.Use(sc.RequireAdmin())
	authed.GET("/overview", sc.Overview)
	authed.GET("/users", sc.ListUsers)
	authed.POST("/users", sc.CreateUser)
	authed.PATCH("/users/:id", sc.UpdateUser)
	authed.POST("/users/:id/password", sc.ResetPassword)
	authed.DELETE("/users/:id", sc.DeleteUser)
}

func (s *Service) isAdminEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, e := range s.cfg.Admin.Emails {
		if e == email {
			return true
		}
	}
	return false
}

/* ------------------------------ 登录 ------------------------------ */

type loginRequest struct {
	Email        string `json:"email" binding:"required,email,max=160"`
	PasswordHash string `json:"passwordHash" binding:"required,min=32,max=128"`
}

// Salt 返回管理员独立凭据使用的固定 KDF salt。
//
// 只有配置了独立凭据才返回；否则 404——前端据此决定是走账号口令还是走后台凭据。
func (s *Service) Salt(c *gin.Context) {
	if s.cfg.Admin.Salt == "" || s.cfg.Admin.PasswordHash == "" {
		writeErr(c, apierr.New(apierr.CodeNotFound, "未配置管理员独立凭据"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"kdfSalt": s.cfg.Admin.Salt})
}

// Login 校验白名单邮箱 + 口令派生值，签发后台 token。
func (s *Service) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "参数不合法"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	key := c.ClientIP() + "|" + email
	if msg, locked := s.locked(key); locked {
		writeErr(c, apierr.New(apierr.CodeRateLimited, msg))
		return
	}

	// 白名单先判：非白名单账号一律按凭据错误处理，不暴露「你不是管理员」这一信息。
	if !s.isAdminEmail(email) {
		s.recordFail(key)
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "邮箱或口令不正确"))
		return
	}

	ctx := c.Request.Context()
	u, userErr := s.users.GetByEmail(ctx, email)
	if userErr != nil && !errors.Is(userErr, store.ErrNotFound) {
		writeErr(c, apierr.Wrap(apierr.CodeInternal, userErr, "登录失败，请稍后重试"))
		return
	}

	// 凭据校验有两条通道：
	//   a) 账号自身的口令（与业务登录完全相同的 argon2id 校验）；
	//   b) 配置的独立管理员凭据（只对后台有效，不动账号、不影响端到端加密数据）。
	// 任一通过即可，但邮箱必须在白名单内（已在上面判过）。
	verified := false
	if u != nil {
		ok, err := auth.VerifyClientHash(u.PasswordHash, req.PasswordHash)
		if err != nil {
			writeErr(c, apierr.Wrap(apierr.CodeInternal, err, "登录失败，请稍后重试"))
			return
		}
		verified = ok
	}
	if !verified && s.cfg.Admin.PasswordHash != "" {
		ok, err := auth.VerifyClientHash(s.cfg.Admin.PasswordHash, req.PasswordHash)
		if err == nil && ok {
			verified = true
		}
	}
	if !verified {
		s.recordFail(key)
		writeErr(c, apierr.New(apierr.CodeUnauthorized, "邮箱或口令不正确"))
		return
	}
	s.clearFail(key)

	// 账号不存在（但凭据来自独立管理员凭据）时，以邮箱作为主体标识；
	// 后台所有操作都以 target id 为准，主体只用于审计与自我保护判断。
	subject := "admin:" + email
	displayEmail := email
	if u != nil {
		subject = u.ID
		displayEmail = u.Email
	}

	ttl := s.cfg.Admin.TTL
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	now := time.Now()
	claims := AdminClaims{
		Email: displayEmail,
		Scope: "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "meddesk-admin",
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.Auth.JWTSecret))
	if err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeInternal, err, "签发后台令牌失败"))
		return
	}
	s.log.Info("admin: 登录成功", "email", displayEmail, "ip", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"token":     signed,
		"email":     displayEmail,
		"expiresIn": int64(ttl.Seconds()),
	})
}

/* ------------------------------ 鉴权中间件 ------------------------------ */

const (
	ctxAdminID    = "admin_user_id"
	ctxAdminEmail = "admin_email"
)

// RequireAdmin 校验后台 token：签名、scope、过期时间、白名单、账号仍然存在。
func (s *Service) RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "请先登录后台"))
			return
		}
		tokenStr := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))

		claims := &AdminClaims{}
		tok, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			if t.Method != jwt.SigningMethodHS256 {
				return nil, errors.New("签名算法不匹配")
			}
			return []byte(s.cfg.Auth.JWTSecret), nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Name}), jwt.WithIssuer("meddesk-admin"))
		if err != nil || !tok.Valid {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "后台会话已失效，请重新登录"))
			return
		}
		if claims.Scope != "admin" || claims.Subject == "" || !s.isAdminEmail(claims.Email) {
			writeErr(c, apierr.New(apierr.CodeForbidden, "无权访问后台"))
			return
		}

		// 账号主体：以数据库为准再核对一次，防止「账号被删/邮箱被改」后旧令牌仍可用。
		// 独立管理员凭据登录的主体形如 admin:<email>，无对应账号，跳过该核对（凭据本身即可信）。
		if !strings.HasPrefix(claims.Subject, "admin:") {
			dbEmail, err := s.repo.AdminUserEmail(c.Request.Context(), claims.Subject)
			if err != nil || !strings.EqualFold(dbEmail, claims.Email) {
				writeErr(c, apierr.New(apierr.CodeUnauthorized, "后台会话已失效，请重新登录"))
				return
			}
		}

		c.Set(ctxAdminID, claims.Subject)
		c.Set(ctxAdminEmail, strings.ToLower(claims.Email))
		c.Next()
	}
}

/* ------------------------------ 概览与用户列表 ------------------------------ */

// Overview 返回后台首屏统计。
func (s *Service) Overview(c *gin.Context) {
	ctx := c.Request.Context()
	data, err := s.repo.AdminStats(ctx)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, data)
}

// ListUsers 分页 + 邮箱模糊搜索。
func (s *Service) ListUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	page := atoiDefault(c.Query("page"), 1)
	size := atoiDefault(c.Query("size"), 20)
	if size > 200 {
		size = 200
	}
	items, total, err := s.repo.AdminListUsers(c.Request.Context(), q, page, size)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"total": total,
		"page":  page,
		"size":  size,
	})
}

/* ------------------------------ 新增用户 ------------------------------ */

type createRequest struct {
	Email    string `json:"email" binding:"required,max=160"`
	Password string `json:"password" binding:"required,min=8,max=128"`
}

// CreateUser 由管理员代为创建账号：服务端生成密钥材料，用户用给定口令即可登录。
func (s *Service) CreateUser(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "邮箱或口令不合法（口令至少 8 位）"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "邮箱格式不正确"))
		return
	}
	mat, err := auth.GenerateCredentials(req.Password, s.cfg.Auth.Argon2Memory, s.cfg.Auth.Argon2Time, s.cfg.Auth.Argon2Threads)
	if err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, err.Error()))
		return
	}
	u := &model.User{
		Email:        email,
		PasswordHash: mat.PasswordHash,
		KDFSalt:      mat.KDFSalt,
		WrappedDEK:   mat.WrappedDEK,
		DEKIV:        mat.DEKIV,
	}
	if err := s.users.Create(c.Request.Context(), u); err != nil {
		writeErr(c, err)
		return
	}
	s.audit(c, "创建用户", u.Email, u.ID)
	c.JSON(http.StatusOK, gin.H{"id": u.ID, "email": u.Email})
}

/* ------------------------------ 编辑信息 ------------------------------ */

type updateRequest struct {
	Email string `json:"email" binding:"required,max=160"`
}

// UpdateUser 目前可编辑的基本信息只有邮箱（登录标识）。
//
// 口令不在这里改：改口令涉及 DEK 重新包裹，必须走「重置口令」这条带数据清空的路径。
func (s *Service) UpdateUser(c *gin.Context) {
	id := c.Param("id")
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "参数不合法"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "邮箱格式不正确"))
		return
	}
	if email == s.currentAdminEmail(c) {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "不能修改自己的登录邮箱"))
		return
	}
	if err := s.repo.AdminUpdateEmail(c.Request.Context(), id, email); err != nil {
		writeErr(c, err)
		return
	}
	s.audit(c, "修改邮箱", email, id)
	c.JSON(http.StatusOK, gin.H{"id": id, "email": email})
}

/* ------------------------------ 重置口令 ------------------------------ */

type resetPasswordRequest struct {
	Password string `json:"password" binding:"required,min=8,max=128"`

	// Mode 决定「数据怎么办」：
	//   keep   （默认）只换口令，保留信封与全部密文，等持有 DEK 的设备登录后自愈；
	//   escrow 账号开启过托管恢复时，服务端解出 DEK 重包裹 —— 任何设备立刻可用新口令；
	//   wipe   换一把新 DEK 并清空该账号密文（旧行为，仅在数据无价值/已泄露时用）。
	Mode string `json:"mode"`
	// WipeData 兼容旧前端：为 true 且 Mode 为空时按 wipe 处理。
	WipeData bool `json:"wipeData"`
}

// ResetPassword 重置指定用户口令。
//
// 【为什么必须清空密文】
// 新口令派生新 KEK，旧 wrapped_dek 无法解开；服务器上的旧密文对新会话永久不可读。
// 保留它们只会让客户端拉取到无法解密的记录并反复报错，因此这里一并清空，
// 并要求调用方显式提交 wipeData=true 以确认理解后果。
func (s *Service) ResetPassword(c *gin.Context) {
	id := c.Param("id")
	if selfEmail, _ := s.repo.AdminUserEmail(c.Request.Context(), id); s.isSelf(c, id, selfEmail) {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "不能在此重置自己的口令；请在应用内修改（会保留数据）"))
		return
	}
	var req resetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "新口令至少 8 位"))
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		if req.WipeData {
			mode = "wipe"
		} else {
			mode = "keep"
		}
	}

	ctx := c.Request.Context()
	argon := func() (string, []byte, error) {
		return auth.GeneratePasswordOnly(req.Password, s.cfg.Auth.Argon2Memory, s.cfg.Auth.Argon2Time, s.cfg.Auth.Argon2Threads)
	}

	switch mode {
	case "wipe":
		// 旧行为：换一把新 DEK 并清空密文。只有「数据本来就该作废」时才用。
		mat, err := auth.GenerateCredentials(req.Password, s.cfg.Auth.Argon2Memory, s.cfg.Auth.Argon2Time, s.cfg.Auth.Argon2Threads)
		if err != nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, err.Error()))
			return
		}
		if err := s.users.UpdateCredentials(ctx, id, mat.PasswordHash, mat.KDFSalt, mat.WrappedDEK, mat.DEKIV); err != nil {
			writeErr(c, err)
			return
		}
		// DEK 换了，旧托管信封对新 DEK 已无意义，一并清掉，避免占用与误用。
		if err := s.users.SetRecoveryEnvelope(ctx, id, nil, nil); err != nil {
			writeErr(c, err)
			return
		}
		if err := s.refresh.RevokeAllForUser(ctx, id); err != nil {
			writeErr(c, err)
			return
		}
		if err := s.repo.AdminWipeSync(ctx, id); err != nil {
			writeErr(c, err)
			return
		}
		email, _ := s.repo.AdminUserEmail(ctx, id)
		s.audit(c, "重置口令（已清空密文）", email, id)
		c.JSON(http.StatusOK, gin.H{"id": id, "mode": "wipe", "wiped": true, "recovered": "none"})
		return

	case "escrow":
		// 账号开启过托管恢复：服务端解出 DEK，用新口令重新包裹 —— 数据一条不动。
		if s.recovery == nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, "本部署未启用托管恢复，请改用「直接改口令（保数据）」"))
			return
		}
		u, err := s.users.GetByID(ctx, id)
		if err != nil {
			writeErr(c, err)
			return
		}
		if len(u.RecoveryWrappedDEK) == 0 {
			writeErr(c, apierr.New(apierr.CodeBadRequest, "该账号未开启托管恢复，无法完全恢复；请改用「直接改口令（保数据）」"))
			return
		}
		dek, err := s.recovery.Unwrap(u.RecoveryWrappedDEK)
		if err != nil {
			s.log.Error("后台：托管信封解密失败", "user", id, "error", err)
			writeErr(c, apierr.New(apierr.CodeInternal, "托管信封无法解密（恢复密钥可能已更换），请改用「直接改口令（保数据）」"))
			return
		}
		mat, err := auth.GenerateCredentialsWithDEK(dek, req.Password, s.cfg.Auth.Argon2Memory, s.cfg.Auth.Argon2Time, s.cfg.Auth.Argon2Threads)
		if err != nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, err.Error()))
			return
		}
		if err := s.users.UpdateCredentials(ctx, id, mat.PasswordHash, mat.KDFSalt, mat.WrappedDEK, mat.DEKIV); err != nil {
			writeErr(c, err)
			return
		}
		// 托管信封里的 DEK 没变，仍然有效，不需要重建。
		if err := s.refresh.RevokeAllForUser(ctx, id); err != nil {
			writeErr(c, err)
			return
		}
		email, _ := s.repo.AdminUserEmail(ctx, id)
		s.audit(c, "改口令（托管恢复，数据保留）", email, id)
		s.log.Info("后台：托管恢复改口令成功", "user", id, "email", email)
		c.JSON(http.StatusOK, gin.H{"id": id, "mode": "escrow", "wiped": false, "recovered": "escrow"})
		return

	case "keep":
		// 默认路径：只换口令与盐，信封与全部密文原样保留，标记为待自愈。
		hash, salt, err := argon()
		if err != nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, err.Error()))
			return
		}
		if err := s.users.UpdatePasswordKeepEnvelope(ctx, id, hash, salt); err != nil {
			writeErr(c, err)
			return
		}
		if err := s.refresh.RevokeAllForUser(ctx, id); err != nil {
			writeErr(c, err)
			return
		}
		email, _ := s.repo.AdminUserEmail(ctx, id)
		s.audit(c, "改口令（保留数据，待设备自愈）", email, id)
		s.log.Info("后台：改口令成功（待设备自愈）", "user", id, "email", email)
		c.JSON(http.StatusOK, gin.H{"id": id, "mode": "keep", "wiped": false, "recovered": "device"})
		return

	default:
		writeErr(c, apierr.New(apierr.CodeBadRequest, "mode 只能是 keep / escrow / wipe"))
		return
	}
}

/* ------------------------------ 删除用户 ------------------------------ */

// DeleteUser 删除账号：级联清空会话、同步密文、授权与附件元数据，并尽力删除附件文件。
func (s *Service) DeleteUser(c *gin.Context) {
	id := c.Param("id")
	email, _ := s.repo.AdminUserEmail(c.Request.Context(), id)
	if s.isSelf(c, id, email) {
		writeErr(c, apierr.New(apierr.CodeBadRequest, "不能删除当前登录的管理员账号"))
		return
	}
	paths, err := s.repo.AdminDeleteUser(c.Request.Context(), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	removed := 0
	for _, rel := range paths {
		full := joinUploadPath(s.cfg.Upload.Dir, rel)
		if full == "" {
			continue
		}
		if err := removeFile(full); err == nil {
			removed++
		}
	}
	s.audit(c, "删除用户", email, id)
	c.JSON(http.StatusOK, gin.H{"id": id, "filesRemoved": removed})
}

/* ------------------------------ 工具 ------------------------------ */

func (s *Service) currentAdminEmail(c *gin.Context) string {
	return c.GetString(ctxAdminEmail)
}

// isSelf 判断目标账号是否就是当前管理员本人（按 id 或邮箱两条路判断，
// 独立凭据登录时主体不是账号 id，只能按邮箱比对）。
func (s *Service) isSelf(c *gin.Context, targetID, targetEmail string) bool {
	if id := c.GetString(ctxAdminID); id != "" && !strings.HasPrefix(id, "admin:") && id == targetID {
		return true
	}
	return targetEmail != "" && strings.EqualFold(targetEmail, s.currentAdminEmail(c))
}

func (s *Service) audit(c *gin.Context, action, target, targetID string) {
	s.log.Info("admin: 操作",
		"actor", s.currentAdminEmail(c),
		"action", action,
		"target", target,
		"targetId", targetID,
		"ip", c.ClientIP())
}

func (s *Service) locked(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.fails[key]
	if !ok {
		return "", false
	}
	if time.Now().Before(st.until) {
		return "尝试过于频繁，请 1 分钟后再试", true
	}
	if st.until.IsZero() && st.count >= loginMaxFailures {
		return "", false // 窗口已过，允许重试
	}
	return "", false
}

func (s *Service) recordFail(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.fails[key]
	st.count++
	if st.count >= loginMaxFailures {
		st.until = time.Now().Add(loginLockWindow)
		st.count = 0
	}
	s.fails[key] = st
	// 简单清扫，避免长期运行后 map 无限增长。
	if len(s.fails) > 10000 {
		now := time.Now()
		for k, v := range s.fails {
			if v.until.Before(now) {
				delete(s.fails, k)
			}
		}
	}
}

func (s *Service) clearFail(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fails, key)
}

func validEmail(s string) bool {
	if s == "" || len(s) > 160 {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && strings.EqualFold(addr.Address, s)
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return def
		}
	}
	if n <= 0 {
		return def
	}
	return n
}

func writeErr(c *gin.Context, err error) {
	e := apierr.FromError(err)
	c.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}
