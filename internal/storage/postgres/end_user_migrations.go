package postgres

// End-user identity, token and spending-limit tables. Split out of migrations.go
// to keep that file's migration list readable as it grows.

// endUserLegacyPasswordLockStateSQL records that the one-shot pass locking
// accounts still on the legacy backfill password has run (see
// enduser.LockLegacyBackfillPasswordAccounts). The pass lives in Go because
// recognising the password takes a bcrypt comparison; this row keeps it from
// re-running on every boot, and locked_count outlives the boot log line.
const endUserLegacyPasswordLockStateSQL = `
CREATE TABLE IF NOT EXISTS end_user_legacy_password_lock_state (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  done_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  locked_count INTEGER NOT NULL DEFAULT 0
);
`

const endUserDailySpendingResetsSQL = `
CREATE TABLE IF NOT EXISTS end_user_daily_spending_resets (
  tenant_id     UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  end_user_id   UUID NOT NULL,
  day_key       TEXT NOT NULL DEFAULT '',
  cost_baseline DOUBLE PRECISION NOT NULL DEFAULT 0,
  reset_at      TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (tenant_id, end_user_id)
);
CREATE INDEX IF NOT EXISTS idx_end_user_daily_spending_resets_day
  ON end_user_daily_spending_resets(tenant_id, day_key);
`

// BIGSERIAL for PG; SQLite bootstrap uses INTEGER PRIMARY KEY AUTOINCREMENT.
const endUserDailySpendingResetEventsSQL = `
CREATE TABLE IF NOT EXISTS end_user_daily_spending_reset_events (
  id                     BIGSERIAL PRIMARY KEY,
  tenant_id              UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  end_user_id            UUID NOT NULL,
  day_key                TEXT NOT NULL DEFAULT '',
  reset_at               TIMESTAMPTZ NOT NULL,
  actor_user_id          TEXT NOT NULL DEFAULT '',
  actor_username         TEXT NOT NULL DEFAULT '',
  actor_kind             TEXT NOT NULL DEFAULT '',
  cost_baseline          DOUBLE PRECISION NOT NULL DEFAULT 0,
  effective_used_before  DOUBLE PRECISION NOT NULL DEFAULT 0,
  raw_today_cost         DOUBLE PRECISION NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_end_user_daily_spending_reset_events_user
  ON end_user_daily_spending_reset_events(tenant_id, end_user_id, reset_at DESC);
`

// Quota + permission template live on end_users so multiple keys share one pool.
// Backfill from each user's default key (or earliest key) then zero owned key limits
// so creating extra keys cannot mint independent budgets.
const endUserAccountQuotaSQL = `
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS permission_profile_id TEXT NOT NULL DEFAULT '';
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS daily_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS total_quota INTEGER NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS spending_limit DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS daily_spending_limit DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS concurrency_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS rpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS tpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS allowed_models TEXT NOT NULL DEFAULT '[]';
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS allowed_channels TEXT NOT NULL DEFAULT '[]';
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS allowed_channel_groups TEXT NOT NULL DEFAULT '[]';
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS system_prompt TEXT NOT NULL DEFAULT '';

UPDATE end_users AS eu
SET
  permission_profile_id = COALESCE(NULLIF(src.permission_profile_id, ''), eu.permission_profile_id),
  daily_limit = CASE WHEN src.daily_limit > 0 THEN src.daily_limit ELSE eu.daily_limit END,
  total_quota = CASE WHEN src.total_quota > 0 THEN src.total_quota ELSE eu.total_quota END,
  spending_limit = CASE WHEN src.spending_limit > 0 THEN src.spending_limit ELSE eu.spending_limit END,
  daily_spending_limit = CASE WHEN src.daily_spending_limit > 0 THEN src.daily_spending_limit ELSE eu.daily_spending_limit END,
  concurrency_limit = CASE WHEN src.concurrency_limit > 0 THEN src.concurrency_limit ELSE eu.concurrency_limit END,
  rpm_limit = CASE WHEN src.rpm_limit > 0 THEN src.rpm_limit ELSE eu.rpm_limit END,
  tpm_limit = CASE WHEN src.tpm_limit > 0 THEN src.tpm_limit ELSE eu.tpm_limit END,
  allowed_models = CASE WHEN src.allowed_models IS NOT NULL AND src.allowed_models <> '' AND src.allowed_models <> '[]'
    THEN src.allowed_models ELSE eu.allowed_models END,
  allowed_channels = CASE WHEN src.allowed_channels IS NOT NULL AND src.allowed_channels <> '' AND src.allowed_channels <> '[]'
    THEN src.allowed_channels ELSE eu.allowed_channels END,
  allowed_channel_groups = CASE WHEN src.allowed_channel_groups IS NOT NULL AND src.allowed_channel_groups <> '' AND src.allowed_channel_groups <> '[]'
    THEN src.allowed_channel_groups ELSE eu.allowed_channel_groups END,
  system_prompt = CASE WHEN src.system_prompt IS NOT NULL AND src.system_prompt <> ''
    THEN src.system_prompt ELSE eu.system_prompt END
FROM (
  SELECT DISTINCT ON (end_user_id)
    end_user_id,
    permission_profile_id,
    daily_limit,
    total_quota,
    spending_limit,
    daily_spending_limit,
    concurrency_limit,
    rpm_limit,
    tpm_limit,
    allowed_models,
    allowed_channels,
    allowed_channel_groups,
    system_prompt
  FROM api_keys
  WHERE end_user_id IS NOT NULL
  ORDER BY end_user_id, is_default DESC, created_at ASC NULLS LAST, id ASC
) AS src
WHERE eu.id = src.end_user_id;

UPDATE api_keys
SET
  permission_profile_id = '',
  daily_limit = 0,
  total_quota = 0,
  spending_limit = 0,
  daily_spending_limit = 0,
  concurrency_limit = 0,
  rpm_limit = 0,
  tpm_limit = 0,
  allowed_models = '[]',
  allowed_channels = '[]',
  allowed_channel_groups = '[]',
  system_prompt = ''
WHERE end_user_id IS NOT NULL;
`

const endUsersAndTokensSQL = `
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS access_token_ttl_seconds INTEGER NOT NULL DEFAULT 43200;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS refresh_token_ttl_seconds INTEGER NOT NULL DEFAULT 2592000;

CREATE TABLE IF NOT EXISTS end_users (
  id                    UUID PRIMARY KEY,
  tenant_id             UUID NOT NULL REFERENCES tenants(id),
  username              TEXT NOT NULL,
  username_normalized   TEXT NOT NULL,
  display_name          TEXT NOT NULL,
  password_hash         TEXT NOT NULL,
  status                TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'locked')),
  must_change_password  BOOLEAN NOT NULL DEFAULT false,
  password_changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at         TIMESTAMPTZ,
  failed_login_count    INTEGER NOT NULL DEFAULT 0,
  lock_stage            INTEGER NOT NULL DEFAULT 0,
  locked_until          TIMESTAMPTZ,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  version               BIGINT NOT NULL DEFAULT 1,
  UNIQUE (username_normalized)
);
CREATE INDEX IF NOT EXISTS idx_end_users_tenant_status ON end_users(tenant_id, status);

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS end_user_id UUID REFERENCES end_users(id) ON DELETE SET NULL;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS is_default BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_api_keys_end_user ON api_keys(tenant_id, end_user_id);
-- at most one default key per end user
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_one_default_per_user
  ON api_keys(tenant_id, end_user_id) WHERE is_default = true AND end_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS end_user_backfill_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  done_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS end_user_sessions (
  id                  UUID PRIMARY KEY,
  end_user_id         UUID NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
  tenant_id           UUID NOT NULL REFERENCES tenants(id),
  access_token_hash   TEXT NOT NULL UNIQUE,
  refresh_token_hash  TEXT NOT NULL UNIQUE,
  access_expires_at   TIMESTAMPTZ NOT NULL,
  refresh_expires_at  TIMESTAMPTZ NOT NULL,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at          TIMESTAMPTZ,
  revoke_reason       TEXT NOT NULL DEFAULT '',
  user_agent_hash     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_end_user_sessions_user_active
  ON end_user_sessions(end_user_id, revoked_at, refresh_expires_at);

ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS refresh_token_hash TEXT;
ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS refresh_expires_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_sessions_refresh_token_hash
  ON user_sessions(refresh_token_hash) WHERE refresh_token_hash IS NOT NULL AND refresh_token_hash <> '';

INSERT INTO permissions (code, name, description, scope, resource, action, sensitive, sort_order, updated_at)
VALUES
  ('end_users.read', 'Read end users', 'List and view portal end users', 'tenant', 'end_users', 'read', true, 410, now()),
  ('end_users.write', 'Write end users', 'Create, update, delete portal end users and their keys', 'tenant', 'end_users', 'write', true, 420, now())
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_permissions (role_id, permission_code)
SELECT r.id, p.code
  FROM roles r
  CROSS JOIN (VALUES ('end_users.read'), ('end_users.write')) AS p(code)
 WHERE r.code IN ('tenant_admin', 'platform_super_admin')
ON CONFLICT DO NOTHING;

-- Menu row is seeded from MenuCatalog (identity bootstrap), not here:
-- parent group.access may be absent on partial upgrade baselines, which would
-- fail menus_parent_code_fkey and leave the migration dirty.
`
