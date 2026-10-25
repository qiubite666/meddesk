# syntax=docker/dockerfile:1
# ==============================================================================
# MedDesk —— 多阶段构建 Dockerfile
# 构建（在仓库根目录执行，注意 deploy/docker-compose.yml 已把 context 设为 `..`）：
#   docker build -f deploy/Dockerfile -t meddesk:local .
# 若要指定中文字体来源：
#   docker build -f deploy/Dockerfile -t meddesk:local \
#     --build-arg FONT_URL="https://<你的内网或对象存储>/NotoSansSC-Regular.ttf" .
#
# 【阶段说明】
#   builder  : golang:1.26-alpine，CGO_ENABLED=0 产出静态二进制
#   fonts    : debian:bookworm-slim，负责取到可用的 CJK 字体
#   runtime  : debian:bookworm-slim，最小可用运行时（含 curl 供健康检查）
#
# 【为什么不用 distroless】
#   distroless 无 shell、无包管理，容器健康检查（compose 里用 CMD-SHELL + curl）
#   与在线排障都会受限；本项目体量不大，bookworm-slim + no-new-privileges + cap_drop=ALL
#   已足够。若你确实想要 distroless：
#     FROM gcr.io/distroless/static-debian12:nonroot AS runtime
#   此时须①去掉 apt/curl 与健康检查里的 shell 命令（改用 HTTP 探针或直接依赖 compose 的
#   depends_on），②tzdata 需自行 COPY /usr/share/zoneinfo。本文件保留了一整段注释备用。
# ==============================================================================

# ------------------------------------------------------------------------------
# 阶段 1：编译
# ------------------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

# GOTOOLCHAIN=auto：当 go.mod 声明的 go 版本高于镜像内 go 版本时，自动下载匹配工具链，
# 避免出现 "go.mod requires go >= 1.26.4" 直接失败。
ENV GOTOOLCHAIN=auto \
    CGO_ENABLED=0 \
    GOOS=linux \
    # 国内可达的模块代理；若在纯内网，改为自己的 Athens/nexus 地址或留请联系运维
    GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org

WORKDIR /src

# 先只拷依赖清单，最大化 Docker 层缓存（源码变动时不重跑 go mod download）
COPY go.mod go.sum ./
RUN go mod download

# 再拷源码。注意：必须包含 web/ 目录，因为仓库根目录的 assets.go 使用
# //go:embed all:web 把前端静态资源编译进二进制。
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build \
      -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
      -o /out/meddesk ./cmd/meddesk

# ------------------------------------------------------------------------------
# 阶段 2：字体（PDF 中文渲染必需）
#
# 背景：/api/v1/export/pdf 用 go-pdf/fpdf 渲染中文。fpdf 需要 **TrueType 轮廓** 的字体，
#       缺字体时该接口会返回 503（apierr.CodeUnavailable）。
#       应用启动时按以下顺序探测（未显式配置 MEDDESK_PDF_FONT_PATH 时）：
#         1 /opt/meddesk/fonts/NotoSansSC-Regular.ttf
#         2 /usr/share/fonts/truetype/noto/NotoSansSC-Regular.ttf
#         3 /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc
#         4 /usr/share/fonts/truetype/wqy/wqy-zenhei.ttc
#       所以本阶段会把取到的字体**统一落到路径 1**，让优先级最高的一项必命中。
#
# 三种来源，按优先级：
#   A 本地离线文件：把 NotoSansSC-Regular.ttf 放进仓库的 deploy/fonts/（目录已内置占位），
#     本阶段直接 COPY —— **推荐的生产做法**：镜像构建不依赖外网，可复现。
#   B 单个直链 URL：通过 --build-arg FONT_URL=... 传入（对象存储 / 内网 mirror / OSS）。
#   C APT 包：从 Debian 源装 fonts-noto-cjk，取其中的 CJK 字体文件改名放到目标路径。
#     国内把 APT 源换成阿里云镜像（APT_MIRROR 可改），保证可达。
#
# ⚠ 说明：Debian 的 fonts-noto-cjk 提供的是 **OTF/ttcf 容器**（CJK 字形体积巨大，
#   Noto 官方基本不再发布单文件 TTF）。fpdf 对 .otf(CFF 轮廓) 支持不稳，
#   因此路线 C 是**兜底**：能出中文则已，若 PDF 导出仍报 503 / 中文变方块，请改用 A 或 B，
#   提供一份真正的 .ttf（例如你自己的字体镜像、或把 Noto Sans SC 转换成 TTF 后的产物）。
# ------------------------------------------------------------------------------
FROM debian:bookworm-slim AS fonts

ARG APT_MIRROR=https://mirrors.aliyun.com
ARG FONT_URL=""

# A：离线字体优先（deploy/fonts/ 目录始终存在，故 COPY 不会失败）
COPY deploy/fonts/ /opt/meddesk/fonts/

RUN set -eux; \
    # 换国内 APT 源（bookworm 用 DEB822 格式，老版本用 sources.list，两种都兼容处理）
    if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i "s|https\\?://deb.debian.org|${APT_MIRROR}|g; s|https\\?://security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    elif [ -f /etc/apt/sources.list ]; then \
      sed -i "s|https\\?://deb.debian.org|${APT_MIRROR}|g; s|https\\?://security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list; \
    fi; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl file fonts-noto-cjk; \
    rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    FONTDIR=/opt/meddesk/fonts; \
    TARGET="${FONTDIR}/NotoSansSC-Regular.ttf"; \
    mkdir -p "$FONTDIR"; \
    # 1) 若 A/B 已经放好文件，校验它是不是 TrueType/OpenType（按 magic bytes 判断）
    valid=0; \
    if [ -s "$TARGET" ]; then \
      magic=$(head -c 4 "$TARGET" | od -An -tx1 | tr -d ' \n'); \
      case "$magic" in \
        00010000|4f54544f|74746366|74727565) valid=1 ;; \
      esac; \
    fi; \
    # 2) B：直链下载
    if [ "$valid" != "1" ] && [ -n "$FONT_URL" ]; then \
      echo "[fonts] 尝试从 FONT_URL 下载: $FONT_URL"; \
      if curl -fsSL --connect-timeout 10 --max-time 120 "$FONT_URL" -o "$TARGET" && [ -s "$TARGET" ]; then \
        valid=1; \
      fi; \
    fi; \
    # 3) C：APT 兜底
    if [ "$valid" != "1" ]; then \
      for cand in \
        /usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf \
        /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc \
        /usr/share/fonts/truetype/noto/NotoSansCJKsc-Regular.otf \
        /usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc \
        /usr/share/fonts/truetype/wqy/wqy-zenhei.ttc \
        /usr/share/fonts/truetype/wqy/wqy-microhei.ttc ; do \
        if [ -s "$cand" ]; then cp -f "$cand" "$TARGET"; valid=1; echo "[fonts] 使用兜底字体: $cand"; break; fi; \
      done; \
    fi; \
    # 4) 最终确认；失败不要在镜像层里静默通过，显式警告让构建者看见
    if [ "$valid" != "1" ]; then \
      echo "[fonts][警告] 未取到中文字体，PDF 导出将返回 503。" >&2; \
      echo "[fonts][提示] 请把 NotoSansSC-Regular.ttf 放到 deploy/fonts/ 或传入 --build-arg FONT_URL=..." >&2; \
      rm -f "$TARGET"; \
    else \
      file "$TARGET" || true; \
      ls -l "$TARGET"; \
    fi

# ------------------------------------------------------------------------------
# 阶段 3：运行时
# ------------------------------------------------------------------------------
FROM debian:bookworm-slim AS runtime

ARG APT_MIRROR=https://mirrors.aliyun.com

RUN set -eux; \
    if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i "s|https\\?://deb.debian.org|${APT_MIRROR}|g; s|https\\?://security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    elif [ -f /etc/apt/sources.list ]; then \
      sed -i "s|https\\?://deb.debian.org|${APT_MIRROR}|g; s|https\\?://security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list; \
    fi; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl tzdata; \
    rm -rf /var/lib/apt/lists/*; \
    # 非 root 运行（uid/gid 固定，便于宿主机给挂载卷 chown）
    groupadd -g 10001 meddesk; \
    useradd -u 10001 -g meddesk -M -s /usr/sbin/nologin meddesk; \
    mkdir -p /var/lib/meddesk/uploads /var/log/meddesk; \
    chown -R meddesk:meddesk /var/lib/meddesk /var/log/meddesk

WORKDIR /var/lib/meddesk

# 二进制
COPY --from=builder /out/meddesk /usr/local/bin/meddesk
# 字体
COPY --from=fonts /opt/meddesk/fonts /opt/meddesk/fonts

ENV MEDDESK_ENV=prod \
    MEDDESK_HTTP_ADDR=:8080 \
    MEDDESK_UPLOAD_DIR=/var/lib/meddesk/uploads \
    MEDDESK_PDF_FONT_PATH=/opt/meddesk/fonts/NotoSansSC-Regular.ttf \
    MEDDESK_LOG_FORMAT=json \
    TZ=Asia/Shanghai

USER meddesk:meddesk
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=5 \
    CMD curl -fsS http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["/usr/local/bin/meddesk"]

# ------------------------------------------------------------------------------
# 【可选】distroless 变体：把上面 FROM debian:bookworm-slim AS runtime 整段替换为：
#
#   FROM gcr.io/distroless/static-debian12:nonroot AS runtime
#   COPY --from=builder /out/meddesk /usr/local/bin/meddesk
#   COPY --from=fonts /opt/meddesk/fonts /opt/meddesk/fonts
#   COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
#   ENV MEDDESK_ENV=prod MEDDESK_HTTP_ADDR=:8080 \
#       MEDDESK_UPLOAD_DIR=/var/lib/meddesk/uploads \
#       MEDDESK_PDF_FONT_PATH=/opt/meddesk/fonts/NotoSansSC-Regular.ttf \
#       MEDDESK_LOG_FORMAT=json TZ=Asia/Shanghai
#   USER 65532:65532
#   EXPOSE 8080
#   ENTRYPOINT ["/usr/local/bin/meddesk"]
#
# 使用 distroless 时必须同步处理三件事：
#   1) 去掉 deploy/docker-compose.yml 里 app 的 healthcheck（无 shell），
#      改用 depends_on + 宿主机侧 curl 探测；
#   2) /var/lib/meddesk/uploads 需宿主机预先 chown 65532:65532（容器内无法 useradd/chown）；
#   3) 排障时无法 docker exec -it sh，只能靠日志与应用自身的 /api/v1/ready。
# ------------------------------------------------------------------------------
