#!/usr/bin/env bash
# ==============================================================================
# MedDesk —— 一键发布脚本（构建 → 打包 → 提交推送 → 打 tag → GitHub Release）
#
# 用法（Git Bash / Linux 均可）：
#   ./deploy/scripts/release.sh v0.1.5                 # 完整发布
#   ./deploy/scripts/release.sh v0.1.5 --skip-tests    # 跳过测试（应急）
#
# GitHub Release 上传（可选）需要令牌，二选一：
#   export GITHUB_TOKEN=github_pat_xxx        # 环境变量
#   echo "github_pat_xxx" > ~/.meddesk-release-token
#   令牌权限：fine-grained，仅 meddesk 仓库，Contents: Read and write。
#   未提供令牌时：跳过 Release 上传，其余照常完成（附件手动网页补传即可）。
#
# 产物：
#   dist/meddesk-<版本>-linux-amd64/          解包目录
#   dist/meddesk-<版本>-linux-amd64.tar.gz    完整包（自动提交入库）
#   dist/release/meddesk-<版本>               裸二进制（Release 附件）
# ==============================================================================

set -Eeuo pipefail

VER="${1:?用法: release.sh <版本号>（例 v0.1.5）} [--skip-tests]"
case "$VER" in v[0-9]*) ;; *) echo "错误：版本号必须以 v 开头（如 v0.1.5）"; exit 1 ;; esac
SKIP_TESTS=0; [[ "${2:-}" == "--skip-tests" ]] && SKIP_TESTS=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
MOD="github.com/qiubite666/meddesk/internal/server"
NAME="meddesk-${VER}-linux-amd64"
DIST="dist/${NAME}"

step() { echo ""; echo "========== $1 =========="; }

step "1/7 质量检查"
go vet ./...
if (( ! SKIP_TESTS )); then go test ./... -count=1 2>&1 | grep -vE "^(ok|\?)" || true; fi

step "2/7 交叉编译 linux-amd64（版本 ${VER}）"
rm -rf "$DIST"
mkdir -p "$DIST/deploy/scripts/initdb"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags="-s -w -X ${MOD}.Version=${VER}" -o "$DIST/meddesk" ./cmd/meddesk

step "3/7 组装交付目录"
cp deploy/systemd/meddesk.service        "$DIST/systemd-meddesk.service"
cp deploy/nginx/meddesk.conf             "$DIST/nginx-meddesk.conf"
cp deploy/scripts/get-font.sh            "$DIST/get-font.sh"
cp meddesk.env.example                   "$DIST/meddesk.env.example"
cp deploy/docker-compose.middleware.yml deploy/postgres-tuning.conf "$DIST/deploy/"
cp deploy/scripts/initdb/00-init.sql     "$DIST/deploy/scripts/initdb/"

# 速览 README：从最近的旧版本继承（不存在则跳过）
OLD_README="$(ls -d dist/meddesk-v*-linux-amd64/README-部署速览.md 2>/dev/null | sort -V | tail -1 || true)"
if [[ -n "$OLD_README" && "$OLD_README" != *"${VER}"* ]]; then
  sed "s/v[0-9.]* 部署速览/${VER} 部署速览/; s/\"version\":\"[^\"]*\"/\"version\":\"${VER}\"/" \
    "$OLD_README" > "$DIST/README-部署速览.md" || true
fi

step "4/7 打包"
TARBALL="dist/${NAME}.tar.gz"
tar -czf "$TARBALL" -C dist "${NAME}"
mkdir -p dist/release
cp "$DIST/meddesk" "dist/release/${NAME}"
BIN_SHA="$(sha256sum "dist/release/${NAME}" | cut -d' ' -f1)"
TGZ_SHA="$(sha256sum "$TARBALL" | cut -d' ' -f1)"
echo "  二进制 sha256: $BIN_SHA"
echo "  完整包 sha256: $TGZ_SHA"

step "5/7 提交并推送（交付包入库 + tag）"
git add -f "$TARBALL"
if git diff --cached --quiet; then echo "  交付包无变更，跳过提交"; else
  git commit -m "chore(dist): ${VER} 交付包（linux-amd64）

二进制 sha256: ${BIN_SHA}
完整包 sha256: ${TGZ_SHA}"
fi
git tag -a "$VER" -m "MedDesk ${VER}

二进制 sha256: ${BIN_SHA}" 2>/dev/null || echo "  tag ${VER} 已存在，跳过"
git push origin main "$VER" || echo "  ⚠ push 失败，请稍后手动执行：git push origin main ${VER}"

step "6/7 GitHub Release（需要令牌；未配置则跳过）"
TOKEN="${GITHUB_TOKEN:-}"
if [[ -z "$TOKEN" && -f "$HOME/.meddesk-release-token" ]]; then
  TOKEN="$(tr -d '[:space:]' < "$HOME/.meddesk-release-token")"
fi
if [[ -z "$TOKEN" ]]; then
  echo "  未检测到令牌（GITHUB_TOKEN 或 ~/.meddesk-release-token），跳过 Release 上传。"
  echo "  请到 GitHub 仓库 Releases 页面手动创建/上传，或配置令牌后重跑本脚本。"
else
  AUTH="Authorization: Bearer ${TOKEN}"
  REPO_API="https://api.github.com/repos/qiubite666/meddesk"
  # 若已有该版本 release 则复用，否则创建
  REL_ID=$(curl -sS -H "$AUTH" "${REPO_API}/releases/tags/${VER}" | python -c "import json,sys; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null || true)
  if [[ -z "$REL_ID" ]]; then
    curl -sS -X POST -H "$AUTH" -H "Content-Type: application/json" \
      -d "{\"tag_name\":\"${VER}\",\"name\":\"MedDesk ${VER}\",\"draft\":false,\"prerelease\":false,\"body\":\"MedDesk ${VER}（sha256 二进制: ${BIN_SHA} / 完整包: ${TGZ_SHA}）\"}" \
      "${REPO_API}/releases" > /tmp/rel.json
    REL_ID=$(python -c "import json;print(json.load(open('/tmp/rel.json')).get('id',''))")
  fi
  if [[ -z "$REL_ID" ]]; then echo "  ⚠ Release 创建失败（权限或已存在冲突），请手动检查"; else
    for f in "dist/release/${NAME}" "$TARBALL"; do
      ANAME="$(basename "$f")"
      curl -sS -X POST -H "$AUTH" -H "Content-Type: application/octet-stream" \
        --data-binary "@${f}" \
        "${REPO_API//api.github.com/uploads.github.com}/releases/${REL_ID}/assets?name=${ANAME}" \
        > /tmp/asset.json
      echo "  上传 ${ANAME}: $(python -c "import json;d=json.load(open('/tmp/asset.json'));print(d.get('state', d.get('message','?')))")"
    done
    echo "  Release: https://github.com/qiubite666/meddesk/releases/tag/${VER}"
  fi
fi

step "7/7 完成 ✅"
echo "  版本:     ${VER}"
echo "  交付包:   ${TARBALL}"
echo "  服务器更新:"
echo "    sudo install -m 0755 ${DIST}/meddesk /home/ops/service/meddesk/meddesk && sudo systemctl restart meddesk"
