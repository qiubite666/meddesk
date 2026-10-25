// Package store 提供 MedDesk 的数据访问层：PostgreSQL 连接池、schema 迁移与四个仓储。
//
// 设计约定：
//   - 所有 SQL 均使用 pgx 占位符（$1、$2…）参数化，禁止字符串拼接。
//   - 仓储层对外只暴露三种错误：ErrNotFound、ErrConflict 与 *apierr.Error。
//     底层错误一律用 fmt.Errorf("...: %w", err) 包装后收敛，不泄漏驱动细节。
//   - 端到端加密（E2EE）：本层只搬运密文字节（iv/ct），永不接触明文业务数据。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

var (
	// ErrNotFound 表示目标记录不存在。调用方应用 errors.Is 判断。
	ErrNotFound = errors.New("store: 记录不存在")
	// ErrConflict 表示写入与既有数据冲突（典型场景：邮箱已被注册）。
	// 需要 HTTP 语义时，仓储会返回 Code 为 apierr.CodeConflict 的 *apierr.Error。
	ErrConflict = errors.New("store: 数据冲突")
)

// Store 持有一个 PostgreSQL 连接池，是全部仓储的入口。
type Store struct {
	pool *pgxpool.Pool
}

// Open 解析数据库 URL 并建立连接池。config 中的 MaxConns/MinConns/MaxConnLifetime
// 会覆盖 URL 查询串中的同名参数（仅在取值为正时覆盖）。建立后会立即 Ping 一次，
// 以便数据库不可用时在启动阶段就暴露问题。
func Open(ctx context.Context, cfg config.DBConfig) (*Store, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("打开数据库失败: %w", errors.New("数据库 URL 为空"))
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("解析数据库 URL 失败: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("创建数据库连接池失败: %w", err)
	}

	s := &Store{pool: pool}
	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return s, nil
}

// Close 关闭连接池并释放全部连接。
func (s *Store) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

// Ping 探测数据库连通性，用于健康检查与启动自检。
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("数据库连接池未初始化")
	}
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("数据库探测失败: %w", err)
	}
	return nil
}

// Pool 返回底层连接池，供需要事务或高级用法的调用方使用（请谨慎使用）。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Users 返回用户仓储。
func (s *Store) Users() UserRepo { return &userRepo{s: s} }

// Refresh 返回刷新令牌仓储。
func (s *Store) Refresh() RefreshRepo { return &refreshRepo{s: s} }

// Sync 返回同步仓储。
func (s *Store) Sync() SyncRepo { return &syncRepo{s: s} }

// Attachments 返回附件仓储。
func (s *Store) Attachments() AttachmentRepo { return &attachmentRepo{s: s} }

// ------------------------------------------------------------------ 仓储接口定义

// UserRepo 是 users 表的仓储接口。
type UserRepo interface {
	// Create 写入新用户；邮箱重复时返回 *apierr.Error（Code=apierr.CodeConflict）。
	// 传入的 u.ID 为空时由仓储生成 UUID；创建时间由数据库回填到 u。
	Create(ctx context.Context, u *model.User) error
	// GetByID 按主键查询，未找到返回 ErrNotFound。
	GetByID(ctx context.Context, id string) (*model.User, error)
	// GetByEmail 按邮箱查询（大小写不敏感），未找到返回 ErrNotFound。
	GetByEmail(ctx context.Context, email string) (*model.User, error)
	// UpdateCredentials 更新口令哈希与密钥信封（改口令 / 重新包裹 DEK 时使用）。
	// 用户不存在时返回 ErrNotFound。
	UpdateCredentials(ctx context.Context, id, passwordHash string, kdfSalt, wrappedDEK, dekIV []byte) error
}

// RefreshRepo 是 refresh_tokens 表的仓储接口。
type RefreshRepo interface {
	// Create 写入新刷新令牌记录（库中只保存令牌的 sha256）。
	Create(ctx context.Context, t *model.RefreshToken) error
	// GetByID 按主键查询，未找到返回 ErrNotFound。
	GetByID(ctx context.Context, id string) (*model.RefreshToken, error)
	// GetByHash 按令牌哈希查询，未找到返回 ErrNotFound。
	GetByHash(ctx context.Context, hash []byte) (*model.RefreshToken, error)
	// MarkReplaced 标记令牌已被 newID 轮换掉（同时置 revoked_at）。未找到返回 ErrNotFound。
	MarkReplaced(ctx context.Context, id, replacedBy string) error
	// Revoke 吊销单个令牌；令牌已吊销或不存在时返回 nil（幂等）。
	Revoke(ctx context.Context, id string) error
	// RevokeAllForUser 吊销该用户的全部有效令牌；无令牌时返回 nil（幂等）。
	RevokeAllForUser(ctx context.Context, userID string) error
	// DeleteExpired 物理删除 expires_at 早于 before 的令牌，返回删除行数。
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// SyncRepo 是 sync_records / sync_state 的仓储接口。
type SyncRepo interface {
	// Apply 在单个事务内完成冲突裁决、写入与 seq 分配，返回已应用项、冲突项与最新游标。
	// 冲突项为服务端当前胜出版本（含其 seq），客户端据此合并。items 为空时直接返回当前游标。
	Apply(ctx context.Context, userID string, items []model.SyncItem) (applied []model.SyncItem, conflicts []model.SyncItem, cursor int64, err error)
	// Pull 按 seq > afterSeq 升序取至多 limit 条；hasMore 表示是否还有下一页，
	// cursor 为本页最大 seq（无数据时等于 afterSeq）。
	Pull(ctx context.Context, userID string, afterSeq int64, limit int) (items []model.SyncItem, cursor int64, hasMore bool, err error)
	// Cursor 返回该用户当前最大 seq；无 sync_state 行时返回 0。
	Cursor(ctx context.Context, userID string) (int64, error)
	// Stats 返回同步概况（游标、记录数、分表统计与服务端时间）。
	Stats(ctx context.Context, userID string) (model.SyncStatsResponse, error)
	// Reset 清空该用户的全部同步记录并归零游标（危险操作，仅附件不受影响）。
	Reset(ctx context.Context, userID string) error
}

// AttachmentRepo 是 attachments 表的仓储接口。
type AttachmentRepo interface {
	// Create 写入附件元数据；a.ID 为空时由仓储生成 UUID，创建时间回填到 a。
	Create(ctx context.Context, a *model.Attachment) error
	// Get 按用户与附件 id 查询，未找到返回 ErrNotFound。
	Get(ctx context.Context, userID, id string) (*model.Attachment, error)
	// Delete 删除附件元数据，未找到返回 ErrNotFound，成功返回 nil。
	Delete(ctx context.Context, userID, id string) error
	// List 列出附件；recordID 为空表示列出该用户全部附件，按创建时间倒序。
	List(ctx context.Context, userID, recordID string) ([]model.Attachment, error)
}
