package postgres

// configOptimisticVersionsSQL adds the optimistic-concurrency versions that
// keep management writes from overwriting each other across cluster nodes.
//
// runtime_settings and routing_config get a per-row version that every write
// increments and a compare-and-swap checks. Existing rows start at 1, which is
// also what a new row gets, so "version 0" always means "row does not exist".
//
// config_versions holds one counter per (tenant, domain) for collections the
// management API replaces as a whole (api keys, permission profiles, proxy
// pool, model owner presets, cc-switch import configs). Every write to such a
// collection bumps its counter first, which both serialises concurrent full
// replacements and lets a client that read version N detect a later write.
//
// Additive only: older binaries keep writing these tables without touching
// the new columns, which merely makes their writes invisible to the checks
// during a rolling upgrade.
const configOptimisticVersionsSQL = `
ALTER TABLE runtime_settings ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE routing_config ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS config_versions (
  tenant_id  TEXT NOT NULL,
  domain     TEXT NOT NULL,
  version    BIGINT NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, domain)
);
`
