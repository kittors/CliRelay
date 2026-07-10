package postgres

func RuntimeMigrations() []Migration {
	return []Migration{
		{Version: "202607050001_runtime_schema", SQL: runtimeSchemaSQL},
	}
}

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
