// Package family 实现「家庭关联」的短码授权接口。
//
// 威胁模型（实现时必须守住的五条）：
//  1. 越权读写：owner 一律从 grant 行取，**绝不接受客户端传 ownerId**；
//     写入口（POST /records）额外限死「只允许 medication 表」——查看方的授权
//     是「共同维护药物清单」，不是「能改对方的排程、服药历史或档案」；
//  2. 短码爆破：8 位 40bit 熵，必须限流，否则可离线/在线穷举；
//  3. 短码泄露：库里只存 HMAC(pepper, code)，服务端不保存明文码；
//  4. 密钥泄露：wrapped_dek 只有短码能解，服务端即使被拖库也拿不到 owner 的 DEK。
//
// 产品决定（已确认）：短码 8 位；授权建立后**永久有效**，直到所有者撤销；
// 不提供一次性分享链接。
package family

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/auth"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

const (
	// pendingCodeTTL 是「已生成但尚未兑换」的短码存活时间。
	// 授权一旦兑换即转为永久（expires_at=NULL），这里限的是悬挂空码的窗口。
	pendingCodeTTL = 24 * time.Hour

	maxCodeLen  = 8
	maxWrapped  = 512 // wrapped_dek 上限（实际约 48 字节，留足余量防滥用）
	maxSaltLen  = 64
	maxPageSize = 200
)

// digits 是短码字符集。按产品决定改为**纯数字**：便于电话口述与短信转发。
// 代价是熵从 40 bit 降到约 26.6 bit（10^8），因此兑换限流必须保持严格：
// 5 次/分钟/IP 时，命中一个码的期望时间以年计，实际不可爆破。
const digits = "0123456789"

// Service 聚合依赖。
type Service struct {
	repo    store.FamilyRepo
	sync    store.SyncRepo
	pepper  []byte
	limiter *redeemLimiter
}

// NewService 构造服务。pepper 由调用方从进程密钥派生，绝不落库。
//
// syncRepo 用于「共享写入」：查看方提交的药物变更必须落进数据所有方的账号空间，
// 复用同步内核的事务性冲突裁决（SyncRepo.Apply），不另造一套写入路径。
// 允许为 nil —— 那样只保留只读能力，写接口会明确报未实现而不是静默丢弃。
func NewService(repo store.FamilyRepo, syncRepo store.SyncRepo, pepper string) *Service {
	return &Service{
		repo:    repo,
		sync:    syncRepo,
		pepper:  []byte(pepper),
		limiter: newRedeemLimiter(5, time.Minute, 20, time.Hour),
	}
}

// lookupOf 计算短码的查找键。用 HMAC 而非裸哈希：
// 短码熵很低，裸 sha256 在库泄露后可被离线爆破，加 pepper 后无法反推。
func (s *Service) lookupOf(code string) []byte {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

// normalizeCode 归一化用户输入：去空格与连字符、转大写，并校验字符集与长度。
func normalizeCode(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r == ' ' || r == '-' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if !strings.ContainsRune(digits, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	code := b.String()
	if len(code) != maxCodeLen {
		return "", false
	}
	return code, true
}

// GenerateCode 由服务端随机生成 8 位纯数字短码。
//
// 用拒绝采样而不是取模：256 % 10 != 0，直接取模会让 0~5 出现概率略高，
// 缩小有效搜索空间。拒绝 >=250 的字节即可得到均匀分布。
func (s *Service) GenerateCode() (string, error) {
	var out strings.Builder
	buf := make([]byte, 1)
	for out.Len() < maxCodeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", wrapErr(err, "生成短码失败")
		}
		if buf[0] >= 250 {
			continue
		}
		out.WriteByte(digits[int(buf[0])%10])
	}
	return out.String(), nil
}

// ------------------------------------------------------------------ DTO

type createRequest struct {
	Code       string `json:"code" binding:"required"`
	KDFSalt    string `json:"kdfSalt" binding:"required"`
	WrappedDEK string `json:"wrappedDek" binding:"required"`
	DEKIV      string `json:"dekIv" binding:"required"`
}

type redeemRequest struct {
	Code string `json:"code" binding:"required"`
}

type grantDTO struct {
	GrantID   string `json:"grantId"`
	OwnerID   string `json:"ownerId,omitempty"`
	OwnerName string `json:"ownerName,omitempty"`
	Status    string `json:"status"`
	KDFSalt   string `json:"kdfSalt,omitempty"`
	WrappedDEK string `json:"wrappedDek,omitempty"`
	DEKIV     string `json:"dekIv,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"` // 0 表示永久
}

type recordDTO struct {
	Table     string `json:"table"`
	RecordID  string `json:"recordId"`
	Ver       int    `json:"ver"`
	UpdatedAt int64  `json:"updatedAt"`
	Deleted   bool   `json:"deleted"`
	IV        string `json:"iv"`
	CT        string `json:"ct"`
	Seq       int64  `json:"seq"`
}

// ------------------------------------------------------------------ 业务

// CreateGrant 登记一条待兑换授权。服务端不保存明文短码。
func (s *Service) CreateGrant(ctx context.Context, ownerID string, req createRequest) (*grantDTO, error) {
	if ownerID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "登录已失效")
	}
	code, ok := normalizeCode(req.Code)
	if !ok {
		return nil, apierr.New(apierr.CodeBadRequest, "短码格式不正确（8 位数字）")
	}
	salt, err := decodeField(req.KDFSalt, maxSaltLen)
	if err != nil {
		return nil, apierr.New(apierr.CodeBadRequest, "kdfSalt 非法")
	}
	wrapped, err := decodeField(req.WrappedDEK, maxWrapped)
	if err != nil {
		return nil, apierr.New(apierr.CodeBadRequest, "wrappedDek 非法")
	}
	iv, err := decodeField(req.DEKIV, maxSaltLen)
	if err != nil {
		return nil, apierr.New(apierr.CodeBadRequest, "dekIv 非法")
	}

	exp := time.Now().Add(pendingCodeTTL)
	g := &store.FamilyGrant{
		ID:         newID(),
		OwnerID:    ownerID,
		CodeLookup: s.lookupOf(code),
		KDFSalt:    salt,
		WrappedDEK: wrapped,
		DEKIV:      iv,
		ExpiresAt:  &exp,
	}
	if err := s.repo.Create(ctx, g); err != nil {
		return nil, wrapErr(err, "创建授权失败")
	}
	return &grantDTO{
		GrantID:   g.ID,
		Status:    store.FamilyGrantPending,
		CreatedAt: time.Now().UnixMilli(),
		ExpiresAt: exp.UnixMilli(),
	}, nil
}

// Redeem 兑换短码：绑定当前用户为查看方，并把授权转为永久。
func (s *Service) Redeem(ctx context.Context, viewerID, clientIP, code string) (*grantDTO, error) {
	if viewerID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "登录已失效")
	}
	if !s.limiter.allow(clientIP, viewerID) {
		// 限流命中返回 429，不透露短码是否存在
		return nil, apierr.New(apierr.CodeRateLimited, "尝试过于频繁，请稍后再试")
	}

	norm, ok := normalizeCode(code)
	if !ok {
		return nil, apierr.New(apierr.CodeBadRequest, "短码格式不正确")
	}

	g, err := s.repo.Redeem(ctx, s.lookupOf(norm), viewerID)
	if err != nil {
		// 统一文案，不区分「不存在」「已过期」「已被他人兑换」「你已关联该账号」。
		// 理由：短码只有 8 位数字（约 26.6 bit），限流是它唯一的爆破防线。
		// 一旦错误信息能区分状态，攻击者就能把爆破目标从「猜中一个有效码」
		// 降级为「猜中一个曾经存在的码」，命中率提升若干个数量级。
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return nil, apierr.New(apierr.CodeConflict, "短码无效或已被使用")
		}
		return nil, wrapErr(err, "兑换失败")
	}

	return &grantDTO{
		GrantID:    g.ID,
		OwnerID:    g.OwnerID,
		Status:     g.Status,
		KDFSalt:    encodeField(g.KDFSalt),
		WrappedDEK: encodeField(g.WrappedDEK),
		DEKIV:      encodeField(g.DEKIV),
		CreatedAt:  g.CreatedAt.UnixMilli(),
		ExpiresAt:  expiresMilli(g.ExpiresAt),
	}, nil
}

// Records 以查看方身份读取 owner 的密文记录。
//
// 【越权防护】ownerID 只从 grant 行取；grant 必须属于当前用户、状态为 active、未过期。
// 任何一步不满足都返回 not found，且不区分具体原因，避免探测。
func (s *Service) Records(ctx context.Context, viewerID, grantID string, sinceSeq int64, limit int) ([]recordDTO, int64, bool, error) {
	if viewerID == "" {
		return nil, 0, false, apierr.New(apierr.CodeUnauthorized, "登录已失效")
	}
	if grantID == "" {
		return nil, 0, false, apierr.New(apierr.CodeBadRequest, "缺少 grantId")
	}
	if limit <= 0 || limit > maxPageSize {
		limit = maxPageSize
	}

	g, err := s.repo.GetByID(ctx, grantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, 0, false, apierr.New(apierr.CodeNotFound, "授权不存在")
		}
		return nil, 0, false, wrapErr(err, "读取授权失败")
	}
	if g.ViewerID == nil || *g.ViewerID != viewerID || g.Status != store.FamilyGrantActive {
		return nil, 0, false, apierr.New(apierr.CodeNotFound, "授权不存在")
	}
	if g.ExpiresAt != nil && time.Now().After(*g.ExpiresAt) {
		return nil, 0, false, apierr.New(apierr.CodeNotFound, "授权已过期")
	}

	items, nextSeq, hasMore, err := s.repo.ListOwnerRecords(ctx, g.OwnerID, sinceSeq, limit)
	if err != nil {
		return nil, 0, false, wrapErr(err, "读取记录失败")
	}
	out := make([]recordDTO, 0, len(items))
	for _, it := range items {
		out = append(out, recordDTO{
			Table: it.Table, RecordID: it.RecordID, Ver: int(it.Ver),
			UpdatedAt: it.UpdatedAt, Deleted: it.Deleted,
			IV: encodeField(it.IV), CT: encodeField(it.CT), Seq: it.Seq,
		})
	}
	return out, nextSeq, hasMore, nil
}

// AppendRecords 让查看方把药物变更写进数据所有方的账号空间。
//
// 【安全边界】
//   - owner 只从 grant 行取，与只读接口同一套三元校验（未绑定/非本人/非 active
//     一律 404，不区分原因，避免 grantId 变成探测入口）；
//   - 表白名单只放行 medication。放开其它表等于让查看方能改对方的排程、
//     服药历史甚至档案，远超「共同维护药物清单」这个授权语义；
//   - 服务端仍然只搬运密文：查看方早已持有 owner 的 DEK（兑换短码时拿到），
//     能解密就能加密，服务端不需要也不应该接触明文。
//
// 【为什么写 owner 的 user_id 就够了】
// 记录落在 owner 的账号空间，owner 自己的 /sync/pull 下次就会拉到这些变更，
// 不需要为 owner 另建一条推送通道 —— 双向同步靠的是同一条同步链路。
func (s *Service) AppendRecords(ctx context.Context, viewerID, grantID string, changes []model.SyncItem) (*model.SyncPushResponse, error) {
	if viewerID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "登录已失效")
	}
	if grantID == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "缺少 grantId")
	}
	if len(changes) == 0 {
		return nil, apierr.New(apierr.CodeBadRequest, "没有要提交的变更")
	}

	g, err := s.repo.GetByID(ctx, grantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.New(apierr.CodeNotFound, "授权不存在")
		}
		return nil, wrapErr(err, "读取授权失败")
	}
	if g.ViewerID == nil || *g.ViewerID != viewerID || g.Status != store.FamilyGrantActive {
		return nil, apierr.New(apierr.CodeNotFound, "授权不存在")
	}
	if g.ExpiresAt != nil && time.Now().After(*g.ExpiresAt) {
		return nil, apierr.New(apierr.CodeNotFound, "授权已过期")
	}

	// 表白名单：写入面越窄越安全。这里宁可硬拒绝，也不做「过滤掉非法表后照写」，
	// 因为静默过滤会让客户端以为整批都成功了。
	for _, it := range changes {
		if it.Table != model.TableMedication {
			return nil, apierr.Newf(apierr.CodeForbidden,
				"只能修改药物清单，不接受表 %s", it.Table)
		}
		if it.ID == "" {
			return nil, apierr.New(apierr.CodeBadRequest, "变更缺少记录 id")
		}
	}

	if s.sync == nil {
		return nil, apierr.New(apierr.CodeNotImplemented, "共享写入未启用")
	}

	applied, conflicts, cursor, err := s.sync.Apply(ctx, g.OwnerID, changes)
	if err != nil {
		return nil, wrapErr(err, "写入共享药物失败")
	}

	keys := make([]string, 0, len(applied))
	for _, it := range applied {
		keys = append(keys, it.Table+":"+it.ID)
	}
	if conflicts == nil {
		conflicts = []model.SyncItem{}
	}
	return &model.SyncPushResponse{
		Applied:   keys,
		Conflicts: conflicts,
		Cursor:    cursor,
		SyncedAt:  time.Now().UnixMilli(),
	}, nil
}

// ListGrants 返回我作为所有者的授权列表（不含任何密钥材料）。
func (s *Service) ListGrants(ctx context.Context, ownerID string) ([]grantDTO, error) {
	if ownerID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "登录已失效")
	}
	rows, err := s.repo.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, wrapErr(err, "查询授权列表失败")
	}
	out := make([]grantDTO, 0, len(rows))
	for _, g := range rows {
		out = append(out, grantDTO{
			GrantID:   g.ID,
			Status:    g.Status,
			CreatedAt: g.CreatedAt.UnixMilli(),
			ExpiresAt: expiresMilli(g.ExpiresAt),
		})
	}
	return out, nil
}

// Revoke 撤销授权。
func (s *Service) Revoke(ctx context.Context, ownerID, grantID string) error {
	if ownerID == "" {
		return apierr.New(apierr.CodeUnauthorized, "登录已失效")
	}
	if err := s.repo.Revoke(ctx, grantID, ownerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.New(apierr.CodeNotFound, "授权不存在")
		}
		return wrapErr(err, "撤销失败")
	}
	return nil
}

// ------------------------------------------------------------------ HTTP

// Mount 挂载 /family/* 路由。
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc) {
	g := rg.Group("/family")
	g.Use(requireAuth)

	g.POST("/grants", func(c *gin.Context) {
		uid, ok := auth.UserID(c)
		if !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		var req createRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, "请求参数有误"))
			return
		}
		out, err := svc.CreateGrant(c.Request.Context(), uid, req)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusCreated, out)
	})

	// 由服务端生成短码：客户端先取码，再用码派生 KEK 包裹自己的 DEK，
	// 最后带着码与信封调 POST /grants。分两步是因为信封的计算依赖码本身。
	g.POST("/grants/code", func(c *gin.Context) {
		if _, ok := auth.UserID(c); !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		code, err := svc.GenerateCode()
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": code, "length": maxCodeLen, "oneTime": true})
	})

	g.POST("/grants/redeem", func(c *gin.Context) {
		uid, ok := auth.UserID(c)
		if !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		var req redeemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, "请求参数有误"))
			return
		}
		out, err := svc.Redeem(c.Request.Context(), uid, c.ClientIP(), req.Code)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	})

	g.GET("/grants", func(c *gin.Context) {
		uid, ok := auth.UserID(c)
		if !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		out, err := svc.ListGrants(c.Request.Context(), uid)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": out})
	})

	g.DELETE("/grants/:id", func(c *gin.Context) {
		uid, ok := auth.UserID(c)
		if !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		if err := svc.Revoke(c.Request.Context(), uid, c.Param("id")); err != nil {
			writeErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	g.GET("/records", func(c *gin.Context) {
		uid, ok := auth.UserID(c)
		if !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		sinceSeq := parseInt64(c.Query("sinceSeq"))
		limit := int(parseInt64(c.Query("limit")))
		items, nextSeq, hasMore, err := svc.Records(c.Request.Context(), uid, c.Query("grantId"), sinceSeq, limit)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items, "nextSeq": nextSeq, "hasMore": hasMore})
	})

	// 共享写入：查看方修改药物清单，落到数据所有方的账号空间。
	// 与只读接口同样挂 requireAuth，viewer 身份来自 JWT 而非请求体。
	g.POST("/records", func(c *gin.Context) {
		uid, ok := auth.UserID(c)
		if !ok {
			writeErr(c, apierr.New(apierr.CodeUnauthorized, "登录已失效"))
			return
		}
		var req struct {
			GrantID string           `json:"grantId" binding:"required"`
			Changes []model.SyncItem `json:"changes" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			writeErr(c, apierr.New(apierr.CodeBadRequest, "请求体格式错误"))
			return
		}
		resp, err := svc.AppendRecords(c.Request.Context(), uid, req.GrantID, req.Changes)
		if err != nil {
			writeErr(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	})
}

// ------------------------------------------------------------------ 限流

// redeemLimiter 是兑换接口的双窗口限流器（按 IP 与按账号各一份）。
// 进程内实现：本服务单实例部署，够用且零依赖；多实例时应换成共享缓存实现。
type redeemLimiter struct {
	mu       sync.Mutex
	perIP    map[string]*window
	perUser  map[string]*window
	ipLimit  int
	ipWindow time.Duration
	uLimit   int
	uWindow  time.Duration
	lastGC   time.Time
}

type window struct {
	count int
	start time.Time
}

func newRedeemLimiter(ipLimit int, ipWindow time.Duration, userLimit int, userWindow time.Duration) *redeemLimiter {
	return &redeemLimiter{
		perIP: map[string]*window{}, perUser: map[string]*window{},
		ipLimit: ipLimit, ipWindow: ipWindow, uLimit: userLimit, uWindow: userWindow,
		lastGC: time.Now(),
	}
}

func (l *redeemLimiter) hit(m map[string]*window, key string, limit int, dur time.Duration, now time.Time) bool {
	w, ok := m[key]
	if !ok || now.Sub(w.start) > dur {
		m[key] = &window{count: 1, start: now}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

func (l *redeemLimiter) allow(ip, userID string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// 每 10 分钟清一次过期窗口，避免 map 无限增长
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, w := range l.perIP {
			if now.Sub(w.start) > l.ipWindow {
				delete(l.perIP, k)
			}
		}
		for k, w := range l.perUser {
			if now.Sub(w.start) > l.uWindow {
				delete(l.perUser, k)
			}
		}
		l.lastGC = now
	}

	ipOK := l.hit(l.perIP, ip, l.ipLimit, l.ipWindow, now)
	userOK := l.hit(l.perUser, userID, l.uLimit, l.uWindow, now)
	return ipOK && userOK
}

// ------------------------------------------------------------------ 小工具

func decodeField(v string, max int) ([]byte, error) {
	if len(v) > max*2 {
		return nil, errors.New("too long")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) == 0 || len(b) > max {
		return nil, errors.New("invalid base64")
	}
	return b, nil
}

func encodeField(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func expiresMilli(t *time.Time) int64 {
	if t == nil {
		return 0 // 0 表示永久
	}
	return t.UnixMilli()
}

func newID() string { return uuid.NewString() }

func writeErr(c *gin.Context, err error) {
	if e, ok := err.(*apierr.Error); ok {
		c.JSON(apierr.HTTPStatus(e.Code), e)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"code": "internal", "message": "服务异常"})
}

func wrapErr(err error, msg string) error {
	if e, ok := err.(*apierr.Error); ok {
		return e
	}
	return apierr.Wrap(apierr.CodeInternal, err, msg)
}

func parseInt64(v string) int64 {
	var n int64
	for _, r := range strings.TrimSpace(v) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}
