# 家庭关联：短码方案接口契约（待评审）

> 状态：**草案，未实现**。本文只定义契约，确认后才动代码。
> 目标：把「关联码」从**数据信封**（KB 级、随历史增长、非实时）改为**固定 8 位指针**（实时同步）。

---

## 1. 为什么必须改（问题陈述）

当前实现把业务数据装进关联码本身：

```
关联码 = base64url( 盐 + 包裹密钥 + AES-GCM(药物清单 + 用药历史 + 依从性) )
```

后果：码随历史范围线性变长（90 天约 3~6 KB），无法口述、无法打印，且数据是快照。

根因是端到端加密：服务端只有密文，**没有 DEK 就读不出任何人的数据**。
所以「短码 + 直读数据库」必须先解决「密钥怎么安全交给查看方」。

---

## 2. 密钥流（核心机制）

```
① 所有者端
   code      = 8 位 Crockford Base32（排除 0/O/1/I/L，40 bit 熵）
   kdfSalt   = random(16)
   KEK       = PBKDF2-SHA256(code, kdfSalt, 210000)
   wrappedDEK= AES-GCM(KEK, ownerDEK)      // 约 48 字节
   上传：{ code_hash: argon2id(code, hashSalt), kdfSalt, wrappedDEK, dekIv, hashSalt, expiresAt }

② 查看方端
   输入 code → POST /family/grants/redeem { code }
   服务端：argon2id(code, hashSalt) 与 code_hash 比对 → 命中则绑定 viewer_id，返回 { kdfSalt, wrappedDEK, dekIv, ownerId }
   查看方：KEK = PBKDF2-SHA256(code, kdfSalt) → ownerDEK = AES-GCM⁻¹(KEK, wrappedDEK)
   再调 GET /family/records?grantId=... 拉密文，本地用 ownerDEK 解密
```

关键：**DEK 全程只有密文过服务端**；服务端能看到的事实只有「账号 A 授权了账号 B」。

---

## 3. 数据模型

```sql
CREATE TABLE family_grants (
    id            uuid PRIMARY KEY,
    owner_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    viewer_id     uuid NULL REFERENCES users(id) ON DELETE CASCADE,  -- 兑换后写入，兑换前为 NULL
    code_hash     bytea NOT NULL,        -- argon2id(code, hash_salt)
    hash_salt     bytea NOT NULL,
    kdf_salt      bytea NOT NULL,        -- 客户端 PBKDF2 用，下发给查看方
    wrapped_dek   bytea NOT NULL,        -- 约 48 字节
    dek_iv        bytea NOT NULL,
    status        text NOT NULL,         -- 'pending' | 'active' | 'revoked'
    expires_at    timestamptz NOT NULL,  -- pending 默认 +24h；active 默认 +30d
    created_at    timestamptz NOT NULL DEFAULT now(),
    redeemed_at   timestamptz,
    revoked_at    timestamptz
);
CREATE INDEX family_grants_owner_idx  ON family_grants (owner_id, status);
CREATE INDEX family_grants_viewer_idx ON family_grants (viewer_id, status);
-- 同一对账号只允许一条 active 授权，避免重复授权造成权限膨胀
CREATE UNIQUE INDEX family_grants_pair_uidx
  ON family_grants (owner_id, viewer_id) WHERE status = 'active';
```

---

## 4. 接口

### 4.1 创建授权（所有者）

```
POST /api/v1/family/grants
Auth: Bearer
Body: { kdfSalt: b64, wrappedDek: b64, dekIv: b64, hashSalt: b64, codeHash: b64, ttlHours?: 24 }
Resp: { grantId, expiresAt, codeHint: '已生成，服务端不保存明文码' }
```

- 服务端**不接收明文 code**，只收 `codeHash`（客户端用 argon2id 或 PBKDF2 先算好）
- 但客户端要 PBKDF2 派生 KEK 需要明文 code → 由客户端生成并**只在本地显示/复制**，永不外传
- 创建后返回一次 `grantId`，之后不再返回任何密钥材料

### 4.2 兑换（查看方）

```
POST /api/v1/family/grants/redeem
Auth: Bearer
Body: { codeHash: b64 }
Resp: { grantId, ownerId, ownerName, kdfSalt, wrappedDek, dekIv, expiresAt }
错误: 404（不存在/已过期）/ 409（已被他人兑换）/ 429（尝试过频）
```

- 命中条件：`status='pending' AND expires_at > now()` 且 `codeHash` 匹配
- 命中后原子更新：`status='active', viewer_id=me, redeemed_at=now(), expires_at=now()+30d`
- 已是 `active` 且 `viewer_id=me` → 幂等返回同一 grant（不报错）
- 已是 `active` 但 `viewer_id≠me` → 409，**不泄露 owner 信息**

### 4.3 拉取对方数据（查看方）

```
GET /api/v1/family/records?grantId=<uuid>&sinceSeq=<int>&limit=<1..200>
Auth: Bearer
Resp: { items:[{table,recordId,ver,updatedAt,deleted,iv,ct}], nextSeq, hasMore }
```

- **鉴权铁律**：先按 `grantId` 查出 grant，校验 `grant.viewer_id = 当前用户` 且 `status='active'` 且未过期；
  **绝不接受客户端传 ownerId**，owner 一律从 grant 行取。否则就是一个跨账号越权读漏洞。
- 复用现有 `sync_records` 的密文结构与 seq 游标语义，但不写 `sync_state`（只读）

### 4.4 列出/撤销（所有者）

```
GET    /api/v1/family/grants            → [{grantId, viewerName, status, expiresAt, createdAt}]
DELETE /api/v1/family/grants/:id        → 204，置 status='revoked', revoked_at=now()
```

---

## 5. 防护（缺一不可）

| 项 | 值 | 理由 |
|---|---|---|
| 兑换限流 | 5 次/分钟/IP，20 次/小时/账号，连续失败 10 次锁 1 小时 | 40 bit 熵在无限流下可爆破 |
| 码有效期 | pending 24h；active 30d（可续） | 过期即失效 |
| 一次性 | 首次兑换绑定 viewer，之后码作废 | 防止转发扩散 |
| 拒绝枚举 | 失败与「不存在」返回同一 404 + 固定延时 | 防探测 |
| 审计 | 记录 redeem/revoke 事件（不含明文码） | 事后追溯 |

---

## 6. 必须承认的两个局限（请在评审时确认接受）

1. **撤销不能回溯**：查看方一旦解出 DEK 并缓存，撤销只能阻止「之后的读取」，
   已经下载的数据无法收回。真正的强撤销需要**轮换 owner 的 DEK 并重加密全部记录**，
   代价极大（每条记录重写 + 所有其他查看方重新授权）。本方案不做，需在 UI 上明说。
2. **服务端可见授权关系**：它知道「A 授权了 B、何时授权、何时撤销」，但看不到任何用药内容。

---

## 7. 前端改动

| 文件 | 改动 |
|---|---|
| `web/assets/js/family.js` | 生成改为「8 位码 + 上传信封」；兑换改为调接口；详情页数据源从 blob 快照改为 `GET /family/records` + 本地解密；新增「我的授权」列表（含撤销） |
| `web/assets/js/family-link.js` | 新增：短码生成/格式化（Crockford Base32、排除易混字符）、PBKDF2 派生、密文分页拉取 |
| 保留 | 现有快照关联码**保留**为「离线分享」，不删除（对方无网络时仍可用） |

---

## 8. 验收清单（含越权测试）

**功能**
- [ ] 生成 8 位码，长度固定，与历史范围无关
- [ ] 兑换后能实时看到 owner 新增的用药记录（owner 侧改一条，viewer 侧刷新可见）
- [ ] 详情页无「今天要处理」

**安全**
- [ ] 未登录调任一接口 → 401
- [ ] 用 B 的 grantId 让 C 去拉 → 403（不能靠 grantId 猜）
- [ ] 篡改请求里的 ownerId → 无效（服务端忽略该参数）
- [ ] 同一码被第二人兑换 → 409，且不返回 owner 信息
- [ ] 过期后兑换 → 404
- [ ] 所有者撤销后，viewer 再拉取 → 403
- [ ] 连续 10 次错误码 → 触发锁定
- [ ] 换密码后：grant 仍有效（DEK 不变），但需重新登录以取得 DEK —— 需实测确认

**兼容**
- [ ] 老版本客户端（只有快照关联码）不受影响

---

## 9. 工作量

| 部分 | 内容 | 量 |
|---|---|---|
| 后端 | 迁移、4 个接口、argon2 校验、限流、越权测试 | ~250 行 + 测试 |
| 前端 | `family-link.js`（~180 行）、`family.js` 改造（~120 行） | ~300 行 |
| 联调 | 双账号手工验证 + 越权用例 | 0.5 天 |

---

## 10. 待你确认的三个决策点

1. **码长 8 位**（40 bit）是否够？可加到 10 位（50 bit）但更难口述。
2. **active 有效期 30 天**是否合适？还是要「长期有效直到撤销」。
3. **是否需要「一次性分享链接」**（看一次即失效），用于临时给医生看记录？

确认后我按此实现，先写后端迁移与接口，再改前端。
