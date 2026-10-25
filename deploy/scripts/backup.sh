#!/usr/bin/env bash
# ==============================================================================
# MedDesk —— 备份脚本
#   1) PostgreSQL：pg_dump 自定义格式（-Fc，可并行恢复、可选择性还原）
#   2) 上传附件目录（MEDDESK_UPLOAD_DIR，全是密文 .bin）：tar 打包
#   3) 计算 SHA-256 校验和；可选异地拷贝（rsync / rclone）
#   4) 按保留天数滚动删除旧备份（默认 30 天）
#
# 用法：
#   ./deploy/scripts/backup.sh                    # 全量备份（DB + 附件）
#   ./deploy/scripts/backup.sh --db-only          # 只备份数据库
#   ./deploy/scripts/backup.sh --files-only       # 只备份附件目录
#   ./deploy/scripts/backup.sh --dest /mnt/bk     # 指定备份根目录
#   ./deploy/scripts/backup.sh --keep-days 30     # 自定义保留天数
#   ./deploy/scripts/backup.sh --remote user@host:/backup/meddesk   # 异地 rsync
#   ./deploy/scripts/backup.sh --dry-run          # 只打印将执行的操作，不落盘
#
# 定时任务（每天 03:17）：
#   17 3 * * * /home/ops/service/meddesk/scripts/backup.sh >> /home/ops/service/meddesk/backup.log 2>&1
#
# ⚠ 重要提醒：
#   数据库备份里只包含**密文**（iv/ct）与账户元数据，**没有**用户的口令派生因子之外的东西。
#   用户的明文数据只存在于浏览器；故除服务端备份外，务必提醒用户定期做
#   「设置 → 导出 JSON 备份」，否则密钥丢失 = 数据不可恢复。
# ==============================================================================

set -Eeuo pipefail

# ---------------------------------------------------------------- 默认配置
DEST="${BACKUP_DEST:-/var/backups/meddesk}"
UPLOAD_DIR="${MEDDESK_UPLOAD_DIR:-/home/ops/service/meddesk/data}"
KEEP_DAYS="${BACKUP_KEEP_DAYS:-30}"
DB_URL="${MEDDESK_DB_URL:-postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable}"
REMOTE="${BACKUP_REMOTE:-}"          # 例：user@host:/backup/meddesk
REMOTE_TYPE="${BACKUP_REMOTE_TYPE:-rsync}"
LOG_TAG="[meddesk-backup]"
MODE="all"                            # all | db | files
DRY_RUN=0

PGDUMP_BIN="${PGDUMP_BIN:-pg_dump}"
TAR_BIN="${TAR_BIN:-tar}"
RSYNC_BIN="${RSYNC_BIN:-rsync}"
RCLONE_BIN="${RCLONE_BIN:-rclone}"
SHA_BIN="${SHA_BIN:-sha256sum}"

usage() {
  sed -n '3,30p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

log()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG $*"; }
warn() { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [警告] $*" >&2; }
die()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [错误] $*" >&2; exit 1; }

trap 'rc=$?; printf "%s [错误] 命令异常退出，退出码 %s，备份中断。请检查上面最近一条输出。\n" "$(date "+%Y-%m-%d %H:%M:%S") ${LOG_TAG}" "$rc" >&2; exit "$rc"' ERR

# ---------------------------------------------------------------- 参数解析
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dest)        DEST="${2:?--dest 需要参数}"; shift 2 ;;
    --upload-dir)  UPLOAD_DIR="${2:?--upload-dir 需要参数}"; shift 2 ;;
    --db-url)      DB_URL="${2:?--db-url 需要参数}"; shift 2 ;;
    --keep-days)   KEEP_DAYS="${2:?--keep-days 需要参数}"; shift 2 ;;
    --remote)      REMOTE="${2:?--remote 需要参数}"; shift 2 ;;
    --remote-type) REMOTE_TYPE="${2:?--remote-type 需要参数(rsync|rclone)}"; shift 2 ;;
    --db-only)     MODE="db"; shift ;;
    --files-only)  MODE="files"; shift ;;
    --dry-run)     DRY_RUN=1; shift ;;
    -h|--help)     usage ;;
    *)             die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done

[[ "$KEEP_DAYS" =~ ^[0-9]+$ ]] || die "--keep-days 必须是非负整数"
[[ "$REMOTE_TYPE" == "rsync" || "$REMOTE_TYPE" == "rclone" ]] || die "--remote-type 只能是 rsync 或 rclone"

# ---------------------------------------------------------------- 环境检查
for bin in "$PGDUMP_BIN" "$TAR_BIN" "$SHA_BIN"; do
  command -v "$bin" >/dev/null 2>&1 || die "缺少命令: $bin（Debian: apt-get install -y postgresql-client tar coreutils）"
done

TS="$(date '+%Y%m%d-%H%M%S')"
STAMP_DIR="$DEST/$TS"
FILES=()                              # 本次产出的文件路径（用于校验与异地拷贝）

if (( DRY_RUN )); then
  log "== DRY-RUN 模式：不会实际写入任何文件 =="
  log "目标目录      : $DEST/$TS"
  log "数据库 URL    : ${DB_URL//:[^:@]*@/:***@}"
  log "附件目录      : $UPLOAD_DIR"
  log "模式          : $MODE"
  log "保留天数      : $KEEP_DAYS"
  log "异地目标      : ${REMOTE:-（未配置）}"
  exit 0
fi

mkdir -p "$STAMP_DIR"
# 备份目录里可能含資料庫口令的连接串痕迹，权限收紧
chmod 700 "$STAMP_DIR"
log "创建备份目录: $STAMP_DIR"

# ---------------------------------------------------------------- 1. 数据库
if [[ "$MODE" == "all" || "$MODE" == "db" ]]; then
  DUMP_FILE="$STAMP_DIR/meddesk-db-$TS.dump"
  log "开始 pg_dump（-Fc 自定义格式）…"
  # -Fc：可 pg_restore 选择性还原、支持并行、体积更小
  # --no-owner --no-acl：还原到不同账号名时不报错（异地灾备友好）
  # 口令通过 PGPASSWORD 传入而非命令行，避免被 ps 看到
  PGPASSWORD="$(printf '%s' "$DB_URL" | sed -n 's|^postgres://[^:/]*:\([^@]*\)@.*|\1|p')" \
    "$PGDUMP_BIN" --format=custom --compress=9 --no-owner --no-acl \
      --dbname="$DB_URL" --file="$DUMP_FILE"
  if [[ ! -s "$DUMP_FILE" ]]; then
    die "pg_dump 产物为空或不存在: $DUMP_FILE"
  fi
  log "数据库备份完成: $DUMP_FILE ($(du -h "$DUMP_FILE" | cut -f1))"
  FILES+=("$DUMP_FILE")

  # 额外导出一份「纯文本 schema」，便于人在没有 PG 的情况下查看表结构
  SCHEMA_FILE="$STAMP_DIR/meddesk-schema-$TS.sql"
  PGPASSWORD="$(printf '%s' "$DB_URL" | sed -n 's|^postgres://[^:/]*:\([^@]*\)@.*|\1|p')" \
    "$PGDUMP_BIN" --schema-only --no-owner --no-acl --dbname="$DB_URL" --file="$SCHEMA_FILE" || \
    warn "导出 schema 失败（非致命）"
  if [[ -s "$SCHEMA_FILE" ]]; then FILES+=("$SCHEMA_FILE"); fi
fi

# ---------------------------------------------------------------- 2. 附件目录
if [[ "$MODE" == "all" || "$MODE" == "files" ]]; then
  if [[ -d "$UPLOAD_DIR" ]]; then
    FILES_TAR="$STAMP_DIR/meddesk-uploads-$TS.tar.gz"
    log "开始打包附件目录（密文，不改变内容）…"
    # -C 切到父目录用相对路径打包，还原时可以解到任意位置
    "$TAR_BIN" -czf "$FILES_TAR" -C "$(dirname "$UPLOAD_DIR")" "$(basename "$UPLOAD_DIR")" \
      2>/dev/null || warn "打包过程中有文件正在被修改（非致命，建议停写或重试）"
    if [[ ! -s "$FILES_TAR" ]]; then
      warn "附件打包失败或目录为空: $UPLOAD_DIR"
    else
      log "附件备份完成: $FILES_TAR ($(du -h "$FILES_TAR" | cut -f1))"
      FILES+=("$FILES_TAR")
    fi
  else
    warn "附件目录不存在，跳过: $UPLOAD_DIR"
  fi
fi

# ---------------------------------------------------------------- 3. 元数据与校验和
META_FILE="$STAMP_DIR/MANIFEST.txt"
{
  echo "# MedDesk 备份清单"
  echo "generated_at : $(date -Is)"
  echo "hostname     : $(hostname)"
  echo "script       : backup.sh"
  echo "mode         : $MODE"
  echo "db_url_masked: ${DB_URL//:[^:@]*@/:***@}"
  echo "upload_dir   : $UPLOAD_DIR"
  echo "keep_days    : $KEEP_DAYS"
  echo
  echo "# 提醒：数据库内容均为密文信封；明文只存在于客户端浏览器。"
  echo "#       恢复业务数据还需用户自己的密钥/口令。"
} > "$META_FILE"

CHECKSUM_FILE="$STAMP_DIR/SHA256SUMS"
( cd "$STAMP_DIR" && "$SHA_BIN" ./*.dump ./*.sql ./*.tar.gz 2>/dev/null || true ) > "$CHECKSUM_FILE"
if [[ ! -s "$CHECKSUM_FILE" ]]; then
  warn "未生成任何校验和，请检查备份是否真的写入了文件"
else
  log "已生成校验和: $CHECKSUM_FILE"
fi

chmod -R go-rwx "$STAMP_DIR" || true
log "本次备份条目数: ${#FILES[@]}"

# ---------------------------------------------------------------- 4. 异地拷贝
if [[ -n "$REMOTE" ]]; then
  log "开始异地拷贝到: $REMOTE"
  case "$REMOTE_TYPE" in
    rsync)
      command -v "$RSYNC_BIN" >/dev/null 2>&1 || warn "未安装 rsync，跳过异地拷贝"
      # -a 保留属性；--delete **不加**：远端由远端自己的保留策略管理，防止本地误删牵连远端
      "$RSYNC_BIN" -a --partial --stats "$STAMP_DIR/" "$REMOTE/$TS/" || warn "rsync 失败（非致命）"
      ;;
    rclone)
      command -v "$RCLONE_BIN" >/dev/null 2>&1 || warn "未安装 rclone，跳过异地拷贝"
      "$RCLONE_BIN" copy --stats "$STAMP_DIR" "$REMOTE/$TS" || warn "rclone 失败（非致命）"
      ;;
  esac
  log "异地拷贝完成"
fi

# ---------------------------------------------------------------- 5. 保留策略
log "清理本地 $KEEP_DAYS 天前的旧备份…"
if (( KEEP_DAYS > 0 )); then
  # -maxdepth 1 只清理顶层时间戳目录，避免误删目录内文件
  find "$DEST" -mindepth 1 -maxdepth 1 -type d -mtime "+${KEEP_DAYS}" -print -exec rm -rf {} + || \
    warn "清理旧备份时出错（非致命）"
else
  log "KEEP_DAYS=0，跳过清理"
fi

# ---------------------------------------------------------------- 6. 完成
log "备份完成 ✅  $STAMP_DIR"
df -h "$DEST" | tail -1 || true
echo
echo "下一步建议："
echo "  1) 校验可用性：sha256sum -c $STAMP_DIR/SHA256SUMS"
echo "  2) 定期做恢复演练：deploy/scripts/restore.sh --list"
echo "  3) 提醒客户端用户在「设置 → 导出 JSON 备份」保存明文快照"
