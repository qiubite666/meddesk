package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/cache"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// Service 实现认证业务逻辑：注册、登录、刷新令牌轮换与吊销、口令修改。
type Service struct {
	users   store.UserRepo
	refresh store.RefreshRepo
	cache   cache.Cache
	cfg     config.AuthConfig
	// recovery 是托管恢复私钥；nil 表示本部署未启用托管恢复。
	recovery *RecoveryKey
	// adminEmails 是后台白名单（小写）。仅用于前端决定是否显示「网站后台」入口；
	// 真正的权限校验在 /api/v1/admin/* 的 RequireAdmin 中间件里，二者互不影响。
	adminEmails map[string]bool
}

// SetAdminEmails 注入后台白名单，供 /auth/me 返回 isAdmin 标记。
//
// 【为什么放在认证服务里】前端不该硬编码管理员邮箱（换人就要改前端、也容易泄露）；
// 让它向服务端问一句"我是不是管理员"，判断依据始终只有配置一处。
func (s *Service) SetAdminEmails(emails []string) {
	s.adminEmails = make(map[string]bool, len(emails))
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			s.adminEmails[e] = true
		}
	}
}

// IsAdminEmail 判断某个邮箱是否在后台白名单内。
func (s *Service) IsAdminEmail(email string) bool {
	if s.adminEmails == nil {
		return false
	}
	return s.adminEmails[strings.ToLower(strings.TrimSpace(email))]
}

// NewService 构造认证服务。users/refresh 由 A1 提供，c 由 A2 提供。
func NewService(users store.UserRepo, refresh store.RefreshRepo, c cache.Cache, cfg config.AuthConfig) *Service {
	normalized := normalizeConfig(cfg)
	recovery, err := ParseRecoveryKey(normalized.RecoveryKey)
	if err != nil {
		// 配置坏了不能让整个服务起不来：降级为「不支持托管恢复」，
		// 管理员改口令仍走设备自愈路径。
		slog.Warn("托管恢复密钥不可用，已禁用该能力", "error", err)
		recovery = nil
	}
	return &Service{users: users, refresh: refresh, cache: c, cfg: normalized, recovery: recovery}
}

// RecoveryEnabled 报告本部署是否启用了托管恢复。
func (s *Service) RecoveryEnabled() bool { return s.recovery != nil }

// RecoveryPublicKey 返回 base64(SPKI DER) 公钥，供客户端包裹 DEK。
func (s *Service) RecoveryPublicKey() (string, error) {
	if s.recovery == nil {
		return "", apierr.New(apierr.CodeNotFound, "本部署未启用托管恢复")
	}
	return s.recovery.PublicKeyB64()
}

// RepairEnvelope 用客户端重新包裹的信封替换「待修复」的旧信封。
//
// 【触发条件】管理员用「直接改口令」模式改过口令：password_hash/kdf_salt 已换新，
// 但 wrapped_dek 仍是旧口令包裹的。持有本地 DEK 的设备登录后重包裹上传即可。
//
// 【为什么可以接受】调用方已通过新口令认证，只影响自己的账号；服务端没有 DEK，
// 无法校验信封内容，所以旧信封保留在 *_prev 列，传坏了还能回滚一次。
func (s *Service) RepairEnvelope(ctx context.Context, userID, wrappedB64, ivB64 string) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
		}
		return apierr.Wrap(apierr.CodeInternal, err, "修复密钥信封失败，请稍后重试")
	}
	if !u.EnvelopeStale {
		return apierr.New(apierr.CodeConflict, "当前密钥信封无需修复")
	}
	wrapped, err := base64.StdEncoding.DecodeString(strings.TrimSpace(wrappedB64))
	if err != nil {
		return apierr.New(apierr.CodeBadRequest, "信封格式有误")
	}
	iv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ivB64))
	if err != nil {
		return apierr.New(apierr.CodeBadRequest, "信封 IV 格式有误")
	}
	// AES-GCM 密文 = 明文(32) + tag(16)；长度不符必然不是合法信封。
	if len(wrapped) != credentialDEKLen+16 || len(iv) != credentialIVLen {
		return apierr.New(apierr.CodeBadRequest, "信封长度异常")
	}
	if err := s.users.RepairEnvelope(ctx, userID, wrapped, iv); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "修复密钥信封失败，请稍后重试")
	}
	return nil
}

// SetRecoveryEnvelope 开启/关闭「托管恢复」。
//
// 开启：客户端用服务端恢复公钥（RSA-OAEP）包裹当前 DEK 后上传，长度必须等于模长。
// 关闭：wrapped 传空，服务端清空两列 —— 之后管理员改口令只能走设备自愈。
func (s *Service) SetRecoveryEnvelope(ctx context.Context, userID, wrappedB64, ivB64 string) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	wrappedB64 = strings.TrimSpace(wrappedB64)
	if wrappedB64 == "" {
		if err := s.users.SetRecoveryEnvelope(ctx, userID, nil, nil); err != nil {
			return apierr.Wrap(apierr.CodeInternal, err, "关闭托管恢复失败，请稍后重试")
		}
		return nil
	}
	if s.recovery == nil {
		return apierr.New(apierr.CodeBadRequest, "本部署未启用托管恢复")
	}
	wrapped, err := base64.StdEncoding.DecodeString(wrappedB64)
	if err != nil {
		return apierr.New(apierr.CodeBadRequest, "托管信封格式有误")
	}
	if len(wrapped) != recoveryCipherLen {
		return apierr.New(apierr.CodeBadRequest, "托管信封长度异常")
	}
	var iv []byte
	if strings.TrimSpace(ivB64) != "" {
		iv, _ = base64.StdEncoding.DecodeString(strings.TrimSpace(ivB64))
	}
	if err := s.users.SetRecoveryEnvelope(ctx, userID, wrapped, iv); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "开启托管恢复失败，请稍后重试")
	}
	return nil
}

// Register 注册新用户并直接返回令牌对。注册未开放返回 CodeForbidden，邮箱重复返回 CodeConflict。
func (s *Service) Register(ctx context.Context, req model.RegisterRequest, meta RequestMeta) (*model.TokenResponse, error) {
	if !s.cfg.SignupEnabled {
		return nil, apierr.New(apierr.CodeForbidden, "当前未开放注册")
	}
	email := normalizeEmail(req.Email)
	if email == "" || req.PasswordHash == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "邮箱与口令派生值不能为空")
	}
	kdfSalt, wrappedDEK, dekIV, err := decodeKeyMaterial(req.KDFSalt, req.WrappedDEK, req.DEKIV)
	if err != nil {
		return nil, err
	}
	passwordHash, err := HashClientHash(req.PasswordHash, s.cfg.Argon2Memory, s.cfg.Argon2Time, s.cfg.Argon2Threads)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, msgInternal)
	}
	u := &model.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: passwordHash,
		KDFSalt:      kdfSalt,
		WrappedDEK:   wrappedDEK,
		DEKIV:        dekIV,
	}
	if err := s.users.Create(ctx, u); err != nil {
		if isConflict(err) {
			return nil, apierr.New(apierr.CodeConflict, "该邮箱已注册")
		}
		return nil, apierr.Wrap(apierr.CodeInternal, err, "注册失败，请稍后重试")
	}
	resp, _, err := s.issue(ctx, u, meta)
	return resp, err
}

// Login 校验凭据并签发令牌对。同一邮箱连续失败超过阈值返回 CodeRateLimited。
func (s *Service) Login(ctx context.Context, req model.LoginRequest, meta RequestMeta) (*model.TokenResponse, error) {
	email := normalizeEmail(req.Email)
	if email == "" || req.PasswordHash == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, msgInvalidCredential)
	}
	failKey := cacheKeyFail + hashKey(email)
	if n, err := s.cache.GetInt(ctx, failKey); err == nil && n >= maxLoginFailures {
		return nil, apierr.New(apierr.CodeRateLimited, msgRateLimited)
	}
	u, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.bumpFailure(ctx, failKey)
			return nil, apierr.New(apierr.CodeUnauthorized, msgInvalidCredential)
		}
		return nil, apierr.Wrap(apierr.CodeInternal, err, "登录失败，请稍后重试")
	}
	ok, err := VerifyClientHash(u.PasswordHash, req.PasswordHash)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "登录失败，请稍后重试")
	}
	if !ok {
		s.bumpFailure(ctx, failKey)
		return nil, apierr.New(apierr.CodeUnauthorized, msgInvalidCredential)
	}
	_ = s.cache.Del(ctx, failKey) // 登录成功清零失败计数
	resp, _, err := s.issue(ctx, u, meta)
	return resp, err
}

// Refresh 用刷新令牌换取新的令牌对，并轮换刷新令牌。
// 检测到已吊销/已替换的令牌被再次使用（重放攻击）时撤销该用户全部会话。
func (s *Service) Refresh(ctx context.Context, refreshToken string, meta RequestMeta) (*model.TokenResponse, error) {
	token := strings.TrimSpace(refreshToken)
	if token == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	sum := sha256.Sum256([]byte(token))
	old, err := s.refresh.GetByHash(ctx, sum[:])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
		}
		return nil, apierr.Wrap(apierr.CodeInternal, err, "刷新失败，请稍后重试")
	}
	if !old.ExpiresAt.IsZero() && time.Now().After(old.ExpiresAt) {
		return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	// 注意：A1 的 MarkReplaced 在标记 replaced_by 的同时会置 revoked_at，
	// 因此「已被轮换」必须先于「已被吊销」判断，否则宽限期内的并发刷新会被误判为重放。
	replaced := old.ReplacedBy != nil && *old.ReplacedBy != ""
	graced := false
	switch {
	case replaced:
		// 宽限期内允许并发刷新重用一次；宽限机会只在首次轮换时发放，不被复用再次续期。
		if graced = s.consumeGrace(ctx, old.ID); !graced {
			s.revokeAllSessions(ctx, old.UserID)
			return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
		}
	case old.RevokedAt != nil:
		// 被显式吊销（登出/改密/管理员操作）的令牌再次出现 → 重放。
		s.revokeAllSessions(ctx, old.UserID)
		return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	case s.isRevoked(ctx, old.ID):
		s.revokeAllSessions(ctx, old.UserID)
		return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}

	u, err := s.users.GetByID(ctx, old.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
		}
		return nil, apierr.Wrap(apierr.CodeInternal, err, "刷新失败，请稍后重试")
	}
	resp, newID, err := s.issue(ctx, u, meta)
	if err != nil {
		return nil, err
	}
	if err := s.refresh.MarkReplaced(ctx, old.ID, newID); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "刷新失败，请稍后重试")
	}
	// 旧令牌进吊销名单；仅在首次轮换时保留一次宽限重用机会。
	s.markRevoked(ctx, old.ID)
	if !graced && s.cfg.RefreshReuseGrace > 0 {
		_ = s.cache.Set(ctx, cacheKeyGrace+old.ID, "1", s.cfg.RefreshReuseGrace)
	}
	return resp, nil
}

// Logout 撤销单个刷新令牌（幂等：令牌不存在或已吊销均返回 nil）。
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	token := strings.TrimSpace(refreshToken)
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	t, err := s.refresh.GetByHash(ctx, sum[:])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return apierr.Wrap(apierr.CodeInternal, err, "登出失败，请稍后重试")
	}
	if t.RevokedAt == nil {
		if err := s.refresh.Revoke(ctx, t.ID); err != nil {
			return apierr.Wrap(apierr.CodeInternal, err, "登出失败，请稍后重试")
		}
	}
	s.markRevoked(ctx, t.ID)
	_ = s.cache.Del(ctx, cacheKeyGrace+t.ID)
	return nil
}

// LogoutAll 撤销该用户全部刷新令牌，并使已签发的访问令牌立即失效。
func (s *Service) LogoutAll(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	if err := s.refresh.RevokeAllForUser(ctx, userID); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "登出失败，请稍后重试")
	}
	// 名单值存吊销时刻：吊销前签发的访问令牌立即失效，吊销后的新登录不受影响。
	_ = s.cache.Set(ctx, cacheKeyUserRevoked+userID, strconv.FormatInt(time.Now().Unix(), 10), s.cfg.AccessTTL)
	return nil
}

// ChangePassword 校验旧口令后更新凭据，并保存客户端重新包裹的 DEK 材料。
func (s *Service) ChangePassword(ctx context.Context, userID string, req model.ChangePasswordRequest) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
		}
		return apierr.Wrap(apierr.CodeInternal, err, "修改口令失败，请稍后重试")
	}
	ok, err := VerifyClientHash(u.PasswordHash, req.OldPasswordHash)
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "修改口令失败，请稍后重试")
	}
	if !ok {
		return apierr.New(apierr.CodeUnauthorized, "当前口令不正确")
	}
	kdfSalt, wrappedDEK, dekIV, err := decodeKeyMaterial(req.KDFSalt, req.WrappedDEK, req.DEKIV)
	if err != nil {
		return err
	}
	passwordHash, err := HashClientHash(req.NewPasswordHash, s.cfg.Argon2Memory, s.cfg.Argon2Time, s.cfg.Argon2Threads)
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, msgInternal)
	}
	if err := s.users.UpdateCredentials(ctx, userID, passwordHash, kdfSalt, wrappedDEK, dekIV); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "修改口令失败，请稍后重试")
	}

	// 口令变更属于凭据失效事件：必须吊销包括本次发起端在内的全部会话。
	// 理由：DEK 虽然不变（本地已缓存的 DEK 仍可用），但旧刷新令牌在剩余 TTL 内
	// 依然能续出访问令牌；设备丢失场景下「改了口令，旧设备还能用满 30 天」，
	// 防护形同虚设。此处撤销全部令牌，让所有设备必须重新用新口令认证。
	// 副作用（有意为之）：发起改密的那台设备也会被登出，需要重新登录一次。
	s.revokeAllSessions(ctx, userID)
	return nil
}

// AccountSalt 返回账户的客户端 KDF 盐（新设备首次登录握手）。
// 客户端用该盐派生口令哈希后调 /auth/login；换设备时本地无缓存盐，由此接口下发。
// 防枚举：账户不存在时返回随机盐并照常 200——攻击者无法据此区分账户是否存在。
// 盐本身不保密（PBKDF2 盐的作用是防彩虹表/预计算），真正的口令强度由
// 客户端 PBKDF2 + 服务端 argon2id 两层派生保障。
func (s *Service) AccountSalt(ctx context.Context, email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			buf := make([]byte, 32)
			if _, err := rand.Read(buf); err != nil {
				return "", apierr.Wrap(apierr.CodeInternal, err, msgInternal)
			}
			return base64.StdEncoding.EncodeToString(buf), nil
		}
		return "", apierr.Wrap(apierr.CodeInternal, err, msgInternal)
	}
	return base64.StdEncoding.EncodeToString(u.KDFSalt), nil
}

// Authorize 校验访问令牌并检查用户级吊销名单，返回用户 ID。
// 吊销名单存的是「吊销时刻」的 Unix 秒：仅早于该时刻签发的令牌视为失效，
// 重新登录签发的新令牌（iat >= 吊销时刻）不受影响。
func (s *Service) Authorize(accessToken string) (string, error) {
	claims, err := s.parseAccessToken(strings.TrimSpace(accessToken))
	if err != nil {
		return "", apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
	}
	if v, err := s.cache.Get(context.Background(), cacheKeyUserRevoked+claims.Subject); err == nil && v != "" {
		if revokedAt, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			iat := int64(0)
			if claims.IssuedAt != nil {
				iat = claims.IssuedAt.Unix()
			}
			if iat < revokedAt {
				return "", apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
			}
		} else {
			// 值非法（兼容旧的 "1" 标记）：保守起见按全部吊销处理。
			return "", apierr.New(apierr.CodeUnauthorized, msgSessionExpired)
		}
	}
	return claims.Subject, nil
}

// issue 签发访问令牌并落库一条新的刷新令牌，返回响应与新刷新令牌 ID。
func (s *Service) issue(ctx context.Context, u *model.User, meta RequestMeta) (*model.TokenResponse, string, error) {
	raw, err := randomToken()
	if err != nil {
		return nil, "", apierr.Wrap(apierr.CodeInternal, err, msgInternal)
	}
	sum := sha256.Sum256([]byte(raw))
	now := time.Now()
	id := uuid.NewString()
	rt := &model.RefreshToken{
		ID:        id,
		UserID:    u.ID,
		TokenHash: sum[:], // 库中只存 sha256，不存明文
		DeviceID:  truncate(meta.DeviceID, maxMetaLen),
		UserAgent: truncate(meta.UserAgent, maxMetaLen),
		IP:        truncate(meta.IP, maxMetaLen),
		ExpiresAt: now.Add(s.cfg.RefreshTTL),
		CreatedAt: now,
	}
	if err := s.refresh.Create(ctx, rt); err != nil {
		return nil, "", apierr.Wrap(apierr.CodeInternal, err, "登录失败，请稍后重试")
	}
	access, expiresIn, err := s.signAccessToken(u.ID)
	if err != nil {
		return nil, "", apierr.Wrap(apierr.CodeInternal, err, "登录失败，请稍后重试")
	}
	return &model.TokenResponse{
		AccessToken:  access,
		RefreshToken: raw,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		UserID:       u.ID,
		KDFSalt:      base64.StdEncoding.EncodeToString(u.KDFSalt),
		WrappedDEK:   base64.StdEncoding.EncodeToString(u.WrappedDEK),
		DEKIV:        base64.StdEncoding.EncodeToString(u.DEKIV),
		// 管理员改过口令但信封仍是旧口令包裹时，客户端必须改走「用本地 DEK 重包裹」，
		// 否则会拿新口令解旧信封必然失败，看起来像"口令对了却进不去"。
		EnvelopeStale: u.EnvelopeStale,
	}, id, nil
}

// revokeAllSessions 撤销该用户全部刷新令牌，并让「吊销时刻之前签发」的访问令牌在一个 TTL 内失效。
// 名单值存吊销时刻的 Unix 秒：吊销后重新登录得到的新令牌不受影响（见 Authorize）。
func (s *Service) revokeAllSessions(ctx context.Context, userID string) {
	slog.Default().Warn("auth: 检测到刷新令牌重放，撤销该用户全部会话", "user_id", userID)
	if err := s.refresh.RevokeAllForUser(ctx, userID); err != nil {
		slog.Default().Error("auth: 撤销全部刷新令牌失败", "user_id", userID, "err", err)
	}
	_ = s.cache.Set(ctx, cacheKeyUserRevoked+userID, strconv.FormatInt(time.Now().Unix(), 10), s.cfg.AccessTTL)
}

// consumeGrace 消费一次宽限重用机会；不存在或已用尽返回 false。
func (s *Service) consumeGrace(ctx context.Context, id string) bool {
	if s.cfg.RefreshReuseGrace <= 0 {
		return false
	}
	v, err := s.cache.Get(ctx, cacheKeyGrace+id)
	if err != nil || v == "" {
		return false
	}
	_ = s.cache.Del(ctx, cacheKeyGrace+id)
	return true
}

// markRevoked 把刷新令牌 ID 写入 cache 吊销名单。
func (s *Service) markRevoked(ctx context.Context, id string) {
	_ = s.cache.Set(ctx, cacheKeyRefreshRevoked+id, "1", s.cfg.RefreshTTL)
}

// isRevoked 判断刷新令牌 ID 是否在吊销名单中（cache 故障时回退到数据库状态）。
func (s *Service) isRevoked(ctx context.Context, id string) bool {
	v, err := s.cache.Get(ctx, cacheKeyRefreshRevoked+id)
	return err == nil && v != ""
}

// bumpFailure 递增登录失败计数并设置 TTL。
func (s *Service) bumpFailure(ctx context.Context, key string) {
	if _, err := s.cache.Incr(ctx, key, loginFailTTL); err != nil {
		slog.Default().Warn("auth: 登录失败计数写入失败", "err", err)
	}
}

// normalizeEmail 邮箱统一去空白并转小写。
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// hashKey 把邮箱等标识哈希为缓存键片段，避免原始邮箱进入 Redis。
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(normalizeEmail(s)))
	return hex.EncodeToString(sum[:])
}

// randomToken 生成 32 字节随机值并以 base64url 编码（发给客户端的刷新令牌明文）。
func randomToken() (string, error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成刷新令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeKeyMaterial 解码客户端提交的密钥包裹材料（kdfSalt/wrappedDek/dekIv）。
func decodeKeyMaterial(kdfSalt, wrappedDEK, dekIV string) ([]byte, []byte, []byte, error) {
	salt, err := decodeBase64(kdfSalt)
	if err != nil {
		return nil, nil, nil, apierr.New(apierr.CodeBadRequest, "kdfSalt 不是合法的 base64")
	}
	wrapped, err := decodeBase64(wrappedDEK)
	if err != nil {
		return nil, nil, nil, apierr.New(apierr.CodeBadRequest, "wrappedDek 不是合法的 base64")
	}
	iv, err := decodeBase64(dekIV)
	if err != nil {
		return nil, nil, nil, apierr.New(apierr.CodeBadRequest, "dekIv 不是合法的 base64")
	}
	return salt, wrapped, iv, nil
}

// decodeBase64 兼容标准与无填充两种 base64 编码。
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// isConflict 判断是否为唯一约束冲突。
func isConflict(err error) bool {
	return apierr.Is(err, apierr.CodeConflict) || errors.Is(err, store.ErrConflict)
}
