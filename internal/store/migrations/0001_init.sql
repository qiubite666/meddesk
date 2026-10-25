-- MedDesk 服务端初始 schema（迁移版本 0001_init）
--
-- 设计说明（A1 决策，写在此处备查）：
--   1) 邮箱唯一性：契约 5.1 推荐使用 citext。但 CREATE EXTENSION 需要超级用户权限，
--      托管/受限实例上常常失败且会让整份迁移回滚。因此本迁移采用「尽力而为」策略：
--      先尝试 CREATE EXTENSION citext（失败只告警不中断）；若扩展可用则把
--      users.email 转为 citext，否则保持 text。
--      两种情况都额外建立 lower(email::text) 的唯一索引，仓储层查询统一写成
--      lower(email::text) = lower($1)，因此行为与索引使用在两种部署下完全一致。
--   2) updated_at 维护方式（二选一）：本迁移**采用触发器** meddesk_touch_updated_at()
--      统一维护 users / sync_state 的 updated_at，业务 SQL 不再显式写 now()，避免遗漏。
--      注意 sync_records.updated_at 是客户端毫秒时间戳（bigint，业务语义字段），
--      不受触发器影响；服务端写库时间单独记在 sync_records.updated_seq（timestamptz）。
--   3) 所有对象使用 IF NOT EXISTS / CREATE OR REPLACE，整份文件在单个事务内执行，
--      可安全重复执行（幂等由 schema_migrations 表在 Go 侧再次兜底）。
--   4) 时间字段一律 timestamptz，避免时区歧义。

-- ------------------------------------------------------------------ 扩展

-- 尝试安装 citext；无权限时仅告警，不中断迁移（退化为 text + lower(email) 唯一索引）。
DO $$
BEGIN
    BEGIN
        EXECUTE 'CREATE EXTENSION IF NOT EXISTS citext';
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'citext 扩展不可用（%），退化为 text + lower(email) 唯一索引', SQLERRM;
    END;
END $$;

-- ------------------------------------------------------------------ 用户

CREATE TABLE IF NOT EXISTS users (
    id            uuid        PRIMARY KEY,
    email         text        NOT NULL,
    password_hash text        NOT NULL, -- argon2id(客户端 PBKDF2 结果) 编码串
    kdf_salt      bytea       NOT NULL, -- 客户端 PBKDF2 salt（服务端无法解）
    wrapped_dek   bytea       NOT NULL, -- 被 KEK 包裹的 DEK（服务端无法解）
    dek_iv        bytea       NOT NULL, -- 包裹 DEK 的 AES-GCM IV
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 扩展可用时把邮箱列升级为 citext（大小写不敏感比较）。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'citext') THEN
        EXECUTE 'ALTER TABLE users ALTER COLUMN email TYPE citext USING email::citext';
    END IF;
END $$;

-- 大小写不敏感的唯一约束。citext 与 text 两种情况下语义一致。
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_key ON users (lower(email::text));

COMMENT ON TABLE  users            IS '用户主表（端到端加密：只存密文信封与 KDF 参数）';
COMMENT ON COLUMN users.password_hash IS 'argon2id 二次哈希结果，格式 $argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>';
COMMENT ON COLUMN users.kdf_salt   IS '客户端 PBKDF2 salt，服务端不可解密任何业务数据';

-- ------------------------------------------------------------------ 刷新令牌

CREATE TABLE IF NOT EXISTS refresh_tokens (
    id          uuid        PRIMARY KEY,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL UNIQUE, -- sha256(刷新令牌原文)，库中不存明文
    device_id   text,
    user_agent  text,
    ip          text, -- 契约 5.1 建议 inet；为空或含代理链时无法解析，故用 text 兜底
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    replaced_by uuid        -- 轮换后的新令牌 id（不建外键，避免级联删除复杂化）
);

CREATE INDEX IF NOT EXISTS refresh_tokens_user_id_idx   ON refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS refresh_tokens_expires_at_idx ON refresh_tokens (expires_at);

COMMENT ON TABLE refresh_tokens IS '刷新令牌（只存 sha256，支持轮换与重放检测）';

-- ------------------------------------------------------------------ 同步记录

CREATE TABLE IF NOT EXISTS sync_records (
    user_id     uuid   NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    table_name  text   NOT NULL, -- 逻辑表名，取值见 model.KnownTables()
    record_id   text   NOT NULL, -- 客户端生成的记录 UUID
    ver         bigint NOT NULL DEFAULT 1,
    updated_at  bigint NOT NULL, -- 客户端修改时刻（Unix 毫秒），冲突裁决依据
    deleted     boolean NOT NULL DEFAULT false, -- 墓碑
    iv          bytea  NOT NULL, -- AES-GCM IV（12 字节）
    ct          bytea  NOT NULL, -- AES-GCM 密文（含 16 字节 tag）
    device_id   text,
    seq         bigint NOT NULL, -- 服务端单调递增序列，pull 游标依据
    updated_seq timestamptz NOT NULL DEFAULT now(), -- 服务端写库时刻
    PRIMARY KEY (user_id, table_name, record_id)
);

CREATE INDEX IF NOT EXISTS sync_records_user_seq_idx   ON sync_records (user_id, seq);
CREATE INDEX IF NOT EXISTS sync_records_user_table_idx ON sync_records (user_id, table_name);

COMMENT ON TABLE sync_records IS '加密同步记录（服务端只存密文，永不解密）';

-- ------------------------------------------------------------------ 同步游标状态

CREATE TABLE IF NOT EXISTS sync_state (
    user_id    uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    last_seq   bigint      NOT NULL DEFAULT 0, -- 已分配的最大 seq
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE sync_state IS '每用户一行，保存同步序列号分配器';

-- ------------------------------------------------------------------ 附件

CREATE TABLE IF NOT EXISTS attachments (
    id         uuid        PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    record_id  text,       -- 关联的业务记录 id（可为空）
    iv         bytea       NOT NULL, -- 附件密文的 AES-GCM IV
    size       bigint      NOT NULL,
    mime       text        NOT NULL,
    path       text        NOT NULL, -- 相对 UploadDir 的密文落盘路径
    seq        bigint      NOT NULL DEFAULT 0, -- 预留：未来纳入同步序列
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS attachments_user_record_idx ON attachments (user_id, record_id);

COMMENT ON TABLE attachments IS '附件元数据（内容以密文落盘，服务端不可读）';

-- ------------------------------------------------------------------ 迁移记录

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    text        PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE schema_migrations IS '已执行迁移版本（Go 侧自建，保证幂等）';

-- ------------------------------------------------------------------ updated_at 触发器

CREATE OR REPLACE FUNCTION meddesk_touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_touch_updated_at ON users;
CREATE TRIGGER users_touch_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION meddesk_touch_updated_at();

DROP TRIGGER IF EXISTS sync_state_touch_updated_at ON sync_state;
CREATE TRIGGER sync_state_touch_updated_at
    BEFORE UPDATE ON sync_state
    FOR EACH ROW EXECUTE FUNCTION meddesk_touch_updated_at();
