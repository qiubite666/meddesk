# MedDesk · HTTP API 文档（v1）

> 本文是 [`CONTRACTS.md`](../CONTRACTS.md) 第 3 节的展开说明，字段以 [`internal/model/model.go`](../internal/model/model.go) 为准。
>
> - **Base URL**：`https://med.example.com`（本地开发：`http://localhost:8089`）
> - **所有接口路径前缀**：`/api/v1`
> - **字符编码**：UTF-8；请求与响应均为 `application/json`（附件上传除外，用 `multipart/form-data`）
> - **契约总则**：成功**直接返回业务对象**；失败返回 `{"code","message","details"}`，HTTP 状态码由错误码映射。

---

## 目录

- [1. 通用约定](#1-通用约定)
- [2. 错误码表](#2-错误码表)
- [3. 健康检查](#3-健康检查)
- [4. 认证接口 /auth](#4-认证接口-auth)
- [5. 同步接口 /sync](#5-同步接口-sync)
- [6. 附件接口 /attachments](#6-附件接口-attachments)
- [7. 导出接口 /export](#7-导出接口-export)
- [8. 客户端调用流程](#8-客户端调用流程)
- [9. 记账本接口 /ledger](#9-记账本接口-ledger)
  - [9.1 分类 /ledger/categories](#91-分类-ledgercategories)
  - [9.2 账单 /ledger/records](#92-账单-ledgerrecords)
  - [9.3 统计 summary / trend / breakdown](#93-统计-summary--trend--breakdown)
  - [9.4 报销 /ledger/reimbursements](#94-报销-ledgerreimbursements)
  - [9.5 凭证附件 /ledger/attachments](#95-凭证附件-ledgerattachments)
  - [9.6 导出与导入 /ledger/export, /ledger/import](#96-导出与导入-ledgerexport-ledgerimport)
  - [9.7 WebDAV 每日备份 /ledger/backup](#97-webdav-每日备份-ledgerbackup)
- [10. 管理后台接口 /admin](#10-管理后台接口-admin)
- [11. 路由总览与文档覆盖范围](#11-路由总览与文档覆盖范围)
- [附录 A：完整 curl 冒烟脚本](#附录-a完整-curl-冒烟脚本)

---

## 1. 通用约定

### 1.1 鉴权

除健康检查与部分认证接口外，所有接口需要在请求头携带访问令牌：

```http
Authorization: Bearer <accessToken>
```

- 令牌类型 claim `typ="access"`，`sub` 为用户 UUID HS256 签名。
- 有效期 `MEDDESK_ACCESS_TTL`（默认 `60m`）。
- 过期 / 无效 / 已吊销 → `401 unauthorized`，客户端应静默调用 `/api/v1/auth/refresh` 换新后重试一次。
- 中间件会把用户 ID 写入 gin 上下文键 `user_id`，业务 handler 据此做**数据隔离**：所有查询都带 `user_id` 条件，跨用户访问必然 `404`。

### 1.2 请求头

| 头 | 说明 |
|---|---|
| `Authorization: Bearer <jwt>` | 鉴权（可选接口不需要） |
| `Content-Type: application/json` | JSON 请求体必填 |
| `X-Request-ID` | 可选。不传则由服务端生成并回写到响应；排障时请提供该值 |

### 1.3 通用响应头

由 `internal/middleware.SecurityHeaders()` 下发：

| 头 | 值 |
|---|---|
| `Content-Security-Policy` | `default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'self'; frame-ancestors 'none'; object-src 'none'` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer`（应用层；Nginx 层为 `strict-origin-when-cross-origin`） |
| `X-Frame-Options` | `DENY`（应用层；Nginx 层为 `SAMEORIGIN`） |
| `Permissions-Policy` | `geolocation=(self), camera=(self), microphone=(), payment=(), usb=(), interest-cohort=()` |
| `Cross-Origin-Opener-Policy` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` |
| `Strict-Transport-Security` | `max-age=31536000`（仅当判定为 HTTPS 时下发） |

### 1.4 限流

三层叠加，任一层触发都会返回 `429`：

| 层 | 位置 | 默认策略 |
|---|---|---|
| Nginx `limit_req` | `deploy/nginx/meddesk.conf` | 普通 API `10r/s` burst 30；鉴权 `10r/m` burst 5；导出/附件 `10r/m` burst 5 |
| 应用 `middleware.RateLimit` | `/api/v1` 组 | 令牌桶，键=客户端 IP，`MEDDESK_RATE_LIMIT_RPS=20` / `BURST=60` |
| 登录失败计数 | `auth` 服务 | 相同邮箱 15 分钟内失败 ≥10 次 → `429 rate_limited` |

> 429 响应带 `Retry-After` 头。若前面有 Nginx，必须设 `MEDDESK_TRUST_PROXY=true`，
> 否则应用看到的全是 `127.0.0.1`，限流会「一荣俱荣、一损俱损」。

### 1.5 请求体大小限制

| 场景 | 上限 | 来源 | 触发的错误码 |
|---|---|---|---|
| 全局 `BodyLimit` | `MEDDESK_UPLOAD_MAX_BYTES`（默认 8 MiB = 8388608 字节） | `config.Upload.MaxBytes` | `payload_too_large` (413) |
| 加密附件上传 `/api/v1/attachments` | 同上（默认 8 MiB） | `internal/attachments/http.go` | `payload_too_large` (413) |
| 导出 `/api/v1/export/*` | 硬编码 **4 MiB**（`maxExportBody = 4 << 20`） | `internal/export/http.go` | `payload_too_large` (413) |
| 记账本凭证上传 `POST /api/v1/ledger/attachments` | 整个 multipart 请求体 **2 MiB + 64 KiB**（2162688 字节），单文件必须 ≤ 2 MiB（2097152 字节） | `maxUploadBodyBytes`，见 [9.5](#95-凭证附件-ledgerattachments) | `payload_too_large` (413) |
| WebDAV 代理 `/api/v1/webdav/*` | **200 MiB**（覆盖全局上限） | `server.webdavUploadLimit` | `payload_too_large` (413) |
| Nginx `client_max_body_size` | `12m` | `deploy/nginx/meddesk.conf` | Nginx 直接返回 413 HTML |

> 记账本导入 `POST /api/v1/ledger/import` 走**全局** 8 MiB 上限；带 base64 附件字节的备份文件很容易顶到该限制，
> 建议导入前用 `GET /ledger/export/json?includeAttachments=0` 生成不含附件字节的备份（见 [9.6](#96-导出与导入-ledgerexport-ledgerimport)）。

### 1.6 路由总览

`internal/server/router.go` 实际挂载的全部对外路由组（均已核对源码）：

| 前缀 | 模块 | 鉴权 | 本文档章节 |
|---|---|---|---|
| `/api/v1/health`、`/api/v1/ready` | 探针 | 否 | [第 3 章](#3-健康检查) |
| `/api/v1/auth/*` | `internal/auth`（12 个） | 部分需登录 | [第 4 章](#4-认证接口-auth) |
| `/api/v1/sync/*` | `internal/syncsvc` | ✅ | [第 5 章](#5-同步接口-sync) |
| `/api/v1/attachments/*` | 加密健康附件 | ✅ | [第 6 章](#6-附件接口-attachments) |
| `/api/v1/export/*` | 健康数据导出（PDF/CSV） | ✅ | [第 7 章](#7-导出接口-export) |
| `/api/v1/ledger/*` | 记账本（26 个） | ✅ | **[第 9 章](#9-记账本接口-ledger)** |
| `/api/v1/admin/*` | 管理后台（7 个） | 后台令牌 | **[第 10 章](#10-管理后台接口-admin)** |
| `/api/v1/family/*` | 家庭关联 | ✅ | 不在本文展开，见 [`CONTRACTS-FAMILY-LINK.md`](./CONTRACTS-FAMILY-LINK.md) |
| `/api/v1/webdav/*` | WebDAV 网盘代理 | ✅ | 不在本文展开，见 [`SECURITY.md`](./SECURITY.md) |
| `/admin`、`/admin/app.js` | 后台页面与脚本 | 否（页面） | [10.9](#109-后台页面入口) |

> **可选挂载**：`/ledger/*` 的三个子模块（凭证附件、导出导入、备份）与 `/family`、`/admin` 都用「可选接口断言」挂载，
> 对应仓储缺失或未配置白名单邮箱时**这些路由不存在**，调用会返回 `404 {"code":"not_found","message":"接口不存在"}`。

---

## 2. 错误码表

错误响应统一格式：

```json
{
  "code": "bad_request",
  "message": "同步数据校验失败",
  "details": { "items": [ { "index": 0, "table": "intake", "id": "abc", "field": "ct", "reason": "不是合法的 base64 字符串" } ] }
}
```

| code | HTTP | 含义 | 典型触发场景 |
|---|---|---|---|
| `bad_request` | 400 | 请求参数不合法 | JSON 解析失败、字段缺失、同步入参校验失败、表名单外、`id` 超长、`updatedAt` 超前 5 分钟 |
| `unauthorized` | 401 | 未认证 / 会话失效 | 令牌缺失/过期/签名错误、刷新令牌被重放、邮箱或口令不正确 |
| `forbidden` | 403 | 禁止访问 | `MEDDESK_SIGNUP_ENABLED=false` 时调用注册 |
| `not_found` | 404 | 资源不存在 | 附件 id 不存在（或不属于当前用户）、未知 `/api/*` 路径 |
| `conflict` | 409 | 资源冲突 | 邮箱已被注册 |
| `payload_too_large` | 413 | 请求体超限 | 附件超过 8 MiB、导出超过 4 MiB |
| `rate_limited` | 429 | 触发限流 | Nginx/应用层令牌桶耗尽、登录失败次数超限 |
| `unavailable` | 503 | 服务暂不可用 | **PDF 中文字体缺失**、Redis 与 DB 同时不可用导致 `/ready` 降级/失败 |
| `not_implemented` | 501 | 未实现 | 预留 |
| `internal` | 500 | 服务器内部错误 | 未知异常；**响应体只含通用文案，细节只写日志** |

> `details` 字段：仅 `bad_request` 保留结构化详情（`auth` 包与 `syncsvc` 包均对 `internal` 做了 `Details=nil` 净化）。

---

## 3. 健康检查

### 3.1 `GET /api/v1/health`

**鉴权**：否。**说明**：存活探针，永远返回 `200`（除非进程已挂）。

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `status` | string | 固定 `"ok"` |
| `version` | string | 服务版本号，如 `"0.1.0"` |
| `db` | string | `"ok"` / `"down"` |
| `cache` | string | `"ok"` / `"down"`（Redis 关闭时自降级为 `ok`） |
| `serverTime` | int64 | 服务端当前 Unix 毫秒时间戳 |

```bash
curl -fsS http://127.0.0.1:8089/api/v1/health
```

```json
{"status":"ok","version":"0.1.0","db":"ok","cache":"ok","serverTime":1761446400000}
```

### 3.2 `GET /api/v1/ready`

**鉴权**：否。**说明**：就绪探针。判定规则：

| 条件 | `status` | HTTP |
|---|---|---|
| DB 可用 + Cache 可用 | `ok` | 200 |
| DB 可用 + Cache 不可用 | `degraded`（可降级运行） | 200 |
| DB 不可用 | `down` | **503** |

响应字段与 `/health` 完全一致。

```bash
curl -fsS http://127.0.0.1:8089/api/v1/ready
```

---

## 4. 认证接口 `/auth`

**完整路由清单**（`internal/auth/handler.go` `Mount`，共 12 个；✅ 需 `Bearer` 访问令牌）：

| # | 方法 | 路径 | 鉴权 | 本节位置 | 说明 |
|---|---|---|---|---|---|
| 1 | GET | `/api/v1/auth/captcha` | 否 | [4.10](#410-get-apiv1authcaptcha) | 下发图形验证码 |
| 2 | POST | `/api/v1/auth/register` | 否 | [4.1](#41-post-apiv1authregister) | 注册（校验验证码） |
| 3 | POST | `/api/v1/auth/login` | 否 | [4.2](#42-post-apiv1authlogin) | 登录 |
| 4 | POST | `/api/v1/auth/salt` | 否 | [4.11](#411-post-apiv1authsalt) | 取 KDF 盐（新设备握手，防账户枚举） |
| 5 | POST | `/api/v1/auth/refresh` | 否（凭 refresh 令牌） | [4.3](#43-post-apiv1authrefresh) | 令牌轮换 |
| 6 | POST | `/api/v1/auth/logout` | 否（凭 refresh 令牌） | [4.4](#44-post-apiv1authlogout) | 单设备登出 |
| 7 | POST | `/api/v1/auth/logout-all` | ✅ | [4.5](#45-post-apiv1authlogout-all) | 全设备登出 |
| 8 | POST | `/api/v1/auth/change-password` | ✅ | [4.6](#46-post-apiv1authchange-password) | 改口令并重包裹 DEK |
| 9 | GET | `/api/v1/auth/recovery-key` | 否 | [4.12](#412-get-apiv1authrecovery-key) | 托管恢复公钥 |
| 10 | POST | `/api/v1/auth/recovery-envelope` | ✅ | [4.13](#413-post-apiv1authrecovery-envelope) | 开启/关闭托管恢复 |
| 11 | POST | `/api/v1/auth/repair-envelope` | ✅ | [4.14](#414-post-apiv1authrepair-envelope) | 自愈回填信封 |
| 12 | GET | `/api/v1/auth/me` | ✅ | [4.7](#47-get-apiv1authme) | 当前用户信息 |

> 典型新设备登录顺序：`POST /auth/salt` 取盐 → 客户端 PBKDF2 派生 → `POST /auth/login`。
> 注册顺序：`GET /auth/captcha` 取码 → `POST /auth/register`（带 `captchaId` / `captchaCode`）。

### 4.1 `POST /api/v1/auth/register`

**鉴权**：否。**说明**：注册新用户并直接返回令牌对。

> ⚠ **`MEDDESK_SIGNUP_ENABLED=false` 时返回 `403 forbidden`（"当前未开放注册"）。**

> 🔐 **服务端收不到明文口令**：`passwordHash` 是客户端 `PBKDF2-SHA256(password, salt, 210000, 32B)` 的 base64 结果；
> 服务端再对它做 **argon2id** 二次哈希后落库，永远不知道原始口令是什么。

**请求字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `email` | string | ✅ | `required,email,max=160` | 邮箱，大小写不敏感存储 |
| `passwordHash` | string | ✅ | `required,min=32,max=128` | 客户端 PBKDF2 派生结果的 base64 |
| `kdfSalt` | string | ✅ | `required` | base64，客户端生成的 16 字节 salt |
| `wrappedDek` | string | ✅ | `required` | base64，被 KEK 包裹的数据密钥（服务端无法解开） |
| `dekIv` | string | ✅ | `required` | base64，包裹 DEK 所用的 12 字节 AES-GCM IV |
| `captchaId` | string | ✅ | `max=64` | 图形验证码 id，取自 [`GET /auth/captcha`](#410-get-apiv1authcaptcha) |
| `captchaCode` | string | ✅ | `max=16` | 用户填写的验证码（6 位数字，字符集 `2-9`） |
| `device` | object | ❌ | — | 设备信息，见下方 `DeviceInfo` |

> 验证码**在业务逻辑之前**校验：任何失败路径（邮箱已存在、未开放注册）都会使这张码作废，
> 因此一次注册失败后必须重新取码。同一张码累计答错 **5 次**即整体作废，有效期 **5 分钟**。

`DeviceInfo`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `deviceId` | string | 客户端生成的设备标识（可选） |
| `userAgent` | string | 浏览器 UA（可选，落库前截断到 256 字节） |
| `ip` | string | 客户端 IP（可选，通常不必填，服务端自行取） |

**响应**：`TokenResponse`，见 [4.9](#49-tokenresponse-结构)。

**错误**

| HTTP | code | 场景 |
|---|---|---|
| 400 | `bad_request` | 参数缺失/格式错误、`kdfSalt`/`wrappedDek`/`dekIv` 不是合法 base64 |
| 403 | `forbidden` | 未开放注册 |
| 409 | `conflict` | 该邮箱已注册 |

```bash
curl -fsS https://med.example.com/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{
        "email":"me@example.com",
        "passwordHash":"<PBKDF2 派生的 base64，≥32 字符>",
        "kdfSalt":"<16 字节 salt 的 base64>",
        "wrappedDek":"<AES-GCM(KEK, dekIv, DEK) 的 base64>",
        "dekIv":"<12 字节 IV 的 base64>",
        "device":{"deviceId":"pwa-abc123","userAgent":"Mozilla/5.0 …"}
      }'
```

### 4.2 `POST /api/v1/auth/login`

**鉴权**：否。**说明**：登录并签发令牌对。

**请求字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `email` | string | ✅ | `required,email,max=160` | |
| `passwordHash` | string | ✅ | `required,min=32,max=128` | 客户端 PBKDF2 派生结果 |
| `device` | object | ❌ | — | 同上 |

**响应**：`TokenResponse`（含 `kdfSalt` / `wrappedDek` / `dekIv`，客户端据此本地解出 DEK）。

**错误**

| HTTP | code | 场景 |
|---|---|---|
| 400 | `bad_request` | 参数缺失或格式错误 |
| 401 | `unauthorized` | 「邮箱或口令不正确」（不区分邮箱是否存在，防用户枚举） |
| 429 | `rate_limited` | 「登录尝试过于频繁，请稍后再试」（15 分钟内失败 ≥10 次） |

> 登录成功会清零该邮箱的失败计数。

```bash
curl -fsS https://med.example.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","passwordHash":"<PBKDF2 派生值的 base64>"}'
```

### 4.3 `POST /api/v1/auth/refresh`

**鉴权**：否（靠 refreshToken 本身鉴权）。**说明**：刷新令牌**轮换**（rotation）。

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `refreshToken` | string | ✅ | 上一次获取的刷新令牌原文（base64url，32 字节随机） |
| `device` | object | ❌ | 设备信息 |

**响应**：全新的 `TokenResponse`（新的 accessToken **和** 新的 refreshToken）。

**轮换与重放检测规则（重要）**

```
1. 库中只存 sha256(refreshToken)，原文仅在签发那一次返回给客户端。
2. 每次刷新：旧令牌 → 标记 replaced_by=<新令牌 id> 并置 revoked_at；
             旧令牌 id 进入 Redis 吊销名单 meddesk:auth:revoked:<id>。
3. 已轮换的令牌在 MEDDESK_REFRESH_REUSE_GRACE（默认 30s）宽限期内允许并发重用一次
   （多标签页同时刷新场景）；宽限机会只在首次轮换时发放，不可续期。
4. 超出宽限期再次使用该令牌 → 判定为重放攻击
   → 撤销该用户【全部】刷新令牌，返回 401 unauthorized。
5. 被显式吊销（登出/改密）的令牌再次出现 → 同样撤销全部会话。
```

**错误**

| HTTP | code | 场景 |
|---|---|---|
| 400 | `bad_request` | 缺少 `refreshToken` |
| 401 | `unauthorized` | 「登录已失效，请重新登录」：令牌不存在/过期/已吊销/重放 |

```bash
curl -fsS https://med.example.com/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d '{"refreshToken":"<上一次返回的 refreshToken>"}'
```

### 4.4 `POST /api/v1/auth/logout`

**鉴权**：否（接口本身不需要 Bearer，靠 `refreshToken` 定位会话）。**说明**：撤销单个刷新令牌，**幂等**。

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `refreshToken` | string | ✅ | 要撤销的刷新令牌原文 |

**响应**

```json
{"ok": true}
```

> 令牌不存在或已吊销也返回 `{"ok":true}`，方便客户端无条件调用。

```bash
curl -fsS -X POST https://med.example.com/api/v1/auth/logout \
  -H 'Content-Type: application/json' \
  -d '{"refreshToken":"<你的刷新令牌>"}'
```

### 4.5 `POST /api/v1/auth/logout-all`

**鉴权**：✅ 是。**说明**：撤销当前用户的**全部**刷新令牌（所有设备下线）。

**请求**：无请求体。

**响应**：`{"ok": true}`

```bash
curl -fsS -X POST https://med.example.com/api/v1/auth/logout-all \
  -H "Authorization: Bearer $ACCESS"
```

### 4.6 `POST /api/v1/auth/change-password`

**鉴权**：✅ 是。**说明**：修改口令并**重新包裹 DEK**。

> 🔐 **关键语义**：
> - 客户端用**新口令**重新派生 `newPasswordHash`，并用新 KEK **重新包裹同一个 DEK**；
> - 服务端更新 `kdf_salt` / `wrapped_dek` / `dek_iv` 与 `password_hash`（argon2id 新串行）；
> - **DEK 不变**，因此历史密文**无需重新加密**；
> - 成功后服务端会**撤销该用户全部刷新令牌**，客户端需要用返回值重新登录或另行获取令牌。

**请求字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `oldPasswordHash` | string | ✅ | `required,min=32,max=128` | 旧口令的 PBKDF2 结果，用于校验身份 |
| `newPasswordHash` | string | ✅ | `required,min=32,max=128` | 新口令的 PBKDF2 结果 |
| `kdfSalt` | string | ✅ | `required` | 新 salt 的 base64 |
| `wrappedDek` | string | ✅ | `required` | 新 KEK 包裹后的 DEK |
| `dekIv` | string | ✅ | `required` | 新的包裹 IV |

**响应**：`{"ok": true}`

**错误**

| HTTP | code | 场景 |
|---|---|---|
| 400 | `bad_request` | 参数缺失或 base64 非法 |
| 401 | `unauthorized` | 旧口令不正确 / 会话失效 |

> Nginx 对该 location 的 `proxy_read_timeout` 特意放宽到 30s——**改密码要做 argon2id**，别设太短。

```bash
curl -fsS -X POST https://med.example.com/api/v1/auth/change-password \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{
        "oldPasswordHash":"<旧 PBKDF2 base64>",
        "newPasswordHash":"<新 PBKDF2 base64>",
        "kdfSalt":"<新 salt base64>",
        "wrappedDek":"<新包裹后的 DEK base64>",
        "dekIv":"<新 IV base64>"
      }'
```

### 4.7 `GET /api/v1/auth/me`

**鉴权**：✅ 是。**说明**：返回当前用户信息。

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `user.id` | string | 用户 UUID |
| `user.email` | string | 邮箱 |
| `user.createdAt` | string(RFC3339) | 注册时间（`timestamptz`） |
| `serverTime` | int64 | 服务端当前 Unix 毫秒，用于客户端时钟对齐 |
| `recoveryEnabled` | bool | 该账号是否开启了「托管恢复」（已存在 `recovery_wrapped_dek`） |
| `isAdmin` | bool | 该邮箱是否命中 `MEDDESK_ADMIN_EMAILS` 白名单；**仅用于前端决定是否显示后台入口**，真正的权限校验在 `/api/v1/admin/*` |

```bash
curl -fsS https://med.example.com/api/v1/auth/me -H "Authorization: Bearer $ACCESS"
```

```json
{"user":{"id":"6f1c…","email":"me@example.com","createdAt":"2026-10-26T02:15:00Z"},
 "serverTime":1761446400000,"recoveryEnabled":false,"isAdmin":false}
```

### 4.8 认证错误文案一览

| message | 触发点 |
|---|---|
| `邮箱或口令不正确` | 登录失败（含邮箱不存在） |
| `登录已失效，请重新登录` | 刷新令牌失效/重放、其余需鉴权接口 token 无效 |
| `登录尝试过于频繁，请稍后再试` | 登录失败计数超限 |
| `当前未开放注册` | `SIGNUP_ENABLED=false` |
| `该邮箱已注册` | 邮箱重复 |
| `服务器内部错误` | 兜底，不回显细节 |

---

### 4.9 TokenResponse 结构

注册 / 登录 / 刷新三个接口共用。

| 字段 | 类型 | 说明 |
|---|---|---|
| `accessToken` | string | JWT，用于 `Authorization: Bearer` |
| `refreshToken` | string | base64url（32 字节随机），**仅本次返回**，请妥善保存 |
| `tokenType` | string | 固定 `"Bearer"` |
| `expiresIn` | int64 | accessToken 剩余**秒数** |
| `userId` | string | 用户 UUID |
| `kdfSalt` | string | base64，注册时提交的 salt 原样回显 |
| `wrappedDek` | string | base64，被 KEK 包裹的 DEK（服务端无法解开） |
| `dekIv` | string | base64，包裹 DEK 的 IV |

### 4.10 `GET /api/v1/auth/captcha`

**鉴权**：否。**说明**：下发一张 6 位数字图形验证码，`POST /auth/register` 必须携带。

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `captchaId` | string | 验证码 id（UUID），注册时放进 `captchaId` |
| `svg` | string | **完整 SVG 源码**（非 base64），前端直接 `innerHTML` 插入即可渲染 |
| `expiresIn` | int | 剩余有效**秒数**（默认 300） |

**响应头**：`Cache-Control: no-store`（验证码是瞬时凭证，禁止任何中间层缓存）。

**错误**：`500 internal`（生成失败，极少见，直接重试即可）。

```bash
curl -fsS https://med.example.com/api/v1/auth/captcha
```

```json
{"captchaId":"b3a1…","svg":"<svg xmlns=\"http://www.w3.org/2000/svg\" …>…</svg>","expiresIn":300}
```

> 校验规则：不区分「不存在 / 已过期 / 已用过 / 输错了」，一律返回同一句 `验证码错误或已过期，请点击图片刷新后重试`；
> 同一 `captchaId` 累计失败 5 次后整张作废。验证码字符集为 `2-9`（刻意排除易混淆的 `0`/`1`，且手机端可直接唤起数字键盘）。

### 4.11 `POST /api/v1/auth/salt`

**鉴权**：否。**说明**：新设备登录前的盐值握手。返回该账号注册时的 KDF salt，客户端据此派生 `passwordHash`。

> **防账户枚举**：邮箱不存在时返回**随机生成的 salt**（用假盐派生出的哈希去登录必然失败，
> 外部表现与「邮箱存在但口令错误」完全一致）。

**请求字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `email` | string | ✅ | `required,email,max=160` | 目标邮箱 |

**响应**

```json
{"kdfSalt":"<注册时提交的 salt 的 base64>"}
```

**错误**：`400 bad_request`（邮箱缺失或格式非法）。**注意**：邮箱不存在时返回 `200` + 随机盐。

```bash
curl -fsS -X POST https://med.example.com/api/v1/auth/salt \
  -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com"}'
```

### 4.12 `GET /api/v1/auth/recovery-key`

**鉴权**：否。**说明**：下发「托管恢复」RSA 公钥，供客户端用它包裹一份 DEK 上传（服务端可用私钥解出 → 支持管理员无损改口令）。

**响应字段**

| 字段 | 类型 | 说明 |
|---|---|---|
| `enabled` | bool | 本部署是否启用托管恢复（取决于 `MEDDESK_RECOVERY_KEY`） |
| `publicKey` | string | base64(SPKI DER) 公钥；`enabled=false` 时为空串 |

**响应头**：`Cache-Control: no-cache`。

```bash
curl -fsS https://med.example.com/api/v1/auth/recovery-key
```

```json
{"enabled":true,"publicKey":"MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A…"}
```

> 未启用时**始终返回 200 + `enabled=false`**，前端据此隐藏设置页开关，不要按 404 处理。

### 4.13 `POST /api/v1/auth/recovery-envelope`

**鉴权**：✅ 是。**说明**：开启或关闭当前账号的托管恢复。

> 客户端用 [4.12](#412-get-apiv1authrecovery-key) 的公钥包裹一份 DEK 后上传；上传即开启，传空串即关闭。
> 服务端**不会**用这份 DEK 解密任何业务数据，只在管理员执行 `mode=escrow` 改口令时使用（见 [10.7](#107-post-apiv1adminusersidpassword)）。

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `wrappedDek` | string | ❌ | base64，被恢复公钥包裹的 DEK；**传空串表示关闭托管恢复** |
| `dekIv` | string | ❌ | base64，包裹 DEK 的 IV |

**响应**

```json
{"ok": true, "enabled": true}
```

（`enabled` 即 `wrappedDek != ""` 的回显。）

**错误**：`401 unauthorized`（会话失效）、`500 internal`（写入失败）。

```bash
curl -fsS -X POST https://med.example.com/api/v1/auth/recovery-envelope \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"wrappedDek":"<RSA-OAEP(恢复公钥, DEK) 的 base64>","dekIv":"<IV base64>"}'
```

### 4.14 `POST /api/v1/auth/repair-envelope`

**鉴权**：✅ 是。**说明**：信封自愈——某台**仍持有 DEK 的旧设备**登录后，用当前口令派生的新 KEK 重新包裹 DEK 并回填，令信封与新口令重新对齐。

> 触发场景：管理员用 `mode=keep` 改过口令后（旧 `wrapped_dek` 与新 KEK 不匹配，见 [10.7](#107-post-apiv1adminusersidpassword)），
> 账号处于「待自愈」状态；此接口是让数据重新可用的路径之一。

**请求字段**：与 [4.13](#413-post-apiv1authrecovery-envelope) 完全相同（`wrappedDek` / `dekIv`），语义为「**新 KEK 包裹的** DEK」。

**响应**

```json
{"ok": true}
```

**错误**：`400 bad_request`（参数非法）、`401 unauthorized`（会话失效）。

```bash
curl -fsS -X POST https://med.example.com/api/v1/auth/repair-envelope \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"wrappedDek":"<新 KEK 包裹的 DEK base64>","dekIv":"<新 IV base64>"}'
```

---

## 5. 同步接口 `/sync`

> **隐私前提**：服务端只处理**密文信封**（`iv` + `ct`）与其元数据，**从不解析业务字段**。
> 下表所有字段中，只有 `table`/`id`/`ver`/`updatedAt`/`deleted`/`deviceId`/`seq` 是明文元数据，其余全是密文。

### 5.1 核心结构 `SyncItem`

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `table` | string | ✅ | 必须命中白名单 | 逻辑表名，取值见下表 |
| `id` | string | ✅ | ≤64 字符，字符集 `[A-Za-z0-9._:-]` | 客户端生成的 UUIDv4 |
| `ver` | int64 | ✅ | ≥1 | 客户端版本号，每次本地修改自增 |
| `updatedAt` | int64 | ✅ | >0 且 ≤ 服务端时间 +5min | 客户端修改时刻（Unix 毫秒） |
| `deleted` | bool | ✅ | — | 墓碑标记 |
| `iv` | string | ✅ | 合法 base64，解码后 12 字节 | AES-GCM 初始向量 |
| `ct` | string | 条件 | 合法 base64，解码后 ≤ `MEDDESK_SYNC_MAX_RECORD_BYTES`（1 MiB） | 密文；**`deleted=true` 时可为空串** |
| `deviceId` | string | ❌ | — | 产生该修改的设备标识 |
| `seq` | int64 | — | **仅出现在响应中** | 服务端单调递增序列，`pull` 的游标依据 |

**`table` 白名单**（`model.KnownTables()`）

| 值 | 含义 |
|---|---|
| `profile` | 家庭成员档案 |
| `medication` | 药物 |
| `schedule` | 用药排程 |
| `intake` | 服药记录（事件型） |
| `setting` | 跨设备同步的设置 |

### 5.2 `POST /api/v1/sync/push`

**鉴权**：✅ 是。**说明**：上传本地变更。

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `changes` | `SyncItem[]` | ✅ | 本次要上传的变更；**空数组合法**（仅回传当前游标） |

**响应字段** `SyncPushResponse`

| 字段 | 类型 | 说明 |
|---|---|---|
| `applied` | string[] | 已应用的 `"table:id"` 列表 |
| `conflicts` | `SyncItem[]` | **服务端当前胜出版本**（客户端需解密后合并） |
| `cursor` | int64 | 应用后服务端最大 `seq` |
| `syncedAt` | int64 | 服务端时间（Unix 毫秒） |

**校验规则**（任一条不合法 → **整批拒绝**，返回 `400 bad_request`）

| 校验 | 说明 |
|---|---|
| 条数 ≤ `MEDDESK_SYNC_MAX_ITEMS`（默认 500） | 违反 → details 中 `index:-1`，`field:"changes"` |
| `table` 在白名单内 | 否则 `field:"table"` |
| `id` 非空、≤64 字符、字符集合法 | 否则 `field:"id"` |
| 同批次 `(table,id)` 不重复 | 否则提示「与第 N 条重复」 |
| `ver` ≥ 1 | 否则 `field:"ver"` |
| `updatedAt` > 0 且不超过服务端时间 +5 分钟 | 容忍客户端时钟轻微前移 |
| `iv` 为合法 base64 | 支持带填充 / 无填充两种写法 |
| `ct` 非空（除非 `deleted=true`）且解码后 ≤1 MiB | 否则 `field:"ct"` |

details 结构：

```json
{"items":[{"index":0,"table":"intake","id":"9f2…","field":"ct","reason":"不是合法的 base64 字符串"}]}
```

**冲突裁决（服务端权威，在单个事务内完成）**

```
better(in, cur) :=  in.ver      >  cur.ver
                 || (in.ver == cur.ver && in.updatedAt      >  cur.updatedAt)
                 || (in.ver == cur.ver && in.updatedAt == cur.updatedAt && in.deleted && !cur.deleted)

better == true  → 覆盖写入，进 applied
better == false → 服务端当前版本进 conflicts（客户端负责合并，服务端不代合并）
```

> **墓碑删除**：删除不是物理删除，而是写一条 `deleted=true` 的记录（`ct` 可空，`iv` 仍必填）。
> 它同样参与版本裁决并占用 `seq`，保证其他设备 `pull` 时能感知到删除。

```bash
curl -fsS -X POST https://med.example.com/api/v1/sync/push \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{
        "changes":[
          {"table":"medication","id":"6f1c…","ver":3,"updatedAt":1761446400000,
           "deleted":false,"iv":"<12B IV base64>","ct":"<密文 base64>","deviceId":"pwa-abc123"}
        ]
      }'
```

```json
{"applied":["medication:6f1c…"],"conflicts":[],"cursor":128,"syncedAt":1761446401234}
```

### 5.3 `GET /api/v1/sync/pull`

**鉴权**：✅ 是。**说明**：增量拉取 `seq > cursor` 的记录，按 `seq` 升序，需翻页直至 `hasMore=false`。

**查询参数**

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `cursor` | int64 | `0` | 上次拉取返回的 `cursor`；必须是非负整数，非法 → 400 |
| `limit` | int | `MEDDESK_SYNC_PULL_PAGE`（300） | 分页大小；超过 `MEDDESK_SYNC_PULL_PAGE_MAX`（1000）自动钳制；≤0 取默认值；非法 → 400 |

**响应字段** `SyncPullResponse`

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | `SyncItem[]` | 密文记录（含服务端 `seq`）；无数据时为 `[]` |
| `cursor` | int64 | **本次返回的最大 `seq`**，客户端应持久化作为下次起点 |
| `hasMore` | bool | 是否还有更多数据（需继续翻页） |

```bash
curl -fsS "https://med.example.com/api/v1/sync/pull?cursor=0&limit=300" \
  -H "Authorization: Bearer $ACCESS"
```

### 5.4 `GET /api/v1/sync/stats`

**鉴权**：✅ 是。**说明**：设置页展示同步概况。

**响应字段** `SyncStatsResponse`

| 字段 | 类型 | 说明 |
|---|---|---|
| `cursor` | int64 | 当前最大 `seq` |
| `recordCount` | int64 | 该用户同步记录总数 |
| `byTable` | map<string,int64> | 按表分组的计数，如 `{"medication":12,"intake":340}` |
| `serverTime` | int64 | 服务端时间 |

```bash
curl -fsS https://med.example.com/api/v1/sync/stats -H "Authorization: Bearer $ACCESS"
```

### 5.5 `POST /api/v1/sync/reset`

**鉴权**：✅ 是。**说明**：清空该用户**全部**云端同步记录。

> ### ⚠️⚠️ 危险操作
> 此接口会**物理删除**该用户在 `sync_records` 中的全部数据（`sync_state` 游标一并归零）。
> 虽然数据本身是密文，但如果某台设备本地 IndexedDB 已被清空，删除后**该设备再也拉不回历史数据**。
> **执行前请确认至少有一台设备持有完整本地数据，或已导出 JSON 备份。**

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `confirm` | string | ✅ | 必须**精确等于** `DELETE-ALL`，否则返回 400 |

**响应**

```json
{"ok": true, "cursor": 0}
```

**错误**：`400 bad_request`，message `"清空云端同步数据需提交确认词 DELETE-ALL"`

```bash
curl -fsS -X POST https://med.example.com/api/v1/sync/reset \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"confirm":"DELETE-ALL"}'
```

> 服务端会以 `WARN` 级别记录该操作（含 `user_id` 与 `request_id`），便于审计。

---

## 6. 附件接口 `/attachments`

> 附件（药物照片）在**客户端**压缩（最长边 ≤1280px，JPEG q=0.82）并加密后再上传；
> 服务端**只存密文文件**（`{UploadDir}/{userID}/{yy}/{id}.bin`）与 IV，**不落任何明文**，也**无法嗅探真实文件类型**。

### 6.1 `POST /api/v1/attachments`

**鉴权**：✅ 是。**Content-Type**：`multipart/form-data`。

**表单字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `file` | file | ✅ | ≤ `MEDDESK_UPLOAD_MAX_BYTES`（默认 8 MiB） | **密文二进制**（不是原图！） |
| `iv` | text | ✅ | base64，解码后必须 **12 字节** | 该附件的 AES-GCM IV |
| `recordId` | text | ❌ | ≤64 字符 | 关联的业务记录 id（通常是 `medication.id`） |
| `mime` | text | ❌ | 必须在白名单内 | 客户端声明的类型；缺省取文件部分 Content-Type；都没有则 `application/octet-stream` |

**mime 白名单**：`image/jpeg` / `image/png` / `image/webp` / `application/octet-stream`

**响应字段** `AttachmentDTO`

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 附件 UUID（后续下载/删除用） |
| `recordId` | string | 关联记录 id，可为空字符串 |
| `iv` | string | base64 的 IV（回显） |
| `size` | int64 | 密文字节数 |
| `mime` | string | 客户端声明的类型 |
| `createdAt` | string(RFC3339) | 创建时间 |

**错误**

| HTTP | code | 场景 |
|---|---|---|
| 400 | `bad_request` | 缺少 `file` / `iv`、base64 非法、IV 非 12 字节、`recordId` 超长、mime 不在白名单 |
| 413 | `payload_too_large` | 超过 8 MiB |

```bash
curl -fsS -X POST https://med.example.com/api/v1/attachments \
  -H "Authorization: Bearer $ACCESS" \
  -F "file=@/tmp/photo.enc.bin" \
  -F "iv=<12 字节 IV 的 base64>" \
  -F "recordId=6f1c…" \
  -F "mime=image/jpeg"
```

```json
{"id":"0a3f…","recordId":"6f1c…","iv":"AbCd…","size":184320,"mime":"image/jpeg","createdAt":"2026-10-26T02:20:11Z"}
```

### 6.2 `GET /api/v1/attachments`

**鉴权**：✅ 是。**说明**：列出当前用户的附件。

**查询参数**

| 参数 | 说明 |
|---|---|
| `recordId` | 可选。传了则只返回关联该记录 id 的附件；**不传则返回全部** |

**响应**：`AttachmentDTO[]`

```bash
curl -fsS "https://med.example.com/api/v1/attachments?recordId=6f1c…" \
  -H "Authorization: Bearer $ACCESS"
```

### 6.3 `GET /api/v1/attachments/:id`

**鉴权**：✅ 是。**说明**：下载密文。

**响应头**

| 头 | 值 |
|---|---|
| `Content-Type` | `application/octet-stream` |
| **`X-MedDesk-IV`** | **base64 的 IV** —— 客户端据此 + DEK 解密 |
| `Content-Disposition` | `attachment; filename="<id>.bin"; filename*=UTF-8''<id>.bin` |
| `Cache-Control` | `no-store` |
| `X-Content-Type-Options` | `nosniff` |

**错误**：`404 not_found`（附件不存在，或**不属于当前用户**）

```bash
curl -fsS -D- -o photo.enc.bin \
  "https://med.example.com/api/v1/attachments/0a3f…" \
  -H "Authorization: Bearer $ACCESS"
# 从响应头拿到 X-MedDesk-IV
```

### 6.4 `DELETE /api/v1/attachments/:id`

**鉴权**：✅ 是。**说明**：删除附件元数据与密文文件。

**响应**：`204 No Content`（无响应体）

**错误**：`404 not_found`（不存在或不属于当前用户）

```bash
curl -fsS -o /dev/null -w '%{http_code}\n' -X DELETE \
  "https://med.example.com/api/v1/attachments/0a3f…" \
  -H "Authorization: Bearer $ACCESS"
```

---

## 7. 导出接口 `/export`

> ### 🔒 隐私提醒（请务必理解此节再使用）
>
> E2EE 下服务端**无法解密**，所以导出必须由**客户端把已解密的行数据一次性 POST 过来**。
> 这意味着一个 HTTP 请求的生命周期内，明文数据**短暂地存在于服务端内存中**。
> 服务端为此做了以下缓解：
>
> | 缓解措施 | 实现 |
> |---|---|
> | **不落盘** | 渲染进 `bytes.Buffer`，直接 `c.Data()` 输出，从不创建临时文件 |
> | **不写日志** | 导出全程不在任何 slog 日志里出现行内容 |
> | **不缓存** | 响应头 `Cache-Control: no-store` |
> | **大小封顶** | 请求体硬上限 **4 MiB**，超出即 `413` |
> | **流式下发** | Nginx 侧 `proxy_buffering off`，避免整包在代理缓冲里停留 |
> | **传输加密** | 强制 HTTPS（Nginx 把 HTTP 307 跳到 HTTPS，不存在明文 HTTP 的业务路径） |
>
> **结论**：风险窗口被压缩到「单次请求的内存+TLS 传输」，但**不等于零**。
> 极度敏感场景下请优先使用**纯前端 CSV 导出**（不经过服务端）。

### 7.1 `ExportRequest`（两接口共用请求体）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `format` | string | ❌ | `pdf` / `csv`；**路由已决定格式，服务端会覆盖此字段**，客户端可不传 |
| `title` | string | ❌ | 导出标题 |
| `profile` | string | ❌ | 成员名（用于 PDF 头部） |
| `rangeFrom` | string | ❌ | 起始日期 `YYYY-MM-DD` |
| `rangeTo` | string | ❌ | 结束日期 `YYYY-MM-DD` |
| `timezone` | string | ❌ | 客户端时区（如 `Asia/Shanghai`） |
| `rows` | `ExportRow[]` | ✅ | 明细行，**明文** |
| `stats` | `ExportStats` | ✅ | 汇总统计 |

**`ExportRow`**

| 字段 | 类型 | 示例 | 说明 |
|---|---|---|---|
| `date` | string | `2026-10-25` | 日期 |
| `time` | string | `08:00` | 时间 |
| `profile` | string | `本人` | 成员名 |
| `medication` | string | `维生素 D` | 药物名 |
| `dosage` | string | `1 粒` | 剂量文案 |
| `status` | string | `taken` | `taken`/`skipped`/`snoozed`/`pending`/`missed` |
| `note` | string | `` | 备注 |

**`ExportStats`**

| 字段 | 类型 | 说明 |
|---|---|---|
| `total` | int | 总次数 |
| `taken` | int | 已服用 |
| `skipped` | int | 已跳过 |
| `missed` | int | 已漏服 |
| `adherence` | float64 | 依从率，**0~1**（PDF 里按百分比展示） |

### 7.2 `POST /api/v1/export/csv`

**鉴权**：✅ 是。**说明**：生成 CSV（**UTF-8 BOM + CRLF**，Excel 双击直接正确打开中文）。

**响应**

| 属性 | 值 |
|---|---|
| `Content-Type` | `text/csv; charset=utf-8` |
| `Content-Disposition` | `attachment; filename="meddesk-20261026-0220.csv"; filename*=UTF-8''<百分号编码的中文名>` |
| `Cache-Control` | `no-store` |

```bash
curl -fsS -X POST https://med.example.com/api/v1/export/csv \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"title":"用药历史","profile":"本人","rangeFrom":"2026-10-01","rangeTo":"2026-10-26",
       "timezone":"Asia/Shanghai",
       "rows":[{"date":"2026-10-26","time":"08:00","profile":"本人","medication":"维生素 D","dosage":"1 粒","status":"taken","note":""}],
       "stats":{"total":1,"taken":1,"skipped":0,"missed":0,"adherence":1}}' \
  -o history.csv
```

### 7.3 `POST /api/v1/export/pdf`

**鉴权**：✅ 是。**说明**：生成 PDF。**需要中文字体**。

**PDF 版式**：A4 纵向；标题 + 汇总区（总次数/已服/跳过/漏服/依从率）+ 表格（日期/时间/成员/药物/剂量/状态/备注）+ 页脚（生成时间、页码）。

**状态中文映射**

| status | 显示 |
|---|---|
| `taken` | 已服用 |
| `skipped` | 已跳过 |
| `snoozed` | 已推迟 |
| `pending` | 待服用 |
| `missed` | 已漏服 |

**字体探测顺序**（`MEDDESK_PDF_FONT_PATH` 为空时）

1. `/home/ops/service/meddesk/fonts/NotoSansSC-Regular.ttf`
2. `/usr/share/fonts/truetype/noto/NotoSansSC-Regular.ttf`
3. `/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc`
4. `/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc`

**错误**：全部失败 → `503 unavailable`，message 为「请先安装中文字体或配置 MEDDESK_PDF_FONT_PATH」。

**响应**

| 属性 | 值 |
|---|---|
| `Content-Type` | `application/pdf` |
| `Content-Disposition` | `attachment; filename="meddesk-<时间戳>.pdf"; filename*=UTF-8''用药历史导出-<时间戳>.pdf` |
| `Cache-Control` | `no-store` |

```bash
curl -fsS -X POST https://med.example.com/api/v1/export/pdf \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"title":"用药历史","rows":[{"date":"2026-10-26","time":"08:00","profile":"本人","medication":"维生素 D","dosage":"1 粒","status":"taken","note":"中文测试"}],
       "stats":{"total":1,"taken":1,"skipped":0,"missed":0,"adherence":1}}' \
  -o history.pdf
```

> 字体自检：`bash deploy/scripts/get-font.sh --check`

---

## 8. 客户端调用流程

```
① 冷启动
   pull?cursor=<本地持久化值>&limit=300  → 解密写 IndexedDB → 更新 cursor
   （hasMore=true 时循环，直到 hasMore=false）

② 本地修改
   MD.data.*.save() → 置脏 → 攒批 → POST /sync/push

③ push 返回后有 conflicts
   解密 conflicts[i].ct → 按 LWW 规则与本地版本合并 → toast 告知用户
   → 合并结果作为新版本再次 push（ver 自增）

④ accessToken 过期（任意接口 401）
   POST /auth/refresh（用 refreshToken）
     ├ 成功 → 换新令牌 → 重试原请求
     └ 失败 → 清空本地 session → 弹出登录框

⑤ 导出
   本机解密 → 组装 ExportRequest → POST /export/pdf | /export/csv → Blob 下载
```

**后台自动同步建议节奏**：前台 `MD.sync.startAuto(30000)`（30s），`visibilitychange` 回到前台立即跑一次，`online` 事件触发一次。

---

## 9. 记账本接口 `/ledger`

> **适用环境**：开发 / 测试 / 生产均可。v0.3.50 已于 **2026-10-30 在生产环境（阿里云 ECS，`https://meddesk.qiubite.fun`，Nginx 反代至 `127.0.0.1:8089`）实测通过**，
> 本章 26 个接口全部逐个验证。
> **适用版本**：`v0.3.50`（`server.Version`，见 `/api/v1/health` 的 `version` 字段）。
> 本文示例中的 `$HOST` 取 `https://meddesk.qiubite.fun`（本地开发 `http://localhost:8089`），`$TOKEN` 为业务 access token。

### 9.0 记账本的通用约定

**（1）必须向用户明示的加密差异 —— 记账本是明文建表**

| | 健康数据（第 5~7 章） | 记账本（本章） |
|---|---|---|
| 存储形态 | **端到端加密**（E2EE）密文信封 | **明文**（服务端可读） |
| 服务端能否聚合统计 | 不能（解不开） | **能**：金额求和、分类占比、逐日趋势、每日自动备份都依赖此能力 |
| 凭证附件 | 客户端压缩加密后上传，服务端只有密文 | 明文图片（服务端按魔数校验真实类型） |
| 库表 | `sync_records` / `attachments` | `ledger_records` / `ledger_categories` / `ledger_attachments` / `ledger_backup_*` |

> 这是**用户明确确认过的取舍**（见 [`CONTRACTS-LEDGER.md`](./CONTRACTS-LEDGER.md) 第 0 节）：
> 账本放弃 E2EE 换取统计与自动备份能力；**健康数据的 E2EE 不受任何影响，两者互不影响**。
> 凭证是明文图片，因此服务端可读；产品侧应在记账本设置页明确告知用户。

**（2）鉴权与账号归属**

- 全部 26 个接口都需要 `Authorization: Bearer <accessToken>`（业务令牌，与第 4 章同一套）。
- 账号**一律取自会话**（中间件写入 gin 上下文 `user_id`），请求体/查询串里的 `user_id` **一律不采信**；导入时会强制改写成当前账号。
- 越权访问与他人资源，统一返回 **404**（不透露资源是否存在）。

**（3）金额口径（禁止浮点）**

- DB 列：`numeric(14,2)`。Go 侧全程 **字符串（精确值）** + **整数分（计算值）**，不出现任何浮点运算。
- 输入格式：`^\d{1,12}(\.\d{1,2})?$`，**必须 > 0**，不允许符号/指数/千分位。
- 输入归一化示例：`"12"` → `"12.00"` / `1200`；`"0.5"` → `"0.50"` / `50`；`"007.10"` → `"7.10"` / `710`。
- 对外统一返回 **`Money` 对象**（统计类字段）或 **`amount` + `amountCents`**（账单对象）：

```json
{"value":"123.45","cents":12345}
```

**（4）时区与自然日**：所有月份/自然日计算按 **`Asia/Shanghai`**（镜像无 `tzdata` 时退化为固定 `+08:00`，等价）。

**（5）默认分页**：`page` 默认 1；`size` 默认 20，上限 200（`size` ≤ 0 取默认，> 200 钳制到 200）。

**（6）本章错误码映射**（响应体形如 `{"code":"...","message":"..."}`）

| HTTP | code | 触发场景 |
|---|---|---|
| 400 | `bad_request` | 字段非法（kind/type/金额/时间/备注/分类名等，见各接口） |
| 401 | `unauthorized` | 未登录或令牌已过期（`{"code":"unauthorized","message":"未登录或令牌已过期"}`） |
| 404 | `not_found` | 账单/分类/凭证不存在，**或不属于当前账号**；导出无匹配数据时见 [9.6](#96-导出与导入-ledgerexport-ledgerimport) |
| 413 | `payload_too_large` | 凭证 > 2 MiB / 整个 multipart 请求体 > 2 MiB + 64 KiB；导入 JSON > 全局 8 MiB |
| 429 | `rate_limited` | `请求过于频繁，请稍后再试`。连发写请求触发令牌桶（默认 `MEDDESK_RATE_LIMIT_RPS=20` / `BURST=60`；**实测连发 30 次写请求即触发**，响应带 `Retry-After`） |
| 500 | `internal` | 未知异常，细节只写日志 |

**（7）共用数据结构**

`Category`

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | UUID |
| `kind` | string | `income` / `expense`（**字段名是 `kind`，不是 `type`**） |
| `name` | string | 分类名，≤ 32 字 |
| `sortOrder` | int | 排序权重 |
| `archived` | bool | 是否已停用 |
| `createdAt` / `updatedAt` | string(RFC3339) | 时间戳 |

`Record`

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | UUID |
| `type` | string | `income` / `expense` |
| `amount` | string | **精确金额字符串**，如 `"123.45"`，无浮点漂移 |
| `amountCents` | int64 | 整数分，用于计算与图表 |
| `currency` | string | 3 位代码，默认 `CNY` |
| `categoryId` | string | 分类 id（导入兜底场景可为空串） |
| `categoryName` | string | 分类名快照 |
| `transactionAt` | string(RFC3339) | 发生时刻 |
| `occurredOn` | string | 上海时区的自然日 `YYYY-MM-DD`，所有日/月统计以此为准 |
| `note` | string | 备注，≤ 500 字 |
| `reimbursementStatus` | string | `none` / `pending` / `done` |
| `attachmentCount` | int | 凭证数量（派生字段，不落库） |
| `createdAt` / `updatedAt` | string(RFC3339) | 时间戳 |

---

### 9.1 分类 `/ledger/categories`

> 首次访问任意分类接口时，服务端会**自动播种内置分类**（`repo.EnsureCategories`）；因此新账号也能立刻拿到一份默认分类列表。

#### 9.1.1 `GET /api/v1/ledger/categories`

**鉴权**：✅ Bearer。

**查询参数**

| 参数 | 必填 | 说明 |
|---|---|---|
| `kind` | ❌ | `income` / `expense`；不传返回全部。传其他值 → 400 |
| `archived` | ❌ | `1` 或 `true` 时**一并返回已停用的**分类；默认只返回未停用 |

**成功响应** `200`

```json
{"items":[
  {"id":"c1…","kind":"expense","name":"餐饮","sortOrder":10,"archived":false,
   "createdAt":"2026-10-30T01:00:00Z","updatedAt":"2026-10-30T01:00:00Z"}
],"total":1}
```

**错误**：`400 {"code":"bad_request","message":"kind 只能是 income 或 expense"}`、`401 unauthorized`、`500 internal`。

```bash
curl -fsS "$HOST/api/v1/ledger/categories?kind=expense&archived=1" -H "Authorization: Bearer $TOKEN"
```

#### 9.1.2 `POST /api/v1/ledger/categories`

**鉴权**：✅ Bearer。**说明**：新建分类。

**请求体**

```json
{"kind":"expense","name":"餐饮","sortOrder":10,"archived":false}
```

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `kind` | string | ✅ | `income` \| `expense` | **字段名是 `kind`**；写成 `type` 会被当作缺失 → 400 |
| `name` | string | ✅ | 非空，≤ 32 字 | 分类名 |
| `sortOrder` | int | ❌ | 默认 0 | 排序权重，小的在前 |
| `archived` | bool | ❌ | 默认 false | 停用开关 |

**成功响应** `201 Created`：完整 `Category` 对象。

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `kind 只能是 income 或 expense`（传 `type` 或未填时的实际返回） |
| 400 | `bad_request` | `分类名不能为空` / `分类名最多 32 个字` |
| 401 | `unauthorized` | `未登录或令牌已过期` |

```bash
curl -fsS -X POST "$HOST/api/v1/ledger/categories" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"expense","name":"餐饮","sortOrder":10}'
```

> ⚠ **踩坑记录（2026-10-30 部署）**：冒烟脚本把分类字段误写成 `type`，导致 `{"code":"bad_request","message":"kind 只能是 income 或 expense"}`
> 并让脚本把整批接口误报成 400。**创建分类用 `kind`，账单类型才用 `type`。**

#### 9.1.3 `PATCH /api/v1/ledger/categories/:id`

**鉴权**：✅ Bearer。**说明**：真正的部分更新——**只有请求体里出现的字段才会被改动**。**路径参数**：`id` = 分类 UUID。

**请求体**：同 9.1.2，所有字段均可选。

| 字段 | 类型 | 不传（缺省） | 传值 |
|---|---|---|---|
| `kind` | string | 不改动 | 覆盖（仅 `income` / `expense`） |
| `name` | string | 不改动 | 覆盖（≤ 32 字） |
| `sortOrder` | int | 不改动 | 覆盖（**`0` 也是有效值**） |
| `archived` | bool | 不改动 | 覆盖（**`false` 也是有效值**） |

> ✅ **语义说明（v0.3.51 起）**：`sortOrder` / `archived` 改为「有则改、无则留」判定，与 `kind` / `name` 一致。
> 只改名字时**不必**再带上这两个字段，也不会被静默清零；确实要归零或取消归档，显式传 `0` / `false` 即可生效。
>
> 历史提示：v0.3.50 及更早版本中这两个字段是「不传即写入零值」的，升级后调用方可以去掉多余的字段。

**成功响应** `200`：更新后的 `Category`。

**错误**

| HTTP | code | message | 说明 |
|---|---|---|---|
| 400 | `bad_request` | `kind 只能是 income 或 expense` | 非法 kind |
| 400 | `bad_request` | `分类名最多 32 个字` | 名称超长 |
| 404 | `not_found` | `分类不存在` | 不存在或不属于当前账号 |
| 409 | `conflict` | `同名分类已存在` | 同账号同 `kind` 下重名（v0.3.51 起正确返回 409，此前误报 500） |
| 401 | `unauthorized` | `未登录或令牌已过期` | |

```bash
# 只改名，sortOrder / archived 保持原值不动
curl -fsS -X PATCH "$HOST/api/v1/ledger/categories/c1…" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"工作餐"}'

# 显式停用（归档）
curl -fsS -X PATCH "$HOST/api/v1/ledger/categories/c1…" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"archived":true}'
```

#### 9.1.4 `DELETE /api/v1/ledger/categories/:id`

**鉴权**：✅ Bearer。**路径参数**：`id`。

**成功响应** `200`

```json
{"ok": true}
```

**错误**

| HTTP | code | message | 说明 |
|---|---|---|---|
| 404 | `not_found` | `删除分类失败；若该分类已被账单使用，请改用「停用」` | **已被账单引用的分类不可删除**（SQL 层 `RowsAffected=0`），请改用 `PATCH` + `archived:true` |
| 404 | `not_found` | `分类不存在` | 不存在或不属于当前账号 |
| 401 | `unauthorized` | `未登录或令牌已过期` | |

---

### 9.2 账单 `/ledger/records`

#### 9.2.1 `GET /api/v1/ledger/records`

**鉴权**：✅ Bearer。**说明**：分页查询账单，排序为 **`transactionAt DESC, id DESC`**（新账在前）。

**说明补充**：分类列表的排序为 **kind → sortOrder → name**（`sortOrder` 小的在前）。

**查询参数**

| 参数 | 必填 | 说明 |
|---|---|---|
| `type` | ❌ | `income` / `expense` |
| `categoryId` / `categoryIds` | ❌ | 分类多选，**逗号分隔**；两个参数名等价、可同时用，值会合并去重 |
| `keyword` | ❌ | 关键词，匹配**备注与分类名** |
| `reimbStatus` | ❌ | `none` / `pending` / `done` |
| `from` / `to` | ❌ | 自然日 `YYYY-MM-DD`（含端点）；**本接口没有默认区间，不传即全时段** |
| `ids` | ❌ | 指定 id，逗号分隔（批量操作/批量导出用） |
| `page` | ❌ | 默认 1 |
| `size` | ❌ | 默认 20，上限 200 |

**成功响应** `200`

```json
{"items":[
  {"id":"r1…","type":"expense","amount":"123.45","amountCents":12345,"currency":"CNY",
   "categoryId":"c1…","categoryName":"餐饮","transactionAt":"2026-10-30T12:30:00+08:00",
   "occurredOn":"2026-10-30","note":"团队聚餐","reimbursementStatus":"pending",
   "attachmentCount":2,"createdAt":"2026-10-30T04:35:00Z","updatedAt":"2026-10-30T04:35:00Z"}
],"total":128,"page":1,"size":20}
```

**错误**：`401 unauthorized`；`500 internal`（读取失败）。

```bash
curl -fsS "$HOST/api/v1/ledger/records?type=expense&from=2026-10-01&to=2026-10-31&page=1&size=20" \
  -H "Authorization: Bearer $TOKEN"
```

#### 9.2.2 `POST /api/v1/ledger/records`

**鉴权**：✅ Bearer。

**请求体**

```json
{"type":"expense","amount":"123.45","currency":"CNY","categoryId":"c1…",
 "transactionAt":"2026-10-30T12:30","note":"团队聚餐","reimbursementStatus":"pending"}
```

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `type` | string | ✅ | `income` \| `expense` | 账单类型 |
| `amount` | string | ✅ | `^\d{1,12}(\.\d{1,2})?$` 且 > 0 | **字符串**，`"123.45"` / `"12"` / `"0.5"` 均可，服务端归一化 |
| `currency` | string | ❌ | 3 位代码 | 默认 `CNY`，大写归一化 |
| `categoryId` | string | 二选一 | 必须是当前账号名下、且 `kind` 与 `type` 一致的分类 | 传了 id 时，分类名以服务端当前值为准（快照） |
| `categoryName` | string | 二选一 | ≤ 32 字 | 仅用于**无 id 场景**（主要为导入兜底）；`categoryId` 与 `categoryName` 都为空 → 400 |
| `transactionAt` | string | ❌ | RFC3339 / `YYYY-MM-DDTHH:mm` / `YYYY-MM-DD` | 留空取当前时刻 |
| `note` | string | ❌ | ≤ 500 字 | 备注 |
| `reimbursementStatus` | string | ❌ | `none` \| `pending` \| `done` | 默认 `none`；**`income` 恒被强制归一为 `none`**（不报错） |

**成功响应** `201 Created`：完整 `Record` 对象。

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `type 只能是 income 或 expense` |
| 400 | `bad_request` | `金额必须是大于 0 的数字，最多两位小数` |
| 400 | `bad_request` | `币种需为 3 位代码（默认 CNY）` |
| 400 | `bad_request` | `分类与账单类型不匹配`（类别 kind 与账单 type 不一致） |
| 400 | `bad_request` | `请选择分类` / `分类名最多 32 个字` |
| 400 | `bad_request` | `发生时间格式有误（建议 YYYY-MM-DDTHH:mm）` |
| 400 | `bad_request` | `备注最多 500 个字` |
| 400 | `bad_request` | `报销状态只能是 none / pending / done` |
| 404 | `not_found` | `分类不存在`（id 无效或不属于当前账号） |
| 401 | `unauthorized` | `未登录或令牌已过期` |

#### 9.2.3 `GET /api/v1/ledger/records/:id`

**鉴权**：✅ Bearer。**路径参数**：`id`。

**成功响应** `200`：单个 `Record`。

**错误**：`404 {"code":"not_found","message":"账单不存在"}`（不存在或不属于当前账号）、`401`。

#### 9.2.4 `PATCH /api/v1/ledger/records/:id`

**鉴权**：✅ Bearer。**路径参数**：`id`。**说明**：**整体覆盖更新**（未提供的字段会被覆盖成默认值），前端请提交完整对象。

**请求体 / 响应 / 错误**：与 [9.2.2](#922-post-apiv1ledgerrecords) 完全一致，仅状态码为 `200`。

#### 9.2.5 `DELETE /api/v1/ledger/records/:id`

**鉴权**：✅ Bearer。**说明**：删除账单；**会先删除其名下全部凭证文件**，再删记录（顺序不可反，否则磁盘文件变孤儿）。

**成功响应** `200`

```json
{"ok": true}
```

**错误**：`404 {"code":"not_found","message":"账单不存在"}`、`401`。

> 凭证清理失败**不阻断**删除：元数据删除是用户明确要求的动作，残留文件只记为 `WARN` 日志。

---

### 9.3 统计 `summary` / `trend` / `breakdown`

三个接口的筛选参数（除区间外）与 [9.2.1](#921-get-apiv1ledgerrecords) 一致：`type`、`categoryId(s)`、`keyword`、`reimbStatus`、`ids`。

**区间解析规则（重要）**

| 条件 | 生效区间 |
|---|---|
| 同时传 `from` 与 `to` | 以它们为准 |
| 只传 `month=YYYY-MM` | 该自然月的 [首日, 末日] |
| 都没传 | **本月**（上海时区） |
| `/trend` | **例外**：始终为「最近 N 个自然日（含今天）」，`from`/`to`/`month` 对窗口无效 |

#### 9.3.1 `GET /api/v1/ledger/summary`

**鉴权**：✅ Bearer。**说明**：区间收支汇总，`balance` = `income - expense`。

**查询参数**：`from` / `to` / `month` + 上述筛选项。

**成功响应** `200`

```json
{"from":"2026-10-01","to":"2026-10-31",
 "income":{"value":"12000.00","cents":1200000},
 "expense":{"value":"4231.55","cents":423155},
 "balance":{"value":"7768.45","cents":776845}}
```

**错误**：`401`、`500 internal`（`统计收支失败`）。

```bash
curl -fsS "$HOST/api/v1/ledger/summary?month=2026-10" -H "Authorization: Bearer $TOKEN"
```

#### 9.3.2 `GET /api/v1/ledger/trend`

**鉴权**：✅ Bearer。**说明**：逐日收支趋势，**没有数据的日子补 0**（SQL `generate_series` 保证，前端不必自行补齐）。

**查询参数**

| 参数 | 必填 | 说明 |
|---|---|---|
| `days` | ❌ | 自然日天数，默认 **7**（含今天）；≤ 0 亦按 7 处理 |
| 其他筛选项 | ❌ | `type` / `categoryId(s)` / `keyword` / `reimbStatus` / `ids` 生效；`from`/`to`/`month` **不影响窗口** |

**成功响应** `200`

```json
{"from":"2026-10-24","to":"2026-10-30","days":[
  {"date":"2026-10-24","income":{"value":"0.00","cents":0},"expense":{"value":"88.00","cents":8800}},
  {"date":"2026-10-25","income":{"value":"0.00","cents":0},"expense":{"value":"0.00","cents":0}}
]}
```

> 返回值 **`{from, to, days:[…]}`**；生产实测为默认 7 天且补 0。

**错误**：`401`、`500 internal`（`统计趋势失败`）。

#### 9.3.3 `GET /api/v1/ledger/breakdown`

**鉴权**：✅ Bearer。**说明**：按分类聚合金额，**收入与支出分组返回，禁止混算**。

**查询参数**：同 [9.3.1](#931-get-apiv1ledgersummary)（默认本月）。

**成功响应** `200`

```json
{"from":"2026-10-01","to":"2026-10-31",
 "income":[{"categoryId":"c9…","name":"工资","money":{"value":"12000.00","cents":1200000}}],
 "expense":[{"categoryId":"c1…","name":"餐饮","money":{"value":"1234.55","cents":123455}}]}
```

---

### 9.4 报销 `/ledger/reimbursements`

#### 9.4.1 `GET /api/v1/ledger/reimbursements`

**鉴权**：✅ Bearer。**说明**：按「报销状态 + 自然日区间」**分页**返回**支出**账单，并附带该区间内的汇总数字与条数。

- 只统计/返回支出：收入不进入报销流程（服务端对收入强制 `reimbursementStatus=none`），因此 `type` 恒为 `expense`，**无需也不能**传 `type` 参数。
- 旧行为（固定只看待报销、固定 200 条、无时间条件）已废弃：用户把记录标成「已报销」后不会再"凭空消失"，历史也能查。

**查询参数**

| 参数 | 类型 | 默认 | 取值范围 | 非法输入的行为 |
|---|---|---|---|---|
| `status` | string | `pending` | `pending` / `done` / `none` / `all` | 其他值 → `400 bad_request`「status 只能是 pending / done / none / all」；空串等同 `pending` |
| `from` | string | 不限 | `YYYY-MM-DD`，**含端点** | 格式不符 → `400`「日期格式应为 YYYY-MM-DD」；`2026-02-30` 这类不存在的日期 → `400`「日期不是有效日期」 |
| `to` | string | 不限 | `YYYY-MM-DD`，**含端点** | 同上；`from` 晚于 `to` → `400`「开始日期不能晚于结束日期」 |
| `days` | int | `30` | 正整数 | 非数字 / `≤0` → **静默回落到 30**（不报错）；**仅当 `from`、`to` 都缺省时生效** |
| `page` | int | `1` | `≥1` | 非数字 / `≤0` → 夹取为 `1` |
| `size` | int | `50` | `1–200` | 非数字 / `≤0` → `50`；`>200` → **夹取为 200**（不报错） |

**区间规则（按 `Asia/Shanghai` 自然日计算）**

1. `from` / `to` 只要传了**任意一个**就以它们为准，另一端留空表示该侧**不设边界**，此时 `days` 被忽略；
2. 两者都缺省时按 `days` 反推：`from = 今天 - (days-1)`、`to = 今天`（`days=30` 即"近 30 天含今天"）；
3. 自然日一律按 **`Asia/Shanghai`** 计算——账本的"今天"必须与用户看到的一致，不能用 UTC。

**成功响应** `200`

```json
{
  "items": [
    {"id":"r1…","type":"expense","amount":"123.45","amountCents":12345,"currency":"CNY",
     "categoryId":"c1…","categoryName":"餐饮","transactionAt":"2026-10-30T12:30:00+08:00",
     "occurredOn":"2026-10-30","note":"午餐","reimbursementStatus":"pending",
     "attachmentCount":1,"createdAt":"2026-10-30T04:35:00+08:00","updatedAt":"2026-10-30T04:35:00+08:00"}
  ],
  "total": 12, "page": 1, "size": 50,
  "pendingTotal": {"value":"123.45","cents":12345},
  "summary": {
    "pendingTotal": {"value":"123.45","cents":12345},
    "doneTotal": {"value":"88.00","cents":8800},
    "counts": {"pending":3,"done":9,"none":0}
  },
  "range": {"from":"2026-10-01","to":"2026-10-30"}
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | object[] | 当前页账单，**复用账单记录对象**（字段定义见 [9.2](#92-账单-ledgerrecords)）；空结果返回 `[]` 而非 `null` |
| `total` | int64 | 满足 `status` + 区间条件的**总条数**（分页前的全量） |
| `page` / `size` | int | **回显服务端实际生效的值**（已被夹取/回落），前端翻页请以此为准 |
| `pendingTotal` | Money | **兼容字段，不可删**。语义为"当前筛选条件下 `status=pending` 的合计"，与 `summary.pendingTotal` **恒等**；保留它是为了旧版前端仍能渲染"待报销合计"，旧客户端全部退场后才能移除 |
| `summary.pendingTotal` | Money | 区间内**待报销**金额合计 |
| `summary.doneTotal` | Money | 区间内**已报销**金额合计 |
| `summary.counts` | object | 区间内三种状态的**条数**：`pending` / `done` / `none`（与 `status` 参数无关，始终给全三种） |
| `range` | object | **回显服务端实际采用的区间** `{"from","to"}`，前端据此显示"近 30 天 / 10-01 至 10-30"等文案；某一侧不设边界时对应值为 `""` |

> **金额口径**：`pendingTotal` / `summary.*` 走 SQL 聚合求**全量**，不是累加当前页——分页后首页金额与 `total` 口径仍然一致。
> **`items[]` 的金额是平铺的 `amount`（字符串，两位小数）+ `amountCents`（整数分）**，与 9.2 的账单对象一致；
> 请勿按嵌套的 `amount.value` 解析（那是早期草案的写法，服务端未采用）。

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `status 只能是 pending / done / none / all` |
| 400 | `bad_request` | `日期格式应为 YYYY-MM-DD` / `日期不是有效日期` / `开始日期不能晚于结束日期` |
| 401 | `unauthorized` | `未登录或令牌已过期` |
| 500 | `internal` | `读取报销列表失败` / `统计待报销金额失败` |

```bash
# 查「已报销」+ 最近 30 天（days 生效的前提是不传 from/to）
curl -fsS "$HOST/api/v1/ledger/reimbursements?status=done&days=30&page=1&size=50" \
  -H "Authorization: Bearer $TOKEN"
```

#### 9.4.2 `POST /api/v1/ledger/reimbursements/mark`

**鉴权**：✅ Bearer。**说明**：批量改报销状态，**只处理当前账号名下、且确实是支出的记录**。接口行为本次**未变更**，新增的只是「退回」这一用法的说明。

**请求体**

```json
{"ids":["r1…","r2…"],"status":"done"}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ids` | string[] | ✅ | 账单 id 列表，至少 1 个非空的元素（空串会被剔除，全空即 400） |
| `status` | string | ✅ | `none` / `pending` / `done`；其他值 → `400`。前后空格会被 `TrimSpace` |

**三种状态的语义**

| `status` | 含义 |
|---|---|
| `pending` | **待报销**。把已报销记录改回 `pending` 就是「**退回待报销**」，可直接作用于 `done` 记录 |
| `done` | 已报销 |
| `none` | 不参与报销（从流程中摘出） |

> 三种状态之间**任意互相转换**（服务端无条件改写，不校验来源状态），因此「退回」不需要先改成 `none` 中转。

> **退回（撤销报销）**：`status:"pending"` 不是"新建待报销"，而是把目标记录的状态**无条件改写**为 `pending`——
> 因此对已标记 `done` 的记录再发一次 `{"status":"pending"}` 即可退回，无需先改成 `none`。
> 退回后该记录会重新出现在默认（`status=pending`）的列表与 `summary.pendingTotal` 中；`doneTotal` 相应减少，服务端不做二次确认。

**成功响应** `200`

```json
{"updated": 2}
```

（`updated` 为**实际更新条数**；不属于本账号、不是支出、或 id 不存在的记录**不计数**，也不报错——批量操作里逐条报错反而会打断整批。）

**幂等性**：同一 `ids` + 同一 `status` 重复提交，`updated` 仍是这些记录的实际匹配条数（服务端按状态改写，不做"值未变则跳过"的过滤）。

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `请先选择要操作的账单`（ids 为空或全是空串） |
| 400 | `bad_request` | `报销状态只能是 none / pending / done`（含 `status` 为空串） |
| 400 | `bad_request` | `请求参数有误`（body 不是合法 JSON 或类型不符） |
| 401 | `unauthorized` | `未登录或令牌已过期` |
| 500 | `internal` | `更新报销状态失败` |

```bash
# 标记已报销
curl -fsS -X POST "$HOST/api/v1/ledger/reimbursements/mark" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ids":["r1…"],"status":"done"}'

# 退回待报销（status=pending 即"撤销报销"，可直接作用于已报销记录）
curl -fsS -X POST "$HOST/api/v1/ledger/reimbursements/mark" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ids":["r1…","r2…"],"status":"pending"}'
```

---

### 9.5 凭证附件 `/ledger/attachments`

> **适用环境**：开发 / 测试 / 生产（需配置了 `ledger_attachments` 仓储，路由才会挂载）。**适用版本 v0.3.50**，2026-10-30 生产实测通过。
>
> 与第 6 章的健康附件**完全不同**：这里是**明文图片**（服务端可读），因此服务端才能把它们内嵌进 HTML/ZIP 导出并随每日备份一起打包。
> 落盘路径 `{UploadDir}/ledger/{userID}/{uuid}.{ext}`，磁盘权限 `0700`/`0600`；**文件名一律由服务端生成**，用户输入不参与路径拼装。

**通用限制**：仅 **JPG / PNG / WebP**（按**文件魔数**嗅探，不看、也不信客户端声明的 `Content-Type`）；单文件 ≤ **2 MiB**（2097152 字节）；
整个 multipart 请求体 ≤ **2 MiB + 64 KiB**（2162688 字节，多出的 64 KiB 留给 boundary 与表单字段）。

`Attachment` 对象

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | UUID（后续预览/删除用） |
| `recordId` | string | 所属账单 id |
| `mime` | string | `image/jpeg` / `image/png` / `image/webp`（服务端嗅探结果） |
| `bytes` | int64 | 文件字节数 |
| `width` / `height` | int | 解码得到的尺寸，导出排版用；解码失败为 0，不影响业务 |
| `createdAt` | string(RFC3339) | 创建时间 |

> `userId`（归属）与 `relPath`（磁盘相对路径）**刻意不对外输出**。

#### 9.5.1 `POST /api/v1/ledger/attachments`

**鉴权**：✅ Bearer。**Content-Type**：`multipart/form-data`。

**表单字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `file` | file | ✅ | 图片二进制，≤ 2 MiB，必须是 JPG/PNG/WebP |
| `recordId` | text | ✅ | 所属账单 id（**必须属于当前账号**，否则 404，且不产生任何临时文件） |

**成功响应** `201 Created`

```json
{"id":"a1…","recordId":"r1…","mime":"image/jpeg","bytes":184320,"width":1280,"height":960,
 "createdAt":"2026-10-30T04:40:00Z"}
```

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `缺少 file 字段` / `缺少 recordId` / `文件内容为空` |
| 400 | `bad_request` | `只支持 JPG / PNG / WebP 图片`（**魔数校验不通过**；生产实测：伪造/改名的图片会被拒） |
| 400 | `bad_request` | `解析上传表单失败`（multipart 格式损坏） |
| 404 | `not_found` | `账单不存在`（recordId 无效或不属于当前账号；归属校验在**落盘之前**，不产生任何临时文件） |
| 500 | `internal` | `打开上传内容失败`；`recordId` 不是合法 UUID 时也落到这一类（message 仍是 `账单不存在`，但状态码是 **500**，客户端请按 5xx 处理） |
| 413 | `payload_too_large` | `单张凭证不能超过 2097152 字节` / `上传内容超过上限 2097152 字节` |

```bash
curl -fsS -X POST "$HOST/api/v1/ledger/attachments" \
  -H "Authorization: Bearer $TOKEN" \
  -F "file=@/tmp/receipt.jpg" -F "recordId=r1…"
```

#### 9.5.2 `GET /api/v1/ledger/attachments`

**鉴权**：✅ Bearer。**说明**：列出凭证。

**查询参数**

| 参数 | 必填 | 说明 |
|---|---|---|
| `recordId` | ✅ | **本接口必须传**；不传返回 `400 缺少 recordId`（与第 6 章 `/attachments` 的可选语义不同） |

**成功响应** `200`（按 `createdAt, id` 升序，与上传顺序一致）

```json
{"items":[{"id":"a1…","recordId":"r1…","mime":"image/jpeg","bytes":184320,"width":1280,"height":960,
           "createdAt":"2026-10-30T04:40:00Z"}],"total":1}
```

**错误**

| HTTP | code | 场景 |
|---|---|---|
| 400 | `bad_request` | `缺少 recordId` |
| 401 | `unauthorized` | 未登录或令牌已过期 |
| 500 | `internal` | `读取凭证列表失败`——含 `recordId` **不是合法 UUID** 的情况（不是 404，调用方别按 404 兜底） |

> `recordId` 是合法 UUID 但不属于当前账号时，返回 200 + **空列表**（SQL 同时按 `user_id` 过滤），不做存在性判断。

#### 9.5.3 `GET /api/v1/ledger/attachments/:id`

**鉴权**：✅ Bearer。**路径参数**：`id`。**说明**：预览/下载凭证原图。

**响应头**

| 头 | 值 |
|---|---|
| `Content-Type` | 服务端嗅探出的真实 mime（`image/jpeg` / `image/png` / `image/webp`） |
| `Cache-Control` | `private, no-store`（财务隐私，禁止任何中间层与浏览器缓存） |
| `X-Content-Type-Options` | `nosniff` |
| `Content-Disposition` | `inline; filename="<id>.<ext>"` |

**响应体**：图片二进制流（`Content-Length` 为 `bytes`）。

**错误**：`404 {"code":"not_found","message":"凭证不存在"}`（不存在或不属于当前账号）、`401`。

```bash
curl -fsS -o receipt.jpg "$HOST/api/v1/ledger/attachments/a1…" -H "Authorization: Bearer $TOKEN"
```

#### 9.5.4 `DELETE /api/v1/ledger/attachments/:id`

**鉴权**：✅ Bearer。**路径参数**：`id`。**说明**：先删元数据（成功才拿到路径），再删磁盘文件。

**成功响应** `200`

```json
{"ok": true}
```

**错误**：`404 {"code":"not_found","message":"凭证不存在"}`、`401`。

---

### 9.6 导出与导入 `/ledger/export`, `/ledger/import`

> **适用环境**：开发 / 测试 / 生产。**适用版本 v0.3.50**，2026-10-30 生产实测：**有数据时返回 200 + 文件，无匹配账单返回 400**。
>
> 三个导出接口共用同一份筛选条件（`ExportFilter`）；HTML / ZIP **总是内嵌附件字节**，JSON 可用 `includeAttachments` 控制。
> ZIP 内的 xlsx 由标准库 `archive/zip` + `encoding/xml` 生成（**未引入 excelize**），Excel / WPS / Numbers 均可打开。

#### 9.6.1 `ExportFilter`（筛选条件）

**POST（`/export/html`、`/export/zip`）走 JSON 请求体，`GET /export/json` 走查询串。**

| 字段 / 查询参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `preset` | string | （空 = `all`） | `thisMonth` / `lastMonth` / `custom` / `all`（`month` 是 `thisMonth` 的别名）；GET 侧还支持 `month=YYYY-MM`（转成自定义区间） |
| `from` | string | 无限 | `YYYY-MM-DD`（含）；非法值按「不限」处理，不报错 |
| `to` | string | 无限 | 同上 |
| `type` | string | 全部 | `income` / `expense` |
| `categoryIds` | string[]（GET：逗号分隔） | 空 | 分类多选；GET 侧**同时支持 `categoryId` 与 `categoryIds`** |
| `keyword` | string | 空 | 备注 / 分类名关键词 |
| `reimbOnly` | bool | false | **仅待报销**：等价于 `type=expense` + `reimbursementStatus=pending` |
| `ids` | string[]（GET：逗号分隔） | 空 | 只导出勾选的记录，上限 **1000** 条 |
| `includeAttachments` | bool | **true** | **仅对 JSON 备份生效**；`false` 时只登记附件清单、不内嵌 base64。HTML/ZIP 忽略此值 |

> `includeAttachments` 在 POST 侧用指针区分「未传」与「显式 false」——**未传时按内嵌处理**（备份完整性优先）。
> GET 侧取值 `0` / `false` / `no`（不区分大小写）视为 false，其他视为 true。

**时间范围：`preset` / `from` / `to` 的精确规则**

| 参数 | 类型 | 默认值 | 取值范围 | 非法输入的行为 |
|---|---|---|---|---|
| `preset` | string | **`all`**（不传、传空串均视为 `all`） | `thisMonth` / `lastMonth` / `custom` / `all`（`month` = `thisMonth` 的别名） | 其他值 → `400`「未知的导出范围 preset，可用值：thisMonth / lastMonth / custom / all」 |
| `from` | string | 不限 | `YYYY-MM-DD`，含端点 | **格式非法 → 静默按「不限」处理，不报错**（导出是"尽量给结果"的操作）；仅当 `preset=custom` 时才严格校验并报 `400 时间范围格式有误，应为 YYYY-MM-DD` |
| `to` | string | 不限 | `YYYY-MM-DD`，含端点 | 同上；`from > to` 时 `all` 模式下不拦截，`custom` 模式下 `400 开始日期不能晚于结束日期` |
| `month`（仅 GET） | string | — | `YYYY-MM` | 非法 → 回落为**当前月**（不报错） |

各样式的实际区间（按 `Asia/Shanghai` 自然日）：

| `preset` | 区间 |
|---|---|
| `all`（默认） | 不限；**若同时给了 `from`/`to`，仍会按它们收窄**（`all` 不等于"忽略日期"） |
| `thisMonth` / `month` | 本自然月首日 ~ 末日 |
| `lastMonth` | 上自然月首日 ~ 末日 |
| `custom` | **必须同时给 `from` 与 `to`**，缺任一 → `400`「自定义时间范围需要同时提供 from 与 to」 |
| 未传 `preset` 但 `from`、`to` 都给了 | **自动按 `custom` 处理**（无需显式写 `preset`） |
| 未传 `preset` 且只给了一端 | 按 `all` + 该端为边界（"某日之后/之前"） |

```bash
# 导出自定义区间（POST /export/html、/export/zip 都用这套 body；GET /export/json 用查询串）
curl -fsS -X POST "$HOST/api/v1/ledger/export/zip" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"preset":"custom","from":"2026-10-01","to":"2026-10-31","type":"expense"}' \
  -o ledger-2026-10.zip
```

#### 9.6.2 `POST /api/v1/ledger/export/html`

**鉴权**：✅ Bearer。**说明**：生成**自包含 HTML 报告**（图片以 `data:` URI 内嵌，可离线打开）。

**请求体**

```json
{"preset":"thisMonth","type":"expense","categoryIds":["c1…"],"includeAttachments":true}
```

**成功响应** `200`

| 属性 | 值 |
|---|---|
| `Content-Type` | `text/html; charset=utf-8` |
| `Content-Disposition` | `attachment; filename="ledger-export-20261030.html"`（日期取导出时刻的上海时区） |
| `Content-Length` | 报告字节数 |

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `没有符合条件的账单，未生成导出文件`（**有意为之**：空文件容易被误判为「导出成功但数据丢了」） |
| 400 | `bad_request` | `未知的导出范围 preset，可用值：thisMonth / lastMonth / custom / all` |
| 400 | `bad_request` | `自定义时间范围需要同时提供 from 与 to` / `时间范围格式有误，应为 YYYY-MM-DD` / `开始日期不能晚于结束日期` |
| 400 | `bad_request` | `type 只能是 income 或 expense` / `一次最多导出 1000 条勾选记录` |
| 401 | `unauthorized` | `未登录或令牌已过期` |

```bash
curl -fsS -X POST "$HOST/api/v1/ledger/export/html" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"preset":"thisMonth"}' -o ledger.html
```

#### 9.6.3 `POST /api/v1/ledger/export/zip`

**鉴权**：✅ Bearer。**说明**：生成导出压缩包。

**ZIP 内容**

```
ledger-export-20261030.zip
├── ledger.xlsx        账单明细（金额写的是精确 decimal 文本）
├── images/<recordId>/<attachmentId>.<ext>
└── manifest.json      {schema, version, generatedAt, recordCount, attachmentCount, attachmentsIncluded, files[], checksum}
```

`manifest.json` 里每个文件都有 sha256，并对「排序后的 `sha256  路径` 行」再算一次整体 `checksum`——改动任一字节都能被发现。

**请求体 / 错误**：与 [9.6.2](#962-post-apiv1ledgerexporthtml) 完全相同。

**成功响应** `200`：`Content-Type: application/zip`，`Content-Disposition: attachment; filename="ledger-export-20261030.zip"`。

```bash
curl -fsS -X POST "$HOST/api/v1/ledger/export/zip" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"preset":"lastMonth","type":"expense"}' -o ledger.zip
```

#### 9.6.4 `GET /api/v1/ledger/export/json`

**鉴权**：✅ Bearer。**说明**：生成 JSON 备份。用途有二：① 用户自行存档；② 与 [9.6.5](#965-post-apiv1ledgerimport) 的导入形成闭环。

**查询参数**：见 [9.6.1](#961-exportfilter筛选条件)（本接口**不带请求体**，走查询串）。

**成功响应** `200`：`Content-Type: application/json; charset=utf-8`，`Content-Disposition: attachment; filename="ledger-backup-20261030.json"`。

响应体为 `BackupPayload`：

```json
{
  "schema": "meddesk-ledger-export",
  "version": 1,
  "exported_at": "2026-10-30T04:45:00+08:00",
  "categories": [{"id":"c1…","kind":"expense","name":"餐饮","sortOrder":10,"archived":false}],
  "records": [{"id":"r1…","type":"expense","amount":"123.45","amountCents":12345,"currency":"CNY",
               "categoryId":"c1…","categoryName":"餐饮","transactionAt":"2026-10-30T12:30:00+08:00",
               "occurredOn":"2026-10-30","note":"团队聚餐","reimbursementStatus":"pending",
               "attachmentCount":1,"createdAt":"2026-10-30T04:35:00+08:00","updatedAt":"2026-10-30T04:35:00+08:00"}],
  "attachments": [{"id":"a1…","recordId":"r1…","mime":"image/jpeg","ext":"jpg","bytes":184320,
                   "data":"<base64 图片字节>","included":true}],
  "attachmentsIncluded": true,
  "attachmentsNote": "…"
}
```

| 字段 | 说明 |
|---|---|
| `schema` | 导出为 `meddesk-ledger-export`；每日备份为 `meddesk-ledger-backup`（**两种都能被导入接受**） |
| `version` | 当前只支持 `1` |
| `categories` / `records` / `attachments` | 恒为非 null 数组（空数组而非 `null`） |
| `attachments[].data` | base64 图片字节；`included=false` 时为空，`reason` 给出原因 |
| `attachmentsIncluded` | 明确标记本次是否真的带了附件字节，防止恢复时误判「已完整恢复」 |

**错误**：与 [9.6.2](#962-post-apiv1ledgerexporthtml) 相同。

> 下行没有额外的大小限制，但 `includeAttachments=true` 时图片以 base64 内嵌，体积会膨胀约 4/3；
> 只想快速备份账单时请用 `includeAttachments=0`。

```bash
curl -fsS "$HOST/api/v1/ledger/export/json?preset=thisMonth&includeAttachments=0" \
  -H "Authorization: Bearer $TOKEN" -o ledger-backup.json
```

#### 9.6.5 `POST /api/v1/ledger/import`

**鉴权**：✅ Bearer。**说明**：从备份恢复。**同一个路径按 `Content-Type` 分流**：

| `Content-Type` | 入口 | 说明 |
|---|---|---|
| `application/json` | JSON 导入 | 原有行为，支持**合并（merge）** 与 **覆盖（overwrite）**；body 需带 `mode`/`payload`/`confirm`。**本次未变更** |
| `application/zip` | ZIP 备份包导入 | **本次新增**。直接把 **WebDAV 每日备份包**（[9.7](#97-webdav-每日备份-ledgerbackup)）当请求体发上来，服务端解析包内的 `ledger.json` 并还原 `images/` 下的附件 |

> **如何判定走 ZIP**：`Content-Type` 为 `application/zip`（或 `application/x-zip-compressed`），
> **或** body 的前 4 个字节是 ZIP 魔数 `PK\x03\x04`——即使客户端错标成 `application/octet-stream` 也能被识别。
> 两条都不满足才走 JSON 解析。
>
> **上限的两个层次别混淆**：`200 MiB` 是**解压后**的内容上限（防 zip bomb），
> 而**上传的 ZIP 本体仍受全局 8 MiB 限制**——含大量图片的备份包压缩后可能超过 8 MiB，
> 此时会得到 `413`。这类备份请改用 `GET /ledger/export/json?includeAttachments=0` 走 JSON 导入，或先从 WebDAV 分卷取出再导入。

> **安全底线（务必阅读）**
> 1. payload 里的任何归属字段**都不采信**，每条记录强制改写为当前账号。
> 2. **覆盖是破坏性操作**：必须先清空当前账本，要求 `confirm=true` 才执行；失败会整体回滚，不会留下「删了一半」的账本。
> 3. **JSON 导入不恢复附件字节**（备份里的 `data` 只是登记），会在结果里明确报告跳过数量；**ZIP 导入会真的把附件落盘还原**。
> 4. 结构性错误一律在写库之前返回 400 —— **一行都不写**。

##### A. JSON 导入（`Content-Type: application/json`，行为不变）

**请求体**

```json
{"mode":"merge","confirm":false,"payload":{"schema":"meddesk-ledger-export","version":1,"categories":[…],"records":[…]}}
```

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `mode` | string | ✅ | — | `merge`（按 id 去重，跳过已存在）/ `overwrite`（清空后重建）；其他值 → 400 |
| `payload` | object | ✅ | — | `BackupPayload`，即 [9.6.4](#964-get-apiv1ledgerexportjson) 的输出结构（`schema` 为 `meddesk-ledger-export` 或 `meddesk-ledger-backup` 均可） |
| `confirm` | bool | 覆盖时必填 | `false` | `mode=overwrite` 时必须为 `true`，否则 400 |

##### B. ZIP 备份包导入（`Content-Type: application/zip`，本次新增）

**请求体**：ZIP 二进制本身（**不是** JSON 里再套一层），用 `--data-binary` 或 `-T` 发送。

**包内结构**（与 [9.7](#97-webdav-每日备份-ledgerbackup) 每日备份一致）

```
ledger-backup-20261030.zip
├── ledger.json          账单 + 分类 + 附件清单（BackupPayload，schema = meddesk-ledger-backup）
├── images/<recordId>/<attachmentId>.<ext>
└── manifest.json        {schema, version, generatedAt, recordCount, attachmentCount, checksum}
```

| 项 | 规则 |
|---|---|
| `ledger.json` | **必需**。位于包根目录（`./ledger.json`），缺失 → `400`「备份包里没有 ledger.json」 |
| `images/…` | 可选。按 `images/<recordId>/<attachmentId>.<ext>` 还原为明文图片附件；不属于本次导入记录的图片会被跳过并记入 `warnings` |
| `manifest.json` | 可选。存在时用于核对 `checksum`；**不一致不会中断导入**，只在 `warnings` 里提示（备份包可能被人工改过） |
| 导入模式 | ZIP 入口**无 body 字段可传 `mode`**：缺省为 `merge`（"直接导入"最安全的那条路）；需要覆盖时用查询串 `?mode=overwrite&confirm=true` 显式声明，未带 `confirm=true` 的 `overwrite` 一律 400 |

**限制与越界行为**（整包级越界即 400 不写库；单条目越界只跳过该条目并记 `warnings`）

| 限制 | 阈值 | 越界行为 |
|---|---|---|
| **上传的 ZIP 本体（压缩后）** | ≤ 全局 **8 MiB**（[1.5](#15-请求体大小上限)），且受 Nginx `client_max_body_size 12m` 约束 | `413 payload_too_large`（Nginx 侧直接返回 413 HTML） |
| 解压后总字节 | ≤ **200 MiB** | `400`「解压后内容超过 200 MiB 上限」 |
| ZIP 条目数 | ≤ **5000** | `400`「备份包条目数超过 5000 上限」 |
| 单个附件 | ≤ **2 MiB**（与 [9.5](#95-凭证附件-ledgerattachments) 一致） | 该附件被跳过并记入 `warnings`，不影响账单导入 |
| 路径穿越（zip slip） | 条目名含 `..` 段或为绝对路径（`/…`、`C:\…`） | 该条目**被拒绝**并记入 `warnings`；包内其余内容照常处理 |
| 账单条数 | ≤ **20000**（与 JSON 导入同一上限） | `400` |

> **明文说明（重要）**：WebDAV 备份的内容是**明文**——`ledger.json` 是普通 JSON，`images/` 是明文图片，`manifest.json` 是普通 JSON，
> 包本身**没有加密、没有二次编码**，就是普通的 ZIP。
> 因此**服务异常时可以直接从 WebDAV 下载备份包，用一条 `curl` 导入**，无需任何解码或额外工具：
>
> ```bash
> curl -fsS -X POST "$HOST/api/v1/ledger/import" \
>   -H "Authorization: Bearer $TOKEN" \
>   -H 'Content-Type: application/zip' \
>   --data-binary @ledger-backup-20261030.zip
> ```
>
> 这也意味着备份包**等同明文账本**：WebDAV 账号的权限、以及备份目录的访问控制就是唯一防线，请勿把备份包发到公开渠道。

**成功响应 `200`（两个入口共用同一结构，ZIP 入口相关字段才有非零值）**

```json
{"mode":"merge","recordsBefore":10,"recordsAfter":13,"recordsInserted":3,"recordsSkipped":2,
 "recordsFailed":0,"categoriesCreated":1,"attachmentsTotal":4,"attachmentsSkipped":0,
 "imported":3,"skipped":2,"attachmentsRestored":4,
 "warnings":["条目 \"../etc/passwd\" 含路径穿越，已忽略","附件 a2… 超过 2 MiB 上限，已跳过"],
 "note":"已从 ZIP 备份包恢复账单与 4 个附件",
 "errors":["第 2 条记录的金额 \"abc\" 非法：必须是大于 0 的数字，最多两位小数"]}
```

| 字段 | 出现在 | 说明 |
|---|---|---|
| `mode` | 两者 | 实际生效的导入模式 `merge` / `overwrite` |
| `recordsBefore` / `recordsAfter` | 两者 | 导入前后的账单总数 |
| `recordsInserted` / `recordsSkipped` / `recordsFailed` | 两者 | 新增 / 已存在跳过 / 失败条数（原有字段） |
| `imported` / `skipped` | 两者 | **`recordsInserted` / `recordsSkipped` 的等价别名**，供备份包客户端与脚本直接读取；两套字段同时返回 |
| `categoriesCreated` | 两者 | 新建分类数 |
| `attachmentsTotal` / `attachmentsSkipped` | 两者 | 备份里登记的附件总数 / 未恢复数 |
| `attachmentsRestored` | 两者 | **真正落盘并登记成功的附件数**；JSON 导入恒为 `0`，ZIP 导入通常 `= attachmentsTotal - attachmentsSkipped` |
| `warnings` | 两者 | 不影响结果但需知会的提示：被拒的 zip slip 条目、超限附件、`manifest.checksum` 不匹配等；无内容时**该字段不出现** |
| `note` | 两者 | 结果摘要（有附件时的固定提示） |
| `errors` | 两者 | 逐条失败原因（**整体仍 200**，失败条目不写入） |

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `请指定导入模式：merge（合并）或 overwrite（覆盖）` / `导入模式只能是 merge 或 overwrite` |
| 400 | `bad_request` | `缺少 payload（备份内容）` |
| 400 | `bad_request` | `覆盖导入会清空当前账本数据，请确认后重试（confirm=true）` |
| 400 | `bad_request` | `备份文件缺少 schema 标识，无法确认格式` / `不支持的备份格式 %q，当前仅支持 meddesk-ledger-export` |
| 400 | `bad_request` | `备份版本 %d 不受支持，当前支持版本 1` |
| 400 | `bad_request` | `备份里没有账单记录，已取消导入` / `备份含 %d 条账单，超过单次导入上限 20000` |
| 400 | `bad_request` | `第 %d 个分类的 kind 非法（应为 income 或 expense）` / `第 %d 条记录的金额 %q 非法：…` / `第 %d 条记录的金额与分值不一致（…）` 等逐条校验失败文案 |
| 400 | `bad_request` | ZIP 专用：`备份包里没有 ledger.json` / `备份包不是合法的 ZIP 文件` / `解压后内容超过 200 MiB 上限` / `备份包条目数超过 5000 上限` |
| 401 | `unauthorized` | `未登录或令牌已过期` |
| 413 | `payload_too_large` | 请求体超过全局 8 MiB（JSON 备份建议改用 `includeAttachments=0`；ZIP 备份包体积通常远小于含附件字节的 JSON 备份，图片很多时仍可能顶到上限） |

```bash
# ① JSON 导入（原有用法）
curl -fsS -X POST "$HOST/api/v1/ledger/import" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"mode\":\"merge\",\"payload\":$(cat ledger-backup.json)}"

# ② ZIP 备份包直接导入（WebDAV 每日备份包，明文无需解码）
curl -fsS -X POST "$HOST/api/v1/ledger/import" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/zip' \
  --data-binary @ledger-backup-20261030.zip
```

> **注意**：`POST /ledger/export/zip` 的产物是**报表包**（内含 `ledger.xlsx`，**没有** `ledger.json`），
> 不可直接回灌到本接口；能直接导入的是 **WebDAV 每日备份包**。要把导出包变成可导入格式，请用 `GET /ledger/export/json`。

---

### 9.7 WebDAV 每日备份 `/ledger/backup`

> **适用环境**：开发 / 测试 / 生产。**适用版本 v0.3.50**。
> 生产实测调度已排程：`{"tz":"Asia/Shanghai","time":"02:00"}`。
>
> **为什么凭据要落盘**：备份是**服务端定时执行**（默认每天 02:00），那一刻没有用户请求、没有会话，凭据只能从数据库取——
> 这是用户明确批准的取舍。降低风险的办法：AES-256-GCM 加密，密钥来自环境变量
> **`MEDDESK_SECRETBOX_KEY=<base64 32 字节密钥>`**；每个字段独立 12 字节 nonce；AAD 绑定 `user_id` + 字段名；
> **任何接口都不回传密码**（密文明文只出现在调用栈里）。

**相关环境变量**

| 变量 | 默认 | 说明 |
|---|---|---|
| `MEDDESK_SECRETBOX_KEY` | 无 | **必填**（否则保存凭据返回 400，且 `/status` 的 `enabled=false`）；base64 的 32 字节密钥 |
| `MEDDESK_LEDGER_BACKUP_ENABLED` | `1` | 自动备份总开关（改成 `0` 且不重启进程时仍会按启动时的值生效） |
| `MEDDESK_LEDGER_BACKUP_TIME` | `02:00` | 每日触发时刻 `HH:MM` |
| `MEDDESK_LEDGER_BACKUP_TZ` | `Asia/Shanghai` | 时区名 |

**已知限制**：调度器是**进程内 ticker**，多副本部署时各实例都会触发一次。内容相同、路径相同 → 最终同名覆盖，
不会产生重复文件，但会浪费带宽；源码未做跨实例选主，不能声称已解决（详见 `internal/ledger/scheduler.go` 文件头）。

#### 9.7.1 `PUT /api/v1/ledger/backup/credentials`

**鉴权**：✅ Bearer。**说明**：保存/更新 WebDAV 凭据。**先做连通性校验（30s 超时），连不上就不写库**。

**请求体**

```json
{"baseURL":"https://dav.example.com/dav/","username":"user@example.com",
 "password":"<占位符口令>","pathPrefix":"meddesk-backup"}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `baseURL` | string | ✅ | WebDAV 服务地址 |
| `username` | string | ✅ | WebDAV 用户名（会 trim） |
| `password` | string | ✅ | WebDAV 口令（**不 trim**，也不回传） |
| `pathPrefix` | string | ❌ | 备份子目录，允许 `a/b`；禁止 `..`、反斜杠、绝对路径、空字节 |

**成功响应** `200`

```json
{"ok": true, "configured": true}
```

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `请填写 WebDAV 服务地址` / `请填写 WebDAV 用户名` / `请填写 WebDAV 密码` |
| 400 | `bad_request` | `备份目录不得包含空字节` / `备份目录不得包含反斜杠` / `备份目录不得包含上级目录引用` |
| 400 | `bad_request` | `服务端未配置 MEDDESK_SECRETBOX_KEY，备份功能不可用（需 base64 编码的 32 字节密钥）` / `MEDDESK_SECRETBOX_KEY 无效：必须是 base64 编码的 32 字节密钥` |
| 401 / 403 / 5xx | 相应 code | 连通性校验失败时**原样透出** WebDAV 侧的可读文案（例如 `unauthorized`） |
| 401 | `unauthorized` | `未登录或令牌已过期` |

```bash
curl -fsS -X PUT "$HOST/api/v1/ledger/backup/credentials" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"baseURL":"https://dav.example.com/dav/","username":"u@example.com","password":"<占位符口令>","pathPrefix":"meddesk-backup"}'
```

#### 9.7.2 `GET /api/v1/ledger/backup/status`

**鉴权**：✅ Bearer。**说明**：查询备份状态与最近运行记录（**不含任何凭据内容**）。无查询参数。

**成功响应** `200`

```json
{"enabled":true,"configured":true,"timezone":"Asia/Shanghai","scheduleTime":"02:00",
 "nextRunAt":"2026-10-31T02:00:00+08:00",
 "runs":[{"date":"2026-10-30","status":"success","remotePath":"meddesk-backup/ledger-backup-2026-10-30.zip",
          "sizeBytes":1048576,"recordCount":128,"message":"","updatedAt":"2026-10-30T02:00:12Z"}]}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `enabled` | bool | 开关开启 **且** `MEDDESK_SECRETBOX_KEY` 可用时才为 true |
| `configured` | bool | 当前账号是否已保存凭据 |
| `timezone` / `scheduleTime` | string | 生效的配置（生产实测 `Asia/Shanghai` / `02:00`） |
| `nextRunAt` | string | RFC3339；`enabled=false` 时为空串 |
| `runs[]` | array | 最近 **10** 条运行记录，按时间倒序 |
| `runs[].status` | string | `running` / `success` / `failed` |
| `runs[].remotePath` / `sizeBytes` / `recordCount` / `message` | — | 远端路径、包大小、账单条数、失败原因 |

#### 9.7.3 `POST /api/v1/ledger/backup/run`

**鉴权**：✅ Bearer。**说明**：手动触发一次全量备份（**只能备份自己的数据**，无管理员特权通道）。无请求体。

**执行流程**：写 `running` 记录 → 打包 → 上传 → 远端校验 → 写最终结果。幂等：文件名只由当天日期决定，同日重复执行是同名覆盖 + 记录 upsert。

**成功响应** `200`（一个 `BackupRunView`）

```json
{"date":"2026-10-30","status":"success","remotePath":"meddesk-backup/ledger-backup-2026-10-30.zip",
 "sizeBytes":1048576,"recordCount":128,"message":"","updatedAt":"2026-10-30T02:00:12Z"}
```

> **成败语义**：成功才返回 200 + `status:"success"`。中间任一步失败（凭据缺失、打包失败、上传失败、
> 上传后远端回查不到文件）都会把失败结果写进 `ledger_backup_runs` **并返回对应的错误状态码** —— 例如凭据未配置是 404、
> 远端查不到文件时是 `503 unavailable`（`上传后未能在远端找到备份文件`）。因此：**不能只看 HTTP 状态码判断是否已备份，
> 请以 `/backup/status` 的 `runs[].status` 为准**。

**错误**：`404 {"code":"not_found","message":"尚未配置 WebDAV 凭据"}`、`401`。

#### 9.7.4 `DELETE /api/v1/ledger/backup/credentials`

**鉴权**：✅ Bearer。**说明**：删除已保存的凭据，**幂等**（本来就未配置也返回成功）。无请求体。

**成功响应** `200`

```json
{"ok": true, "configured": false}
```

**错误**：`401 unauthorized`。

---

### 9.8 记账本错误文案速查

| HTTP | code | message | 触发接口 |
|---|---|---|---|
| 400 | `bad_request` | `kind 只能是 income 或 expense` | 分类增改查（**把 `kind` 写成 `type` 时就是这个**） |
| 400 | `bad_request` | `分类名不能为空` / `分类名最多 32 个字` | 分类增改 |
| 400 | `bad_request` | `删除分类失败；若该分类已被账单使用，请改用「停用」` | 删除分类 |
| 400 | `bad_request` | `type 只能是 income 或 expense` | 账单增改、导出筛选 |
| 400 | `bad_request` | `金额必须是大于 0 的数字，最多两位小数` | 账单增改、导入 |
| 400 | `bad_request` | `币种需为 3 位代码（默认 CNY）` | 账单增改 |
| 400 | `bad_request` | `分类与账单类型不匹配` / `请选择分类` | 账单增改 |
| 400 | `bad_request` | `发生时间格式有误（建议 YYYY-MM-DDTHH:mm）` | 账单增改 |
| 400 | `bad_request` | `备注最多 500 个字` | 账单增改 |
| 400 | `bad_request` | `报销状态只能是 none / pending / done` | 账单增改、批量标记 |
| 400 | `bad_request` | `请先选择要操作的账单` | 批量标记报销 |
| 400 | `bad_request` | `只支持 JPG / PNG / WebP 图片` | 凭证上传（魔数校验） |
| 400 | `bad_request` | `缺少 file 字段` / `缺少 recordId` | 凭证上传 / 凭证列表 |
| 400 | `bad_request` | `没有符合条件的账单，未生成导出文件` | 三个导出接口 |
| 400 | `bad_request` | `未知的导出范围 preset，可用值：thisMonth / lastMonth / custom / all` | 三个导出接口 |
| 400 | `bad_request` | `覆盖导入会清空当前账本数据，请确认后重试（confirm=true）` | 导入 |
| 400 | `bad_request` | `不支持的备份格式 %q，当前仅支持 meddesk-ledger-export` | 导入 |
| 401 | `unauthorized` | `未登录或令牌已过期` | 全部 26 个接口 |
| 404 | `not_found` | `账单不存在` / `分类不存在` / `凭证不存在` | 详情/修改/删除（越权同此） |
| 404 | `not_found` | `尚未配置 WebDAV 凭据` | 手动备份 |
| 413 | `payload_too_large` | `单张凭证不能超过 2097152 字节` | 凭证上传 |
| 429 | `rate_limited` | `请求过于频繁，请稍后再试` | 全部 `/api/v1/*`（含记账本，连发 30 次写请求即触发） |
| 503 | `unavailable` | `上传后未能在远端找到备份文件` | 手动备份（上传成功但远端回查失败） |

---

## 10. 管理后台接口 `/admin`

> **适用环境**：开发 / 测试 / 生产（**生产仅在配置了后台白名单邮箱时挂载**）。**适用版本 v0.3.50**。
> 页面入口：`https://meddesk.qiubite.fun/admin`（见 [10.9](#109-后台页面入口)）。

### 10.0 前置条件与身份模型

| 项 | 说明 |
|---|---|
| **挂载条件** | `MEDDESK_ADMIN_EMAILS` **非空**且仓储实现了 Admin* 方法；否则后台路由**根本不存在**，调用返回 `404 {"code":"not_found","message":"接口不存在"}` |
| 管理员身份 | **白名单邮箱 + 该账号自身的口令**（复用 `users` 表凭据，后台不另存密码）；或可选的一组**独立后台凭据**（`MEDDESK_ADMIN_SALT` + `MEDDESK_ADMIN_PASSWORD_HASH`） |
| 令牌差异 | 后台 token 由同一 `MEDDESK_JWT_SECRET` 签发，但 `scope=admin`、`iss=meddesk-admin`、默认有效期 `MEDDESK_ADMIN_TTL`（**8 小时**）。**业务令牌即使泄露也调不动后台接口** |
| 数据隔离 | 后台 token 与业务 token 在 `Authorization` 头里位置相同，但**两者互不可用**；每次请求还会回数据库核对账号是否仍存在、邮箱是否变更 |

**通用错误**

| HTTP | code | message | 场景 |
|---|---|---|---|
| 401 | `unauthorized` | `请先登录后台` | 未带 `Bearer` |
| 401 | `unauthorized` | `后台会话已失效，请重新登录` | 签名/过期失败，或账号已被删/邮箱已改 |
| 403 | `forbidden` | `无权访问后台` | 令牌缺 `scope=admin`，或邮箱已不在白名单 |
| 429 | `rate_limited` | `尝试过于频繁，请 1 分钟后再试` | 同一 IP + 邮箱连续失败 5 次 → 锁 60 秒 |

### 10.1 `GET /api/v1/admin/salt`

**鉴权**：否。**说明**：返回**独立后台凭据**的 KDF 盐；前端据此派生 PBKDF2 后提交登录。

> 只有同时配置了 `MEDDESK_ADMIN_SALT` 与 `MEDDESK_ADMIN_PASSWORD_HASH` 才返回 200；
> 未配置时返回 **404**，前端据此改走「账号口令」登录路径。

**成功响应** `200`

```json
{"kdfSalt":"<16 字节 salt 的 base64>"}
```

**错误**：`404 {"code":"not_found","message":"未配置管理员独立凭据"}`。

```bash
curl -fsS "$HOST/api/v1/admin/salt"
```

### 10.2 `POST /api/v1/admin/login`

**鉴权**：否（这是后台唯一的入口）。**说明**：校验白名单邮箱 + 口令派生值，签发后台令牌。

**请求字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `email` | string | ✅ | `required,email,max=160` | 必须在白名单内 |
| `passwordHash` | string | ✅ | `required,min=32,max=128` | 客户端 PBKDF2 派生值的 base64 |

> 校验通道有两条，任一通过即可：① **账号自身的口令**（与业务登录同一套 argon2id）；② **独立后台凭据**。
> 邮箱不在白名单时按凭据错误处理，不暴露「你不是管理员」这一信息。

**成功响应** `200`

```json
{"token":"eyJhbGciOiJIUzI1NiIs…","email":"admin@example.com","expiresIn":28800}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `token` | string | 后台令牌，后续请求 `Authorization: Bearer <token>` |
| `email` | string | 管理员邮箱 |
| `expiresIn` | int64 | 有效秒数（`MEDDESK_ADMIN_TTL`，默认 28800） |

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `参数不合法` |
| 401 | `unauthorized` | `邮箱或口令不正确`（白名单外、邮箱不存在、口令不符统一文案） |
| 429 | `rate_limited` | `尝试过于频繁，请 1 分钟后再试` |

```bash
curl -fsS -X POST "$HOST/api/v1/admin/login" \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","passwordHash":"<PBKDF2 派生值的 base64>"}'
```

### 10.3 `GET /api/v1/admin/overview`

**鉴权**：✅ 后台令牌。**说明**：后台首屏统计。无查询参数。

**成功响应** `200`

```json
{"users":128,"records":34567,"activeGrants":7}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `users` | int64 | 用户总数 |
| `records` | int64 | 服务端密文记录总数（`sync_records`） |
| `activeGrants` | int64 | 生效中的家庭授权数 |

```bash
curl -fsS "$HOST/api/v1/admin/overview" -H "Authorization: Bearer $ADMIN_TOKEN"
```

### 10.4 `GET /api/v1/admin/users`

**鉴权**：✅ 后台令牌。**说明**：用户列表，**分页 + 邮箱模糊搜索**。

**查询参数**

| 参数 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `q` | ❌ | 空 | 邮箱模糊匹配（**大小写不敏感**）；空表示不过滤 |
| `page` | ❌ | 1 | 页码（非法值退回 1） |
| `size` | ❌ | 20 | 每页条数，**> 200 钳制到 200**（非法值退回 20） |

**成功响应** `200`

```json
{"items":[
  {"id":"6f1c…","email":"me@example.com","createdAt":"2026-10-26T02:15:00Z","updatedAt":"2026-10-30T01:00:00Z",
   "recordCount":340,"lastSeq":1024,"recoveryEnabled":false,"envelopeStale":true}
],"total":128,"page":1,"size":20}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` / `email` | string | 账号标识与邮箱 |
| `createdAt` / `updatedAt` | string(RFC3339) | 时间戳 |
| `recordCount` | int64 | 该账号的**密文记录数**（仅计数，服务端读不到明文） |
| `lastSeq` | int64 | 同步游标，可粗略反映活跃度 |
| `recoveryEnabled` | bool | 是否开启「托管恢复」；**为 true 时管理员可无损改口令**（`mode=escrow`） |
| `envelopeStale` | bool | 口令已被改过、信封待持有 DEK 的设备自愈 |

```bash
curl -fsS "$HOST/api/v1/admin/users?q=example&page=1&size=20" -H "Authorization: Bearer $ADMIN_TOKEN"
```

### 10.5 `POST /api/v1/admin/users`

**鉴权**：✅ 后台令牌。**说明**：管理员代为创建账号；服务端生成密钥材料，用户用给定口令即可登录。

**请求字段**

| 字段 | 类型 | 必填 | 约束 | 说明 |
|---|---|---|---|---|
| `email` | string | ✅ | 合法邮箱，≤160 | 统一小写存储 |
| `password` | string | ✅ | `min=8,max=128` | **明文口令**（此处是后台代建的特例；服务端立刻 argon2id 派生，不存明文） |

**成功响应** `200`

```json
{"id":"6f1c…","email":"new@example.com"}
```

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `邮箱或口令不合法（口令至少 8 位）` / `邮箱格式不正确` |
| 409 | `conflict` | 邮箱已注册（仓储层上抛） |

```bash
curl -fsS -X POST "$HOST/api/v1/admin/users" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"email":"new@example.com","password":"<至少 8 位的占位口令>"}'
```

### 10.6 `PATCH /api/v1/admin/users/:id`

**鉴权**：✅ 后台令牌。**说明**：编辑账号基本信息 —— **目前只有邮箱**（登录标识）。

> 口令不在这里改：改口令涉及 DEK 重新包裹，必须走 [10.7](#107-post-apiv1adminusersidpassword) 那条带数据处置语义的路径。

**路径参数**：`id` = 用户 UUID。

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `email` | string | ✅ | 新邮箱，≤160，须合法 |

**成功响应** `200`

```json
{"id":"6f1c…","email":"renamed@example.com"}
```

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `参数不合法` / `邮箱格式不正确` |
| 400 | `bad_request` | `不能修改自己的登录邮箱`（自我保护，防止管理员把自己锁在门外） |

### 10.7 `POST /api/v1/admin/users/:id/password`

**鉴权**：✅ 后台令牌。**路径参数**：`id`。**说明**：重置指定用户口令，由 `mode` 决定「已有数据怎么办」。

> 🔐 **E2EE 硬约束**：新口令派生新 KEK，旧 `wrapped_dek` 解不开 → 服务端上的旧密文对新会话**永久不可读**。
> 因此重置口令必须显式处置数据，不能假装什么都没发生。

**请求字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `password` | string | ✅ | 新口令，`min=8,max=128` |
| `mode` | string | ❌ | `keep`（默认）/ `escrow` / `wipe`；留空时看 `wipeData` |
| `wipeData` | bool | ❌ | 兼容旧前端：`true` 且未传 `mode` 时按 `wipe` 处理 |

**三种模式**

| mode | 动作 | 何时使用 | 是否需要 `recoveryEnabled` |
|---|---|---|---|
| `keep`（**默认**） | 只换 `password_hash` 与 `kdf_salt`，**信封与全部密文原样保留**，标记「待自愈」；撤销全部刷新令牌 | 目标设备仍持有 DEK，登录后走 `/auth/repair-envelope` 自愈 | 否 |
| `escrow` | 服务端用托管恢复私钥解出 DEK，用新口令重新包裹 —— **数据一条不动**，任何设备立刻可用新口令 | 账号已开启托管恢复，要求零损亏 | **是** |
| `wipe` | 换一把**新 DEK** 并清空该账号全部密文与托管信封；撤销全部会话 | 数据本身就该作废 / 已泄露时才用 | 否 |

**成功响应** `200`

```json
{"id":"6f1c…","mode":"keep","wiped":false,"recovered":"device"}
```

| 字段 | 说明 |
|---|---|
| `mode` | 生效的模式（回显） |
| `wiped` | 是否清空了密文 |
| `recovered` | `none`（未恢复）/ `device`（待设备自愈）/ `escrow`（服务端已用托管信封恢复） |

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `不能在此重置自己的口令；请在应用内修改（会保留数据）` |
| 400 | `bad_request` | `新口令至少 8 位` |
| 400 | `bad_request` | `mode 只能是 keep / escrow / wipe` |
| 400 | `bad_request` | `本部署未启用托管恢复，请改用「直接改口令（保数据）」`（`escrow` 且未配 `MEDDESK_RECOVERY_KEY`） |
| 400 | `bad_request` | `该账号未开启托管恢复，无法完全恢复；请改用「直接改口令（保数据）」` |
| 500 | `internal` | `托管信封无法解密（恢复密钥可能已更换），请改用「直接改口令（保数据）」` |

```bash
curl -fsS -X POST "$HOST/api/v1/admin/users/6f1c…/password" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"password":"<至少 8 位的新占位口令>","mode":"keep"}'
```

> 全部 HTTPS 失败路径都会把 `AdminUserEmail` 的结果用于自我保护判断，**不会因查邮箱失败而放行**。

### 10.8 `DELETE /api/v1/admin/users/:id`

**鉴权**：✅ 后台令牌。**路径参数**：`id`。**说明**：删除账号，级联清空会话、同步密文、授权与附件元数据，并尽力删除磁盘附件文件。

**成功响应** `200`

```json
{"id":"6f1c…","filesRemoved":7}
```

| 字段 | 说明 |
|---|---|
| `id` | 已删除的账号 id |
| `filesRemoved` | 成功删除的附件文件数（**尽力而为**：文件不存在视为成功；路径越权会被 `joinUploadPath` 拦下并跳过） |

**错误**

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `不能删除当前登录的管理员账号` |
| 404 | `not_found` | 账号不存在（仓储层上抛） |

### 10.9 后台页面入口

| 路径 | 说明 |
|---|---|
| `GET /admin` 或 `GET /admin/` | 后台 HTML 页面（`admin.html`），`Content-Type: text/html; charset=utf-8` |
| `GET /admin/app.js` | 后台脚本（`admin-app.js`），`Content-Type: application/javascript; charset=utf-8` |

两者均响应 **no-store** 组合头，杜绝中间缓存：

```http
Cache-Control: no-store, no-cache, must-revalidate, max-age=0
Pragma: no-cache
Expires: 0
```

> **破缓存约定（本次部署引入）**：页面引用脚本时必须带版本号 `?v=N`（`N` 随每次发版递增）。
> 反代/网关/CDN 会按 URI 缓存响应，只有 `no-store` 一道保险时，仍可能出现「新 HTML + 旧 JS」被长期命中；
> URL 变化才能强制回源。发布后台脚本改动时请**同步递增 `v`**。
>
> 文件缺失返回 `404` 纯文本 `页面不存在`；读取失败返回 `500` 纯文本 `读取页面失败`（均由 `serveFile` 处理）。

### 10.10 后台错误文案速查

| HTTP | code | message |
|---|---|---|
| 400 | `bad_request` | `参数不合法` / `邮箱格式不正确` / `邮箱或口令不合法（口令至少 8 位）` |
| 400 | `bad_request` | `不能修改自己的登录邮箱` / `不能删除当前登录的管理员账号` / `不能在此重置自己的口令；请在应用内修改（会保留数据）` |
| 400 | `bad_request` | `新口令至少 8 位` / `mode 只能是 keep / escrow / wipe` |
| 400 | `bad_request` | `本部署未启用托管恢复，请改用「直接改口令（保数据）」` / `该账号未开启托管恢复，无法完全恢复；…` |
| 401 | `unauthorized` | `请先登录后台` / `后台会话已失效，请重新登录` / `邮箱或口令不正确` |
| 403 | `forbidden` | `无权访问后台` |
| 404 | `not_found` | `未配置管理员独立凭据`（`/salt`） |
| 429 | `rate_limited` | `尝试过于频繁，请 1 分钟后再试`（`/login`） |

---

## 11. 路由总览与文档覆盖范围

按 `internal/server/router.go` 挂载顺序列出全部对外路由组与完成情况：

| # | 路由组 | 处理器位置 | 本文状态 |
|---|---|---|---|
| 1 | `/api/v1/health`、`/api/v1/ready` | `internal/server/router.go` | ✅ [第 3 章](#3-健康检查) |
| 2 | `/api/v1/auth/*` | `internal/auth/handler.go` | ✅ [第 4 章](#4-认证接口-auth)（12 个全覆盖） |
| 3 | `/api/v1/sync/*` | `internal/syncsvc` | ✅ [第 5 章](#5-同步接口-sync) |
| 4 | `/api/v1/attachments/*` | `internal/attachments` | ✅ [第 6 章](#6-附件接口-attachments) |
| 5 | `/api/v1/export/*` | `internal/export` | ✅ [第 7 章](#7-导出接口-export) |
| 6 | `/api/v1/ledger/*` | `internal/ledger/http.go`（14） | ✅ [9.1~9.4](#9-记账本接口-ledger) |
| 7 | `/api/v1/ledger/attachments/*` | `internal/ledger/attachment_http.go`（4） | ✅ [9.5](#95-凭证附件-ledgerattachments) |
| 8 | `/api/v1/ledger/export/*`、`/ledger/import` | `internal/ledger/export_http.go`（4） | ✅ [9.6](#96-导出与导入-ledgerexport-ledgerimport) |
| 9 | `/api/v1/ledger/backup/*` | `internal/ledger/backup_http.go`（4） | ✅ [9.7](#97-webdav-每日备份-ledgerbackup) |
| 10 | `/api/v1/family/*` | `internal/family/family.go`（7） | ⚠ **本文未展开**：`POST /grants`、`POST /grants/code`、`POST /grants/redeem`、`GET /grants`、`DELETE /grants/:id`、`GET /records`、`POST /records`；字段以 [`CONTRACTS-FAMILY-LINK.md`](./CONTRACTS-FAMILY-LINK.md) 为准 |
| 11 | `/api/v1/webdav/*` | `internal/webdav/webdav.go`（6） | ⚠ **本文未展开**：`POST /test`、`/list`、`/mkdir`、`/delete`、`/upload`、`/download`；凭据随请求传入、服务端不持久化，见 [`SECURITY.md`](./SECURITY.md) |
| 12 | `/api/v1/admin/*` | `internal/admin/admin.go`（7） | ✅ [第 10 章](#10-管理后台接口-admin) |
| 13 | `/admin`、`/admin/app.js` | `serveFile` | ✅ [10.9](#109-后台页面入口) |
| 14 | 前端 SPA 与 NoRoute 回退 | `mountSPA` | 不适用（非 API） |

> **记账本接口总数：26**（14 + 4 + 4 + 4），全部在 [第 9 章](#9-记账本接口-ledger) 展开。

---

## 附录 A：完整 curl 冒烟脚本

```bash
#!/usr/bin/env bash
# MedDesk 端到端冒烟脚本（把 HOST 换成你的域名）
set -Eeuo pipefail
HOST="https://med.example.com"
EMAIL="smoke@example.com"
PH="<客户端 PBKDF2 派生值的 base64>"     # ≥32 字符
SALT="<16 字节 salt 的 base64>"
WRAP="<被 KEK 包裹的 DEK base64>"
IV="<12 字节 IV base64>"

echo "== 1. 健康检查 =="
curl -fsS "$HOST/api/v1/health"; echo
curl -fsS "$HOST/api/v1/ready";  echo

echo "== 2. 注册 / 登录 =="
TOKENS=$(curl -fsS -X POST "$HOST/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"passwordHash\":\"$PH\"}")
if [ -z "$TOKENS" ]; then
  TOKENS=$(curl -fsS -X POST "$HOST/api/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"passwordHash\":\"$PH\",\"kdfSalt\":\"$SALT\",\"wrappedDek\":\"$WRAP\",\"dekIv\":\"$IV\"}")
fi
ACCESS=$(printf '%s' "$TOKENS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["accessToken"])')
REFRESH=$(printf '%s' "$TOKENS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["refreshToken"])')
AUTH="Authorization: Bearer $ACCESS"

echo "== 3. me =="
curl -fsS "$HOST/api/v1/auth/me" -H "$AUTH"; echo

echo "== 4. sync stats / pull / push =="
curl -fsS "$HOST/api/v1/sync/stats" -H "$AUTH"; echo
curl -fsS "$HOST/api/v1/sync/pull?cursor=0&limit=10" -H "$AUTH"; echo
curl -fsS -X POST "$HOST/api/v1/sync/push" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"changes":[]}'; echo

echo "== 5. 刷新令牌（轮换） =="
curl -fsS -X POST "$HOST/api/v1/auth/refresh" -H 'Content-Type: application/json' \
  -d "{\"refreshToken\":\"$REFRESH\"}" >/dev/null && echo "刷新 OK"

echo "== 6. 导出 CSV（应拿到 200 + attachment 头） =="
curl -fsS -D- -o /tmp/meddesk-smoke.csv -X POST "$HOST/api/v1/export/csv" \
  -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"title":"冒烟","rows":[],"stats":{"total":0,"taken":0,"skipped":0,"missed":0,"adherence":0}}' \
  | grep -iE 'HTTP/|content-disposition|cache-control'

echo "== 7. 登出（撤销当前刷新令牌） =="
curl -fsS -X POST "$HOST/api/v1/auth/logout" -H 'Content-Type: application/json' \
  -d "{\"refreshToken\":\"$REFRESH\"}"; echo

echo "== 全部通过 ✅ =="
```

> ⚠ **注意**：脚本中第 7 步之后 `$REFRESH` 已失效；`$PH` / `$SALT` / `$WRAP` / `$IV` 必须由
> **真实的 WebCrypto 派生结果**替换（`PBKDF2-SHA256` 210000 轮 + `HKDF` + `AES-GCM`），
> 服务端会校验 `passwordHash` 长度并与 argon2id 存储串比对，任意字符串会被判为口令不正确。
