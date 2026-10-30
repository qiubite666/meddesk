-- 0004_ledger 回滚脚本（仅用于手动回滚，自动迁移会跳过 *.down.sql）
--
-- 用法（示例）：
--   psql "$MEDDESK_DB_URL" -f internal/store/migrations/0004_ledger.down.sql
--
-- 警告：执行后该模块的账单与凭证元数据将被删除，请务必先备份数据库。
-- 健康数据、同步记录、用户表不受影响。

DROP TRIGGER IF EXISTS trg_ledger_records_touch ON ledger_records;
DROP TRIGGER IF EXISTS trg_ledger_categories_touch ON ledger_categories;

DROP INDEX IF EXISTS idx_ledger_attachments_record;
DROP INDEX IF EXISTS idx_ledger_records_user_occurred;
DROP INDEX IF EXISTS idx_ledger_records_user_reimb;
DROP INDEX IF EXISTS idx_ledger_records_user_cat;
DROP INDEX IF EXISTS idx_ledger_records_user_type;
DROP INDEX IF EXISTS idx_ledger_records_user_time;
DROP INDEX IF EXISTS idx_ledger_categories_user_kind;

DROP TABLE IF EXISTS ledger_attachments;
DROP TABLE IF EXISTS ledger_records;
DROP TABLE IF EXISTS ledger_categories;

DELETE FROM schema_migrations WHERE version = '0004_ledger';
