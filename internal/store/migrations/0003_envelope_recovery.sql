-- 口令信封的「待修复」状态与可选的恢复信封（密钥托管）。
--
-- 背景：DEK 只被「口令派生的 KEK」包裹，服务端拿不到明文 DEK。
-- 因此管理员改口令时若换一把新 DEK，旧密文永久不可解（旧行为）。
-- 这里提供两条无损路径：
--
--   1) envelope_stale：管理员只改 password_hash + kdf_salt，信封原样保留并打标。
--      用户下次在「曾登录过、本地仍持有 DEK」的设备上登录时，客户端用本地 DEK
--      重新包裹并调用 /auth/repair-envelope，数据一条不动。服务端全程拿不到 DEK。
--
--   2) recovery_wrapped_dek：账号可以选择开启「托管恢复」——客户端把 DEK 用服务端
--      恢复公钥（RSA-OAEP）再包一份。开启后管理员可以真正直接改口令（任何设备都能
--      立刻用新口令登录），代价是持有恢复私钥的一方可解密该账号数据。
--      默认关闭；关闭时把两列清空即可。

ALTER TABLE users ADD COLUMN IF NOT EXISTS envelope_stale    boolean     NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS envelope_stale_at timestamptz;

-- 上一份信封，仅用于管理员误操作后回滚（改密前的 wrapped_dek/dek_iv）。
ALTER TABLE users ADD COLUMN IF NOT EXISTS wrapped_dek_prev bytea;
ALTER TABLE users ADD COLUMN IF NOT EXISTS dek_iv_prev      bytea;

-- 托管恢复信封：被服务端恢复公钥包裹的 DEK（RSA-OAEP-SHA256，长度 = 密钥模长）。
-- recovery_iv 预留给将来换成 ECDH+AES-GCM 的实现，RSA 方案下为空。
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_wrapped_dek bytea;
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_iv          bytea;
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_at          timestamptz;

-- 后台列表要按「是否可完全恢复」筛选/展示，单独建索引不划算（用户量小），
-- 但 stale 状态的账号在登录路径上会被读到，这里补一个部分索引避免全表扫。
CREATE INDEX IF NOT EXISTS idx_users_envelope_stale
    ON users (id) WHERE envelope_stale;

COMMENT ON COLUMN users.envelope_stale IS '口令已改但信封仍是旧口令包裹，等待持有 DEK 的设备自愈';
COMMENT ON COLUMN users.recovery_wrapped_dek IS '被服务端恢复公钥包裹的 DEK（账号自愿开启的托管恢复）';
