# MedDesk · 安全文档

> 一句话模型：**服务端是一个"看不懂内容的保险柜"**——它校验身份、存储密文信封、做冲突裁决，但任何业务数据（药物、档案、照片）在离开你的浏览器之前就已加密。

## 目录

1. [密钥派生链](#1-密钥派生链)
2. [口令与登录](#2-口令与登录)
3. [令牌策略](#3-令牌策略)
4. [威胁模型与能力边界](#4-威胁模型与能力边界)
5. [传输与 Nginx 加固](#5-传输与-nginx-加固)
6. [导出功能的明文窗口](#6-导出功能的明文窗口)
7. [给用户的操作建议](#7-给用户的操作建议)

---

## 1. 密钥派生链

```
password（仅存在于你的内存）
   │
   ├─① PBKDF2-SHA256(password, salt, 210000 iter, 32B) ──→ authHash ──base64──→ 送服务端（argon2id 二次哈希后落库）
   │
   └─② HKDF-SHA256(authHash, salt, info="meddesk-kek-v1", 32B) ──→ KEK
                                                                      │
随机 DEK（32B，首次注册生成） ◄──③ AES-GCM 包裹── wrappedDEK + dekIv ──→ 送服务端落库
   │
   └─④ 每条记录：AES-GCM-256(DEK, iv12, JSON.stringify(record)) ──→ {iv, ct} 上传
```

| 环节 | 算法/参数 | 存储位置 |
|---|---|---|
| ① authHash | PBKDF2-SHA256，210000 轮，16B salt | 仅 base64 后上行，服务端 argon2id 二次哈希 |
| ② KEK | HKDF-SHA256，info=`meddesk-kek-v1` | **从不离开浏览器内存** |
| ③ DEK 包裹 | AES-GCM-256，12B IV | `wrapped_dek`/`dek_iv` 落服务端（密文） |
| ④ 记录加密 | AES-GCM-256，每条独立 12B IV | `sync_records.iv/ct`（密文） |

要点：

- 服务端只有 `authHash`（还做了 argon2id 二次哈希）与 `wrappedDEK`——**没有口令、没有 KEK，无法解开 DEK**。
- KEK 只在内存中存在；刷新页面后由「口令 + salt」重新派生，`wrappedDEK` 从服务端取回解包。
- 附件（照片）：客户端压缩（最长边 ≤1280px、JPEG q=0.82）→ 同一 DEK 加密 → 以密文落盘 `/home/ops/service/meddesk/data/{user}/{yy}/{id}.bin`（权限 0600）。

## 2. 口令与登录

- 服务端收到 `passwordHash`（即 authHash 的 base64）后，用 **argon2id**（默认 m=64MiB, t=3, p=2，可用环境变量调）再哈希一次存储，比较用 `subtle.ConstantTimeCompare` 等价的常数时间逻辑。即使库泄露，攻击者拿到的是 argon2id 哈希而非可直接验证的 authHash。
- 登录失败限流：同一邮箱 15 分钟窗口内失败 ≥10 次返回 429（Redis 关闭时由进程内计数器承担）。
- **改口令**：客户端用旧口令解出 DEK → 用新口令重新派生 KEK → 重新包裹 DEK → 上传新 `wrappedDek`/`kdfSalt`/`dekIv`。全程**无需服务端介入**，服务端只更新三个字段。

## 3. 令牌策略

| 令牌 | 有效期 | 存储位置 | 特性 |
|---|---|---|---|
| Access Token | 15 分钟 | 内存 + sessionStorage | HS256 JWT；`sub/typ/iat/exp/jti` |
| Refresh Token | 30 天 | 服务端只存 sha256 | 32B 随机数 base64url；**每次刷新轮换** |

- **轮换**：每次 refresh 签发新令牌对；旧 refresh token 进入 cache 吊销名单（TTL=RefreshTTL）。
- **宽限**：`MEDDESK_REFRESH_REUSE_GRACE`（默认 30s）内允许旧令牌重用一次，兼容多标签页并发刷新。
- **重放检测**：宽限期外使用已轮换/已吊销的旧令牌 → 判定令牌泄露，**撤销该用户全部会话**（含全部 refresh token；吊销时刻之前签发的 access token 在其剩余有效期内同步失效，吊销之后重新登录不受影响）。
- **全设备登出**：`logout-all` 吊销该用户全部 refresh token，并记录用户级吊销时刻。
- Access Token 校验为无状态 JWT + 用户级吊销名单检查；**秒级 iat 的固有窗口**：同一秒内「签发的令牌」与「吊销动作」无法区分先后，旧令牌最多多存活 1 秒——对个人工具可接受。

## 4. 威胁模型与能力边界

| 攻击场景 | 结果 | 依据 |
|---|---|---|
| 服务端被完整攻破（DB+文件+内存快照） | 攻击者只拿到密文信封、argon2id 哈希、wrappedDEK | KEK 由口令派生，从未上行 |
| 数据库被拖走（无应用内存） | 同上，且 argon2id 显著提高离线爆破成本 | ① |
| 网络中间人 | TLS 1.2+ 强制 + HSTS | 见 §5 |
| 浏览器设备被物理接触 | **超出保护范围**：登录态 + 内存 DEK 可被读取 | 建议启用系统磁盘加密与锁屏 |
| 服务端管理员作恶 | 可以删数据（DoS），**不能**读/改你的记录内容（篡改会被 AES-GCM 认证标签拒绝） | E2EE 核心价值 |

**明确的能力边界（务必知晓）**：

1. **忘记口令 = 数据不可恢复**。没有 KEK 就没有 DEK；服务端无重置通道，这是 E2EE 的代价而非缺陷。
2. 删除服务端数据是永久的（`sync/reset` 需 `confirm: DELETE-ALL` 显式确认）。
3. 服务器只做"存储与转发"：可用性由你维护（备份见 DEPLOYMENT.md 第 8 节）。

## 5. 传输与 Nginx 加固

- **CSP**：`default-src 'self'`；`img-src 'self' data: blob:`（照片预览）；`script-src 'self'`——**禁止任何第三方脚本**。
- `X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、`X-Frame-Options: DENY`、`Cross-Origin-Opener-Policy: same-origin`。
- HTTPS 下自动附加 `Strict-Transport-Security: max-age=31536000`（HSTS）。
- CORS：默认只允许同源（**不**返回 `*`）；白名单用 `MEDDESK_CORS_ORIGINS` 精确匹配。
- 限流：按客户端 IP 令牌桶（默认 20 rps / burst 60），超限 429 + `Retry-After`。
- 上传体积：Nginx `client_max_body_size` 与应用 `MEDDESK_UPLOAD_MAX_BYTES` 双层限制。
- 公网部署建议（二选一）：Nginx 层加 IP 白名单（`allow/deny`），或在 Nginx 前再套一层 Basic Auth / mTLS——MedDesk 自身鉴权已完备，这两层是额外的"防扫描噪音"。

## 6. 导出功能的明文窗口

PDF/CSV 导出在**服务端内存**渲染，这是唯一一次明文经过服务端的路径：

- 客户端把**已解密**的行数据一次性 POST 到 `/api/v1/export/*`；
- 服务端仅在内存中渲染（CSV 加 UTF-8 BOM；PDF 用 go-pdf/fpdf），**不落盘、不写日志**；
- 响应头 `Cache-Control: no-store`，请求体上限 4 MiB、行数上限 20000。

缓解：该窗口仅覆盖你主动导出的时间段与所选行数；若希望完全消除，可用应用内「导出 JSON 备份」（纯客户端生成）替代服务端 PDF 渲染（代价是无中文排版 PDF）。

## 7. 给用户的操作建议

1. **口令即一切**：使用 ≥16 位且只在这里使用的口令；改口令=重新包裹 DEK，旧口令立即失效。
2. **恢复码/口令备份**：把口令抄写在纸上放安全位置，或使用密码管理器；没有任何"找回"通道。
3. **每月导出 JSON 备份**（设置页），保存在本地加密盘——这是"服务器挂了/忘了口令前最后一次导出"之外唯一可自救的快照。
4. 新设备登录需要：邮箱 + 口令（其余全部可重新派生），无需迁移任何密钥文件。
5. PWA 关键提醒需要**系统通知白名单**：浏览器通知无法强制穿透系统勿扰，请把站点/PWA 加入系统级允许列表（详见 EXTENDING.md 的 Web Push 预留点）。
