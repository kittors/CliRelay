package postgres

// requestLogCacheAndSessionSQL stores each request's cache reads, cache writes
// and prompt size apart, and the client session it belongs to.
//
// request_logs.cached_tokens holds cache reads, or cache writes when a request
// read nothing, so a turn that only wrote the cache counted as a hit. The new
// cache columns stay NULL on the rows written before them: whether their cached
// tokens were reads or writes cannot be recovered, and hit rates leave those
// rows out instead of guessing. The rollup columns start at zero for the same
// reason, which reads as "unknown" there.
//
// session_id used to exist only on request_log_content, which keeps fewer days,
// skips bodies over the size cap and is emptied by "clear details". Existing
// rows get it from there in a background pass
// (usage.ScheduleRequestLogSessionBackfill), not here.
//
// On PostgreSQL 11 and later none of this rewrites a table: the columns are
// nullable or have constant defaults, so each ALTER only updates the catalog and
// holds its lock for a moment.
const requestLogCacheAndSessionSQL = `
ALTER TABLE request_logs
  ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT,
  ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT,
  ADD COLUMN IF NOT EXISTS prompt_tokens BIGINT,
  ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_rollup_buckets
  ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cache_known_prompt_tokens BIGINT NOT NULL DEFAULT 0;
`
