package postgres

// authCredentialsSQL creates the table cluster mode keeps credentials in.
//
// content is the credential document exactly as the auth file holds it, minus
// the runtime keys (health, quota gates, probe back-off), which live in
// runtime so request bookkeeping never bumps version. version increases on
// every credential write and on deletion; writers compare-and-set on it.
// Deletions leave a tombstone (deleted_at, cleared content) so a late write
// from a node that has not caught up cannot bring the credential back.
// refresh_owner/refresh_until form the lease that lets one node at a time
// spend a refresh token. auth_index is pinned at creation because request
// logs, quota snapshots and account bindings are keyed by it.
//
// Additive only: nothing reads this table unless cluster.enabled is set.
const authCredentialsSQL = `
CREATE TABLE IF NOT EXISTS auth_credentials (
  id            TEXT PRIMARY KEY,
  tenant_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  file_name     TEXT NOT NULL DEFAULT '',
  auth_index    TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  content       JSONB NOT NULL DEFAULT '{}'::jsonb,
  runtime       JSONB NOT NULL DEFAULT '{}'::jsonb,
  version       BIGINT NOT NULL DEFAULT 1,
  refresh_owner TEXT,
  refresh_until TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by    TEXT NOT NULL DEFAULT '',
  deleted_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_auth_credentials_tenant_live
  ON auth_credentials(tenant_id) WHERE deleted_at IS NULL;
`
