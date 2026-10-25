-- 0001_init 的回滚脚本（仅用于本地开发重置，生产请走备份恢复）。
-- Go 侧 Migrate 会跳过所有 *.down.sql 文件，不会自动执行本文件。

DROP TRIGGER IF EXISTS sync_state_touch_updated_at ON sync_state;
DROP TRIGGER IF EXISTS users_touch_updated_at ON users;
DROP FUNCTION IF EXISTS meddesk_touch_updated_at();

DROP TABLE IF EXISTS schema_migrations;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS sync_state;
DROP TABLE IF EXISTS sync_records;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
