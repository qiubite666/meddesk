#!/usr/bin/env bash
# ==============================================================================
# MedDesk —— 中文字体获取脚本
#   目标：把一份可用于 PDF 中文渲染的字体放到 /opt/meddesk/fonts/NotoSansSC-Regular.ttf
#
# 为什么需要它：
#   /api/v1/export/pdf 使用 go-pdf/fpdf 渲染中文。fpdf 需要 **TrueType 轮廓** 的字体文件，
#   找不到可用字体时该接口返回 503（apierr.CodeUnavailable），错误写明
#   「请先安装中文字体或配置 MEDDESK_PDF_FONT_PATH」。
#
# 用法：
#   sudo ./deploy/scripts/get-font.sh                       # 自动按优先级尝试
#   sudo ./deploy/scripts/get-font.sh --method apt          # 只用 Debian/Ubuntu 包
#   sudo ./deploy/scripts/get-font.sh --method url --url https://<内网>/NotoSansSC-Regular.ttf
#   sudo ./deploy/scripts/get-font.sh --dest /opt/meddesk/fonts --target NotoSansSC-Regular.ttf
#   ./deploy/scripts/get-font.sh --check                    # 只校验当前字体是否可用
#   ./deploy/scripts/get-font.sh --dry-run
#
# 完成后请确认 MEDDESK_PDF_FONT_PATH 指向该文件（留空时应用会自动按序探测，
# /opt/meddesk/fonts/NotoSansSC-Regular.ttf 是探测序列的第一项，通常无需显式配置）。
#
# ⚠ 关于字形来源的现实约束（务必读完）：
#   Noto Sans SC 官方基本只发布 OTF/OTC/sub-font 形态，中文是多万分字形，
#   几乎不存在「官方单文件 TTF」。因此本脚本的兜底方案（Debian 的 fonts-noto-cjk）
#   拿到的是 .otf/.ttc。fpdf 对 CFF 轮廓的 .otf 支持不佳，可能出现：
#     - 中文全是方块/空白
#     - AddUTF8Font 直接失败导致 503
#   所以：**生产请以你自己准备的 .ttf 为准**（内网 mirror / 对象存储 / 已授权字体），
#   把它放进 deploy/fonts/NotoSansSC-Regular.ttf，Dockerfile 会自动 COPY 进镜像。
# ==============================================================================

set -Eeuo pipefail

LOG_TAG="[meddesk-font]"
FONT_DIR="${FONT_DIR:-/opt/meddesk/fonts}"
TARGET_NAME="${TARGET_NAME:-NotoSansSC-Regular.ttf}"
METHOD="auto"              # auto | apt | url
FONT_URL="${FONT_URL:-}"
APT_MIRROR="${APT_MIRROR:-https://mirrors.aliyun.com}"
DRY_RUN=0
CHECK_ONLY=0

log()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG $*"; }
warn() { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [警告] $*" >&2; }
die()  { echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG [错误] $*" >&2; exit 1; }
trap 'rc=$?; printf "%s [错误] 命令异常退出（退出码 %s）。\n" "$(date "+%Y-%m-%d %H:%M:%S") ${LOG_TAG}" "$rc" >&2; exit "$rc"' ERR

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dest|--dir)  FONT_DIR="$2"; shift 2 ;;
    --target)      TARGET_NAME="$2"; shift 2 ;;
    --method)      METHOD="$2"; shift 2 ;;
    --url)         FONT_URL="$2"; METHOD="url"; shift 2 ;;
    --apt-mirror)  APT_MIRROR="$2"; shift 2 ;;
    --check)       CHECK_ONLY=1; shift ;;
    --dry-run)     DRY_RUN=1; shift ;;
    -h|--help)     sed -n '3,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             die "未知参数: $1（--help 查看用法）" ;;
  esac
done

TARGET="$FONT_DIR/$TARGET_NAME"

# ---------------------------------------------------------------- 工具：按 magic bytes 判断字体类型
font_type() {
  # 输出：ttf | otf | ttc | unknown
  local f="$1"
  [[ -s "$f" ]] || { echo unknown; return 0; }
  local magic
  magic="$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n' 2>/dev/null || echo '')"
  case "$magic" in
    00010000|74727565) echo ttf ;;
    4f54544f)          echo otf ;;
    74746366)          echo ttc ;;
    *)                 echo unknown ;;
  esac
}

# ---------------------------------------------------------------- --check 模式
if (( CHECK_ONLY )); then
  log "检查字体: $TARGET"
  if [[ ! -s "$TARGET" ]]; then
    die "文件不存在或为空: $TARGET"
  fi
  t="$(font_type "$TARGET")"
  size="$(wc -c < "$TARGET" | tr -d ' ')"
  log " 大小    : $size 字节"
  log " 格式    : $t"
  case "$t" in
    ttf) log " 结论    : ✅ 标准 TrueType，fpdf 可直接用" ;;
    ttc) warn " 结论    : ⚠ TrueType Collection，fpdf 支持有限，务必实测 PDF 导出" ;;
    otf) warn " 结论    : ⚠ OpenType/CFF 轮廓，fpdf 大概率不支持，建议换 .ttf" ;;
    *)   die  " 结论    : ❌ 无法识别的字体格式" ;;
  esac
  log " 验证方式：界面导出一次 PDF，或："
  log "   curl -X POST https://med.example.com/api/v1/export/pdf -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' -d '{\"format\":\"pdf\",\"title\":\"字体自检\",\"rows\":[{\"date\":\"2026-10-26\",\"time\":\"08:00\",\"profile\":\"本人\",\"medication\":\"维生素 D\",\"dosage\":\"1 粒\",\"status\":\"taken\",\"note\":\"中文测试\"}],\"stats\":{\"total\":1,\"taken\":1,\"skipped\":0,\"missed\":0,\"adherence\":1}}' -o /tmp/t.pdf"
  exit 0
fi

log "目标文件: $TARGET"
log "获取方式: $METHOD"

# ---------------------------------------------------------------- 需求命令检查
if [[ "$METHOD" == "url" || -n "$FONT_URL" ]]; then
  command -v curl >/dev/null 2>&1 || die "缺少 curl（apt-get install -y curl）"
fi
if [[ "$METHOD" == "apt" || "$METHOD" == "auto" ]]; then
  command -v apt-get >/dev/null 2>&1 || warn "非 Debian/Ubuntu 系统，apt 路线不可用"
fi
command -v od >/dev/null 2>&1 || die "缺少 od（apt-get install -y coreutils）"

# ---------------------------------------------------------------- 已经存在且有效？
if [[ -s "$TARGET" ]]; then
  t="$(font_type "$TARGET")"
  if [[ "$t" == "ttf" ]]; then
    log "已存在有效的 TrueType 字体，无需重复下载：$TARGET"
    exit 0
  fi
  warn "已存在文件但格式是 $t，将尝试替换为更好的字体"
fi

if (( DRY_RUN )); then
  log "DRY-RUN：将创建 $FONT_DIR，尝试 url/apt 取得字体并写入 $TARGET"
  log "  FONT_URL   : ${FONT_URL:-（未指定）}"
  log "  APT_MIRROR : $APT_MIRROR"
  exit 0
fi

mkdir -p "$FONT_DIR"
TMP="$(mktemp "${TARGET}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

got=0

# ---------------------------------------------------------------- 方式 1：直链 URL
if [[ -n "$FONT_URL" ]]; then
  log "尝试从 URL 下载: $FONT_URL"
  if curl -fsSL --connect-timeout 10 --max-time 300 "$FONT_URL" -o "$TMP" && [[ -s "$TMP" ]]; then
    got=1; log "下载成功 ✅"
  else
    warn "下载失败，继续尝试其他方式"
  fi
fi

# ---------------------------------------------------------------- 方式 2：本地已放置的文件
if (( ! got )); then
  LOCAL_CANDIDATES=(
    "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/fonts/NotoSansSC-Regular.ttf"
    "./deploy/fonts/NotoSansSC-Regular.ttf"
    "$HOME/NotoSansSC-Regular.ttf"
  )
  for cand in "${LOCAL_CANDIDATES[@]}"; do
    if [[ -s "$cand" ]]; then
      log "发现本地字体文件: $cand"
      cp -f "$cand" "$TMP"; got=1; break
    fi
  done
fi

# ---------------------------------------------------------------- 方式 3：APT 包（国内镜像）
if (( ! got )) && command -v apt-get >/dev/null 2>&1; then
  log "尝试通过 APT 安装 CJK 字体（镜像: $APT_MIRROR）"
  if [[ "$(id -u)" -ne 0 ]]; then
    warn "APT 安装需要 root，请用 sudo 重跑或手工执行："
    warn "  sudo apt-get update && sudo apt-get install -y fonts-noto-cjk fonts-wqy-zenhei"
  else
    # 临时把官方源换成国内镜像，避免 deb.debian.org 在国内超时
    updated=0
    for sf in /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list; do
      if [[ -f "$sf" ]]; then
        cp -a "$sf" "$sf.bak.$(date +%Y%m%d%H%M%S)"
        sed -i "s|https\\?://deb.debian.org|${APT_MIRROR}|g; s|https\\?://security.debian.org|${APT_MIRROR}|g" "$sf"
        updated=1
      fi
    done
    apt-get update -o Acquire::Retries=3 || warn "apt update 失败，请检查网络或镜像地址"
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends fonts-noto-cjk fonts-wqy-zenhei \
      || warn "APT 安装字体失败"
    for cand in \
      /usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf \
      /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc \
      /usr/share/fonts/truetype/noto/NotoSansCJKsc-Regular.otf \
      /usr/share/fonts/truetype/wqy/wqy-zenhei.ttc \
      /usr/share/fonts/truetype/wqy/wqy-microhei.ttc ; do
      if [[ -s "$cand" ]]; then
        log "使用系统字体: $cand"
        cp -f "$cand" "$TMP"; got=1; break
      fi
    done
    if (( ! got )); then
      warn "APT 路线未找到可用字体文件"
    fi
    echo "提示：APT 源已被改为 ${APT_MIRROR}，原文件备份为 *.bak.<时间戳>，不需要可自行还原。"
  fi
fi

# ---------------------------------------------------------------- 落位与校验
if (( ! got )); then
  cat >&2 <<'MSG'

┌───────────────────────────────────────────────────────────────────────┐
│ ❌ 未能自动获取中文字体。PDF 导出功能将不可用（接口返回 503）。          │
│                                                                        │
│ 离线备选方案（推荐，最可控）：                                          │
│   1) 在能联网的机器上取得一份 NotoSansSC-Regular.ttf（内网 mirror /     │
│      对象存储 / 已购买的商业字体授权版本），用 scp 传到本机：            │
│         scp NotoSansSC-Regular.ttf root@<host>:/opt/meddesk/fonts/      │
│   2) 或放到仓库的 deploy/fonts/ 后重新 docker build（Dockerfile 会 COPY）│
│   3) 或配置 MEDDESK_PDF_FONT_PATH 指向任意一个已存在的 .ttf              │
│                                                                        │
│ 临时替代：先导出 CSV（UTF-8 BOM，Excel 可直接打开），先行可用。           │
└───────────────────────────────────────────────────────────────────────┘
MSG
  exit 1
fi

install -m 0644 "$TMP" "$TARGET"
rm -f "$TMP"

t="$(font_type "$TARGET")"
log "字体就位: $TARGET ($(wc -c < "$TARGET" | tr -d ' ') 字节, 格式=$t)"
case "$t" in
  ttf) log "✅ TrueType 字体，可直接用于 PDF 渲染" ;;
  ttc|otf) warn "⚠ $t 格式：fpdf 支持有限，请务必导出一次 PDF 实测，中文异常请改用 .ttf" ;;
  *) warn "⚠ 无法识别的格式，请检查文件是否完整" ;;
esac

cat <<'MSG'

接下来：
  1) 若服务以 systemd 运行，确认 meddesk 用户对该文件有读权限：
       sudo chmod 644 /opt/meddesk/fonts/NotoSansSC-Regular.ttf
     systemd 的 ProtectSystem=strict 下 /opt 是只读挂载，安装后无需额外处理。
  2) 显式指定（可选，能省掉启动时的探测）：
       echo 'MEDDESK_PDF_FONT_PATH=/opt/meddesk/fonts/NotoSansSC-Regular.ttf' >> /etc/meddesk/meddesk.env
       sudo systemctl restart meddesk
  3) 重启后导出一次 PDF 验证中文是否正常显示。
MSG
