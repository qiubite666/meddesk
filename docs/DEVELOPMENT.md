# MedDesk 开发文档（新人上手指南）

> **适用版本**：v0.3.50（Go 模块 `github.com/qiubite666/meddesk`）
> **适用环境**：开发（dev）/ 测试（test）/ 生产（prod）—— 各章节标题下方标注
> **目标**：新人半天内完成「拉代码 → 起依赖 → 跑服务 → 跑测试 → 改一行前端并看到效果」
> **配套文档**：`docs/API.md`（接口契约）、`docs/DEPLOYMENT.md`（部署）、`docs/OPERATIONS.md`（运维）、`CONTRACTS.md`（端到端契约）、`docs/DATA-MODEL.md`

**环境标记约定**

| 标记 | 含义 |
| --- | --- |
| 🟢 开发 | 本机 `go run` / Docker 依赖容器，可随意破坏 |
| 🟡 测试 | CI 或本机 `go test`，无外部依赖也可跑（`internal/fake`） |
| 🔴 生产 | 服务器 systemd + nginx，改动需走发布流程 |

---

## 1. 项目结构

**适用环境**：🟢 开发 / 🟡 测试 / 🔴 生产　**适用版本**：v0.3.50

```
meddesk/
├── assets.go                  # 一行 //go:embed all:web，把整个 web/ 内嵌进二进制
├── go.mod / go.sum            # Go 1.26.4；gin 1.12 / pgx v5 / jwt v5 / redis v9 / godotenv / fpdf
├── Makefile                   # 全部常用命令入口（make help 可列全）
├── meddesk.env.example        # 环境变量样例（唯一可提交的配置文件）
├── CONTRACTS.md               # 端到端加密与同步契约，改加密/同步前必读
├── README.md                  # 产品与快速上手
├── cmd/
│   ├── meddesk/               # 服务端唯一入口 main.go：配置 → 连库 → 迁移 → 起 HTTP
│   └── meddesk-admin/         # 运维辅助命令行入口
├── internal/                  # 全部后端代码（Go 内部包，外部不可引用）
│   ├── server/                # 路由组装、SPA 与 /admin 静态服务、健康探针
│   ├── config/                # 环境变量 → Config 结构体，含校验
│   ├── apierr/                # 统一错误码与错误响应体
│   ├── model/                 # 服务端 DTO（请求/响应结构体）
│   ├── store/                 # pgx 连接池 + 迁移 + 各业务仓储（唯一写 SQL 的地方）
│   ├── middleware/            # 请求ID/恢复/日志/安全头/CORS/限流/体积限制
│   ├── cache/                 # Redis 与进程内 Noop 两种实现，统一 Cache 接口
│   ├── auth/                  # 注册登录、JWT、刷新令牌、验证码、托管恢复公钥
│   ├── syncsvc/               # 加密同步（push/pull/stats/reset）
│   ├── attachments/           # 加密附件落盘与读取（健康数据侧，密文）
│   ├── ledger/                # 记账本（明文建表）、凭证附件、导出导入、每日备份
│   ├── family/                # 家庭关联：短码兑换 + 跨账号只读
│   ├── webdav/                # WebDAV 代理（浏览器无法直连网盘，由本站转发）
│   ├── admin/                 # 管理后台 API（白名单邮箱 + 独立口令）
│   ├── export/                # 用药历史 CSV / PDF 导出
│   └── fake/                  # 内存版仓储，仅供集成测试，无 PG/Redis 也能跑
├── web/                       # 前端：原生 JS + PWA，无构建步骤
│   ├── index.html             # 主应用入口（服务端注入 ?v=<buildID>）
│   ├── admin.html + admin-app.js  # 管理后台页面（独立入口，no-store）
│   ├── sw.js                  # Service Worker，含 SHELL_REV 与预缓存清单
│   ├── manifest.webmanifest   # PWA 清单
│   └── assets/                # css/app.css + js/*.js（28 个业务脚本）
├── deploy/
│   ├── docker-compose.yml          # 生产全套编排
│   ├── docker-compose.dev.yml      # 本地开发依赖（PG18 + Redis7）
│   ├── docker-compose.middleware.yml # 仅中间件
│   ├── Dockerfile                  # 生产镜像构建
│   ├── systemd/meddesk.service     # systemd 单元（EnvironmentFile=.meddesk.env）
│   ├── nginx/meddesk.conf          # 反向代理与 TLS 配置
│   ├── postgres-tuning.conf        # PG 参数建议
│   ├── scripts/                    # backup.sh / restore.sh / install.sh / get-font.sh
│   └── scripts/initdb/00-init.sql  # 首次建库建用户脚本
├── android/                   # Capacitor 生成的 Android 工程（整体 .gitignore，仅 alarm/ 下 3 个 Kotlin 文件强制跟踪）
├── capacitor-plugins/         # 自研 Capacitor 插件源码（meddesk-alarm）
├── capacitor.config.json      # Capacitor 打包配置
├── package.json               # 仅 Capacitor 相关，前端无 npm 构建
├── docs/                      # 全部文档（本文件在此）
├── tmp/                       # 开发期临时脚本/调试产物（已被 .gitignore 忽略）
└── dist/                      # 历史发布产物（已被 .gitignore 忽略）
```

---

## 2. 模块职责

**适用环境**：🟢 开发 / 🟡 测试 / 🔴 生产　**适用版本**：v0.3.50

路由表以 `internal/server/router.go` 为准（全部挂在 `/api/v1` 下，除 `/api/v1/health`、`/api/v1/ready` 外默认需鉴权）。

| 包 | 职责 | 入口文件 | 对外接口前缀 |
| --- | --- | --- | --- |
| `server` | 组装 gin 引擎、中间件链、各模块挂载、SPA 静态服务 | `internal/server/router.go`（`New`） | `/api/v1/health`、`/api/v1/ready`、`/`、`/admin`、`/sw.js`、`/assets/*` |
| `config` | 环境变量 → 配置结构体 + 校验 | `internal/config/config.go`（`Load`） | — |
| `apierr` | 统一错误码与 `{"code","message","details"}` 响应 | `internal/apierr/errors.go` | — |
| `model` | 服务端 DTO（请求/响应结构） | `internal/model/model.go` 等 | — |
| `store` | pgx 连接池、`Migrate`、全部仓储（唯一写 SQL 处） | `internal/store/store.go`（`Open`） | — |
| `middleware` | 请求ID / 恢复 / 日志 / 安全头 / CORS / 限流 / 体积限制 | `internal/middleware/*.go` | — |
| `cache` | Redis 与进程内降级实现 | `internal/cache/cache.go`（`Open`） | — |
| `auth` | 注册登录、JWT 签发校验、刷新令牌、图形验证码、托管恢复；**导出 `RequireAuth` 给其它模块复用** | `internal/auth/handler.go`（`Mount`） | `/api/v1/auth/*` |
| `syncsvc` | 加密增量双向同步（服务端只见密文） | `internal/syncsvc/handler.go`（`Mount`） | `/api/v1/sync/*` |
| `attachments` | 健康侧加密附件落盘/读取/删除/列表 | `internal/attachments/http.go`（`Mount`） | `/api/v1/attachments/*` |
| `ledger` | 记账本 CRUD + 统计；**明文建表** | `internal/ledger/http.go`（`Mount`） | `/api/v1/ledger/*` |
| `ledger`（附件） | 账单凭证图片（明文存储，仅所属账号可访问） | `internal/ledger/attachment_http.go`（`MountAttachments`） | `/api/v1/ledger/attachments/*` |
| `ledger`（导出） | 账单导出 HTML/ZIP/JSON 与导入 | `internal/ledger/export_http.go`（`MountExport`） | `/api/v1/ledger/export/*`、`/api/v1/ledger/import` |
| `ledger`（备份） | 每日自动备份到用户 WebDAV；凭据服务端加密 | `internal/ledger/backup_http.go`（`MountBackup`） | `/api/v1/ledger/backup/*` |
| `family` | 家庭关联：8 位短码生成/兑换/撤销、跨账号只读 | `internal/family/family.go`（`Mount`） | `/api/v1/family/*` |
| `webdav` | WebDAV 代理（凭据随请求带入，不落库不记日志，做 SSRF 与路径校验） | `internal/webdav/webdav.go`（`RegisterWebDAV` / `NewClient`） | `/api/v1/webdav/*` |
| `admin` | 管理后台：概览、用户增删改、重置口令、清同步数据 | `internal/admin/admin.go`（`Mount`） | `/api/v1/admin/*` |
| `export` | 用药历史 CSV / PDF 导出 | `internal/export/http.go`（`Mount`） | `/api/v1/export/*` |
| `fake` | 内存版仓储，只在测试中使用 | `internal/fake/repos.go` | — |

**关键路由清单（v0.3.50）**

| 前缀 | 主要端点 | 鉴权 |
| --- | --- | --- |
| `/api/v1/auth` | `GET /captcha`、`POST register|login|salt|refresh|logout`、`POST logout-all|change-password`、`GET /recovery-key`、`POST /recovery-envelope|/repair-envelope`、`GET /me` | 部分需登录 |
| `/api/v1/sync` | `POST /push`、`GET /pull`、`GET /stats`、`POST /reset` | 需登录 |
| `/api/v1/attachments` | `POST ""`、`GET ""`、`GET /:id`、`DELETE /:id` | 需登录 |
| `/api/v1/export` | `POST /csv`、`POST /pdf` | 需登录 |
| `/api/v1/ledger` | `/categories`（GET/POST/PATCH/DELETE）、`/records`（CRUD）、`GET /summary|/trend|/breakdown`、`GET /reimbursements`、`POST /reimbursements/mark` | 需登录 |
| `/api/v1/ledger/attachments` | `POST ""`、`GET ""`、`GET /:id`、`DELETE /:id` | 需登录 |
| `/api/v1/ledger` | `POST /export/html`、`POST /export/zip`、`GET /export/json`、`POST /import` | 需登录 |
| `/api/v1/ledger` | `PUT /backup/credentials`、`GET /backup/status`、`POST /backup/run`、`DELETE /backup/credentials` | 需登录 |
| `/api/v1/family` | `POST /grants`、`POST /grants/code`、`POST /grants/redeem`、`GET /grants`、`DELETE /grants/:id`、`GET /records`、`POST /records` | 需登录 |
| `/api/v1/webdav` | `POST /test`、`/list`、`/mkdir`、`/delete`、`/upload`、`/download` | 需登录 |
| `/api/v1/admin` | `GET /salt`、`POST /login`、`GET /overview`、`GET|POST /users`、`PATCH /users/:id`、`POST /users/:id/password`、`DELETE /users/:id` | 后台令牌 |

**挂载约定**：各业务包统一暴露 `Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc)`，内部自己 `rg.Group("/xxx")` 并 `g.Use(requireAuth)`。可选模块（ledger / family / admin）在 `router.go` 里用**接口断言**挂载——仓储没实现对应方法就跳过，测试用的 `internal/fake` 内存替身因此不受影响。

---

## 3. 技术选型说明

**适用环境**：🟢 开发 / 🟡 测试 / 🔴 生产　**适用版本**：v0.3.50

| 选型 | 为什么选 | 代价 / 约束 |
| --- | --- | --- |
| **Go 1.26 + gin** | 单静态二进制、交叉编译直接 scp/rsync 上线；启动快、内存小，适合小 VPS；gin 生态成熟、中间件链清晰 | 无泛型 ORM，SQL 全手写；错误处理靠约定（`apierr`）而非框架 |
| **PostgreSQL 18** | 强事务、`numeric` 精确十进制、`citext` 邮箱唯一索引、`pg_advisory_xact_lock` 天然解决多实例迁移竞争 | 需自行维护迁移脚本；单机部署，横向扩展靠连接池上限约束 |
| **pgx v5** | 原生 PG 协议、性能与类型支持好于 `database/sql` 通用驱动；支持简单协议执行多语句迁移脚本 | 参数必须显式类型（如 advisory lock 键需 `int64`）；团队需熟悉 pgx API |
| **原生 JS（无框架、无构建）** | ① 零构建步骤，前端可直接 `//go:embed` 进二进制，交付物是**一个文件**；② 无 npm 依赖树，供应链攻击面几乎为零、无版本升级负担；③ PWA 离线可用，代码体积可控；④ 页面数量有限、交互以表单与列表为主，框架的组件化收益不明显 | 无类型检查、无组件复用，DOM 需手工维护；XSS 防护靠人工纪律（`textContent`/转义）；`sw.js` 预缓存清单需与 `index.html` 手工同步；新人上手需先读 `core.js` 的 `MD.*` 约定 |
| **PWA（Service Worker）** | 离线打开、可安装到桌面/Android（Capacitor 复用同一套 web 资源） | 缓存失效是踩坑重灾区，见第 5 章 `SHELL_REV` 与 `buildID` |
| **Docker Compose** | 本机一条命令起 PG/Redis，避免污染宿主机；生产可只用它跑中间件 | 生产中间件容器与 systemd 应用进程混管，排障要两边看（`docker compose logs` + `journalctl`） |
| **systemd** | 自带重启、日志（journald）、安全加固（`ProtectSystem=strict` 等），无需容器守护 | 单元文件需随部署同步；`EnvironmentFile` 权限必须 0640 |
| **nginx** | TLS 终止、安全头、`X-Forwarded-For` 传递、静态长缓存；应用只监听 `127.0.0.1:8080` | 必须设 `MEDDESK_TRUST_PROXY=true` 才能拿到真实客户端 IP；应用只信任 `127.0.0.1`/`::1` 作为代理 |

### 3.1 为什么健康数据必须 E2E，而记账本不用

| 维度 | 健康数据（E2E） | 记账本（明文） |
| --- | --- | --- |
| 客户端 | PBKDF2-SHA256 / 210000 轮 → `authHash` → HKDF(info=`meddesk-kek-v1`) → KEK → AES-GCM 包裹 DEK | 无需密钥派生 |
| 服务端只存 | `kdf_salt` / `wrapped_dek` / `dek_iv` + 密文 | 明文字段（`amount` 等） |
| 服务端能力 | **无法**读取、聚合、统计、搜索 | 可 `SUM/GROUP BY` 做汇总、趋势、分类占比 |
| 备份 | 只能整包密文搬运 | 可每日自动备份成可读报表上传用户 WebDAV |

**为什么账本放弃 E2E**：账本的核心需求是**服务端聚合统计**（月度汇总、趋势、分类占比）与**每日自动备份**。密文在服务端不可计算，若强行 E2E，统计只能在客户端拉取全量后算（数据量一大即不可用），每日备份也只能原样搬密文（无法生成可读对账文件）。因此账本主动降级为明文，**以「传输 TLS + 账号级访问控制 + 备份文件保密」作为补偿控制**。

**代价（必须让使用者知情）**：账本金额、分类、备注、凭证图片以明文落库与落盘；任何拿到数据库或备份文件的人都能读到。文档与界面需明确区分「健康数据端到端加密 / 账本明文存储」两条线。

**账本 WebDAV 凭据保护**：`MEDDESK_SECRETBOX_KEY`（base64 的 32 字节）经 AES-256-GCM 加密后落盘，AAD 绑定 `user_id|字段名`——把密文复制到别的账号或别的字段都会解密失败（见 `internal/ledger/credstore.go`）。未配置该环境变量时备份功能整体不可用（接口会给出明确提示）。

---

## 4. 本地环境搭建

**适用环境**：🟢 开发（主机 + Git Bash / Linux 均可）　**适用版本**：v0.3.50

### 4.1 依赖清单

| 依赖 | 版本要求 | 说明 |
| --- | --- | --- |
| Go | ≥ 1.26.4（`go.mod` 声明） | 交叉编译不需要额外工具链 |
| Docker + Compose | 任意较新版本 | 用于 `make deps-up` 起 PG/Redis；不用 Docker 时可在宿主机自行安装 PG18/Redis7 |
| PostgreSQL | 18 | 不强制：应用启动会自动迁移建表 |
| Redis | 7（**可选**） | `MEDDESK_REDIS_ENABLED=false` 时走进程内降级实现，功能等价 |
| Node | 可选 | 仅用于 `make check-js`（`node --check` 语法自检）与 `make check-yaml` 回退路径 |
| psql | 可选 | 仅 `make psql` / `make db-reset` 需要 |

### 4.2 一条命令起依赖

```bash
make deps-up      # docker compose -f deploy/docker-compose.dev.yml up -d
                  # 起 PG18 与 Redis7，账号/口令见 deploy/docker-compose.dev.yml（开发口令，仅限本机）
make deps-down    # 停止并保留数据卷
make deps-down DOWN_V=1   # 连数据卷一起清（等于从零开始）
```

> 开发用 compose 里的口令是**硬编码的开发口令**，仅限本机，禁止用于生产。

### 4.3 配置文件

| 场景 | 文件名 | 谁读它 |
| --- | --- | --- |
| 本机开发 | 仓库根目录 `.env` | `config.Load()` 里的 `godotenv.Load()`（无参，只认仓库根目录 `.env`） |
| 生产 systemd | `/home/ops/service/meddesk/.meddesk.env` | `meddesk.service` 的 `EnvironmentFile=` |
| Docker Compose | 仓库根目录 `.env` | compose 的 `env_file` |

```bash
cp meddesk.env.example .env          # 本机开发（推荐）
# 或用 make 目标（默认写 .env，可用 ENV_FILE=.meddesk.env 覆盖）
make dev-init
```

随后必须替换所有 `<...>` 占位符，至少：

| 变量 | 说明 | 本机建议值（示例占位符） |
| --- | --- | --- |
| `MEDDESK_ENV` | `dev` / `prod`，`dev` 下 CORS 与日志更宽松 | `dev` |
| `MEDDESK_JWT_SECRET` | **必填，≥32 字节**；`openssl rand -base64 48` | `<openssl rand -base64 48 的输出>` |
| `MEDDESK_DB_URL` | PG 连接串 | `postgres://meddesk:<开发口令>@127.0.0.1:5432/meddesk?sslmode=disable` |
| `MEDDESK_REDIS_ENABLED` | 默认 `false`（进程内降级） | `false` |
| `MEDDESK_SIGNUP_ENABLED` | 是否开放注册；公网必须 `false` | `true`（本机） |
| `MEDDESK_HTTP_ADDR` | 监听地址 | `:8080` |
| `MEDDESK_UPLOAD_DIR` | 附件落盘目录，不能为空 | `./data` |
| `MEDDESK_ADMIN_EMAILS` | 后台白名单邮箱，**未配置则后台不挂载** | `<你的开发邮箱>` |
| `MEDDESK_WEB_DIR` | 非空则从磁盘读前端（改 JS 免重编 Go） | `./web` |
| `MEDDESK_SECRETBOX_KEY` | 账本 WebDAV 凭据加密密钥（base64 32 字节），不配则备份不可用 | `<openssl rand -base64 32 的输出>` |

> ⚠️ **`.env` / `.meddesk.env` / 任何 `*.env` 绝不提交**。`.gitignore` 已忽略 `.env`、`*.env`、`.meddesk.env`；仓库里只提交 `meddesk.env.example`。

### 4.4 启动与访问

```bash
# 方式 A（推荐，前端免重编）：从磁盘读 web/
MEDDESK_WEB_DIR=./web go run ./cmd/meddesk
# 等价于
make dev

# 方式 B（走内嵌前端，验证 embed 是否生效）
make build && ./bin/meddesk
# 或
make run

# 只校验配置与依赖连通性（不起服务）
go run ./cmd/meddesk -check
# 只执行迁移后退出
go run ./cmd/meddesk -migrate
# 打印版本号
go run ./cmd/meddesk -version
```

| 地址 | 用途 |
| --- | --- |
| `http://127.0.0.1:8080/` | 主应用（PWA） |
| `http://127.0.0.1:8080/api/v1/health` | 存活探针，返回版本/DB/缓存状态 |
| `http://127.0.0.1:8080/api/v1/ready` | 就绪探针（DB 挂 = 503；缓存挂 = degraded） |
| `http://127.0.0.1:8080/admin` | 管理后台页面 |
| `http://127.0.0.1:5432` | PG（容器映射） |
| `http://127.0.0.1:6379` | Redis（容器映射） |

**初始化数据**：项目**没有种子数据脚本**。表结构由启动时的自动迁移创建（`MEDDESK_DB_MIGRATE_ON_BOOT=true`，幂等，记录在 `schema_migrations` 表）。首个账号通过 `/api/v1/auth/register` 注册（需 `MEDDESK_SIGNUP_ENABLED=true`）；注册后该账号邮箱若在 `MEDDESK_ADMIN_EMAILS` 内即可登录 `/admin`。

### 4.5 Windows 本机（Git Bash）特别说明

- 所有命令在 **Git Bash** 下执行（不要 cmd.exe / PowerShell）；路径用正斜杠：`cd /d/workbuddy/工作台/meddesk`。
- `make` 目标内部大量依赖 `bash` 语法与 `docker`/`psql`/`node` 命令；缺失时对应目标会失败，可直接按 Makefile 里的原始命令手动执行。
- `make db-reset` 用 `read -p` 交互确认，Git Bash 下可正常使用；输入 `RESET` 才执行。
- 生产服务器一律用 **SSH 别名 `web`** 访问，文档中不要写真实 IP：

```bash
ssh web                                   # 登录生产机
ssh web 'journalctl -u meddesk -n 100 --no-pager'
ssh web 'systemctl status meddesk'
make deploy HOST=web REMOTE_DIR=/opt/meddesk/bin
```

- 交叉编译产出在 Windows 上需要注意：目标若为 Linux，产出的二进制**不要**在本机双击运行。

### 4.6 生产构建与发布（🔴 生产）

```bash
# 推荐：显式注入版本号（变量在 internal/server）
GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w -X github.com/qiubite666/meddesk/internal/server.Version=v0.3.50" \
  -o bin/meddesk ./cmd/meddesk

# 或用 Makefile（注意：见「已知风险」第 1 条，make build 的版本注入路径与代码不一致）
make build          # → bin/meddesk
make build-all      # linux-amd64 / linux-arm64 / darwin-arm64 / windows-amd64
make build-linux    # 仅 GOOS=linux GOARCH=amd64
make docker-build TAG=v0.3.50
```

---

## 5. 开发调试流程

**适用环境**：🟢 开发 / 🟡 测试　**适用版本**：v0.3.50

### 5.1 后端测试

```bash
# 全量（等价于 make test）
go test ./... -count=1

# 单个包 + 单个用例
go test ./internal/auth/ -run TestRegister -v
go test ./internal/ledger/ -run 'TestBackup.*' -v

# 只看失败（过滤掉 ok / no test files）
go test ./... -count=1 2>&1 | grep -E '^(FAIL|---)|^\s+---'

# 竞态 + 覆盖率
go test ./... -race -count=1
go test ./... -coverprofile=cover.out && go tool cover -html=cover.out

# 静态检查与格式化
make vet        # go vet ./...
make fmt        # gofmt -l -w .
make check      # vet + test
```

集成测试不需要 PG/Redis：`internal/fake` 提供内存版仓储，`internal/server/integration_test.go`、`ledger_integration_test.go` 在 `httptest` 下跑完整 HTTP 链路。

### 5.2 前端改动如何生效（最容易踩的坑）

前端**无构建步骤**，但会被 `//go:embed all:web` 编进二进制：

| 场景 | 做法 | 是否需要重新编译 Go |
| --- | --- | --- |
| 本机开发（推荐） | 用 `MEDDESK_WEB_DIR=./web go run ./cmd/meddesk` 或 `make dev`，服务端从磁盘读 | **不需要**，刷新即可 |
| 验证内嵌产物 | 改完 `web/` 后 `make build` 再运行 | **需要** |
| 只改 `sw.js` / `index.html` | 同上，仍需重编（它们也是 embed 的一部分） | **需要** |

> 提示：`web/.gitkeep` 不能删——`//go:embed all:web` 依赖它保证目录存在。`make embed-check` 会校验。

### 5.3 PWA / 缓存排障

服务端在启动时会做三件事（见 `router.go` 的 `mountSPA`）：

1. 对内嵌前端全部文件算 sha256，取前 12 位作为 `buildID`；
2. 把 `index.html` 里所有 `/assets/...` 引用注入 `?v=<buildID>`，并把 `buildID` 与版本号写进 `<meta>`（CSP `script-src 'self'` 会拦内联脚本，所以**不能用内联 `<script>`**）；
3. 把 `sw.js` 里的 `__BUILD_ID__` 占位符替换成同一个 `buildID`，作为 SW 缓存名后缀，`activate` 时自动清理旧缓存。

缓存策略：

| 路径 | 策略 |
| --- | --- |
| `/assets/*` | `public, max-age=31536000, immutable` + ETag（靠 URL 上的 `?v=` 破缓存） |
| `/`、`/index.html`、`/sw.js`、`/manifest.webmanifest` | `no-cache, must-revalidate` + ETag 协商 |
| `/admin`、`/admin/`、`/admin/app.js` | `no-store, no-cache, must-revalidate, max-age=0`（后台页不进 SW 预缓存） |

**发版后用户仍看到旧页面的排查顺序**

1. 确认服务端已重启且 `buildID` 变了（日志里有 `前端静态资源构建指纹就绪`）；
2. `sw.js` 顶部 `SHELL_REV`（当前 `'35'`）——**仅当预缓存清单结构变化（增删文件）时才人工递增**。纯内容修改靠 `buildID` 自动破缓存即可；若未重编二进制就直接替换 `web/`（例如 nginx 直接托管静态目录），`sw.js` 里仍是占位符，此时必须递增 `SHELL_REV` 才能让老 SW 重新预缓存；
3. 核对 `sw.js` 的 `SHELL_ASSETS` 清单与 `index.html` 的 `<link>/<script>` **一一对应**：漏项 → 离线时该脚本缺失、模块静默不注册；多项 → `cache.addAll` 是原子的，任一 404 会让整个 install 失败、SW 完全不生效；
4. 浏览器 DevTools → Application → Service Workers 用 *Unregister*，Storage → *Clear site data* 强制清干净。

### 5.4 数据库迁移

- 位置：`internal/store/migrations/`，命名 `000X_简述.sql`，配套回滚 `000X_简述.down.sql`；
- 通过 `//go:embed migrations/*.sql` 内嵌；`version` = 文件名去掉 `.sql`；`*.down.sql` 不参与自动迁移；
- 执行时机：应用启动（`MEDDESK_DB_MIGRATE_ON_BOOT=true`）或 `go run ./cmd/meddesk -migrate`；
- 全部脚本在**一个事务**里按文件名升序执行，用 `pg_advisory_xact_lock` 串行化多实例竞争，任一步失败整体回滚；
- 因为同一事务内失败会整体回滚，脚本内请使用 `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` 保证幂等。

**新增迁移的标准动作**

1. `internal/store/migrations/0006_新功能.sql`（下一个序号）；
2. 同时写 `0006_新功能.down.sql`（哪怕只是 `DROP TABLE IF EXISTS ...`）；
3. 新增表必须配索引（外键/查询条件列），金额列用 `numeric(14,2)`；
4. 重新编译并 `-migrate` 验证；`psql` 查 `SELECT * FROM schema_migrations ORDER BY version;` 确认已记录；
5. 若同时新增了仓储方法，记得它是**可选模块挂载**所需的接口断言方法——生产 `*store.Store` 要实现，`internal/fake` 不实现则测试里该模块跳过挂载。

现有迁移：`0001_init`、`0002_family_grants`、`0003_envelope_recovery`、`0004_ledger`、`0005_ledger_backup`。

### 5.5 常用调试手法与工具脚本

| 手法 | 说明 |
| --- | --- |
| 健康探针 | `curl -s http://127.0.0.1:8080/api/v1/health`、`/api/v1/ready` 看版本与 DB/缓存状态 |
| 结构化日志 | `MEDDESK_LOG_LEVEL=debug MEDDESK_LOG_FORMAT=text` 输出人类可读的单行日志 |
| 前端语法自检 | `make check-js`（`node --check` 逐个校验 `web/assets/js/*.js` 与 `web/sw.js`） |
| Shell / YAML 自检 | `make check-shell`、`make check-yaml` |
| 文档与配置存在性 | `make check-docs` |
| 一键全量 | `make check-all` |
| 直连数据库 | `make psql`（从 `.env` 取 `MEDDESK_DB_URL`） |
| 清空业务数据 | `make db-reset`（需输入 `RESET`，仅开发环境） |
| 备份演练 | `make backup-dry`（只打印计划，不落文件） |

`tmp/` 下积累了大量开发期一次性脚本（如 `test-*.js` 系列的接口冒烟脚本、`patch-*.py` 系列的代码批量改写脚本、`e2e-*.js` 的端到端验证脚本）。它们**不是产品代码、也不在版本控制之内**，只作为「历史上存在这类临时验证手段」的参考；需要验证脚本时建议照此风格新建，而不要直接复用旧脚本（多数已与当前版本脱节）。

---

## 6. 编码规范

**适用环境**：🟢 开发 / 🟡 测试 / 🔴 生产　**适用版本**：v0.3.50

### 6.1 Go

| 项 | 规范 |
| --- | --- |
| 包划分 | 一个业务域一个包：`http.go`（路由挂载与 handler）+ `service.go`（业务逻辑）+ 仓储放 `internal/store`。跨包只依赖 `internal/`，禁止反向依赖 |
| 挂载约定 | 统一 `func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc)`；包内 `rg.Group("/xxx")` 并 `g.Use(requireAuth)`；可选模块在 `router.go` 用接口断言挂载 |
| 错误处理 | 一律走 `internal/apierr`：`apierr.New(code, msg)` / `apierr.Wrap(code, err, msg)` → `apierr.HTTPStatus(code)`。响应体统一 `{"code","message","details"}`；**不要把内部 error 原文直接返回给前端** |
| 仓储层 | 只允许 `internal/store` 写 SQL；**必须参数化查询**（`$1, $2`），禁止字符串拼接 SQL。仓储返回的未找到统一 `store.ErrNotFound` |
| 日志 | 统一 `log/slog` 结构化输出（`slog.Info("msg", "key", val)`）；**禁止记录**口令、令牌、`wrapped_dek`、WebDAV 凭据、验证码、`Authorization` 头 |
| 测试命名 | `Test<被测对象>_<场景>`，如 `TestRegister_DuplicateEmail`；表驱动用 `cases := []struct{ name string; ... }`；集成测试放 `<pkg>_test.go`，跨包冒烟放 `internal/server/*_integration_test.go` |
| 格式 | 提交前 `make fmt && make vet`；`gofmt` 是硬要求 |

### 6.2 前端（原生 JS）

| 项 | 规范 |
| --- | --- |
| 模块命名空间 | 所有模块挂到全局单一对象 `MD` 上，一个文件一个命名空间：`MD.ui` / `MD.icon` / `MD.toast` / `MD.router` / `MD.api` / `MD.crypto` / `MD.db` / `MD.data` / `MD.sync` / `MD.med` / `MD.ledger` / `MD.family` / `MD.webdav` / `MD.tools` / `MD.util` / `MD.theme` 等。新增文件先注册命名空间再使用，避免全局散落变量 |
| 事件绑定 | 用事件委托：`容器.addEventListener('click', ...)` + `data-act="xxx"` 属性分发（现有 `med.js`、`ledger.js` 已是这个风格）。**禁止内联 `onclick=`**（CSP `script-src 'self'` 会拦，且无法统一收口） |
| XSS 防护 | 渲染用户输入优先 `textContent` / `createElement` + `appendChild`；必须拼 HTML 时先转义。**禁止裸 `innerHTML` 拼接用户输入** |
| CSP | 页面脚本必须是同源外部文件（`<script src="/assets/js/*.js">`），不能内联；后台页同理使用 `/admin/app.js` |
| 新增脚本 | ① 在 `web/index.html` 加 `<script src>`；② **同步加到 `web/sw.js` 的 `SHELL_ASSETS`**（见 5.3）；③ 需要时递增 `SHELL_REV` |
| 语法自检 | `make check-js` |

### 6.3 数据库

| 项 | 规范 |
| --- | --- |
| 金额 | 一律 `numeric(14,2)` 定点数，**禁止浮点**（`float`/`double`/`real`）。对外给整数分时 `(SUM(amount)*100)::bigint` |
| 新增表 | 必须带主键、必要索引（`CREATE INDEX IF NOT EXISTS`）、`COMMENT`，并在迁移文件里写清字段含义 |
| 迁移 | 新表 = 新迁移文件 + `.down.sql`，禁止直接改历史迁移 |
| 查询 | 聚合在 SQL 内完成（精确），前端只做展示与图表换算 |

### 6.4 Git 与提交

| 项 | 规范 |
| --- | --- |
| 提交信息 | `<type>(<scope>): <简述>`，类型用 `feat` / `fix` / `refactor` / `docs` / `test` / `chore` / `build`；例如 `fix(ledger): 修正月度汇总跨月边界`。简述用中文，说明「改了什么 + 为什么」 |
| 绝密文件 | **`.env`、`.meddesk.env`、任何 `*.env` 一律不提交**，仓库只保留 `meddesk.env.example`（占位符版本）。提交前自查 `git status` |
| 密钥 | 代码中不得出现真实密钥/口令/令牌；示例一律 `<...>` 占位符 |
| 分支与评审 | 变更涉及加密契约（`CONTRACTS.md`）、迁移文件、`sw.js` 缓存清单时必须单独说明并请人复核 |

### 6.5 注释语言

- 与现有代码保持一致：**中文注释**，且重点写「为什么这么做」，而不是复述代码做了什么。
- 好的注释示例（摘录自现有代码风格）：解释安全取舍（`router.go` 关于只信任 `127.0.0.1` 的说明）、解释历史坑位（`sw.js` 关于 `SHELL_REV` 的说明）、解释降级原因（`config.go` 关于 Redis 默认关闭的说明）。
- 导出的标识符必须有包级或函数级注释；复杂的非显然逻辑（加密、限流、冲突裁决）必须写清前提与失效条件。

---

## 附录 A：已知实现侧风险

> 以下为阅读源码时发现的不一致/隐患，供后续排期处理。
>
> **修复状态（v0.3.51，2026-10-30 已上生产）**
> - 第 **1** 项（版本注入静默失效）→ **已修复**：`Makefile` 改用 `internal/server` 全限定包路径，并在该包补齐 `Commit`/`BuildTime`；`meddesk -version` 现可正确输出。
> - 第 **8** 项（`MEDDESK_ADMIN_EMAILS` 内置个人邮箱）→ **已修复**：默认值改为空名单，启动时若为空打 WARN；生产必须在 `.meddesk.env` 显式配置。
> - 同批修复（本表未列）：分类 `PATCH` 局部更新会把 `sortOrder`/`archived` 静默清零；待报销 `pendingTotal` 只累加首页 200 条；仓储冲突错误被误映射为 `500`（类型断言改 `errors.As` 后正确返回 `409`）。
> - 第 **2–7、9** 项**仍未处理**，按下表继续跟踪。

| # | 风险 | 位置 | 影响 |
| --- | --- | --- | --- |
| 1 ✅ | Makefile 的 `LDFLAGS` 注入的是 `-X main.Version`，但版本变量实际定义在 `internal/server.Version`（`main` 包无此变量）；`make build-linux` 甚至完全不带 `-X` | `Makefile:25`、`internal/server/router.go:38` | `make build*` 产出的二进制版本号注入**无效**，`/api/v1/health` 与页面标题会停留在默认值 `0.1.0`；应按 `internal/server.Version` 全路径注入 |
| 2 | 配置文件命名不统一：代码 `godotenv.Load()` 无参只认仓库根 `.env`；Makefile `ENV_FILE ?= .env`；systemd 与运维文档用 `.meddesk.env` | `internal/config/config.go:116`、`Makefile:37`、`deploy/systemd/meddesk.service` | 只放 `.meddesk.env` 时本机开发**不生效**，容易被误判为「配置没写对」 |
| 3 | `make deps-up` 的提示文案写「PG16」，compose 实际镜像是 `postgres:18-alpine` | `Makefile:51` vs `deploy/docker-compose.dev.yml` | 误导新人，实际版本以 compose 为准 |
| 4 | 迁移 `0002_family_grants.sql`、`0003_envelope_recovery.sql` 缺 `.down.sql` | `internal/store/migrations/` | 这两步**无法自动回滚**；只能靠 `db-drop` 或备份恢复 |
| 5 | `sw.js` 的 `SHELL_ASSETS` 需与 `index.html` 手工同步，且 `SHELL_REV` 需人工递增 | `web/sw.js:18`、`:27` | 漏同步会导致离线缺脚本或 SW 整体 install 失败，属高危手工点；建议加 CI 校验 |
| 6 | 账本每日备份调度器是进程内 `go` 协程 | `internal/server/router.go:179` | 多实例部署时每个实例都会触发一次备份（源码注释已标注该限制） |
| 7 | 家庭短码 pepper 由 `MEDDESK_JWT_SECRET` 派生 | `internal/server/router.go:193` | 轮换 JWT 密钥会让**尚未兑换**的短码失效（已建立的授权不受影响），轮换前需公告 |
| 8 ✅ | `MEDDESK_ADMIN_EMAILS` 在 `config` 里有非空默认值 | `internal/config/config.go:150` | 若生产环境未显式覆盖，会启用一个内置邮箱作为后台白名单，属默认凭据风险；上线前必须显式设置 |
| 9 | `tmp/` 与 `dist/` 内容靠 `.gitignore` 排除，但目录本身承载着历史调试脚本与旧版本二进制 | `tmp/`、`dist/` | 新人容易误以为它们是产品代码；建议定期清理，避免磁盘与认知负担 |

## 附录 B：新人半天上手路线

1. 读 `README.md` + 本文第 1、2 章（约 40 分钟）
2. `make deps-up` → `cp meddesk.env.example .env` → 替换占位符 → `make dev`（约 20 分钟）
3. 打开 `http://127.0.0.1:8080/` 注册账号，`curl /api/v1/health` 确认版本（约 10 分钟）
4. `go test ./... -count=1` 跑通全量测试（约 10 分钟）
5. 改 `web/assets/js/core.js` 一行 → 刷新页面看效果 → 再 `make build` 验证内嵌路径（约 30 分钟）
6. 读 `CONTRACTS.md` 了解 E2E 与同步契约，再动手改业务代码（约 60 分钟）
