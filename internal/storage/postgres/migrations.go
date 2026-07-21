package postgres

func RuntimeMigrations() []Migration {
	return []Migration{
		{Version: "202607050001_runtime_schema", SQL: runtimeSchemaSQL},
		{Version: "202607100001_identity_fingerprint_profiles", SQL: identityFingerprintProfilesSQL},
		{Version: "202607110001_multi_tenant_identity", SQL: multiTenantIdentitySQL},
		{Version: "202607110002_multi_tenant_scope", SQL: multiTenantScopeSQL},
		{Version: "202607110003_multi_tenant_constraints", SQL: multiTenantConstraintsSQL},
		{Version: "202607110004_identity_delete_constraints", SQL: identityDeleteConstraintsSQL},
		{Version: "202607110005_ccswitch_tenant_primary_key", SQL: ccSwitchTenantPrimaryKeySQL},
		{Version: "202607120001_dynamic_menus", SQL: dynamicMenusSQL},
		{Version: "202607120002_menu_management_v2", SQL: menuManagementV2SQL},
		{Version: "202607130001_model_config_openrouter_metadata", SQL: modelConfigOpenRouterMetadataSQL},
		// AI Accounts page fans out entity-stats + per-card auth-file-trend over
		// request_logs; composite indexes avoid sequential scans under concurrent load.
		{Version: "202607160001_request_logs_auth_lookup_indexes", SQL: requestLogsAuthLookupIndexesSQL},
		// Latest AI-account status + daily usage projection so cards stop scanning request_logs.
		{Version: "202607160002_ai_account_status_read_model", SQL: aiAccountStatusReadModelSQL},
		// Manual same-day daily spending reset baseline per API key (does not delete request_logs).
		{Version: "202607160003_api_key_daily_spending_resets", SQL: apiKeyDailySpendingResetsSQL},
		// Daily USD spending limit belongs on reusable permission profiles.
		{Version: "202607170001_profile_daily_spending_limit", SQL: profileDailySpendingLimitSQL},
		// Append-only history of manual daily spending resets (who/when/amount).
		{Version: "202607170002_api_key_daily_spending_reset_events", SQL: apiKeyDailySpendingResetEventsSQL},
		// End-user portal accounts (isolated from admin users) + multi-key ownership + refresh tokens.
		{Version: "202607170003_end_users_and_tokens", SQL: endUsersAndTokensSQL},
		// Account-level quota/permissions: shared across all API keys of an end user.
		{Version: "202607190001_end_user_account_quota", SQL: endUserAccountQuotaSQL},
		// Account-level same-day spending reset baseline for end-user quota pools.
		{Version: "202607190002_end_user_daily_spending_resets", SQL: endUserDailySpendingResetsSQL},
		// Small usage rollup so stats/limits stop scanning request_logs.
		{Version: "202607200001_usage_rollup_buckets", SQL: usageRollupBucketsSQL},
		// Shared physical AI-account subjects; credentials remain tenant-private bindings.
		{Version: "202607200002_ai_account_shared_subjects", SQL: aiAccountSharedSubjectsSQL},
		// Append-only history of manual account-level daily spending resets.
		{Version: "202607210001_end_user_daily_spending_reset_events", SQL: endUserDailySpendingResetEventsSQL},
	}
}

const usageRollupBucketsSQL = `
CREATE TABLE IF NOT EXISTS usage_rollup_buckets (
  tenant_id               UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  bucket_kind             TEXT NOT NULL,
  bucket_start            TEXT NOT NULL,
  api_key_id              TEXT NOT NULL DEFAULT '',
  end_user_id             TEXT NOT NULL DEFAULT '',
  auth_subject_id         TEXT NOT NULL DEFAULT '',
  model                   TEXT NOT NULL DEFAULT '',
  source                  TEXT NOT NULL DEFAULT '',
  channel_name            TEXT NOT NULL DEFAULT '',
  request_count           BIGINT NOT NULL DEFAULT 0,
  success_count           BIGINT NOT NULL DEFAULT 0,
  failure_count           BIGINT NOT NULL DEFAULT 0,
  streaming_count         BIGINT NOT NULL DEFAULT 0,
  input_tokens            BIGINT NOT NULL DEFAULT 0,
  output_tokens           BIGINT NOT NULL DEFAULT 0,
  reasoning_tokens        BIGINT NOT NULL DEFAULT 0,
  cached_tokens           BIGINT NOT NULL DEFAULT 0,
  effective_input_tokens  BIGINT NOT NULL DEFAULT 0,
  total_tokens            BIGINT NOT NULL DEFAULT 0,
  cost_total              DOUBLE PRECISION NOT NULL DEFAULT 0,
  latency_sum_ms          BIGINT NOT NULL DEFAULT 0,
  latency_count           BIGINT NOT NULL DEFAULT 0,
  first_token_sum_ms      BIGINT NOT NULL DEFAULT 0,
  first_token_count       BIGINT NOT NULL DEFAULT 0,
  updated_at              TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (
    tenant_id, bucket_kind, bucket_start,
    api_key_id, end_user_id, auth_subject_id,
    model, source, channel_name
  )
);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_kind_start
  ON usage_rollup_buckets(tenant_id, bucket_kind, bucket_start);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_key_day
  ON usage_rollup_buckets(tenant_id, bucket_kind, api_key_id, bucket_start);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_user_day
  ON usage_rollup_buckets(tenant_id, bucket_kind, end_user_id, bucket_start);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_subject_day
  ON usage_rollup_buckets(tenant_id, bucket_kind, auth_subject_id, bucket_start);

CREATE TABLE IF NOT EXISTS request_log_storage_state (
  id                        INTEGER PRIMARY KEY CHECK (id = 1),
  metadata_row_count        BIGINT NOT NULL DEFAULT 0,
  content_row_count         BIGINT NOT NULL DEFAULT 0,
  last_cleanup_started_at   TIMESTAMPTZ,
  last_cleanup_finished_at  TIMESTAMPTZ,
  last_cleanup_status       TEXT NOT NULL DEFAULT '',
  last_cleanup_deleted_rows BIGINT NOT NULL DEFAULT 0,
  last_cleanup_duration_ms  BIGINT NOT NULL DEFAULT 0,
  last_cleanup_error        TEXT NOT NULL DEFAULT '',
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO request_log_storage_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
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

const profileDailySpendingLimitSQL = `
ALTER TABLE api_key_permission_profiles
  ADD COLUMN IF NOT EXISTS daily_spending_limit DOUBLE PRECISION NOT NULL DEFAULT 0;
`

// TIMESTAMPTZ matches other PG runtime tables; SQLite bootstrap uses TIMESTAMP (see usage package).
const apiKeyDailySpendingResetsSQL = `
CREATE TABLE IF NOT EXISTS api_key_daily_spending_resets (
  tenant_id     UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  api_key_id    TEXT NOT NULL,
  day_key       TEXT NOT NULL DEFAULT '',
  cost_baseline DOUBLE PRECISION NOT NULL DEFAULT 0,
  reset_at      TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (tenant_id, api_key_id)
);
CREATE INDEX IF NOT EXISTS idx_api_key_daily_spending_resets_day
  ON api_key_daily_spending_resets(tenant_id, day_key);
`

// BIGSERIAL for PG; SQLite bootstrap uses INTEGER PRIMARY KEY AUTOINCREMENT.
const apiKeyDailySpendingResetEventsSQL = `
CREATE TABLE IF NOT EXISTS api_key_daily_spending_reset_events (
  id                     BIGSERIAL PRIMARY KEY,
  tenant_id              UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  api_key_id             TEXT NOT NULL,
  day_key                TEXT NOT NULL DEFAULT '',
  reset_at               TIMESTAMPTZ NOT NULL,
  actor_user_id          TEXT NOT NULL DEFAULT '',
  actor_username         TEXT NOT NULL DEFAULT '',
  actor_kind             TEXT NOT NULL DEFAULT '',
  cost_baseline          DOUBLE PRECISION NOT NULL DEFAULT 0,
  effective_used_before  DOUBLE PRECISION NOT NULL DEFAULT 0,
  raw_today_cost         DOUBLE PRECISION NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_key_daily_spending_reset_events_key
  ON api_key_daily_spending_reset_events(tenant_id, api_key_id, reset_at DESC);
`

const requestLogsAuthLookupIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_index_time
  ON request_logs(tenant_id, auth_index, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_subject_time_cost
  ON request_logs(tenant_id, auth_subject_id, timestamp DESC)
  INCLUDE (cost);
`

// Schema only: historical backfill uses usageLoc at process init (not UTC-hardcoded SQL).
const aiAccountStatusReadModelSQL = `
CREATE TABLE IF NOT EXISTS ai_account_status (
  tenant_id                 UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  auth_subject_id           TEXT NOT NULL,
  auth_index                TEXT NOT NULL DEFAULT '',
  provider                  TEXT NOT NULL DEFAULT '',
  refresh_state             TEXT NOT NULL DEFAULT 'idle',
  health_status             TEXT NOT NULL DEFAULT '',
  plan_type                 TEXT NOT NULL DEFAULT '',
  restriction_summary       TEXT NOT NULL DEFAULT '',
  error_summary             TEXT NOT NULL DEFAULT '',
  error_code                TEXT NOT NULL DEFAULT '',
  error_message             TEXT NOT NULL DEFAULT '',
  quota_json                TEXT NOT NULL DEFAULT '[]',
  reset_credit_count        BIGINT,
  reset_credit_expirations  TEXT NOT NULL DEFAULT '[]',
  upstream_checked_at       TIMESTAMPTZ,
  usage_updated_at          TIMESTAMPTZ,
  expires_at                TIMESTAMPTZ,
  version                   BIGINT NOT NULL DEFAULT 0,
  updated_at                TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (tenant_id, auth_subject_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_status_tenant_auth_index
  ON ai_account_status(tenant_id, auth_index);
CREATE INDEX IF NOT EXISTS idx_ai_account_status_tenant_refresh
  ON ai_account_status(tenant_id, refresh_state, updated_at);

CREATE TABLE IF NOT EXISTS auth_subject_usage_daily (
  tenant_id       UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  auth_subject_id TEXT NOT NULL,
  day_key         TEXT NOT NULL,
  request_count   BIGINT NOT NULL DEFAULT 0,
  success_count   BIGINT NOT NULL DEFAULT 0,
  failure_count   BIGINT NOT NULL DEFAULT 0,
  cost_total      DOUBLE PRECISION NOT NULL DEFAULT 0,
  updated_at      TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (tenant_id, auth_subject_id, day_key)
);
CREATE INDEX IF NOT EXISTS idx_auth_subject_usage_daily_tenant_day
  ON auth_subject_usage_daily(tenant_id, day_key);

CREATE TABLE IF NOT EXISTS usage_projection_markers (
  marker_key   TEXT PRIMARY KEY,
  marker_value TEXT NOT NULL DEFAULT '',
  updated_at   TIMESTAMPTZ NOT NULL
);
`

const runtimeSchemaSQL = `
CREATE TABLE IF NOT EXISTS request_logs (
  id               BIGSERIAL PRIMARY KEY,
  timestamp        TIMESTAMPTZ NOT NULL,
  api_key          TEXT NOT NULL DEFAULT '',
  api_key_id       TEXT NOT NULL DEFAULT '',
  auth_subject_id  TEXT NOT NULL DEFAULT '',
  api_key_name     TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  upstream_model   TEXT NOT NULL DEFAULT '',
  vision_fallback_model TEXT NOT NULL DEFAULT '',
  source           TEXT NOT NULL DEFAULT '',
  channel_name     TEXT NOT NULL DEFAULT '',
  auth_index       TEXT NOT NULL DEFAULT '',
  failed           INTEGER NOT NULL DEFAULT 0,
  streaming        INTEGER NOT NULL DEFAULT 0,
  latency_ms       BIGINT NOT NULL DEFAULT 0,
  first_token_ms   BIGINT NOT NULL DEFAULT 0,
  input_tokens     BIGINT NOT NULL DEFAULT 0,
  output_tokens    BIGINT NOT NULL DEFAULT 0,
  reasoning_tokens BIGINT NOT NULL DEFAULT 0,
  cached_tokens    BIGINT NOT NULL DEFAULT 0,
  total_tokens     BIGINT NOT NULL DEFAULT 0,
  cost             DOUBLE PRECISION NOT NULL DEFAULT 0,
  input_content    TEXT NOT NULL DEFAULT '',
  output_content   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS request_log_content (
  log_id           BIGINT PRIMARY KEY REFERENCES request_logs(id) ON DELETE CASCADE,
  timestamp        TIMESTAMPTZ NOT NULL,
  compression      TEXT NOT NULL DEFAULT 'zstd',
  input_content    BYTEA NOT NULL DEFAULT decode('', 'hex'),
  output_content   BYTEA NOT NULL DEFAULT decode('', 'hex'),
  detail_content   BYTEA NOT NULL DEFAULT decode('', 'hex'),
  session_id       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON request_logs(api_key);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_timestamp ON request_logs(api_key, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_id ON request_logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_id_timestamp ON request_logs(api_key_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_chart_cover ON request_logs(api_key, api_key_id, timestamp DESC, model, failed, input_tokens, output_tokens, total_tokens, cost, cached_tokens);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_id_chart_cover ON request_logs(api_key_id, timestamp DESC, model, failed, input_tokens, output_tokens, total_tokens, cost, cached_tokens);
CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_logs_failed ON request_logs(failed);
CREATE INDEX IF NOT EXISTS idx_logs_auth_index ON request_logs(auth_index);
CREATE INDEX IF NOT EXISTS idx_logs_auth_subject_id ON request_logs(auth_subject_id);
CREATE INDEX IF NOT EXISTS idx_log_content_timestamp ON request_log_content(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_log_content_detail_timestamp ON request_log_content(timestamp DESC) WHERE length(detail_content) > 0;
CREATE INDEX IF NOT EXISTS idx_log_content_session_timestamp ON request_log_content(session_id, timestamp DESC) WHERE session_id <> '';

CREATE TABLE IF NOT EXISTS auth_file_quota_snapshots (
  date_key      TEXT NOT NULL,
  auth_index    TEXT NOT NULL,
  auth_subject_id TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  quota_key     TEXT NOT NULL,
  percent       DOUBLE PRECISION,
  recorded_at   TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (date_key, auth_index, quota_key)
);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_date ON auth_file_quota_snapshots(date_key);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_auth ON auth_file_quota_snapshots(auth_index);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_subject ON auth_file_quota_snapshots(auth_subject_id);

CREATE TABLE IF NOT EXISTS auth_file_quota_snapshot_points (
  id             BIGSERIAL PRIMARY KEY,
  recorded_at    TIMESTAMPTZ NOT NULL,
  auth_index     TEXT NOT NULL,
  auth_subject_id TEXT NOT NULL DEFAULT '',
  provider       TEXT NOT NULL DEFAULT '',
  quota_key      TEXT NOT NULL,
  quota_label    TEXT NOT NULL DEFAULT '',
  percent        DOUBLE PRECISION,
  reset_at       TIMESTAMPTZ,
  window_seconds BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_auth_time ON auth_file_quota_snapshot_points(auth_index, recorded_at);
CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_auth_key_time ON auth_file_quota_snapshot_points(auth_index, quota_key, recorded_at);
CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_subject_time ON auth_file_quota_snapshot_points(auth_subject_id, recorded_at);

CREATE TABLE IF NOT EXISTS auth_subject_quota_cycles (
  subject_id       TEXT NOT NULL,
  auth_index       TEXT NOT NULL DEFAULT '',
  provider         TEXT NOT NULL DEFAULT '',
  quota_key        TEXT NOT NULL,
  cycle_start_at   TIMESTAMPTZ NOT NULL,
  reset_at         TIMESTAMPTZ NOT NULL,
  window_seconds   BIGINT NOT NULL DEFAULT 0,
  last_verified_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (subject_id, quota_key)
);
CREATE INDEX IF NOT EXISTS idx_auth_subject_quota_cycles_subject_window
  ON auth_subject_quota_cycles(subject_id, window_seconds, last_verified_at);

CREATE TABLE IF NOT EXISTS model_pricing (
  model_id                      TEXT PRIMARY KEY,
  input_price_per_million        DOUBLE PRECISION NOT NULL DEFAULT 0,
  output_price_per_million       DOUBLE PRECISION NOT NULL DEFAULT 0,
  cached_price_per_million       DOUBLE PRECISION NOT NULL DEFAULT 0,
  cache_read_price_per_million   DOUBLE PRECISION NOT NULL DEFAULT 0,
  cache_write_price_per_million  DOUBLE PRECISION NOT NULL DEFAULT 0,
  updated_at                    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS api_key_permission_profiles (
  id                     TEXT PRIMARY KEY NOT NULL,
  name                   TEXT NOT NULL DEFAULT '',
  daily_limit            INTEGER NOT NULL DEFAULT 0,
  total_quota            INTEGER NOT NULL DEFAULT 0,
  concurrency_limit      INTEGER NOT NULL DEFAULT 0,
  rpm_limit              INTEGER NOT NULL DEFAULT 0,
  tpm_limit              INTEGER NOT NULL DEFAULT 0,
  allowed_models         TEXT NOT NULL DEFAULT '[]',
  allowed_channels       TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  system_prompt          TEXT NOT NULL DEFAULT '',
  created_at             TEXT NOT NULL DEFAULT '',
  updated_at             TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS api_keys (
  key               TEXT PRIMARY KEY NOT NULL,
  id                TEXT NOT NULL DEFAULT '',
  name              TEXT NOT NULL DEFAULT '',
  disabled          INTEGER NOT NULL DEFAULT 0,
  permission_profile_id TEXT NOT NULL DEFAULT '',
  daily_limit       INTEGER NOT NULL DEFAULT 0,
  total_quota       INTEGER NOT NULL DEFAULT 0,
  spending_limit    DOUBLE PRECISION NOT NULL DEFAULT 0,
  daily_spending_limit DOUBLE PRECISION NOT NULL DEFAULT 0,
  concurrency_limit INTEGER NOT NULL DEFAULT 0,
  rpm_limit         INTEGER NOT NULL DEFAULT 0,
  tpm_limit         INTEGER NOT NULL DEFAULT 0,
  allowed_models    TEXT NOT NULL DEFAULT '[]',
  allowed_channels  TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  system_prompt     TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL DEFAULT '',
  updated_at        TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_id ON api_keys(id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key ON api_keys(key);

CREATE TABLE IF NOT EXISTS model_configs (
  model_id                      TEXT PRIMARY KEY,
  owned_by                      TEXT NOT NULL DEFAULT '',
  description                   TEXT NOT NULL DEFAULT '',
  enabled                       INTEGER NOT NULL DEFAULT 1,
  input_modalities              TEXT NOT NULL DEFAULT '',
  output_modalities             TEXT NOT NULL DEFAULT '',
  pricing_mode                  TEXT NOT NULL DEFAULT 'token',
  input_price_per_million        DOUBLE PRECISION NOT NULL DEFAULT 0,
  output_price_per_million       DOUBLE PRECISION NOT NULL DEFAULT 0,
  cached_price_per_million       DOUBLE PRECISION NOT NULL DEFAULT 0,
  cache_read_price_per_million   DOUBLE PRECISION NOT NULL DEFAULT 0,
  cache_write_price_per_million  DOUBLE PRECISION NOT NULL DEFAULT 0,
  price_per_call                 DOUBLE PRECISION NOT NULL DEFAULT 0,
  source                        TEXT NOT NULL DEFAULT 'user',
  updated_at                    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_model_configs_owned_by ON model_configs(owned_by);

CREATE TABLE IF NOT EXISTS model_owner_presets (
  value       TEXT PRIMARY KEY,
  label       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  updated_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_group_model_owner_mappings (
  auth_group TEXT PRIMARY KEY,
  owner      TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_auth_group_model_owner_mappings_owner
  ON auth_group_model_owner_mappings(owner);

CREATE TABLE IF NOT EXISTS model_openrouter_sync_state (
  id               INTEGER PRIMARY KEY CHECK(id = 1),
  enabled          INTEGER NOT NULL DEFAULT 0,
  interval_minutes INTEGER NOT NULL DEFAULT 1440,
  last_sync_at     TEXT NOT NULL DEFAULT '',
  last_success_at  TEXT NOT NULL DEFAULT '',
  last_error       TEXT NOT NULL DEFAULT '',
  last_seen        INTEGER NOT NULL DEFAULT 0,
  last_added       INTEGER NOT NULL DEFAULT 0,
  last_updated     INTEGER NOT NULL DEFAULT 0,
  last_skipped     INTEGER NOT NULL DEFAULT 0,
  updated_at       TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_pool (
  id          TEXT PRIMARY KEY NOT NULL,
  name        TEXT NOT NULL DEFAULT '',
  url         TEXT NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS routing_config (
  id         INTEGER PRIMARY KEY NOT NULL CHECK (id = 1),
  payload    TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS runtime_settings (
  setting_key TEXT PRIMARY KEY NOT NULL,
  payload     TEXT NOT NULL DEFAULT '{}',
  updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS identity_fingerprints (
  provider          TEXT NOT NULL,
  account_key       TEXT NOT NULL,
  auth_subject_id   TEXT NOT NULL DEFAULT '',
  client_product    TEXT NOT NULL DEFAULT '',
  client_variant    TEXT NOT NULL DEFAULT '',
  version           TEXT NOT NULL DEFAULT '',
  fields_json       TEXT NOT NULL DEFAULT '{}',
  observed_headers_json TEXT NOT NULL DEFAULT '{}',
  created_at        TEXT NOT NULL DEFAULT '',
  updated_at        TEXT NOT NULL DEFAULT '',
  last_seen_at      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, account_key)
);
CREATE INDEX IF NOT EXISTS idx_identity_fingerprints_provider_seen
  ON identity_fingerprints(provider, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS ccswitch_import_configs (
  id                     TEXT PRIMARY KEY NOT NULL,
  client_type            TEXT NOT NULL,
  provider_name          TEXT NOT NULL DEFAULT '',
  note                   TEXT NOT NULL DEFAULT '',
  default_model          TEXT NOT NULL DEFAULT '',
  model_mappings         TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  route_path             TEXT NOT NULL DEFAULT '',
  endpoint_path          TEXT NOT NULL DEFAULT '',
  usage_auto_interval    INTEGER NOT NULL DEFAULT 30,
  api_key_field          TEXT NOT NULL DEFAULT '',
  created_at             TEXT NOT NULL DEFAULT '',
  updated_at             TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ccswitch_import_configs_route_path
  ON ccswitch_import_configs(route_path) WHERE route_path <> '';
`

const identityFingerprintProfilesSQL = `
ALTER TABLE identity_fingerprints
  ADD COLUMN IF NOT EXISTS profile_key TEXT NOT NULL DEFAULT 'default';

WITH identity_signals AS (
  SELECT provider, account_key,
         lower(client_variant) AS variant,
         lower(client_product) AS product,
         lower(fields_json) AS fields,
         (
           lower(client_variant) LIKE '%desktop%'
           OR lower(client_variant) IN ('codex_app', 'codex_chatgpt_desktop', 'codex_atlas')
           OR lower(client_product) IN ('codex_app', 'codex_chatgpt_desktop', 'codex_atlas')
           OR lower(fields_json) LIKE '%codex desktop%'
           OR lower(fields_json) LIKE '%codex_app%'
           OR lower(fields_json) LIKE '%codex_chatgpt_desktop%'
           OR lower(fields_json) LIKE '%codex_atlas%'
         ) AS desktop_signal,
         (
           lower(client_variant) IN ('codex_cli_rs', 'codex-tui', 'codex_tui', 'codex_exec', 'codex_vscode', 'codex_sdk_ts')
           OR lower(client_product) IN ('codex_cli_rs', 'codex-tui', 'codex_tui', 'codex_exec', 'codex_vscode', 'codex_sdk_ts')
           OR lower(fields_json) LIKE '%codex_cli_rs%'
           OR lower(fields_json) LIKE '%codex-tui%'
           OR lower(fields_json) LIKE '%codex_tui%'
           OR lower(fields_json) LIKE '%codex_exec%'
           OR lower(fields_json) LIKE '%codex_vscode%'
           OR lower(fields_json) LIKE '%codex_sdk_ts%'
         ) AS cli_signal
    FROM identity_fingerprints
)
UPDATE identity_fingerprints AS target
   SET profile_key = CASE
     WHEN target.provider <> 'codex' THEN 'default'
     -- Historical account-level rows can already contain CLI UA plus Desktop
     -- Originator (or the reverse). Quarantine them instead of blessing the
     -- mixed bundle as a selectable profile.
     WHEN signals.desktop_signal AND signals.cli_signal THEN 'codex_quarantined'
     WHEN signals.desktop_signal THEN
       CASE
         WHEN signals.variant IN ('codex_app', 'codex_chatgpt_desktop', 'codex_atlas') THEN signals.variant
         WHEN signals.product IN ('codex_app', 'codex_chatgpt_desktop', 'codex_atlas') THEN signals.product
         ELSE 'codex_desktop'
       END
     WHEN signals.cli_signal THEN
       CASE
         WHEN signals.variant IN ('codex_cli_rs', 'codex-tui', 'codex_exec', 'codex_vscode', 'codex_sdk_ts') THEN signals.variant
         WHEN signals.variant = 'codex_tui' THEN 'codex-tui'
         WHEN signals.product IN ('codex_cli_rs', 'codex-tui', 'codex_exec', 'codex_vscode', 'codex_sdk_ts') THEN signals.product
         WHEN signals.product = 'codex_tui' THEN 'codex-tui'
         WHEN signals.fields LIKE '%codex_cli_rs%' THEN 'codex_cli_rs'
         WHEN signals.fields LIKE '%codex-tui%' OR signals.fields LIKE '%codex_tui%' THEN 'codex-tui'
         WHEN signals.fields LIKE '%codex_exec%' THEN 'codex_exec'
         WHEN signals.fields LIKE '%codex_vscode%' THEN 'codex_vscode'
         WHEN signals.fields LIKE '%codex_sdk_ts%' THEN 'codex_sdk_ts'
         ELSE 'codex_unknown'
       END
     ELSE 'codex_unknown'
   END
  FROM identity_signals AS signals
 WHERE target.provider = signals.provider
   AND target.account_key = signals.account_key;

ALTER TABLE identity_fingerprints
  DROP CONSTRAINT IF EXISTS identity_fingerprints_pkey;
ALTER TABLE identity_fingerprints
  ADD PRIMARY KEY (provider, account_key, profile_key);

CREATE INDEX IF NOT EXISTS idx_identity_fingerprints_account_seen
  ON identity_fingerprints(provider, account_key, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS identity_fingerprint_account_policies (
  provider           TEXT NOT NULL,
  account_key        TEXT NOT NULL,
  strategy           TEXT NOT NULL DEFAULT 'cli_preferred',
  active_profile_key TEXT NOT NULL DEFAULT '',
  revision           BIGINT NOT NULL DEFAULT 1,
  updated_at         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, account_key),
  CONSTRAINT identity_fingerprint_account_policies_strategy_check
    CHECK (strategy IN ('cli_preferred', 'active_profile')),
  CONSTRAINT identity_fingerprint_account_policies_active_check
    CHECK (
      (strategy = 'active_profile' AND active_profile_key <> '')
      OR (strategy = 'cli_preferred' AND active_profile_key = '')
    )
);
`

const multiTenantIdentitySQL = `
CREATE TABLE IF NOT EXISTS tenants (
  id          UUID PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  type        TEXT NOT NULL CHECK (type IN ('system', 'standard')),
  status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'disabled')),
  expires_at  TIMESTAMPTZ,
  description TEXT NOT NULL DEFAULT '',
  created_by  UUID,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  version     BIGINT NOT NULL DEFAULT 1,
  CONSTRAINT tenants_standard_expiry CHECK (type = 'system' OR expires_at IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_single_system
  ON tenants(type) WHERE type = 'system';
INSERT INTO tenants (id, slug, name, type, status, created_at, updated_at)
VALUES ('00000000-0000-0000-0000-000000000001', 'system', 'System Administration', 'system', 'active', now(), now())
ON CONFLICT (id) DO NOTHING;
CREATE INDEX IF NOT EXISTS idx_tenants_status_expiry
  ON tenants(status, expires_at);

CREATE TABLE IF NOT EXISTS users (
  id                    UUID PRIMARY KEY,
  tenant_id             UUID NOT NULL REFERENCES tenants(id),
  username              TEXT NOT NULL,
  username_normalized   TEXT NOT NULL UNIQUE,
  display_name          TEXT NOT NULL,
  password_hash         TEXT NOT NULL,
  status                TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'locked')),
  must_change_password  BOOLEAN NOT NULL DEFAULT false,
  password_changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at         TIMESTAMPTZ,
  failed_login_count    INTEGER NOT NULL DEFAULT 0,
  locked_until          TIMESTAMPTZ,
  created_by            UUID REFERENCES users(id),
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  version               BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_users_tenant_status ON users(tenant_id, status);

CREATE TABLE IF NOT EXISTS roles (
  id               UUID PRIMARY KEY,
  tenant_id        UUID NOT NULL REFERENCES tenants(id),
  code             TEXT NOT NULL,
  name             TEXT NOT NULL,
  description      TEXT NOT NULL DEFAULT '',
  scope            TEXT NOT NULL CHECK (scope IN ('platform', 'tenant')),
  system_protected BOOLEAN NOT NULL DEFAULT false,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  version          BIGINT NOT NULL DEFAULT 1,
  UNIQUE (tenant_id, code),
  UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS permissions (
  code        TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  scope       TEXT NOT NULL CHECK (scope IN ('platform', 'tenant')),
  resource    TEXT NOT NULL,
  action      TEXT NOT NULL,
  sensitive   BOOLEAN NOT NULL DEFAULT false,
  sort_order  INTEGER NOT NULL DEFAULT 0,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS role_permissions (
  role_id         UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  permission_code TEXT NOT NULL REFERENCES permissions(code),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by      UUID REFERENCES users(id),
  PRIMARY KEY (role_id, permission_code)
);

CREATE TABLE IF NOT EXISTS user_roles (
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role_id    UUID NOT NULL REFERENCES roles(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by UUID REFERENCES users(id),
  PRIMARY KEY (user_id, role_id)
);

CREATE TABLE IF NOT EXISTS user_sessions (
  id              UUID PRIMARY KEY,
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tenant_id       UUID NOT NULL REFERENCES tenants(id),
  token_hash      TEXT NOT NULL UNIQUE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at      TIMESTAMPTZ NOT NULL,
  last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at      TIMESTAMPTZ,
  revoke_reason   TEXT NOT NULL DEFAULT '',
  user_agent_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_user_sessions_user_active
  ON user_sessions(user_id, revoked_at, expires_at);
CREATE INDEX IF NOT EXISTS idx_user_sessions_tenant_active
  ON user_sessions(tenant_id, revoked_at, expires_at);

CREATE TABLE IF NOT EXISTS audit_logs (
  id               BIGSERIAL PRIMARY KEY,
  tenant_id        UUID REFERENCES tenants(id),
  actor_kind       TEXT NOT NULL CHECK (actor_kind IN ('user_session', 'service_credential', 'system')),
  actor_user_id    UUID REFERENCES users(id),
  actor_session_id UUID REFERENCES user_sessions(id),
  action           TEXT NOT NULL,
  resource_type    TEXT NOT NULL,
  resource_id      TEXT NOT NULL DEFAULT '',
  result           TEXT NOT NULL CHECK (result IN ('success', 'denied', 'failed')),
  request_id       TEXT NOT NULL DEFAULT '',
  changes          JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_time ON audit_logs(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor_time ON audit_logs(actor_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action_time ON audit_logs(action, created_at DESC);
`

const multiTenantScopeSQL = `
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE api_key_permission_profiles ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE request_log_content ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE auth_file_quota_snapshots ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE auth_file_quota_snapshot_points ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE auth_subject_quota_cycles ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE model_pricing ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE model_owner_presets ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE auth_group_model_owner_mappings ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE model_openrouter_sync_state ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE proxy_pool ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE routing_config ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE runtime_settings ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE identity_fingerprints ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE identity_fingerprint_account_policies ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);
ALTER TABLE ccswitch_import_configs ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES tenants(id);

CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_timestamp ON request_logs(tenant_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_api_key_id_time ON request_logs(tenant_id, api_key_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_subject_time ON request_logs(tenant_id, auth_subject_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id);
CREATE INDEX IF NOT EXISTS idx_api_key_profiles_tenant ON api_key_permission_profiles(tenant_id);
CREATE INDEX IF NOT EXISTS idx_runtime_settings_tenant ON runtime_settings(tenant_id);
CREATE INDEX IF NOT EXISTS idx_proxy_pool_tenant ON proxy_pool(tenant_id);
CREATE INDEX IF NOT EXISTS idx_model_configs_tenant ON model_configs(tenant_id);
`

const multiTenantConstraintsSQL = `
ALTER TABLE api_key_permission_profiles DROP CONSTRAINT IF EXISTS api_key_permission_profiles_pkey;
ALTER TABLE api_key_permission_profiles ADD PRIMARY KEY (tenant_id, id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_request_logs_tenant_id ON request_logs(tenant_id, id);
ALTER TABLE request_log_content DROP CONSTRAINT IF EXISTS request_log_content_log_id_fkey;
ALTER TABLE request_log_content DROP CONSTRAINT IF EXISTS request_log_content_pkey;
ALTER TABLE request_log_content ADD PRIMARY KEY (tenant_id, log_id);
ALTER TABLE request_log_content ADD CONSTRAINT request_log_content_tenant_log_fkey
  FOREIGN KEY (tenant_id, log_id) REFERENCES request_logs(tenant_id, id) ON DELETE CASCADE;

ALTER TABLE auth_file_quota_snapshots DROP CONSTRAINT IF EXISTS auth_file_quota_snapshots_pkey;
ALTER TABLE auth_file_quota_snapshots ADD PRIMARY KEY (tenant_id, date_key, auth_index, quota_key);
ALTER TABLE auth_subject_quota_cycles DROP CONSTRAINT IF EXISTS auth_subject_quota_cycles_pkey;
ALTER TABLE auth_subject_quota_cycles ADD PRIMARY KEY (tenant_id, subject_id, quota_key);

ALTER TABLE model_pricing DROP CONSTRAINT IF EXISTS model_pricing_pkey;
ALTER TABLE model_pricing ADD PRIMARY KEY (tenant_id, model_id);
ALTER TABLE model_configs DROP CONSTRAINT IF EXISTS model_configs_pkey;
ALTER TABLE model_configs ADD PRIMARY KEY (tenant_id, model_id);
ALTER TABLE model_owner_presets DROP CONSTRAINT IF EXISTS model_owner_presets_pkey;
ALTER TABLE model_owner_presets ADD PRIMARY KEY (tenant_id, value);
ALTER TABLE auth_group_model_owner_mappings DROP CONSTRAINT IF EXISTS auth_group_model_owner_mappings_pkey;
ALTER TABLE auth_group_model_owner_mappings ADD PRIMARY KEY (tenant_id, auth_group);
ALTER TABLE model_openrouter_sync_state DROP CONSTRAINT IF EXISTS model_openrouter_sync_state_pkey;
ALTER TABLE model_openrouter_sync_state ADD PRIMARY KEY (tenant_id, id);
ALTER TABLE proxy_pool DROP CONSTRAINT IF EXISTS proxy_pool_pkey;
ALTER TABLE proxy_pool ADD PRIMARY KEY (tenant_id, id);
ALTER TABLE routing_config DROP CONSTRAINT IF EXISTS routing_config_pkey;
ALTER TABLE routing_config ADD PRIMARY KEY (tenant_id, id);
ALTER TABLE runtime_settings DROP CONSTRAINT IF EXISTS runtime_settings_pkey;
ALTER TABLE runtime_settings ADD PRIMARY KEY (tenant_id, setting_key);

ALTER TABLE identity_fingerprints DROP CONSTRAINT IF EXISTS identity_fingerprints_pkey;
ALTER TABLE identity_fingerprints ADD PRIMARY KEY (tenant_id, provider, account_key, profile_key);
ALTER TABLE identity_fingerprint_account_policies DROP CONSTRAINT IF EXISTS identity_fingerprint_account_policies_pkey;
ALTER TABLE identity_fingerprint_account_policies ADD PRIMARY KEY (tenant_id, provider, account_key);

DROP INDEX IF EXISTS idx_ccswitch_import_configs_route_path;
CREATE UNIQUE INDEX idx_ccswitch_import_configs_tenant_route_path
  ON ccswitch_import_configs(tenant_id, route_path) WHERE route_path <> '';

CREATE INDEX IF NOT EXISTS idx_request_log_content_tenant_time ON request_log_content(tenant_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_tenant_auth ON auth_file_quota_snapshots(tenant_id, auth_index);
CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_tenant_auth_time ON auth_file_quota_snapshot_points(tenant_id, auth_index, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_model_pricing_tenant ON model_pricing(tenant_id);
CREATE INDEX IF NOT EXISTS idx_model_owner_presets_tenant ON model_owner_presets(tenant_id);
CREATE INDEX IF NOT EXISTS idx_routing_config_tenant ON routing_config(tenant_id);
`

const identityDeleteConstraintsSQL = `
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_created_by_fkey;
ALTER TABLE users ADD CONSTRAINT users_created_by_fkey
  FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE role_permissions DROP CONSTRAINT IF EXISTS role_permissions_created_by_fkey;
ALTER TABLE role_permissions ADD CONSTRAINT role_permissions_created_by_fkey
  FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE user_roles DROP CONSTRAINT IF EXISTS user_roles_created_by_fkey;
ALTER TABLE user_roles ADD CONSTRAINT user_roles_created_by_fkey
  FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS audit_logs_actor_user_id_fkey;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_actor_user_id_fkey
  FOREIGN KEY (actor_user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS audit_logs_actor_session_id_fkey;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_actor_session_id_fkey
  FOREIGN KEY (actor_session_id) REFERENCES user_sessions(id) ON DELETE SET NULL;
`

const ccSwitchTenantPrimaryKeySQL = `
ALTER TABLE ccswitch_import_configs DROP CONSTRAINT IF EXISTS ccswitch_import_configs_pkey;
ALTER TABLE ccswitch_import_configs ADD PRIMARY KEY (tenant_id, id);
`

const dynamicMenusSQL = `
CREATE TABLE IF NOT EXISTS menus (
  code             TEXT PRIMARY KEY,
  parent_code      TEXT REFERENCES menus(code) ON DELETE RESTRICT,
  menu_type        TEXT NOT NULL CHECK (menu_type IN ('directory', 'menu')),
  path             TEXT NOT NULL DEFAULT '',
  label_key        TEXT NOT NULL,
  icon             TEXT NOT NULL DEFAULT '',
  permission_code  TEXT REFERENCES permissions(code) ON DELETE RESTRICT,
  sort_order       INTEGER NOT NULL DEFAULT 0,
  visible          BOOLEAN NOT NULL DEFAULT true,
  enabled          BOOLEAN NOT NULL DEFAULT true,
  system_protected BOOLEAN NOT NULL DEFAULT true,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  version          BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_menus_parent_sort ON menus(parent_code, sort_order, code);
CREATE INDEX IF NOT EXISTS idx_menus_permission ON menus(permission_code);
`

const menuManagementV2SQL = `
ALTER TABLE menus DROP CONSTRAINT IF EXISTS menus_menu_type_check;
ALTER TABLE menus ADD CONSTRAINT menus_menu_type_check CHECK (menu_type IN ('directory', 'menu', 'button', 'embed', 'link'));
ALTER TABLE menus ADD COLUMN IF NOT EXISTS component TEXT NOT NULL DEFAULT '';
ALTER TABLE menus ADD COLUMN IF NOT EXISTS link_url TEXT NOT NULL DEFAULT '';
ALTER TABLE menus ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
ALTER TABLE menus ADD COLUMN IF NOT EXISTS badge_type TEXT NOT NULL DEFAULT '';
ALTER TABLE menus ADD COLUMN IF NOT EXISTS badge_content TEXT NOT NULL DEFAULT '';
ALTER TABLE menus ADD COLUMN IF NOT EXISTS hide_menu BOOLEAN NOT NULL DEFAULT false;
`

const modelConfigOpenRouterMetadataSQL = `
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS context_length INTEGER NOT NULL DEFAULT 0;
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS max_completion_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS supported_parameters TEXT NOT NULL DEFAULT '';
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS reasoning TEXT NOT NULL DEFAULT '';
ALTER TABLE model_configs ADD COLUMN IF NOT EXISTS knowledge_cutoff TEXT NOT NULL DEFAULT '';
`

const aiAccountSharedSubjectsSQL = `
CREATE TABLE IF NOT EXISTS ai_account_subjects (
  auth_subject_id          TEXT PRIMARY KEY,
  provider                 TEXT NOT NULL,
  subject_scope            TEXT NOT NULL CHECK (subject_scope IN ('shared', 'tenant')),
  seed_kind                TEXT NOT NULL,
  seed_hash                TEXT NOT NULL,
  share_eligible           BOOLEAN NOT NULL DEFAULT false,
  usage_projected_since    TIMESTAMPTZ,
  usage_history_complete   BOOLEAN NOT NULL DEFAULT false,
  created_at               TIMESTAMPTZ NOT NULL,
  updated_at               TIMESTAMPTZ NOT NULL,
  UNIQUE (provider, subject_scope, seed_kind, seed_hash)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subjects_provider_scope
  ON ai_account_subjects(provider, subject_scope, updated_at);

CREATE TABLE IF NOT EXISTS ai_account_tenant_bindings (
  tenant_id                UUID NOT NULL,
  auth_id                  TEXT NOT NULL,
  auth_index               TEXT NOT NULL,
  provider                 TEXT NOT NULL,
  auth_subject_id          TEXT NOT NULL,
  binding_seed_kind        TEXT NOT NULL,
  binding_seed_hash        TEXT NOT NULL,
  share_eligible           BOOLEAN NOT NULL DEFAULT false,
  binding_state            TEXT NOT NULL DEFAULT 'active'
                           CHECK (binding_state IN ('active', 'deleted')),
  binding_revision         BIGINT NOT NULL DEFAULT 1,
  bound_at                 TIMESTAMPTZ NOT NULL,
  last_seen_at             TIMESTAMPTZ NOT NULL,
  deleted_at               TIMESTAMPTZ,
  PRIMARY KEY (tenant_id, auth_id),
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_account_binding_active_index
  ON ai_account_tenant_bindings(tenant_id, auth_index)
  WHERE binding_state = 'active';
CREATE INDEX IF NOT EXISTS idx_ai_account_binding_subject
  ON ai_account_tenant_bindings(auth_subject_id, binding_state);
CREATE INDEX IF NOT EXISTS idx_ai_account_binding_tenant_subject
  ON ai_account_tenant_bindings(tenant_id, auth_subject_id, binding_state);

CREATE TABLE IF NOT EXISTS ai_account_subject_status (
  auth_subject_id           TEXT PRIMARY KEY,
  provider                  TEXT NOT NULL,
  last_probe_state          TEXT NOT NULL DEFAULT 'idle'
                            CHECK (last_probe_state IN ('idle', 'success', 'error')),
  health_status             TEXT NOT NULL DEFAULT '',
  plan_type                 TEXT NOT NULL DEFAULT '',
  subscription_started_at   TIMESTAMPTZ,
  subscription_expires_at   TIMESTAMPTZ,
  subscription_source       TEXT NOT NULL DEFAULT ''
                            CHECK (subscription_source IN ('', 'probe', 'signed_claims', 'migration')),
  restriction_summary       TEXT NOT NULL DEFAULT '',
  error_code                TEXT NOT NULL DEFAULT '',
  error_summary             TEXT NOT NULL DEFAULT '',
  quota_json                TEXT NOT NULL DEFAULT '[]',
  reset_credit_count        BIGINT,
  reset_credit_expirations  TEXT NOT NULL DEFAULT '[]',
  upstream_checked_at       TIMESTAMPTZ,
  version                   BIGINT NOT NULL DEFAULT 1,
  updated_at                TIMESTAMPTZ NOT NULL,
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subject_status_checked
  ON ai_account_subject_status(upstream_checked_at, updated_at);

CREATE TABLE IF NOT EXISTS ai_account_subject_usage_buckets (
  auth_subject_id   TEXT NOT NULL,
  bucket_kind       TEXT NOT NULL CHECK (bucket_kind IN ('day', 'lifetime', 'cycle')),
  bucket_start      TEXT NOT NULL,
  request_count     BIGINT NOT NULL DEFAULT 0,
  success_count     BIGINT NOT NULL DEFAULT 0,
  failure_count     BIGINT NOT NULL DEFAULT 0,
  cost_total        DOUBLE PRECISION NOT NULL DEFAULT 0,
  first_event_at    TIMESTAMPTZ NOT NULL,
  updated_at        TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (auth_subject_id, bucket_kind, bucket_start)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subject_usage_day
  ON ai_account_subject_usage_buckets(bucket_kind, bucket_start, auth_subject_id);

CREATE TABLE IF NOT EXISTS ai_account_subject_quota_cycles (
  auth_subject_id    TEXT NOT NULL,
  provider           TEXT NOT NULL,
  quota_key          TEXT NOT NULL,
  cycle_start_at     TIMESTAMPTZ NOT NULL,
  reset_at           TIMESTAMPTZ NOT NULL,
  window_seconds     BIGINT NOT NULL DEFAULT 0,
  last_verified_at   TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (auth_subject_id, quota_key),
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);

CREATE TABLE IF NOT EXISTS ai_account_subject_quota_points (
  id                 BIGSERIAL PRIMARY KEY,
  auth_subject_id    TEXT NOT NULL,
  provider           TEXT NOT NULL,
  quota_key          TEXT NOT NULL,
  quota_label        TEXT NOT NULL DEFAULT '',
  percent            DOUBLE PRECISION,
  reset_at           TIMESTAMPTZ,
  window_seconds     BIGINT NOT NULL DEFAULT 0,
  recorded_at        TIMESTAMPTZ NOT NULL,
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subject_quota_points_key_time
  ON ai_account_subject_quota_points(auth_subject_id, quota_key, recorded_at DESC);
`
