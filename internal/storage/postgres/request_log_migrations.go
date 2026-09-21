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
