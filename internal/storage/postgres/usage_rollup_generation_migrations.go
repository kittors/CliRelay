package postgres

// usageRollupGenerationSpeedSQL adds what the monitor's generation speed is
// computed from: per bucket, how many requests measured it, and their summed
// output tokens, time after the first token and per-request speed.
//
// A request measures generation speed when it is a successful stream with a
// recorded first token, at least 500 ms of generation and at least 50 output
// tokens (usage.requestGenerationTotals). Buckets written before these columns
// keep zeros, which reads as "no sample": the monitor shows no speed for them
// instead of one computed another way.
//
// Old buckets are not backfilled from request_logs. A bucket's end_user_id is
// the key owner resolved when the request was written, which request_logs does
// not keep, so a backfill could not always find the bucket a request went to;
// and the detail rows it would read are the same ones the monitor already
// takes its generation-speed percentiles from.
//
// Constant defaults on PostgreSQL 11 and later only update the catalog, so the
// ALTER holds its lock on this table, which every request writes to, for a
// moment rather than for a rewrite.
const usageRollupGenerationSpeedSQL = `
ALTER TABLE usage_rollup_buckets
  ADD COLUMN IF NOT EXISTS generation_count BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS generation_output_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS generation_sum_ms BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS generation_speed_sum DOUBLE PRECISION NOT NULL DEFAULT 0;
`
