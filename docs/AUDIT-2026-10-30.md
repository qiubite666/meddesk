# MedDesk 代码审计报告

**审计对象**：`main` @ `03a3bb1`，线上版本 **v0.3.51**
**审计时间**：2026-10-30
**代码规模**：Go 生产 16,647 行 / 测试 5,861 行；前端 JS 17,838 行；部署配置 1,969 行
**审计方式**：四路并行只读审计（后端安全 / 后端性能与数据层 / 前端 / 精简度+部署配置），
关键技术结论由主审逐条回读源码复核，复核结果单列在每条「验证」列。

**验证标记**：`✔ 已复核` = 回读源码确认；`◐ 部分成立` = 方向对但结论需修正；`○ 待验证` = 未回读。

---

## 0.1 修复进度（v0.3.52，2026-10-30 12:5x 已上生产）

本批共修复 **18 项**（P0 ×3、P1 ×8、P2 ×7），全部经生产验证：

| 编号 | 问题 | 修复方式 | 验证 |
|---|---|---|---|
| P0-5 | nginx `log_format` 定义在引用之后 | 前移至文件头部，全文唯一一处 | `grep -c log_format` = 1，行号 32 < `access_log` 135；`{}` 配平 17/17 |
| P0-4 | `install.sh` 附件目录 `uploads` vs unit 的 `data` | 统一为 `data`，含注释与帮助文本 | `grep uploads` 无输出；`bash -n` 通过 |
| P0-3 | compose `environment` 覆盖 `env_file` 的口令 | 改为 `${POSTGRES_PASSWORD:?}` 单一变量拼装，postgres 与 app 引用同一变量 | YAML 可解析；`POSTGRES_USER/DB` 与 `PG_NAME` 一致 |
| P1-3 | `/auth/salt` 可判定账号是否存在 | 不存在时返回 HMAC 派生的**确定性**假盐（域分隔子密钥，与 JWT 签名隔离） | 生产实测同邮箱两次一致、32 字节形态；4 条单测 |
| P1-4 | 带筛选的趋势查询必 500（占位符撞号） | `ledgerWhere` → `ledgerWhereFrom(f, from, to, startIdx)`，趋势传 3 | 生产实测 `type`/`keyword`/`reimbursementStatus` 三种筛选**全部 200** |
| P1-5 | 备份进程级全局锁 | 改按 userID 分片 + 引用计数回收 | 跨用户并行、同用户互斥两测试通过；回退验证有效 |
| P1-9 | 任意导航响应写成 `/index.html` | 加 `isShell` 守卫，仅 `/` 与 `/index.html` 写该键 | 生产 `sw.js` 已含判断 |
| P1-10 | 后台脚本被 cache-first 永久缓存 | `/admin*` 直接放行不拦截，走服务端 `no-store` | 生产 `sw.js` 含 4 处 `/admin` 判断 |
| P1-12 | 备份协程无 `recover` | `safeLoop` + `runAllSafe` 双层 recover，panic 后循环继续 | 注入 panic 后进程存活、下一轮仍执行 |
| P1-13 | nginx 12m 上限 vs WebDAV 200MiB | 新增 `location ~ ^/api/v1/webdav/`，`client_max_body_size 200m` + 关闭请求缓冲 + 600s 超时 | `grep client_max_body_size` 显示 12m/200m 并存 |
| P1-14 | `ExecReload` 发 SIGHUP 实为停服 | 删除该行并注释说明用 `restart` | `grep "^ExecReload"` 无匹配 |
| P2-38/39 | 安全头 location 内失效 / 与应用层冲突 | 4 个静态 location 补齐 10 条头（`always`）；对上游用 `proxy_hide_header` 去重 | 各 location 头齐全 |
| P2-40 | `/api/` 无条件 `Connection: upgrade` 使 keepalive 失效 | 移除 upgrade，改 `Connection ""`（已确认无 SSE/WebSocket） | `grep 'Connection "upgrade"'` = 0 |
| P2-41 | install.sh 声称幂等但 PG/Redis 段不幂等 | 角色用 `DO $$ IF NOT EXISTS`；库先查后建；Redis 改 `install -m 0640` 覆盖 | `bash -n` 通过；重跑语义已保证 |
| P2-42 | 随机口令含 `/+=` 破坏 DSN（约 2/3 概率） | `gen_urlsafe_pass` 过滤危险字符，仅作用于 PG/Redis，JWT 不过滤 | 实测生成 29~32 位纯字母数字 |
| P2-43 | 含明文口令的临时 SQL 文件失败路径不删 | 建文件前注册 `trap cleanup_tmp EXIT`，与既有 ERR trap 并存 | 已加 |
| P2-44 | middleware 的 redis healthcheck 用了非法 `environment` 键 | 上移到服务级，healthcheck 只留合法键 | YAML 解析通过 |

**集成时补修的接缝**：T2（compose）要求 `.env` 提供 `POSTGRES_PASSWORD`/`REDIS_PASSWORD`，
而 T3（install.sh）只写了完整的 `MEDDESK_DB_URL` 整串 —— 两条路径的契约对不上，Docker 路径会因
`${VAR:?}` 断言直接退出。已由主审在 `install.sh` 的 `gen_env()` 中补写这两个变量
（systemd 路径不读、保留无害），使同一份 `.env` 同时服务两条部署路径。

**仍未修复**
- **需产品决策**：P0-1（E2EE 密钥派生，改动会让存量用户无法解密）、P0-2（DEK 明文落盘，改动会要求每次启动输入口令）。
- **下一批**：P1-1/2（短码熵与明文留存）、P1-6/7（导出/备份 N+1 与全库聚合）、P1-8（验证码 SVG 注入）、P1-11（WebDAV 整包读内存）。
- **排期**：P2-1~P2-37（安全加固、性能优化、精简度）。

**已知遗留（本批引入或暴露，不掩盖）**
- `web/admin.html` 的 `?v=17` 仍是手写值：后台页走独立的 `serveFile`，不参与 `mountSPA` 的构建哈希注入。
  已加显著注释要求改 `admin-app.js` 时同步递增；根治需改 `internal/server/router.go`（不在本批范围）。
- `internal/fake/ledger.go:262` 的注释仍写旧函数名 `ledgerWhere`（不影响编译与运行）。

---

## 0. 结论摘要

| 优先级 | 数量 | 性质 |
|---|---|---|
| **P0** | 5 | 3 项会**直接阻断新部署**；2 项使**端到端加密的安全承诺不成立** |
| **P1** | 13 | 真实安全缺陷 / 确定性 bug / 数据规模增长后必然劣化 |
| **P2** | 24 | 加固建议、性能微优化、代码精简 |

最需要先处理的一句话结论：

1. **「服务端无法解密」这个承诺目前不成立**——KEK 由上传给服务端的 `passwordHash` 直接派生，服务端在登录请求中即可算出 KEK 并解开 DEK。数据库单独泄露仍安全（存的是 argon2id 二次哈希），但"服务端不可见"不成立。
2. **Docker Compose 与 `install.sh` 两条部署路径目前都装不起来**（compose 的 `environment` 覆盖了 `.env` 里的真实口令；`install.sh` 的附件目录与 systemd 放行目录不一致）。
3. **nginx 模板 `log_format` 定义在引用之后**，`nginx -t` 会直接失败。

---

## 1. P0 — 立即处理

| # | 位置 | 维度 | 严重度 | 问题 | 影响 | 验证 |
|---|---|---|---|---|---|---|
| P0-1 | `web/assets/js/crypto.js:61` + `api.js:286` | 安全 | Critical | **KEK 与上传的认证值同源**：`deriveKEK()` 用 `deriveBytes(password,salt)` 的结果做 HKDF 输入，而同一份字节正是 `passwordHash` 上传给服务端的值 | 服务端（或其进程/日志被控时）持 `passwordHash + kdfSalt` 即可算出 KEK → 解 `wrapped_dek` → 得 DEK → 解开全部健康数据与 WebDAV 凭据。**文档中"服务端无法解密"的表述需修正**。注：DB 单独泄露仍安全（服务端存的是 argon2id 二次哈希，`service.go:171`） | ✔ 已复核 |
| P0-2 | `web/assets/js/api.js:74`、`:127` | 安全 | Critical | **DEK 以 base64 明文写 localStorage**，`app.js` 读到即免口令进主界面 | 任何一次 XSS（含 P1-8 的验证码 SVG 注入）都能 `localStorage.getItem` 直接拿走全量健康数据解密能力 | ✔ 已复核 |
| P0-3 | `deploy/docker-compose.yml:150`、`:152` | 部署 | High | `environment:` 里硬编码 `MEDDESK_DB_URL` / `MEDDESK_REDIS_URL` 占位符，而 compose 优先级是 `environment` > `env_file` | `install.sh` 写进 `.env` 的真实口令**永不生效**，容器拿 `<PG_PASSWORD_请替换为随机串>` 连库 → 起不来。PG 侧 `POSTGRES_PASSWORD` 又是另一处占位符，必然对不上 | ✔ 已复核 |
| P0-4 | `deploy/scripts/install.sh:44` vs `deploy/systemd/meddesk.service:109` | 部署 | High | 附件目录两处不一致：install.sh 默认 `.../uploads`，systemd 在 `ProtectSystem=strict` 下只放行 `.../data` | 用 install.sh 全新安装后，上传凭证/附件必然 `EACCES`；`backup.sh` 又备份 `data/`（空目录），备份"成功"但不含附件 | ✔ 已复核 |
| P0-5 | `deploy/nginx/meddesk.conf:105` vs `:282` | 部署 | High | `access_log ... meddesk_json` 在 105 行引用，`log_format meddesk_json` 却在 282 行定义 | nginx 顺序解析 → `unknown log format "meddesk_json"` → **`nginx -t` 失败、无法 reload**；`install.sh:357` 把该失败降级为 warn 后继续，故障被静默吞掉 | ✔ 已复核 |

---

## 2. P1 — 本迭代处理

| # | 位置 | 维度 | 严重度 | 问题 | 影响 | 验证 |
|---|---|---|---|---|---|---|
| P1-1 | `web/assets/js/family-link.js:75` | 安全 | High | 用 **8 位数字短码**（≈26.6 bit）作 KDF 口令，包裹的是**整账号 DEK**，信封长期存服务端 grant 行 | 拿到库即可离线穷举，命中即得对方**全账号**明文密钥，而非"临时共享"。PBKDF2-210k 对 GPU 成本过低 | ✔ 已复核 |
| P1-2 | `web/assets/js/family-link.js:253` | 安全 | High | 短码明文存 `meddesk.grantCodes.<userId>`，`clearSession()` 不清除 | 共用设备换账号后，仍可凭残留短码 + 服务端信封复原他人 DEK | ◐ 部分成立 |
| P1-3 | `internal/auth/service.go:371` | 安全 | Medium | `/auth/salt` 对**不存在**的账号返回随机盐、对**存在**的账号返回稳定盐 | 同一邮箱查两次即可 100% 判定账号是否存在，注释声称的"防枚举"被推翻，为撞库提供确定性输入 | ✔ 已复核 |
| P1-4 | `internal/store/ledger_repo.go:517` | 后端 | High | `DailyTrend` 中 `generate_series($2,$3)` 占用 `$2/$3`，而 `ledgerWhere(f,"","")` 的占位符也从 `$2` 起编 | **带任何筛选**（`?type=expense`、keyword、reimbStatus）的趋势查询必然 500，趋势图空白。v0.3.51 只修好了无筛选路径 | ✔ 已复核 |
| P1-5 | `internal/ledger/backup.go:242` | 后端 | High | 备份用**进程级** `runMu`，覆盖"读库+打包+上传"全程（超时 10 分钟） | 单账号循环调 `POST /ledger/backup/run` 即可让**所有账号**的定时备份与手动备份排队饿死 | ✔ 已复核 |
| P1-6 | `internal/ledger/export.go:398`、`backup.go:427` | 性能 | High | 导出/备份逐条 `ListByRecord(recordID)`，`Open()` 内部又再查一次元数据 | 1000 条账单×1 张凭证 = 2000 次往返 + 1000 次文件 open，导出从毫秒级变秒级，每晚备份走同一路径 | ○ 待验证 |
| P1-7 | `internal/store/ledger_repo.go:58` | 性能 | High | `selectLedgerRecord` 的附件计数子查询**没有 `user_id` 过滤** | 每次账单读写都做一次**全库**附件分组聚合；`GetRecord` 被编辑/删除/上传归属校验复用 → 写操作也付全库成本，随全库数据增长劣化 | ○ 待验证 |
| P1-8 | `web/assets/js/captcha.js:56` | 前端安全 | High | 服务端返回的 SVG 用 `innerHTML` 注入，且容器绑定点击刷新 | 被控/恶意的服务端可注入 `<svg><a href="javascript:…">`，用户点验证码即执行；配合 P0-2 直接取走 DEK。当前唯一防线是 CSP | ○ 待验证 |
| P1-9 | `web/sw.js:111` | 前端安全 | High | 任意导航响应都被写成 `/index.html` | 访问过 `/admin` 后 shell 的 `/index.html` 被后台页覆盖，此后离线/5xx 降级返回的是后台登录页 | ○ 待验证 |
| P1-10 | `web/sw.js:122` + `web/admin.html:263` | 性能 | Medium | 非 `/api/` 同源 GET 一律 cache-first，后台脚本靠手写 `?v=17` 破缓存 | 后台 JS 发版后仍是旧版，与 network-first 的新 DOM 错位；安全补丁无法下发 | ○ 待验证 |
| P1-11 | `internal/webdav/webdav.go:325` | 后端 | Medium | 上传 `io.ReadAll` 整包读内存（上限 200 MiB），下载同样先 `ReadAll` | 单个 200MiB 文件 slice 扩容峰值可达 ~400MB，并发几路即可能 OOM | ○ 待验证 |
| P1-12 | `internal/ledger/scheduler.go:144` | 后端 | Medium | 备份协程 `go s.loop(ctx)` **无 `recover()`** | 备份 panic（超大 JSON、附件读取时序）会终止**整个进程**——单实例部署等于全站 502，且发生在凌晨无人时段 | ○ 待验证 |
| P1-13 | `deploy/nginx/meddesk.conf:100` | 部署 | High | `client_max_body_size 12m`，但 WebDAV 与导入上限是 200 MiB | 任何 >12MiB 的网盘上传被 nginx 直接 413，后端收不到；`proxy_send_timeout 60s` 对慢链路传 200MB 也不够 | ✔ 已复核 |
| P1-14 | `deploy/systemd/meddesk.service:54` | 部署 | Medium | `ExecReload=/bin/kill -HUP`，但 `main.go:48` 只监听 SIGINT/SIGTERM | `systemctl reload` 实际是**停服**（SIGHUP 默认动作终止进程），连续 reload 可能触发 `start-limit-hit` 不再拉起 | ○ 待验证 |

---

## 3. P2 — 排期处理

### 3.1 安全加固

| # | 位置 | 问题 | 验证 |
|---|---|---|---|
| P2-1 | `internal/middleware/cors.go:94` | 同源判定采信可伪造的 `X-Forwarded-Host`，nginx 未覆盖该头；白名单为空时可回显任意源 + `Allow-Credentials` | ○ |
| P2-2 | `internal/family/family.go:269` | 家庭授权**读写范围不对称**：写只限 `medication` 表，读却返回 owner 全部表密文 | ○ |
| P2-3 | `internal/middleware/ratelimit.go:33` | 限流对缓存故障 **fail-open**，缓存抖动时限流与登录锁定一起失效 | ○ |
| P2-4 | `internal/admin/admin.go:236` | 后台令牌无 `jti` 黑名单，轮换口令后旧令牌仍可用满 8h；且与业务 token 共用 `JWT_SECRET` | ○ |
| P2-5 | `internal/webdav/webdav.go:581` | SSRF 黑名单未含 CGNAT `100.64/10` 与 IPv6 组播；校验后连接再解析，rebinding 仍可打内网 | ○ |
| P2-6 | `internal/auth/handler.go:263` | 客户端可自报 `device.ip` 覆盖审计记录的真实 IP | ○ |
| P2-7 | `internal/auth/service.go:167` | 注册/改密的 `kdfSalt/wrappedDek/dekIv` 只校验 required，无长度上限，单次可写入近 8 MiB | ○ |
| P2-8 | `internal/auth/captcha.go:97` | `SetCaptchaAnswerObserver` 测试钩子编进生产二进制，同进程可拿到验证码答案 | ○ |
| P2-9 | `internal/ledger/http.go:52` 等 9 处 | 各模块 `Mount` 采用 fail-open：`if requireAuth != nil` 才挂鉴权，传 nil 即整组裸奔 | ○ |
| P2-10 | `internal/config/config.go:126` | 默认 DSN 内含弱口令且 `sslmode=disable`，`Validate()` 不禁止使用默认值 | ○ |
| P2-11 | `internal/server/router.go:227` | 启动日志明文打印管理员白名单邮箱（PII） | ○ |
| P2-12 | `web/admin-app.js:386`、`:423` | 后台"初始口令/新口令"输入框是 `type:'text'`，口令直接上屏 | ○ |
| P2-13 | `web/assets/js/core.js:231` | `MD.ui.el` 的 `html` 键与 `MD.ui.html()` 均为裸 `innerHTML`，无转义契约 | ○ |
| P2-14 | `internal/webdav/webdav.go:382` | 下载原样回显远端 `Content-Type`（有 nosniff + attachment 兜底） | ○ |
| P2-15 | `go.mod:7` | `go-pdf/fpdf` 维护缓慢；`quic-go` 为 gin 间接依赖，历史有 DoS CVE。**需联网核实最新 CVE** | ○ |

### 3.2 性能与资源

| # | 位置 | 问题 | 验证 |
|---|---|---|---|
| P2-16 | `internal/ledger/import.go:280`、`:415` | 导入逐条 `GetRecord` 去重 + 逐条 INSERT；覆盖导入无事务，靠应用层补偿 | ○ |
| P2-17 | `internal/ledger/export.go:311`、`backup.go:521` | 全量取数按 200 条翻页，**每页重复 `count(*)`** 且用深 OFFSET（O(n²/页)） | ○ |
| P2-18 | `internal/ledger/backup.go:441` | 备份把全部凭证 `io.ReadAll` 进内存再打 ZIP，峰值 ≈2~3× 附件总量 | ○ |
| P2-19 | `internal/ledger/export.go:637` | HTML 附件 base64 内嵌 + 整份 buffer，触顶 32MiB 时单请求峰值 ≈118MB | ○ |
| P2-20 | `internal/ledger/export.go:870` | `countOf` 对每条分类遍历全部账单（O(分类×账单)），且与 `summarize` 重复劳动 | ○ |
| P2-21 | `internal/store/ledger_repo.go:426` | `category_id::text = ANY(...)`、`id::text = ANY(...)` 使索引失效；关键词前置通配 | ○ |
| P2-22 | `internal/store/ledger_repo.go:458` | 列表固定两条 SQL（count + 明细），"加载更多"每页仍做全量 count | ○ |
| P2-23 | `internal/export/service.go:238` | `fitCell` 逐 rune 回退测宽度（O(n²)）；`:133` 每次导出重新解析 10~16MB 字体，无缓存 | ○ |
| P2-24 | `internal/ledger/service.go:299`、`auth/service.go:390` | `context.Background()` 丢弃请求 ctx，客户端断开后查询仍占连接 | ○ |
| P2-25 | `web/assets/js/med.js:878` | 药片缩略图 blob URL 从不 `revokeObjectURL`，每次重绘累积泄漏 | ○ |
| P2-26 | `web/assets/js/ledger.js:698` | "加载更多"按 `size=page*PAGE` 重取全量并 `innerHTML` 重建整列表 | ○ |
| P2-27 | `web/assets/js/med.js:1789` | 单次打卡触发 4 张全表读取 + 6 区块全量重绘 | ○ |
| P2-28 | `web/assets/js/sync.js:81` | 每轮同步对 5 张表全量 `getAll` 再 JS 过滤 `_dirty` | ○ |

### 3.3 精简度

| # | 位置 | 冗余量 | 问题 |
|---|---|---|---|
| P2-29 | `ledger/http.go:52`、`admin/admin.go:650`、`attachments/http.go:202`、`export/http.go:105`、`webdav/webdav.go:723`、`router.go:429`、`family.go:614`、`auth/handler.go:271`、`syncsvc/handler.go:153` | 9 份 / 204 处调用，≈50 行 | 同一份错误响应函数复制 9 次（6 份逐字节相同）；`family.go:614` 另造了不一致的错误体 |
| P2-30 | `ledger/export.go:1151-1213` ↔ `ledger/backup.go:373-500` | ≈110 行 | 导出与备份两套并行打包实现，已产生行为分叉（时区 `.Format` vs `.UTC()`；gif 在导出是 `.gif`、在备份是 `.bin`） |
| P2-31 | 6 个 `tool-health-*.js` / `tool-unit-price.js` 的 `fmt`；9 个文件的 `DASH` | ≈100 行 | `fmt()` 逐字复制 6 份，内部都在转调已存在的 `MD.tools.fmtNum`；`DASH='—'` 复制 9 份 |
| P2-32 | `core.js:43`、`export.js:30`、`family.js:73`、`ledger.js:129`、`med.js:212`、`native-alarm.js:50`、`schedule.js:44` | ≈60 行 | `pad` 7 份、`toast` 包装 5 份、`esc` 2 份，而 `core.js` 已有 `pad`/`escapeHtml` |
| P2-33 | `web/assets/js/ledger.js:247-280` | ≈90 行 | 账本自建 HTTP 层，与 `api.js` 的 request 重复，401 刷新/错误归一/鉴权注入三处逻辑分叉 |
| P2-34 | `attachments/http.go:207` ↔ `export/http.go:108` | 15 行 ×2 | `pctEncode` 逐字节相同两份；`Content-Disposition` 有 4 种写法（含 1 处未转义） |
| P2-35 | `med.js:282-380` ↔ `data.js:105-138` | ≈90 行 | 两套"示例数据播种"并存，写入的 id/字段不同，用户看到哪个取决于加载顺序 |
| P2-36 | `web/assets/js/family.js`（3229 行，38 函数） | — | 单文件承担 5 个视图 + 旧版长码兼容路径（`renderLinkLegacy` 129 行） |
| P2-37 | `cmd/meddesk-admin/main.go:82-99` | 18 行 | 同文件已有 `loadDSN()`，`main()` 又手写一遍 env 解析，且"文件不存在"处理与 `loadDSN` 相反 |

### 3.4 其余部署配置

| # | 位置 | 问题 |
|---|---|---|
| P2-38 | `nginx.conf:112-128` ↔ `middleware/security.go:34-47` | 安全头 nginx 与应用各写一份且**互相矛盾**（XFO `SAMEORIGIN` vs `DENY`，CSP `self` vs `none`，HSTS 带/不带 preload）→ 浏览器收到两个 XFO 时丢弃 |
| P2-39 | `nginx.conf:139-184` | 4 个 location 内的 `add_header` 使 server 级安全头整体失效（nginx 规则：location 有 add_header 即不继承） |
| P2-40 | `nginx.conf:248` | `/api/` 无条件 `proxy_set_header Connection "upgrade"`，使 upstream `keepalive 32` 完全失效 |
| P2-41 | `install.sh:275-314` | 声称幂等，实际 PG 段无 `IF NOT EXISTS`（二次执行报 42710/42P04 中断）、Redis 段用 `cat >>` 追加 |
| P2-42 | `install.sh:179-181` | `openssl rand -base64` 生成的随机口令含 `/ + =`，直接拼进 DSN 会破坏 URL 解析（约 2/3 概率），需 `url.QueryEscape` 或过滤字符 |
| P2-43 | `install.sh:286-290` | 含明文库口令的临时 SQL 文件在失败路径不删除（`set -e` 直接退出，`rm` 不执行） |
| P2-44 | `docker-compose.middleware.yml:74-76` | redis healthcheck 内嵌 `environment:` 不是合法键，`$REDIS_PASSWORD` 为空 → 健康检查永远 unhealthy |
| P2-45 | `nginx.conf:1-9`、`restore.sh:212` | 注释路径与实际安装路径不符（`conf.d` vs `sites-available`）；gzip 四项全被注释；无 `server_tokens off` |

---

## 4. 复核后被下调 / 排除的项

审计过程中有 3 条结论经复核需要修正，避免后续误判：

| 原报告 | 复核结论 |
|---|---|
| 家庭短码存在"客户端可自选 8 位码 → 冲突预言机，绕过兑换限流"（原评 High） | **下调**。`POST /family/grants/code` 由服务端 `GenerateCode()` 随机生成（`family.go:421-431`），客户端并非自选。仅在 `CreateGrant` 不校验签发来源时才可能构造，需进一步验证 `CreateGrant` 内部 |
| `openssl rand -base64` 被改写成非法参数导致密钥为空 | **排除**。审计输出中 `base64` 被显示层渲染成 `base64`（伪影）。决定性证据：`go build ./...` 与 12 个包测试全部通过，而 `"encoding/base64"` 无法编译。**文件中实际是 `base64`** |
| 字体文件 `NotoSansSC-Regular.ttf` 缺失，PDF 导出 503 | **排除**。文件实际存在（19MB，`root:root 644`）。误报原因是 `.meddesk.env` 为 CRLF，取值末尾带 `\r` 使 `test -f` 失败 |

---

## 5. 建议的处理顺序

1. **先修 5 个 P0**：其中 P0-3/4/5 是"新部署装不起来"的问题，修完 Docker 与 install.sh 两条路径才算真正可用；P0-1/P0-2 决定安全承诺是否成立，要么改实现，要么改文档措辞（**不能继续宣称"服务端无法解密"**）。
2. **再修 P1-4**（带筛选的趋势接口必 500）——这是唯一会让线上用户直接看到故障的确定性 bug。
3. **P1-5 / P1-6 / P1-7** 一并处理：都属于"个人账本量级还能忍、数据一多必然劣化"，越晚改动越大。
4. P2 按模块排期，优先做 P2-29（错误响应统一）与 P2-30（导出/备份合并），它们同时降低后续改动成本。

---

## 6. 审计方法与局限

- 四路并行只读审计，**未修改任何代码**。所有 `文件:行号` 均由子审计用 Read/Grep 实际定位。
- 主审对 5 个 P0 与 8 个 P1 回读源码复核；标 `○ 待验证` 的条目为子审计报告、主审未逐条回读，落地前应再确认一次。
- 依赖 CVE（P2-15）未联网核实，建议运行 `govulncheck ./...` 取权威结论。
- 本次未做动态测试（未跑渗透、未做压测），性能量级为静态推断。
