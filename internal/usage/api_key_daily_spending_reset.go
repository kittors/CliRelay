package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// APIKeyDailySpendingReset is a per-key baseline for project-timezone daily spending.
// Effective today cost = max(raw_today_cost - cost_baseline, 0) when day_key matches today.
type APIKeyDailySpendingReset struct {
	TenantID     string
	APIKeyID     string
	DayKey       string
	CostBaseline float64
	ResetAt      time.Time
}

// TIMESTAMP works on both SQLite (affinity) and PostgreSQL (native type).
// Avoid DATETIME: PostgreSQL rejects it when bootstrap runs on a PG connection.
const apiKeyDailySpendingResetsTableSQL = `
CREATE TABLE IF NOT EXISTS api_key_daily_spending_resets (
  tenant_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  api_key_id    TEXT NOT NULL,
  day_key       TEXT NOT NULL DEFAULT '',
  cost_baseline REAL NOT NULL DEFAULT 0,
  reset_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (tenant_id, api_key_id)
);
CREATE INDEX IF NOT EXISTS idx_api_key_daily_spending_resets_day
  ON api_key_daily_spending_resets(tenant_id, day_key);
`

func ensureAPIKeyDailySpendingResetsTable(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(apiKeyDailySpendingResetsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure api_key_daily_spending_resets: %w", err)
	}
	return nil
}

func bootstrapAPIKeyDailySpendingResets(db *sql.DB) error {
	return ensureAPIKeyDailySpendingResetsTable(db)
}

// QueryRawTodayCostByKeyForTenant returns SUM(cost) for the current project day (no reset baseline).
func QueryRawTodayCostByKeyForTenant(tenantID, apiKey string) (float64, error) {
	tenantID = normalizeTenantID(tenantID)
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return 0, nil
	}
	apiKeyID := ""
	if identity := ResolveAPIKeyIdentity(apiKey); identity != nil {
		apiKeyID = identity.ID
	}
	if apiKeyID == "" {
		if row := GetAPIKeyForTenant(tenantID, apiKey); row != nil {
			apiKeyID = strings.TrimSpace(row.ID)
		}
	}
	if apiKeyID == "" {
		return 0, nil
	}
	return queryTodayCostByAPIKeyIDFromRollup(tenantID, apiKeyID)
}

// QueryTodayCostByKey returns effective project-day cost after applying any same-day reset baseline.
func QueryTodayCostByKey(apiKey string) (float64, error) {
	tenantID := ResolveAPIKeyTenant(apiKey)
	if tenantID == "" {
		tenantID = systemTenantID
	}
	return QueryTodayEffectiveCostByKeyForTenant(tenantID, apiKey)
}

// QueryTodayEffectiveCostByKeyForTenant returns max(raw_today - baseline, 0) for the key.
func QueryTodayEffectiveCostByKeyForTenant(tenantID, apiKey string) (float64, error) {
	raw, err := QueryRawTodayCostByKeyForTenant(tenantID, apiKey)
	if err != nil {
		return 0, err
	}
	row := GetAPIKeyForTenant(tenantID, apiKey)
	if row == nil || strings.TrimSpace(row.ID) == "" {
		return raw, nil
	}
	baseline, ok, err := GetDailySpendingResetBaseline(tenantID, row.ID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return raw, nil
	}
	return effectiveTodayCost(raw, baseline), nil
}

func effectiveTodayCost(raw, baseline float64) float64 {
	used := raw - baseline
	if used < 0 {
		return 0
	}
	return used
}

// DailySpendingRemaining returns remaining budget for a limit, or nil when unlimited (limit <= 0).
func DailySpendingRemaining(limit, used float64) *float64 {
	if limit <= 0 {
		return nil
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return &remaining
}

// GetDailySpendingResetBaseline returns today's cost baseline when a same-day reset exists.
func GetDailySpendingResetBaseline(tenantID, apiKeyID string) (float64, bool, error) {
	db := getDB()
	if db == nil {
		return 0, false, nil
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return 0, false, nil
	}
	var dayKey string
	var baseline float64
	err := db.QueryRow(
		`SELECT day_key, cost_baseline FROM api_key_daily_spending_resets WHERE tenant_id = ? AND api_key_id = ?`,
		tenantID, apiKeyID,
	).Scan(&dayKey, &baseline)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("usage: get daily spending reset: %w", err)
	}
	if strings.TrimSpace(dayKey) != LocalDayKeyAt(time.Now()) {
		return 0, false, nil
	}
	return baseline, true, nil
}

// ListDailySpendingResetBaselines returns same-day baselines keyed by api_key_id.
func ListDailySpendingResetBaselines(tenantID string, apiKeyIDs []string) (map[string]float64, error) {
	return listDailySpendingResetBaselinesAt(tenantID, apiKeyIDs, LocalDayKeyAt(time.Now()))
}

func listDailySpendingResetBaselinesAt(tenantID string, apiKeyIDs []string, dayKey string) (map[string]float64, error) {
	out := make(map[string]float64)
	if len(apiKeyIDs) == 0 {
		return out, nil
	}
	db := getDB()
	if db == nil {
		return out, nil
	}
	tenantID = normalizeTenantID(tenantID)
	today := strings.TrimSpace(dayKey)
	ids := make([]string, 0, len(apiKeyIDs))
	seen := make(map[string]struct{}, len(apiKeyIDs))
	for _, id := range apiKeyIDs {
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
	args := make([]interface{}, 0, len(ids)+2)
	args = append(args, tenantID, today)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT api_key_id, cost_baseline FROM api_key_daily_spending_resets
		 WHERE tenant_id = ? AND day_key = ? AND api_key_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: list daily spending resets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var baseline float64
		if err := rows.Scan(&id, &baseline); err != nil {
			return nil, fmt.Errorf("usage: scan daily spending reset: %w", err)
		}
		out[strings.TrimSpace(id)] = baseline
	}
	return out, rows.Err()
}

// QueryRawTodayCostsByKeysForTenant batch-loads raw today costs for many keys (by id/key).
// Result is keyed by api_key_id when present, else by api key string.
func QueryRawTodayCostsByKeysForTenant(tenantID string, keys []APIKeyRow) (map[string]float64, error) {
	out := make(map[string]float64)
	if len(keys) == 0 {
		return out, nil
	}
	db := getReadDB()
	if db == nil {
		return out, nil
	}
	tenantID = normalizeTenantID(tenantID)
	ids := make([]string, 0, len(keys))
	idSet := make(map[string]struct{}, len(keys))
	idToKey := make(map[string]string, len(keys))
	for _, k := range keys {
		id := strings.TrimSpace(k.ID)
		if id == "" {
			if identity := ResolveAPIKeyIdentity(k.Key); identity != nil {
				id = identity.ID
			}
		}
		if id == "" {
			continue
		}
		if _, ok := idSet[id]; !ok {
			idSet[id] = struct{}{}
			ids = append(ids, id)
		}
		if key := strings.TrimSpace(k.Key); key != "" {
			idToKey[id] = key
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	dayKey := localDayKeyAt(time.Now())
	var b strings.Builder
	args := make([]interface{}, 0, 3+len(ids))
	b.WriteString(`SELECT api_key_id, COALESCE(SUM(cost_total), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start = ? AND api_key_id IN (`)
	args = append(args, tenantID, rollupBucketDay, dayKey)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteString(`) GROUP BY api_key_id`)

	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: batch raw today cost: %w", err)
	}
	defer rows.Close()

	// Map raw rows into lookup by id and by key string for callers.
	costByID := make(map[string]float64)
	costByKey := make(map[string]float64)
	// legacyCostByKey kept empty: rollup always uses stable api_key_id.
	legacyCostByKey := make(map[string]float64)
	for rows.Next() {
		var apiKeyID string
		var cost float64
		if err := rows.Scan(&apiKeyID, &cost); err != nil {
			return nil, fmt.Errorf("usage: scan batch raw today cost: %w", err)
		}
		apiKeyID = strings.TrimSpace(apiKeyID)
		apiKey := idToKey[apiKeyID]
		apiKey = strings.TrimSpace(apiKey)
		if apiKeyID != "" {
			costByID[apiKeyID] += cost
			continue
		}
		if apiKey != "" {
			legacyCostByKey[apiKey] += cost
			costByKey[apiKey] += cost
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Same key identity as buildSingleAPIKeySelectorClauseForTenant (api_key_id).
	// legacyCostByKey stays empty because rollup buckets are always keyed by id, so
	// the sum is the id branch alone.
	for _, k := range keys {
		id := strings.TrimSpace(k.ID)
		key := strings.TrimSpace(k.Key)
		if id != "" {
			out[id] = costByID[id] + legacyCostByKey[key]
			continue
		}
		if key != "" {
			out[key] = costByKey[key]
		}
	}
	return out, nil
}

// UpsertDailySpendingReset stores/replaces the same-day baseline for a key.
func UpsertDailySpendingReset(tenantID, apiKeyID string, costBaseline float64) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return fmt.Errorf("usage: api_key_id is required")
	}
	if costBaseline < 0 {
		costBaseline = 0
	}
	now := time.Now()
	dayKey := LocalDayKeyAt(now)
	_, err := db.Exec(
		`INSERT INTO api_key_daily_spending_resets (tenant_id, api_key_id, day_key, cost_baseline, reset_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, api_key_id) DO UPDATE SET
		   day_key = excluded.day_key,
		   cost_baseline = excluded.cost_baseline,
		   reset_at = excluded.reset_at`,
		tenantID, apiKeyID, dayKey, costBaseline, now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("usage: upsert daily spending reset: %w", err)
	}
	return nil
}

// DeleteDailySpendingReset removes the reset marker for a key (e.g. on key delete).
func DeleteDailySpendingReset(tenantID, apiKeyID string) error {
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return nil
	}
	_, err := db.Exec(
		`DELETE FROM api_key_daily_spending_resets WHERE tenant_id = ? AND api_key_id = ?`,
		tenantID, apiKeyID,
	)
	if err != nil {
		return fmt.Errorf("usage: delete daily spending reset: %w", err)
	}
	return nil
}
