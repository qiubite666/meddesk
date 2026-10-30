// Package fake 提供四个仓储的**纯内存**实现，仅用于集成冒烟测试。
//
// 设计目的：本机没有 PostgreSQL / Redis / Docker，无法跑真实的端到端测试。
// 本包用 sync.Mutex + map 模拟 PG 的语义（唯一约束、LWW 冲突裁决、seq 单调递增、
// ErrNotFound 等），让 HTTP 全链路（路由 → 中间件 → 服务 → 仓储）可以在
// net/http/httptest 下被完整验证。
//
// 与真实实现的边界（便于定位差异）：
//   - 只依赖标准库 + google/uuid，不做任何 SQL / 网络调用；
//   - 错误语义严格对齐 internal/store：未找到返回 store.ErrNotFound，
//     邮箱重复返回 *apierr.Error{Code: apierr.CodeConflict}；
//   - 附件只登记元数据，密文内容由 attachments.Service 真实落盘（测试里
//     Upload.Dir 指向 t.TempDir()），因此 fake 不持有文件字节；
//   - 不具备事务性：Apply 中途不会失败，因此「回滚」语义在 fake 中不可观测。
package fake

import (
	"context"
	"sync"

	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// Repos 是内存版仓储聚合，对应 server.Repos 接口（也天然满足生产用的 *store.Store 形状）。
//
// 全部数据由一把互斥锁保护：集成测试里的 HTTP 请求是并发可达的（httptest 不保证串行），
// 真实仓储由 PG 事务保证一致性，这里退化为全库一把锁，语义足够且不会死锁。
type Repos struct {
	mu sync.Mutex

	// users: 主键 id -> 用户；emailIdx: 小写邮箱 -> 用户 id（模拟 citext 唯一索引）。
	users    map[string]*model.User
	emailIdx map[string]string

	// refresh: 主键 id -> 刷新令牌；refreshHashIdx: hex(sha256(令牌明文)) -> id。
	refresh        map[string]*model.RefreshToken
	refreshHashIdx map[string]string

	// syncStates: 用户 id -> 该用户的同步记录集合与 seq 计数器（对应 sync_state 行）。
	syncStates map[string]*syncState

	// attachments: 用户 id -> (附件 id -> 附件元数据)。
	attachments map[string]map[string]*model.Attachment

	// ledger 是记账本的内存数据（见 ledger.go），懒初始化。
	ledger *ledgerStore
	// ledgerAttachments 是账本凭证附件的内存数据（见 ledger_attachment.go）。
	ledgerAttachments map[string]*model.LedgerAttachment
}

// New 创建一套空的内存仓储。
func New() *Repos {
	return &Repos{
		users:             make(map[string]*model.User),
		emailIdx:          make(map[string]string),
		refresh:           make(map[string]*model.RefreshToken),
		refreshHashIdx:    make(map[string]string),
		syncStates:        make(map[string]*syncState),
		attachments:       make(map[string]map[string]*model.Attachment),
		ledgerAttachments: make(map[string]*model.LedgerAttachment),
	}
}

// Ping 探测连通性；内存仓储永远可用，恒返回 nil（对应 *store.Store.Ping）。
func (r *Repos) Ping(context.Context) error { return nil }

// Users 返回用户仓储（每次调用返回同一份底层数据的轻量视图）。
func (r *Repos) Users() store.UserRepo { return userRepo{r} }

// Refresh 返回刷新令牌仓储。
func (r *Repos) Refresh() store.RefreshRepo { return refreshRepo{r} }

// Sync 返回同步仓储。
func (r *Repos) Sync() store.SyncRepo { return syncRepo{r} }

// Attachments 返回附件仓储。
func (r *Repos) Attachments() store.AttachmentRepo { return attachmentRepo{r} }

// 编译期断言：四个 fake 仓储均实现对应的 store 接口。
var (
	_ store.UserRepo       = userRepo{}
	_ store.RefreshRepo    = refreshRepo{}
	_ store.SyncRepo       = syncRepo{}
	_ store.AttachmentRepo = attachmentRepo{}
)
