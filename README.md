# MedDesk · 个人数字工作台

> **一句话**：一个可以自己托管的、端到端加密的私人工作台。一期做「**用药提醒**」，后续会扩展健康记录、日程待办、家庭账本等模块。
>
> **核心承诺**：服务端**永远看不到**你的明文数据。

---

## 目录

- [1. 项目定位](#1-项目定位)
- [2. 一期范围与功能清单](#2-一期范围与功能清单)
- [3. 架构总览](#3-架构总览)
- [4. 技术选型与取舍](#4-技术选型与取舍)
- [5. 目录结构](#5-目录结构)
- [6. 快速开始（本地开发）](#6-快速开始本地开发)
- [7. 构建与交叉编译](#7-构建与交叉编译)
- [8. 部署](#8-部署)
- [9. 配置参考](#9-配置参考)
- [10. 模块路线图](#10-模块路线图)
- [11. 文档索引](#11-文档索引)
- [12. 安全提醒（务必先读）](#12-安全提醒务必先读)
- [13. 许可证与致谢](#13-许可证与致谢)

---

## 1. 项目定位

MedDesk 是一个**自托管（self-hosted）**的个人 / 家庭数字工作台，设计目标是：

| 目标 | 说明 |
|---|---|
| **隐私优先** | 端到端加密（E2EE）。服务端只存密文信封（`iv` + `ct`）与 KDF 参数，**没有**解密能力。 |
| **本地优先** | 全部业务数据落在浏览器 IndexedDB，离线可用；服务端只是「加密中转站 + 多设备同步器」。 |
| **自己掌控** | 一台 2 核 4 GB 的小鸡 / NAS 就能跑，数据在自己手里，不依赖任何第三方 SaaS。 |
| **单二进制** | Go 静态编译出一个可执行文件，前端一同内嵌（`//go:embed all:web`），零运行时依赖。 |
| **零外链** | 前端不引任何 CDN、外部字体、第三方统计；图标是内联 SVG，图表是手写 SVG。 |

### 一期做什么、不做什么

| ✅ 一期做 | ❌ 一期不做 |
|---|---|
| 家庭成员档案与数据隔离 | 多用户协作 / 共享 |
| 药物档案（剂量、频次、照片、副作用…） | 药品知识库 / 相互作用查询 |
| 6 类排程 + 提醒 + 一键打卡 | 服务端推送（Web Push） |
| 用药历史 / 日历视图 / 依从性统计 | 医疗诊断建议 |
| PDF / CSV 导出 | SaaS 托管版本 |
| 加密双向同步 | 明文云备份 |

---

## 2. 一期范围与功能清单

下面按需求逐条对齐，均已在一期范围内实现（打勾表示「已纳入一期交付范围」）。

### 2.1 排程与提醒

- [x] **每日一次**（`daily`）——每天固定时刻，如 `08:00`
- [x] **每日两次**（`twice_daily`）——如 `08:00`、`20:00`（`times` 数组支持更多次）
- [x] **按小时间隔**（`interval_hours`）——如「每 8 小时一次」
- [x] **指定日期**（`specific_dates`）——只在给定的日期列表服药
- [x] **自定义间隔**（`custom_interval`）——每 N 天（或 N 周 / N 月）一次
- [x] **周期性用药**（`cycle`）——21 天服药 + 7 天停药的经典口服避孕药 / 激素疗法循环
- [x] **关键提醒穿透勿扰**——药物标记为 `critical` 时，该次提醒归属于「关键提醒」类别，优先展示与提示（具体能否穿透设备级勿扰取决于浏览器/系统设置）
- [x] 逾期判定宽限 15 分钟；昨日未处理的剂量自动顺延到今天

### 2.2 打卡与记录

- [x] 一键 **已服用** / **跳过** / **推迟**（推迟可选分钟数）
- [x] 用药历史流水（按时间倒序，可按成员筛选）
- [x] 依从性统计：总次数 / 已服 / 跳过 / 漏服 / 依从率 / 连续达标天数
- [x] 日历进度视图（热力/环形图）

### 2.3 家庭档案

- [x] 多成员档案（姓名、关系、生日、主题色、备注、归档）
- [x] 数据隔离：药物归属某个 `profileId`，视图按成员过滤

### 2.4 药物信息

- [x] 剂量（`dosageAmount` + `dosageUnit`）、剂型（`form`）、规格（`strength`）
- [x] 饮食说明（`foodInstruction`）：空腹 / 餐前 / 餐后 / 随餐
- [x] 处方医生（`prescriber`）、药房（`pharmacy`）
- [x] 副作用记录（`sideEffects`）、备注（`notes`）
- [x] 照片（`photoKeys`，客户端压缩到最长边 ≤1280px / JPEG q=0.82 后加密上传）
- [x] 库存与提醒阈值（`stock` / `refillThreshold`）
- [x] 生效与失效日期（`startDate` / `endDate`）、停用开关（`active`）

### 2.5 导出

- [x] **PDF**：A4 纵向，标题 + 汇总区 + 明细表格 + 页脚（生成时间 / 页码），需中文字体
- [x] **CSV**：UTF-8 BOM + CRLF，Excel 双击即可打开
- [x] **JSON 备份**：完整明文快照，可导入恢复

### 2.6 体验

- [x] 移动端优先：`<768px` 单列，汉堡抽屉 + 底部 Tab
- [x] PC：左侧固定侧栏（可折叠）
- [x] 深浅色主题：跟随系统 + 手动切换，写入 localStorage
- [x] 过渡动画 120–240ms `cubic-bezier(.2,.8,.2,1)`，尊重 `prefers-reduced-motion`
- [x] 触觉反馈：`navigator.vibrate` 兜底，失败静默
- [x] 按钮最小 44×44px，输入字号 ≥16px（防 iOS 自动缩放）
- [x] 首屏「今天要处理」+ 逾期标红 + 一键操作
- [x] 首次打开预置 3–5 条示例数据（含 1 条逾期），并提供「清空示例数据」

### 2.7 安全与同步

- [x] 端到端加密（PBKDF2 → HKDF → KEK → 包裹 DEK → 记录 AES-GCM-256）
- [x] 本地优先（IndexedDB）+ 增量双向同步（push / pull 游标）
- [x] 服务端二次哈希 argon2id；刷新令牌轮换 + 重放检测
- [x] 关闭公开注册开关（`MEDDESK_SIGNUP_ENABLED=false`）

---

## 3. 架构总览

### 3.1 分层架构图（ASCII）

```
┌──────────────────────────────────────────────────────────────────────────┐
│                              浏览器 / PWA                                 │
│  ┌────────────┐ ┌──────────┐ ┌────────────┐ ┌─────────┐ ┌────────────┐  │
│  │ index.html │ │ core.js  │ │ crypto.js  │ │  db.js  │ │  api.js    │  │
│  │  壳/DOM    │ │ 工具/路由 │ │ PBKDF2/HKDF│ │IndexedDB│ │ HTTP 客户端 │  │
│  └─────┬──────┘ └────┬─────┘ └─────┬──────┘ └────┬────┘ └─────┬──────┘  │
│        │             │             │             │             │          │
│  ┌─────┴─────────────┴─────────────┴─────────────┴─────────────┴──────┐  │
│  │                       data.js（本地仓储 + 脏标记）                    │  │
│  └──────────────────────────┬──────────────────────────────────────────┘  │
│                             │                                             │
│  ┌──────────────────────────┴──────────────────────────────────────────┐  │
│  │  schedule.js（排程引擎）  remind.js（提醒）  med.js（视图）  export.js│  │
│  └──────────────────────────┬──────────────────────────────────────────┘  │
│                             │             ▲                                │
│                             ▼             │  sync.js（增量同步，脏记录 Push）│
│                    【 明文只到这里为止；之后一切对外传输皆为密文 】          │
└─────────────────────────────┼─────────────┼──────────────────────────────┘
                              │ HTTPS       │ JSON：{iv, ct} 密文信封
                              ▼             │
┌──────────────────────────────────────────────────────────────────────────┐
│                    Nginx（TLS 终止 · 限流 · 安全头 · 静态）                │
│                      监听 443/80，反代到 127.0.0.1:8080                    │
└─────────────────────────────┬────────────────────────────────────────────┘
                              │ proxy_pass http://meddesk_app
                              ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                    meddesk（Go 单二进制，监听 :8080）                      │
│  ┌───────────────────────────────────────────────────────────────────┐   │
│  │ 中间件链：RequestID → Recovery → Logger → SecurityHeaders →        │   │
│  │            CORS → BodyLimit →（/api/v1 组）RateLimit               │   │
│  └───────────────────────────────────────────────────────────────────┘   │
│  ┌────────┐ ┌────────┐ ┌──────────┐ ┌──────────┐ ┌──────────────────┐   │
│  │  auth  │ │ syncsvc│ │attachments│ │  export  │ │ mountSPA（内嵌   │   │
│  │ 认证授权│ │ 加密同步│ │ 密文附件 │ │ PDF/CSV  │ │ //go:embed web） │   │
│  └───┬────┘ └───┬────┘ └────┬─────┘ └────┬─────┘ └──────────────────┘   │
└──────┼──────────┼───────────┼────────────┼──────────────────────────────┘
       │          │           │            │
       ▼          ▼           ▼            │    （纯内存渲染，不落盘）
┌──────────────┐  ┌──────────────────┐     │
│ PostgreSQL 18│  │  PostgreSQL 18   │     │
│ users        │  │  sync_records    │     │
│ refresh_tokens│ │  sync_state      │     │
└──────────────┘  │  attachments     │     │
┌──────────────┐  └──────────────────┘     │
│   Redis 7    │  密文落盘目录              │
│ 限流/吊销名单 │  /home/ops/service/meddesk/data │
│ 登录失败计数  │  {userID}/{yy}/{id}.bin  │
└──────────────┘                            │
                                            ▼
                                  立即流式返回给浏览器
```

### 3.2 数据流简述

1. **写入**：用户在前端点「已服用」→ `MD.data.intakes.save()` → IndexedDB 落盘 + 置脏 → `sync.js` 触发
   → 客户端用 DEK 加密成一堆 `{table, id, ver, updatedAt, deleted, iv, ct}` → `POST /api/v1/sync/push`。
2. **读取（其他设备）**：`GET /api/v1/sync/pull?cursor=N` → 服务端按 `seq > N` 返回密文 → 客户端解密写本地 IndexedDB。
3. **冲突**：服务端按 `ver → updatedAt → 删除优先` 裁决，输的一方进 `conflicts[]` 回传，客户端解密后自行合并并 toast 提示。
4. **导出**：客户端把**已解密**的行数据 POST 到 `/api/v1/export/pdf|csv`，服务端**只在内存里渲染**后流式返回，不落盘不写日志。

---

## 4. 技术选型与取舍

### 4.1 主存：PostgreSQL 18 🥇（对比 MySQL 8）

| 维度 | PostgreSQL 18（16/17 同） | MySQL 8 | 对 MedDesk 的影响 |
|---|---|---|---|
| `citext` 大小写不敏感类型 | 原生扩展 | 需靠 `collation` 绕 | 邮箱唯一性更干净 |
| `timestamptz` | 原生，时区语义明确 | `DATETIME` 不带时区 | 客户端多时区不会踩坑 |
| `bytea` 密文存储 | 一等公民 | `BLOB`/`VARBINARY` 也行 | 本项目大量存 iv/ct/wrapped_dek |
| 单事务内 `RETURNING` + CTE | 成熟，一条 SQL 完成「分配 seq + 批量 upsert」 | 支持但写法更啰嗦 | push 的批量写入是核心路径 |
| JSONB | 原生 + GIN 索引 | JSON 类型功能弱 | 二期模块（健康记录等半结构化数据）可直接复用 |
| 扩展生态 | 极其丰富（pgvector 等） |较弱 | 二期若要加向量检索无需换库 |
| 许可证与中立性 | BSD-like | GPL/商业双授权 | 自托管项目更省心 |
| 运维复杂度 | 略高（vacuum 等） | 略低 | 一期数据量极小（万行量级），可控 |

> **结论**：MedDesk 的场景是「小数据量 + 强事务 + 大量二进制 + 未来要加半结构化字段」，PG 的优势几乎全部命中；运维复杂度在万行数据量下可以忽略，因此选 **PostgreSQL 18**（当前最新稳定大版本；16/17 亦可，SQL 无版本特有特性）。

### 4.2 Redis 7：只做四类辅助工作，**不是数据源**

| 用途 | 键示例 | 失效后的影响 |
|---|---|---|
| ① 访问速率限制令牌桶 | `meddesk:rl:<ip>` | 退化为进程内计数（单实例仍有效） |
| ② 刷新令牌吊销名单 | `meddesk:auth:revoked:<tokenID>` | 该令牌吊销状态可能延迟生效（有库表兜底） |
| ③ 登录失败计数（防爆破） | `meddesk:auth:fail:<sha256(email)>` | 防爆破阈值失效（库表无此计数） |
| ④ 并发刷新宽限标记 | `meddesk:auth:grace:<oldTokenID>` | 多标签页并发刷新可能误判重放，需重新登录 |

> **设计原则**：Redis 里**没有任何不可替代的数据**。设 `MEDDESK_REDIS_ENABLED=false` 会走进程内 Noop 实现（内存 map + TTL + 惰性清理），单实例部署可安全关掉 Redis。
> Redis 里的内容全部 `maxmemory 256mb` + `allkeys-lru` + 关闭 RDB/AOF，随意淘汰。

### 4.3 反向代理：Nginx（对比 Caddy）

| 维度 | Nginx | Caddy | 选择理由 |
|---|---|---|---|
| 自动 HTTPS | 需外部工具（acme.sh） | 原生自动 | Caddy 胜，但… |
| 配置粒度 | 极细（每 location 独立限流、超时、缓冲） | 相对抽象 | 我们需要对「鉴权 / 导出 / 普通 API」分别限流 |
| `limit_req_zone` 令牌桶 | 原生成熟 | 需插件/较新版本 | Nginx 胜 |
| 生态与文档 | 二十年积累，运维人人会 | 较新 | 出问题更容易查到答案 |
| 反向代理性能 | 极高 | 也很高 | 打平 |
| 配置可复用性 | `deploy/nginx/meddesk.conf` 可直接套用 | 需另写 Caddyfile | Nginx 胜 |

> **结论**：选 **Nginx**，TLS 用 **acme.sh** 补齐自动签发与续期（见 [DEPLOYMENT.md](docs/DEPLOYMENT.md) 第 6 步）。
> 若你偏好 Caddy，本项目只需把 TLS 终止层换掉即可——应用本身完全不感知。

### 4.4 后端依赖（`go.mod` 已冻结）

| 依赖 | 用途 | 备注 |
|---|---|---|
| `gin v1.12` | HTTP 路由与中间件 | 生态成熟 |
| `pgx/v5` | PostgreSQL 驱动 + 连接池 | 原生 PG 协议，性能好 |
| `go-redis/v9` | Redis 客户端 | 支持 `rediss://` |
| `golang-jwt/jwt/v5` | 访问令牌签发 | HS256 |
| `google/uuid` | 记录主键生成 | UUIDv4 |
| `golang.org/x/crypto` | **argon2id** 服务端二次哈希 | 口令存储核心 |
| `joho/godotenv` | 加载根目录 `.env` | 仅本地开发 |
| `go-pdf/fpdf` | PDF 渲染 | 需要 CJK TTF 字体 |

### 4.5 前端：为什么不用框架？

| 诉求 | 结论 |
|---|---|
| 零构建步骤 | 经典 `<script>` 加载，改完 JS 刷新页面即生效，没有 webpack/vite |
| 零外部依赖 | 不引 CDN（隐私 + 离线可用） |
| 体积极小 | 全部 JS/CSS 合计不到 100 KB，内嵌进二进制 |
| 可控 | 原生 ES2019+ 写个小路由/事件总线即可 |
| PWA | 手写 `sw.js` + `manifest.webmanifest` 即可实现离线壳 |

---

## 5. 目录结构

```
meddesk/
├── assets.go                      # //go:embed all:web（把前端编进二进制）
├── go.mod / go.sum                # 依赖清单（已冻结）
├── Makefile                       # 构建/测试/部署/备份统一入口
├── README.md                      # 本文件
├── CONTRACTS.md                   # 契约文档（唯一事实来源）
├── .env.example                   # 本地开发环境变量样例
│
├── cmd/
│   └── meddesk/                   # main()：加载配置 → 开日志 → 连库/缓存 → 迁移 → ListenAndServe
│
├── internal/
│   ├── config/                    # 环境变量加载与校验（权威定义）
│   ├── apierr/                    # 统一错误码与 HTTP 状态码映射
│   ├── model/                     # DTO 与表名常量（KnownTables）
│   ├── server/                    # 路由组装与 SPA 静态挂载
│   ├── middleware/                # RequestID/Logger/Recovery/安全头/CORS/BodyLimit/RateLimit
│   ├── cache/                     # Cache 接口 + Redis 实现 + 进程内 Noop 降级
│   ├── auth/                      # 注册/登录/刷新轮换/改密/JWT/RequireAuth
│   ├── store/                     # pgx 仓储层
│   │   └── migrations/            # 0001_init.sql（幂等）
│   ├── syncsvc/                   # 加密 push/pull/stats/reset
│   ├── attachments/               # 密文附件上传/下载/删除
│   └── export/                    # PDF/CSV（纯内存渲染）
│
├── web/                           # 前端（零构建、零外链）
│   ├── index.html
│   ├── manifest.webmanifest
│   ├── sw.js
│   └── assets/
│       ├── css/app.css            # CSS 变量 + 工具类 + 深浅色主题
│       └── js/
│           ├── core.js            # MD.bus / MD.util / MD.toast / MD.haptic / MD.theme / MD.ui / MD.router
│           ├── crypto.js          # WebCrypto：PBKDF2 → HKDF → AES-GCM
│           ├── db.js              # IndexedDB 封装
│           ├── api.js             # fetch 封装（自动续 token）
│           ├── data.js            # 领域仓储 + 脏标记 + JSON 备份/恢复
│           ├── sync.js            # 增量同步编排
│           ├── schedule.js        # 排程引擎（纯函数）
│           ├── remind.js          # 提醒调度
│           ├── med.js             # 用药模块视图
│           ├── export.js          # 导出交互
│           └── app.js             # 启动引导
│
├── deploy/
│   ├── Dockerfile                 # 多阶段构建（含 CJK 字体）
│   ├── docker-compose.yml         # 一键起 PG + Redis + App
│   ├── env.example                # 生产环境变量样例
│   ├── nginx/meddesk.conf         # Nginx 站点配置
│   ├── systemd/meddesk.service    # systemd 单元（含安全加固）
│   ├── postgres-tuning.conf       # PG 调优片段（与 mem_limit 500m 配套）
│   ├── fonts/.gitkeep             # 放离线中文字体用（勿删）
│   └── scripts/
│       ├── install.sh             # 一键初始化（幂等，支持 --dry-run）
│       ├── backup.sh              # pg_dump -Fc + 附件打包 + 异地 + 保留策略
│       ├── restore.sh             # 恢复（默认二次确认）
│       └── get-font.sh            # 中文字体获取
│
└── docs/
    ├── API.md                     # HTTP 接口详述
    ├── DEPLOYMENT.md              # 从零到上线 11 步
    ├── DATA-MODEL.md              # 数据模型与同步协议
    ├── SECURITY.md                # 威胁模型与安全基线
    └── EXTENDING.md               # 如何新增模块
```

---

## 6. 快速开始（本地开发）

### 6.1 前置依赖

| 依赖 | 版本 | 说明 |
|---|---|---|
| Go | ≥ 1.26.4 | `go version` 确认 |
| Node.js | 可选 | 仅用于 `make check-js`（`node --check`） |
| PostgreSQL | 18（16+ 均可） | apt 装 postgresql-18，或 `make deps-up` 用 Docker |
| Redis | 7（可选） | 不想装就把 `MEDDESK_REDIS_ENABLED=false` |

### 6.2 起一个本地数据库

```bash
# 用 docker compose 起依赖（PG 16 + Redis 7，仅本机监听；应用本身在宿主机 go run）
docker compose -f deploy/docker-compose.dev.yml up -d

# 查看状态 / 停止 / 停止并清空数据
docker compose -f deploy/docker-compose.dev.yml ps
docker compose -f deploy/docker-compose.dev.yml down
docker compose -f deploy/docker-compose.dev.yml down -v
```

> 本地开发用上面这份（口令硬编码仅限本机）。**生产环境**的 PG/Redis 容器化
> （口令走环境变量、citext 自动初始化）见 `deploy/docker-compose.middleware.yml`
> 与 [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) 第 2–3 节的环境变量对照表。

### 6.3 准备环境变量

```bash
cp meddesk.env.example .env

# JWT 密钥是必填项，必须 ≥32 字节
openssl rand -base64 48          # 把输出填进 MEDDESK_JWT_SECRET
```

最小可用 `.env`：

```bash
MEDDESK_ENV=dev
MEDDESK_HTTP_ADDR=:8080
MEDDESK_JWT_SECRET=<openssl rand -base64 48 的输出>
MEDDESK_DB_URL=postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable
MEDDESK_REDIS_ENABLED=false
MEDDESK_SIGNUP_ENABLED=true
MEDDESK_LOG_FORMAT=text
MEDDESK_LOG_LEVEL=debug
```

### 6.4 跑起来

```bash
# 方式一：直接指定环境变量启动（不写 .env 也行）
MEDDESK_JWT_SECRET="$(openssl rand -base64 48)" \
MEDDESK_WEB_DIR=./web \
MEDDESK_SIGNUP_ENABLED=true \
  go run ./cmd/meddesk

# 方式二：用 Makefile（等价于上面那行）
make dev
```

> **`MEDDESK_WEB_DIR=./web` 是本地开发的关键**：设了它之后，Go 会从磁盘读 `web/` 下的静态资源，
> 改 JS/CSS **不用重新编译** Go 二进制，刷新浏览器即可。
> 生产环境请**留空**，使用二进制内嵌的前端（`//go:embed all:web`）。

启动后打开 <http://localhost:8080> ，验证：

```bash
curl -fsS http://127.0.0.1:8080/api/v1/health | head -c 300; echo
curl -fsS http://127.0.0.1:8080/api/v1/ready  | head -c 300; echo
```

> **首次使用**：如果 `MEDDESK_SIGNUP_ENABLED=true`，直接在页面上注册即可；
> 若为 `false`，见 [DEPLOYMENT.md](docs/DEPLOYMENT.md) 第 7 步「创建第一个账号」。

### 6.5 常用 Make 目标

```bash
make help           # 查看所有目标
make dev            # 本地开发启动（从磁盘读前端）
make build          # 构建 Linux amd64 到 ./bin/meddesk
make build-all      # 交叉编译全部目标
make test           # go test ./...
make vet            # go vet ./...
make check-js       # node --check 逐个校验前端脚本
make check-shell    # bash -n 校验 deploy 脚本
make check-all      # 全部校验（含文档存在性）
```

---

## 7. 构建与交叉编译

### 7.1 生产构建（推荐）

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-s -w" -o meddesk ./cmd/meddesk
```

- `CGO_ENABLED=0`：产出**纯静态**二进制，不依赖 glibc，扔到任何 Linux 都能跑
- `-ldflags="-s -w"`：去掉符号表与 DWARF，体积减少约 30%
- 产物通常 < 20 MB

### 7.2 带版本号构建

```bash
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT}" \
  -o meddesk ./cmd/meddesk
```

> 版本号会出现在 `/api/v1/health` 的 `version` 字段里。

### 7.3 其他平台

```bash
# Linux ARM64（NAS / 树莓派 / 甲骨文 ARM）
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags="-s -w" -o meddesk-linux-arm64   ./cmd/meddesk

# macOS Apple Silicon（本地调试）
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o meddesk-darwin-arm64  ./cmd/meddesk

# Windows
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o meddesk.exe           ./cmd/meddesk
```

或直接 `make build-all`（产出 `bin/meddesk-<GOOS>-<GOARCH>`）。

> ⚠ **注意**：`//go:embed all:web` 依赖 `web/.gitkeep` 存在，**不要删除它**，否则 `web/` 目录为空时构建会失败。

---

## 8. 部署

MedDesk 提供两条等价的部署路径，**二选一**：

| 路径 | 适用人群 | 入口 |
|---|---|---|
| **A · Docker Compose** | 想快速跑起来、NAS/小主机、不在意容器化运维 | `cp meddesk.env.example .env && docker compose -f deploy/docker-compose.yml up -d --build` |
| **B · systemd + 宿主机 PG** | 追求最小攻击面、已有 PG 实例、想用 journald 统一运维 | `sudo ./deploy/scripts/install.sh --method systemd --domain med.example.com --with-postgres --with-redis` |

完整的 11 步上线流程（含 PostgreSQL 安装、Redis 加固、acme.sh 申请 ECC 证书、备份 crontab、
故障排查表）见 👉 **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)**。

最小冒烟：

```bash
curl -fsS https://med.example.com/api/v1/health
# {"status":"ok","version":"0.1.0","db":"ok","cache":"ok","serverTime":...}
```

---

## 9. 配置参考

所有配置项均为环境变量，前缀 **`MEDDESK_`**。优先级：**真实环境变量 > `.env` 文件 > 代码默认值**。

必填的只有一个：`MEDDESK_JWT_SECRET`（≥32 字节，用 `openssl rand -base64 48` 生成）。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `MEDDESK_ENV` | `prod` | `dev`/`development`/`prod` |
| `MEDDESK_HTTP_ADDR` | `:8080` | 监听地址，Nginx 反代时保持默认 |
| `MEDDESK_BASE_URL` | `http://localhost:8080` | 站点外链基址 |
| `MEDDESK_TRUST_PROXY` | `false` | 前面有 Nginx 时必须 `true` |
| `MEDDESK_WEB_DIR` | 空 | 非空则从磁盘读静态资源（开发用） |
| `MEDDESK_DB_URL` | `postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable` | PG 连接串 |
| `MEDDESK_DB_MAX_CONNS` | `10` | 连接池上限 |
| `MEDDESK_DB_MIN_CONNS` | `1` | 连接池下限 |
| `MEDDESK_DB_MAX_CONN_LIFETIME` | `30m` | 连接最大存活 |
| `MEDDESK_DB_MIGRATE_ON_BOOT` | `true` | 启动时自动迁移（幂等） |
| `MEDDESK_REDIS_ENABLED` | `false` | 默认走进程内降级（单实例）；多副本部署建议 `true` |
| `MEDDESK_REDIS_URL` | `redis://127.0.0.1:6379/0` | Redis 连接串（`rediss://` 走 TLS） |
| `MEDDESK_JWT_SECRET` | **必填** | ≥32 字节 |
| `MEDDESK_ACCESS_TTL` | `15m` | 访问令牌有效期 |
| `MEDDESK_REFRESH_TTL` | `720h` | 刷新令牌有效期（30 天） |
| `MEDDESK_ARGON2_MEMORY` | `65536` | KiB（64 MiB） |
| `MEDDESK_ARGON2_ITERATIONS` | `3` | t |
| `MEDDESK_ARGON2_PARALLELISM` | `2` | p |
| `MEDDESK_SIGNUP_ENABLED` | `true` | 公网务必设 `false` |
| `MEDDESK_REFRESH_REUSE_GRACE` | `30s` | 并发刷新宽限 |
| `MEDDESK_SYNC_MAX_ITEMS` | `500` | 单次 push 上限 |
| `MEDDESK_SYNC_MAX_RECORD_BYTES` | `1048576` | 单条密文上限（1 MiB） |
| `MEDDESK_SYNC_PULL_PAGE` | `300` | pull 默认分页 |
| `MEDDESK_SYNC_PULL_PAGE_MAX` | `1000` | pull 最大分页 |
| `MEDDESK_UPLOAD_DIR` | `/home/ops/service/meddesk/data` | 密文附件目录 |
| `MEDDESK_UPLOAD_MAX_BYTES` | `8388608` | 单附件上限（8 MiB） |
| `MEDDESK_PDF_FONT_PATH` | 空 | CJK TTF 路径，留空则自动探测 |
| `MEDDESK_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `MEDDESK_LOG_FORMAT` | `json` | `json`/`text` |
| `MEDDESK_RATE_LIMIT_RPS` | `20` | 每 IP 令牌桶速率 |
| `MEDDESK_RATE_LIMIT_BURST` | `60` | 突发上限 |
| `MEDDESK_CORS_ALLOWED_ORIGINS` | 空 | 逗号分隔，空 = 同源 |

> 权威定义见 [`internal/config/config.go`](internal/config/config.go)；带完整注释的样例见 [`meddesk.env.example`](meddesk.env.example)。

---

## 10. 模块路线图

### 一期（当前）

```
┌─────────────────────────────────────────────────┐
│  用药提醒 medication                             │
│  档案 / 药物 / 排程 / 打卡 / 历史 / 统计 / 导出   │
└─────────────────────────────────────────────────┘
```

### 二期（规划中）

| 模块 | 计划内容 | 复用一期的哪些能力 |
|---|---|---|
| **健康记录** *(health)* | 血压、血糖、体重、体温趋势图；异常值标注 | 成员档案、同步协议、导出管道、主题/图表工具 |
| **日程待办** *(todo)* | 家庭日程、重复任务、到期提醒 | 排程引擎（`schedule.js` 的 occurrence 逻辑可泛化）、提醒通道 |
| **家庭账本** *(ledger)* | 收入支出、分类统计、家庭共享视图 | 成员档案、同步协议、CSV/PDF 导出 |
| **Web Push**（服务端推送） | VAPID + Push API，实现真正的「后台到达」 | 见 [EXTENDING.md](docs/EXTENDING.md) 预留点 |

侧栏里未上线的模块会显示为「即将上线」，点击给出提示。

---

## 11. 文档索引

| 文档 | 内容 |
|---|---|
| [CONTRACTS.md](CONTRACTS.md) | **契约**：所有接口的唯一事实来源 |
| [docs/API.md](docs/API.md) | HTTP 接口详述（字段表 + 错误码 + curl 示例） |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | 从零到上线 11 步 + 故障排查表 |
| [docs/DATA-MODEL.md](docs/DATA-MODEL.md) | 服务端关系模型、客户端域模型、同步协议、IndexedDB |
| [docs/SECURITY.md](docs/SECURITY.md) | 密钥派生、令牌策略、威胁模型与能力边界 |
| [docs/EXTENDING.md](docs/EXTENDING.md) | 新增模块的标准步骤与二期预留点 |

---

## 12. 安全提醒（务必先读）

> ### ⚠️ 服务端无法帮你找回密码
>
> MedDesk 是**端到端加密**：服务端只保存密文信封和 KDF 参数，**没有**你的口令、**没有**你的数据密钥。
> 这意味着：
>
> 1. **忘记口令 = 数据永久不可恢复**，全世界没有任何人（包括你自己部署的服务）能解开。
> 2. 因此请务必：
>    - 把口令写进密码管理器并**离线备份**；
>    - 定期在「**设置 → 导出 JSON 备份**」保存一份明文快照（它是唯一不依赖服务端可读性就能恢复的数据）；
>    - 服务端备份（`deploy/scripts/backup.sh`）**只备份密文**，单独存在是不够的。
> 3. 修改 `MEDDESK_JWT_SECRET` 会让所有已签发的访问令牌失效，用户需要重新登录；本地 IndexedDB 数据不受影响。
> 4. 公网部署请把 `MEDDESK_SIGNUP_ENABLED` 设为 `false`，并考虑 IP 白名单 / 前置鉴权。
>
> 详细威胁模型与安全基线见 👉 **[docs/SECURITY.md](docs/SECURITY.md)**。

---

## 13. 许可证与致谢

- 模块路径：`github.com/qiubite666/meddesk`
- 前端字体遵循「零外链」原则：PDF 渲染使用的中文字体需自行准备（见 `deploy/scripts/get-font.sh`）
- 感谢所有为端到端加密隐私工具做出贡献的先行者

> 如果你发现文档与代码不一致，请以 [`CONTRACTS.md`](CONTRACTS.md) 与源码为准，并提 issue。
