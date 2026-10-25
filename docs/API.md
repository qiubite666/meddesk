# MedDesk · HTTP API 文档（v1）

> 本文是 [`CONTRACTS.md`](../CONTRACTS.md) 第 3 节的展开说明，字段以 [`internal/model/model.go`](../internal/model/model.go) 为准。
>
> - **Base URL**：`https://med.example.com`（本地开发：`http://localhost:8080`）
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
- [附录 A：完整 curl 冒烟脚本](#附录-a完整-curl-冒烟脚本)

---

## 1. 通用约定

### 1.1 鉴权

除健康检查与部分认证接口外，所有接口需要在请求头携带访问令牌：

```http
Authorization: Bearer <accessToken>
```

- 令牌类型 claim `typ="access"`，`sub` 为用户 UUID HS256 签名。
- 有效期 `MEDDESK_ACCESS_TTL`（默认 `15m`）。
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

| 场景 | 上限 | 触发的错误码 |
|---|---|---|
| 全局 `BodyLimit` | `MEDDESK_UPLOAD_MAX_BYTES`（默认 8 MiB） | `payload_too_large` (413) |
| 附件上传接口 | 同上 | `payload_too_large` (413) |
| 导出接口 | 硬编码 **4 MiB** | `payload_too_large` (413) |
| Nginx `client_max_body_size` | `12m` | Nginx 直接返回 413 HTML |

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
curl -fsS http://127.0.0.1:8080/api/v1/health
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
curl -fsS http://127.0.0.1:8080/api/v1/ready
```

---

## 4. 认证接口 `/auth`

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
| `device` | object | ❌ | — | 设备信息，见下方 `DeviceInfo` |

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

```bash
curl -fsS https://med.example.com/api/v1/auth/me -H "Authorization: Bearer $ACCESS"
```

```json
{"user":{"id":"6f1c…","email":"me@example.com","createdAt":"2026-10-26T02:15:00Z"},"serverTime":1761446400000}
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

1. `/home/ops/service/fonts/NotoSansSC-Regular.ttf`
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
