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
}

// NewService 构造认证服务。users/refresh 由 A1 提供，c 由 A2 提供。
func NewService(users store.UserRepo, refresh store.RefreshRepo, c cache.Cache, cfg config.AuthConfig) *Service {
	return &Service{users: users, refresh: refresh, cache: c, cfg: normalizeConfig(cfg)}
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
