# MedDesk · 部署文档（从零到上线）

> 适用对象：单台云服务器（Debian 12 / Ubuntu 22.04，2C4G 起步，20GB+ 系统盘）。
> 全文域名以 `med.example.com` 示例，部署前全文替换为你的真实域名。
> 两条路径任选其一：**路径 A：systemd 裸机部署（推荐，资源占用最低）**；**路径 B：Docker Compose（最快起环境）**。

## 目录

- [0. 两条路径怎么选](#0-两条路径怎么选)
- [1. 系统准备](#1-系统准备)
- [2. PostgreSQL 18（两种方式二选一）](#2-postgresql-18两种方式二选一)
- [3. Redis 7（可选；启用时同样两种方式二选一）](#3-redis-7可选启用时同样两种方式二选一)
- [4. 编译安装二进制](#4-编译安装二进制)
- [5. systemd 单元](#5-systemd-单元)
- [6. Nginx 与 TLS 证书](#6-nginx-与-tls-证书)
- [7. 首次迁移与冒烟](#7-首次迁移与冒烟)
- [8. 备份策略](#8-备份策略)
- [9. 监控与日志](#9-监控与日志)
- [10. 升级与回滚](#10-升级与回滚)
- [11. 故障排查表](#11-故障排查表)
- [附：Docker Compose 一键方案（路径 B）](#附docker-compose-一键方案路径-b)

---

## 0. 两条路径怎么选

| | 路径 A：systemd 裸机 | 路径 B：Docker Compose |
|---|---|---|
| 适用 | 长期自托管、想精细控制资源与安全加固 | 快速验证、迁移服务器 |
| 组件 | PG18 裸装 + 二进制 + Nginx（宿主机） | postgres + redis + app 三个容器 + 宿主机 Nginx |
| Redis | **默认不装**（进程内降级，功能等价） | compose 自带 redis，默认启用 |
| 升级 | 替换二进制 + `systemctl restart` | `docker compose up -d --build` |
| 资源 | 约 200–350MB 常驻 | 约 450–700MB 常驻（多一层容器开销） |

安全加固要点（systemd：`NoNewPrivileges`、`ProtectSystem=strict`、seccomp 白名单）已写在
[deploy/systemd/meddesk.service](../deploy/systemd/meddesk.service)，本目录文件可直接复制使用。

---

## 1. 系统准备

```bash
# 以 root 执行；Debian 12 / Ubuntu 22.04
apt update && apt -y upgrade
apt -y install curl git unzip ca-certificates gnupg ufw

# 防火墙：只放行 SSH 与 Web（先确认 SSH 端口再启用！）
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp && ufw --force enable

# 时钟同步（JWT 校验对时钟漂移敏感，见第 11 节排查表）
timedatectl set-ntp true && timedatectl status | grep -E 'synchronized|NTP'
```

> **DNS 前置**：把 `med.example.com` 的 A/AAAA 记录指到本机公网 IP，等待解析生效再申请证书。

---

## 2. PostgreSQL 18（两种方式二选一）

> 版本说明：当前最新稳定大版本为 **PostgreSQL 18**（18.x 小版本滚动更新，如 18.6）。
> 项目迁移 SQL 未使用任何版本特有特性，**16 / 17 / 18 均可运行**，下文以 18 为例。

### 2.1 方式 A：宿主机安装（apt + systemd，资源占用最低）

```bash
# 官方 PGDG 源（Debian 12 示例；Ubuntu 把 bookworm 换成 jammy）
install -d /usr/share/postgresql-common/pgdg && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
  | gpg --dearmor -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
  > /etc/apt/sources.list.d/pgdg.list
apt update && apt -y install postgresql-18   # PGDG 源同时提供 16/17/18，按需选择

# 建角色与库（最小权限：不授 SUPERUSER / CREATEDB）
sudo -u postgres psql <<'SQL'
CREATE ROLE meddesk LOGIN PASSWORD '<请替换为随机口令，openssl rand -base64 24>';
CREATE DATABASE meddesk OWNER meddesk;
\c meddesk
-- citext 用于邮箱大小写不敏感唯一；云厂商禁扩展时迁移会自动退化为 lower(email) 唯一索引
CREATE EXTENSION IF NOT EXISTS citext;
SQL
```

小机调优参数（2C4G）见 [deploy/postgres-tuning.conf](../deploy/postgres-tuning.conf)，
复制到 `/etc/postgresql/18/main/conf.d/meddesk.conf` 后 `systemctl restart postgresql`。
PG 默认只监听 `127.0.0.1`，应用与库同机时**不要**开放 `listen_addresses = '*'`。

### 2.2 方式 B：docker compose（不动系统包，升级/迁移方便）

配置文件：[deploy/docker-compose.middleware.yml](../deploy/docker-compose.middleware.yml)——
只起 **PG18 + Redis7** 两个容器（应用跑在宿主机），口令/端口全部走环境变量，必填缺失时直接拒绝启动。

```bash
cd /home/ops/service/compose   # 建议目录；也可直接在 deploy/ 下执行
# ① 准备 .env（与 compose 文件同目录，权限 600）
cat > .env <<'EOF'
POSTGRES_USER=meddesk
POSTGRES_PASSWORD=<openssl rand -base64 24 的输出>
POSTGRES_DB=meddesk
PG_PORT=5432
REDIS_PASSWORD=<openssl rand -base64 24 的输出>
REDIS_PORT=6379
REDIS_MAXMEMORY=256mb
TZ=Asia/Shanghai
EOF
chmod 600 .env

# ② 起服务（数据持久化在 docker 卷 pgdata_mw；首次启动自动安装 citext）
docker compose -f deploy/docker-compose.middleware.yml up -d
docker compose -f deploy/docker-compose.middleware.yml ps     # healthy 即就绪

# ③ 验证
docker exec -it meddesk-pg psql -U meddesk -d meddesk -c 'SELECT version();'
# 常用运维
docker compose -f deploy/docker-compose.middleware.yml logs -f postgres
docker compose -f deploy/docker-compose.middleware.yml down          # 停止，保留数据
```

要点：数据在 `pgdata_mw` 卷中，`down` 不丢数据，`down -v` **会删库**（勿在生产执行）；
备份用第 8 节的 `pg_dump`（容器方式加 `docker exec meddesk-pg` 前缀）；
端口仅绑定 `127.0.0.1`，应用必须与容器同机。

---

## 3. Redis 7（可选；启用时同样两种方式二选一）

MedDesk 默认 `MEDDESK_REDIS_ENABLED=false`，走进程内 Noop 降级（限流/失败计数/吊销名单在单进程内生效），**单实例部署无需 Redis**。仅多副本或需要跨进程共享时才启用：

### 3.1 方式 A：宿主机安装

```bash
apt -y install redis-server
# /etc/redis/redis.conf 关键项：
#   bind 127.0.0.1
#   maxmemory 256mb
#   maxmemory-policy allkeys-lru
#   requirepass <请替换为随机口令>
systemctl restart redis-server
```

### 3.2 方式 B：docker compose

[deploy/docker-compose.middleware.yml](../deploy/docker-compose.middleware.yml) **已同时包含 Redis**（`requirepass`/`maxmemory`/`allkeys-lru` 由 `.env` 控制，纯缓存语义不落盘）——按 2.2 起一次即同时获得 PG 与 Redis。若只想要 Redis 不要 PG：

```bash
docker compose -f deploy/docker-compose.middleware.yml up -d redis
```

### 3.3 环境变量与连接配置对照表

**compose 侧变量**（写在 compose 文件同目录的 `.env`，方式 B 专用）：

| 变量 | 默认 | 是否必填 | 说明 |
|---|---|---|---|
| `POSTGRES_USER` | `meddesk` | 否 | PG 业务角色名（也是超级用户，容器内初始化用） |
| `POSTGRES_PASSWORD` | — | **是** | `openssl rand -base64 24`；缺失时 compose 拒绝启动 |
| `POSTGRES_DB` | `meddesk` | 否 | 库名 |
| `PG_PORT` | `5432` | 否 | 宿主机映射端口（仅 127.0.0.1） |
| `REDIS_PASSWORD` | — | **是** | `openssl rand -base64 24` |
| `REDIS_PORT` | `6379` | 否 | 宿主机映射端口 |
| `REDIS_MAXMEMORY` | `256mb` | 否 | 内存上限，超限按 allkeys-lru 淘汰 |
| `TZ` | `Asia/Shanghai` | 否 | 容器时区 |

**应用侧变量**（`/home/ops/service/.meddesk.env`，两种方式只是连接串不同）：

| 变量 | 方式 A（宿主机安装） | 方式 B（docker compose） |
|---|---|---|
| `MEDDESK_DB_URL` | `postgres://meddesk:<PG口令>@127.0.0.1:5432/meddesk?sslmode=disable` | `postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@127.0.0.1:$PG_PORT/$POSTGRES_DB?sslmode=disable` |
| `MEDDESK_REDIS_ENABLED` | 启用 Redis 时 `true` | 同左 |
| `MEDDESK_REDIS_URL` | `redis://:<Redis口令>@127.0.0.1:6379/0` | `redis://:$REDIS_PASSWORD@127.0.0.1:$REDIS_PORT/0` |

注意：口令若含 `@ : / # ?` 等 URL 保留字符需 URL 编码（或直接用 base64 输出里不含保留字符的变体）；
改完 `.meddesk.env` 执行 `systemctl restart meddesk` 生效。
Redis 里没有任何不可替代的数据；挂掉或关闭时应用自动降级，不影响业务数据。

---

## 4. 编译安装二进制

```bash
# ---- 本机交叉编译（推荐，服务器无需装 Go）----
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o meddesk ./cmd/meddesk
# 带 git 版本号：
#   go build -ldflags="-s -w -X main.version=$(git describe --tags --always)" ...

# ---- 传到服务器并安装 ----
scp meddesk root@<服务器IP>:/tmp/

ssh root@<服务器IP> '
set -e
# 运行用户 ops（若尚未创建：useradd -m -s /bin/bash ops）
id ops >/dev/null 2>&1 || useradd -m -s /bin/bash ops
install -d -o ops -g ops -m 0750 /home/ops/service /home/ops/service/uploads /home/ops/service/fonts
install -m 0755 /tmp/meddesk /home/ops/service/meddesk
# 中文字体（PDF 导出必需；缺失时导出返回 503，其余功能不受影响）
bash deploy/scripts/get-font.sh   # 下载 Noto Sans SC 到 /home/ops/service/fonts/
'
```

写环境变量文件（也可直接跑 `deploy/scripts/install.sh` 自动生成，含 `--with-postgres --with-redis --issue-cert` 可选开关）：

```bash
cat > /home/ops/service/.meddesk.env <<EOF
MEDDESK_HTTP_ADDR=:8080
MEDDESK_BASE_URL=https://med.example.com
MEDDESK_TRUST_PROXY=true
MEDDESK_DB_URL=postgres://meddesk:<数据库口令>@127.0.0.1:5432/meddesk?sslmode=disable
MEDDESK_REDIS_ENABLED=false
MEDDESK_JWT_SECRET=<openssl rand -base64 48 生成，至少 32 字节>
MEDDESK_SIGNUP_ENABLED=false
MEDDESK_PDF_FONT_PATH=/home/ops/service/fonts/NotoSansSC-Regular.ttf
MEDDESK_UPLOAD_DIR=/home/ops/service/uploads
MEDDESK_LOG_FORMAT=json
TZ=Asia/Shanghai
EOF
chown root:ops /home/ops/service/.meddesk.env && chmod 640 /home/ops/service/.meddesk.env
```

> `MEDDESK_SIGNUP_ENABLED=false` 表示注册仅限首个账号走命令行开启；个人自托管建议：注册第一个账号后立即关闭公开注册（在环境变量中改回 `false` 并重启）。

---

## 5. systemd 单元

```bash
cp deploy/systemd/meddesk.service /etc/systemd/system/meddesk.service
systemctl daemon-reload
systemctl enable --now meddesk
systemctl status meddesk --no-pager | head -12
journalctl -u meddesk -n 20 --no-pager
```

表结构由应用启动时自动迁移创建（`MEDDESK_DB_MIGRATE_ON_BOOT=true`），无需手工执行 SQL。

---

## 6. Nginx 与 TLS 证书

```bash
apt -y install nginx
cp deploy/nginx/meddesk.conf /etc/nginx/sites-available/meddesk.conf
# 全文替换域名（conf 中 server_name 共 3 处）
sed -i 's/med\.example\.com/你的真实域名/g' /etc/nginx/sites-available/meddesk.conf
ln -sf /etc/nginx/sites-available/meddesk.conf /etc/nginx/sites-enabled/
rm -f /etc/nginx/sites-enabled/default
nginx -t
```

用 **acme.sh 申请 ECC 证书**（与你现有 qiubite.fun 的管理方式一致）：

```bash
curl https://get.acme.sh | sh -s email=you@example.com
source ~/.bashrc
# 若 80 端口已被 nginx 占用且域名已解析到本机：
acme.sh --issue --nginx -d med.example.com --keylength ec-256
# 安装到 nginx conf 期望的路径（与 deploy/nginx/meddesk.conf 第 97-98 行一致）：
acme.sh --install-cert -d med.example.com --ecc \
  --fullchain-file /etc/letsencrypt/live/med.example.com/fullchain.pem \
  --key-file       /etc/letsencrypt/live/med.example.com/privkey.pem \
  --reloadcmd      "nginx -s reload"
# acme.sh 已自动写入 crontab 做续期，续期成功会触发上面的 reloadcmd
nginx -s reload
```

---

## 7. 首次迁移与冒烟

```bash
# 健康检查（应返回 {"status":"ok","version":"...","db":"ok",...}）
curl -fsS https://med.example.com/api/v1/health
# 就绪检查（db/cache 连通性）
curl -fsS https://med.example.com/api/v1/ready

# 首个账号注册（sign up 开启期间；或临时把 MEDDESK_SIGNUP_ENABLED=true 重启后注册）
curl -fsS https://med.example.com/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","passwordHash":"<客户端PBKDF2结果base64>","kdfSalt":"...","wrappedDek":"...","dekIv":"..."}'
```

浏览器打开 `https://med.example.com`，应看到登录界面；注册/登录后进入「用药提醒」视图。

---

## 8. 备份策略

用 [deploy/scripts/backup.sh](../deploy/scripts/backup.sh)（`pg_dump -Fc` 自定义格式 + 上传目录打包 + 保留策略）：

```bash
install -m 0750 deploy/scripts/backup.sh /usr/local/bin/meddesk-backup.sh
crontab -e
# 每日 03:30 备份，保留 30 天
30 3 * * * /usr/local/bin/meddesk-backup.sh >> /home/ops/service/backup.log 2>&1
```

异地容灾建议：crontab 里追加一步 `rclone copy /var/backups/meddesk remote:meddesk-backup`（或 rsync 到另一台机）。
恢复用 [deploy/scripts/restore.sh](../deploy/scripts/restore.sh)（**覆盖式操作，脚本内置二次确认**，先 `--dry-run` 预览）。

> 数据安全兜底：应用内「设置 → 导出 JSON 备份」可导出明文快照，作为服务器之外的最后一道保险（建议每月一次存本地加密盘）。

---

## 9. 监控与日志

```bash
# 应用日志（结构化 JSON）：journald 持久化
journalctl -u meddesk -f --output=json | jq -r '."\u001b"' 2>/dev/null || journalctl -u meddesk -f
# Nginx 日志轮转（logrotate）
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

可选：`node_exporter` + `postgres_exporter` + Prometheus/Grafana 做主机与数据库监控；
个人规模用 `systemctl status` + journald 关键字告警（如 `logcheck` 或 Server酱推送 `journalctl -p err`）即可。

---

## 10. 升级与回滚

```bash
# ---- 升级 ----
cp /home/ops/service/meddesk /home/ops/service/meddesk.bak.$(date +%F)   # 保留旧版
install -m 0755 meddesk-new /home/ops/service/meddesk
systemctl restart meddesk && curl -fsS https://med.example.com/api/v1/health

# ---- 回滚 ----
install -m 0755 /home/ops/service/meddesk.bak.<日期> /home/ops/service/meddesk
systemctl restart meddesk
```

- 迁移文件只增不改、向前兼容（`schema_migrations` 幂等），旧二进制兼容新表结构，可直接回滚。
- 若新版本迁移新增了表，回滚后旧版忽略多余表，不影响运行；**不建议**手工 DROP。

---

## 11. 故障排查表

| 现象 | 可能原因 | 处置 |
|---|---|---|
| 502 Bad Gateway | 应用未监听 `:8080` / 崩溃循环 | `systemctl status meddesk`；`journalctl -u meddesk -n 50` |
| `curl /api/v1/health` 里 `db:"fail"` | PG 口令错 / 库未建 / PG 未监听 | 核对 `MEDDESK_DB_URL`；`sudo -u postgres psql -c '\l'` |
| `db:"ok" cache:"degraded"` | Redis 未启或被禁 | 预期行为（进程内降级），非故障 |
| PDF 导出返回 503 | 缺中文字体 | `deploy/scripts/get-font.sh` 或设 `MEDDESK_PDF_FONT_PATH` 指向有效 TTF |
| 证书续期失败 | 80 端口被防火墙拦 / DNS 解析漂移 | `acme.sh --renew -d med.example.com --ecc --force` 看具体报错 |
| 上传返回 413 | Nginx `client_max_body_size` 或应用 `MEDDESK_UPLOAD_MAX_BYTES` 超限 | 两处上限需**同时**调大，Nginx 值 ≥ 应用值 |
| 登录后立刻 401 | 服务器时钟漂移导致 JWT 过期判定异常 | `timedatectl` 确认 NTP 已同步 |
| 启动即退出 status=203/SIGSYS | seccomp `SystemCallFilter` 与内核不兼容 | 注释该行后逐段收紧（见 unit 文件注释） |
| 迁移失败 `migration ... failed` | 表被手工改动 / 半途失败残留 | `SELECT * FROM schema_migrations;` 核对版本；修复后重启自动续跑 |
| Redis 启用时 429 异常频繁 | 多实例共享限流桶口径 | 确认所有实例 `MEDDESK_REDIS_ENABLED` 一致 |

---

## 附：Docker Compose 一键方案（路径 B）

```bash
# 1. 装 docker（或使用国内镜像源）
curl -fsSL https://get.docker.com | sh

# 2. 准备环境变量
cp meddesk.env.example .env    # 替换所有 <...> 占位符（含 PG/Redis 口令）

# 3. 构建并启动（postgres + redis + app，全部只绑 127.0.0.1）
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml ps

# 4. 冒烟
curl -fsS http://127.0.0.1:8080/api/v1/health

# 5. 宿主机 Nginx 仍按第 6 节配置反代到 127.0.0.1:8080（TLS 终止在 Nginx）
```

要点：

- compose 内 `MEDDESK_REDIS_ENABLED=true`（自带 redis 服务）；不想要 Redis 就把 `redis` 服务与 app 的 `depends_on`/Redis 环境变量一并删掉，应用自动降级。
- 上传密文目录在 `appdata` 卷中持久化；**删除卷 = 丢失全部药物照片附件**，操作前先备份。
- 镜像构建时自动下载中文字体；离线环境请先手动放置字体，参考 [deploy/Dockerfile](../deploy/Dockerfile) 注释。
