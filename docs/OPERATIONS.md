# MedDesk v0.3.50 运维值班交接手册

> **适用版本：** v0.3.50（2026-10-30 已部署生产）  
> **项目根：** `D:\workbuddy\工作台\meddesk`  
> **生产入口：** `https://meddesk.qiubite.fun`；SSH：`web`  
> **部署形态：** 单二进制（Go embed 内嵌前端）+ `meddesk.service` + nginx；应用监听 `:8089`  
> **值班原则：** 先定位、再止损、后修复；涉及恢复、删除、覆盖前必须保留当前快照并取得负责人确认。

## 1. 监控指标与告警

> **适用环境：** 开发 / 测试 / 生产（生产必须接入持续探测与告警）  
> **适用版本：** v0.3.50

### 1.1 健康检查

| 端点 | 鉴权/限流 | 正常响应 | 判定规则 |
|---|---|---|---|
| `GET /api/v1/health` | 无鉴权，不计入业务限流 | HTTP 200；`status` 固定为 `ok` | 仅判定进程可响应；即使数据库或缓存不可用仍可能返回 200，必须同时检查 `db`、`cache` |
| `GET /api/v1/ready` | 无鉴权，不计入业务限流 | HTTP 200；数据库正常时 `status=ok` 或缓存降级时 `status=degraded` | 数据库不可用时 HTTP 503、`status=down`；缓存不可用不会阻断流量 |

两端点响应字段相同：

```json
{
  "status": "ok",
  "version": "v0.3.50",
  "db": "ok",
  "cache": "ok",
  "serverTime": 1793318400000
}
```

字段说明：`status` 为 `ok|degraded|down`，`db`/`cache` 为 `ok|down`，`serverTime` 为 Unix 毫秒。生产探测：

```bash
curl -fsS --max-time 5 https://meddesk.qiubite.fun/api/v1/health | jq .
curl -fsS --max-time 5 https://meddesk.qiubite.fun/api/v1/ready | jq .
ssh web 'curl -fsS --max-time 3 http://127.0.0.1:8089/api/v1/ready | jq .'
```

开发/测试把地址放入变量后复用：

```bash
BASE_URL=http://127.0.0.1:8089; curl -fsS --max-time 5 "$BASE_URL/api/v1/ready" | jq .
```

### 1.2 建议采集项与阈值

| 指标 | 采集/计算 | Warning | Critical | 理由与动作 |
|---|---|---:|---:|---|
| 服务存活 | `/health` 外部探测，30 秒一次 | 连续 2 次失败 | 连续 4 次失败 | 过滤单次网络抖动；2 分钟不可用应立即处置 |
| 服务就绪 | `/ready` 外部和本机各探测 | `degraded` 持续 5 分钟 | HTTP 503 连续 2 次 | Redis 可降级；数据库不可用会阻断核心读写 |
| 版本一致性 | `/health.version` | 非 `v0.3.50` 持续 5 分钟 | 多实例版本不一致 | 发现发布遗漏、旧进程或错误上游 |
| systemd 重启 | `NRestarts` 增量 | 10 分钟 ≥ 2 | 10 分钟 ≥ 3 | 反复重启通常是配置、迁移或资源故障 |
| HTTP 5xx 比率 | 访问日志中 `status>=500` / 总请求；5 分钟窗且总请求 ≥100 | ≥1% | ≥5% | 低流量时设最小样本，避免一条错误触发；5% 已影响大量请求 |
| HTTP 4xx 比率 | `400<=status<500` / 总请求；10 分钟窗且总请求 ≥200 | ≥15% | ≥30% | 常见于客户端版本、鉴权或路由异常；需按 401/404/429 分组 |
| HTTP 429 比率 | `status=429` / 总请求；5 分钟窗且总请求 ≥100 | ≥1% | ≥5% | 当前限流为 RPS=20、Burst=60；持续 429 可能误伤正常用户 |
| 延迟 P95 | 访问日志 `latency_ms`；10 分钟窗 | ≥800 ms 持续 10 分钟 | ≥1500 ms 持续 5 分钟 | 代码将 >1 秒记为“慢请求”；阈值在用户明显感知前预警 |
| sync 冲突率 | `conflicts/(applied+conflicts)`，按 `/api/v1/sync/push` 聚合；最小 20 条变更 | >2% 持续 15 分钟 | >10% 持续 5 分钟 | 偶发多设备冲突可接受；高冲突会触发服务端高版本覆盖客户端修改 |
| 记账本备份结果 | 日志“记账本自动备份已执行”的 `success`/`failed`，并统计“备份完成/失败” | 03:00 前无当日执行记录 | `failed>0` 或“自动备份未执行” | 计划时间为 02:00 Asia/Shanghai；预留一小时给大账户上传 |
| 根分区 `/` | `df -P /` 使用率、可用空间 | ≥75% 或可用 <2 GiB | ≥85% 或可用 <1 GiB | 为日志、备份与发布保留操作空间 |
| 临时区 `/tmp` | `df -P /tmp`；若为 tmpfs 同时看容量 | ≥70% | ≥85% | 已发生 tmpfs 撑满导致 nginx 502；临时文件突增需更早预警 |

> **实现侧监控缺口：** v0.3.50 没有独立 Prometheus 指标端点；HTTP 比率/P95/备份计数需从 JSON 日志聚合。同步响应含 `applied[]`、`conflicts[]`，但服务端访问日志不记录响应体，当前无法仅靠 journald 精确计算冲突率；应由客户端遥测或后续专用计数指标补齐，禁止为采集而记录密文响应体。

快速查看服务重启和磁盘：

```bash
ssh web 'systemctl show meddesk -p ActiveState -p SubState -p NRestarts -p ExecMainStatus'
ssh web 'df -hP / /tmp; findmnt /tmp || true'
```

## 2. 日志位置与采集

> **适用环境：** 开发 / 测试 / 生产  
> **适用版本：** v0.3.50

### 2.1 位置与格式

| 环境 | 位置 | 查看方式 |
|---|---|---|
| 生产 | stdout/stderr 由 systemd 写入 journald | `ssh web 'journalctl -u meddesk -n 100 --no-pager'` |
| 开发/测试 | 启动进程的 stdout | 在运行终端查看；需要筛选时重定向到 `jq`，不要长期落含敏感信息的调试日志 |
| 生产入口层 | `/var/log/nginx/meddesk.access.log`、`/var/log/nginx/meddesk.error.log` | 仅用于区分 nginx、上游连接和应用错误 |

生产默认 `MEDDESK_LOG_FORMAT=json`，Go `slog` 每行一条 JSON。通用字段为 `time`、`level`、`msg`、`service`；访问日志 `msg` 为“访问日志”或“慢请求”，并带：

| 字段 | 含义 |
|---|---|
| `method`、`path`、`status` | HTTP 方法、路由模板、响应码；`path` 不含查询参数 |
| `latency_ms` | 应用处理耗时（毫秒）；超过 1000 ms 的请求记为 WARN“慢请求” |
| `request_id` | 请求链路 ID，同时通过响应头 `X-Request-ID` 返回 |
| `client_ip`、`user_agent` | 客户端来源与 UA；仅用于故障定位，按隐私数据控制访问与留存 |
| `error` | Gin 链上的最后错误（存在时才有） |

读取应用原始 JSON 应使用 `journalctl -o cat`；`journalctl -o json` 返回的是 journald 外层 JSON，应用日志会在 `MESSAGE` 中再次编码。

### 2.2 `request_id` 追踪

1. 从用户报错响应、浏览器 Network 面板或以下命令取得 `X-Request-ID`。
2. 在应用日志中按该值查询，核对 `path/status/latency_ms/error`。
3. 同时间窗检查 nginx error log与数据库状态。

```bash
curl -sS -D - -o /dev/null https://meddesk.qiubite.fun/api/v1/health | tr -d '\r' | grep -i '^X-Request-ID:'
ssh web "journalctl -u meddesk --since '2 hours ago' -o cat --no-pager | jq -c 'select(.request_id==\"<request_id>\")'"
ssh web 'sudo tail -n 200 /var/log/nginx/meddesk.error.log'
```

> nginx 当前 JSON 格式记录请求头中的 `$http_x_request_id`。若客户端未主动传入该头，应用生成的 ID 只出现在响应头和应用日志中，nginx access log 的 `request_id` 可能为空；此时用时间、路径、状态码和客户端来源交叉定位。

### 2.3 常用查询

最近 100 条与实时跟踪：

```bash
ssh web 'journalctl -u meddesk -n 100 -o cat --no-pager'
ssh web 'journalctl -u meddesk -f -o cat'
```

按状态码筛选（5xx、429）和汇总：

```bash
ssh web "journalctl -u meddesk --since '1 hour ago' -o cat --no-pager | jq -c 'select((.status // 0) >= 500)'"
ssh web "journalctl -u meddesk --since '1 hour ago' -o cat --no-pager | jq -c 'select(.status==429)'"
ssh web "journalctl -u meddesk --since '1 hour ago' -o cat --no-pager | jq -s 'map(select(.msg==\"访问日志\" or .msg==\"慢请求\")) | group_by(.status) | map({status:.[0].status,count:length})'"
```

按路由与消息筛选：

```bash
ssh web "journalctl -u meddesk --since today -o cat --no-pager | jq -c 'select(.path==\"/api/v1/sync/push\")'"
ssh web "journalctl -u meddesk --since today -o cat --no-pager | jq -c 'select(.msg==\"记账本备份失败\" or .msg==\"记账本自动备份未执行\")'"
ssh web "journalctl -u meddesk --since today -o cat --no-pager | jq -c 'select(.level==\"ERROR\" or .level==\"WARN\")'"
```

按时间窗口筛选与计算延迟 P95：

```bash
ssh web "journalctl -u meddesk --since '2026-10-30 10:00:00' --until '2026-10-30 10:30:00' -o cat --no-pager | jq -c 'select(.msg==\"访问日志\" or .msg==\"慢请求\")'"
ssh web "journalctl -u meddesk --since '10 minutes ago' -o cat --no-pager | jq -s 'map(select((.msg==\"访问日志\" or .msg==\"慢请求\") and (.latency_ms!=null)) | .latency_ms) | sort | if length==0 then null else .[((length*0.95|ceil)-1)] end'"
```

### 2.4 禁止记录

任何环境均不得记录或临时打印以下内容：

- 用户口令、口令哈希输入、验证码、Cookie、`Authorization`、access/refresh token。
- `MEDDESK_JWT_SECRET`、`MEDDESK_SECRETBOX_KEY`、`MEDDESK_RECOVERY_KEY`、私钥、完整数据库连接串中的口令。
- DEK、KEK、`wrappedDek` 解包后的值、WebDAV 用户名/密码或其他凭据明文。
- HTTP 请求体/响应体、健康档案或记账本业务明文、带凭据的 URL。
- `.meddesk.env` 内容、备份密钥文件内容、用户导出的明文备份内容。

只记录错误类别、状态码、计数、脱敏路径和 `request_id`。临时提高日志级别前确认不会输出上述内容，排障结束立即恢复。

## 3. 备份恢复策略

> **适用环境：** 测试 / 生产；开发环境仅用于验证脚本  
> **适用版本：** v0.3.50

### 3.1 备份对象与目标

| 对象 | 当前机制 | 建议 RPO | 保留/异地要求 |
|---|---|---:|---|
| 记账本用户备份 | 每日 02:00 Asia/Shanghai，全量 ZIP 上传到各用户 WebDAV | 24 小时 | 由用户 WebDAV 保留；每天检查运行结果 |
| PostgreSQL | `backup.sh` 执行 `pg_dump -Fc --compress=9 --no-owner --no-acl`，另存 schema SQL | 24 小时 | 本地 30 天；至少一份异地主机或对象存储副本 |
| 附件目录 | `backup.sh` 将 `MEDDESK_UPLOAD_DIR` 整体打成 `tar.gz` | 24 小时 | 与同批数据库 dump 一起保留、校验和管理 |
| `MEDDESK_SECRETBOX_KEY` | 不在数据库，必须独立保存 | 变更即备份 | 密码管理器一份 + 离线加密介质一份，限制最小授权 |

数据库中健康档案为 E2E 密文，但记账本表是明文，WebDAV 凭据为 AES-256-GCM 密文；附件目录中的 `ledger/` 是明文凭证图片。因此数据库 dump、附件 tar 和密钥都按高敏数据保护，备份目录权限必须为 `0700`，异地链路和落盘必须加密。

### 3.2 记账本自动备份

- 调度：进程内调度器每日 `02:00`（默认 `MEDDESK_LEDGER_BACKUP_TIME`），时区 `Asia/Shanghai`（默认 `MEDDESK_LEDGER_BACKUP_TZ`）。
- 格式：`ledger-backup-YYYY-MM-DD.zip`，含 `ledger.json`、`manifest.json`、`images/…`。
- 凭据：`base_url/username/password/path_prefix` 分字段使用 `MEDDESK_SECRETBOX_KEY`（base64 编码的 32 字节密钥）做 AES-256-GCM 加密后存库；接口不回传凭据。
- 幂等：文件按日期同名 PUT 覆盖；`ledger_backup_runs` 对 `(user_id, run_date)` 唯一并 upsert。
- 完成条件：PUT 成功后必须再调用 WebDAV `Exists`；只有远端存在才记 `success`。
- 单账号最长执行 10 分钟；单进程内串行。多实例之间没有选主或分布式锁，会重复上传并覆盖同名文件。
- 恢复限制：可从 ZIP 取出 `ledger.json` 走记账本导入；v0.3.50 暂不能把 ZIP 中 `images/` 自动导入还原，不能把“账单已恢复”等同于“凭证图片已恢复”。

每日检查：

```bash
ssh web "journalctl -u meddesk --since 'today 01:50' -o cat --no-pager | jq -c 'select(.msg==\"记账本自动备份已执行\" or .msg==\"记账本自动备份未执行\" or .msg==\"记账本备份失败\" or .msg==\"记账本备份完成\")'"
```

### 3.3 数据库与附件备份（按脚本真实行为）

脚本：`deploy/scripts/backup.sh`。支持 `--db-only`、`--files-only`、`--dest`、`--upload-dir`、`--db-url`、`--keep-days`、`--remote`、`--remote-type rsync|rclone`、`--dry-run`。默认目标为 `/var/backups/meddesk`、保留 30 天；每次生成时间戳目录、`MANIFEST.txt` 和 `SHA256SUMS`。

生产首次准备：

```bash
ssh web 'sudo install -d -o ops -g ops -m 0700 /var/backups/meddesk'
ssh web 'test -x /home/ops/service/meddesk/scripts/backup.sh && test -x /home/ops/service/meddesk/scripts/restore.sh'
```

先预演，再执行全量备份：

```bash
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/backup.sh --dest /var/backups/meddesk --keep-days 30 --dry-run'"
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/backup.sh --dest /var/backups/meddesk --keep-days 30'"
ssh web 'LATEST=$(find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort -r | head -n1); cd "/var/backups/meddesk/$LATEST" && sha256sum -c SHA256SUMS'
```

只备份单类对象或启用异地复制：

```bash
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/backup.sh --dest /var/backups/meddesk --db-only'"
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/backup.sh --dest /var/backups/meddesk --files-only'"
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/backup.sh --dest /var/backups/meddesk --remote \"<异地目标>\" --remote-type rsync'"
```

附件策略：

1. 始终备份整个 `/home/ops/service/meddesk/data`，不要只匹配 `.bin`；`ledger/` 下存在明文图片。
2. 数据库和附件应在同一低峰窗口备份，并保留相同时间戳。脚本打包期间文件变化只告警、不失败，且 DB/附件不是原子快照；严格一致性恢复点应先短暂停写或使用云盘/文件系统快照。
3. 每周检查 tar 可列目录：

```bash
ssh web 'LATEST=$(find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort -r | head -n1); tar -tzf /var/backups/meddesk/$LATEST/meddesk-uploads-*.tar.gz >/dev/null && echo OK'
```

> `backup.sh` 将 rsync/rclone 失败按“非致命”告警处理，脚本仍可能成功退出；异地副本必须独立校验，不能只看脚本退出码。脚本当前把完整 DB URL 传给 `pg_dump --dbname`，即使同时设置 `PGPASSWORD`，本机进程参数仍可能短暂暴露连接串；只允许受控主机用户查看进程，并安排后续实现修正。

### 3.4 恢复流程

脚本：`deploy/scripts/restore.sh`。恢复 DB 使用 `pg_restore --clean --if-exists --single-transaction --exit-on-error`，会覆盖同名数据库对象；附件 tar 解回上传目录并覆盖同名文件。**生产恢复必须先获负责人确认，先备份当前状态，禁止直接使用 `--force` 或 `--skip-checksum`。**

1. 列出备份并做 dry-run：

```bash
ssh web 'sudo -u ops /home/ops/service/meddesk/scripts/restore.sh --dest /var/backups/meddesk --list'
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/restore.sh --dest /var/backups/meddesk --from latest --dry-run'"
```

2. 对当前状态做最后快照并校验：

```bash
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/backup.sh --dest /var/backups/meddesk --keep-days 30'"
ssh web 'LATEST=$(find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort -r | head -n1); cd "/var/backups/meddesk/$LATEST" && sha256sum -c SHA256SUMS'
```

3. 停服务，交互式恢复指定时间戳；按提示输入时间戳和 `yes`：

```bash
ssh -t web 'sudo systemctl stop meddesk && sudo systemctl is-active meddesk || true'
ssh -t web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/scripts/restore.sh --dest /var/backups/meddesk --from <YYYYMMDD-HHMMSS>'"
```

4. 启动并验证：

```bash
ssh web 'sudo systemctl start meddesk && systemctl is-active meddesk'
curl -fsS https://meddesk.qiubite.fun/api/v1/ready | jq -e '.status=="ok" or .status=="degraded"'
curl -fsS https://meddesk.qiubite.fun/api/v1/health | jq -e '.version=="v0.3.50" and .db=="ok"'
```

如仅恢复 DB 或附件，分别追加 `--db-only`、`--files-only`；仍必须先 dry-run 和快照。恢复完成后抽查用户登录、同步、记账本及附件读取，不要只看健康端点。

### 3.5 `MEDDESK_SECRETBOX_KEY` 与演练

- 值必须是 `<base64 32 字节密钥>`；只放 `.meddesk.env`、受控密码管理器和离线加密介质，不放代码库、工单、聊天、日志或普通备份目录。
- 生产 `.meddesk.env` 保持 `root:ops 0640`；备份密钥至少双副本、分离保管，并记录创建/轮换日期，不记录明文到本手册。
- **丢失或错误轮换后果：** 数据库中的既有 WebDAV 凭据密文永久无法解密，所有已配置用户的自动备份将失败；只能让每位用户重新填写并保存 WebDAV 凭据。它不会解密或恢复 E2E 健康档案，也不能替代用户自己的 DEK/口令备份。
- 轮换前必须设计“旧密钥解密、用新密钥重加密”的迁移；v0.3.50 没有自动轮换流程，禁止直接替换后重启。

演练频率：每月在隔离测试环境恢复最新 DB+附件并做业务冒烟；每季度完成一次含异地副本、密钥取用、记账本导入的全链路演练；每次迁移、存储方案或密钥管理变更后追加演练。演练目标建议：RPO ≤24 小时、RTO ≤2 小时，并记录备份时间戳、校验结果、恢复耗时与未恢复项（特别是 ZIP `images/`）。

## 4. 常见故障排查手册（Runbook）

> **适用环境：** 测试 / 生产；开发环境可复用定位思路  
> **适用版本：** v0.3.50

开始处置前先记录故障起止时间、影响范围、最近变更和一个 `request_id`。禁止在工单中粘贴 `.meddesk.env`、请求/响应体或任何凭据。命令中的 `<...>` 必须替换为当次故障的受控值；不得把替换后的敏感命令写入共享记录。

### 4.1 服务起不来 / 反复重启

**现象**

- 公网 502；`meddesk.service` 为 `failed/activating`；日志反复出现配置、数据库、迁移或 HTTP 监听失败。
- 5 分钟内失败超过 5 次后出现 `start-limit-hit`。

**快速定位命令**

```bash
ssh web 'systemctl status meddesk --no-pager -l; systemctl show meddesk -p ActiveState -p SubState -p Result -p NRestarts -p ExecMainStatus'
ssh web 'sudo journalctl -u meddesk -b -n 200 -o cat --no-pager'
ssh web 'sudo systemctl cat meddesk; sudo systemd-analyze verify /etc/systemd/system/meddesk.service'
ssh web 'sudo ss -ltnp | grep -E ":8089[[:space:]]" || true; sudo -u ops /home/ops/service/meddesk/meddesk -version'
ssh web 'sudo -u ops test -r /home/ops/service/meddesk/.meddesk.env && sudo -u ops test -x /home/ops/service/meddesk/meddesk && echo FILES_OK'
```

**根因**

1. `.meddesk.env` 缺失/权限错误，或必填配置无效（常见为 JWT、DB URL）。
2. PostgreSQL 不可达；`MEDDESK_DB_MIGRATE_ON_BOOT=true` 时迁移失败会让进程退出。
3. `:8089` 被其他进程占用；二进制架构/执行权限错误。
4. systemd 加固项与宿主机内核不兼容（日志含 `SIGSYS`/`EPERM`），或启动限流生效。

**处置步骤**

1. 根据第一条明确错误修配置、数据库、权限或端口；不要靠连续重启掩盖根因，不要直接终止未知占端口进程。
2. 若改了 unit：`ssh web 'sudo systemctl daemon-reload'`；只改环境文件不需要 daemon-reload。
3. 原因消除后重置失败计数并启动：

```bash
ssh web 'sudo systemctl reset-failed meddesk; sudo systemctl start meddesk'
```

4. 只有日志明确指向 systemd 沙箱兼容性时，才在变更审批后临时放宽对应项验证；不得整体移除安全加固。

**验证**

```bash
ssh web 'systemctl is-active meddesk; sudo ss -ltnp | grep -E ":8089[[:space:]]"'
ssh web 'curl -fsS http://127.0.0.1:8089/api/v1/ready | jq .'
curl -fsS https://meddesk.qiubite.fun/api/v1/health | jq -e '.version=="v0.3.50" and .db=="ok"'
```

### 4.2 数据库连不上或迁移失败

**现象**

- 启动日志为“数据库连接失败”或“数据库迁移失败”；`/ready` 返回 503、`db=down`。

**快速定位命令**

```bash
ssh web 'pg_isready -h 127.0.0.1 -p 5432 || true'
ssh web 'sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml ps postgres'
ssh web 'sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml logs --tail=150 postgres'
ssh web "sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml exec -T postgres psql -U '<数据库用户>' -d '<数据库名>' -c \"SELECT version,applied_at FROM schema_migrations ORDER BY version;\""
ssh web 'df -hP / /home/ops/service/docker/meddesk; df -ihP / /home/ops/service/docker/meddesk'
```

**根因**

- PostgreSQL 容器未启动/不健康、5432 未监听、磁盘满、连接串或口令不一致。
- 迁移账户缺 DDL 权限、迁移 SQL 失败，或另一实例持有 advisory lock。
- 数据库已部分人工修改，与内嵌迁移预期不一致。

**处置步骤**

1. 先恢复 PostgreSQL 健康；核对配置只检查变量是否存在和目标主机/端口，禁止输出完整连接串。
2. 迁移在单事务和 advisory lock 下执行，失败会回滚。不得手工插入 `schema_migrations`、不得自动执行 `.down.sql`。
3. 有最新备份且确认错误原因后，停应用并显式重跑迁移：

```bash
ssh web 'sudo systemctl stop meddesk'
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; /home/ops/service/meddesk/meddesk -migrate'"
ssh web 'sudo systemctl start meddesk'
```

4. 若是 schema 漂移或迁移 SQL 本身失败，保留日志和当前 dump，升级给开发处理，不在生产即兴改表。

**验证**

```bash
ssh web 'curl -fsS http://127.0.0.1:8089/api/v1/ready | jq -e '\''.db=="ok"'\'''
ssh web 'sudo journalctl -u meddesk --since "10 minutes ago" -o cat --no-pager | grep -E "数据库迁移完成|数据库连接失败|数据库迁移失败"'
```

### 4.3 HTTP 5xx 激增

**现象**

- 5 分钟 5xx ≥1%，用户集中报保存、同步、附件或导出失败。

**快速定位命令**

```bash
ssh web "sudo journalctl -u meddesk --since '15 minutes ago' -o cat --no-pager | jq -s 'map(select((.status // 0)>=500)) | group_by(.path) | map({path:.[0].path,count:length}) | sort_by(-.count)'"
ssh web "sudo journalctl -u meddesk --since '15 minutes ago' -o cat --no-pager | jq -c 'select(.level==\"ERROR\" or ((.status // 0)>=500))'"
ssh web 'curl -sS http://127.0.0.1:8089/api/v1/ready | jq .; sudo tail -n 100 /var/log/nginx/meddesk.error.log'
ssh web 'systemctl show meddesk -p MemoryCurrent -p MemoryPeak -p TasksCurrent; df -hP / /tmp'
```

**根因**

- 数据库/连接池异常、磁盘或 `/tmp` 满、附件目录权限错误。
- WebDAV 上游失败、PDF 字体缺失、单个接口数据异常或发布回归。
- nginx 到 `127.0.0.1:8089` 连接失败时通常表现为 502，应用访问日志可能没有对应请求。

**处置步骤**

1. 先按路径和 `request_id` 缩小到单模块；对比外部请求与本机直连，确定 nginx 还是应用。
2. 依根因转对应 Runbook；不要无证据重启数据库或删除数据。
3. 若与刚发布强相关，冻结继续发布，保存当前日志和备份；回退二进制前确认数据库迁移向后兼容并取得负责人批准。

**验证**

```bash
curl -fsS https://meddesk.qiubite.fun/api/v1/ready | jq .
ssh web "sudo journalctl -u meddesk --since '10 minutes ago' -o cat --no-pager | jq -s '[.[] | select((.status // 0)>=500)] | length'"
```

10 分钟内无新增同类 5xx，并完成受影响业务的最小读写冒烟后关闭故障。

### 4.4 磁盘或 `/tmp` 写满（真实事故：tmpfs 撑满导致 502）

**现象**

- nginx 502，日志含 `No space left on device`、临时文件写入失败；`/tmp` 或根分区使用率 100%。

**快速定位命令**

```bash
ssh web 'df -hP / /tmp; df -ihP / /tmp; findmnt -T /tmp -o TARGET,SOURCE,FSTYPE,SIZE,USED,AVAIL,OPTIONS'
ssh web 'sudo du -xhd1 /var/log /var/backups /tmp 2>/dev/null | sort -h'
ssh web 'sudo find /tmp -xdev -type f -printf "%s %TY-%Tm-%Td %p\n" 2>/dev/null | sort -nr | head -30'
ssh web 'sudo lsof +L1; sudo nginx -T 2>/dev/null | grep -E "(_temp_path|client_body_buffer_size)" || true'
ssh web 'sudo journalctl -u meddesk -u nginx --since "30 minutes ago" --no-pager | grep -Ei "no space|tmp|write|502"'
```

**根因**

- `/tmp` 挂载为容量较小的 tmpfs，被 nginx 请求体/代理临时文件、导出或其他进程占满。
- journald、备份、Docker 日志或已删除但仍被进程占用的文件占满根分区。
- inode 耗尽时容量看似尚有剩余，但仍无法创建文件。

**处置步骤**

1. 先识别写入进程并停止新增流量/任务；不得执行无范围的 `rm -rf /tmp/*`。
2. 对已确认不再使用的单个临时文件执行：

```bash
ssh web "sudo rm -- '<已确认的过期临时文件路径>'"
```

3. `lsof +L1` 若显示大文件已删除但仍占用，优雅重启持有它的**单个**进程释放空间。
4. 根分区满时按保留策略清理过期备份/日志；先列清单再删。tmpfs 容量调整须评估内存余量并走变更审批。
5. 若 nginx 配置把大请求缓冲到 `/tmp`，修正为有水位监控的持久目录或提高受控 tmpfs 容量；不要只重启来临时清空。

**验证**

```bash
ssh web 'df -hP / /tmp; df -ihP / /tmp; curl -fsS http://127.0.0.1:8089/api/v1/ready | jq .'
curl -fsS https://meddesk.qiubite.fun/api/v1/ready | jq .
```

确认 `/tmp` <70%、`/` <75%，公网连续探测正常且 nginx 不再产生 502。

### 4.5 前端白屏 / 资源 404 / 错误 Content-Type

**现象**

- 页面白屏或点击无反应；Console 报脚本 MIME 错误/404。
- 已知事故：`/admin/app.js` 被中间层缓存为 `text/html`，脚本未执行。

**快速定位命令**

```bash
curl -sS -D - -o /dev/null "https://meddesk.qiubite.fun/admin/app.js?diag=$(date +%s)"
curl -sS -D - -o /dev/null "https://meddesk.qiubite.fun/admin?diag=$(date +%s)"
ssh web 'curl -sS -D - -o /dev/null -H "Host: meddesk.qiubite.fun" "http://127.0.0.1:8089/admin/app.js?diag=$(date +%s)"'
ssh web 'sudo nginx -T 2>/dev/null | grep -nE "admin|proxy_cache|try_files|error_page|8089"'
curl -fsS https://meddesk.qiubite.fun/ | grep -Eo 'md-(build-id|version)" content="[^"]+' | head
```

**根因**

- 中间层缓存了 SPA fallback 的 HTML，并用相同 URI 回给 JS；或 nginx location/重写顺序错误。
- 发布的二进制不含目标资源、Service Worker/浏览器仍持有旧缓存，或 `/assets/` 指纹与内容不匹配。
- `X-Content-Type-Options: nosniff` 会正确阻止 `text/html` 冒充 JavaScript。

**处置步骤**

1. 比较公网和 `127.0.0.1:8089` 响应。直连正确而公网错误，处理 nginx/CDN/网关缓存；两者都错则核对二进制版本和内嵌资源。
2. `/admin` 应为 `text/html`；`/admin/app.js` 必须为 `application/javascript`，且带 `Cache-Control: no-store, no-cache, must-revalidate, max-age=0`。清除中间层中该 URI 的错误缓存，并配置不缓存 `/admin`、`/admin/`、`/admin/app.js`、`/index.html`、`/sw.js` 和错误响应。
3. nginx 修改后先校验再重载：

```bash
ssh web 'sudo nginx -t && sudo systemctl reload nginx'
```

4. 服务端正确后，让受影响浏览器硬刷新；PWA 主站仍旧时再在 DevTools 的 Application 中注销 Service Worker、清站点 Cache Storage 后重开。不要先清用户 IndexedDB 数据。

**验证**

```bash
curl -fsS https://meddesk.qiubite.fun/api/v1/health | jq -e '.version=="v0.3.50"'
curl -sS -D - -o /dev/null https://meddesk.qiubite.fun/admin/app.js | grep -Ei 'HTTP/|content-type|cache-control'
```

浏览器 Console 无 MIME/404/CSP 错误，后台登录按钮和一次只读查询可正常执行。

### 4.6 同步冲突 / 数据被回退

**现象**

- 修改后短时间又变回旧值；同步提示“其他设备更新”；真实事故为服务端高 `ver` 的陈旧副本压回改名。

**快速定位命令**

```bash
ssh web "sudo journalctl -u meddesk --since '2 hours ago' -o cat --no-pager | jq -c 'select(.path==\"/api/v1/sync/push\" or .path==\"/api/v1/sync/pull\")'"
ssh web "sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml exec -T postgres psql -U '<数据库用户>' -d '<数据库名>' -c \"SELECT table_name,record_id,ver,updated_at,deleted,device_id,seq,updated_seq FROM sync_records WHERE user_id='<用户 UUID>' AND table_name='<逻辑表名>' AND record_id='<记录 UUID>';\""
curl -fsS https://meddesk.qiubite.fun/api/v1/health | jq -r '.version'
```

同时在受影响浏览器 Network 中查看 `/api/v1/sync/push` 的 `applied[]/conflicts[]` 元数据；不得复制或记录 `iv/ct`。服务端无法解密内容，最终值必须由用户在客户端确认。

**根因**

- 服务端按 `ver`、再按 `updatedAt` 裁决；旧客户端曾反复重试并把陈旧记录 `ver` 抬高。
- 多设备同时编辑同一记录；旧缓存仍运行历史版本前端；客户端时钟异常。
- v0.3.50 客户端遇到“本地脏数据较新、远端 `ver` 更高”时会把本地 `ver` 提到远端 +1 后只重试一次；若仍回退，需保留现场升级。

**处置步骤**

1. 立即暂停其他设备同步（关闭页面或断网），只保留用户确认内容正确的权威设备。
2. 在每台可能含正确内容的设备先执行客户端“导出 JSON 备份”；不得先清 IndexedDB、重装 PWA 或调用同步 reset。
3. 确认权威设备已加载 v0.3.50；若为旧前端，按 4.5 清静态缓存但保留 IndexedDB。
4. 在权威设备重新编辑并保存目标记录，触发同步；观察一次 push 的 `applied`，再在另一台设备拉取确认。
5. 若仍循环冲突，保留两端导出和上述元数据，升级开发处理。`POST /api/v1/sync/reset` 会清空该用户全部云端同步记录，仅在有完整客户端导出、用户确认和负责人批准时作为最后手段。

**验证**

- 数据库元数据中目标记录 `ver` 只向前推进，`updated_seq` 更新；push 不再持续返回该记录冲突。
- 逐台恢复联网，至少两台设备显示用户确认的同一值；等待两个 60 秒自动同步周期后不再回退。

### 4.7 备份失败

**现象**

- 02:00 后出现“记账本备份失败/自动备份未执行”，或运行状态长期为 `running/failed`。

**快速定位命令**

```bash
ssh web "sudo journalctl -u meddesk --since 'today 01:50' -o cat --no-pager | jq -c 'select(.msg|test(\"备份\"))'"
ssh web 'df -hP / /tmp /var/backups/meddesk; getent hosts <WebDAV 主机名> || true'
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; python3 -c \"import os,base64; assert len(base64.b64decode(os.environ[\\\"MEDDESK_SECRETBOX_KEY\\\"],validate=True))==32; print(\\\"KEY_FORMAT_OK\\\")\"'"
ssh web "sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml exec -T postgres psql -U '<数据库用户>' -d '<数据库名>' -c \"SELECT run_date,status,count(*) FROM ledger_backup_runs GROUP BY run_date,status ORDER BY run_date DESC LIMIT 14;\""
```

**根因**

- `MEDDESK_SECRETBOX_KEY` 缺失、格式错误或被直接轮换，导致既有凭据无法解密。
- 用户 WebDAV 口令变更、权限/配额不足、TLS/DNS/网络异常，或 PUT 后 `Exists` 回查失败。
- 附件读取异常、账号数据过大超过 10 分钟；进程在 `running` 状态时重启。
- 多副本无选主，重复上传/覆盖或运行记录互相覆盖。

**处置步骤**

1. 先按日志 `message` 分类；禁止打印凭据、完整 URL 或密钥。
2. 密钥错误时先恢复原密钥；若已丢失，只能通知用户重新保存 WebDAV 凭据，不得生成新密钥冒充旧密钥。
3. WebDAV 认证/配额问题由用户在设置页重新保存凭据（保存时会 Ping 并创建目录）。修复 DNS、TLS或出网后，让用户点击手动备份。
4. 附件问题先修文件权限/缺失；多副本问题退回单副本，完成跨实例选主前不得并行调度。

**验证**

```bash
ssh web "sudo journalctl -u meddesk --since '30 minutes ago' -o cat --no-pager | jq -c 'select(.msg==\"记账本备份完成\" or .msg==\"记账本备份失败\")'"
```

状态为 `success`、`failed=0`，且日志确认上传后远端存在；必要时由用户在 WebDAV 端核对当日同名 ZIP 可下载。

### 4.8 限流 429 误伤正常用户

**现象**

- 正常操作收到 429；同一办公网/NAT 下多人同时失败；响应带 `Retry-After`。

**快速定位命令**

```bash
ssh web "sudo journalctl -u meddesk --since '15 minutes ago' -o cat --no-pager | jq -s 'map(select(.status==429)) | group_by([.client_ip,.path]) | map({client_ip:.[0].client_ip,path:.[0].path,count:length}) | sort_by(-.count)'"
ssh web "sudo jq -s 'map(select(.status==429)) | group_by([.remote_addr,.uri]) | map({remote_addr:.[0].remote_addr,uri:.[0].uri,count:length}) | sort_by(-.count)' /var/log/nginx/meddesk.access.log"
ssh web 'sudo tail -n 100 /var/log/nginx/meddesk.error.log | grep -i "limiting requests" || true'
ssh web 'sudo nginx -T 2>/dev/null | grep -nE "limit_req(_zone|_status)?|real_ip|set_real_ip_from"'
```

**根因**

- 应用限流为每 IP RPS=20/Burst=60；nginx 还有限制，普通 API 10r/s、鉴权和重接口更严格。
- nginx/应用未还原真实客户端 IP，所有用户被聚合到代理 IP；NAT 用户共享同一桶。
- 前端重试循环、批量操作或异常客户端产生突发。

**处置步骤**

1. 先区分层次：应用 429 会有 JSON `code=rate_limited` 和应用访问日志；只有 nginx 429 时应用日志没有对应请求，nginx error log通常有 `limiting requests`。
2. 让客户端遵守 `Retry-After`，停止自动重试风暴；封禁明显异常来源时保留证据。
3. 若前方有 CDN/负载均衡，只信任 `<可信代理网段>` 配置 nginx `set_real_ip_from`，再向应用传递正确 XFF；应用仅应信任本机 nginx，禁止信任 `0.0.0.0/0`。
4. 确认是正常业务量后，小步提高**对应层**阈值；应用改 `MEDDESK_RATE_LIMIT_RPS/BURST` 需重启，nginx 改 zone rate 需 `nginx -t` 后 reload。不得整体关闭鉴权限流。

**验证**

```bash
ssh web 'sudo nginx -t; systemctl is-active meddesk nginx'
ssh web "sudo journalctl -u meddesk --since '10 minutes ago' -o cat --no-pager | jq -s '[.[] | select(.status==429)] | length'"
```

正常操作连续通过，429 比率低于 1%，日志中的 `client_ip` 不再全部相同；同时确认异常突发仍会触发 429。

## 5. 扩容指引

> **适用环境：** 测试 / 生产（开发环境仅做方案验证）  
> **适用版本：** v0.3.50

### 5.1 决策顺序

1. 先确认瓶颈：CPU、内存、磁盘 IOPS、数据库连接/慢 SQL、外部 WebDAV，禁止仅因单次峰值扩容。
2. 优先垂直扩容单实例；v0.3.50 的本地附件与进程内备份调度天然适合单实例。
3. 只有垂直扩容仍不足，且 5.2 的全部前置条件已完成，才允许多副本。

基线采集：

```bash
ssh web 'uptime; free -h; df -hP / /tmp; vmstat 1 5'
ssh web 'systemctl show meddesk -p CPUUsageNSec -p MemoryCurrent -p MemoryPeak -p TasksCurrent -p NRestarts'
ssh web "sudo journalctl -u meddesk --since '1 hour ago' -o cat --no-pager | jq -s 'map(select(.latency_ms!=null)|.latency_ms) | sort | if length==0 then null else {p50:.[((length*0.50|ceil)-1)],p95:.[((length*0.95|ceil)-1)],max:.[-1]} end'"
```

### 5.2 垂直扩容

1. 扩容前执行全量备份并校验，记录 `/health.version`、P95、5xx、内存峰值和磁盘水位。
2. ECS 升配后复核 CPU/内存；云盘扩容后还必须扩文件系统并用 `df -hP` 确认容量已生效。
3. 若增加 PostgreSQL 容器内存，同步调整 compose 资源限制与 PostgreSQL `shared_buffers/effective_cache_size/work_mem`；不要只提高容器上限。
4. 应用本身无 worker 数配置；单进程会使用 Go 调度器。先观察扩容后数据，再决定是否调连接池。
5. 重启后按本章 5.5 验证；若指标无改善，回到数据库、磁盘或外部依赖定位。

### 5.3 多副本前置条件（全部必须完成）

| 前置条件 | v0.3.50 当前状态 | 扩容前要求 |
|---|---|---|
| 备份调度选主 | **未实现**；每个实例都会在 02:00 执行 `RunAll` | **必须先解决**。推荐在整个 `RunAll` 周期持有 PostgreSQL session advisory lock，或把调度拆成唯一外部任务；验证每天只触发一次 |
| 附件存储 | 本机 `/home/ops/service/meddesk/data` | 改为所有副本一致可见的共享持久存储，或完成对象存储改造；同时保护 `ledger/` 明文图片 |
| 缓存/限流状态 | Redis 可选；关闭时为单进程内状态 | 多副本必须启用同一 Redis，统一限流、验证码/失败计数和会话相关缓存；验证 Redis 故障降级行为 |
| 数据库迁移 | 已用 PostgreSQL advisory transaction lock 串行 | 可保留开机迁移，但发布前先独立执行一次 `-migrate`，避免所有副本等待迁移 |
| 密钥与配置 | 单机环境文件 | 所有副本使用同一 `MEDDESK_SECRETBOX_KEY`、JWT/恢复密钥与业务配置；通过密钥管理系统分发，不复制到镜像 |
| 版本 | 单二进制 | 同一流量池必须是相同二进制、相同前端 build ID；健康检查确认 `version=v0.3.50` |
| 负载均衡 | nginx → 单上游 `127.0.0.1:8089` | 配置 `/ready` 摘除、连接超时、真实 IP 信任链和优雅下线；不得用健康端点判断数据库不可用时仍可接流量 |

> **禁止直接加副本：** 幂等约束只能避免同一天产生多行/多个文件，不能阻止多实例重复打包、占用带宽、同名覆盖和运行记录互相覆盖。选主未完成前只能运行一个启用记账本调度的实例。

建议实施顺序：

1. 在测试环境实现并验证调度选主；强杀 leader 后，下一实例能在下一调度周期接管，且同一周期不会双跑。
2. 迁移并核对共享附件：数据库元数据与文件数量/校验一致，所有副本可读写。
3. 启用共享 Redis；确认 nginx 和应用看到的 `client_ip` 正确。
4. 按 5.4 预算连接池，配置 LB `/ready` 探针。
5. 用隔离测试域名做候选版本验证；不要在生产流量池内长时间混跑不同 build ID。
6. 一次切齐同版本副本，验证 5xx、P95、429、附件和备份单次执行后再扩大流量。

### 5.4 数据库连接池

配置项：`MEDDESK_DB_MAX_CONNS`（默认 10）、`MEDDESK_DB_MIN_CONNS`（默认 1）、`MEDDESK_DB_MAX_CONN_LIFETIME`（默认 30 分钟）。连接池值按**每个实例**生效。

预算规则：

```text
全部应用 MaxConns 之和 ≤ PostgreSQL max_connections 的 70%
每实例 MaxConns = floor((max_connections × 0.70) / 应用副本数)
全部应用 MinConns 之和 ≤ PostgreSQL max_connections 的 25%
```

至少为数据库管理、迁移、备份和应急连接预留 20 个连接；以上两条取更保守值。低流量从 `MaxConns=10/MinConns=1` 起，不因增加 CPU 盲目增加连接。等待连接和 P95 同时升高、数据库仍有 CPU/IO 余量时再小步调整。

查看当前设置与连接：

```bash
ssh web "sudo grep -E '^MEDDESK_DB_(MAX|MIN)_CONNS=' /home/ops/service/meddesk/.meddesk.env || true"
ssh web "sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml exec -T postgres psql -U '<数据库用户>' -d '<数据库名>' -c \"SHOW max_connections; SELECT state,count(*) FROM pg_stat_activity GROUP BY state ORDER BY state;\""
```

修改 `.meddesk.env` 后逐实例重启并观察；生产不得同时重启全部副本。若数据库连接已接近上限，先减小池或治理慢 SQL，不要继续加副本。

### 5.5 静态资源与 PWA 缓存

| 资源 | 正确策略 | 扩容/发布注意 |
|---|---|---|
| `/index.html`、`/sw.js` | `no-cache, must-revalidate`；nginx 可更严格 `no-store` | 每次必须回源校验；LB/CDN 不得缓存错误页 |
| `/manifest.webmanifest` | `no-cache` + ETag | 不得由 SPA fallback 代替 |
| `/assets/*?v=<buildID>` | 一年 `immutable` | 缓存键必须包含查询串，禁止剥离 `v`；只有同 build ID 副本才能同池 |
| `/admin`、`/admin/`、`/admin/app.js` | `no-store, no-cache` | 禁止中间层缓存；JS 必须保持 `application/javascript` |

v0.3.50 的静态文件路径本身不含内容哈希，build ID 位于查询串。若滚动期间 A 返回 HTML/`v=A`，资源请求却落到 B，B 会在 `v=A` URL 下返回 B 的内容，中间层还可能永久缓存错误组合；不同副本的 `sw.js` 也会让 PWA 缓存来回切换。因此多副本发布必须保持流量池版本一致，或先实现内容寻址文件名/按版本流量隔离后再滚动混跑。

扩容或发布后验证：

```bash
for i in 1 2 3 4 5; do curl -fsS https://meddesk.qiubite.fun/api/v1/health | jq -r '[.version,.status,.db,.cache]|@tsv'; done
curl -sS -D - -o /dev/null https://meddesk.qiubite.fun/index.html | grep -Ei 'HTTP/|cache-control|etag|content-type'
curl -sS -D - -o /dev/null https://meddesk.qiubite.fun/sw.js | grep -Ei 'HTTP/|cache-control|etag|content-type'
curl -sS -D - -o /dev/null https://meddesk.qiubite.fun/admin/app.js | grep -Ei 'HTTP/|cache-control|content-type'
```

验收条件：所有响应版本一致；`/ready` 全部正常；P95/5xx/429 不劣化；跨副本附件可读；02:00 备份只执行一次且结果成功。

## 6. 日常巡检清单

> **适用环境：** 生产为主；表内标注测试/开发项  
> **适用版本：** v0.3.50

命令中的 `<数据库用户>`、`<数据库名>`、`<测试 SSH 别名>` 按受控配置替换；不得把口令写入命令历史或巡检记录。异常项立即转第 4 章，不要只登记不处置。

### 6.1 日巡检（建议 09:30，备份任务完成后）

| ID | 环境 | 巡检项 | 命令 | 正常判据 |
|---|---|---|---|---|
| D1 | 生产 | 服务状态/重启 | `ssh web 'systemctl show meddesk -p ActiveState -p SubState -p NRestarts -p ExecMainStatus'` | `active/running`、退出码 0；`NRestarts` 无异常增长 |
| D2 | 生产 | 外部与本机就绪 | `curl -fsS https://meddesk.qiubite.fun/api/v1/ready \| jq .`<br>`ssh web 'curl -fsS http://127.0.0.1:8089/api/v1/ready \| jq .'` | HTTP 200，`status=ok`、`db=ok`、版本 `v0.3.50`；Redis 明确降级时可暂为 `degraded` |
| D3 | 生产 | 5xx/429/延迟 | 执行下方“日指标汇总”命令 | 5xx <1%、429 <1%、P95 <800 ms；4xx 无突增 |
| D4 | 生产 | 磁盘与 inode | `ssh web 'df -hP / /tmp; df -ihP / /tmp'` | `/` <75%、`/tmp` <70%、inode <75%，可用空间分别高于 2 GiB/水位预留 |
| D5 | 生产 | 记账本 02:00 备份 | `ssh web "sudo journalctl -u meddesk --since 'today 01:50' -o cat --no-pager \| jq -c 'select(.msg\|test(\"备份\"))'"` | 03:00 后有“自动备份已执行”；`failed=0`，无“未执行/备份失败” |
| D6 | 生产 | PostgreSQL 容器 | `ssh web 'sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml ps postgres'` | `running (healthy)`；无反复重启 |
| D7 | 生产 | nginx 入口错误 | `ssh web 'sudo tail -n 200 /var/log/nginx/meddesk.error.log'` | 无新增 upstream、磁盘、TLS、限流异常；无持续 502 |
| D8 | 生产 | 最近 DB/附件备份 | `ssh web 'LATEST=$(find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f\n" \| sort -r \| head -n1); echo "$LATEST"; test -n "$LATEST" && test -s "/var/backups/meddesk/$LATEST/SHA256SUMS"'` | 按每日计划执行时，最新时间戳不超过 26 小时，`SHA256SUMS` 非空 |

日指标汇总：

```bash
ssh web "sudo journalctl -u meddesk --since '24 hours ago' -o cat --no-pager | jq -s 'map(select(.msg==\"访问日志\" or .msg==\"慢请求\")) as \$r | (\$r|length) as \$n | (\$r|map(.latency_ms)|sort) as \$l | {total:\$n,pct_5xx:(if \$n==0 then 0 else ([\$r[]|select((.status//0)>=500)]|length)*100/\$n end),pct_4xx:(if \$n==0 then 0 else ([\$r[]|select((.status//0)>=400 and (.status//0)<500)]|length)*100/\$n end),pct_429:(if \$n==0 then 0 else ([\$r[]|select(.status==429)]|length)*100/\$n end),p95_ms:(if (\$l|length)==0 then null else \$l[((\$l|length)*0.95|ceil)-1] end)}'"
```

### 6.2 周巡检（建议每周一）

| ID | 环境 | 巡检项 | 命令 | 正常判据 |
|---|---|---|---|---|
| W1 | 生产 | 最新备份校验和 | `ssh web 'LATEST=$(find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f\n" \| sort -r \| head -n1); cd "/var/backups/meddesk/$LATEST" && sha256sum -c SHA256SUMS'` | 所有条目 `OK`，无缺失/篡改 |
| W2 | 生产 | 附件 tar 可读 | `ssh web 'LATEST=$(find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f\n" \| sort -r \| head -n1); tar -tzf /var/backups/meddesk/$LATEST/meddesk-uploads-*.tar.gz >/dev/null && echo TAR_OK'` | 输出 `TAR_OK`；tar 非空 |
| W3 | 生产 | 可恢复备份清单 | `ssh web 'sudo -u ops /home/ops/service/meddesk/scripts/restore.sh --dest /var/backups/meddesk --list'` | 至少覆盖最近 7 天；DB/FILES 标识符合计划 |
| W4 | 生产 | DB 连接与容量 | `ssh web "sudo docker compose -f /home/ops/service/docker/meddesk/docker-compose.middleware.yml exec -T postgres psql -U '<数据库用户>' -d '<数据库名>' -c \"SHOW max_connections; SELECT state,count(*) FROM pg_stat_activity GROUP BY state; SELECT pg_size_pretty(pg_database_size(current_database()));\""` | 连接峰值有 ≥30% 余量，无长期异常连接堆积；容量增长符合业务量 |
| W5 | 生产 | 目录容量/权限 | `ssh web 'sudo du -xhd1 /var/backups/meddesk /home/ops/service/meddesk/data /var/log 2>/dev/null; stat -c "%U:%G %a %n" /home/ops/service/meddesk/.meddesk.env /home/ops/service/meddesk/data'` | 环境文件 `root:ops 640`；data 属主可由 ops 写，容量无异常跃升 |
| W6 | 生产 | TLS 到期 | `echo \| openssl s_client -connect meddesk.qiubite.fun:443 -servername meddesk.qiubite.fun 2>/dev/null \| openssl x509 -noout -dates -checkend 1209600 && echo CERT_OK` | 输出 `CERT_OK`，证书剩余 >14 天 |
| W7 | 生产 | 静态缓存头 | `curl -sS -D - -o /dev/null https://meddesk.qiubite.fun/admin/app.js \| grep -Ei 'HTTP/\|content-type\|cache-control'` | 200、JavaScript Content-Type、`no-store`；不是 `text/html` |
| W8 | 生产 | 一周备份错误 | `ssh web "sudo journalctl -u meddesk --since '7 days ago' -o cat --no-pager \| jq -s '[.[]\|select(.msg==\"记账本备份失败\" or .msg==\"记账本自动备份未执行\") ]\|length'"` | 输出 0；任何非零都需关联故障单 |

异地副本每周至少抽查一次：对比最新时间戳、文件数和 `SHA256SUMS`；`backup.sh` 的远端复制失败不会可靠地体现在最终退出码中。

### 6.3 月巡检

| ID | 环境 | 巡检项 | 命令/操作 | 正常判据 |
|---|---|---|---|---|
| M1 | 隔离测试 | DB+附件恢复演练 | 在 `<测试 SSH 别名>` 按 3.4 先 `--list`、`--dry-run`，再恢复最新备份；禁止指向生产数据库 | 校验通过，服务可启动，登录/同步/记账本/附件冒烟通过；记录 RPO/RTO |
| M2 | 隔离测试 | 记账本 WebDAV 恢复 | 下载当日 ZIP，校验 `manifest.json`/`ledger.json` 并导入测试账号 | 账单和分类数量一致；明确登记 `images/` 尚不能自动还原 |
| M3 | 生产 | 连接池预算复核 | 执行 5.4 的 `max_connections/pg_stat_activity` 查询，并核对副本数 × `MAX_CONNS` | 总 MaxConns ≤数据库上限 70%，保留至少 20 个应急连接 |
| M4 | 生产 | 保留策略/异地副本 | `ssh web 'find /var/backups/meddesk -mindepth 1 -maxdepth 1 -type d -printf "%f %k KiB\n" \| sort -r'` | 本地覆盖 30 天目标；异地存在独立可校验副本，无异常空目录 |
| M5 | 生产 | 密钥托管与权限 | 核对密码管理器/离线介质双副本；执行下方密钥格式检查 | 两份均可由授权人员取用；格式检查 `KEY_FORMAT_OK`，全程不显示密钥 |
| M6 | 生产 | 版本与二进制 | `ssh web 'sudo -u ops /home/ops/service/meddesk/meddesk -version'; curl -fsS https://meddesk.qiubite.fun/api/v1/health \| jq -r .version` | 两处均为 `v0.3.50`；多副本时全部一致 |
| M7 | 开发/测试 | 回归测试 | Git Bash：`cd /d/workbuddy/工作台/meddesk && go test ./...` | 全部通过；失败项先修复，不带入生产发布 |
| M8 | 生产 | 告警与事故复盘 | 复核当月 5xx/P95/429/磁盘/备份告警及本手册阈值 | 告警可触达、有处置记录；重复故障形成修复项，监控缺口有负责人 |
| M9 | 生产 | 扩容前置项复核 | 核对副本数、共享附件、Redis、调度选主和静态 build ID | 单副本可保持现状；若多副本，5.3 所有条件必须已完成 |

不显示密钥的格式检查：

```bash
ssh web "sudo -u ops bash -lc 'set -a; source /home/ops/service/meddesk/.meddesk.env; set +a; python3 -c \"import os,base64; key=base64.b64decode(os.environ.get(\\\"MEDDESK_SECRETBOX_KEY\\\",\\\"\\\"),validate=True); assert len(key)==32; print(\\\"KEY_FORMAT_OK\\\")\"'"
```

