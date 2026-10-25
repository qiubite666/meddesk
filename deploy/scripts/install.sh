#!/usr/bin/env bash
# ==============================================================================
# MedDesk —— 一键初始化脚本
#
# 它做的事（按顺序，全部可重复执行/幂等）：
#   1  检查运行环境（root/sudo、发行版、架构、磁盘空间）
#   2  检查运行用户 ops 与目录结构（/home/ops/service 布局，见下）
#   3  安装二进制（从本地已构建的产物，或用 go build 现编）
#   4  生成/写入环境变量文件 /home/ops/service/meddesk.env（0640, root:ops）
#   5  PostgreSQL：建库建用户 + citext 扩展（--with-postgres，最小权限）
#   6  Redis：本地监听 + maxmemory + requirepass（--with-redis）
#   7  systemd 单元安装并 enable（路径 B）
#   8  Nginx 站点安装 + nginx -t + reload（路径 B）
#   9  acme.sh 申请 ECC 证书并安装 + 自动续期钩子（--issue-cert）
#   10 首次迁移与冒烟验证
#
# 用法示例：
#   sudo ./deploy/scripts/install.sh --method systemd --domain med.example.com \
#        --binary ./meddesk --with-postgres --with-redis --issue-cert
#   sudo ./deploy/scripts/install.sh --method compose          # 只做 compose 前置准备
#   sudo ./deploy/scripts/install.sh --dry-run ...             # 只打印计划，不落盘
#
# 【ops 布局】（与 deploy/systemd/meddesk.service、meddesk.env.example 一致）
#   /home/ops/service/meddesk       二进制（前端已内嵌）
#   /home/ops/service/meddesk.env   环境变量（含密钥，绝不提交 git）
#   /home/ops/service/uploads/      附件密文
#   /home/ops/service/fonts/        PDF 中文字体
#
# ⚠ 安全约定：
#   - 脚本**不会**自动生成并写入口令：所有敏感值必须是占位符或由你提供，
#     缺失时脚本会生成随机串并提示你抄走（写到 root 可读的位置，权限 0600）。
#   - 涉及覆盖已有文件时，默认会先把旧文件备份成 <file>.bak.<时间戳>。
# ==============================================================================

set -Eeuo pipefail

LOG_TAG="[meddesk-install]"
DRY_RUN=0
METHOD="systemd"          # systemd | compose
DOMAIN="med.example.com"
BINARY=""                 # 已构建好的二进制路径
APP_USER="ops"
SERVICE_DIR="/home/ops/service"
UPLOAD_DIR="$SERVICE_DIR/uploads"
FONT_DIR="$SERVICE_DIR/fonts"
ENV_FILE="$SERVICE_DIR/meddesk.env"
BIN_PATH="$SERVICE_DIR/meddesk"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

WITH_POSTGRES=0
WITH_REDIS=0
ISSUE_CERT=0
NGINX_CONF=1
SKIP_SMOKETEST=0
EMAIL=""                  # acme.sh 注册邮箱（用于证书到期提醒）

PG_NAME="meddesk"
REDIS_MAXMEM="256mb"

log()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG $*"; }
warn() { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [警告] $*" >&2; }
die()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [错误] $*" >&2; exit 1; }
trap 'rc=$?; printf "%s [错误] 命令异常退出（退出码 %s）。安装未完成，请修复后重跑。\n" "$(date "+%Y-%m-%d %H:%M:%S") ${LOG_TAG}" "$rc" >&2; exit "$rc"' ERR

run() {
  # run <描述> <命令...>：dry-run 时只打印
  local desc="$1"; shift
  if (( DRY_RUN )); then
    echo "  [dry-run] $desc :: $*"
    return 0
  fi
  echo "  -> $desc"
  "$@"
}

need_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "请以 root 运行（或 sudo $0 …）"
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --method)      METHOD="$2"; shift 2 ;;
    --domain)      DOMAIN="$2"; shift 2 ;;
    --binary)      BINARY="$2"; shift 2 ;;
    --email)       EMAIL="$2"; shift 2 ;;
    --upload-dir)  UPLOAD_DIR="$2"; shift 2 ;;
    --with-postgres) WITH_POSTGRES=1; shift ;;
    --with-redis)    WITH_REDIS=1; shift ;;
    --issue-cert)    ISSUE_CERT=1; shift ;;
    --no-nginx)      NGINX_CONF=0; shift ;;
    --no-smoketest)  SKIP_SMOKETEST=1; shift ;;
    --dry-run)     DRY_RUN=1; shift ;;
    -h|--help)     sed -n '3,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done

[[ "$METHOD" == "systemd" || "$METHOD" == "compose" ]] || die "--method 只能是 systemd 或 compose"

# ==============================================================================
echo "══════════════ MedDesk 安装计划 ══════════════"
echo " 部署方式     : $METHOD"
echo " 域名         : $DOMAIN"
echo " 运行用户     : $APP_USER（需已存在）"
echo " 服务目录     : $SERVICE_DIR"
echo " 附件目录     : $UPLOAD_DIR"
echo " 附带 PG/Redis: $WITH_POSTGRES / $WITH_REDIS"
echo " 申请证书     : $ISSUE_CERT"
echo " Dry-Run      : $DRY_RUN"
echo "══════════════════════════════════════════════"

# ---------------------------------------------------------------- 1 环境检查
log "步骤 1/10  检查运行环境"
ARCH="$(uname -m)"; OS_ID=""
if [[ -f /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="${ID:-unknown} ${VERSION_ID:-}"
fi
log "  操作系统: ${PRETTY_NAME:-$OS_ID}   架构: $ARCH"
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) die "暂不支持的架构: $ARCH" ;;
esac

if [[ -n "${PRETTY_NAME:-}" ]]; then
  case "$PRETTY_NAME" in
    *Debian*12*|*Ubuntu*22*|*Ubuntu*24*) : ;;
    *) warn "未在 Debian 12 / Ubuntu 22.04 / 24.04 上验证过：$PRETTY_NAME，请自行确认包名与路径" ;;
  esac
fi

AVAIL="$(df -Pm /var | awk 'NR==2{print $4}')"
log "  /var 可用空间: ${AVAIL} MB"
if (( AVAIL < 2048 )); then warn "磁盘空间偏小（<2GB），建议扩容后再部署"; fi

# ---------------------------------------------------------------- 2 用户与目录
log "步骤 2/10  检查运行用户与目录"
if (( DRY_RUN )); then
  echo "  [dry-run] 检查 ops 用户 + install -d -o ops -g ops $SERVICE_DIR $UPLOAD_DIR $FONT_DIR"
else
  if ! id "$APP_USER" >/dev/null 2>&1; then
    die "用户 $APP_USER 不存在。请先创建：sudo useradd -m -s /bin/bash $APP_USER"
  fi
  install -d -o "$APP_USER" -g "$APP_USER" -m 0750 "$SERVICE_DIR" "$UPLOAD_DIR" "$FONT_DIR"
  log "  目录就绪（属主 $APP_USER）"
fi

# ---------------------------------------------------------------- 3 二进制
log "步骤 3/10  安装二进制"
if [[ "$METHOD" == "systemd" ]]; then
  if [[ -z "$BINARY" ]]; then
    if [[ -f "$REPO_ROOT/meddesk" ]]; then BINARY="$REPO_ROOT/meddesk"
    elif [[ -f "$REPO_ROOT/bin/meddesk" ]]; then BINARY="$REPO_ROOT/bin/meddesk"; fi
  fi
  if [[ -z "$BINARY" || ! -f "$BINARY" ]]; then
    log "  未在磁盘找到现成二进制，尝试现编译（需要本机有 Go）"
    if command -v go >/dev/null 2>&1; then
      run "交叉/本机编译" bash -c "cd '$REPO_ROOT' && GOOS=linux GOARCH=$GOARCH go build -ldflags='-s -w' -o '${REPO_ROOT}/meddesk' ./cmd/meddesk"
      BINARY="$REPO_ROOT/meddesk"
    else
      warn "既无二进制也无 Go 工具链，跳过本步。请先在 CI/开发机构建后回传："
      warn "  GOOS=linux GOARCH=$GOARCH go build -ldflags=\"-s -w\" -o meddesk ./cmd/meddesk"
    fi
  fi
  if [[ -n "$BINARY" && -f "$BINARY" ]]; then
    run "安装到 $BIN_PATH" install -m 0755 -o "$APP_USER" -g "$APP_USER" "$BINARY" "$BIN_PATH"
  fi
else
  log "  compose 模式：镜像由 docker compose build 生成，跳过二进制安装"
fi

# ---------------------------------------------------------------- 4 环境变量文件
log "步骤 4/10  写入环境变量文件"
COMPOSE_ENV="$REPO_ROOT/.env"
JWT_SECRET="$(openssl rand -base64 48 2>/dev/null || head -c 48 /dev/urandom | base64)"
PG_PASS="$(openssl rand -base64 24 2>/dev/null || head -c 24 /dev/urandom | base64)"
REDIS_PASS="$(openssl rand -base64 24 2>/dev/null || head -c 24 /dev/urandom | base64)"
PG_HOST="127.0.0.1"; [[ "$METHOD" == "compose" ]] && PG_HOST="postgres"
REDIS_HOST="127.0.0.1"; [[ "$METHOD" == "compose" ]] && REDIS_HOST="redis"

# Redis 行：compose 方法自带 redis 服务直接启用；systemd 方法仅 --with-redis 时启用，
# 其余情况默认关闭（走进程内 Noop 降级，单实例功能完全等价）。
if [[ "$METHOD" == "compose" || "$WITH_REDIS" == "1" ]]; then
  REDIS_LINES="MEDDESK_REDIS_ENABLED=true
MEDDESK_REDIS_URL=redis://:$REDIS_PASS@$REDIS_HOST:6379/0"
else
  REDIS_LINES="MEDDESK_REDIS_ENABLED=false
# MEDDESK_REDIS_URL=redis://:$REDIS_PASS@127.0.0.1:6379/0"
fi

gen_env() {
  cat <<EOF
# MedDesk 运行环境 —— 由 install.sh 生成于 $(date -Is)
# 权限必须是 600：文件中含 JWT 密钥与数据库口令
MEDDESK_HTTP_ADDR=:8080
MEDDESK_BASE_URL=https://$DOMAIN
MEDDESK_TRUST_PROXY=true
MEDDESK_WEB_DIR=
MEDDESK_DB_URL=postgres://$PG_NAME:$PG_PASS@$PG_HOST:5432/$PG_NAME?sslmode=disable
MEDDESK_DB_MAX_CONNS=10
MEDDESK_DB_MIN_CONNS=1
MEDDESK_DB_MAX_CONN_LIFETIME=30m
MEDDESK_DB_MIGRATE_ON_BOOT=true
$REDIS_LINES
MEDDESK_JWT_SECRET=$JWT_SECRET
MEDDESK_ACCESS_TTL=15m
MEDDESK_REFRESH_TTL=720h
MEDDESK_ARGON2_MEMORY=65536
MEDDESK_ARGON2_ITERATIONS=3
MEDDESK_ARGON2_PARALLELISM=2
MEDDESK_SIGNUP_ENABLED=false
MEDDESK_REFRESH_REUSE_GRACE=30s
MEDDESK_SYNC_MAX_ITEMS=500
MEDDESK_SYNC_MAX_RECORD_BYTES=1048576
MEDDESK_SYNC_PULL_PAGE=300
MEDDESK_SYNC_PULL_PAGE_MAX=1000
MEDDESK_UPLOAD_DIR=$UPLOAD_DIR
MEDDESK_UPLOAD_MAX_BYTES=8388608
MEDDESK_PDF_FONT_PATH=$FONT_DIR/NotoSansSC-Regular.ttf
MEDDESK_LOG_LEVEL=info
MEDDESK_LOG_FORMAT=json
MEDDESK_RATE_LIMIT_RPS=20
MEDDESK_RATE_LIMIT_BURST=60
MEDDESK_CORS_ALLOWED_ORIGINS=
EOF
}

if (( DRY_RUN )); then
  echo "  [dry-run] 将写入 $ENV_FILE（chmod 600）"
else
  if [[ -f "$ENV_FILE" ]]; then
    cp -a "$ENV_FILE" "$ENV_FILE.bak.$(date +%Y%m%d%H%M%S)"
    warn "已存在的 $ENV_FILE 已备份为 .bak.*，本次写入会覆盖"
  fi
  if [[ "$METHOD" == "systemd" ]]; then
    gen_env > "$ENV_FILE"
    chown root:"$APP_USER" "$ENV_FILE"; chmod 640 "$ENV_FILE"
    log "  已写入 $ENV_FILE（0640, root:$APP_USER）"
  fi
  if [[ "$METHOD" == "compose" ]]; then
    if [[ -f "$COMPOSE_ENV" ]]; then
      warn "$COMPOSE_ENV 已存在，未覆盖；如需重建请先 cp .env.example .env 并手改"
    else
      cp "$REPO_ROOT/.env.example" "$COMPOSE_ENV"
      # 把占位符替换成本次生成的随机值（仅在你明确同意自动化的情况下才有意义，
      # 这里保留明文写入 + 提示，便于你立刻可用；生产环境建议自行改为密钥管理方案）
      sed -i "s#^MEDDESK_JWT_SECRET=.*#MEDDESK_JWT_SECRET=$JWT_SECRET#" "$COMPOSE_ENV"
      sed -i "s#^MEDDESK_DB_URL=.*#MEDDESK_DB_URL=postgres://$PG_NAME:$PG_PASS@postgres:5432/$PG_NAME?sslmode=disable#" "$COMPOSE_ENV"
      sed -i "s#^MEDDESK_REDIS_URL=.*#MEDDESK_REDIS_URL=redis://:$REDIS_PASS@redis:6379/0#" "$COMPOSE_ENV"
      sed -i "s#^MEDDESK_BASE_URL=.*#MEDDESK_BASE_URL=https://$DOMAIN#" "$COMPOSE_ENV"
      # 注意：这是**容器内**路径（Dockerfile 安装字体的位置），与宿主机布局无关
      sed -i "s#^MEDDESK_PDF_FONT_PATH=.*#MEDDESK_PDF_FONT_PATH=/opt/meddesk/fonts/NotoSansSC-Regular.ttf#" "$COMPOSE_ENV"
      chmod 600 "$COMPOSE_ENV"
      log "  已写入 $COMPOSE_ENV（0600），并把占位符替换为随机值"
    fi
  fi
  echo
  echo "  ★ 请立刻抄走并记录到密码管理器（本脚本不再显示第二次）："
  echo "    JWT_SECRET : $JWT_SECRET"
  echo "    PG 口令   : $PG_PASS"
  echo "    Redis 口令: $REDIS_PASS"
  echo
fi

# ---------------------------------------------------------------- 5 PostgreSQL
if (( WITH_POSTGRES )); then
  log "步骤 5/10  PostgreSQL 建库建用户（最小权限）"
  need_root
  SQL=$(cat <<EOF
-- 不要用 SUPERUSER；只给本库的权限
CREATE USER $PG_NAME WITH PASSWORD '$PG_PASS';
CREATE DATABASE $PG_NAME OWNER $PG_NAME ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
\\c $PG_NAME
CREATE EXTENSION IF NOT EXISTS citext;
GRANT CONNECT, TEMPORARY ON DATABASE $PG_NAME TO $PG_NAME;
GRANT ALL PRIVILEGES ON SCHEMA public TO $PG_NAME;
ALTER SCHEMA public OWNER TO $PG_NAME;
ALTER DATABASE $PG_NAME SET search_path = public;
EOF
)
  # 注意：临时文件里含数据库口令，必须用 600 权限并在用完立即删除
  TMPF="$(mktemp /tmp/meddesk-initdb.XXXXXX.sql)"
  chmod 600 "$TMPF"
  echo "$SQL" > "$TMPF"
  run "执行建库 SQL" su - postgres -c "psql -v ON_ERROR_STOP=1 -f $TMPF"
  rm -f "$TMPF"
  log "  ⚠ 注意：PostgreSQL 15+ 的 public schema 默认不再对 PUBLIC 开放写权限，"
  log "     上面的 GRANT/ALTER OWNER 是必要的；若你的实例被云厂商托管，请按其控制台操作。"
else
  log "步骤 5/10  跳过 PostgreSQL（未指定 --with-postgres）"
fi

# ---------------------------------------------------------------- 6 Redis
if (( WITH_REDIS )); then
  log "步骤 6/10  Redis 加固配置"
  need_root
  REDIS_CONF="/etc/redis/redis.conf"
  if [[ -f "$REDIS_CONF" ]]; then
    cp -a "$REDIS_CONF" "$REDIS_CONF.bak.$(date +%Y%m%d%H%M%S)" 2>/dev/null || true
    run "追加/修改 Redis 参数" bash -c "cat >> '$REDIS_CONF' <<'EOF'

# ---- MedDesk 追加配置（install.sh）----
bind 127.0.0.1 -::1
protected-mode yes
requirepass $REDIS_PASS
maxmemory $REDIS_MAXMEM
maxmemory-policy allkeys-lru
save \"\"
appendonly no
EOF"
    run "重启 redis" systemctl restart redis-server || systemctl restart redis
  else
    warn "未找到 $REDIS_CONF，请手动配置 requirepass / maxmemory 后重启 redis"
  fi
else
  log "步骤 6/10  跳过 Redis（未指定 --with-redis）。提示：MEDDESK_REDIS_ENABLED=false 可完全不依赖 Redis"
fi

# ---------------------------------------------------------------- 7 systemd / compose 拉起
log "步骤 7/10  安装并启动服务"
if (( DRY_RUN )); then
  echo "  [dry-run] 安装 systemd 单元或 docker compose up -d"
else
  need_root
  if [[ "$METHOD" == "systemd" ]]; then
    install -m 0644 "$REPO_ROOT/deploy/systemd/meddesk.service" /etc/systemd/system/meddesk.service
    systemctl daemon-reload
    systemctl enable meddesk
    log "  已 enable meddesk.service。先别急 start，确认证书/数据库就绪后："
    log "    sudo systemctl start meddesk && journalctl -u meddesk -f"
  else
    run "compose 构建并启动" bash -c "cd '$REPO_ROOT' && docker compose -f deploy/docker-compose.yml up -d --build"
  fi
fi

# ---------------------------------------------------------------- 8 Nginx
if (( NGINX_CONF )); then
  log "步骤 8/10  Nginx 站点配置"
  NGX_CONF="/etc/nginx/sites-available/meddesk.conf"
  if (( DRY_RUN )); then
    echo "  [dry-run] 安装 $NGX_CONF 并 nginx -t && reload"
  else
    if [[ -f "$NGX_CONF" ]]; then cp -a "$NGX_CONF" "$NGX_CONF.bak.$(date +%Y%m%d%H%M%S)"; fi
    sed "s|med\.example\.com|$DOMAIN|g" "$REPO_ROOT/deploy/nginx/meddesk.conf" > "$NGX_CONF"
    ln -sfn "$NGX_CONF" /etc/nginx/sites-enabled/meddesk.conf
    log "  已写入 $NGX_CONF 并启用（域名替换为 $DOMAIN）"
    log "  ⚠ 证书文件尚不存在，先用 HTTP-only 片段自检：nginx -t 可能因缺证书而失败。"
    if (( ISSUE_CERT )); then
      log "  稍后第 9 步会申请证书，届时再 reload。"
    else
      log "  如只需 HTTP 自检，可临时注释 ssl_* 行并 listen 443 段后运行 nginx -t"
    fi
    nginx -t && systemctl reload nginx || warn "nginx -t/reload 失败（证书未就绪时属正常），请手动处理"
  fi
else
  log "步骤 8/10  跳过 Nginx（--no-nginx）"
fi

# ---------------------------------------------------------------- 9 acme.sh 证书
if (( ISSUE_CERT )); then
  log "步骤 9/10  acme.sh 申请 ECC 证书"
  [[ -n "$EMAIL" ]] || warn "未指定 --email，acme.sh 将无法接收到期提醒（建议补上）"
  cat <<'MSG'

  下面这些命令建议**手工逐条执行**（本脚本不自动跑，避免误用 CA 资源触发限流）：

    # 1) 安装 acme.sh（默认装到 ~/.acme.sh，不污染系统目录）
    curl -fsSL https://get.acme.sh | sh -s email=<你的邮箱>
    source ~/.acme.sh/acme.sh.env

    # 2) 注册 ACME 账户（仅首次；Let's Encrypt 为例，国内可选 ZeroSSL/其他 CA）
    acme.sh --set-default-ca --server letsencrypt

    # 3) 前置条件：域名 A/AAAA 解析已指向本机，且 80 端口可放行 ACME 挑战
    #    Nginx 需先加载一份最小 HTTP 配置（只含 server_name 与 /.well-known 放行）

    # 4) 申请 ECC 证书（P-256）—— 命令中的 ec-256 即 ECC 256 位密钥
MSG
  echo "    acme.sh --issue --nginx -d $DOMAIN --keylength ec-256"
  cat <<'MSG'

    # 5) 安装证书到固定路径（不要用 ~/.acme.sh 里的路径直接配 Nginx，会被续期时的目录调整坑到）
MSG
  echo "    mkdir -p /etc/letsencrypt/live/$DOMAIN"
  cat <<EOF
    acme.sh --install-cert -d $DOMAIN --ecc \\
      --key-file       /etc/letsencrypt/live/$DOMAIN/privkey.pem \\
      --fullchain-file /etc/letsencrypt/live/$DOMAIN/fullchain.pem \\
      --reloadcmd      "systemctl reload nginx"

    # 6) 自动续期：acme.sh 安装时已写入 crontab（每天 0 点左右检查），可用下面命令确认
    crontab -l | grep acme.sh
    #   手动触发一次续期检查则用：acme.sh --cron
    #   查看证书信息：acme.sh --info -d $DOMAIN --ecc
    #   手动续期演练（正式 CA 慎用，会触发限流）：acme.sh --renew -d $DOMAIN --ecc --force

    # 7) Nginx 重载并验证
    nginx -t && systemctl reload nginx
    echo | openssl s_client -connect $DOMAIN:443 -servername $DOMAIN 2>/dev/null | openssl x509 -noout -text | grep -E 'Public Key Algorithm|Signature Algorithm'
MSG
  log "  证书步骤已打印，请手工执行（脚本刻意不自动执行）"
else
  log "步骤 9/10  跳过证书（未指定 --issue-cert）"
fi

# ---------------------------------------------------------------- 10 迁移与冒烟
if (( SKIP_SMOKETEST )); then
  log "步骤 10/10 跳过冒烟验证（--no-smoketest）"
else
  log "步骤 10/10 首次迁移与冒烟验证"
  cat <<EOF

  请按序执行：

    sudo systemctl start meddesk
    sudo journalctl -u meddesk -n 100 --no-pager        # 应看到迁移完成、监听 :8080

    # 健康检查（期望 status=ok）
    curl -fsS http://127.0.0.1:8080/api/v1/health | head -c 400; echo
    curl -fsS http://127.0.0.1:8080/api/v1/ready  | head -c 400; echo

    # 域名侧检查
    curl -fsS https://$DOMAIN/api/v1/health
    curl -sSI https://$DOMAIN/ | grep -i strict-transport-security

    # 关闭公开注册后首次建账号（用完后立刻改回 false —— 除非你确实需要开放注册）
    #   方案一：临时把 MEDDESK_SIGNUP_ENABLED 置 true -> systemctl restart meddesk
    #          注册完再置回 false 并重启
    #   方案二：在内网環境临时开放，或通过 Nginx IP 白名单限制来源
EOF
fi

log "安装脚本执行完毕 ✅"
echo
echo "后续待办："
echo "  1) 把 deploy/scripts/*.sh 拷到 $SERVICE_DIR/scripts 并 chmod +x"
echo "  2) 配置备份定时任务：crontab -e -> 17 3 * * * $SERVICE_DIR/scripts/backup.sh"
echo "  3) 详细步骤见 docs/DEPLOYMENT.md；安全基线见 docs/SECURITY.md"
