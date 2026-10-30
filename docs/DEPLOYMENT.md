# MedDesk · 部署文档

> **文档更新时间：** 2026-10-30 · **对应发布版本：** v0.3.50（2026-10-30 01:32 完成生产部署并验证通过）
> **生产环境：** 阿里云 ECS · 域名 `https://meddesk.qiubite.fun` · SSH 别名 `web`
> **生产形态：** systemd 单二进制（前端已用 Go `embed` 内嵌，**不需要单独部署静态资源**）+ nginx 终结 TLS 反代到 `127.0.0.1:8089`
> **阅读方式：** 全新装机走第 4 节；日常升级直接跳第 5 节；出问题走第 6 节回滚。
> **安全红线：** 全文 `<...>` 均为占位符。**禁止**把真实密钥、口令、私钥、令牌写进本文档、`.env` 或任何会被 git 跟踪的文件。

## 目录

- [1. 适用环境与版本矩阵](#1-适用环境与版本矩阵)
- [2. 目标环境依赖与前置条件](#2-目标环境依赖与前置条件)
- [3. 配置说明（.meddesk.env）](#3-配置说明meddesk-env)
- [4. 全新安装（从裸机到上线）](#4-全新安装从裸机到上线)
- [5. 增量升级（标准发布流程）](#5-增量升级标准发布流程)
- [6. 回滚](#6-回滚)
- [7. 部署后验证清单](#7-部署后验证清单)
- [8. 备份与运维](#8-备份与运维)
- [9. 监控与日志](#9-监控与日志)
- [10. 故障排查表](#10-故障排查表)
- [11. 版本与变更记录](#11-版本与变更记录)
- [附：Docker Compose 方案（路径 B）](#附docker-compose-方案路径-b)

---

## 1. 适用环境与版本矩阵

### 1.1 环境矩阵

| 环境 | 用途 | 部署方式 | 访问入口 | 适用版本 |
|---|---|---|---|---|
| 开发 | 本地开发、改前端不重编 Go | 源码运行：`MEDDESK_WEB_DIR=./web go run ./cmd/meddesk` | `http://localhost:8089` | v0.3.50 |
| 测试 / 预发 | 集成验证、迁移试跑 | Docker Compose（路径 B，见附录） | `http://127.0.0.1:8089` | v0.3.50 |
| 生产 | 对外服务 | systemd 单二进制 + nginx（路径 A，本文档主线） | `https://meddesk.qiubite.fun` | v0.3.50 |

### 1.2 组件版本矩阵

| 组件 | 开发 / 测试 | 生产 | 说明 |
|---|---|---|---|
| Go（构建） | Go 1.26.x（与 `go.mod` 的 `go 1.26.4` 一致） | 服务器**不需要**装 Go，构建机交叉编译 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` | 版本号经 `-ldflags -X ...internal/server.Version` 注入 |
| PostgreSQL | 16 / 17 / 18 均可 | 16 / 17 / 18 均可（本文档以 18 为例，PGDG 小版本滚动更新） | 迁移 SQL 未用版本特有特性 |
| Redis | 可选（默认关闭） | 可选（默认关闭，走进程内降级） | 仅多副本需跨进程共享限流/吊销名单时开启 |
| nginx | — | nginx + acme.sh（ECC 证书，DNS-01 验证） | TLS 终止在 nginx，应用只听 127.0.0.1 |
| 操作系统 | 任意 | Debian 12 / Ubuntu 22.04 | 阿里云 ECS |
| 应用端口 | `:8089`（默认） | `:8089`（与默认一致） | 改端口必须同步 nginx upstream |

> **端口已全仓统一为 `:8089`**（2026-10-30）：代码默认值、`meddesk.env.example`、nginx `upstream`、
> systemd 注释、Compose 端口映射、`install.sh` 与全部文档均为 `:8089`，不再存在「模板 8080 / 现网 8089」的错位。
> 8089 为非特权端口，仍满足 systemd 安全基线（无需 CAP_NET_BIND_SERVICE）。

---

## 2. 目标环境依赖与前置条件

### 2.1 依赖清单

| 类别 | 要求 | 说明 |
|---|---|---|
| 操作系统 | Debian 12 / Ubuntu 22.04 | 20GB+ 系统盘；实测 1.6GB 内存可跑全套（PG 500m + Redis 320m + 应用 + nginx，且不使用 swap） |
| 构建环境 | 构建机装 Go 1.26.x | 服务器零 Go 依赖，产物是单个静态二进制 |
| 数据库 | PostgreSQL 16/17/18，仅监听 `127.0.0.1` | 迁移开机自动执行（`MEDDESK_DB_MIGRATE_ON_BOOT=true`，幂等） |
| 缓存 | Redis 7（**可选**） | 不装也能跑，功能等价 |
| 反向代理 | nginx（TLS 终止 + 反代） | 证书用 acme.sh DNS-01，不需要 80 端口挑战路径 |
| 字体 | Noto Sans SC（`deploy/scripts/get-font.sh`） | PDF 导出必需；缺失时导出返回 503，其余功能不受影响 |
| 运行时用户 | `ops`（需具备 sudo 权限，本次部署全程用 sudo） | 二进制由 root 安装，进程以 ops 运行 |
| 客户端工具 | 本地需 `scp` / `ssh` / `sha256sum` | 上传与校验用 |

### 2.2 磁盘与 /tmp 空间要求

| 检查项 | 阈值 | 处置 |
|---|---|---|
| 根分区 `/` 使用率 | ≥75% 预警 / ≥85% 严重 | 清理日志、旧备份、旧 `.bak-*` 二进制 |
| `/tmp` 使用率 | ≥70% 预警 / ≥85% 严重 | **发布前必查**；满了先清临时文件再上传 |

> ⚠️ **历史教训（必读）**：`/tmp` 在部分镜像/配置下是 tmpfs（内存盘）。曾因反复上传 34MB 的 meddesk 二进制把 tmpfs 撑到 100%，nginx 直接 502。
> 因此：**每次发布第一步就是 `df -h /tmp`，并且必须在装完后立刻 `rm -f` 掉临时文件**（见第 5 节步骤 4）。

```bash
ssh web 'df -h /tmp; findmnt /tmp || true'
```

### 2.3 防火墙与安全组端口

```bash
# 以 root 执行；ufw 只放行 SSH 与 Web（先确认 SSH 端口再启用！）
ufw allow <SSH端口>/tcp && ufw allow 80/tcp && ufw allow 443/tcp && ufw --force enable
```

| 端口 | 对外开放 | 说明 |
|---|---|---|
| SSH（本环境 1047） | 是 | 仅密钥认证；改端口后记得同步 ufw 与阿里云安全组 |
| 80 / 443 | 是 | 80 只做全量 307 强跳 HTTPS |
| 8089（应用） | **否** | 只监听 127.0.0.1，由 nginx 反代；阿里云安全组也不要放行 |
| 5432（PG）/ 6379（Redis） | **否** | 仅本机回环，切勿开 `listen_addresses='*'` |

> 阿里云 ECS 有**两道**防线：系统内 ufw + 云控制台安全组，两者都要收敛。

### 2.4 本机 SSH 别名（可选，推荐）

本机 `~/.ssh/config`（私钥路径只是占位符，勿把私钥内容贴进任何文档）：

```
Host web
    HostName <ECS 公网 IP>
    User ops
    Port 1047
    IdentityFile ~/.ssh/<你的私钥文件名>
    IdentitiesOnly yes
```

配好后全文的 `ssh web '...'` / `scp ... web:/tmp/...` 可直接复制执行。

### 2.5 其他前置

```bash
# 时钟同步（JWT 对时钟漂移敏感）
timedatectl set-ntp true && timedatectl status | grep -E 'synchronized|NTP'
```

> **DNS 前置**：把 `meddesk.qiubite.fun` 的 A/AAAA 记录指到 ECS 公网 IP，等解析生效再申请证书。

---

## 3. 配置说明（.meddesk.env）

### 3.1 文件位置与权限

| 项 | 生产路径 | 属主 / 权限 |
|---|---|---|
| 二进制 | `/home/ops/service/meddesk/meddesk` | `root:root` `0755`（用 `install -m 0755` 安装） |
| 环境变量 | `/home/ops/service/meddesk/.meddesk.env` | `root:ops` `0640`（**绝不提交 git**） |
| 附件目录 | `/home/ops/service/meddesk/data` | `ops:ops` `0750`，且必须在 unit 的 `ReadWritePaths` 中 |
| 字体目录 | `/home/ops/service/meddesk/fonts` | `ops:ops` `0755` |
| compose 目录 | `/home/ops/service/docker/meddesk/` | 仅路径 B 使用 |
| systemd 单元 | `/etc/systemd/system/meddesk.service` | 添加/修改后需 `systemctl daemon-reload` |

```bash
sudo chown root:ops /home/ops/service/meddesk/.meddesk.env
sudo chmod 640 /home/ops/service/meddesk/.meddesk.env
```

### 3.2 记账本相关（v0.3.50 新增，重点）

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_SECRETBOX_KEY` | 无（**必填**） | base64 编码的 **32 字节**密钥，用于把用户的 WebDAV 凭据 **AES-256-GCM 加密后落盘**（AAD 绑定账号）。生成：`openssl rand -base64 32`。**丢失或更改 = 已存凭据全部无法解密，用户必须重新填写**；必须离线备份到密码管理器。未配置时备份接口直接报错「服务端未配置 MEDDESK_SECRETBOX_KEY，备份功能不可用」，其余功能不受影响 | **是（最高级）** |
| `MEDDESK_LEDGER_BACKUP_ENABLED` | `1` | 记账本每日自动备份总开关。`1/true/yes/on` 开，其他值关 | 否 |
| `MEDDESK_LEDGER_BACKUP_TIME` | `02:00` | 每日触发时刻，格式 `HH:MM`；非法值回落 `02:00` | 否 |
| `MEDDESK_LEDGER_BACKUP_TZ` | `Asia/Shanghai` | 时区名；容器无 tzdata 时 `LoadLocation` 失败，自动退化为固定 +08:00（中国无夏令时，等价） | 否 |

**关于 `MEDDESK_SECRETBOX_KEY` 的五条铁律：**

1. 只在**首次启用记账本备份前**生成一次，之后**永不更改**。
2. 生成后立刻存进密码管理器（与 JWT 密钥同等保护级别）；它不在数据库里，数据库备份救不了它。
3. 改了它 → 用户保存的 WebDAV 凭据全部作废 → 需要逐个提醒用户重新填写。
4. 丢了它 → 同上，且无法恢复，只能让用户重填。
5. 它与 `MEDDESK_JWT_SECRET` 是**两把不同的钥匙**，不要图省事复用同一个值。

**关于每日备份的已知限制：** 调度器是**进程内 ticker**，多实例部署时每个实例都会触发一次上传（同名覆盖，不产生重复文件，但浪费带宽与配额，且运行记录互相覆盖）。当前**没有**跨实例选主。生产单实例即可；多实例需自行加 `pg_advisory_xact_lock` 或 Redis 锁。

**管理后台（v0.3.51 起必配）**

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_ADMIN_EMAILS` | **空（必填）** | 管理员邮箱白名单，逗号分隔、大小写不敏感。**v0.3.51 起代码不再内置任何默认邮箱**：不配置则 `/admin` 无人可登录，启动日志会出现 `管理后台配置告警`（仅告警、不阻断启动）。从旧版本升级时**必须**显式补上 | 否 |
| `MEDDESK_ADMIN_SALT`<br>`MEDDESK_ADMIN_PASSWORD_HASH` | 空 | 后台口令的盐与哈希。在本机执行 `go run ./cmd/meddesk-admin admin-credential` 生成后把两行贴入；**明文口令不落任何文件、不进 git、不进日志** | **是** |
| `MEDDESK_ADMIN_TTL` | `8h` | 后台会话有效期 | 否 |
| `MEDDESK_RECOVERY_KEY` | 空 | 托管恢复私钥，格式 `base64(PKCS#8 DER)` 的 RSA 私钥。账号开启托管后，服务端用它解出 DEK 并用新口令重新包裹，实现「改密不清数据」；**持有者即可解密已托管账号的数据**，按最高密级保管，不用则留空 | **是（最高级）** |

> 升级检查：v0.3.51 之后如果后台「点登录没反应」或直接 401，先看启动日志有没有 `管理后台配置告警`——
> 十有八九是 `.meddesk.env` 漏了 `MEDDESK_ADMIN_EMAILS`，而不是口令错了。

### 3.3 运行环境

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_ENV` | `prod` | `dev`/`development` 会有更宽松的 CORS 与日志；生产必须 `prod` | 否 |
| `MEDDESK_HTTP_ADDR` | `:8089` | 监听地址。生产本环境为 `:8089`；前面有 nginx 时**不要**改成对外网卡地址 | 否 |
| `MEDDESK_BASE_URL` | `http://localhost:8089` | 站点外链基址（邮件/导出/分享链接），生产 `https://meddesk.qiubite.fun`，结尾不带斜杠 | 否 |
| `MEDDESK_TRUST_PROXY` | `false` | 前面有 nginx 时必须 `true`，否则客户端 IP 全是 127.0.0.1，限流与登录失败计数会「一损俱损」 | 否 |
| `MEDDESK_WEB_DIR` | 空 | 空 = 用二进制内嵌的前端（生产必须留空）；开发设 `./web` 可改前端不重编 | 否 |

### 3.4 数据库

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_DB_URL` | `postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable` | 连接串。生产：`postgres://<user>:<password>@127.0.0.1:5432/<db>?sslmode=disable`；口令含 `@ : / # ?` 需 URL 编码 | **是** |
| `MEDDESK_DB_MAX_CONNS` | `10` | 连接池上限；必须满足 `MAX_CONNS × 实例数 < PG max_connections - 3` | 否 |
| `MEDDESK_DB_MIN_CONNS` | `1` | 预热连接数，小机 1 即可 | 否 |
| `MEDDESK_DB_MAX_CONN_LIFETIME` | `30m` | 单连接最长存活，避免被 LB/防火墙静默断开后首次请求必失败 | 否 |
| `MEDDESK_DB_MIGRATE_ON_BOOT` | `true` | 开机自动迁移（幂等，依赖 `schema_migrations`）；生产保留 `true`，多实例滚动发布时只在首批实例开启 | 否 |

### 3.5 Redis（可选）

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_REDIS_ENABLED` | `false` | 单实例不装 Redis 也能跑（进程内 Noop 降级，功能等价）；多副本才开 | 否 |
| `MEDDESK_REDIS_URL` | `redis://127.0.0.1:6379/0` | 生产：`redis://:<password>@127.0.0.1:6379/0`；TLS 用 `rediss://` | **是** |

### 3.6 鉴权

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_JWT_SECRET` | 空（**必填**） | 签名密钥，长度 ≥ 32 字节，生成 `openssl rand -base64 48`。**改它 = 所有用户令牌立即失效需重新登录** | **是（最高级）** |
| `MEDDESK_ACCESS_TTL` | `60m` | 访问令牌有效期。日常使用下前端每小时才刷新一次（15m 时是每 15 分钟），多设备/多标签页同时刷新的概率显著下降；令牌泄露窗口变长为代价，但有 refresh 轮换与重放检测兜底 | 否 |
| `MEDDESK_REFRESH_TTL` | `720h`（30 天） | 刷新令牌有效期，每次使用都会轮换并做重放检测 | 否 |
| `MEDDESK_REFRESH_REUSE_GRACE` | `30s` | 并发刷新宽限期，超过则视为重放攻击 | 否 |
| `MEDDESK_ARGON2_MEMORY` | `65536`（64MiB） | 弱机降到 32768，强机升到 131072；参数写在哈希串里，改了不影响老用户验证 | 否 |
| `MEDDESK_ARGON2_ITERATIONS` | `3` | 同上 | 否 |
| `MEDDESK_ARGON2_PARALLELISM` | `2` | 同上 | 否 |
| `MEDDESK_SIGNUP_ENABLED` | **代码默认 `true`** | 公网部署**必须显式设为 `false`**（代码默认值是危险的！）。首个账号：`true` → 重启 → 注册 → 改回 `false` → 重启 | 否 |
| `MEDDESK_RECOVERY_KEY` | 空 | 托管恢复用的 RSA 私钥（base64 PKCS#8 DER）；留空 = 本部署不支持托管恢复 | **是** |

### 3.7 管理后台

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_ADMIN_EMAILS` | 代码内置了一个个人邮箱 | 允许登录 `/admin` 的邮箱白名单（逗号分隔，大小写不敏感）。**必须在 `.meddesk.env` 显式写成本部署的管理员邮箱**，不要依赖代码默认值 | 否 |
| `MEDDESK_ADMIN_TTL` | `8h` | 后台会话有效期；后台 token 与业务 token 不同 scope，业务 token 泄露也调不动后台接口 | 否 |
| `MEDDESK_ADMIN_SALT` | 空 | 独立管理员凭据的 KDF salt（base64）。可选：设置后白名单邮箱可用「后台专用口令」登录，不必动真实账号口令（E2E 下改账号口令会让旧 KEK 解不开 DEK，代价极大） | **是** |
| `MEDDESK_ADMIN_PASSWORD_HASH` | 空 | 同上，argon2id(PBKDF2(管理员口令, salt))，与用户表同格式 | **是** |

### 3.8 同步 / 附件 / PDF / 日志 / 限流 / CORS

| 变量 | 默认值 | 用途与生产建议 | 敏感 |
|---|---|---|---|
| `MEDDESK_SYNC_MAX_ITEMS` | `500` | 单次 push 最大条数，超出返回 bad_request | 否 |
| `MEDDESK_SYNC_MAX_RECORD_BYTES` | `1048576`（1MiB） | 单条密文上限 | 否 |
| `MEDDESK_SYNC_PULL_PAGE` | `300` | pull 默认分页大小 | 否 |
| `MEDDESK_SYNC_PULL_PAGE_MAX` | `1000` | pull 分页上限，客户端传更大值会被夹到这里 | 否 |
| `MEDDESK_UPLOAD_DIR` | `/home/ops/service/meddesk/data` | 密文附件落盘根目录。**丢了 = 附件永久不可恢复** | 否 |
| `MEDDESK_UPLOAD_MAX_BYTES` | `8388608`（8MiB） | 单附件上限；nginx 的 `client_max_body_size` 必须 ≥ 此值 | 否 |
| `MEDDESK_PDF_FONT_PATH` | 空（自动探测 4 个候选路径） | 中文 TTF；留空先探测 `/home/ops/service/meddesk/fonts/NotoSansSC-Regular.ttf` | 否 |
| `MEDDESK_LOG_LEVEL` | `info` | `debug` 会打请求/同步明细，**生产慎用** | 否 |
| `MEDDESK_LOG_FORMAT` | `json` | json 便于 journald/loki 检索；text 适合本地 | 否 |
| `MEDDESK_RATE_LIMIT_RPS` | `20` | 每 IP 令牌桶速率。本生产环境取值 **20** | 否 |
| `MEDDESK_RATE_LIMIT_BURST` | `60` | 突发上限。本生产环境取值 **60**；发现大量 429 再上调 | 否 |
| `MEDDESK_CORS_ALLOWED_ORIGINS` | 空 | 留空 = 只放行同源（推荐）。前端与 API 不同域时才填，**不要填 `*`**（所有接口都带凭据） | 否 |

### 3.9 上线前最小检查清单

```bash
openssl rand -base64 48   # → MEDDESK_JWT_SECRET
openssl rand -base64 32   # → MEDDESK_SECRETBOX_KEY（记账本，生成后离线备份！）
openssl rand -base64 24   # → PG / Redis 口令
```

- [ ] 全部 `<...>` 占位符已替换
- [ ] `MEDDESK_SIGNUP_ENABLED=false`
- [ ] `MEDDESK_TRUST_PROXY=true`、`MEDDESK_ENV=prod`
- [ ] `.meddesk.env` 权限 `root:ops 0640`，且**不在 git 里**
- [ ] `MEDDESK_SECRETBOX_KEY` 已存密码管理器

---

## 4. 全新安装（从裸机到上线）

> 全新安装用 root 执行系统部分，应用部分最终以 `ops` 运行。

### 4.1 系统准备

```bash
apt update && apt -y upgrade
apt -y install curl git unzip ca-certificates gnupg ufw

# 防火墙（先按 2.3 确认 SSH 端口）
ufw allow <SSH端口>/tcp && ufw allow 80/tcp && ufw allow 443/tcp && ufw --force enable

# 时钟同步
timedatectl set-ntp true && timedatectl status | grep -E 'synchronized|NTP'

id ops >/dev/null 2>&1 || useradd -m -s /bin/bash ops
```

### 4.2 目录结构（ops 多服务布局）

```bash
install -d -o ops -g ops -m 0750 /home/ops/service/meddesk
install -d -o ops -g ops -m 0750 /home/ops/service/meddesk/data
install -d -o ops -g ops -m 0755 /home/ops/service/meddesk/fonts
mkdir -p /home/ops/service/docker/meddesk   # 仅路径 B 需要
```

### 4.3 PostgreSQL（宿主机方式）

```bash
# 官方 PGDG 源（Debian 12；Ubuntu 把 bookworm 换成 jammy）
install -d /usr/share/postgresql-common/pgdg && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
  | gpg --dearmor -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
  > /etc/apt/sources.list.d/pgdg.list
apt update && apt -y install postgresql-18

sudo -u postgres psql <<'SQL'
CREATE ROLE meddesk LOGIN PASSWORD '<openssl rand -base64 24 的输出>';
CREATE DATABASE meddesk OWNER meddesk;
\c meddesk
CREATE EXTENSION IF NOT EXISTS citext;
SQL

# 小机调优（与 PG 500m 上限配套）
cp deploy/postgres-tuning.conf /etc/postgresql/18/main/conf.d/meddesk.conf
systemctl restart postgresql
```

- 角色**不授** SUPERUSER / CREATEDB；PG 只监听 `127.0.0.1`。
- `citext` 用于邮箱大小写不敏感唯一；云厂商禁扩展时迁移会自动退化为 `lower(email)` 唯一索引。

### 4.4 字体（PDF 导出必需）

```bash
bash deploy/scripts/get-font.sh    # → /home/ops/service/meddesk/fonts/
ls -l /home/ops/service/meddesk/fonts/
```

### 4.5 安装二进制

```bash
# ① 构建机交叉编译（服务器无需 Go）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags="-s -w -X github.com/qiubite666/meddesk/internal/server.Version=v0.3.50" \
  -o dist/release/meddesk-v0.3.50 ./cmd/meddesk
# 也可一步到位：./deploy/scripts/release.sh v0.3.50

# ② 上传到**短文件名**（防 /tmp 撑满，见 2.2）
scp dist/release/meddesk-v0.3.50 web:/tmp/m50.bin

# ③ 校验 + 安装 + 清理
ssh web 'sha256sum /tmp/m50.bin'
ssh web 'sudo install -m 0755 /tmp/m50.bin /home/ops/service/meddesk/meddesk && sudo rm -f /tmp/m50.bin'
```

### 4.6 写 `.meddesk.env`

```bash
cat > /home/ops/service/meddesk/.meddesk.env <<'EOF'
MEDDESK_ENV=prod
MEDDESK_HTTP_ADDR=:8089
MEDDESK_BASE_URL=https://meddesk.qiubite.fun
MEDDESK_TRUST_PROXY=true
MEDDESK_WEB_DIR=

MEDDESK_DB_URL=postgres://<user>:<password>@127.0.0.1:5432/<db>?sslmode=disable
MEDDESK_DB_MAX_CONNS=10
MEDDESK_DB_MIN_CONNS=1
MEDDESK_DB_MAX_CONN_LIFETIME=30m
MEDDESK_DB_MIGRATE_ON_BOOT=true

MEDDESK_REDIS_ENABLED=false
# 启用时：MEDDESK_REDIS_URL=redis://:<password>@127.0.0.1:6379/0

MEDDESK_JWT_SECRET=<openssl rand -base64 48 的输出>
MEDDESK_ACCESS_TTL=60m
MEDDESK_REFRESH_TTL=720h
MEDDESK_REFRESH_REUSE_GRACE=30s
MEDDESK_ARGON2_MEMORY=65536
MEDDESK_ARGON2_ITERATIONS=3
MEDDESK_ARGON2_PARALLELISM=2
MEDDESK_SIGNUP_ENABLED=false

MEDDESK_ADMIN_EMAILS=<管理员邮箱，逗号分隔>
MEDDESK_ADMIN_TTL=8h
# 可选独立后台口令（需与 salt 配对生成）：
# MEDDESK_ADMIN_SALT=<base64>
# MEDDESK_ADMIN_PASSWORD_HASH=<argon2id 串>

MEDDESK_UPLOAD_DIR=/home/ops/service/meddesk/data
MEDDESK_UPLOAD_MAX_BYTES=8388608
MEDDESK_PDF_FONT_PATH=/home/ops/service/meddesk/fonts/NotoSansSC-Regular.ttf

MEDDESK_LOG_LEVEL=info
MEDDESK_LOG_FORMAT=json
MEDDESK_RATE_LIMIT_RPS=20
MEDDESK_RATE_LIMIT_BURST=60
MEDDESK_CORS_ALLOWED_ORIGINS=

# ---------------- 记账本（v0.3.50 新增）----------------
# 32 字节 base64：openssl rand -base64 32 ；生成即离线备份，永不再改
MEDDESK_SECRETBOX_KEY=<base64 32 字节密钥>
MEDDESK_LEDGER_BACKUP_ENABLED=1
MEDDESK_LEDGER_BACKUP_TIME=02:00
MEDDESK_LEDGER_BACKUP_TZ=Asia/Shanghai

TZ=Asia/Shanghai
EOF

sudo chown root:ops /home/ops/service/meddesk/.meddesk.env
sudo chmod 640 /home/ops/service/meddesk/.meddesk.env
```

### 4.7 systemd 单元

```bash
cp deploy/systemd/meddesk.service /etc/systemd/system/meddesk.service
# 核对两处：EnvironmentFile 指向 .meddesk.env、ReadWritePaths 含附件目录
grep -E 'EnvironmentFile|ReadWritePaths|User=' /etc/systemd/system/meddesk.service
sudo systemctl daemon-reload
sudo systemctl enable --now meddesk
sudo systemctl status meddesk --no-pager | head -12
```

- 表结构由应用启动时自动迁移创建，**无需手工执行 SQL**。
- unit 已含 `ProtectSystem=strict` / `ProtectHome=read-only` / `NoNewPrivileges` / seccomp 白名单等加固；若某内核下启动即退出（`status=203/EXEC` 或 SIGSYS），先注释 `SystemCallFilter` 再二分收紧。

### 4.8 nginx 与 TLS

```bash
apt -y install nginx
cp deploy/nginx/meddesk.conf /etc/nginx/sites-available/meddesk.conf
sed -i 's/med\.example\.com/meddesk.qiubite.fun/g' /etc/nginx/sites-available/meddesk.conf
grep -n 'server 127.0.0.1:8089;' /etc/nginx/sites-available/meddesk.conf  # 确认 upstream 端口为 8089
ln -sf /etc/nginx/sites-available/meddesk.conf /etc/nginx/sites-enabled/
rm -f /etc/nginx/sites-enabled/default
nginx -t

# acme.sh：DNS-01 验证，不需要 80 端口与站点可访问
curl https://get.acme.sh | sh -s email=you@example.com
source ~/.bashrc
acme.sh --issue --dns dns_xx -d meddesk.qiubite.fun --keylength ec-256
acme.sh --install-cert -d meddesk.qiubite.fun --ecc \
  --fullchain-file /etc/letsencrypt/live/meddesk.qiubite.fun/fullchain.pem \
  --key-file       /etc/letsencrypt/live/meddesk.qiubite.fun/privkey.pem \
  --reloadcmd      "nginx -s reload"
nginx -s reload
```

> `dns_xx` 换成你的 DNS 提供商插件（cf=Cloudflare、dp=DNSPod、aliyun=阿里云…）。acme.sh 会自动写 crontab 续期。

### 4.9 首次冒烟

```bash
curl -fsS --noproxy '*' http://127.0.0.1:8089/api/v1/health
curl -fsS --noproxy '*' https://meddesk.qiubite.fun/api/v1/health
curl -fsS --noproxy '*' https://meddesk.qiubite.fun/api/v1/ready
```

浏览器打开 `https://meddesk.qiubite.fun` 应看到登录页。首个账号：临时把 `MEDDESK_SIGNUP_ENABLED=true` → 重启 → 注册 → 改回 `false` → 重启。

---

## 5. 增量升级（标准发布流程）

> 以下为 **2026-10-30 v0.3.50 实际执行的流程**，逐条可直接复制。示例短文件名 `m50.bin`、端口 `8089`、域名 `meddesk.qiubite.fun`。

### 步骤 0（本地）：构建并记下 sha256

```bash
cd "<项目根>"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags="-s -w -X github.com/qiubite666/meddesk/internal/server.Version=v0.3.50" \
  -o dist/release/meddesk-v0.3.50 ./cmd/meddesk
sha256sum dist/release/meddesk-v0.3.50
```

### 步骤 1：部署前检查 `/tmp`

```bash
ssh web 'df -h /tmp'
```

> 使用率 ≥70% 先清理再上传。**这一步不是形式主义**——见 2.2 的 502 事故。

### 步骤 2：备份现网二进制

```bash
ssh web 'cd /home/ops/service/meddesk && sudo cp meddesk meddesk.bak-$(date +%Y%m%d-%H%M%S) && ls -l meddesk.bak-* | tail -3'
```

> v0.3.50 本次生成：`meddesk.bak-20261030-013018`。

### 步骤 3：上传到短文件名并核对 sha256

```bash
scp dist/release/meddesk-v0.3.50 web:/tmp/m50.bin
# 本地与远端各算一次，逐字符比对
sha256sum dist/release/meddesk-v0.3.50
ssh web 'sha256sum /tmp/m50.bin'
```

两边必须完全一致（本次 v0.3.50：
`63c553a1d0e1c11cb652ec594114c9085ff0e1381b56d076532393e429370979`）。不一致 → 重传，**不要继续**。

### 步骤 4：安装（关键）并清理临时文件

```bash
ssh web 'sudo install -m 0755 /tmp/m50.bin /home/ops/service/meddesk/meddesk && sudo rm -f /tmp/m50.bin'
```

> ⚠️ **绝不能用 `scp` 直接覆盖目标二进制**：scp 会保留本地文件权限，本地构建产物的执行位可能丢失，导致 systemd 启动失败报 `203/EXEC`。
> 必须用 `install -m 0755`，它同时负责「原子改名 + 置权限」。

### 步骤 5：重启

```bash
ssh web 'sudo systemctl restart meddesk'
```

### 步骤 6：验证

```bash
ssh web 'systemctl is-active meddesk'
ssh web 'journalctl -u meddesk -n 40 --no-pager'
```

日志里必须看到两条：`数据库迁移完成`、`记账本自动备份已排程`。

```bash
ssh web "curl -fsS --noproxy '*' http://127.0.0.1:8089/api/v1/health"
ssh web "curl -fsS --noproxy '*' http://127.0.0.1:8089/assets/js/ledger.js -o /dev/null -w '%{http_code}\n'"
ssh web "curl -fsS --noproxy '*' http://127.0.0.1:8089/admin -o /dev/null -w '%{http_code}\n'"
ssh web "curl -fsS --noproxy '*' http://127.0.0.1:8089/admin/app.js -o /dev/null -w '%{http_code}\n'"
```

> `curl` 一律带 `--noproxy '*'`：本机若配了 HTTP 代理，不加会把请求发到代理上，误判成服务不可用。

### 步骤 7：灰度与强刷提醒（发布后必做）

> ⚠️ **历史教训**：旧标签页在新 JS 生效前仍在用旧前端推数据，会出现「数据看起来没更新 / 报旧接口错」的假故障。

1. 发布后在群里通知用户：**关闭多余的 MedDesk 标签页，只保留一个**。
2. 保留的那个标签页执行强制刷新（Windows/Linux `Ctrl+F5`，macOS `Cmd+Shift+R`）。
3. 移动端/App（Capacitor）端：退出后重进一次。
4. 若用户仍反馈异常，先确认 `curl /api/v1/health` 的 `version` 已是 `v0.3.50`，再让用户强刷。

---

## 6. 回滚

```bash
# ① 列出可用备份
ssh web 'ls -l /home/ops/service/meddesk/meddesk.bak-*'

# ② 停服务 → 用备份覆盖 → 起服务
ssh web 'sudo systemctl stop meddesk'
ssh web 'cd /home/ops/service/meddesk && sudo install -m 0755 meddesk.bak-20261030-013018 meddesk'
ssh web 'sudo systemctl start meddesk'

# ③ 验证
ssh web 'systemctl is-active meddesk'
ssh web 'journalctl -u meddesk -n 40 --no-pager'
ssh web "curl -fsS --noproxy '*' http://127.0.0.1:8089/api/v1/health"
```

- 迁移文件只增不改、向前兼容（`schema_migrations` 幂等），旧二进制兼容新表结构，**可直接回滚**。
- 若新版本迁移新增了表（v0.3.50 新增了记账本与备份相关表），回滚后旧版忽略多余表，不影响运行；**不建议**手工 DROP。
- 回滚同样要注意第 5 节步骤 7：让用户关多余标签页 + 强刷。
- 回滚也要**先留一份当前快照**：`sudo cp meddesk meddesk.bak-$(date +%Y%m%d-%H%M%S).before-rollback`，便于反向排查。

---

## 7. 部署后验证清单

| # | 检查项 | 命令 | 期望 |
|---|---|---|---|
| 1 | 服务状态 | `ssh web 'systemctl is-active meddesk'` | `active` |
| 2 | 无崩溃重启 | `ssh web 'systemctl show meddesk -p NRestarts'` | 增量未增加 |
| 3 | 迁移完成 | `ssh web 'journalctl -u meddesk --since today -o cat \| grep -c 数据库迁移完成'` | ≥1 |
| 4 | 备份已排程 | `ssh web 'journalctl -u meddesk --since today -o cat \| grep 记账本自动备份已排程'` | 有 02:00 / Asia/Shanghai |
| 5 | 版本号 | `curl -fsS --noproxy '*' https://meddesk.qiubite.fun/api/v1/health` | `version` = `v0.3.50` |
| 6 | 健康/就绪 | 同上 + `/api/v1/ready` | `status=ok`、`db=ok` |
| 7 | 记账本前端 | `curl -fsS --noproxy '*' https://meddesk.qiubite.fun/assets/js/ledger.js -o /dev/null -w '%{http_code}'` | `200` |
| 8 | 管理后台 | `curl -fsS --noproxy '*' https://meddesk.qiubite.fun/admin -o /dev/null -w '%{http_code}'` | `200` |
| 9 | 后台脚本 | `curl -fsS --noproxy '*' https://meddesk.qiubite.fun/admin/app.js -o /dev/null -w '%{http_code}'` | `200` |
| 10 | TLS 证书 | `curl -fsS --noproxy '*' -o /dev/null -w '%{http_code}' https://meddesk.qiubite.fun/` | `200`，证书未过期 |
| 11 | 磁盘 / tmpfs | `ssh web 'df -hP / /tmp'` | 均 <75%，且 `/tmp` 无残留 `.bin` |
| 12 | 附件目录可写 | `ssh web 'sudo -u ops test -w /home/ops/service/meddesk/data && echo ok'` | `ok` |
| 13 | 二进制权限 | `ssh web 'ls -l /home/ops/service/meddesk/meddesk'` | `-rwxr-xr-x root root` |
| 14 | 旧标签页清理 | 通知用户关多余标签页 + 强刷 | 用户侧无旧 JS |

---

## 8. 备份与运维

### 8.1 数据库与附件

```bash
install -m 0750 deploy/scripts/backup.sh /usr/local/bin/meddesk-backup.sh
crontab -e
# 每日 03:30 备份，保留 30 天
30 3 * * * /usr/local/bin/meddesk-backup.sh >> /var/backups/meddesk/backup.log 2>&1
```

- 恢复用 `deploy/scripts/restore.sh`（**覆盖式操作，内置二次确认**，先 `--dry-run` 预览）。
- 异地容灾：crontab 追加 `rclone copy /var/backups/meddesk remote:meddesk-backup`。
- 应用内「设置 → 导出 JSON 备份」是服务器之外的最后一道保险，建议每月一次。

### 8.2 记账本每日 WebDAV 备份（v0.3.50 起）

- 计划时间 `MEDDESK_LEDGER_BACKUP_TIME`（默认 `02:00`，时区 `Asia/Shanghai`），由进程内调度器触发。
- 凭据以 `MEDDESK_SECRETBOX_KEY` 加密落库；**该密钥不在数据库里，数据库备份救不了它**，必须另有密码管理器副本。
- 同日幂等（`UNIQUE(user_id, run_date)` + 内存互斥），重复触发不会多出文件（多实例除外，见 3.2 已知限制）。

```bash
# 次日检查昨晚是否跑成
ssh web "journalctl -u meddesk --since today -o cat --no-pager | grep '记账本自动备份已执行'"
ssh web "journalctl -u meddesk --since today -o cat --no-pager | grep -E '记账本自动备份未执行|记账本备份失败'"
```

---

## 9. 监控与日志

```bash
# 应用日志（结构化 JSON，走 journald）
journalctl -u meddesk -f
# 只看错误
journalctl -u meddesk -p err --since today
```

```bash
# nginx 日志轮转
cat > /etc/logrotate.d/meddesk-nginx <<'EOF'
/var/log/nginx/meddesk.*.log {
    daily
    rotate 14
    compress
    missingok
    notifempty
    sharedscripts
    postrotate
        [ -f /var/run/nginx.pid ] && kill -USR1 `cat /var/run/nginx.pid`
    endscript
}
EOF
```

- 可选 `node_exporter` + `postgres_exporter` + Prometheus/Grafana；个人规模用 `systemctl status` + journald 关键字告警即可。
- 更细的告警阈值与指标口径见 [OPERATIONS.md](OPERATIONS.md)。

---

## 10. 故障排查表

| 现象 | 可能原因 | 处置 |
|---|---|---|
| 502 Bad Gateway | 应用未监听 / 崩溃循环 / **/tmp tmpfs 被撑满** | `systemctl status meddesk`；`journalctl -u meddesk -n 50`；`df -h /tmp`（被撑满时先 `rm -f /tmp/*.bin`） |
| 启动即退出 `status=203/EXEC` | 用 `scp` 直接覆盖了目标二进制，丢了执行位 | 改用 `sudo install -m 0755 <新文件> /home/ops/service/meddesk/meddesk` |
| 启动即退出 `status=203/SIGSYS` | seccomp `SystemCallFilter` 与内核不兼容 | 注释该行后逐段收紧 |
| `/health` 里 `db:"fail"` | PG 口令错 / 库未建 / PG 未监听 | 核对 `MEDDESK_DB_URL`；`sudo -u postgres psql -c '\l'` |
| `db:"ok" cache:"degraded"` | Redis 未启或被禁 | 预期行为（进程内降级），非故障 |
| PDF 导出返回 503 | 缺中文字体 | `deploy/scripts/get-font.sh` 或设 `MEDDESK_PDF_FONT_PATH` |
| 证书续期失败 | DNS API Token 失效 / 解析被改 | `acme.sh --renew -d meddesk.qiubite.fun --ecc --force` 看报错 |
| 上传返回 413 | nginx `client_max_body_size` 或 `MEDDESK_UPLOAD_MAX_BYTES` 超限 | 两处**同时**调大，nginx 值 ≥ 应用值 |
| 登录后立刻 401 | 服务器时钟漂移 | `timedatectl` 确认 NTP 已同步 |
| 发布后「数据没更新」/ 旧接口报错 | 旧标签页仍在跑旧前端 | 通知关多余标签页 + `Ctrl+F5` 强刷（见 5 节步骤 7） |
| 记账本备份报「备份功能不可用」 | 未配 `MEDDESK_SECRETBOX_KEY` | `openssl rand -base64 32` 生成后写入 `.meddesk.env` 并重启 |
| 记账本报「凭据无法解密」 | `MEDDESK_SECRETBOX_KEY` 被改过 | 无法恢复，只能让用户重新填写 WebDAV 凭据 |
| 记账本备份重复上传 | 多实例各跑一次（无选主） | 保持单实例；或加 `pg_advisory_xact_lock` / Redis 锁 |
| 迁移失败 `migration ... failed` | 表被手工改动 / 半途失败残留 | `SELECT * FROM schema_migrations;` 核对版本；修好后重启自动续跑 |
| Redis 启用时 429 异常频繁 | 多实例共享限流桶口径 | 确认所有实例 `MEDDESK_REDIS_ENABLED` 一致 |

---

## 11. 版本与变更记录

### v0.3.51（2026-10-30 02:17 生产部署并验证通过）

**部署信息**：`https://meddesk.qiubite.fun` · 应用端口 `:8089`（代码默认值 = 现网口径）· 二进制 sha256 `30f89a920b28cfccfa88e1bfb6aaae426aa82d7c2c7e652d2c6cc2703bfd8742` · 回滚备份 `meddesk.bak-20261030-122238`（= 上一版 v0.3.51 初版）、`meddesk.bak-20261030-021034`（= v0.3.50）。

**端口统一（2026-10-30 12:22）**

- 代码默认值 `MEDDESK_HTTP_ADDR` 由 `:8080` 改为 **`:8089`**，`MEDDESK_BASE_URL` 默认同步改为 `http://localhost:8089`。
- 同步统一 51 处：nginx `upstream`、systemd 单元注释、Compose 端口映射与 healthcheck、`install.sh` 的内联 env 与验证命令、`meddesk.env.example`、`README` / `CONTRACTS` / `API` / `DEPLOYMENT` / `DEVELOPMENT`。
- 目的：消除「模板 8080 / 现网 8089」错位——尤其避免 `.meddesk.env` 丢失时二进制回落到 8080、与 nginx upstream 不匹配导致 502。
- 生产验证：启动日志 `addr: ":8089"`、`ss` 显示 meddesk 监听 `8089`、`/api/v1/health` 返回 200。
- 保留未改：`internal/middleware/cors_test.go` 中的 `localhost:8080` 是 CORS 白名单测试字符串，与端口约定无关。

**缺陷修复（均为代码级，无接口破坏性变更）**

| # | 问题 | 影响 | 修复 |
|---|---|---|---|
| 1 | `MEDDESK_ADMIN_EMAILS` 在源码里内置了个人邮箱作为默认值 | 安全：管理员白名单不该有硬编码默认值 | 默认值改为**空名单**；启动时若为空会打 WARN（不阻断启动） |
| 2 | `MEDDESK_SIGNUP_ENABLED` 代码默认 `true` | 安全：公网部署容易忘记关注册 | 默认值改为 `false` |
| 3 | `Makefile` 用 `-X main.Version` 注入版本，但实际变量在 `internal/server` | **版本注入静默失效**，`meddesk -version` 永远显示 `0.1.0` | 改用全限定包路径；并在 `internal/server` 补齐 `Commit` / `BuildTime` 变量 |
| 4 | `PATCH /ledger/categories/:id` 无条件写入 `sortOrder` / `archived` | **静默丢数据**：只改分类名时排序被清零、归档状态被重置 | 引入 `CategoryPatch`（指针字段），nil 表示"本次不改"；零值仍可显式写入 |
| 5 | `GET /ledger/reimbursements` 的 `pendingTotal` 只累加首页 200 条 | 待报销 > 200 笔时，条数是全量、金额只是前 200 笔，**口径自相矛盾** | 改用 SQL 聚合（`SumRecords`）求全量合计 |
| 6 | 仓储层冲突错误（如同名分类）被映射成 `500 internal` | 前端拿到 internal，无法给用户可读提示 | `wrapRepoErr` 的类型断言改用 `errors.As`，冲突正确返回 `409 conflict` |

> 第 6 项是修第 4 项时连带发现的：`store.newConflict` 返回的是内部 `conflictError`（`Unwrap` 出 `*apierr.Error`），
> 而 `wrapRepoErr` 用的是直接类型断言 `err.(*apierr.Error)`，永远为 false → 掉进 500 分支。

**配置变更（升级必做）**

- ⚠ **必须在 `.meddesk.env` 显式配置 `MEDDESK_ADMIN_EMAILS`**。v0.3.51 起代码不再内置默认管理员邮箱；
  未配置时 `/admin` 将无人可登录，启动日志会出现 `管理后台配置告警`。
  从 v0.3.50 升级时该值以原先的默认值为准，本次部署已写入并验证后台可登录。
- `MEDDESK_SIGNUP_ENABLED` 建议显式写 `false`（默认值已改，但显式更安全）。

**验证结果（生产实测）**

- 服务 `active`；`meddesk -version` → `v0.3.51`（版本注入已生效）。
- 启动日志无「管理后台配置告警」→ 白名单与口令凭据均已正确配置。
- 记账本业务冒烟 30 项通过（CRUD / 分类 / 附件魔数校验 / 报销 / 四类导出 / 备份状态 / 越权）。
- 分类局部更新专项 12 项通过：只改名时 `sortOrder` 保持 7、`archived` 保持 `false`；
  显式 `archived:true` 生效；显式 `sortOrder:0` 可写入。
- 同名冲突专项：`PATCH` 改名为已存在名称 → `409 {"code":"conflict","message":"同名分类已存在"}`。
- 测试数据已全部清理（`pending` 账单数归零）。

**已知限制（未在本版处理）**

- 记账本备份调度仍为进程内 ticker，多实例会重复上传（同名覆盖）。
- 分页 `size` 上限 200。
- ZIP 导出的 `images/` 暂不能被导入还原。
- ~~模板端口与现网不一致（8080 / 8089）~~ → **已统一为 `:8089`**（代码默认值、样例、nginx、systemd、Compose、安装脚本、文档）。

### v0.3.50（2026-10-30 01:32 生产部署并验证通过）

**部署信息**：阿里云 ECS · `https://meddesk.qiubite.fun` · systemd 单二进制 · 应用端口 `8089` · 二进制 sha256 `63c553a1d0e1c11cb652ec594114c9085ff0e1381b56d076532393e429370979` · 备份 `meddesk.bak-20261030-013018`。

**新功能**

- **记账本模块上线**：账本 CRUD、分类管理（内置 16 条自动播种）、凭证附件、报销管理（待报销列表 / 批量标记 / 批量导出）、统计图表（月度汇总 + 7 日趋势 + 分类金额）、导出（HTML / XLSX / ZIP / JSON）与导入（merge / overwrite + 快照回滚）、每日 WebDAV 自动备份（凭据 AES-256-GCM 加密落库）。
- **管理后台**：`/admin`（白名单邮箱 + 独立后台口令可选，后台 token 与业务 token 分离 scope）。
- **家庭档案**：改名支持；修复档案合并产生的重复项。

**配置变更**

- 新增 `MEDDESK_SECRETBOX_KEY`（**必填，见 3.2**）、`MEDDESK_LEDGER_BACKUP_ENABLED` / `_TIME` / `_TZ`。

**运维注意**

- 首次启用记账本备份前必须先生成并离线备份 `MEDDESK_SECRETBOX_KEY`。
- 发布后需通知用户关多余标签页 + 强刷（旧 JS 会推旧数据）。
- 已知限制：多实例下记账本备份会重复上传（进程内 ticker 无选主）；分页 size 上限 200；ZIP 的 `images/` 暂不能被导入还原。

### 历史条目（摘要）

| 版本 | 时间 | 要点 |
|---|---|---|
| v0.3.49 | 2026-10-29 | 记账本开发期预发布：账本数据层与 CRUD 上线；修复 7 日趋势接口 500（`generate_series` 与筛选占位符冲突） |
| v0.3.4x 及更早 | — | 家庭档案与短码关联、健康工具五大类、改密后注销全部会话、WebDAV 导出等；细节以 `git log` 与 tag 为准（本节仅作索引，未逐条考证） |

---

## 附：Docker Compose 方案（路径 B）

> 适合测试/预发或快速起环境；生产主线是路径 A（systemd）。

```bash
# 1. 装 docker
curl -fsSL https://get.docker.com | sh

# 2. 准备环境变量（替换所有 <...> 占位符）
cp meddesk.env.example /home/ops/service/docker/meddesk/.env
chmod 600 /home/ops/service/docker/meddesk/.env

# 3. 只起中间件（PG + Redis）：应用仍在宿主机
cd /home/ops/service/docker/meddesk
docker compose -f deploy/docker-compose.middleware.yml up -d
docker compose -f deploy/docker-compose.middleware.yml ps

# 4. 全容器方案（app 也在容器内）
docker compose -f deploy/docker-compose.yml up -d --build

# 5. 宿主机 nginx 仍按 4.8 反代到 127.0.0.1:8089（TLS 终止在 nginx）

# 6. 冒烟
curl -fsS --noproxy '*' http://127.0.0.1:8089/api/v1/health
```

中间件 compose 变量（写在 compose 同目录 `.env`）：`POSTGRES_USER` / `POSTGRES_PASSWORD`（必填）/ `POSTGRES_DB` / `PG_PORT` / `REDIS_PASSWORD`（必填）/ `REDIS_PORT` / `REDIS_MAXMEMORY` / `TZ`。

要点：

- 数据在 `pgdata_mw` 卷中，`down` 不丢数据，`down -v` **会删库**（勿在生产执行）。
- compose 内 `MEDDESK_REDIS_ENABLED=true`（自带 redis）；不想要就把 `redis` 服务与 app 的 `depends_on`、Redis 环境变量一并删掉，应用自动降级。
- 上传密文目录在 `appdata` 卷中持久化；**删除卷 = 丢失全部附件**，先备份。
- 容器缺 tzdata 时记账本备份时区会自动退化为固定 +08:00（等价 `Asia/Shanghai`）。
- 镜像构建时会自动下载中文字体；离线环境先手动放字体，参考 [deploy/Dockerfile](../deploy/Dockerfile)。

---

> **文档与实现不一致清单（v0.3.51 已全部处理，留档备查）：**
> 1. ✅ `meddesk.env.example` 已补 `MEDDESK_SECRETBOX_KEY` 与 `MEDDESK_LEDGER_BACKUP_{ENABLED,TIME,TZ}`。
> 2. ✅ `meddesk.env.example` 已补 `MEDDESK_ADMIN_{EMAILS,SALT,PASSWORD_HASH,TTL}` 与 `MEDDESK_RECOVERY_KEY`。
> 3. ✅ 端口已全仓统一为 `:8089`（代码默认值、样例、nginx、systemd、Compose、install.sh、文档）。
> 4. ✅ `config.go`：`MEDDESK_ADMIN_EMAILS` 默认值改为**空名单**（原内置个人邮箱）、`MEDDESK_SIGNUP_ENABLED` 默认改为 `false`；空名单时启动打 WARN 但不阻断。
