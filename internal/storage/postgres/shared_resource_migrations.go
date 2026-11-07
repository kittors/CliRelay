package postgres

// platformResourceGrantsSQL records which tenants may use a resource the
// system tenant owns: a proxy of its proxy pool or one of its AI account
// credentials. See internal/storage/sqlstore/resourcegrants for the model.
//
// tenant_id is a tenant UUID or '*' for every tenant, so it carries no foreign
// key. Tenants are only ever soft-deleted (status = disabled), and a disabled
// tenant cannot authenticate, so a grant to one is inert rather than dangling.
// resource_id has no foreign key either: proxies are keyed per tenant and
// credentials live outside this schema in file mode. Proxy grants are removed
// together with the proxy instead (proxypool.Store.ReplaceExpect).
//
// created_at is TEXT, like the other sqlstore-written tables, because the
// store writes the same RFC 3339 strings on SQLite and PostgreSQL.
const platformResourceGrantsSQL = `
CREATE TABLE IF NOT EXISTS platform_resource_grants (
  resource_kind TEXT NOT NULL CHECK (resource_kind IN ('proxy', 'auth')),
  resource_id   TEXT NOT NULL,
  tenant_id     TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (resource_kind, resource_id, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_platform_resource_grants_tenant
  ON platform_resource_grants (tenant_id, resource_kind);
`

// ccSwitchCodexContextWindowSQL stores the context window a Codex import
// preset sets for its default model. It is a new column rather than a reuse
// of the per-mapping context windows: earlier panels wrote a 128000
// placeholder into every mapping, which CC Switch never applied, and reading
// those as intent would cap Codex at 128k on every re-import.
const ccSwitchCodexContextWindowSQL = `
ALTER TABLE ccswitch_import_configs ADD COLUMN IF NOT EXISTS codex_context_window INTEGER NOT NULL DEFAULT 0;
`
