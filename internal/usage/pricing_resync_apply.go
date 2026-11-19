package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// Applying a batch of recomputed costs. The live write path adds a request to
// request_logs and to every total built from it in one transaction; a cost
// correction moves the same totals by the difference, in one transaction per
// batch, and nothing but cost:
//
//   - request_logs.cost, guarded by the cost it was read with, so a row another
//     writer changed in between is left alone and counted nowhere;
//   - usage_rollup_buckets, under the keys and bucket starts the live write used
//     (rollupBucketStarts, the request's own time and dimensions). Minute buckets
//     are only kept a day and hour buckets thirty days: past that they are gone
//     and get no delta;
//   - ai_account_subject_usage_buckets: day and lifetime, and the cycle bucket
//     only when the request falls in the account's current cycle
//     (aiAccountSubjectCycleAt), never the current cycle for an older request;
//   - manual spending resets taken after the request in the window it belongs
//     to: their baseline rises by the same delta, so a request from before a
//     reset stays out of the spend counted since it.
//
// Request and token counts never change. Locks follow the live path's order
// (request rows, then rollup buckets kind by kind, then subject buckets day,
// lifetime, cycle), so a batch cannot deadlock against live writers.

type pricingResyncApplied struct {
	row       pricingResyncRow
	delta     float64
	endUserID string
}

var pricingResyncRollupKinds = []string{rollupBucketMinute, rollupBucketQuotaMinuteUTC, rollupBucketHour, rollupBucketDay, rollupBucketLifetime}

func applyPricingResyncBatchWithRetry(ctx context.Context, db *sql.DB, jobID int64, candidates []pricingResyncCandidate, scanned, skipped, cursor int64, now time.Time) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = applyPricingResyncBatch(ctx, db, jobID, candidates, scanned, skipped, cursor, now)
		if err == nil || !isRetryableUsageWriteErr(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return err
}

func applyPricingResyncBatch(ctx context.Context, db *sql.DB, jobID int64, candidates []pricingResyncCandidate, scanned, skipped, cursor int64, now time.Time) error {
	usageWriteLease.RLock()
	defer usageWriteLease.RUnlock()
	usageProjectionMu.RLock()
	defer usageProjectionMu.RUnlock()

	endUsers, err := pricingResyncEndUsers(ctx, db, candidates)
	if err != nil {
		return err
	}
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage: begin pricing resync batch: %w", err)
	}
	tx := boundTx{tx: sqlTx, ctx: ctx, metadata: metadataForUsageDB(db)}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	applied := make([]pricingResyncApplied, 0, len(candidates))
	total := 0.0
	for _, candidate := range candidates {
		result, err := tx.Exec(`UPDATE request_logs SET cost = ? WHERE id = ? AND cost = ?`,
			candidate.newCost, candidate.row.ID, candidate.row.Cost)
		if err != nil {
			return fmt.Errorf("usage: pricing resync update cost: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			continue
		}
		delta := candidate.newCost - candidate.row.Cost
		applied = append(applied, pricingResyncApplied{
			row:       candidate.row,
			delta:     delta,
			endUserID: endUsers[strings.TrimSpace(candidate.row.APIKeyID)],
		})
		total += delta
	}
	if len(applied) > 0 {
		if err := applyRollupCostDeltasTx(tx, applied, now); err != nil {
			return err
		}
		if err := applySubjectCostDeltasTx(tx, applied); err != nil {
			return err
		}
		if err := applyResetBaselineDeltasTx(tx, applied, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE pricing_resync_jobs
		SET cursor_id = ?, scanned = scanned + ?, changed = changed + ?, skipped = skipped + ?,
		    cost_delta = cost_delta + ?, updated_at = ?
		WHERE id = ?`, cursor, scanned, int64(len(applied)), skipped, total, now.UTC().Format(time.RFC3339Nano), jobID); err != nil {
		return fmt.Errorf("usage: pricing resync progress: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit pricing resync batch: %w", err)
	}
	committed = true
	return nil
}

// pricingResyncEndUsers maps the batch's key ids to the account they belong
// to now, the way the rollup rebuild does: request_logs keeps the key, not the
// account.
func pricingResyncEndUsers(ctx context.Context, db *sql.DB, candidates []pricingResyncCandidate) (map[string]string, error) {
	ids := make([]string, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.row.APIKeyID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(CAST(end_user_id AS TEXT), '') FROM api_keys
		WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: pricing resync key owners: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, endUser string
		if err := rows.Scan(&id, &endUser); err != nil {
			return nil, fmt.Errorf("usage: scan pricing resync key owner: %w", err)
		}
		out[strings.TrimSpace(id)] = strings.TrimSpace(endUser)
	}
	return out, rows.Err()
}

type rollupCostDeltaKey struct {
	kind, start                      string
	tenant, apiKey, endUser, subject string
	model, source, channel           string
}

// rollupKindKept says whether a bucket of this kind still exists for a request
// that happened at at (see cleanupExpiredUsageRollupBuckets).
func rollupKindKept(kind string, at, now time.Time) bool {
	age := now.Sub(at)
	switch kind {
	case rollupBucketMinute, rollupBucketQuotaMinuteUTC:
		return age < rollupMinuteRetention
	case rollupBucketHour:
		return age < rollupHourRetention
	case rollupBucketDay:
		return age < rollupDayRetention
	default:
		return true
	}
}

func applyRollupCostDeltasTx(tx usageMetadataWriteTx, applied []pricingResyncApplied, now time.Time) error {
	loc := tx.usageMetadata().loc
	if loc == nil {
		loc = time.Local
	}
	deltas := map[rollupCostDeltaKey]float64{}
	for _, item := range applied {
		starts := rollupBucketStarts(item.row.At, loc)
		for _, kind := range pricingResyncRollupKinds {
			if !rollupKindKept(kind, item.row.At, now) {
				continue
			}
			key := rollupCostDeltaKey{
				kind: kind, start: starts[kind],
				tenant:  normalizeTenantID(item.row.TenantID),
				apiKey:  strings.TrimSpace(item.row.APIKeyID),
				endUser: strings.TrimSpace(item.endUserID),
				subject: strings.TrimSpace(item.row.AuthSubjectID),
				model:   strings.TrimSpace(item.row.Model),
				source:  strings.TrimSpace(item.row.Source),
				channel: strings.TrimSpace(item.row.ChannelName),
			}
			deltas[key] += item.delta
		}
	}
	keys := make([]rollupCostDeltaKey, 0, len(deltas))
	for key := range deltas {
		keys = append(keys, key)
	}
	kindOrder := map[string]int{}
	for i, kind := range pricingResyncRollupKinds {
		kindOrder[kind] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if kindOrder[a.kind] != kindOrder[b.kind] {
			return kindOrder[a.kind] < kindOrder[b.kind]
		}
		return strings.Join([]string{a.start, a.tenant, a.apiKey, a.endUser, a.subject, a.model, a.source, a.channel}, "\x00") <
			strings.Join([]string{b.start, b.tenant, b.apiKey, b.endUser, b.subject, b.model, b.source, b.channel}, "\x00")
	})
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, key := range keys {
		if _, err := tx.Exec(`
			INSERT INTO usage_rollup_buckets (
				tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
				model, source, channel_name, cost_total, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(
				tenant_id, bucket_kind, bucket_start,
				api_key_id, end_user_id, auth_subject_id,
				model, source, channel_name
			) DO UPDATE SET
				cost_total = usage_rollup_buckets.cost_total + excluded.cost_total,
				updated_at = excluded.updated_at
		`, key.tenant, key.kind, key.start, key.apiKey, key.endUser, key.subject,
			key.model, key.source, key.channel, deltas[key], stamp); err != nil {
			return fmt.Errorf("usage: pricing resync rollup %s: %w", key.kind, err)
		}
	}
	return nil
}

type subjectCostDeltaKey struct{ kind, subject, start string }

func applySubjectCostDeltasTx(tx usageMetadataWriteTx, applied []pricingResyncApplied) error {
	loc := tx.usageMetadata().loc
	if loc == nil {
		loc = time.Local
	}
	deltas := map[subjectCostDeltaKey]float64{}
	firstEvent := map[subjectCostDeltaKey]time.Time{}
	add := func(key subjectCostDeltaKey, delta float64, at time.Time) {
		deltas[key] += delta
		if first, ok := firstEvent[key]; !ok || at.Before(first) {
			firstEvent[key] = at
		}
	}
	for _, item := range applied {
		subject := strings.TrimSpace(item.row.AuthSubjectID)
		if subject == "" {
			continue
		}
		at := item.row.At
		add(subjectCostDeltaKey{kind: "day", subject: subject, start: at.In(loc).Format("2006-01-02")}, item.delta, at)
		add(subjectCostDeltaKey{kind: "lifetime", subject: subject, start: rollupLifetimeStart}, item.delta, at)
		// The same lookup the live projection makes: known only when the request
		// falls in the account's current cycle (rolled forward to it), never for
		// a request from before the stored cycle start.
		cycle, known, err := aiAccountSubjectCycleAt(tx, subject, at)
		if err != nil {
			return err
		}
		if known {
			add(subjectCostDeltaKey{kind: "cycle", subject: subject, start: formatAIAccountSubjectCycleBucketStart(cycle.CycleStartAt)}, item.delta, at)
		}
	}
	keys := make([]subjectCostDeltaKey, 0, len(deltas))
	for key := range deltas {
		keys = append(keys, key)
	}
	kindOrder := map[string]int{"day": 0, "lifetime": 1, "cycle": 2}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if kindOrder[a.kind] != kindOrder[b.kind] {
			return kindOrder[a.kind] < kindOrder[b.kind]
		}
		if a.subject != b.subject {
			return a.subject < b.subject
		}
		return a.start < b.start
	})
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, key := range keys {
		if _, err := tx.Exec(`
			INSERT INTO ai_account_subject_usage_buckets (
				auth_subject_id, bucket_kind, bucket_start, request_count,
				success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
			) VALUES (?, ?, ?, 0, 0, 0, ?, 0, ?, ?)
			ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
				cost_total = ai_account_subject_usage_buckets.cost_total + excluded.cost_total,
				updated_at = excluded.updated_at
		`, key.subject, key.kind, key.start, deltas[key], firstEvent[key].UTC().Format(time.RFC3339Nano), stamp); err != nil {
			return fmt.Errorf("usage: pricing resync shared subject %s: %w", key.kind, err)
		}
	}
	return nil
}

// rowInResetWindow says whether a request at at belongs to the window a reset
// was taken in. The reset only counts while that window is still the current
// one (see subjectPeriodWindows.windowKey), so the window ends now.
func rowInResetWindow(period quota.Period, at time.Time, windowKey string, loc *time.Location) bool {
	switch period {
	case quota.PeriodFiveHour:
		return quotaMinuteKey(at) >= windowKey
	case quota.PeriodDay:
		return localDayKeyAtLocation(at, loc) == windowKey
	case quota.PeriodWeek, quota.PeriodMonth:
		return localDayKeyAtLocation(at, loc) >= windowKey
	case quota.PeriodLifetime:
		return true
	default:
		return false
	}
}

func applyResetBaselineDeltasTx(tx usageMetadataWriteTx, applied []pricingResyncApplied, now time.Time) error {
	byTenant := map[string][]pricingResyncApplied{}
	for _, item := range applied {
		tenant := normalizeTenantID(item.row.TenantID)
		byTenant[tenant] = append(byTenant[tenant], item)
	}
	tenants := make([]string, 0, len(byTenant))
	for tenant := range byTenant {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	for _, tenant := range tenants {
		items := byTenant[tenant]
		if err := applyDailyResetDeltasTx(tx, tenant, items, now, "api_key_daily_spending_resets", "api_key_id", func(item pricingResyncApplied) string {
			return strings.TrimSpace(item.row.APIKeyID)
		}); err != nil {
			return err
		}
		if err := applyDailyResetDeltasTx(tx, tenant, items, now, "end_user_daily_spending_resets", "end_user_id", func(item pricingResyncApplied) string {
			return strings.TrimSpace(item.endUserID)
		}); err != nil {
			return err
		}
		if err := applyPeriodResetDeltasTx(tx, tenant, items, now, periodSubjectAPIKey, func(item pricingResyncApplied) string {
			return strings.TrimSpace(item.row.APIKeyID)
		}); err != nil {
			return err
		}
		if err := applyPeriodResetDeltasTx(tx, tenant, items, now, periodSubjectEndUser, func(item pricingResyncApplied) string {
			return strings.TrimSpace(item.endUserID)
		}); err != nil {
			return err
		}
	}
	return nil
}

func distinctSubjects(items []pricingResyncApplied, subjectOf func(pricingResyncApplied) string) ([]string, []any) {
	seen := map[string]struct{}{}
	ids := make([]string, 0)
	for _, item := range items {
		id := subjectOf(item)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return ids, args
}

// applyDailyResetDeltasTx raises today's reset baselines by the cost of the
// requests that were made today before the reset.
func applyDailyResetDeltasTx(tx usageMetadataWriteTx, tenant string, items []pricingResyncApplied, now time.Time, table, column string, subjectOf func(pricingResyncApplied) string) error {
	ids, args := distinctSubjects(items, subjectOf)
	if len(ids) == 0 {
		return nil
	}
	loc := tx.usageMetadata().loc
	today := localDayKeyAtLocation(now, loc)
	query := `SELECT CAST(` + column + ` AS TEXT), day_key, reset_at FROM ` + table + `
		WHERE tenant_id = ? AND day_key = ? AND CAST(` + column + ` AS TEXT) IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + `)`
	rows, err := tx.Query(query, append([]any{tenant, today}, args...)...)
	if err != nil {
		return fmt.Errorf("usage: pricing resync read %s: %w", table, err)
	}
	type reset struct {
		dayKey string
		at     time.Time
	}
	resets := map[string]reset{}
	for rows.Next() {
		var id, dayKey string
		var at storedTime
		if err := rows.Scan(&id, &dayKey, &at); err != nil {
			_ = rows.Close()
			return fmt.Errorf("usage: pricing resync scan %s: %w", table, err)
		}
		if at.Valid {
			resets[strings.TrimSpace(id)] = reset{dayKey: strings.TrimSpace(dayKey), at: at.Time}
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("usage: pricing resync iterate %s: %w", table, err)
	}
	deltas := map[string]float64{}
	for _, item := range items {
		id := subjectOf(item)
		r, ok := resets[id]
		if !ok || localDayKeyAtLocation(item.row.At, loc) != r.dayKey || !item.row.At.Before(r.at) {
			continue
		}
		deltas[id] += item.delta
	}
	for _, id := range ids {
		delta, ok := deltas[id]
		if !ok {
			continue
		}
		if _, err := tx.Exec(`UPDATE `+table+` SET cost_baseline = cost_baseline + ?
			WHERE tenant_id = ? AND day_key = ? AND CAST(`+column+` AS TEXT) = ?`, delta, tenant, today, id); err != nil {
			return fmt.Errorf("usage: pricing resync raise %s: %w", table, err)
		}
	}
	return nil
}

// applyPeriodResetDeltasTx raises the baselines of the period resets (five
// hours, day, week, month, lifetime) taken after a request in the window the
// request belongs to, together with the per-model cost in their usage baseline.
func applyPeriodResetDeltasTx(tx usageMetadataWriteTx, tenant string, items []pricingResyncApplied, now time.Time, subject periodSubject, subjectOf func(pricingResyncApplied) string) error {
	ids, args := distinctSubjects(items, subjectOf)
	if len(ids) == 0 {
		return nil
	}
	subjectType, err := periodResetSubjectType(subject)
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT subject_id, period, window_key, reset_at, usage_baseline FROM period_spending_resets
		WHERE tenant_id = ? AND subject_type = ? AND subject_id IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")+`)`,
		append([]any{tenant, subjectType}, args...)...)
	if err != nil {
		return fmt.Errorf("usage: pricing resync read period resets: %w", err)
	}
	type reset struct {
		subjectID, windowKey, usage string
		period                      quota.Period
		at                          time.Time
	}
	resets := make([]reset, 0)
	for rows.Next() {
		var r reset
		var rawPeriod string
		var at storedTime
		if err := rows.Scan(&r.subjectID, &rawPeriod, &r.windowKey, &at, &r.usage); err != nil {
			_ = rows.Close()
			return fmt.Errorf("usage: pricing resync scan period reset: %w", err)
		}
		if !at.Valid {
			continue
		}
		r.subjectID = strings.TrimSpace(r.subjectID)
		r.windowKey = strings.TrimSpace(r.windowKey)
		r.period = quota.Period(rawPeriod)
		r.at = at.Time
		resets = append(resets, r)
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("usage: pricing resync iterate period resets: %w", err)
	}
	if len(resets) == 0 {
		return nil
	}
	windows, err := loadSubjectPeriodWindows(tenant, subject, ids, now)
	if err != nil {
		return err
	}
	loc := tx.usageMetadata().loc
	for _, r := range resets {
		if expected := windows.windowKey(r.subjectID, r.period); expected == "" || expected != r.windowKey {
			// A reset of a window that has rolled over no longer counts.
			continue
		}
		costDelta := 0.0
		modelDeltas := map[string]float64{}
		for _, item := range items {
			if subjectOf(item) != r.subjectID || !item.row.At.Before(r.at) || !rowInResetWindow(r.period, item.row.At, r.windowKey, loc) {
				continue
			}
			costDelta += item.delta
			modelDeltas[strings.TrimSpace(item.row.Model)] += item.delta
		}
		if costDelta == 0 {
			continue
		}
		usage := r.usage
		var baseline windowUsageBaseline
		if strings.TrimSpace(usage) != "" && json.Unmarshal([]byte(usage), &baseline) == nil && baseline.Window == r.windowKey {
			if baseline.Models == nil {
				baseline.Models = map[string]modelBaseline{}
			}
			for model, delta := range modelDeltas {
				item := baseline.Models[model]
				item.Cost += delta
				baseline.Models[model] = item
			}
			usage = encodeUsageBaseline(baseline)
		}
		if _, err := tx.Exec(`UPDATE period_spending_resets SET cost_baseline = cost_baseline + ?, usage_baseline = ?
			WHERE tenant_id = ? AND subject_type = ? AND subject_id = ? AND period = ? AND window_key = ?`,
			costDelta, usage, tenant, subjectType, r.subjectID, string(r.period), r.windowKey); err != nil {
			return fmt.Errorf("usage: pricing resync raise period reset: %w", err)
		}
	}
	return nil
}
