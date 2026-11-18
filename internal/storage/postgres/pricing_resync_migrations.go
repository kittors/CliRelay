package postgres

// pricingResyncJobsSQL is the queue of request-cost recomputations after a model
// price changed (see internal/usage/pricing_resync.go). A job names the tenant
// whose catalog changed (the system tenant stands for every tenant inheriting
// it), the catalog IDs whose price changed and the mode: "fill" recomputes only
// requests stored without a cost, "reprice" recomputes every request of the last
// seven days. The leader runs the jobs in batches and keeps its cursor here, so a
// deploy or a leader change resumes a job instead of restarting it.
const pricingResyncJobsSQL = `
CREATE TABLE IF NOT EXISTS pricing_resync_jobs (
  id          BIGSERIAL PRIMARY KEY,
  tenant_id   UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  model_ids   TEXT NOT NULL DEFAULT '',
  mode        TEXT NOT NULL DEFAULT 'fill',
  origin      TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'pending',
  not_before  TIMESTAMPTZ NOT NULL,
  upper_time  TIMESTAMPTZ NOT NULL,
  cursor_id   BIGINT NOT NULL DEFAULT 0,
  scanned     BIGINT NOT NULL DEFAULT 0,
  changed     BIGINT NOT NULL DEFAULT 0,
  skipped     BIGINT NOT NULL DEFAULT 0,
  cost_delta  DOUBLE PRECISION NOT NULL DEFAULT 0,
  last_error  TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL,
  finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_pricing_resync_jobs_due
  ON pricing_resync_jobs(status, not_before);
CREATE INDEX IF NOT EXISTS idx_pricing_resync_jobs_tenant
  ON pricing_resync_jobs(tenant_id, id DESC);
`
