// Package syncsvc 实现加密双向增量同步的服务端部分。
//
// 【同步语义说明（服务端视角）】
//
//  1. 端到端加密：服务端只处理密文信封（iv + ct），永远不解析、不解密、不落任何业务字段
//     （药物名、剂量、成员姓名等）。所有校验只针对信封元数据（table/id/ver/updatedAt/iv/ct）
//     与长度、格式，不涉及明文语义。
//
//  2. 游标 = 服务端 seq：每个用户一行 sync_state 维护单调递增的 last_seq，每条被写入的记录
//     分配一个 seq。pull(afterSeq) 只返回 seq > afterSeq 的记录，响应中的 cursor 是本次
//     返回的最大 seq，客户端需持久化该值，下次以它为起点继续增量拉取。seq 是服务端权威时钟，
//     不依赖客户端时间戳，因此天然免疫客户端时钟漂移。
//
//  3. LWW（Last-Writer-Wins）裁决规则，服务端是权威，裁决在 store 层的一个事务内完成：
//     即：先比 ver（大者胜）；ver 相同再比 updatedAt（大者胜）；两者都相同则「删除优先于修改」。
//     better 为真 → 覆盖写入并进 applied；否则服务端当前版本胜出并进 conflicts。
//
//  4. 墓碑删除：deleted=true 的记录不是物理删除，而是写入一条「墓碑」（ct 允许为空串，iv 仍必填），
//     同样参与版本裁决并占用 seq，保证其他设备拉取时能感知到删除。
//
//  5. 冲突由客户端合并：push 响应的 conflicts[] 直接透传服务端当前胜出版本（含服务端 seq），
//     服务端不代为合并；客户端解密后按上述规则自行合并，并通过 toast 告知用户。
//
//  6. intake 表的特殊说明：intake 是事件型记录（某次服药），语义上更适合追加式合并，
//     但一期为了协议统一，仍采用与其它表完全相同的 LWW 规则；发生冲突时服务端版本胜出
//     并回传给客户端，由客户端决定是否提示用户。
package syncsvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// Service 是同步业务服务，只依赖仓储接口，不感知 HTTP。
type Service struct {
	repo store.SyncRepo
	cfg  config.SyncConfig
}

// NewService 构造同步服务。repo 为 A1 提供的同步仓储，cfg 为同步相关配置。
func NewService(repo store.SyncRepo, cfg config.SyncConfig) *Service {
	return &Service{repo: repo, cfg: cfg}
}

// Push 接收客户端本地变更：先做整体入参校验，再交由仓储在一个事务内完成冲突裁决与写入。
// 返回的 Conflicts 为服务端当前胜出版本（含服务端 seq），供客户端合并。
func (s *Service) Push(ctx context.Context, userID string, req model.SyncPushRequest) (*model.SyncPushResponse, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未认证")
	}
	// 空批次：不写库，仅回传当前游标。
	if len(req.Changes) == 0 {
		cursor, err := s.repo.Cursor(ctx, userID)
		if err != nil {
			return nil, mapRepoError(err)
		}
		return &model.SyncPushResponse{
			Applied:   []string{},
			Conflicts: []model.SyncItem{},
			Cursor:    cursor,
			SyncedAt:  time.Now().UnixMilli(),
		}, nil
	}
	// 先整体校验，任一条不合法则整批拒绝（避免半写入）。
	if err := validatePush(s.cfg, time.Now(), req.Changes); err != nil {
		return nil, err
	}
	applied, conflicts, cursor, err := s.repo.Apply(ctx, userID, req.Changes)
	if err != nil {
		return nil, mapRepoError(err)
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

// Pull 增量拉取：返回 seq > afterSeq 的记录、本次游标与是否还有更多。
// limit <= 0 时取默认分页，超过 MaxPullPageSize 时钳制到上限。
func (s *Service) Pull(ctx context.Context, userID string, afterSeq int64, limit int) (*model.SyncPullResponse, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未认证")
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	if limit <= 0 {
		limit = s.cfg.PullPageSize
	}
	if s.cfg.MaxPullPageSize > 0 && limit > s.cfg.MaxPullPageSize {
		limit = s.cfg.MaxPullPageSize
	}
	items, cursor, hasMore, err := s.repo.Pull(ctx, userID, afterSeq, limit)
	if err != nil {
		return nil, mapRepoError(err)
	}
	if items == nil {
		items = []model.SyncItem{}
	}
	return &model.SyncPullResponse{Items: items, Cursor: cursor, HasMore: hasMore}, nil
}

// Stats 返回该用户的同步概况（游标、记录数、分表统计、服务端时间）。
func (s *Service) Stats(ctx context.Context, userID string) (*model.SyncStatsResponse, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未认证")
	}
	st, err := s.repo.Stats(ctx, userID)
	if err != nil {
		return nil, mapRepoError(err)
	}
	if st.ServerTime == 0 {
		st.ServerTime = time.Now().UnixMilli()
	}
	return &st, nil
}

// Reset 清空该用户全部云端同步记录（危险操作）。确认词校验在 HTTP 层完成。
func (s *Service) Reset(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, "未认证")
	}
	if err := s.repo.Reset(ctx, userID); err != nil {
		return mapRepoError(err)
	}
	return nil
}

// mapRepoError 把仓储错误映射为对外错误码，内部细节不外泄（原始错误只用于日志）。
func mapRepoError(err error) error {
	if err == nil {
		return nil
	}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apierr.New(apierr.CodeNotFound, "同步记录不存在")
	case errors.Is(err, store.ErrConflict):
		return apierr.New(apierr.CodeConflict, "同步版本冲突")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return apierr.New(apierr.CodeUnavailable, "请求已取消或超时")
	default:
		// 不回显底层错误文本，仅记日志。
		slog.Error("同步仓储错误", "error", err)
		return apierr.New(apierr.CodeInternal, "同步服务暂时不可用，请稍后重试")
	}
}
