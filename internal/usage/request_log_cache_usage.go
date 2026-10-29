package usage

import (
	"database/sql"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Cache reads, cache writes and hit rates.
//
// request_logs.cached_tokens holds cache reads, or cache writes when a request
// read nothing (see coreusage.Detail.CachedTokens). A turn that only wrote the
// cache, the first turn of every Claude conversation, therefore counted as a
// near-100% hit, and a turn that did both lost its writes. The column keeps that
// meaning for the clients that still show it, and so that a rollback to an
// older build reads what it expects.
//
// Reads and writes are now stored apart, in nullable columns: NULL means the
// request's cache use is unknown, either because it was logged before these
// columns existed (what cached_tokens held then cannot be told apart) or
// because its upstream reports no cache accounting at all. Hit rates are cache
// reads over the prompt size of the requests whose cache use is known; unknown
// requests are left out rather than counted as misses.

// CacheUsage is the cache side of an aggregate. Its fields sit at the top level
// of LogStats, DashboardKPI and MonitorTotals.
type CacheUsage struct {
	// CacheReadTokens and CacheWriteTokens sum the requests whose cache use is
	// known.
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// CacheHitRate is cache reads as a percentage (0-100) of the prompt tokens
	// of those requests, and null when none of the requests in the aggregate
	// has a known cache use.
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

// legacyCacheRate is what the deprecated cache_rate field reports: the hit
// rate, or 0 where it is unknown, because older panels read it as a number.
func (c CacheUsage) legacyCacheRate() float64 {
	if c.CacheHitRate == nil {
		return 0
	}
	return *c.CacheHitRate
}

// cacheTotals accumulates the cache columns of request_logs or
// usage_rollup_buckets.
type cacheTotals struct {
	ReadTokens  int64
	WriteTokens int64
	// KnownPromptTokens sums the prompt size of the requests whose cache use is
	// known; it is the denominator of the hit rate.
	KnownPromptTokens int64
}

func (c *cacheTotals) add(o cacheTotals) {
	c.ReadTokens += o.ReadTokens
	c.WriteTokens += o.WriteTokens
	c.KnownPromptTokens += o.KnownPromptTokens
}

func (c cacheTotals) usage() CacheUsage {
	out := CacheUsage{CacheReadTokens: c.ReadTokens, CacheWriteTokens: c.WriteTokens}
	if c.KnownPromptTokens > 0 {
		rate := float64(c.ReadTokens) / float64(c.KnownPromptTokens) * 100
		if rate > 100 {
			rate = 100
		}
		out.CacheHitRate = &rate
	}
	return out
}

// requestCacheTotals is one request's contribution to the cache aggregates.
func requestCacheTotals(tokens TokenStats) cacheTotals {
	if !tokens.CacheReported {
		return cacheTotals{}
	}
	return cacheTotals{
		ReadTokens:        tokens.CacheReadTokens,
		WriteTokens:       tokens.CacheWriteTokens,
		KnownPromptTokens: promptTokenTotal(tokens),
	}
}

// promptTokenTotal is the size of the prompt the model processed: the uncached
// input plus what was read from and written to the cache.
//
// Upstreams disagree on what their input count holds. OpenAI, Codex and Gemini
// count cache reads inside it, and so does the Ollama estimate
// (CacheReadIncludedInInput); Anthropic counts neither reads nor writes. No
// upstream we parse counts cache writes inside its input figure.
func promptTokenTotal(tokens TokenStats) int64 {
	prompt := max(tokens.InputTokens, 0)
	if tokens.CacheReadIncludedInInput {
		// An upstream claiming to have read more than the whole prompt: trust
		// the read count, as billing does.
		prompt = max(prompt, tokens.CacheReadTokens)
	} else {
		prompt += max(tokens.CacheReadTokens, 0)
	}
	return prompt + max(tokens.CacheWriteTokens, 0)
}

// requestLogCacheValues returns what request_logs stores for one request: the
// cache reads and writes, NULL when the upstream did not report them, and the
// prompt size.
func requestLogCacheValues(tokens TokenStats) (read, write sql.NullInt64, prompt int64) {
	if tokens.CacheReported {
		read = sql.NullInt64{Int64: tokens.CacheReadTokens, Valid: true}
		write = sql.NullInt64{Int64: tokens.CacheWriteTokens, Valid: true}
	}
	return read, write, promptTokenTotal(tokens)
}

// cacheTotalsFromStoredRow rebuilds a request's contribution from its
// request_logs columns. read is NULL on rows whose cache use is unknown.
func cacheTotalsFromStoredRow(read, write, prompt sql.NullInt64) cacheTotals {
	if !read.Valid {
		return cacheTotals{}
	}
	return cacheTotals{ReadTokens: read.Int64, WriteTokens: write.Int64, KnownPromptTokens: prompt.Int64}
}

// requestLogCacheSumsSQL aggregates the cache columns of request_logs, in the
// order of cacheTotals.
const requestLogCacheSumsSQL = `
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(CASE WHEN cache_read_tokens IS NULL THEN 0 ELSE COALESCE(prompt_tokens, 0) END), 0)`

// rollupCacheSumsSQL aggregates the cache columns of usage_rollup_buckets, in
// the order of cacheTotals.
const rollupCacheSumsSQL = `
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(cache_known_prompt_tokens), 0)`

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func (c *cacheTotals) scanTargets() []any {
	return []any{&c.ReadTokens, &c.WriteTokens, &c.KnownPromptTokens}
}

// migrateRequestLogCacheColumns adds the cache and session columns to a SQLite
// request_logs table. PostgreSQL gets them from the runtime migration
// 202610300001_request_log_cache_and_session.
func migrateRequestLogCacheColumns(db *sql.DB) {
	addSQLiteColumns(db, "request_logs", []string{
		"cache_read_tokens INTEGER",
		"cache_write_tokens INTEGER",
		"prompt_tokens INTEGER",
		"session_id TEXT NOT NULL DEFAULT ''",
	})
}

// migrateUsageRollupCacheColumns adds the cache columns to a SQLite
// usage_rollup_buckets table. Buckets filled before them keep zeros there,
// which reads as "cache use unknown" (see cacheTotals.usage). PostgreSQL gets
// them from the runtime migration, and is skipped here: an ALTER TABLE on a
// column that exists still queues for an exclusive lock on a table every
// request writes to.
func migrateUsageRollupCacheColumns(db *sql.DB) {
	if usageDriver == "postgres" {
		return
	}
	addSQLiteColumns(db, "usage_rollup_buckets", []string{
		"cache_read_tokens INTEGER NOT NULL DEFAULT 0",
		"cache_write_tokens INTEGER NOT NULL DEFAULT 0",
		"cache_known_prompt_tokens INTEGER NOT NULL DEFAULT 0",
	})
}

func addSQLiteColumns(db *sql.DB, table string, columns []string) {
	if db == nil {
		return
	}
	for _, column := range columns {
		_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column)
		if err != nil && !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: add column %s.%s: %v", table, column, err)
		}
	}
}
