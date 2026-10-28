-- 家庭关联授权表（短码方案）
--
-- 设计要点：
--  1. 服务端**不保存明文短码**，只保存 HMAC(pepper, code) 作为查找键。
--     用 HMAC 而不是裸 sha256：短码只有 40 bit 熵，裸哈希在库泄露后可被离线爆破；
--     加上只在进程内的 pepper，攻击者拿到库也无法反推。
--  2. 服务端**看不到任何业务内容**：wrapped_dek 是用短码派生的 KEK 加密的 owner DEK，
--     解密需要短码本身，而短码不在库里。
--  3. expires_at 为 NULL 表示永久有效（产品决定：授权一旦建立就一直有效，直到显式撤销）。
--     pending 状态的短码仍受 24 小时限制，避免长期悬挂的空码被撞库。
--  4. status 三态：pending（待兑换）→ active（已绑定查看方）→ revoked（已撤销）。

CREATE TABLE IF NOT EXISTS family_grants (
    id          uuid        PRIMARY KEY,
    owner_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    viewer_id   uuid        REFERENCES users (id) ON DELETE CASCADE,
    code_lookup bytea       NOT NULL,
    kdf_salt    bytea       NOT NULL,
    wrapped_dek bytea       NOT NULL,
    dek_iv      bytea       NOT NULL,
    status      text        NOT NULL DEFAULT 'pending',
    expires_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    redeemed_at timestamptz,
    revoked_at  timestamptz,
    CONSTRAINT family_grants_status_chk CHECK (status IN ('pending', 'active', 'revoked')),
    CONSTRAINT family_grants_redeemed_chk CHECK (
        (status = 'pending' AND viewer_id IS NULL)
        OR (status <> 'pending' AND viewer_id IS NOT NULL)
    )
);

-- 短码查找：全局唯一，兑换时按此列定位
CREATE UNIQUE INDEX IF NOT EXISTS family_grants_code_lookup_key
    ON family_grants (code_lookup);

-- 所有者的授权列表
CREATE INDEX IF NOT EXISTS family_grants_owner_idx
    ON family_grants (owner_id, status);

-- 查看方可见的授权（拉取他人记录时按此索引）
CREATE INDEX IF NOT EXISTS family_grants_viewer_idx
    ON family_grants (viewer_id, status);

-- 同一对账号只允许一条 active 授权，避免重复授权导致权限膨胀
CREATE UNIQUE INDEX IF NOT EXISTS family_grants_pair_uidx
    ON family_grants (owner_id, viewer_id)
    WHERE status = 'active';

COMMENT ON TABLE  family_grants             IS '家庭关联授权：短码兑换与跨账号只读授权';
COMMENT ON COLUMN family_grants.code_lookup IS 'HMAC(pepper, 短码)，服务端不存明文短码';
COMMENT ON COLUMN family_grants.wrapped_dek IS '被短码派生 KEK 包裹的 owner DEK，服务端不可解';
COMMENT ON COLUMN family_grants.expires_at  IS 'NULL 表示永久有效';
