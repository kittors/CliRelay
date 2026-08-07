package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// QuotaWindowDTO is a typed quota window for AI account latest status.
type QuotaWindowDTO struct {
	QuotaKey      string     `json:"quota_key"`
	QuotaLabel    string     `json:"quota_label,omitempty"`
	Percent       *float64   `json:"percent,omitempty"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	WindowSeconds int64      `json:"window_seconds,omitempty"`
	Value         string     `json:"value,omitempty"`
	Meta          string     `json:"meta,omitempty"`
	// ObservedAt is when this specific window was last confirmed by the upstream.
	// Upstreams routinely return only a subset of windows per probe (codex answers
	// with just `rate_limit` or just `additional_rate_limits` in ~20% of probes), so
	// windows carried forward by a merge keep their original timestamp instead of
	// inheriting the probe time. Nil means "unknown" (pre-migration rows) and the
	// row-level QuotaObservedAt is used as the fallback.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// AIAccountStatusRecord is the persisted latest status for one tenant+auth_subject_id.
type AIAccountStatusRecord struct {
	TenantID               string
	AuthSubjectID          string
	AuthIndex              string
	Provider               string
	RefreshState           string
	HealthStatus           string
	PlanType               string
	RestrictionSummary     string
	ErrorSummary           string
	ErrorCode              string
	ErrorMessage           string
	Quotas                 []QuotaWindowDTO
	ResetCreditCount       *int64
	ResetCreditExpirations []string
	UpstreamCheckedAt      *time.Time
	UsageUpdatedAt         *time.Time
	ExpiresAt              *time.Time
	Version                int64
	UpdatedAt              time.Time
}

const aiAccountStatusTablesSQL = `
CREATE TABLE IF NOT EXISTS ai_account_status (
  tenant_id                 TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  auth_subject_id           TEXT NOT NULL,
  auth_index                TEXT NOT NULL DEFAULT '',
  provider                  TEXT NOT NULL DEFAULT '',
  refresh_state             TEXT NOT NULL DEFAULT 'idle',
  health_status             TEXT NOT NULL DEFAULT '',
  plan_type                 TEXT NOT NULL DEFAULT '',
  restriction_summary       TEXT NOT NULL DEFAULT '',
  error_summary             TEXT NOT NULL DEFAULT '',
  error_code                TEXT NOT NULL DEFAULT '',
  error_message             TEXT NOT NULL DEFAULT '',
  quota_json                TEXT NOT NULL DEFAULT '[]',
  reset_credit_count        INTEGER,
  reset_credit_expirations  TEXT NOT NULL DEFAULT '[]',
  upstream_checked_at       DATETIME,
  usage_updated_at          DATETIME,
  expires_at                DATETIME,
  version                   INTEGER NOT NULL DEFAULT 0,
  updated_at                DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, auth_subject_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_status_tenant_auth_index
  ON ai_account_status(tenant_id, auth_index);
CREATE INDEX IF NOT EXISTS idx_ai_account_status_tenant_refresh
  ON ai_account_status(tenant_id, refresh_state, updated_at);

CREATE TABLE IF NOT EXISTS auth_subject_usage_daily (
  tenant_id       TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  auth_subject_id TEXT NOT NULL,
  day_key         TEXT NOT NULL,
  request_count   INTEGER NOT NULL DEFAULT 0,
  success_count   INTEGER NOT NULL DEFAULT 0,
  failure_count   INTEGER NOT NULL DEFAULT 0,
  cost_total      REAL NOT NULL DEFAULT 0,
  updated_at      DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, auth_subject_id, day_key)
);
CREATE INDEX IF NOT EXISTS idx_auth_subject_usage_daily_tenant_day
  ON auth_subject_usage_daily(tenant_id, day_key);
`

func bootstrapAIAccountStatusReadModels(db *sql.DB, loc *time.Location) {
	ensureAIAccountStatusReadModels(db)
	if err := runAuthSubjectUsageDailyBackfillAtInitDB(db, authSubjectUsageDailyBackfillDays, loc); err != nil {
		log.Warnf("usage: auth subject usage daily backfill: %v", err)
	}
}

func ensureAIAccountStatusReadModels(db *sql.DB) {
	if db == nil {
		return
	}
	log.Debugf("usage: ensuring ai account status read models")
	if _, err := db.Exec(aiAccountStatusTablesSQL); err != nil {
		log.Warnf("usage: ensure ai account status tables: %v", err)
	}
	// Additive columns for older SQLite installs created mid-development.
	for _, stmt := range []string{
		`ALTER TABLE auth_subject_usage_daily ADD COLUMN success_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE auth_subject_usage_daily ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ai_account_status ADD COLUMN reset_credit_count INTEGER`,
		`ALTER TABLE ai_account_status ADD COLUMN reset_credit_expirations TEXT NOT NULL DEFAULT '[]'`,
	} {
		_, _ = db.Exec(stmt) // ignore duplicate-column on older SQLite installs
	}
}

func UpsertAIAccountStatus(record AIAccountStatusRecord) error {
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID := normalizeTenantID(record.TenantID)
	subjectID := strings.TrimSpace(record.AuthSubjectID)
	if subjectID == "" {
		return fmt.Errorf("usage: ai account status requires auth_subject_id")
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	quotaJSON, err := json.Marshal(record.Quotas)
	if err != nil {
		return fmt.Errorf("usage: marshal quota: %w", err)
	}
	if record.Quotas == nil {
		quotaJSON = []byte("[]")
	}
	expJSON, _ := json.Marshal(record.ResetCreditExpirations)
	if record.ResetCreditExpirations == nil {
		expJSON = []byte("[]")
	}
	refreshState := strings.TrimSpace(record.RefreshState)
	if refreshState == "" {
		refreshState = "idle"
	}
	var resetCount any
	if record.ResetCreditCount != nil {
		resetCount = *record.ResetCreditCount
	}

	_, err = db.Exec(`
		INSERT INTO ai_account_status (
			tenant_id, auth_subject_id, auth_index, provider, refresh_state, health_status, plan_type,
			restriction_summary, error_summary, error_code, error_message, quota_json,
			reset_credit_count, reset_credit_expirations,
			upstream_checked_at, usage_updated_at, expires_at, version, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, auth_subject_id) DO UPDATE SET
			auth_index = excluded.auth_index,
			provider = excluded.provider,
			refresh_state = excluded.refresh_state,
			health_status = excluded.health_status,
			plan_type = excluded.plan_type,
			restriction_summary = excluded.restriction_summary,
			error_summary = excluded.error_summary,
			error_code = excluded.error_code,
			error_message = excluded.error_message,
			quota_json = excluded.quota_json,
			reset_credit_count = excluded.reset_credit_count,
			reset_credit_expirations = excluded.reset_credit_expirations,
			upstream_checked_at = excluded.upstream_checked_at,
			usage_updated_at = excluded.usage_updated_at,
			expires_at = excluded.expires_at,
			version = ai_account_status.version + 1,
			updated_at = excluded.updated_at
	`,
		tenantID, subjectID, strings.TrimSpace(record.AuthIndex), strings.TrimSpace(record.Provider),
		refreshState, strings.TrimSpace(record.HealthStatus), strings.TrimSpace(record.PlanType),
		strings.TrimSpace(record.RestrictionSummary), strings.TrimSpace(record.ErrorSummary),
		strings.TrimSpace(record.ErrorCode), sanitizeErrorMessage(record.ErrorMessage), string(quotaJSON),
		resetCount, string(expJSON),
		nullableTime(record.UpstreamCheckedAt), nullableTime(record.UsageUpdatedAt), nullableTime(record.ExpiresAt),
		record.Version, record.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("usage: upsert ai account status: %w", err)
	}
	return nil
}

// UpdateAIAccountRefreshFailure records a failed upstream check while retaining
// the last successful quota/plan/reset-credit payload. A transient provider
// failure must not erase the latest usable account snapshot.
func UpdateAIAccountRefreshFailure(tenantID, authSubjectID, authIndex, provider, healthStatus, errorCode, errorMessage string, checkedAt time.Time) error {
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	authSubjectID = strings.TrimSpace(authSubjectID)
	if authSubjectID == "" {
		return fmt.Errorf("usage: refresh failure requires auth_subject_id")
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	checked := checkedAt.UTC().Format(time.RFC3339Nano)
	message := sanitizeErrorMessage(errorMessage)
	_, err := db.Exec(`
		INSERT INTO ai_account_status (
			tenant_id, auth_subject_id, auth_index, provider, refresh_state, health_status,
			error_summary, error_code, error_message, quota_json,
			reset_credit_expirations, upstream_checked_at, version, updated_at
		) VALUES (?, ?, ?, ?, 'error', ?, 'upstream probe failed', ?, ?, '[]', '[]', ?, 0, ?)
		ON CONFLICT(tenant_id, auth_subject_id) DO UPDATE SET
			auth_index = CASE WHEN excluded.auth_index <> '' THEN excluded.auth_index ELSE ai_account_status.auth_index END,
			provider = CASE WHEN excluded.provider <> '' THEN excluded.provider ELSE ai_account_status.provider END,
			refresh_state = 'error',
			health_status = CASE WHEN excluded.health_status <> '' THEN excluded.health_status ELSE ai_account_status.health_status END,
			error_summary = 'upstream probe failed',
			error_code = excluded.error_code,
			error_message = excluded.error_message,
			upstream_checked_at = excluded.upstream_checked_at,
			version = ai_account_status.version + 1,
			updated_at = excluded.updated_at
	`, tenantID, authSubjectID, strings.TrimSpace(authIndex), strings.TrimSpace(provider),
		strings.TrimSpace(healthStatus), strings.TrimSpace(errorCode), message, checked, checked)
	if err != nil {
		return fmt.Errorf("usage: update ai account refresh failure: %w", err)
	}
	return nil
}

// UpdateAIAccountRefreshState patches only refresh lifecycle fields without wiping quotas/plan.
func UpdateAIAccountRefreshState(tenantID, authSubjectID, authIndex, provider, refreshState, healthStatus, errorCode, errorMessage string) error {
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	authSubjectID = strings.TrimSpace(authSubjectID)
	if authSubjectID == "" {
		return fmt.Errorf("usage: refresh state requires auth_subject_id")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	refreshState = strings.TrimSpace(refreshState)
	if refreshState == "" {
		refreshState = "idle"
	}
	// Insert shell row if missing, otherwise patch only lifecycle columns.
	_, err := db.Exec(`
		INSERT INTO ai_account_status (
			tenant_id, auth_subject_id, auth_index, provider, refresh_state, health_status,
			error_code, error_message, quota_json, reset_credit_expirations, version, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', 0, ?)
		ON CONFLICT(tenant_id, auth_subject_id) DO UPDATE SET
			auth_index = CASE WHEN excluded.auth_index <> '' THEN excluded.auth_index ELSE ai_account_status.auth_index END,
			provider = CASE WHEN excluded.provider <> '' THEN excluded.provider ELSE ai_account_status.provider END,
			refresh_state = excluded.refresh_state,
			health_status = CASE WHEN excluded.health_status <> '' THEN excluded.health_status ELSE ai_account_status.health_status END,
			error_code = excluded.error_code,
			error_message = excluded.error_message,
			version = ai_account_status.version + 1,
			updated_at = excluded.updated_at
	`, tenantID, authSubjectID, strings.TrimSpace(authIndex), strings.TrimSpace(provider),
		refreshState, strings.TrimSpace(healthStatus), strings.TrimSpace(errorCode),
		sanitizeErrorMessage(errorMessage), now)
	if err != nil {
		return fmt.Errorf("usage: update refresh state: %w", err)
	}
	return nil
}

// NormalizeStaleAIAccountRefreshStates flips stuck queued/running rows past threshold.
func NormalizeStaleAIAccountRefreshStates(tenantID string, olderThan time.Duration) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	if olderThan <= 0 {
		olderThan = 15 * time.Minute
	}
	tenantID = normalizeTenantID(tenantID)
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(`
		UPDATE ai_account_status
		SET refresh_state = 'error',
		    error_code = CASE WHEN trim(coalesce(error_code,'')) = '' THEN 'stale_refresh' ELSE error_code END,
		    error_message = CASE WHEN trim(coalesce(error_message,'')) = '' THEN 'refresh interrupted' ELSE error_message END,
		    version = version + 1,
		    updated_at = ?
		WHERE tenant_id = ?
		  AND refresh_state IN ('queued','running')
		  AND updated_at < ?
	`, now, tenantID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("usage: normalize stale refresh: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func ListAIAccountStatusForTenant(tenantID string, authSubjectIDs []string) ([]AIAccountStatusRecord, error) {
	// Pure SELECT: use read pool so management status does not queue behind the single writer.
	db := getReadDB()
	if db == nil {
		return []AIAccountStatusRecord{}, nil
	}
	tenantID = normalizeTenantID(tenantID)
	query := `
		SELECT tenant_id, auth_subject_id, auth_index, provider, refresh_state, health_status, plan_type,
			restriction_summary, error_summary, error_code, error_message, quota_json,
			reset_credit_count, reset_credit_expirations,
			upstream_checked_at, usage_updated_at, expires_at, version, updated_at
		FROM ai_account_status
		WHERE tenant_id = ?
	`
	args := []any{tenantID}
	if ids := dedupeExactStrings(authSubjectIDs); len(ids) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		query += " AND auth_subject_id IN (" + placeholders + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += " ORDER BY auth_index ASC, auth_subject_id ASC"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: list ai account status: %w", err)
	}
	defer rows.Close()

	out := make([]AIAccountStatusRecord, 0)
	for rows.Next() {
		var rec AIAccountStatusRecord
		var quotaJSON, expJSON string
		var resetCount sql.NullInt64
		var upstream, usageUpdated, expires, updated sql.NullString
		if err := rows.Scan(
			&rec.TenantID, &rec.AuthSubjectID, &rec.AuthIndex, &rec.Provider, &rec.RefreshState,
			&rec.HealthStatus, &rec.PlanType, &rec.RestrictionSummary, &rec.ErrorSummary,
			&rec.ErrorCode, &rec.ErrorMessage, &quotaJSON, &resetCount, &expJSON,
			&upstream, &usageUpdated, &expires, &rec.Version, &updated,
		); err != nil {
			return nil, fmt.Errorf("usage: scan ai account status: %w", err)
		}
		rec.Quotas = decodeQuotaWindows(quotaJSON)
		if resetCount.Valid {
			v := resetCount.Int64
			rec.ResetCreditCount = &v
		}
		rec.ResetCreditExpirations = decodeStringSlice(expJSON)
		rec.UpstreamCheckedAt = parseNullableTime(upstream)
		rec.UsageUpdatedAt = parseNullableTime(usageUpdated)
		rec.ExpiresAt = parseNullableTime(expires)
		if t, ok := parseStoredTimeString(updated.String); ok {
			rec.UpdatedAt = t
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// QueryLatestWeeklyQuotaCyclesBatch loads cycle starts for many subjects in one query.
func QueryLatestWeeklyQuotaCyclesBatch(tenantID string, subjectIDs []string, preferredKeys []string) (map[string]time.Time, error) {
	// Pure SELECT: prefer read pool to avoid serializing behind SetMaxOpenConns(1) writer.
	db := getReadDB()
	if db == nil {
		return map[string]time.Time{}, nil
	}
	tenantID = normalizeTenantID(tenantID)
	ids := dedupeExactStrings(subjectIDs)
	if len(ids) == 0 {
		return map[string]time.Time{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{tenantID}
	for _, id := range ids {
		args = append(args, id)
	}
	query := `
		SELECT subject_id, quota_key, cycle_start_at, last_verified_at, window_seconds
		FROM auth_subject_quota_cycles
		WHERE tenant_id = ?
		  AND subject_id IN (` + placeholders + `)
		  AND window_seconds >= 604800
	`
	if keys := dedupeExactStrings(preferredKeys); len(keys) > 0 {
		kp := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
		query += " AND quota_key IN (" + kp + ")"
		for _, k := range keys {
			args = append(args, k)
		}
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: batch quota cycles: %w", err)
	}
	defer rows.Close()

	type cand struct {
		start    time.Time
		verified time.Time
	}
	best := make(map[string]cand)
	for rows.Next() {
		var subject, quotaKey, startStr, verifiedStr string
		var window int64
		if err := rows.Scan(&subject, &quotaKey, &startStr, &verifiedStr, &window); err != nil {
			return nil, err
		}
		start, ok1 := parseStoredTimeString(startStr)
		verified, ok2 := parseStoredTimeString(verifiedStr)
		if !ok1 {
			continue
		}
		if !ok2 {
			verified = start
		}
		cur, ok := best[subject]
		if !ok || verified.After(cur.verified) {
			best[subject] = cand{start: start, verified: verified}
		}
	}
	out := make(map[string]time.Time, len(best))
	for sid, c := range best {
		out[sid] = c.start.UTC()
	}
	return out, rows.Err()
}

func decodeQuotaWindows(raw string) []QuotaWindowDTO {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []QuotaWindowDTO{}
	}
	var quotas []QuotaWindowDTO
	if err := json.Unmarshal([]byte(raw), &quotas); err != nil {
		return []QuotaWindowDTO{}
	}
	if quotas == nil {
		return []QuotaWindowDTO{}
	}
	return quotas
}

func decodeStringSlice(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseNullableTime(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t, ok := parseStoredTimeString(v.String)
	if !ok {
		return nil
	}
	return &t
}

func sanitizeErrorMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") {
		return "upstream request failed"
	}
	if len(msg) > 240 {
		return msg[:240]
	}
	return msg
}
