-- 记账本（ledger）模块初始 schema（迁移版本 0004_ledger）
--
-- 【架构决策（用户确认，2026-10-29）】
-- 记账数据**明文建表**：服务端需要做金额聚合、分类统计与每日自动备份，
-- 端到端加密下这些都做不到（服务端没有数据密钥）。代价是账本金额/分类/备注
-- 以明文存储于数据库，健康数据与同步记录仍保持端到端加密，两者互不影响。
-- 设置页与文档必须向用户明示这一点。
--
-- 【金额口径】
-- 一律使用 numeric(14,2) 定点数，**禁止浮点**。写入前由服务层校验
-- 正则 ^\d{1,12}(\.\d{1,2})?$ 且 > 0；聚合在 SQL 内 SUM(amount) 完成（精确），
-- 再以 (SUM(amount)*100)::bigint 得到整数分给前端计算与图表使用。
--
-- 【时区】
-- transaction_at 存 timestamptz；occurred_on 是 Asia/Shanghai 下的自然日，
-- 由服务层在写入时计算并落库，月度汇总与趋势均按它统计，避免反复做时区换算。
--
-- 幂等：全部使用 IF NOT EXISTS / CREATE OR REPLACE，可安全重复执行。

-- ------------------------------------------------------------------ 分类

CREATE TABLE IF NOT EXISTS ledger_categories (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text        NOT NULL,
    name       text        NOT NULL,
    sort_order integer     NOT NULL DEFAULT 0,
    -- 停用分类只影响"新增时能否选择"，历史账单靠 category_name_snapshot 保留名称。
    archived   boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_categories_kind_chk CHECK (kind IN ('income', 'expense')),
    CONSTRAINT ledger_categories_name_chk CHECK (length(name) BETWEEN 1 AND 32),
    CONSTRAINT ledger_categories_unique_name UNIQUE (user_id, kind, name)
);

CREATE INDEX IF NOT EXISTS idx_ledger_categories_user_kind
    ON ledger_categories (user_id, kind, sort_order, name);

-- ------------------------------------------------------------------ 账单

CREATE TABLE IF NOT EXISTS ledger_records (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type           text        NOT NULL,
    amount         numeric(14, 2) NOT NULL,
    currency       text        NOT NULL DEFAULT 'CNY',
    category_id    uuid,
    -- 快照：分类被停用或删除后，历史账单仍能看到当时的分类名。
    category_name_snapshot text NOT NULL,
    transaction_at timestamptz NOT NULL,
    occurred_on    date        NOT NULL,
    note           text        NOT NULL DEFAULT '',
    reimbursement_status text  NOT NULL DEFAULT 'none',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_records_type_chk  CHECK (type IN ('income', 'expense')),
    CONSTRAINT ledger_records_amount_chk CHECK (amount > 0),
    CONSTRAINT ledger_records_reimb_chk
        CHECK (reimbursement_status IN ('none', 'pending', 'done')),
    -- 收入不进入报销流程：只允许 none。
    CONSTRAINT ledger_records_income_no_reimb_chk
        CHECK (type <> 'income' OR reimbursement_status = 'none'),
    CONSTRAINT ledger_records_note_chk CHECK (length(note) <= 500),
    CONSTRAINT ledger_records_currency_chk CHECK (length(currency) = 3)
);

-- 列表主查询：按账号 + 发生时间倒序 + id 兜底（同一毫秒写入时排序稳定）
CREATE INDEX IF NOT EXISTS idx_ledger_records_user_time
    ON ledger_records (user_id, transaction_at DESC, id DESC);

-- 按类型筛选（收入/支出）
CREATE INDEX IF NOT EXISTS idx_ledger_records_user_type
    ON ledger_records (user_id, type, transaction_at DESC);

-- 按分类筛选与分类金额统计
CREATE INDEX IF NOT EXISTS idx_ledger_records_user_cat
    ON ledger_records (user_id, category_id, occurred_on);

-- 报销列表：只索引非 none 的少量记录，避免为绝大多数记录建索引条目
CREATE INDEX IF NOT EXISTS idx_ledger_records_user_reimb
    ON ledger_records (user_id, reimbursement_status)
    WHERE reimbursement_status <> 'none';

-- 关键词搜索（备注 ILIKE '%kw%'）在国内场景走全表扫描代价可接受，
-- 这里只按账号 + 日期范围收敛，不建 trigram 索引（避免引入扩展依赖）。
CREATE INDEX IF NOT EXISTS idx_ledger_records_user_occurred
    ON ledger_records (user_id, occurred_on);

-- ------------------------------------------------------------------ 凭证附件

CREATE TABLE IF NOT EXISTS ledger_attachments (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    record_id  uuid        NOT NULL REFERENCES ledger_records(id) ON DELETE CASCADE,
    rel_path   text        NOT NULL,   -- 相对上传根目录的路径，禁止 ../ 与绝对路径
    mime       text        NOT NULL,
    bytes      bigint      NOT NULL,
    width      integer,
    height     integer,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_attachments_mime_chk
        CHECK (mime IN ('image/jpeg', 'image/png', 'image/webp')),
    CONSTRAINT ledger_attachments_bytes_chk CHECK (bytes > 0 AND bytes <= 2 * 1024 * 1024)
);

CREATE INDEX IF NOT EXISTS idx_ledger_attachments_record
    ON ledger_attachments (user_id, record_id, created_at);

-- ------------------------------------------------------------------ 触发器

-- 复用已有的 updated_at 维护触发器（0001_init 已创建 meddesk_touch_updated_at）。
DROP TRIGGER IF EXISTS trg_ledger_categories_touch ON ledger_categories;
CREATE TRIGGER trg_ledger_categories_touch
    BEFORE UPDATE ON ledger_categories
    FOR EACH ROW EXECUTE FUNCTION meddesk_touch_updated_at();

DROP TRIGGER IF EXISTS trg_ledger_records_touch ON ledger_records;
CREATE TRIGGER trg_ledger_records_touch
    BEFORE UPDATE ON ledger_records
    FOR EACH ROW EXECUTE FUNCTION meddesk_touch_updated_at();

COMMENT ON TABLE ledger_records IS '记账本账单：明文存储（该模块放弃端到端加密，便于统计与自动备份）';
COMMENT ON COLUMN ledger_records.amount IS '定点数 numeric(14,2)，禁止浮点；单位由 currency 决定';
COMMENT ON COLUMN ledger_records.category_name_snapshot IS '分类名快照：分类停用/删除后历史账单仍可读';
