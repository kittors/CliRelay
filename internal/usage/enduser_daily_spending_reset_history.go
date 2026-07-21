package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// EndUserDailySpendingResetEvent is one manual account-level daily-spending reset action.
type EndUserDailySpendingResetEvent struct {
	ID            int64     `json:"id"`
	TenantID      string    `json:"tenant_id"`
	EndUserID     string    `json:"end_user_id"`
	DayKey        string    `json:"day_key"`
	ResetAt       time.Time `json:"reset_at"`
	ActorUserID   string    `json:"actor_user_id,omitempty"`
	ActorUsername string    `json:"actor_username,omitempty"`
	ActorKind     string    `json:"actor_kind,omitempty"`
	// CostBaseline is raw today cost at reset time.
	CostBaseline float64 `json:"cost_baseline"`
	// EffectiveUsedBefore is the effective daily used amount cleared by this reset.
	EffectiveUsedBefore float64 `json:"effective_used_before"`
	// RawTodayCost is the true project-day spend (no baseline) at reset time.
	RawTodayCost float64 `json:"raw_today_cost"`
}

// TIMESTAMP for SQLite affinity + PG bootstrap compatibility (same as baseline table).
const endUserDailySpendingResetEventsTableSQL = `
CREATE TABLE IF NOT EXISTS end_user_daily_spending_reset_events (
  id                     INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  end_user_id            TEXT NOT NULL,
  day_key                TEXT NOT NULL DEFAULT '',
  reset_at               TIMESTAMP NOT NULL,
  actor_user_id          TEXT NOT NULL DEFAULT '',
  actor_username         TEXT NOT NULL DEFAULT '',
  actor_kind             TEXT NOT NULL DEFAULT '',
  cost_baseline          REAL NOT NULL DEFAULT 0,
  effective_used_before  REAL NOT NULL DEFAULT 0,
  raw_today_cost         REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_end_user_daily_spending_reset_events_user
  ON end_user_daily_spending_reset_events(tenant_id, end_user_id, reset_at DESC);
`

func ensureEndUserDailySpendingResetEventsTable(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(endUserDailySpendingResetEventsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure end_user_daily_spending_reset_events: %w", err)
	}
	return nil
}

func bootstrapEndUserDailySpendingResetEvents(db *sql.DB) error {
	return ensureEndUserDailySpendingResetEventsTable(db)
}

// InsertEndUserDailySpendingResetEvent appends one reset history row.
func InsertEndUserDailySpendingResetEvent(ev EndUserDailySpendingResetEvent) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	tenantID := normalizeTenantID(ev.TenantID)
	endUserID := strings.TrimSpace(ev.EndUserID)
	if endUserID == "" {
		return fmt.Errorf("usage: end_user_id is required")
	}
	resetAt := ev.ResetAt
	if resetAt.IsZero() {
		resetAt = time.Now().UTC()
	}
	dayKey := strings.TrimSpace(ev.DayKey)
	if dayKey == "" {
		dayKey = LocalDayKeyAt(resetAt)
	}
	baseline := ev.CostBaseline
	if baseline < 0 {
		baseline = 0
	}
	usedBefore := ev.EffectiveUsedBefore
	if usedBefore < 0 {
		usedBefore = 0
	}
	raw := ev.RawTodayCost
	if raw < 0 {
		raw = 0
	}
	_, err := db.Exec(
		`INSERT INTO end_user_daily_spending_reset_events
		 (tenant_id, end_user_id, day_key, reset_at, actor_user_id, actor_username, actor_kind,
		  cost_baseline, effective_used_before, raw_today_cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, endUserID, dayKey, resetAt.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(ev.ActorUserID), strings.TrimSpace(ev.ActorUsername), strings.TrimSpace(ev.ActorKind),
		baseline, usedBefore, raw,
	)
	if err != nil {
		return fmt.Errorf("usage: insert end-user daily spending reset event: %w", err)
	}
	return nil
}

// CountEndUserDailySpendingResetEvents returns how many reset events exist for an end user.
func CountEndUserDailySpendingResetEvents(tenantID, endUserID string) (int, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return 0, nil
	}
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM end_user_daily_spending_reset_events WHERE tenant_id = ? AND end_user_id = ?`,
		tenantID, endUserID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("usage: count end-user daily spending reset events: %w", err)
	}
	return n, nil
}

// ListEndUserDailySpendingResetEventCounts returns reset counts keyed by end_user_id.
func ListEndUserDailySpendingResetEventCounts(tenantID string, endUserIDs []string) (map[string]int, error) {
	out := make(map[string]int)
	if len(endUserIDs) == 0 {
		return out, nil
	}
	db := getDB()
	if db == nil {
		return out, nil
	}
	tenantID = normalizeTenantID(tenantID)
	ids := make([]string, 0, len(endUserIDs))
	seen := make(map[string]struct{}, len(endUserIDs))
	for _, id := range endUserIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)+1)
	args = append(args, tenantID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT end_user_id, COUNT(*) FROM end_user_daily_spending_reset_events
		 WHERE tenant_id = ? AND end_user_id IN (`+strings.Join(placeholders, ",")+`)
		 GROUP BY end_user_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: list end-user daily spending reset event counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("usage: scan end-user reset event count: %w", err)
		}
		out[strings.TrimSpace(id)] = n
	}
	return out, rows.Err()
}

// ListEndUserDailySpendingResetEvents returns newest-first history for an end user.
func ListEndUserDailySpendingResetEvents(tenantID, endUserID string, limit int) ([]EndUserDailySpendingResetEvent, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := db.Query(
		`SELECT id, tenant_id, end_user_id, day_key, reset_at, actor_user_id, actor_username, actor_kind,
		        cost_baseline, effective_used_before, raw_today_cost
		 FROM end_user_daily_spending_reset_events
		 WHERE tenant_id = ? AND end_user_id = ?
		 ORDER BY reset_at DESC, id DESC
		 LIMIT ?`,
		tenantID, endUserID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: list end-user daily spending reset events: %w", err)
	}
	defer rows.Close()
	out := make([]EndUserDailySpendingResetEvent, 0)
	for rows.Next() {
		var ev EndUserDailySpendingResetEvent
		var resetAt string
		if err := rows.Scan(
			&ev.ID, &ev.TenantID, &ev.EndUserID, &ev.DayKey, &resetAt,
			&ev.ActorUserID, &ev.ActorUsername, &ev.ActorKind,
			&ev.CostBaseline, &ev.EffectiveUsedBefore, &ev.RawTodayCost,
		); err != nil {
			return nil, fmt.Errorf("usage: scan end-user daily spending reset event: %w", err)
		}
		if ts, err := time.Parse(time.RFC3339Nano, resetAt); err == nil {
			ev.ResetAt = ts
		} else if ts, err := time.Parse(time.RFC3339, resetAt); err == nil {
			ev.ResetAt = ts
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// DeleteEndUserDailySpendingResetEvents removes history for an end user.
func DeleteEndUserDailySpendingResetEvents(tenantID, endUserID string) error {
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	_, err := db.Exec(
		`DELETE FROM end_user_daily_spending_reset_events WHERE tenant_id = ? AND end_user_id = ?`,
		tenantID, endUserID,
	)
	if err != nil {
		return fmt.Errorf("usage: delete end-user daily spending reset events: %w", err)
	}
	return nil
}
