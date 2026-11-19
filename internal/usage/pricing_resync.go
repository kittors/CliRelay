package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// How a pricing resync job runs (see pricing_resync_store.go for the queue).
//
// A job names catalog IDs whose price changed. The request models it touches
// are the ones whose price lookup passes through those IDs
// (modelPricingLookupModelIDs): a price on "gpt-6.2" also prices
// "gpt-6.2-sol" when nothing closer does. The job walks request_logs of the
// last seven days by id, 200 rows at a time, recomputes each row with the same
// CalculateCostV2ForTenant the request was logged with, and applies only the
// rows whose cost really changes. Requests are never re-counted: only cost
// moves, in request_logs and in every total built from it (see
// pricing_resync_apply.go), and the job's progress commits with each batch.

const (
	pricingResyncBatchSize = 200
	pricingResyncPause     = 250 * time.Millisecond
	pricingResyncMaxPause  = 4 * time.Second
	// A batch slower than this doubles the pause before the next one, so a busy
	// database gets room instead of a steady stream of extra writes.
	pricingResyncSlowBatch = time.Second
	// Costs are doubles; recomputing the same inputs gives the same bits, so
	// anything above this is a real change and not rounding.
	pricingResyncEpsilon = 1e-12
)

// pricingResyncRow is the part of a request_logs row that its cost and its
// totals depend on.
type pricingResyncRow struct {
	ID            int64
	TenantID      string
	APIKeyID      string
	APIKey        string
	AuthSubjectID string
	Model         string
	Source        string
	ChannelName   string
	At            time.Time
	Cost          float64
	Tokens        TokenStats
	// Exact is false when the row lacks an input its cost needs: cache writes
	// whose one-hour share was not stored (rows logged before that column), or a
	// prompt size that fits neither way upstreams count cache reads.
	Exact bool
}

// pricingResyncCandidate is a row whose cost changes.
type pricingResyncCandidate struct {
	row     pricingResyncRow
	newCost float64
}

// pricingResyncScope is what a job reads: the request models it touches and,
// for a business tenant's job, that tenant only.
type pricingResyncScope struct {
	tenantID string // "" for a system-catalog job: every tenant inherits it
	models   []string
}

// pricingResyncAffectedModels lists the request models of the last days whose
// price lookup passes through one of the job's catalog IDs. It reads day
// buckets, a few rows per model and day, not the logs.
func pricingResyncAffectedModels(db *sql.DB, job PricingResyncJob, now time.Time) (pricingResyncScope, error) {
	scope := pricingResyncScope{}
	if normalizeTenantID(job.TenantID) != systemTenantID {
		scope.tenantID = normalizeTenantID(job.TenantID)
	}
	changed := make(map[string]struct{}, len(job.ModelIDs))
	for _, id := range job.ModelIDs {
		changed[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	if len(changed) == 0 {
		return scope, nil
	}
	// One day of margin: day keys are local days and the retention is measured
	// from now.
	since := localDayKeyAtLocation(now.Add(-pricingResyncRetention-24*time.Hour), metadataForUsageDB(db).loc)
	query := `SELECT DISTINCT model FROM usage_rollup_buckets WHERE bucket_kind = ? AND bucket_start >= ?`
	args := []any{rollupBucketDay, since}
	if scope.tenantID != "" {
		query += ` AND tenant_id = ?`
		args = append(args, scope.tenantID)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return scope, fmt.Errorf("usage: pricing resync models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return scope, fmt.Errorf("usage: scan pricing resync model: %w", err)
		}
		if strings.TrimSpace(model) == "" {
			continue
		}
		for _, lookupID := range modelPricingLookupModelIDs(model) {
			if _, ok := changed[strings.ToLower(strings.TrimSpace(lookupID))]; ok {
				scope.models = append(scope.models, model)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return scope, fmt.Errorf("usage: iterate pricing resync models: %w", err)
	}
	sort.Strings(scope.models)
	return scope, nil
}

// loadPricingResyncRows reads the next batch after cursor.
func loadPricingResyncRows(ctx context.Context, db *sql.DB, job PricingResyncJob, scope pricingResyncScope, cursor int64, now time.Time) ([]pricingResyncRow, error) {
	if len(scope.models) == 0 {
		return nil, nil
	}
	upper, ok := parseStoredTimeString(job.UpperTime)
	if !ok {
		upper = now
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(scope.models)), ", ")
	query := `
		SELECT id, tenant_id, COALESCE(api_key_id, ''), COALESCE(api_key, ''), COALESCE(auth_subject_id, ''),
		       model, COALESCE(source, ''), COALESCE(channel_name, ''), timestamp, cost,
		       input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		       cache_read_tokens, cache_write_tokens, prompt_tokens, cache_write_1h_tokens
		FROM request_logs
		WHERE id > ? AND timestamp >= ? AND timestamp < ? AND model IN (` + placeholders + `)`
	args := []any{cursor, now.Add(-pricingResyncRetention).UTC().Format(time.RFC3339), upper.UTC().Format(time.RFC3339)}
	for _, model := range scope.models {
		args = append(args, model)
	}
	if scope.tenantID != "" {
		query += ` AND tenant_id = ?`
		args = append(args, scope.tenantID)
	}
	if job.Mode != PricingResyncReprice {
		query += ` AND cost = 0`
	}
	query += ` ORDER BY id ASC LIMIT ?`
	args = append(args, pricingResyncBatchSize)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: pricing resync batch: %w", err)
	}
	defer rows.Close()
	out := make([]pricingResyncRow, 0, pricingResyncBatchSize)
	for rows.Next() {
		var (
			row                                    pricingResyncRow
			at                                     storedTime
			cacheRead, cacheWrite, prompt, write1h sql.NullInt64
		)
		if err := rows.Scan(&row.ID, &row.TenantID, &row.APIKeyID, &row.APIKey, &row.AuthSubjectID,
			&row.Model, &row.Source, &row.ChannelName, &at, &row.Cost,
			&row.Tokens.InputTokens, &row.Tokens.OutputTokens, &row.Tokens.ReasoningTokens,
			&row.Tokens.CachedTokens, &row.Tokens.TotalTokens,
			&cacheRead, &cacheWrite, &prompt, &write1h); err != nil {
			return nil, fmt.Errorf("usage: scan pricing resync row: %w", err)
		}
		if !at.Valid {
			// Without a time the row has no buckets to correct; keep the cursor moving.
			row.Exact = false
		} else {
			row.At = at.Time
			row.Exact = restorePricingTokens(&row.Tokens, cacheRead, cacheWrite, prompt, write1h)
		}
		row.TenantID = normalizeTenantID(row.TenantID)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate pricing resync rows: %w", err)
	}
	return out, nil
}

// restorePricingTokens rebuilds the cache side of TokenStats from the stored
// columns, the inverse of requestLogCacheValues and promptTokenTotal. It
// reports false when the cost cannot be reproduced exactly.
func restorePricingTokens(tokens *TokenStats, cacheRead, cacheWrite, prompt, write1h sql.NullInt64) bool {
	if !cacheRead.Valid {
		// Cache use not reported (or logged before the cache columns): the live
		// path priced it from input, output and cached_tokens alone.
		return true
	}
	tokens.CacheReported = true
	tokens.CacheReadTokens = cacheRead.Int64
	if cacheWrite.Valid {
		tokens.CacheWriteTokens = cacheWrite.Int64
	}
	if tokens.CacheWriteTokens > 0 {
		if !write1h.Valid {
			return false
		}
		tokens.CacheWrite1hTokens = write1h.Int64
	}
	if tokens.CacheReadTokens == 0 {
		// Whether reads sit inside the input count changes nothing without reads.
		return true
	}
	if !prompt.Valid {
		return false
	}
	input := max(tokens.InputTokens, 0)
	read := max(tokens.CacheReadTokens, 0)
	write := max(tokens.CacheWriteTokens, 0)
	included := max(input, read)+write == prompt.Int64
	separate := input+read+write == prompt.Int64
	switch {
	case included && !separate:
		tokens.CacheReadIncludedInInput = true
	case separate && !included:
		tokens.CacheReadIncludedInInput = false
	case included && separate:
		// Only when input is 0: both forms bill the same.
		tokens.CacheReadIncludedInInput = false
	default:
		return false
	}
	return true
}

// pricingResolver resolves each tenant and model's price once per job. A price
// lookup reads the catalog from the database, candidate by candidate, so a batch
// that looked it up for every row would make hundreds of round trips. A price
// edited while a job runs queues a job of its own.
type pricingResolver struct {
	rows map[string]resolvedPricing
}

type resolvedPricing struct {
	row ModelConfigRow
	ok  bool
}

func newPricingResolver() *pricingResolver {
	return &pricingResolver{rows: map[string]resolvedPricing{}}
}

// cost is CalculateCostV2ForTenant with the price resolved once.
func (r *pricingResolver) cost(tenantID, model string, tokens TokenStats) float64 {
	key := tenantID + "\x00" + model
	resolved, seen := r.rows[key]
	if !seen {
		row, ok := resolveModelPricingRowForTenant(tenantID, model)
		resolved = resolvedPricing{row: row, ok: ok}
		r.rows[key] = resolved
	}
	return costForResolvedPricing(resolved.row, resolved.ok, tokens)
}

// pricingResyncCandidates recomputes a batch and keeps the rows whose cost
// changes. Rows that cannot be reproduced exactly are counted, never guessed.
func pricingResyncCandidates(rows []pricingResyncRow, mode string, prices *pricingResolver) (candidates []pricingResyncCandidate, skipped int64) {
	for _, row := range rows {
		if !row.Exact {
			skipped++
			continue
		}
		if mode != PricingResyncReprice && row.Cost != 0 {
			continue
		}
		newCost := prices.cost(row.TenantID, row.Model, row.Tokens)
		if math.IsNaN(newCost) || math.IsInf(newCost, 0) || newCost < 0 {
			skipped++
			continue
		}
		if math.Abs(newCost-row.Cost) <= pricingResyncEpsilon {
			continue
		}
		candidates = append(candidates, pricingResyncCandidate{row: row, newCost: newCost})
	}
	return candidates, skipped
}

// runPricingResyncJob runs a claimed job batch by batch until it is done, the
// context ends or this node stops leading. A stop between batches leaves the
// job running with its cursor; the next leader resumes it.
func runPricingResyncJob(ctx context.Context, db *sql.DB, job PricingResyncJob, leading func() bool, now func() time.Time) error {
	scope, err := pricingResyncAffectedModels(db, job, now())
	if err != nil {
		return err
	}
	if len(scope.models) == 0 {
		finishPricingResyncJob(db, job.ID, pricingResyncDone, "", now())
		return nil
	}
	pause := pricingResyncPause
	cursor := job.CursorID
	prices := newPricingResolver()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if leading != nil && !leading() {
			return errPricingResyncStopped
		}
		started := time.Now()
		rows, err := loadPricingResyncRows(ctx, db, job, scope, cursor, now())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			finishPricingResyncJob(db, job.ID, pricingResyncDone, "", now())
			return nil
		}
		candidates, skipped := pricingResyncCandidates(rows, job.Mode, prices)
		nextCursor := rows[len(rows)-1].ID
		if err := applyPricingResyncBatchWithRetry(ctx, db, job.ID, candidates, int64(len(rows)), skipped, nextCursor, now()); err != nil {
			return err
		}
		cursor = nextCursor
		if elapsed := time.Since(started); elapsed > pricingResyncSlowBatch {
			pause = min(pause*2, pricingResyncMaxPause)
		} else {
			pause = pricingResyncPause
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}
}
