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

// requestLogClientSQL records who sent a request: the client named from its
// headers, the version it reported and its raw User-Agent (see
// internal/usage/request_log_client.go). Existing rows keep the empty defaults,
// which read as "unknown client". Constant defaults make this a catalog change,
// like the migrations above. No index, for the same reason as guard_kind: the
// filter and the facet read a window the (tenant_id, timestamp) index narrows.
const requestLogClientSQL = `
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS client_name TEXT NOT NULL DEFAULT '',
ADD COLUMN IF NOT EXISTS client_version TEXT NOT NULL DEFAULT '',
ADD COLUMN IF NOT EXISTS user_agent TEXT NOT NULL DEFAULT '';
`

// requestLogCacheWrite1hSQL keeps how many of a request's cache writes went to
// Anthropic's one-hour cache, which is billed at a higher rate than the
// five-minute one. With it a stored row holds every input the cost function
// reads, so a later price change can recompute the row exactly (see
// internal/usage/pricing_resync.go). NULL where cache use was not reported and
// on the rows written before it; those rows are not recomputed when they have
// cache writes. Nullable, so this is a catalog change.
const requestLogCacheWrite1hSQL = `
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS cache_write_1h_tokens BIGINT;
`
