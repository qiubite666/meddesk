-- 刷新令牌族绝对过期（会话最长存活时间）
--
-- 【为什么单独加一列】expires_at 是"本次令牌"的有效期，每次轮换都会重置为 now+TTL，
-- 于是只要每周用一次，会话就可以无限续期；令牌一旦泄露，攻击者能永久续命。
-- family_expires_at 记录"这一族会话的最晚存活时刻"，轮换时原样继承、不再延后。
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS family_expires_at timestamptz;

-- 历史行回填：按创建时间 + 90 天（比默认 30 天 TTL 长，避免升级瞬间把所有活跃会话踢下线）。
UPDATE refresh_tokens
   SET family_expires_at = created_at + interval '90 days'
 WHERE family_expires_at IS NULL;

ALTER TABLE refresh_tokens ALTER COLUMN family_expires_at SET NOT NULL;

COMMENT ON COLUMN refresh_tokens.family_expires_at IS '会话族最晚存活时刻；轮换继承，不可延长';
