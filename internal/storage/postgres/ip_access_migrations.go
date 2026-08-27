package postgres

// laterRuntimeMigrations carries migrations added after migrations.go reached
// its size ceiling. RuntimeMigrations appends this list, so new schema changes
// land here instead of growing a file that the structure gate only lets shrink.
// Order still matters: entries run after every migration in the main list.
func laterRuntimeMigrations() []Migration {
	return []Migration{
		// Operator-managed IP allow/deny list, authentication attempt log, and
		// source address on audit rows.
		{Version: "202608100001_ip_access_control", SQL: ipAccessControlSQL},
		// Drop model/channel scopes stranded on end users by an unbound permission
		// profile. See endUserUnboundProfileScopeCleanupSQL.
		{Version: "202608270001_end_user_unbound_profile_scope_cleanup", SQL: endUserUnboundProfileScopeCleanupSQL},
	}
}

// endUserUnboundProfileScopeCleanupSQL clears model/channel scopes left on end
// users that have no permission profile.
//
// Binding a profile copied its allowed-models / allowed-channels /
// allowed-channel-groups onto the account row; unbinding cleared the quota
// fields and left those three behind. The console renders them only while a
// profile is attached, so the account showed "unrestricted" while a stale
// channel-group whitelist kept every credential outside that group unreachable
// — invisible in the UI, and with no way to clear it from there either.
//
// The unbind path now clears them, but that only helps the next unbind: an
// account already in this state can no longer reach the code that would fix it.
// Hence this one-off pass.
//
// Scope is deliberately narrow: only rows with no profile, and only the fields
// a profile owns. A tenant that set these through the API without a profile is
// outside what the console can express, and would have been equally invisible
// there; the trade is a visible empty list over a silent unreachable one.
const endUserUnboundProfileScopeCleanupSQL = `
UPDATE end_users
SET allowed_models         = '[]',
    allowed_channels       = '[]',
    allowed_channel_groups = '[]',
    system_prompt          = '',
    updated_at             = CURRENT_TIMESTAMP
WHERE COALESCE(TRIM(permission_profile_id), '') = ''
  AND (
        COALESCE(allowed_models, '[]')         NOT IN ('[]', '')
     OR COALESCE(allowed_channels, '[]')       NOT IN ('[]', '')
     OR COALESCE(allowed_channel_groups, '[]') NOT IN ('[]', '')
     OR COALESCE(system_prompt, '')            <> ''
      );
`

// ipAccessControlSQL adds the operator-managed IP allow/deny list, the
// authentication attempt log that makes an attack source identifiable, and the
// source address on audit rows.
//
// Attempts live in their own table rather than in audit_logs because they are
// high-volume, low-value-per-row telemetry with a short retention: mixing them
// into audit_logs would bury the deliberate, compliance-relevant records under
// failed-login noise, and audit_logs' actor_kind/result CHECK constraints have
// no vocabulary for "an anonymous source failed a guess".
const ipAccessControlSQL = `
-- 1) allow / deny rules -----------------------------------------------------
-- allow and deny share one table because every lookup consults both and must
-- resolve their precedence in one place; splitting them only creates two
-- queries and a window where the two halves disagree.
CREATE TABLE IF NOT EXISTS ip_access_rules (
  id           UUID PRIMARY KEY,
  cidr         TEXT NOT NULL,
  family       SMALLINT NOT NULL,
  effect       TEXT NOT NULL CHECK (effect IN ('allow', 'deny')),
  source       TEXT NOT NULL CHECK (source IN ('manual', 'auto')),
  reason       TEXT NOT NULL DEFAULT '',
  note         TEXT NOT NULL DEFAULT '',
  enabled      BOOLEAN NOT NULL DEFAULT true,
  expires_at   TIMESTAMPTZ,
  created_by   UUID REFERENCES users(id),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  hit_count    BIGINT NOT NULL DEFAULT 0,
  last_hit_at  TIMESTAMPTZ
);

-- CIDRs are normalised (host bits cleared) before insert, so this constraint
-- makes "1.2.3.0/24" and "1.2.3.5/24" collide as the one network they describe.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ip_access_rules_effect_cidr
  ON ip_access_rules(effect, cidr);
CREATE INDEX IF NOT EXISTS idx_ip_access_rules_active
  ON ip_access_rules(enabled, expires_at);

-- 2) authentication attempts ------------------------------------------------
CREATE TABLE IF NOT EXISTS auth_attempt_events (
  id           BIGSERIAL PRIMARY KEY,
  occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  ip           TEXT NOT NULL DEFAULT '',
  -- ip_prefix is the throttle bucket key (/32 or /64) so aggregation here lines
  -- up with what the rate limiter actually counted.
  ip_prefix    TEXT NOT NULL DEFAULT '',
  -- trusted records whether the address identified a client at all; without it
  -- a report built from proxy addresses would read as a single mega-attacker.
  trusted      BOOLEAN NOT NULL DEFAULT false,
  scope        TEXT NOT NULL DEFAULT '',
  surface      TEXT NOT NULL DEFAULT '',
  username     TEXT NOT NULL DEFAULT '',
  outcome      TEXT NOT NULL,
  reason       TEXT NOT NULL DEFAULT '',
  user_agent   TEXT NOT NULL DEFAULT '',
  request_path TEXT NOT NULL DEFAULT '',
  request_id   TEXT NOT NULL DEFAULT '',
  tenant_id    UUID
);

CREATE INDEX IF NOT EXISTS idx_auth_attempts_time
  ON auth_attempt_events(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_auth_attempts_prefix_time
  ON auth_attempt_events(ip_prefix, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_auth_attempts_username_time
  ON auth_attempt_events(username, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_auth_attempts_outcome_time
  ON auth_attempt_events(outcome, occurred_at DESC);

-- 3) audit rows carry their source address ----------------------------------
-- Every audit event gains provenance, not just logins: "who changed this" is
-- only half an answer without "from where".
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS ip_address TEXT NOT NULL DEFAULT '';
`
