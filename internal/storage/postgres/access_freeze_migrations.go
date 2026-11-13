package postgres

// accessFreezesSQL holds requesters back from a provider after its upstream
// refused their requests for their content (internal/accessfreeze).
//
// access_freeze_strikes is every refusal, the evidence a freeze rests on and
// what the threshold counts. access_freezes is the freezes themselves; a
// released one stays as history. access_freeze_settings is each tenant's
// threshold; a tenant without a row freezes on the first refusal.
const accessFreezesSQL = `
CREATE TABLE IF NOT EXISTS access_freeze_strikes (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         UUID NOT NULL,
  subject_kind      TEXT NOT NULL CHECK (subject_kind IN ('end_user', 'api_key')),
  subject_id        TEXT NOT NULL,
  provider          TEXT NOT NULL,
  reason_code       TEXT NOT NULL,
  upstream_message  TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  auth_label        TEXT NOT NULL DEFAULT '',
  request_id        TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_access_freeze_strikes_subject
  ON access_freeze_strikes(tenant_id, subject_kind, subject_id, provider, created_at);

CREATE TABLE IF NOT EXISTS access_freezes (
  id                BIGSERIAL PRIMARY KEY,
  tenant_id         UUID NOT NULL,
  subject_kind      TEXT NOT NULL CHECK (subject_kind IN ('end_user', 'api_key')),
  subject_id        TEXT NOT NULL,
  provider          TEXT NOT NULL,
  reason_code       TEXT NOT NULL,
  upstream_message  TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  auth_label        TEXT NOT NULL DEFAULT '',
  request_id        TEXT NOT NULL DEFAULT '',
  strike_count      INTEGER NOT NULL DEFAULT 1,
  frozen_at         TIMESTAMPTZ NOT NULL,
  released_at       TIMESTAMPTZ,
  released_by       TEXT NOT NULL DEFAULT '',
  release_note      TEXT NOT NULL DEFAULT ''
);
-- One standing freeze per requester and provider, so refusals of two requests
-- in flight at once cannot freeze the same requester twice.
CREATE UNIQUE INDEX IF NOT EXISTS uq_access_freezes_active
  ON access_freezes(tenant_id, subject_kind, subject_id, provider) WHERE released_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_access_freezes_tenant_frozen_at
  ON access_freezes(tenant_id, frozen_at DESC);

CREATE TABLE IF NOT EXISTS access_freeze_settings (
  tenant_id         UUID PRIMARY KEY,
  threshold         INTEGER NOT NULL CHECK (threshold >= 0 AND threshold <= 100),
  updated_at        TIMESTAMPTZ NOT NULL,
  updated_by        TEXT NOT NULL DEFAULT ''
);
`
