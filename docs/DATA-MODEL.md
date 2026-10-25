# MedDesk · 数据模型文档

> 分三层：**服务端关系模型**（PostgreSQL，只存密文）→ **客户端域模型**（明文，仅浏览器）→ **同步协议**（密文信封 + 游标）。
> 服务端 DDL 的唯一事实来源：[internal/store/migrations/0001_init.sql](../internal/store/migrations/0001_init.sql)。

## 目录

1. [服务端关系模型](#1-服务端关系模型)
2. [客户端域模型](#2-客户端域模型)
3. [同步协议](#3-同步协议)
4. [客户端 IndexedDB 结构](#4-客户端-indexeddb-结构)

---

## 1. 服务端关系模型

应用启动时自动迁移（`MEDDESK_DB_MIGRATE_ON_BOOT=true`），迁移幂等、只增不改，版本记录在 `schema_migrations`。

### 1.1 users — 用户主表

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | `uuid` PK | 服务端生成 |
| `email` | `citext`/`text` UNIQUE | 大小写不敏感（citext 扩展不可用时自动退化为 `lower(email)` 唯一索引） |
| `password_hash` | `text` | `argon2id(客户端 PBKDF2 结果)`，格式 `$argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>` |
| `kdf_salt` | `bytea` | 客户端 PBKDF2 salt（**服务端不可解**任何业务数据） |
| `wrapped_dek` | `bytea` | 被 KEK 包裹的 DEK（见 SECURITY.md 密钥派生链） |
| `dek_iv` | `bytea` | 包裹 DEK 的 AES-GCM IV |
| `created_at` / `updated_at` | `timestamptz` | |

### 1.2 refresh_tokens — 刷新令牌

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | `uuid` PK | 吊销名单/宽限键用此 id |
| `user_id` | `uuid` FK users `ON DELETE CASCADE` | |
| `token_hash` | `bytea` UNIQUE | **只存 sha256(明文)**，明文只在响应里出现一次 |
| `device_id` / `user_agent` / `ip` | `text` | 登录设备画像（审计用） |
| `expires_at` | `timestamptz` | 默认 30 天 |
| `revoked_at` | `timestamptz` | 登出/改密/重放时置位 |
| `replaced_by` | `uuid` | 轮换指向的新令牌 id（不建外键） |

索引：`(user_id)`、`(expires_at)`。
语义：轮换（`MarkReplaced`）会同时置 `revoked_at`，因此**「已被轮换」先于「已被吊销」判断**，否则宽限期内的并发刷新会被误判为重放（见 `internal/auth/service.go`）。

### 1.3 sync_records — 加密同步记录（核心表）

| 字段 | 类型 | 说明 |
|---|---|---|
| `user_id` | `uuid` FK `ON DELETE CASCADE` | |
| `table_name` | `text` | 逻辑表名：`profile` / `medication` / `schedule` / `intake` |
| `record_id` | `text` | 客户端生成的记录 UUID |
| `ver` | `bigint` | 客户端版本号，每次修改 +1（LWW 第一依据） |
| `updated_at` | `bigint` | 客户端修改时刻 Unix 毫秒（LWW 第二依据） |
| `deleted` | `boolean` | **墓碑**：删除=写入一条 `deleted=true`，不物理删行 |
| `iv` / `ct` | `bytea` | AES-GCM IV（12B）与密文（含 16B tag）——服务端**永不解密** |
| `device_id` | `text` | 最后写入设备 |
| `seq` | `bigint` | **服务端单调递增**（每用户独立分配器），pull 游标依据 |

主键 `(user_id, table_name, record_id)`；索引 `(user_id, seq)`（增量拉取）、`(user_id, table_name)`（按表统计）。
`seq` 由 `sync_state.last_seq` 在**应用事务内**原子分配，保证无空洞乱序。

### 1.4 sync_state — 序列号分配器

`user_id` PK FK，`last_seq bigint`（该用户已分配的最大 seq）。

### 1.5 attachments — 附件元数据

| 字段 | 说明 |
|---|---|
| `id` uuid PK | 下载路径 `/api/v1/attachments/:id` |
| `user_id` FK `ON DELETE CASCADE` | 越权访问一律返回 404（防资源枚举） |
| `record_id` text | 关联的业务记录 id（可空） |
| `iv` / `size` / `mime` | 附件密文的 IV / 字节数 / 客户端声明类型（白名单） |
| `path` | **相对** `MEDDESK_UPLOAD_DIR` 的密文落盘路径，如 `{user}/{yy}/{id}.bin` |
| `seq` | 预留：未来附件纳入同步序列 |

### 1.6 schema_migrations

`version text PK`，`applied_at`。Go 侧自建，按文件名升序、事务内执行、可重复跑。

---

## 2. 客户端域模型

明文只存在于：浏览器内存 / IndexedDB / 导出的 JSON 备份。上传前逐条加密为信封。

### 2.1 Profile（家庭档案）

```js
{ id, name, relation, birthDate, color, note, archived, updatedAt, ver, deleted }
```

- `relation`：`self` / `spouse` / `child` / `parent` / `other`。
- 档案之间数据严格隔离：`medication.profileId` 决定归属，视图按当前档案过滤。

### 2.2 Medication（药物）

```js
{ id, profileId, name, dosageAmount, dosageUnit, form, strength,
  foodInstruction, prescriber, pharmacy, sideEffects, notes,
  photoKeys: [], critical, active, startDate, endDate,
  stock, refillThreshold, updatedAt, ver, deleted }
```

- `foodInstruction`：`empty` 空腹 / `before` 餐前 / `after` 餐后 / `with` 随餐 / 空。
- `photoKeys`：附件 id 数组（密文图片，经 `/api/v1/attachments` 上传下载）。
- `critical`：关键提醒（重复提醒、请求系统通知白名单穿透）。
- `stock` / `refillThreshold`：库存与补药阈值，低于阈值 UI 显示「需补药」。

### 2.3 Schedule（排程）— 6 种频次的字段组合

```js
{ id, medicationId, kind, times:["08:00"], intervalHours, everyN, unit,
  specificDates:["2026-10-25"], cycleOnDays, cycleOffDays, cycleStart,
  startDate, endDate, remindOffsetMin, critical, updatedAt, ver, deleted }
```

| kind | 使用字段 | 示例 |
|---|---|---|
| `daily` | `times` | 每天 08:00 |
| `twice_daily` | `times`（取前 2 个，缺省 08:00/20:00） | 早晚各一次 |
| `interval_hours` | `intervalHours` + `times[0]` 起点 | 每 8 小时一次 |
| `specific_dates` | `specificDates[]` × `times` | 2026-10-25、11-01 复查前服药 |
| `custom_interval` | `everyN` + `unit('day'\|'week'\|'month')` + `startDate` + `times` | 每 3 天 / 每月 15 日（无该日取月末） |
| `cycle` | `cycleStart` + `cycleOnDays` + `cycleOffDays` + `times` | **21 天服 / 7 天停** |

通用字段：`startDate`/`endDate` 生效区间；`remindOffsetMin` 提前提醒分钟数。
周期判定：`(dayIndex % (on+off)) < on` → 服药期，否则停药期（`isActiveOn` 返回 false）。

完整示例（21 天周期）：

```json
{
  "id": "0a1b2c3d-...", "medicationId": "med-9f8e...", "kind": "cycle",
  "times": ["20:30"], "cycleStart": "2026-10-01", "cycleOnDays": 21, "cycleOffDays": 7,
  "startDate": "2026-10-01", "remindOffsetMin": 0, "critical": true,
  "updatedAt": 1769472000000, "ver": 3, "deleted": false
}
```

### 2.4 Intake（服药记录）

```js
{ id, occKey, medicationId, profileId, scheduledFor, status,
  takenAt, doseText, note, snoozeUntil, updatedAt, ver, deleted }
```

- `occKey` = `${scheduleId}@${timeMs}`：一次排程发生的幂等键。重复打卡=同 occKey upsert。
- `status`：`pending` / `taken` 已服用 / `skipped` 已跳过 / `snoozed` 已推迟。
  漏服（`missed`）与逾期是**派生态**：由排程引擎按 `timeMs < now-15min` 且未记录推导，不落库。
- `scheduledFor`：计划时刻毫秒；`takenAt`：实际打卡时刻。

---

## 3. 同步协议

### 3.1 信封格式

每条记录上传/下发均为密文信封，服务端**只看到**：

```json
{ "table": "medication", "id": "…", "ver": 3, "updatedAt": 1769472000000,
  "deleted": false, "iv": "<base64 12B>", "ct": "<base64 密文>" }
```

`ct` = `AES-GCM-256(dek, JSON.stringify(记录))`；删除墓碑允许 `ct` 为空串。

### 3.2 Push（上行）

`POST /api/v1/sync/push` 携带一批变更（≤ `MEDDESK_SYNC_MAX_ITEMS`），服务端在**单事务**内逐条裁决：

```
LWW 裁决（better(in, cur)）：
  in.ver >  cur.ver                → 上行胜
  in.ver == cur.ver && in.updatedAt >  cur.updatedAt → 上行胜
  in.ver == cur.ver && in.updatedAt == cur.updatedAt && in.deleted && !cur.deleted → 上行胜
  其余                             → 服务端当前版本胜
```

- 上行胜：写库、分配新 `seq`、计入 `applied`。
- 服务端胜：原样回传服务端版本计入 `conflicts`，**客户端负责合并**（提示「该记录已在其他设备更新」）。
- 响应：`{ applied, conflicts, cursor, syncedAt }`，`cursor` = 本次分配的最大 seq。

> `intake` 虽是事件型数据，一期仍采用同一 LWW 规则：冲突时服务端版本胜出并回传，避免事件流合并的复杂度（幂等键 occKey 已覆盖绝大多数并发场景）。

### 3.3 Pull（下行）

`GET /api/v1/sync/pull?cursor=<seq>&limit=<n>`：

- 按 `seq > cursor` 升序返回，`limit` 缺省 300、上限 1000；
- 返回 `hasMore` 与新的 `cursor`（本页最大 seq），客户端循环拉取直到 `hasMore=false`；
- 游标语义 = 「我已见到该用户的第 N 个 seq」，跨设备不共享，各设备独立维护。

### 3.4 时序示例（两设备并发改同一药物）

```
设备A: 改药名(ver2) → push {ver:2} → applied, seq=7
设备B(离线): 改剂量(ver2, updatedAt 更晚) → 上线 push {ver:2} →
             裁决: ver 相等, B 的 updatedAt 更新 → B 胜, seq=8
设备A: pull(cursor=7) → 收到 B 的 ver2 信封 → 解密覆盖本地 → UI 提示已合并
设备B: pull(cursor=8) → 空增量
删除: 任意设备 push {deleted:true} → 双方 pull 后各自物理删除本地行
```

### 3.5 服务端 API 与同步表的对应

| 同步表名 | 服务端存储 | 说明 |
|---|---|---|
| `profile` / `medication` / `schedule` / `intake` | `sync_records`（同一张密文表） | 逻辑表名只是信封的一个字段，新增模块无需改表 |

---

## 4. 客户端 IndexedDB 结构

库名 `meddesk`，版本 1（见 `web/assets/js/db.js`）：

| objectStore | keyPath | 索引 | 内容 |
|---|---|---|---|
| `records` | `['table','id']` | `table` | **明文**域模型记录（含墓碑行） |
| `blobs` | `key` | — | 附件明文 Blob（本地照片压缩后暂存） |
| `meta` | `key` | — | 同步游标 `sync.cursor`、示例数据标记等 |

要点：

- 本地库存**明文**（浏览器沙箱 + 设备锁保护）；上传前才在内存中加密，密钥不落盘（会话结束即失效）。
- `sync_state.last_seq` 在**服务端**，客户端只存自己的读取游标（`meta.sync.cursor`）。
- 「设置 → 清空全部数据」会清空三个 store 与会话；服务端数据需另走 `POST /api/v1/sync/reset`（需 `confirm: DELETE-ALL`）。
