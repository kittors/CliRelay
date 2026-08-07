package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// quotaWindowRetention bounds how long a window that the upstream stopped
// reporting is carried forward. Upstreams answer with a partial window set on a
// large minority of probes, so a window missing from one payload must not be
// dropped; a window missing for a full day is genuinely gone (plan change,
// product retired) and is removed on the next merge.
const quotaWindowRetention = 24 * time.Hour

// AIAccountSubjectStatusRecord is the shared, cross-tenant latest status for one
// physical AI account.
type AIAccountSubjectStatusRecord struct {
	AuthSubjectID          string
	Provider               string
	LastProbeState         string
	HealthStatus           string
	PlanType               string
	SubscriptionStartedAt  *time.Time
	SubscriptionExpiresAt  *time.Time
	SubscriptionSource     string
	RestrictionSummary     string
	ErrorCode              string
	ErrorSummary           string
	Quotas                 []QuotaWindowDTO
	ResetCreditCount       *int64
	ResetCreditExpirations []string
	// UpstreamCheckedAt is the last probe *attempt* and moves even when the probe
	// failed. QuotaObservedAt is the last time the quota payload itself was
	// confirmed by the upstream. Keeping them apart is what lets the UI tell
	// "checked a second ago, data from six days ago" from genuinely fresh data.
	UpstreamCheckedAt *time.Time
	QuotaObservedAt   *time.Time
	Version           int64
	UpdatedAt         time.Time
}

// mergeQuotaWindows folds a probe payload into the stored window set.
//
// The upstream is not required to report every window on every probe — codex
// answers with only `rate_limit` or only `additional_rate_limits` in roughly a
// fifth of probes. Replacing the whole set with the payload therefore made
// windows disappear and reappear between refreshes. Each window instead keeps
// its own ObservedAt: windows present in the payload are refreshed and stamped,
// windows absent from it are carried forward untouched so their staleness stays
// visible, and windows unconfirmed past quotaWindowRetention are dropped.
//
// Previous ordering is preserved so cards do not reshuffle when the upstream
// varies the order it reports windows in.
func mergeQuotaWindows(previous, incoming []QuotaWindowDTO, observedAt time.Time) []QuotaWindowDTO {
	observedAt = observedAt.UTC()
	incomingByKey := make(map[string]QuotaWindowDTO, len(incoming))
	incomingOrder := make([]string, 0, len(incoming))
	for _, window := range incoming {
		key := strings.TrimSpace(window.QuotaKey)
		if key == "" {
			continue
		}
		window.QuotaKey = key
		stamp := observedAt
		window.ObservedAt = &stamp
		if _, seen := incomingByKey[key]; !seen {
			incomingOrder = append(incomingOrder, key)
		}
		incomingByKey[key] = window
	}

	merged := make([]QuotaWindowDTO, 0, len(previous)+len(incomingOrder))
	consumed := make(map[string]struct{}, len(incomingByKey))
	for _, window := range previous {
		key := strings.TrimSpace(window.QuotaKey)
		if key == "" {
			continue
		}
		if fresh, ok := incomingByKey[key]; ok {
			merged = append(merged, fresh)
			consumed[key] = struct{}{}
			continue
		}
		if _, duplicate := consumed[key]; duplicate {
			continue
		}
		consumed[key] = struct{}{}
		// Carried-forward window: keep the value and its original timestamp so the
		// UI can show how old it is, unless it has gone unconfirmed for too long.
		if window.ObservedAt == nil || observedAt.Sub(window.ObservedAt.UTC()) <= quotaWindowRetention {
			merged = append(merged, window)
		}
	}
	for _, key := range incomingOrder {
		if _, ok := consumed[key]; ok {
			continue
		}
		consumed[key] = struct{}{}
		merged = append(merged, incomingByKey[key])
	}
	return merged
}

// applyQuotaObservedFallback fills per-window ObservedAt for rows written before
// quota freshness tracking existed, so migrated data reports one coherent age
// instead of "unknown".
func applyQuotaObservedFallback(quotas []QuotaWindowDTO, rowObserved *time.Time) []QuotaWindowDTO {
	if rowObserved == nil {
		return quotas
	}
	for i := range quotas {
		if quotas[i].ObservedAt == nil {
			stamp := rowObserved.UTC()
			quotas[i].ObservedAt = &stamp
		}
	}
	return quotas
}

// latestQuotaObservedAt reports the newest per-window observation, used as the
// row-level summary the API exposes.
func latestQuotaObservedAt(quotas []QuotaWindowDTO, fallback *time.Time) *time.Time {
	var newest *time.Time
	for i := range quotas {
		at := quotas[i].ObservedAt
		if at == nil {
			continue
		}
		if newest == nil || at.After(*newest) {
			value := at.UTC()
			newest = &value
		}
	}
	if newest == nil {
		return fallback
	}
	return newest
}

func loadSubjectQuotaState(db *sql.DB, subjectID string) ([]QuotaWindowDTO, *time.Time) {
	if db == nil {
		return nil, nil
	}
	var quotaJSON string
	var observed sql.NullString
	err := db.QueryRow(`
		SELECT quota_json, quota_observed_at FROM ai_account_subject_status WHERE auth_subject_id = ?
	`, subjectID).Scan(&quotaJSON, &observed)
	if err != nil {
		return nil, nil
	}
	rowObserved := parseNullableTime(observed)
	return applyQuotaObservedFallback(decodeQuotaWindows(quotaJSON), rowObserved), rowObserved
}

// UpsertAIAccountSubjectStatus persists a successful probe result.
//
// Quota windows are merged rather than replaced (see mergeQuotaWindows). The
// read-modify-write is safe because status refresh is serialized per subject by
// the singleflight group in the aiaccountstatus service — one physical account
// is never probed concurrently, even across tenants.
func UpsertAIAccountSubjectStatus(record AIAccountSubjectStatusRecord) error {
	db := getDB()
	if db == nil || strings.TrimSpace(record.AuthSubjectID) == "" {
		return nil
	}
	if record.LastProbeState == "" {
		record.LastProbeState = "idle"
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	if record.Version < 1 {
		record.Version = 1
	}

	observedAt := record.UpdatedAt.UTC()
	if record.UpstreamCheckedAt != nil && !record.UpstreamCheckedAt.IsZero() {
		observedAt = record.UpstreamCheckedAt.UTC()
	}
	previous, previousObserved := loadSubjectQuotaState(db, record.AuthSubjectID)
	merged := mergeQuotaWindows(previous, record.Quotas, observedAt)
	// An empty payload confirms nothing: carried-forward windows keep their old
	// timestamps and the row-level marker stays put, so the UI reports the data as
	// aging instead of silently presenting it as just-refreshed.
	quotaObserved := previousObserved
	if len(record.Quotas) > 0 {
		quotaObserved = &observedAt
	}
	record.Quotas = merged
	record.QuotaObservedAt = quotaObserved

	quotaJSON, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	expJSON, err := json.Marshal(record.ResetCreditExpirations)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO ai_account_subject_status (
			auth_subject_id, provider, last_probe_state, health_status, plan_type,
			subscription_started_at, subscription_expires_at, subscription_source,
			restriction_summary, error_code, error_summary, quota_json,
			reset_credit_count, reset_credit_expirations, upstream_checked_at,
			quota_observed_at, version, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id) DO UPDATE SET
			provider = excluded.provider,
			last_probe_state = excluded.last_probe_state,
			health_status = excluded.health_status,
			plan_type = excluded.plan_type,
			subscription_started_at = CASE WHEN excluded.subscription_source <> ''
				THEN excluded.subscription_started_at ELSE ai_account_subject_status.subscription_started_at END,
			subscription_expires_at = CASE WHEN excluded.subscription_source <> ''
				THEN excluded.subscription_expires_at ELSE ai_account_subject_status.subscription_expires_at END,
			subscription_source = CASE WHEN excluded.subscription_source <> ''
				THEN excluded.subscription_source ELSE ai_account_subject_status.subscription_source END,
			restriction_summary = excluded.restriction_summary,
			error_code = excluded.error_code,
			error_summary = excluded.error_summary,
			quota_json = excluded.quota_json,
			reset_credit_count = excluded.reset_credit_count,
			reset_credit_expirations = excluded.reset_credit_expirations,
			upstream_checked_at = excluded.upstream_checked_at,
			quota_observed_at = excluded.quota_observed_at,
			version = CASE WHEN excluded.version > ai_account_subject_status.version
				THEN excluded.version ELSE ai_account_subject_status.version + 1 END,
			updated_at = excluded.updated_at
	`, record.AuthSubjectID, record.Provider, record.LastProbeState, record.HealthStatus, record.PlanType,
		nullableTimeValue(record.SubscriptionStartedAt), nullableTimeValue(record.SubscriptionExpiresAt), record.SubscriptionSource,
		record.RestrictionSummary, record.ErrorCode, record.ErrorSummary, string(quotaJSON), record.ResetCreditCount,
		string(expJSON), nullableTimeValue(record.UpstreamCheckedAt), nullableTimeValue(record.QuotaObservedAt),
		record.Version, record.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("usage: upsert shared ai account status: %w", err)
	}
	return nil
}

// UpdateAIAccountSubjectProbeFailure records a failed probe.
//
// quota_json and quota_observed_at are deliberately left untouched: the last
// known quota is still the best answer available, but it must not be presented
// as fresh. upstream_checked_at moves (the attempt did happen) while
// quota_observed_at stays behind, which is exactly the gap the UI surfaces.
func UpdateAIAccountSubjectProbeFailure(subjectID, provider, errorCode, errorSummary string, checked time.Time) error {
	db := getDB()
	if db == nil || strings.TrimSpace(subjectID) == "" {
		return nil
	}
	if checked.IsZero() {
		checked = time.Now().UTC()
	}
	_, err := db.Exec(`
		INSERT INTO ai_account_subject_status (
			auth_subject_id, provider, last_probe_state, error_code, error_summary,
			upstream_checked_at, version, updated_at
		) VALUES (?, ?, 'error', ?, ?, ?, 1, ?)
		ON CONFLICT(auth_subject_id) DO UPDATE SET
			provider = excluded.provider,
			last_probe_state = 'error',
			error_code = excluded.error_code,
			error_summary = excluded.error_summary,
			upstream_checked_at = excluded.upstream_checked_at,
			version = ai_account_subject_status.version + 1,
			updated_at = excluded.updated_at
	`, subjectID, provider, strings.TrimSpace(errorCode), sanitizeSharedStatusError(errorSummary),
		checked.UTC().Format(time.RFC3339Nano), checked.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("usage: update shared ai account failure: %w", err)
	}
	return nil
}

func sanitizeSharedStatusError(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "timeout"), strings.Contains(value, "deadline"):
		return "upstream status probe timed out"
	case strings.Contains(value, "unauthorized"), strings.Contains(value, "http 401"):
		return "upstream authorization failed"
	case strings.Contains(value, "forbidden"), strings.Contains(value, "http 403"):
		return "upstream access was denied"
	default:
		return "upstream status probe failed"
	}
}

func ListAIAccountSubjectStatus(subjectIDs []string) ([]AIAccountSubjectStatusRecord, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	if db == nil || len(ids) == 0 {
		return []AIAccountSubjectStatusRecord{}, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, last_probe_state, health_status, plan_type,
			subscription_started_at, subscription_expires_at, subscription_source,
			restriction_summary, error_code, error_summary, quota_json,
			reset_credit_count, reset_credit_expirations, upstream_checked_at,
			quota_observed_at, version, updated_at
		FROM ai_account_subject_status
		WHERE auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: list shared ai account status: %w", err)
	}
	defer rows.Close()
	out := make([]AIAccountSubjectStatusRecord, 0, len(ids))
	for rows.Next() {
		var row AIAccountSubjectStatusRecord
		var started, expires, checked, observed, updated sql.NullString
		var reset sql.NullInt64
		var quotaJSON, expJSON string
		if err := rows.Scan(&row.AuthSubjectID, &row.Provider, &row.LastProbeState, &row.HealthStatus, &row.PlanType,
			&started, &expires, &row.SubscriptionSource, &row.RestrictionSummary, &row.ErrorCode, &row.ErrorSummary,
			&quotaJSON, &reset, &expJSON, &checked, &observed, &row.Version, &updated); err != nil {
			return nil, err
		}
		row.SubscriptionStartedAt = parseNullableTime(started)
		row.SubscriptionExpiresAt = parseNullableTime(expires)
		row.UpstreamCheckedAt = parseNullableTime(checked)
		rowObserved := parseNullableTime(observed)
		row.Quotas = applyQuotaObservedFallback(decodeQuotaWindows(quotaJSON), rowObserved)
		row.QuotaObservedAt = latestQuotaObservedAt(row.Quotas, rowObserved)
		row.ResetCreditExpirations = decodeStringSlice(expJSON)
		if reset.Valid {
			v := reset.Int64
			row.ResetCreditCount = &v
		}
		if t, ok := parseStoredTimeString(updated.String); ok {
			row.UpdatedAt = t
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// backfillSQLiteQuotaObservedAt dates existing quota payloads on SQLite.
//
// ai_account_subject_quota_points is only written after a successful probe, so
// its newest row is the true last-observation time — including for accounts that
// have been failing for days and whose upstream_checked_at has long since moved
// past the data it describes.
func backfillSQLiteQuotaObservedAt(db *sql.DB) {
	if db == nil {
		return
	}
	_, _ = db.Exec(`
		UPDATE ai_account_subject_status
		SET quota_observed_at = COALESCE(
			(SELECT MAX(p.recorded_at) FROM ai_account_subject_quota_points p
			 WHERE p.auth_subject_id = ai_account_subject_status.auth_subject_id),
			CASE WHEN last_probe_state = 'success' THEN upstream_checked_at END
		)
		WHERE quota_observed_at IS NULL AND quota_json <> '[]'
	`)
}
