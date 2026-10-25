# MedDesk 契约文档（v0.1.0）

> 本文是**唯一事实来源**。所有子 agent 只依赖本文定义的接口与数据结构，
> **严禁修改本人不拥有的文件**，严禁跨包改写他人逻辑。如发现契约缺陷，
> 在最终汇报中写明「建议修改点」，由 lead 统一裁决，不得擅自改动。

---

## 0. 项目概览

- **名称**：MedDesk（个人数字工作台 · 一期「用药提醒」）
- **模块路径**：`github.com/qiubite666/meddesk`
- **语言/版本**：Go 1.26.4；前端原生 HTML/CSS/JS（ES2019+，**零外部依赖、零 CDN**）
- **技术选型**：PostgreSQL 18（主存；16+ 兼容）+ Redis 7（缓存/限流/刷新令牌吊销）+ Nginx（TLS 终止与反向代理）+ Gin（HTTP）+ pgx/v5（驱动）+ JWT（访问令牌）
- **核心安全模型**：**端到端加密（E2EE）**。服务端只存密文信封（iv + ct），永远看不到明文业务数据。
- **数据层**：客户端 IndexedDB 本地优先（离线可用），后台与服务端做**加密双向增量同步**。

### 目录与文件归属（越界即冲突）

| 归属 | 路径 | 说明 |
|---|---|---|
| **lead** | `go.mod`/`go.sum`、`assets.go`、`cmd/meddesk/main.go`、`internal/server/`、`internal/config/`、`internal/apierr/`、`internal/model/` | 已冻结，任何人只读 |
| **A1** | `internal/store/**`、`internal/store/migrations/*.sql` | 数据库与仓储层 |
| **A2** | `internal/cache/**`、`internal/middleware/**` | 缓存与通用中间件 |
| **A3** | `internal/auth/**` | 认证与授权 |
| **A4** | `internal/syncsvc/**` | 同步服务 |
| **A5** | `internal/attachments/**`、`internal/export/**` | 附件与导出 |
| **A6** | `web/index.html`、`web/assets/css/app.css`、`web/manifest.webmanifest`、`web/sw.js`、`web/assets/js/{core,crypto,db,data,api,sync,app}.js` | 前端平台层/壳 |
| **A7** | `web/assets/js/schedule.js`、`web/assets/js/remind.js` | 排程引擎与提醒（纯逻辑） |
| **A8** | `web/assets/js/med.js`、`web/assets/js/export.js` | 用药模块视图与导出交互 |
| **A9** | `deploy/**`、`docs/**`、`README.md`、`.env.example`、`Makefile` | 运维与文档 |

> `web/.gitkeep` 已存在（供 `//go:embed all:web` 使用）。**禁止删除**。

---

## 1. 通用编码约束（所有 Go 子 agent 必须遵守）

1. **禁止**运行 `go mod tidy` / `go get` / 修改 `go.mod`、`go.sum`。依赖已由 lead 预置：
   `gin v1.12`、`pgx/v5`、`go-redis/v9`、`golang-jwt/jwt/v5`、`google/uuid`、`golang.org/x/crypto`、`joho/godotenv`、`go-pdf/fpdf`。
   确需新增依赖 → 在汇报中写明，由 lead 添加。
2. 只编译自己的包：`go build ./internal/<own>/... && go vet ./internal/<own>/...`。
   **不要** `go build ./...`（他人包可能尚未完成）。
3. 所有错误响应统一使用 `apierr`；**handler 不得直接 `c.JSON(400, ...)` 裸写**。
4. 日志使用标准库 `log/slog`，从 `*slog.Logger` 参数注入，不要自建全局 logger。
5. 注释与用户可见文案一律**简体中文**；标识符用英文。
6. 所有 SQL 使用参数化查询（`pgx` 占位符 `$1`），禁止字符串拼接。
7. 上下文：所有 I/O 函数第一个参数为 `ctx context.Context`。
8. 导出的函数/类型必须有中文注释（golint 风格）。

---

## 2. Go 侧契约

### 2.1 `internal/config`（已冻结，只读）

```go
func Load() (*Config, error)
type Config struct {
    Env, HTTPAddr, BaseURL string
    TrustProxy bool
    WebDir     string            // 非空=开发模式从磁盘读静态资源
    DB         DBConfig          // URL/MaxConns/MinConns/MaxConnLifetime/MigrateOnBoot
    Redis      RedisConfig       // URL/Enabled
    Auth       AuthConfig        // JWTSecret/AccessTTL/RefreshTTL/Argon2*/SignupEnabled/RefreshReuseGrace
    Sync       SyncConfig        // MaxItemsPerPush/MaxRecordBytes/PullPageSize/MaxPullPageSize
    Upload     UploadConfig      // Dir/MaxBytes
    PDF        PDFConfig         // FontPath
    Log        LogConfig         // Level/Format
    RateLimit  RateLimitConfig   // RPS/Burst
    CORS       CORSConfig        // AllowedOrigins
}
func (c *Config) IsDev() bool
```

### 2.2 `internal/apierr`（已冻结，只读）

```go
type Code string
const (CodeBadRequest Code = "bad_request"; CodeUnauthorized; CodeForbidden; CodeNotFound;
       CodeConflict; CodePayloadTooLarge; CodeRateLimited; CodeUnavailable; CodeNotImplemented; CodeInternal)
type Error struct{ Code Code; Message string; Details interface{} }
func New(c Code, msg string) *Error
func Newf(c Code, format string, args ...interface{}) *Error
func (e *Error) WithDetails(d interface{}) *Error
func Wrap(c Code, err error, msg string) *Error
func HTTPStatus(c Code) int
func Is(err error, c Code) bool
func FromError(err error) *Error
```

### 2.3 `internal/model`（已冻结，只读）

见源码。关键类型：`SyncItem`、`SyncPushRequest/Response`、`SyncPullResponse`、`SyncStatsResponse`、
`RegisterRequest`、`LoginRequest`、`RefreshRequest`、`LogoutRequest`、`ChangePasswordRequest`、
`TokenResponse`、`UserDTO`、`MeResponse`、`User`、`RefreshToken`、`Attachment`、`AttachmentDTO`、
`ExportRequest`、`ExportRow`、`ExportStats`、`HealthResponse`；常量 `TableProfile/TableMedication/TableSchedule/TableIntake/TableSetting`、
`KnownTables()`、`IsKnownTable()`。

### 2.4 `internal/store`（A1 实现）

```go
var ErrNotFound = errors.New("store: not found")
var ErrConflict = errors.New("store: conflict")

type Store struct{ /* 内部持有 *pgxpool.Pool */ }
func Open(ctx context.Context, cfg config.DBConfig) (*Store, error)
func (s *Store) Close()
func (s *Store) Ping(ctx context.Context) error
func (s *Store) Migrate(ctx context.Context) error   // 执行内嵌 SQL 迁移，幂等
func (s *Store) Pool() *pgxpool.Pool
func (s *Store) Users() UserRepo
func (s *Store) Refresh() RefreshRepo
func (s *Store) Sync() SyncRepo
func (s *Store) Attachments() AttachmentRepo

type UserRepo interface {
    Create(ctx context.Context, u *model.User) error                       // 邮箱重复返回 apierr.CodeConflict
    GetByID(ctx context.Context, id string) (*model.User, error)
    GetByEmail(ctx context.Context, email string) (*model.User, error)     // 未找到返回 ErrNotFound
    UpdateCredentials(ctx context.Context, id, passwordHash string, kdfSalt, wrappedDEK, dekIV []byte) error
}

type RefreshRepo interface {
    Create(ctx context.Context, t *model.RefreshToken) error
    GetByID(ctx context.Context, id string) (*model.RefreshToken, error)
    GetByHash(ctx context.Context, hash []byte) (*model.RefreshToken, error)
    MarkReplaced(ctx context.Context, id, replacedBy string) error
    Revoke(ctx context.Context, id string) error
    RevokeAllForUser(ctx context.Context, userID string) error
    DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

type SyncRepo interface {
    // Apply 在一个事务内完成冲突裁决与写入，返回已应用项、冲突项与最新游标。
    Apply(ctx context.Context, userID string, items []model.SyncItem) (applied []model.SyncItem, conflicts []model.SyncItem, cursor int64, err error)
    Pull(ctx context.Context, userID string, afterSeq int64, limit int) (items []model.SyncItem, cursor int64, hasMore bool, err error)
    Cursor(ctx context.Context, userID string) (int64, error)
    Stats(ctx context.Context, userID string) (model.SyncStatsResponse, error)
    Reset(ctx context.Context, userID string) error   // 清空该用户全部同步记录（危险操作）
}

type AttachmentRepo interface {
    Create(ctx context.Context, a *model.Attachment) error
    Get(ctx context.Context, userID, id string) (*model.Attachment, error)
    Delete(ctx context.Context, userID, id string) error   // 返回 ErrNotFound 或 nil
    List(ctx context.Context, userID, recordID string) ([]model.Attachment, error) // recordID=="" 表示全部
}
```

**冲突裁决规则（必须严格实现，服务端是权威）**：

```
better(in, cur) := in.Ver > cur.Ver
                || (in.Ver == cur.Ver && in.UpdatedAt > cur.UpdatedAt)
                || (in.Ver == cur.Ver && in.UpdatedAt == cur.UpdatedAt && in.Deleted && !cur.Deleted)
better → 覆盖写入，进 applied；否则 → 当前服务端版本进 conflicts（客户端负责合并）
```

**序列号分配**：每用户一行 `sync_state(user_id, last_seq)`；一个事务内
`UPDATE sync_state SET last_seq = last_seq + $n WHERE user_id=$1 RETURNING last_seq`，
再倒序为每条 applied 记录分配 seq。`sync_records.seq` 是 pull 的游标依据。

**迁移**：`internal/store/migrations/0001_init.sql`（up）与可选 `0001_init.down.sql`；
用 `//go:embed migrations/*.sql` 内嵌，自建 `schema_migrations(version, applied_at)` 表保证幂等。
必须包含的表：`users`、`refresh_tokens`、`sync_records`、`sync_state`、`attachments`、`schema_migrations`。
字段见第 5 节数据模型。合理建立索引与外键（用户删除级联）。

### 2.5 `internal/cache`（A2 实现）

```go
var ErrMiss = errors.New("cache: miss")

type Cache interface {
    Ping(ctx context.Context) error
    Close() error
    Get(ctx context.Context, key string) (string, error)          // 未命中 → ErrMiss
    GetInt(ctx context.Context, key string) (int64, error)
    Set(ctx context.Context, key string, value string, ttl time.Duration) error
    SetNX(ctx context.Context, key string, value string, ttl time.Duration) (bool, error)
    Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
    Del(ctx context.Context, keys ...string) error
}
func Open(ctx context.Context, cfg config.RedisConfig) (Cache, error) // cfg.Enabled==false → NewNoop()
func NewNoop() Cache   // 进程内 map + TTL + 惰性清理，用于 Redis 不可用时降级
```

### 2.6 `internal/middleware`（A2 实现）

```go
func RequestID() gin.HandlerFunc                                  // 注入 X-Request-ID，写入 gin 上下文键 "request_id"
func Logger(log *slog.Logger) gin.HandlerFunc                     // 结构化访问日志
func Recovery(log *slog.Logger) gin.HandlerFunc                   // panic → apierr.CodeInternal JSON
func CORS(origins []string) gin.HandlerFunc                       // origins 为空=回显同源请求
func SecurityHeaders() gin.HandlerFunc                            // CSP/HSTS/X-Content-Type-Options/Referrer-Policy/Permissions-Policy
func BodyLimit(maxBytes int64) gin.HandlerFunc                    // 超限 → apierr.CodePayloadTooLarge
func RateLimit(c cache.Cache, rps float64, burst int) gin.HandlerFunc // 令牌桶，键=客户端 IP（c.ClientIP()），超限 → 429 + Retry-After
```
> `middleware` **不得** import `internal/auth`（避免循环依赖）。

### 2.7 `internal/auth`（A3 实现）

```go
type RequestMeta struct{ IP, UserAgent, DeviceID string }

func NewService(users store.UserRepo, refresh store.RefreshRepo, cache cache.Cache, cfg config.AuthConfig) *Service

func (s *Service) Register(ctx context.Context, req model.RegisterRequest, meta RequestMeta) (*model.TokenResponse, error)
func (s *Service) Login(ctx context.Context, req model.LoginRequest, meta RequestMeta) (*model.TokenResponse, error)
func (s *Service) Refresh(ctx context.Context, refreshToken string, meta RequestMeta) (*model.TokenResponse, error)
func (s *Service) Logout(ctx context.Context, refreshToken string) error
func (s *Service) LogoutAll(ctx context.Context, userID string) error
func (s *Service) ChangePassword(ctx context.Context, userID string, req model.ChangePasswordRequest) error
func (s *Service) Authorize(accessToken string) (userID string, err error)   // 校验 JWT 且检查 cache 吊销名单

func NewHandler(svc *Service) *Handler
func Mount(rg *gin.RouterGroup, svc *Service)              // 自挂 /auth/* 子路由
func RequireAuth(svc *Service) gin.HandlerFunc             // 校验 Authorization: Bearer <jwt>，写入上下文键 "user_id"
func UserID(c *gin.Context) (string, bool)
```

**口令与令牌规范**：
- 服务端**收不到明文口令**：收到的是客户端 PBKDF2 结果 `passwordHash`（base64）。
  服务端再用 **argon2id**（`golang.org/x/crypto/argon2`，参数取自 `cfg.Auth`）对其做二次哈希后存储，
  存储串格式 `$argon2id$v=19$m=...,t=...,p=...$<b64salt>$<b64hash>`。
- 访问令牌：JWT（HS256，claim `sub`=userID、`typ`="access"、`exp`=AccessTTL）。
- 刷新令牌：32 字节随机 → base64url 发给客户端；**库中只存 sha256 值**。
  刷新时**轮换**（旧令牌标记 `replaced_by` 并吊销）；检测到已吊销令牌被复用（重放攻击）→
  撤销该用户全部刷新令牌并返回 `apierr.CodeUnauthorized`。宽限期 `RefreshReuseGrace` 内允许并发刷新。
- 登录失败计数写入 cache（`auth:fail:<emailHash>`，TTL 15 分钟，超过 10 次返回 `CodeRateLimited`）。
- `MEDDESK_SIGNUP_ENABLED=false` 时注册接口返回 `CodeForbidden`。

**路由**：`POST /api/v1/auth/register`、`/login`、`/refresh`、`/logout`、`/logout-all`、`/change-password`、`GET /api/v1/auth/me`。
（后缀统一用小写连字符，如 `logout-all`。）

### 2.8 `internal/syncsvc`（A4 实现）

```go
func NewService(repo store.SyncRepo, cfg config.SyncConfig) *Service
func (s *Service) Push(ctx context.Context, userID string, req model.SyncPushRequest) (*model.SyncPushResponse, error)
func (s *Service) Pull(ctx context.Context, userID string, afterSeq int64, limit int) (*model.SyncPullResponse, error)
func (s *Service) Stats(ctx context.Context, userID string) (*model.SyncStatsResponse, error)
func (s *Service) Reset(ctx context.Context, userID string) error
func NewHandler(svc *Service) *Handler
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc)
```
**校验**：表名必须在 `model.KnownTables()` 内；`id` 长度 ≤64；单次条数 ≤ `cfg.MaxItemsPerPush`；
单条密文 ≤ `cfg.MaxRecordBytes`；`iv`/`ct` 必须是合法 base64；`updatedAt` 必须 > 0 且不超过当前时间 +5 分钟。
违规 → `apierr.CodeBadRequest` 并在 `details` 中给出具体条目。

**路由**：`POST /api/v1/sync/push`、`GET /api/v1/sync/pull?cursor=&limit=`、`GET /api/v1/sync/stats`、`POST /api/v1/sync/reset`。

### 2.9 `internal/attachments`（A5 实现）

```go
func NewService(repo store.AttachmentRepo, cfg config.UploadConfig) *Service
func (s *Service) Save(ctx context.Context, userID, recordID string, iv []byte, mime string, r io.Reader, size int64) (*model.AttachmentDTO, error)
func (s *Service) Open(ctx context.Context, userID, id string) (*model.Attachment, io.ReadCloser, error)
func (s *Service) Delete(ctx context.Context, userID, id string) error
func (s *Service) List(ctx context.Context, userID, recordID string) ([]model.AttachmentDTO, error)
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc)
```
- 密文落盘路径：`{UploadDir}/{userID}/{yy}/{id}.bin`；**不落任何明文**。
- 上传接口：`POST /api/v1/attachments`，`multipart/form-data`，字段 `file`（二进制密文）、`iv`（base64）、`recordId`（可选）。
  限制 `cfg.MaxBytes`（超限 `CodePayloadTooLarge`）；mime 白名单 `image/jpeg,image/png,image/webp,application/octet-stream`。
- 下载：`GET /api/v1/attachments/:id`，返回 `application/octet-stream`，响应头 `X-MedDesk-IV: <base64>`。
- 删除：`DELETE /api/v1/attachments/:id`；列表：`GET /api/v1/attachments?recordId=`。

### 2.10 `internal/export`（A5 实现）

```go
func NewService(cfg config.PDFConfig) *Service
func (s *Service) RenderCSV(w io.Writer, req model.ExportRequest) error   // UTF-8 BOM + CRLF
func (s *Service) RenderPDF(w io.Writer, req model.ExportRequest) error   // go-pdf/fpdf + CJK TTF
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc)
```
- 路由：`POST /api/v1/export/csv`、`POST /api/v1/export/pdf`。
- **隐私说明（必须写进代码注释与文档）**：E2EE 下服务端无法解密，故导出由客户端把
  已解密数据一次性 POST 过来，**仅在内存中渲染，绝不落盘、绝不写日志**；
  请求体最大 4 MiB，响应头 `Cache-Control: no-store`、`Content-Disposition: attachment`。
- PDF：A4 纵向；标题 + 汇总区（总次数/已服/跳过/漏服/依从率）+ 表格（日期/时间/成员/药物/剂量/状态/备注）
  + 页脚（生成时间、页码）。中文字体：`cfg.PDF.FontPath`，为空时按顺序探测
  `/opt/meddesk/fonts/NotoSansSC-Regular.ttf`、`/usr/share/fonts/truetype/noto/NotoSansSC-Regular.ttf`、
  `/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc`、`/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc`；
  全部失败 → 返回 `apierr.CodeUnavailable`，message 写明「请先安装中文字体或配置 MEDDESK_PDF_FONT_PATH」。
- 状态中文映射：`taken=已服用`、`skipped=已跳过`、`snoozed=已推迟`、`pending=待服用`、`missed=已漏服`。

---

## 3. HTTP API 一览

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/api/v1/health` | 否 | 健康检查（lead 实现） |
| GET | `/api/v1/ready` | 否 | 就绪检查（含 DB/Cache 探测） |
| POST | `/api/v1/auth/register` | 否 | 注册 |
| POST | `/api/v1/auth/login` | 否 | 登录 |
| POST | `/api/v1/auth/refresh` | 否 | 刷新令牌（轮换） |
| POST | `/api/v1/auth/logout` | 否 | 撤销单个刷新令牌 |
| POST | `/api/v1/auth/logout-all` | 是 | 撤销本用户全部刷新令牌 |
| POST | `/api/v1/auth/change-password` | 是 | 改口令并重新包裹 DEK |
| GET | `/api/v1/auth/me` | 是 | 当前用户 |
| POST | `/api/v1/sync/push` | 是 | 上传变更 |
| GET | `/api/v1/sync/pull` | 是 | 增量拉取 |
| GET | `/api/v1/sync/stats` | 是 | 同步概况 |
| POST | `/api/v1/sync/reset` | 是 | 清空云端同步数据 |
| POST | `/api/v1/attachments` | 是 | 上传加密附件 |
| GET | `/api/v1/attachments` | 是 | 列表 |
| GET | `/api/v1/attachments/:id` | 是 | 下载密文 |
| DELETE | `/api/v1/attachments/:id` | 是 | 删除 |
| POST | `/api/v1/export/csv` | 是 | 导出 CSV |
| POST | `/api/v1/export/pdf` | 是 | 导出 PDF |

统一响应约定：成功直接返回业务对象；失败返回
`{"code":"...","message":"...","details":...}` 且 HTTP 状态码由 `apierr.HTTPStatus` 决定。

---

## 4. 端到端加密与同步协议（客户端，A6 实现；A7/A8 只读依赖）

1. 客户端生成 16 字节 salt，`authHash = PBKDF2-SHA256(password, salt, 210000, 32B)` → base64 送服务端。
2. `KEK = HKDF-SHA256(authHash, salt, info="meddesk-kek-v1", 32B)`；`DEK = 随机 32 字节`。
3. `wrappedDEK = AES-GCM(KEK, iv12, DEK)`，连同 salt、iv 存服务端（服务端无法解开）。
4. 业务记录在本地加密：`AES-GCM(DEK, iv12, JSON.stringify(record))` → `{iv, ct}` 上传。
5. 同步游标：客户端保存 `cursor`（服务端 seq），`pull?cursor=N` 只取增量。
6. 冲突：push 返回的 `conflicts[]` 由客户端解密后按第 2.4 节规则合并，并通过 toast 告知用户。
7. 附件（照片）同样在客户端压缩（最长边 ≤ 1280px，JPEG q=0.82）后加密上传。

---

## 5. 数据模型

### 5.1 服务端关系模型（PostgreSQL）

```
users(id uuid pk, email citext unique not null, password_hash text not null,
      kdf_salt bytea not null, wrapped_dek bytea not null, dek_iv bytea not null,
      created_at timestamptz default now(), updated_at timestamptz default now())

refresh_tokens(id uuid pk, user_id uuid fk users on delete cascade, token_hash bytea unique not null,
      device_id text, user_agent text, ip inet_NULLABLE(text), expires_at timestamptz not null,
      created_at timestamptz default now(), revoked_at timestamptz, replaced_by uuid)
   idx: (user_id), (expires_at)

sync_records(user_id uuid fk users on delete cascade, table_name text not null, record_id text not null,
      ver bigint not null default 1, updated_at bigint not null, deleted boolean not null default false,
      iv bytea not null, ct bytea not null, device_id text, seq bigint not null,
      updated_seq timestamptz default now(),
      primary key (user_id, table_name, record_id))
   idx: (user_id, seq), (user_id, table_name)

sync_state(user_id uuid pk fk users on delete cascade, last_seq bigint not null default 0,
      updated_at timestamptz default now())

attachments(id uuid pk, user_id uuid fk users on delete cascade, record_id text,
      iv bytea not null, size bigint not null, mime text not null, path text not null,
      seq bigint not null default 0, created_at timestamptz default now())
   idx: (user_id, record_id)

schema_migrations(version text pk, applied_at timestamptz default now())
```
> 若目标实例未安装 `citext` 扩展，请在迁移中 `CREATE EXTENSION IF NOT EXISTS citext;`；
> 或退化为 `text` + `lower(email)` 唯一索引（由 A1 决定并在迁移注释中说明）。

### 5.2 客户端域模型（明文，仅存在于浏览器内存与备份文件中）

```js
Profile    = { id, name, relation, birthDate, color, note, archived, updatedAt, ver, deleted }
Medication = { id, profileId, name, dosageAmount, dosageUnit, form, strength,
               foodInstruction, prescriber, pharmacy, sideEffects, notes,
               photoKeys: [], critical: bool, active: bool, startDate, endDate,
               stock, refillThreshold, updatedAt, ver, deleted }
Schedule   = { id, medicationId, kind, times:["08:00"], intervalHours, everyN, unit,
               specificDates:["2026-10-25"], cycleOnDays, cycleOffDays, cycleStart,
               startDate, endDate, remindOffsetMin, critical, updatedAt, ver, deleted }
Intake     = { id, occKey, medicationId, profileId, scheduledFor, status,
               takenAt, doseText, note, snoozeUntil, updatedAt, ver, deleted }
```
`kind` 取值：`daily` | `twice_daily` | `interval_hours` | `specific_dates` | `custom_interval` | `cycle`。
`status` 取值：`pending` | `taken` | `skipped` | `snoozed`。

---

## 6. 前端契约

### 6.1 加载顺序（`index.html`，A6 负责）

```
core.js → crypto.js → db.js → api.js → data.js → sync.js → schedule.js → remind.js → med.js → export.js → app.js
```
全部为经典脚本（非 ES module），统一挂在全局 `window.MD` 命名空间下。

### 6.2 DOM 骨架（A6 必须提供，A8 依赖）

```html
<div class="app" id="app">
  <aside class="sidebar" id="sidebar">
    <div class="sidebar__brand" id="brand"></div>
    <nav class="sidebar__nav" id="nav-list"></nav>      <!-- 路由注册后自动渲染 -->
    <div class="sidebar__foot" id="sidebar-foot"></div>
  </aside>
  <div class="scrim" id="scrim" hidden></div>
  <div class="main">
    <header class="topbar" id="topbar">
      <button id="btn-menu">…</button>
      <h1 id="page-title"></h1>
      <div id="sync-indicator"></div>
      <button id="btn-theme"></button>
      <button id="btn-account"></button>
    </header>
    <main class="main__body" id="app-main"></main>       <!-- 路由视图挂载点 -->
    <nav class="tabbar" id="tabbar"></nav>               <!-- 仅移动端显示 -->
  </div>
</div>
<div id="toast-host" class="toast-host"></div>
<div id="modal-host" class="modal-host" hidden></div>
<div id="auth-screen" class="auth-screen"></div>          <!-- 未登录时盖住全屏 -->
```
路由切换时由 `MD.router` 在 `#app-main` 内创建 `<section class="view" id="view-<id>">` 并传入该元素给 `render(el)`。

### 6.3 CSS 变量与工具类（A6 定义，A8 只能用这些）

变量：`--bg --surface --surface-2 --border --text --text-dim --brand --brand-weak --danger --warn --ok
--radius --radius-lg --space --shadow --font --dur`。
工具类：`.card .card--flat .btn .btn-primary .btn-ghost .btn-danger .btn-icon .chip .chip--active
.badge .badge--danger .badge--warn .badge--ok .list .list-item .field .label .input .select .textarea
.seg .seg__item .grid .stack .row .spacer .section-title .muted .empty .fab .ring .heat .divider`。
> A8 如需新样式，**在 `med.js` 内注入一段 `<style>`**（前缀 `.md-`），不得改 `app.css`。

### 6.4 全局 API（A6 提供）

```js
MD.bus.on(evt, fn) / .off(evt, fn) / .emit(evt, payload)      // 事件：'data:changed' 'sync:state' 'auth:changed'
MD.util.uuid() .nowMs() .dayKey(ms) .fmtDate(ms) .fmtTime(ms) .fmtDateTime(ms)
        .debounce(fn, ms) .escapeHtml(s) .clamp(n, a, b) .startOfDay(ms) .addDays(ms, n)
MD.toast.show(msg, kind)            // kind: info|success|warn|error
MD.haptic.tap() / .success() / .warn()          // navigator.vibrate 兜底，失败静默
MD.theme.get() / .set('light'|'dark'|'auto') / .toggle() / .onChange(fn)
MD.ui.el(tag, attrs, children) / .html(node, html) / .clear(node)
       .confirm({title, message, confirmText, danger}) -> Promise<bool>
       .sheet({title, body(el), actions:[{label, kind, onClick}]}) -> 打开底部抽屉/模态
       .closeSheet()
MD.router.register({id, title, subtitle, icon, group, order, comingSoon, render(el)})
MD.router.go(id) / .current() / .start()
MD.crypto.generateSalt() -> Uint8Array(16)
MD.crypto.derive(password, salt) -> Promise<{hashB64}>                  // PBKDF2 210k
MD.crypto.newDEK() -> Uint8Array(32)
MD.crypto.wrapDEK(dek, password, salt) -> Promise<{wrappedB64, ivB64}>
MD.crypto.unwrapDEK(wrappedB64, ivB64, password, salt) -> Promise<Uint8Array>
MD.crypto.encrypt(dek, plain) -> Promise<{ivB64, ctB64}>
MD.crypto.decrypt(dek, ivB64, ctB64) -> Promise<string>
MD.db.get(table, id) / .put(table, rec) / .del(table, id) / .list(table) / .clear(table)   // 均返回 Promise
MD.data.<profiles|meds|schedules|intakes>.list(filter?) .get(id) .save(obj) .remove(id)
MD.data.blobs.save(key, blobOrDataURL) .get(key) .remove(key)
MD.data.backup.export() -> Promise<Object>     // 明文完整快照
MD.data.backup.import(obj) -> Promise<void>    // 合并导入（同 id 取 updatedAt 大者）
MD.data.wipeAll() -> Promise<void>
MD.data.onChange(fn)
MD.api.register({email, password}) -> Promise<session>
MD.api.login({email, password}) -> Promise<session>    // session={userId,access,refresh,expiresAt,kdfSalt,wrappedDek,dekIv}
MD.api.refresh() / .logout() / .me()
MD.api.push(changes) / .pull(cursor, limit) / .stats() / .reset()
MD.api.uploadAttachment({recordId, ivB64, blob}) / .getAttachment(id) / .deleteAttachment(id)
MD.api.postExport(format, payload) -> Promise<Blob>
MD.api.onUnauthorized(fn)        // 401 且刷新失败 → 触发重新登录
MD.sync.state                    // 'idle'|'syncing'|'offline'|'error'
MD.sync.run() -> Promise<{pushed, pulled, conflicts}>
MD.sync.startAuto(ms) / .stopAuto() / .onChange(fn) / .markDirty()
```
> 所有记录经 `MD.data.*.save()` 写入时自动补全 `id/updatedAt/ver++`，并置脏、触发同步与 `data:changed`。

### 6.5 A7 提供的排程引擎（`schedule.js` / `remind.js`，无 DOM 依赖）

```js
MD.sched.KINDS = [{id:'daily',label:'每日'},{id:'twice_daily',label:'每日两次'},
  {id:'interval_hours',label:'按小时间隔'},{id:'specific_dates',label:'指定日期'},
  {id:'custom_interval',label:'自定义间隔'},{id:'cycle',label:'周期性用药（21天服/7天停）'}]
MD.sched.occKey(schedule, timeMs) -> string           // `${schedule.id}@${timeMs}`
MD.sched.isActiveOn(schedule, dayKey) -> boolean
MD.sched.occurrences(schedule, fromMs, toMs) -> [{ timeMs, medId, schedId, occKey }]
MD.sched.nextAfter(schedule, afterMs) -> occ|null
MD.sched.describe(schedule) -> string                 // 人类可读："每日 08:00、20:00"
MD.sched.buildToday({meds, schedules, intakes, profileId, nowMs}) -> DoseVM[]
   // DoseVM={occKey, medId, schedId, profileId, medName, dosageText, timeMs, status, intakeId, overdue, critical}
MD.sched.adherence({intakes, fromMs, toMs}) -> {total, taken, skipped, missed, rate, streakDays}
MD.remind.init() / .start() / .stop()
MD.remind.ensurePermission() -> Promise<'granted'|'denied'|'default'>
MD.remind.schedule(doses)      // 由 A8 传入今日待提醒列表
MD.remind.snooze(occKey, minutes)
MD.remind.onFire(cb)           // cb(dose)
MD.remind.state -> {supported, permission, running}
```

### 6.6 A8 提供的视图（`med.js` / `export.js`）

```js
MD.med.register()      // 向 MD.router 注册 id='medication' 的路由（app.js 在启动时调用）
MD.med.openMedForm(medId|null)   // 供其他模块复用（预留）
MD.export.rows({from, to, profileId}) -> Promise<ExportRow[]>
MD.export.csv(rows) / MD.export.pdf(rows)     // csv 纯前端 Blob 下载；pdf 走 MD.api.postExport
```

### 6.7 交互与视觉要求（A6/A7/A8 共同）

- **移动端优先**：`<768px` 单列；按钮最小 44×44px；输入字号 ≥16px；`env(safe-area-inset-bottom)`。
- **PC**：左侧固定侧栏（可折叠）；**移动端**：汉堡抽屉 + 底部 Tab（最多 4 项）。
- 深浅色主题（CSS 变量 + `prefers-color-scheme` 自动 + 手动切换，写入 localStorage）。
- 过渡动画 `120–240ms cubic-bezier(.2,.8,.2,1)`；尊重 `prefers-reduced-motion`。
- 触觉反馈：`MD.haptic.tap()` 于所有关键点击。
- **零外链**：不引任何 CDN/字体/图表库；图标用内联 SVG path（**禁用 emoji 当图标**）；统计图表用内联 SVG 手写。
- 首屏顶部必须是「**今天要处理**」：逾期标红 + 一键「已服用/跳过/推迟」；昨天未处理的自动顺延到今天。
- 首次打开预置 3–5 条示例数据（**含 1 条逾期**），并提供「清空示例数据」按钮。
- 设置页提供「导出 JSON 备份 / 导入恢复 / 清空全部数据」（二次确认）。
- 未来模块入口（健康记录、日程待办、家庭账本…）在侧栏展示为「即将上线」，点击给出提示。

---

## 7. 环境变量（`A9` 据此写 `.env.example`）

| 变量 | 默认 | 说明 |
|---|---|---|
| `MEDDESK_ENV` | `prod` | dev/prod |
| `MEDDESK_HTTP_ADDR` | `:8080` | 监听地址 |
| `MEDDESK_BASE_URL` | `http://localhost:8080` | 站点外链基址 |
| `MEDDESK_TRUST_PROXY` | `false` | 信任 Nginx 的 X-Forwarded-For |
| `MEDDESK_WEB_DIR` | 空 | 非空则从磁盘读静态资源（开发用） |
| `MEDDESK_DB_URL` | `postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable` | PG 连接串 |
| `MEDDESK_DB_MAX_CONNS` | `10` | 连接池上限 |
| `MEDDESK_DB_MIN_CONNS` | `1` | 连接池下限 |
| `MEDDESK_DB_MAX_CONN_LIFETIME` | `30m` | 连接最大存活 |
| `MEDDESK_DB_MIGRATE_ON_BOOT` | `true` | 启动时自动迁移 |
| `MEDDESK_REDIS_ENABLED` | `true` | 关闭则走进程内降级 |
| `MEDDESK_REDIS_URL` | `redis://127.0.0.1:6379/0` | Redis 连接串 |
| `MEDDESK_JWT_SECRET` | **必填** | ≥32 字节 |
| `MEDDESK_ACCESS_TTL` | `15m` | 访问令牌有效期 |
| `MEDDESK_REFRESH_TTL` | `720h` | 刷新令牌有效期 |
| `MEDDESK_ARGON2_MEMORY` | `65536` | KiB |
| `MEDDESK_ARGON2_ITERATIONS` | `3` | t |
| `MEDDESK_ARGON2_PARALLELISM` | `2` | p |
| `MEDDESK_SIGNUP_ENABLED` | `true` | 关闭公开注册 |
| `MEDDESK_REFRESH_REUSE_GRACE` | `30s` | 并发刷新宽限 |
| `MEDDESK_SYNC_MAX_ITEMS` | `500` | 单次 push 上限 |
| `MEDDESK_SYNC_MAX_RECORD_BYTES` | `1048576` | 单条密文上限 |
| `MEDDESK_SYNC_PULL_PAGE` | `300` | 默认分页 |
| `MEDDESK_SYNC_PULL_PAGE_MAX` | `1000` | 最大分页 |
| `MEDDESK_UPLOAD_DIR` | `/var/lib/meddesk/uploads` | 附件目录 |
| `MEDDESK_UPLOAD_MAX_BYTES` | `8388608` | 单附件上限 |
| `MEDDESK_PDF_FONT_PATH` | 空 | CJK TTF 路径 |
| `MEDDESK_LOG_LEVEL` | `info` | debug/info/warn/error |
| `MEDDESK_LOG_FORMAT` | `json` | json/text |
| `MEDDESK_RATE_LIMIT_RPS` | `20` | 每 IP 速率 |
| `MEDDESK_RATE_LIMIT_BURST` | `60` | 突发 |
| `MEDDESK_CORS_ALLOWED_ORIGINS` | 空 | 逗号分隔，空=同源 |

---

## 8. 完成标准（DoD）

- 每个 Go 包 `go build` + `go vet` 通过，无未使用导入/变量。
- 每个 JS 文件 `node --check` 通过，无未定义全局引用（按本文命名空间核对）。
- 不越界修改他人文件；Git 工作区不出现与契约冲突的改动。
- 汇报格式：① 新增/修改文件清单；② 对外暴露的签名；③ 自测方式与结果；④ 假设与建议修改点。
