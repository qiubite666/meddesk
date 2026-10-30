-- 0005_ledger_backup 回滚脚本（仅用于手动回滚，自动迁移会跳过 *.down.sql）
--
-- 用法（示例）：
--   psql "$MEDDESK_DB_URL" -f internal/store/migrations/0005_ledger_backup.down.sql
--
-- 警告：执行后已保存的 WebDAV 凭据密文与备份运行记录将被删除，
-- 用户需要在设置页重新填写凭据；账本账单/分类（0004_ledger）不受影响。

DROP TRIGGER IF EXISTS trg_ledger_backup_runs_touch ON ledger_backup_runs;
DROP TRIGGER IF EXISTS trg_user_webdav_credentials_touch ON user_webdav_credentials;

DROP INDEX IF EXISTS idx_ledger_backup_runs_user_date;

DROP TABLE IF EXISTS ledger_backup_runs;
DROP TABLE IF EXISTS user_webdav_credentials;

DELETE FROM schema_migrations WHERE version = '0005_ledger_backup';
