# ==============================================================================
# MedDesk —— Makefile
#   面向开发、构建、测试、迁移、部署、备份的常用入口。
#   全部目标都在仓库根目录执行：make <目标>
#
#   查看所有目标：make help
#
# 设计约定：
#   - 版本号从 git describe 取，取不到用 dev
#   - 交叉编译目标为 Linux amd64/arm64（覆盖绝大多数 VPS 与 NAS）
#   - 危险目标（db-reset / db-drop / restore）一律要求显式传变量或二次确认
# ==============================================================================

SHELL := /usr/bin/env bash

APP_NAME   := meddesk
CMD_DIR    := ./cmd/meddesk
BUILD_DIR  := ./bin
BINARY     := $(BUILD_DIR)/$(APP_NAME)

VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS    := -s -w \
              -X main.Version=$(VERSION) \
              -X main.Commit=$(COMMIT) \
              -X main.BuildTime=$(BUILD_TIME)

# 交叉编译目标 matrix：<输出名> = <GOOS>/<GOARCH>
TARGETS := linux-amd64 linux-arm64 darwin-arm64 windows-amd64

GOOS_DEFAULT   := linux
GOARCH_DEFAULT := amd64

DOCKER_COMPOSE := docker compose -f deploy/docker-compose.yml
ENV_FILE       ?= .env

.PHONY: help
help: ## 显示帮助
	@echo "MedDesk Make 目标："
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "交叉编译产出位于 $(BUILD_DIR)/ ，命名 meddesk-<GOOS>-<GOARCH>"

# ------------------------------------------------------------------ 开发
.PHONY: deps-up deps-down dev
deps-up: ## 用 docker compose 起本地依赖（PG16 + Redis7，见 deploy/docker-compose.dev.yml）
	docker compose -f deploy/docker-compose.dev.yml up -d
	@echo "PG: 127.0.0.1:5432 (meddesk/meddesk)  Redis: 127.0.0.1:6379 (devpass)"

deps-down: ## 停止本地依赖（保留数据卷；连数据一起清加 DOWN_V=1）
	docker compose -f deploy/docker-compose.dev.yml down $(if $(DOWN_V),-v,)

dev: ## 本地开发启动（从磁盘读前端，改 JS 不用重编 Go）；先 deps-up 起数据库
	MEDDESK_WEB_DIR=./web go run $(CMD_DIR)

.PHONY: dev-init
dev-init: ## 准备本地 .env（从样例复制，需自行替换占位符）
	@if [ -f $(ENV_FILE) ]; then echo "$(ENV_FILE) 已存在，跳过"; else cp deploy/env.example $(ENV_FILE); echo "已生成 $(ENV_FILE)，请替换其中的 <...> 占位符"; fi

.PHONY: run
run: build ## 构建后直接运行（使用内嵌前端）
	./$(BINARY)

# ------------------------------------------------------------------ 构建
.PHONY: build
build: ## 构建 Linux amd64 二进制到 ./bin/meddesk
	@mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS_DEFAULT) GOARCH=$(GOARCH_DEFAULT) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD_DIR)
	@ls -lh $(BINARY)

.PHONY: build-all
build-all: ## 交叉编译全部目标：linux-amd64 / linux-arm64 / darwin-arm64 / windows-amd64
	@mkdir -p $(BUILD_DIR)
	@for t in $(TARGETS); do \
	  os=$${t%-*}; arch=$${t#*-}; \
	  out=$(BUILD_DIR)/$(APP_NAME)-$$os-$$arch; \
	  if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
	  echo "构建 $$os/$$arch -> $$out"; \
	  GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(CMD_DIR) || exit 1; \
	done
	@ls -lh $(BUILD_DIR)

.PHONY: build-linux
build-linux: ## 生产推荐：GOOS=linux GOARCH=amd64 go build -ldflags="-s -w"
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(APP_NAME) $(CMD_DIR)

# ------------------------------------------------------------------ 质量
.PHONY: test
test: ## 跑测试
	go test ./... -count=1

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: fmt
fmt: ## 格式化 Go 代码
	gofmt -l -w .

.PHONY: check
check: vet test ## vet + test

.PHONY: check-js
check-js: ## 用 node --check 逐个校验前端脚本（需 node）
	@for f in web/assets/js/*.js web/sw.js; do \
	  [ -f "$$f" ] || continue; \
	  node --check "$$f" && echo "OK   $$f" || { echo "FAIL $$f"; exit 1; }; \
	done

.PHONY: check-shell
check-shell: ## 校验所有 shell 脚本语法（bash -n）
	@for f in deploy/scripts/*.sh; do bash -n "$$f" && echo "OK   $$f" || exit 1; done

.PHONY: check-yaml
check-yaml: ## 校验两份 docker-compose（优先 docker compose config，退回 python yaml）
	@if command -v docker >/dev/null 2>&1; then \
	   $(DOCKER_COMPOSE) config >/dev/null && echo "OK   deploy/docker-compose.yml (docker compose config)"; \
	   docker compose -f deploy/docker-compose.dev.yml config >/dev/null && echo "OK   deploy/docker-compose.dev.yml (docker compose config)"; \
	   docker compose --env-file /dev/null -f deploy/docker-compose.middleware.yml config >/dev/null 2>&1 || echo "SKIP deploy/docker-compose.middleware.yml (缺必填口令，属预期)"; \
	 else \
	   python3 -c "import yaml,sys; yaml.safe_load(open('deploy/docker-compose.yml')); print('OK   deploy/docker-compose.yml (python yaml.safe_load)')"; \
	   python3 -c "import yaml,sys; yaml.safe_load(open('deploy/docker-compose.dev.yml')); print('OK   deploy/docker-compose.dev.yml (python yaml.safe_load)')"; \
	   python3 -c "import yaml,sys; yaml.safe_load(open('deploy/docker-compose.middleware.yml')); print('OK   deploy/docker-compose.middleware.yml (python yaml.safe_load)')"; \
	 fi

.PHONY: check-docs
check-docs: ## 校验 docs 与 deploy 下所有脚本/配置的存在性
	@for f in docs/API.md docs/DEPLOYMENT.md docs/DATA-MODEL.md docs/SECURITY.md docs/EXTENDING.md \
	          deploy/nginx/meddesk.conf deploy/systemd/meddesk.service deploy/Dockerfile \
	          deploy/postgres-tuning.conf deploy/env.example .env.example; do \
	   [ -f "$$f" ] && echo "OK   $$f" || { echo "MISS $$f"; exit 1; }; \
	 done

.PHONY: check-all
check-all: check check-shell check-yaml check-docs ## 全部校验

# ------------------------------------------------------------------ 前端
.PHONY: embed-check
embed-check: ## 确认 web/.gitkeep 存在（//go:embed all:web 需要）
	@test -f web/.gitkeep && echo "OK   web/.gitkeep 存在" || { echo "缺失 web/.gitkeep！不要删除"; exit 1; }

# ------------------------------------------------------------------ 数据库
.PHONY: migrate
migrate: ## 执行一次迁移（应用启动自动迁移；这里是手动触发）
	MEDDESK_DB_MIGRATE_ON_BOOT=true go run $(CMD_DIR)

PG_DSN ?= $(shell grep -E '^MEDDESK_DB_URL=' $(ENV_FILE) 2>/dev/null | cut -d= -f2-)

.PHONY: psql
psql: ## 用 psql 连库（PG_DSN 取自 .env）
	psql "$(PG_DSN)"

.PHONY: db-reset
db-reset: ## 【危险】清空 users/refresh_tokens/sync_records/sync_state/attachments，保留表结构
	@echo "⚠ 即将清空 $(PG_DSN) 中所有业务表数据，此操作不可撤销。"
	@read -p "请输入 RESET 确认：" c; [ "$$c" = "RESET" ] || { echo "已取消"; exit 1; }
	psql "$(PG_DSN)" -c "TRUNCATE TABLE attachments, refresh_tokens, sync_records, sync_state, users RESTART IDENTITY CASCADE;"

.PHONY: db-drop
db-drop: ## 【危险】DROP SCHEMA public 重建（等于删库），需 FORCE=1
ifndef FORCE
	$(error db-drop 需要显式 FORCE=1 才会执行，例：make db-drop FORCE=1)
endif
	psql "$(PG_DSN)" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
	psql "$(PG_DSN)" -c "GRANT ALL ON SCHEMA public TO $(shell psql -tAq "$(PG_DSN)" -c 'SELECT current_user');"
	psql "$(PG_DSN)" -c "CREATE EXTENSION IF NOT EXISTS citext;"

# ------------------------------------------------------------------ 部署
.PHONY: docker-build
docker-build: ## 构建容器镜像（可用 TAG=xxx 指定标签）
	docker build -f deploy/Dockerfile -t $(APP_NAME):$(or $(TAG),local) .

.PHONY: compose-up
compose-up: ## compose 一键起全套（先 rm -rf 「不干净」的旧容器请自行处理）
	$(DOCKER_COMPOSE) up -d --build

.PHONY: compose-down
compose-down: ## 停止 compose 服务（保留数据卷）
	$(DOCKER_COMPOSE) down

.PHONY: compose-logs
compose-logs: ## 跟踪 compose 日志
	$(DOCKER_COMPOSE) logs -f --tail=200

.PHONY: compose-config
compose-config: ## 校验并打印 compose 最终配置
	$(DOCKER_COMPOSE) config

.PHONY: deploy
deploy: ## 交叉编译后 rsync 到目标机并安装二进制（HOST/REMOTE_DIR 必填）
ifndef HOST
	$(error 用法：make deploy HOST=root@your.server REMOTE_DIR=/opt/meddesk/bin)
endif
	$(MAKE) build-linux
	rsync -avz --progress $(BUILD_DIR)/$(APP_NAME) $(HOST):$(or $(REMOTE_DIR),/opt/meddesk/bin)/$(APP_NAME).new
	ssh $(HOST) 'set -e; \
	  systemctl stop $(APP_NAME) || true; \
	  cp -a $(or $(REMOTE_DIR),/opt/meddesk/bin)/$(APP_NAME) $(or $(REMOTE_DIR),/opt/meddesk/bin)/$(APP_NAME).old.$$(date +%Y%m%d%H%M%S) || true; \
	  mv $(or $(REMOTE_DIR),/opt/meddesk/bin)/$(APP_NAME).new $(or $(REMOTE_DIR),/opt/meddesk/bin)/$(APP_NAME); \
	  chmod 755 $(or $(REMOTE_DIR),/opt/meddesk/bin)/$(APP_NAME); \
	  systemctl start $(APP_NAME); \
	  sleep 2; systemctl is-active --quiet $(APP_NAME) && echo 已启动 || journalctl -u $(APP_NAME) -n 50 --no-pager'

.PHONY: deploy-rollback
deploy-rollback: ## 回滚：把 .old.<时间戳> 换回来（FILE 必填，用 ssh 的 ls 查看）
ifndef FILE
	$(error 用法：make deploy-rollback HOST=root@your.server FILE=/opt/meddesk/bin/meddesk.old.20261026031500)
endif
	ssh $(HOST) 'set -e; \
	  systemctl stop $(APP_NAME) || true; \
	  cp -a $(FILE) /opt/meddesk/bin/$(APP_NAME); \
	  systemctl start $(APP_NAME); \
	  systemctl is-active --quiet $(APP_NAME) && echo 回滚完成'

.PHONY: install-service
install-service: ## 在目标机安装 systemd 单元（HOST 必填）
ifndef HOST
	$(error 用法：make install-service HOST=root@your.server)
endif
	scp deploy/systemd/meddesk.service $(HOST):/etc/systemd/system/meddesk.service
	ssh $(HOST) 'systemctl daemon-reload && systemctl enable meddesk && echo 已 enable'

.PHONY: install-scripts
install-scripts: ## 把 deploy/scripts 拷到目标机并 chmod +x（HOST 必填）
ifndef HOST
	$(error 用法：make install-scripts HOST=root@your.server)
endif
	ssh $(HOST) 'mkdir -p /opt/meddesk/scripts'
	scp deploy/scripts/*.sh $(HOST):/opt/meddesk/scripts/
	ssh $(HOST) 'chmod +x /opt/meddesk/scripts/*.sh && ls -l /opt/meddesk/scripts'

.PHONY: nginx-test
nginx-test: ## 目标机 nginx 配置自检（HOST 必填）
ifndef HOST
	$(error 用法：make nginx-test HOST=root@your.server)
endif
	scp deploy/nginx/meddesk.conf $(HOST):/etc/nginx/conf.d/meddesk.conf
	ssh $(HOST) 'nginx -t && systemctl reload nginx && echo nginx 已重载'

# ------------------------------------------------------------------ 备份恢复
BACKUP_DEST ?= /var/backups/meddesk

.PHONY: backup
backup: ## 本地跑一次备份（Deploy 目标机上执行）
	BACKUP_DEST=$(BACKUP_DEST) ./deploy/scripts/backup.sh

.PHONY: backup-dry
backup-dry: ## 备份演练：只打印计划
	BACKUP_DEST=$(BACKUP_DEST) ./deploy/scripts/backup.sh --dry-run

.PHONY: restore-list
restore-list: ## 列出可用备份
	BACKUP_DEST=$(BACKUP_DEST) ./deploy/scripts/restore.sh --list

.PHONY: restore
restore: ## 【危险】恢复，需 FROM=<时间戳|latest>
ifndef FROM
	$(error 用法：make restore FROM=latest  或  make restore FROM=20261026-031700)
endif
	BACKUP_DEST=$(BACKUP_DEST) ./deploy/scripts/restore.sh --from $(FROM)

.PHONY: font
font: ## 下载/安装中文字体（可能需要 sudo）
	sudo ./deploy/scripts/get-font.sh

# ------------------------------------------------------------------ 清理
.PHONY: clean
clean: ## 删除构建产物
	rm -rf $(BUILD_DIR) dist cover.out

.PHONY: dist-clean
dist-clean: clean ## 删除构建产物与本地 .env、日志
	rm -f .env meddesk *.log
