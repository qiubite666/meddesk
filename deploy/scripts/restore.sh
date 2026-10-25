#!/usr/bin/env bash
# ==============================================================================
# MedDesk —— 恢复脚本（与 backup.sh 配套）
#
# ⚠⚠⚠ 高危操作警告 ⚠⚠⚠
#   恢复数据库会 **覆盖当前库中的同名对象**（pg_restore --clean --if-exists 会先 DROP），
#   恢复附件会把 tar 包内容 **解回 MEDDESK_UPLOAD_DIR**（同名文件被覆盖）。
#   执行前请务必：
#     1) 先做一次当前状态的快照备份：deploy/scripts/backup.sh
#     2) 停掉应用：sudo systemctl stop meddesk
#     3) 先 --dry-run 看清楚脚本打算干什么，再真跑
#     4) 非交互/自动化场景才用 --force（跳过二次确认），强烈不建议手写 before 第一个参数
#
# 用法：
#   ./deploy/scripts/restore.sh --list                       # 列出可用备份
#   ./deploy/scripts/restore.sh --from 20261026-031700 --dry-run
#   ./deploy/scripts/restore.sh --from 20261026-031700       # 交互式二次确认后恢复（DB+附件）
#   ./deploy/scripts/restore.sh --from latest --db-only
#   ./deploy/scripts/restore.sh --from latest --files-only --upload-dir /home/ops/service/meddesk/data
#   ./deploy/scripts/restore.sh --from latest --force         # 跳过确认（危险）
#
# 恢复目标 PostgreSQL：可用 --db-url 覆盖，或读环境变量 MEDDESK_DB_URL。
# ==============================================================================

set -Eeuo pipefail

DEST="${BACKUP_DEST:-/var/backups/meddesk}"
UPLOAD_DIR="${MEDDESK_UPLOAD_DIR:-/home/ops/service/meddesk/data}"
DB_URL="${MEDDESK_DB_URL:-postgres://meddesk:meddesk@127.0.0.1:5432/meddesk?sslmode=disable}"
LOG_TAG="[meddesk-restore]"

FROM=""
MODE="all"          # all | db | files
DRY_RUN=0
FORCE=0
SKIP_CHECKSUM=0

PGRESTORE_BIN="${PGRESTORE_BIN:-pg_restore}"
PSQL_BIN="${PSQL_BIN:-psql}"
TAR_BIN="${TAR_BIN:-tar}"
SHA_BIN="${SHA_BIN:-sha256sum}"

log()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG $*"; }
warn() { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [警告] $*" >&2; }
die()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [错误] $*" >&2; exit 1; }
trap 'rc=$?; printf "%s [错误] 命令异常退出（退出码 %s），恢复中断。库可能处于半恢复状态，请检查后重跑。\n" "$(date "+%Y-%m-%d %H:%M:%S") ${LOG_TAG}" "$rc" >&2; exit "$rc"' ERR

while [[ $# -gt 0 ]]; do
  case "$1" in
    --from)        FROM="${2:?--from 需要参数（时间戳目录名或 latest）}"; shift 2 ;;
    --dest)        DEST="${2:?--dest 需要参数}"; shift 2 ;;
    --upload-dir)  UPLOAD_DIR="${2:?--upload-dir 需要参数}"; shift 2 ;;
    --db-url)      DB_URL="${2:?--db-url 需要参数}"; shift 2 ;;
    --db-only)     MODE="db"; shift ;;
    --files-only)  MODE="files"; shift ;;
    --skip-checksum) SKIP_CHECKSUM=1; shift ;;
    --dry-run)     DRY_RUN=1; shift ;;
    --force)       FORCE=1; shift ;;
    --list)        list_only=1; shift ;;
    -h|--help)     sed -n '3,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done

[[ -d "$DEST" ]] || die "备份根目录不存在: $DEST（可用 --dest 指定）"

# ---------------------------------------------------------------- --list
if [[ "${list_only:-0}" == "1" ]]; then
  echo "可用备份（$DEST）："
  echo
  printf "%-22s %-10s %s\n" "时间戳" "大小" "包含内容"
  find "$DEST" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort -r | while read -r ts; do
    size="$(du -sh "$DEST/$ts" 2>/dev/null | cut -f1)"
    kinds=""
    if compgen -G "$DEST/$ts/*.dump" >/dev/null; then kinds="${kinds}DB "; fi
    if compgen -G "$DEST/$ts/*.tar.gz" >/dev/null; then kinds="${kinds}FILES "; fi
    printf "%-22s %-10s %s\n" "$ts" "${size:-?}" "$kinds"
  done
  echo
  echo "用法：$(basename "$0") --from <上表的时间戳> [--db-only|--files-only] [--dry-run]"
  exit 0
fi

[[ -n "$FROM" ]] || die "必须指定 --from <时间戳|latest>（用 --list 查看）"

if [[ "$FROM" == "latest" ]]; then
  FROM="$(find "$DEST" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort -r | head -n1)"
  [[ -n "$FROM" ]] || die "备份目录为空，无可用备份"
  log "--from latest 解析为: $FROM"
fi

STAMP_DIR="$DEST/$FROM"
[[ -d "$STAMP_DIR" ]] || die "备份不存在: $STAMP_DIR"

# ---------------------------------------------------------------- 定位备份文件
DUMP_FILE=""
FILES_TAR=""
shopt -s nullglob
for f in "$STAMP_DIR"/*.dump;   do DUMP_FILE="$f"; done
for f in "$STAMP_DIR"/*.tar.gz; do FILES_TAR="$f"; done
shopt -u nullglob

if [[ "$MODE" == "all" || "$MODE" == "db" ]]; then
  [[ -n "$DUMP_FILE" ]] || die "备份中没有找到 *.dump：$STAMP_DIR"
fi
if [[ "$MODE" == "all" || "$MODE" == "files" ]]; then
  [[ -n "$FILES_TAR" ]] || warn "备份中没有找到 *.tar.gz，将跳过附件恢复"
fi

# ---------------------------------------------------------------- 校验和校验
if (( ! SKIP_CHECKSUM )); then
  if [[ -f "$STAMP_DIR/SHA256SUMS" ]]; then
    log "校验 SHA-256…"
    if ( cd "$STAMP_DIR" && "$SHA_BIN" -c SHA256SUMS >/dev/null 2>&1 ); then
      log "校验通过 ✅"
    else
      die "校验失败 ❌ 备份可能已损坏或被篡改，拒绝恢复。若确认要强制继续，加 --skip-checksum"
    fi
  else
    warn "备份中没有 SHA256SUMS，跳过校验"
  fi
fi

# ---------------------------------------------------------------- 计划预览
echo
echo "════════════════════ 恢复计划 ════════════════════"
echo " 备份时间戳   : $FROM"
echo " 备份目录     : $STAMP_DIR"
echo " 恢复模式     : $MODE"
echo " 目标数据库   : ${DB_URL//:[^:@]*@/:***@}"
if [[ -n "$DUMP_FILE" ]]; then
  echo " 数据库备份   : $(basename "$DUMP_FILE")  ($(du -h "$DUMP_FILE" | cut -f1))"
fi
if [[ -n "$FILES_TAR" ]]; then
  echo " 附件备份     : $(basename "$FILES_TAR")  ($(du -h "$FILES_TAR" | cut -f1))"
  echo " 附件解到     : $UPLOAD_DIR"
fi
echo " 是否 Dry-Run : $([[ $DRY_RUN == 1 ]] && echo 是 || echo 否)"
echo "═══════════════════════════════════════════════════"

if (( DRY_RUN )); then
  log "DRY-RUN：以上仅为将要执行的动作预览，未做任何修改。"
  log "确认无误后去掉 --dry-run 重新执行；脚本会再次要求二次确认。"
  exit 0
fi

# ---------------------------------------------------------------- 运行状态检查
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet meddesk 2>/dev/null; then
  warn "检测到 meddesk 服务正在运行。恢复期间写入会造成数据不一致。"
  if (( FORCE )); then
    log "--force 生效，尝试停止服务…"
    systemctl stop meddesk || warn "停止失败，继续恢复（请自行确认没有写入）"
  else
    echo -n "是否现在停止 meddesk 服务？(y/N) "
    read -r ans
    if [[ "$ans" =~ ^[Yy]$ ]]; then
      systemctl stop meddesk || true
      log "已停止 meddesk"
    else
      die "已取消。请手动执行 sudo systemctl stop meddesk 后再重试。"
    fi
  fi
fi

# ---------------------------------------------------------------- 二次确认
if (( ! FORCE )); then
  cat >&2 <<'MSG'

┌───────────────────────────────────────────────────────────┐
│  ⚠ 危险：此操作会用备份数据 **覆盖** 当前数据库/附件。      │
│     被覆盖的数据无法找回（尤其 -Fc 恢复带 --clean 会 DROP） │
└───────────────────────────────────────────────────────────┘
MSG
  echo "请输入备份时间戳以确认：$FROM"
  echo -n "> "
  read -r confirm
  if [[ "$confirm" != "$FROM" ]]; then
    die "输入的确认串不匹配，已取消恢复。"
  fi
  echo -n "最后确认，真的要恢复吗？(yes/N) "
  read -r confirm2
  [[ "$confirm2" == "yes" ]] || die "已取消恢复。"
else
  warn "--force 已跳过全部二次确认，后果自负。"
fi

# ---------------------------------------------------------------- 1. 数据库恢复
if [[ "$MODE" == "all" || "$MODE" == "db" ]]; then
  log "开始恢复数据库…"
  # --clean --if-exists：先 DROP 已存在对象再重建，保证恢复后与目标备份完全一致
  # --no-owner --no-acl ：与 backup.sh 的 --no-owner --no-acl 对应
  # --single-transaction：整体作为一个事务，失败则完全回滚（推荐！避免半恢复状态）
  # ⚠ 若 dump 中包含 CREATE INDEX CONCURRENTLY 等不支持事务内执行的语句，需去掉该参数
  PGPASSWORD="$(printf '%s' "$DB_URL" | sed -n 's|^postgres://[^:/]*:\([^@]*\)@.*|\1|p')" \
    "$PGRESTORE_BIN" --dbname="$DB_URL" --clean --if-exists --no-owner --no-acl \
      --single-transaction --exit-on-error "$DUMP_FILE"
  log "数据库恢复完成 ✅"

  log "刷新统计信息（ANALYZE）…"
  PGPASSWORD="$(printf '%s' "$DB_URL" | sed -n 's|^postgres://[^:/]*:\([^@]*\)@.*|\1|p')" \
    "$PSQL_BIN" "$DB_URL" -c "ANALYZE;" >/dev/null || warn "ANALYZE 失败（非致命，建议手动执行）"
fi

# ---------------------------------------------------------------- 2. 附件恢复
if [[ "$MODE" == "all" || "$MODE" == "files" ]]; then
  if [[ -z "$FILES_TAR" ]]; then
    warn "跳过附件恢复（备份中无 tar.gz）"
  else
    log "恢复附件到 $UPLOAD_DIR …"
    mkdir -p "$UPLOAD_DIR"
    # 先看看包里顶层是什么，避免解错层级
    TOP="$("$TAR_BIN" -tzf "$FILES_TAR" | head -n1 | cut -d/ -f1)"
    log "包内顶层目录: $TOP"
    "$TAR_BIN" -xzf "$FILES_TAR" -C "$(dirname "$UPLOAD_DIR")"
    log "附件恢复完成 ✅"
    # 权限对齐 systemd 的运行用户（不对的话应用会 500）
    APP_USER="$(stat -c '%U' "$(dirname "$UPLOAD_DIR")" 2>/dev/null || echo ops)"
    if id "$APP_USER" >/dev/null 2>&1; then
      chown -R "$APP_USER":"$APP_USER" "$UPLOAD_DIR" || warn "chown 失败，请手动执行"
      log "已修正属主为: $APP_USER"
    fi
  fi
fi

# ---------------------------------------------------------------- 3. 收尾
if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files meddesk.service >/dev/null 2>&1; then
  echo -n "是否启动 meddesk 服务？(Y/n) "
  read -r ans
  if [[ "${ans:-Y}" =~ ^[Yy]$ ]]; then
    systemctl start meddesk || warn "启动失败，请检查：journalctl -u meddesk -n 100"
    sleep 2
    systemctl is-active --quiet meddesk && log "服务已启动 ✅" || warn "服务未处于 active，请排查"
  else
    log "未自动启动，请手动：sudo systemctl start meddesk"
  fi
fi

log "恢复流程结束。请执行冒烟验证："
echo "  curl -fsS https://med.example.com/api/v1/health"
echo "  curl -fsS https://med.example.com/api/v1/ready"
