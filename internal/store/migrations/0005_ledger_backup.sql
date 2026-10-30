-- 记账本每日自动备份（迁移版本 0005_ledger_backup）
--
-- 【背景】契约 0/7 节：记账数据明文建表，服务端每天 02:00（Asia/Shanghai）生成
-- ledger-backup-YYYY-MM-DD.zip 全量备份并上传到用户自己的 WebDAV。要做到"服务端定时"
-- 就必须把用户的 WebDAV 凭据**加密持久化**（这是用户明确批准的变更，与 internal/webdav
-- 代理包"凭据随单次请求带来、不落盘"的设计并不冲突——那个包仍然是纯代理）。
--
-- 【为什么必须加密】
-- WebDAV 账号口令等价于"用户整个网盘的写权限"。明文落库意味着一次数据库泄露
-- （备份、SQL 注入、运维误操作）就扩散成网盘泄露。这里用 AES-256-GCM 加密，
-- 密钥来自环境变量 MEDDESK_SECRETBOX_KEY（base64 的 32 字节），不进数据库、不进代码。
--
-- 【nonce 约定】
-- AES-GCM 在同一密钥下**绝不允许复用 nonce**（复用会导致认证密钥直接可恢复）。
-- 因此每个字段独立生成 12 字节 nonce，四个 nonce 按顺序拼接存进 iv 列（共 48 字节）：
--   [0:12] base_url   [12:24] username   [24:36] password   [36:48] path_prefix
-- 密文的附加认证数据（AAD）绑定 user_id + 字段名，密文被复制到别的账号/字段会解密失败。
--
-- 【幂等】全部使用 IF NOT EXISTS / DROP TRIGGER IF EXISTS，可安全重复执行。

-- ------------------------------------------------------------------ 用户 WebDAV 凭据（密文）

CREATE TABLE IF NOT EXISTS user_webdav_credentials (
    user_id        uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    -- 四个字段的密文（AES-256-GCM，密钥不落库）
    base_url_ct    bytea       NOT NULL,
    username_ct    bytea       NOT NULL,
    password_ct    bytea       NOT NULL,
    path_prefix_ct bytea       NOT NULL,
    -- 4 × 12 字节 nonce 的拼接（顺序见文件头），长度固定 48
    iv             bytea       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_webdav_credentials_iv_chk CHECK (length(iv) = 48),
    CONSTRAINT user_webdav_credentials_url_chk CHECK (length(base_url_ct) > 0),
    CONSTRAINT user_webdav_credentials_user_chk CHECK (length(username_ct) > 0),
    CONSTRAINT user_webdav_credentials_pass_chk CHECK (length(password_ct) > 0)
);

COMMENT ON TABLE user_webdav_credentials IS '用户 WebDAV 凭据密文：AES-256-GCM，密钥来自环境变量，明文永不落库';
COMMENT ON COLUMN user_webdav_credentials.iv IS '4 个 12 字节 GCM nonce 的拼接（base_url/username/password/path_prefix），禁止复用';

-- ------------------------------------------------------------------ 备份运行记录

CREATE TABLE IF NOT EXISTS ledger_backup_runs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- 备份所对应的自然日（Asia/Shanghai）；与唯一约束配合实现"一天一行"的幂等
    run_date      date        NOT NULL,
    status        text        NOT NULL,
    remote_path   text        NOT NULL DEFAULT '',
    size_bytes    bigint      NOT NULL DEFAULT 0,
    record_count  integer     NOT NULL DEFAULT 0,
    -- 失败原因摘要（**绝不包含凭据**），成功时为空串
    message       text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_backup_runs_status_chk
        CHECK (status IN ('running', 'success', 'failed')),
    CONSTRAINT ledger_backup_runs_size_chk CHECK (size_bytes >= 0),
    CONSTRAINT ledger_backup_runs_count_chk CHECK (record_count >= 0),
    -- 同一账号同一天只保留一行：重复执行是覆盖（UPDATE）而不是新增，
    -- 与"同名 ZIP 覆盖上传"共同保证幂等，不会产生不可控的重复文件。
    CONSTRAINT ledger_backup_runs_unique_day UNIQUE (user_id, run_date)
);

CREATE INDEX IF NOT EXISTS idx_ledger_backup_runs_user_date
    ON ledger_backup_runs (user_id, run_date DESC);

COMMENT ON TABLE ledger_backup_runs IS '记账本每日备份运行记录：UNIQUE(user_id, run_date) 保证一天一行（幂等覆盖）';
COMMENT ON COLUMN ledger_backup_runs.message IS '失败原因摘要，禁止写入凭据或完整 URL';

-- ------------------------------------------------------------------ 触发器

-- 复用 0001_init 创建的 meddesk_touch_updated_at()；业务 SQL 不显式写 updated_at。
DROP TRIGGER IF EXISTS trg_user_webdav_credentials_touch ON user_webdav_credentials;
CREATE TRIGGER trg_user_webdav_credentials_touch
    BEFORE UPDATE ON user_webdav_credentials
    FOR EACH ROW EXECUTE FUNCTION meddesk_touch_updated_at();

DROP TRIGGER IF EXISTS trg_ledger_backup_runs_touch ON ledger_backup_runs;
CREATE TRIGGER trg_ledger_backup_runs_touch
    BEFORE UPDATE ON ledger_backup_runs
    FOR EACH ROW EXECUTE FUNCTION meddesk_touch_updated_at();
