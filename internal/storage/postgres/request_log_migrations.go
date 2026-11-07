package postgres

const requestLogsAuthLookupIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_index_time
  ON request_logs(tenant_id, auth_index, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_subject_time_cost
  ON request_logs(tenant_id, auth_subject_id, timestamp DESC)
  INCLUDE (cost);
`

const requestLogThinkingLevelSQL = `
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS thinking_level TEXT NOT NULL DEFAULT '';
`

// requestLogUpstreamResponseModelSQL records the model an upstream declared in
// its own response. Existing rows keep the empty default, which reads as "the
// upstream never declared one" and is therefore never audited as a mismatch.
const requestLogUpstreamResponseModelSQL = `
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS upstream_response_model TEXT NOT NULL DEFAULT '';
`

// requestLogGuardKindSQL records which loop protection check ended a request
// ("reply_loop", "tool_loop"). Existing rows keep the empty default, which reads
// as "loop protection did not end this request". A constant default is a
// catalog change on PostgreSQL 11 and later, the same shape as the migration
// above. No index: the filter and the monitor count read a time window that the
// (tenant_id, timestamp) index already narrows, like the failed-request views.
const requestLogGuardKindSQL = `
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS guard_kind TEXT NOT NULL DEFAULT '';
`
