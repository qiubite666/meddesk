-- -----------------------------------------------------------------------------
-- MedDesk —— PostgreSQL 首次初始化脚本（Docker Compose 方案专用）
-- 位置：deploy/scripts/initdb/00-init.sql
-- 说明：官方 postgres 镜像会把 /docker-entrypoint-initdb.d/*.sql 按文件名排序，
--       **仅在数据目录为空（首次启动）时**执行一次，之后修改本文件不会再次执行。
--       宿主机方案（路径 B）不需要本文件，请直接看 docs/DEPLOYMENT.md 第 2 步。
--
-- 【为什么放在这里】
--   CONTRACTS.md 5.1 指出：若实例未安装 citext 扩展，迁移需执行
--   `CREATE EXTENSION IF NOT EXISTS citext;`。但创建扩展通常需要库所有者权限，
--   把这一步前置到这里，可以让应用账号只拿最小权限，无需 SUPERUSER。
-- -----------------------------------------------------------------------------

\set ON_ERROR_STOP on

-- case-insensitive text：用于 users.email，保证邮箱大小写不敏感且唯一。
-- 若你的实例来自云厂商且禁止装扩展，请改为普通 text + lower(email) 唯一索引，
-- 并同步修改 internal/store 的迁移（由 A1 负责）。
CREATE EXTENSION IF NOT EXISTS citext;

-- pgcrypto 目前未使用（随机数是 Go 侧生成的），保留注释以免误装：
-- CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 提示：不要在这里 CREATE TABLE。表结构由应用启动时自动迁移创建
-- （MEDDESK_DB_MIGRATE_ON_BOOT=true，见 internal/store/migrations/0001_init.sql）。
