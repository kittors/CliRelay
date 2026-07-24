package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const authSubjectUsageDailyBackfillMarker = "auth_subject_usage_daily_v2"
const authSubjectUsageDailyBackfillDays = 30

// AuthSubjectUsageSummary is the lightweight cycle/window summary for cards.
type AuthSubjectUsageSummary struct {
	AuthSubjectID     string     `json:"auth_subject_id"`
	RequestTotal      int64      `json:"request_total"`
	SuccessTotal      int64      `json:"success_total"`
	FailureTotal      int64      `json:"failure_total"`
	CostTotal         float64    `json:"cost_total"`
	SuccessRate       *float64   `json:"success_rate,omitempty"`
	RequestTotal7d    int64      `json:"request_total_7d"`
	CostTotal7d       float64    `json:"cost_total_7d"`
	RequestTotal30d   int64      `json:"request_total_30d"`
	SuccessTotal30d   int64      `json:"success_total_30d"`
	FailureTotal30d   int64      `json:"failure_total_30d"`
	CycleRequestTotal int64      `json:"cycle_request_total"`
	CycleCostTotal    float64    `json:"cycle_cost_total"`
	CycleTotalTokens  int64      `json:"cycle_total_tokens"`
	CycleKnown        bool       `json:"cycle_known"`
	CycleStart        string     `json:"cycle_start,omitempty"`
	ProjectedSince    *time.Time `json:"projected_since,omitempty"`
	HistoryComplete   bool       `json:"history_complete"`
	WeeklyQuotaUsed   *float64   `json:"weekly_quota_used_percent,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at,omitempty"`
}

func ensureUsageProjectionMarkerTable(db *sql.DB) {
	if db == nil {
		return
	}
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS usage_projection_markers (
			marker_key   TEXT NOT NULL PRIMARY KEY,
			marker_value TEXT NOT NULL DEFAULT '',
			updated_at   DATETIME NOT NULL
		)
	`)
}

func projectionMarkerValue(db *sql.DB, key string) string {
	if db == nil {
		return ""
	}
	var value string
	err := db.QueryRow(`SELECT marker_value FROM usage_projection_markers WHERE marker_key = ?`, key).Scan(&value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func setProjectionMarker(db *sql.DB, key, value string) error {
	if db == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO UPDATE SET
			marker_value = excluded.marker_value,
			updated_at = excluded.updated_at
	`, key, value, now)
	return err
}

// RunAuthSubjectUsageDailyBackfillAtInit rebuilds daily projection once per marker version
// before request traffic uses incremental UPSERTs. Uses usageLoc day keys (not UTC-hardcoded SQL).
// Safe to call multiple times; no-ops when marker already set. Never called from GET handlers.
func RunAuthSubjectUsageDailyBackfillAtInit(days int) error {
	return runAuthSubjectUsageDailyBackfillAtInitDB(getDB(), days, getUsageLocation())
}

// runAuthSubjectUsageDailyBackfillAtInitDB accepts the already-open database so
// startup can run it while usageDBMu is held without re-entering getDB.
func runAuthSubjectUsageDailyBackfillAtInitDB(db *sql.DB, days int, loc *time.Location) error {
	if db == nil {
		return nil
	}
	ensureUsageProjectionMarkerTable(db)
	if projectionMarkerValue(db, authSubjectUsageDailyBackfillMarker) == "done" {
		return nil
	}
	if days < 1 {
		days = authSubjectUsageDailyBackfillDays
	}
	// Backfill all tenants present in request_logs for the window.
	tenants, err := listRequestLogTenants(db)
	if err != nil {
		return err
	}
	for _, tenantID := range tenants {
		if err := backfillAuthSubjectUsageDailyForTenantDB(db, tenantID, days, loc); err != nil {
			return err
		}
	}
	return setProjectionMarker(db, authSubjectUsageDailyBackfillMarker, "done")
}

func listRequestLogTenants(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT tenant_id FROM request_logs`)
	if err != nil {
		// Empty DB / missing table during tests.
		return []string{systemTenantID}, nil
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var tenant string
		if err := rows.Scan(&tenant); err != nil {
			return nil, err
		}
		out = append(out, normalizeTenantID(tenant))
	}
	if len(out) == 0 {
		out = append(out, systemTenantID)
	}
	return out, rows.Err()
}

// BackfillAuthSubjectUsageDailyForTenant rebuilds daily projection for one tenant.
// Uses absolute UPSERT values for the window (no DELETE race with live increments).
func BackfillAuthSubjectUsageDailyForTenant(tenantID string, days int) error {
	return backfillAuthSubjectUsageDailyForTenantDB(getDB(), tenantID, days, getUsageLocation())
}

func backfillAuthSubjectUsageDailyForTenantDB(db *sql.DB, tenantID string, days int, loc *time.Location) error {
	if db == nil {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	if days < 1 {
		days = authSubjectUsageDailyBackfillDays
	}
	if days > 90 {
		days = 90
	}
	cutoff := cutoffStartUTCAtLocation(time.Now(), days, loc)
	cutoffStr := cutoff.UTC().Format(time.RFC3339Nano)

	rows, err := db.Query(`
		SELECT auth_subject_id, timestamp, cost, failed
		FROM request_logs
		WHERE tenant_id = ?
		  AND timestamp >= ?
		  AND trim(coalesce(auth_subject_id, '')) <> ''
	`, tenantID, cutoffStr)
	if err != nil {
		return fmt.Errorf("usage: usage daily backfill scan: %w", err)
	}
	defer rows.Close()

	type aggKey struct {
		subject string
		day     string
	}
	type aggVal struct {
		req, ok, fail int64
		cost          float64
	}
	aggs := make(map[aggKey]aggVal)
	for rows.Next() {
		var subject, ts string
		var cost float64
		var failed int
		if err = rows.Scan(&subject, &ts, &cost, &failed); err != nil {
			return fmt.Errorf("usage: usage daily backfill row: %w", err)
		}
		subject = strings.TrimSpace(subject)
		if subject == "" {
			continue
		}
		parsed, ok := parseStoredTimeString(ts)
		if !ok {
			continue
		}
		key := aggKey{subject: subject, day: localDayKeyAtLocation(parsed, loc)}
		cur := aggs[key]
		cur.req++
		cur.cost += cost
		if failed != 0 {
			cur.fail++
		} else {
			cur.ok++
		}
		aggs[key] = cur
	}
	if err = rows.Err(); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: usage daily backfill begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(`
		INSERT INTO auth_subject_usage_daily (
			tenant_id, auth_subject_id, day_key, request_count, success_count, failure_count, cost_total, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, auth_subject_id, day_key) DO UPDATE SET
			request_count = excluded.request_count,
			success_count = excluded.success_count,
			failure_count = excluded.failure_count,
			cost_total = excluded.cost_total,
			updated_at = excluded.updated_at
	`)
	if err != nil {
		return fmt.Errorf("usage: usage daily backfill prepare: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for key, val := range aggs {
		if _, err = stmt.Exec(tenantID, key.subject, key.day, val.req, val.ok, val.fail, val.cost, now); err != nil {
			return fmt.Errorf("usage: usage daily backfill insert: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage: usage daily backfill commit: %w", err)
	}
	return nil
}

// QueryAuthSubjectUsageSummaries returns batch card summaries from the daily projection.
// Never scans request_logs.
func QueryAuthSubjectUsageSummaries(tenantID string, authSubjectIDs []string, cycleStartBySubject map[string]time.Time) (map[string]AuthSubjectUsageSummary, error) {
	// Pure SELECT: prefer read pool so GET status does not queue behind the single writer.
	db := getReadDB()
	if db == nil {
		return map[string]AuthSubjectUsageSummary{}, nil
	}
	tenantID = normalizeTenantID(tenantID)
	ids := dedupeExactStrings(authSubjectIDs)
	if len(ids) == 0 {
		return map[string]AuthSubjectUsageSummary{}, nil
	}

	dayFrom30 := localDayKeyAt(time.Now().AddDate(0, 0, -29))
	dayFrom7 := localDayKeyAt(time.Now().AddDate(0, 0, -6))
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, 2+len(ids))
	args = append(args, tenantID, dayFrom30)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := db.Query(`
		SELECT auth_subject_id, day_key, request_count, success_count, failure_count, cost_total, updated_at
		FROM auth_subject_usage_daily
		WHERE tenant_id = ?
		  AND day_key >= ?
		  AND auth_subject_id IN (`+placeholders+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: query usage daily: %w", err)
	}
	defer rows.Close()

	out := make(map[string]AuthSubjectUsageSummary, len(ids))
	for _, id := range ids {
		summary := AuthSubjectUsageSummary{AuthSubjectID: id}
		if cycleStart, ok := cycleStartBySubject[id]; ok && !cycleStart.IsZero() {
			summary.CycleKnown = true
			summary.CycleStart = cycleStart.UTC().Format(time.RFC3339)
		}
		out[id] = summary
	}

	for rows.Next() {
		var subject, dayKey, updated string
		var count, success, failure int64
		var cost float64
		if err := rows.Scan(&subject, &dayKey, &count, &success, &failure, &cost, &updated); err != nil {
			return nil, fmt.Errorf("usage: scan usage daily: %w", err)
		}
		subject = strings.TrimSpace(subject)
		cur := out[subject]
		cur.AuthSubjectID = subject
		cur.RequestTotal30d += count
		cur.SuccessTotal30d += success
		cur.FailureTotal30d += failure
		if dayKey >= dayFrom7 {
			cur.RequestTotal7d += count
			cur.CostTotal7d += cost
		}
		if t, ok := parseStoredTimeString(updated); ok && t.After(cur.UpdatedAt) {
			cur.UpdatedAt = t
		}
		if cycleStart, ok := cycleStartBySubject[subject]; ok && !cycleStart.IsZero() {
			if dayKey >= localDayKeyAt(cycleStart) {
				cur.CycleRequestTotal += count
				cur.CycleCostTotal += cost
			}
		}
		out[subject] = cur
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
