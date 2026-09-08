package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

const (
	periodResetSubjectAPIKey  = "api_key"
	periodResetSubjectEndUser = "end_user"
)

const periodSpendingResetsTableSQL = `
CREATE TABLE IF NOT EXISTS period_spending_resets (
  tenant_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  subject_type  TEXT NOT NULL,
  subject_id    TEXT NOT NULL,
  period        TEXT NOT NULL,
  window_key    TEXT NOT NULL DEFAULT '',
  cost_baseline REAL NOT NULL DEFAULT 0,
  reset_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (tenant_id, subject_type, subject_id, period)
);
CREATE INDEX IF NOT EXISTS idx_period_spending_resets_window
  ON period_spending_resets(tenant_id, subject_type, period, window_key);
`

const periodSpendingResetEventsTableSQL = `
CREATE TABLE IF NOT EXISTS period_spending_reset_events (
  id                     INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  subject_type           TEXT NOT NULL,
  subject_id             TEXT NOT NULL,
  periods                TEXT NOT NULL DEFAULT '[]',
  window_keys            TEXT NOT NULL DEFAULT '{}',
  cost_baselines         TEXT NOT NULL DEFAULT '{}',
  effective_used_before  TEXT NOT NULL DEFAULT '{}',
  raw_costs              TEXT NOT NULL DEFAULT '{}',
  reset_at               TIMESTAMP NOT NULL,
  actor_user_id          TEXT NOT NULL DEFAULT '',
  actor_username         TEXT NOT NULL DEFAULT '',
  actor_kind             TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_period_spending_reset_events_subject
  ON period_spending_reset_events(tenant_id, subject_type, subject_id, reset_at DESC);
`

func bootstrapPeriodSpendingResets(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(periodSpendingResetsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure period_spending_resets: %w", err)
	}
	if _, err := db.Exec(periodSpendingResetEventsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure period_spending_reset_events: %w", err)
	}
	return ensurePeriodWindowAnchors(db)
}

type PeriodSpendingResetResult struct {
	Periods             []quota.Period            `json:"periods"`
	WindowKeys          map[quota.Period]string   `json:"window_keys"`
	CostBaselines       map[quota.Period]float64  `json:"cost_baselines"`
	EffectiveUsedBefore quota.PeriodSpendingUsage `json:"effective_used_before"`
	Raw                 quota.PeriodSpendingUsage `json:"raw"`
	EffectiveAfter      quota.PeriodSpendingUsage `json:"effective_after"`
}

type periodSpendingResetEvent struct {
	ID                  int64                     `json:"id"`
	TenantID            string                    `json:"tenant_id"`
	SubjectType         string                    `json:"subject_type"`
	SubjectID           string                    `json:"subject_id"`
	Periods             []quota.Period            `json:"periods"`
	WindowKeys          map[quota.Period]string   `json:"window_keys"`
	CostBaselines       map[quota.Period]float64  `json:"cost_baselines"`
	EffectiveUsedBefore quota.PeriodSpendingUsage `json:"effective_used_before"`
	Raw                 quota.PeriodSpendingUsage `json:"raw"`
	ResetAt             time.Time                 `json:"reset_at"`
	ActorUserID         string                    `json:"actor_user_id,omitempty"`
	ActorUsername       string                    `json:"actor_username,omitempty"`
	ActorKind           string                    `json:"actor_kind,omitempty"`
}

type PeriodSpendingResetActor struct {
	UserID   string
	Username string
	Kind     string
}

func periodResetSubjectType(subject periodSubject) (string, error) {
	switch subject {
	case periodSubjectAPIKey:
		return periodResetSubjectAPIKey, nil
	case periodSubjectEndUser:
		return periodResetSubjectEndUser, nil
	default:
		return "", fmt.Errorf("usage: invalid period reset subject %q", subject)
	}
}

// lifetimeWindowKey is the constant window key stored for cumulative-spend resets.
const lifetimeWindowKey = "lifetime"

func setPeriodUsageValue(used *quota.PeriodSpendingUsage, period quota.Period, value float64) {
	if value < 0 {
		value = 0
	}
	switch period {
	case quota.PeriodFiveHour:
		used.FiveHour = value
	case quota.PeriodDay:
		used.Day = value
	case quota.PeriodWeek:
		used.Week = value
	case quota.PeriodMonth:
		used.Month = value
	case quota.PeriodLifetime:
		used.Lifetime = value
	}
}

func applyPeriodBaseline(used *quota.PeriodSpendingUsage, period quota.Period, baseline float64) {
	setPeriodUsageValue(used, period, used.Value(period)-baseline)
}

func listPeriodSpendingResetBaselines(tenantID string, subject periodSubject, ids []string, windows subjectPeriodWindows) (map[string]map[quota.Period]float64, error) {
	out := make(map[string]map[quota.Period]float64)
	if len(ids) == 0 {
		return out, nil
	}
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	subjectType, err := periodResetSubjectType(subject)
	if err != nil {
		return nil, err
	}
	query := `SELECT subject_id, period, window_key, cost_baseline FROM period_spending_resets
		WHERE tenant_id = ? AND subject_type = ? AND subject_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+2)
	args = append(args, normalizeTenantID(tenantID), subjectType)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, rawPeriod, windowKey string
		var baseline float64
		if err := rows.Scan(&id, &rawPeriod, &windowKey, &baseline); err != nil {
			return nil, err
		}
		period := quota.Period(rawPeriod)
		// 窗口标识不一致说明基线属于已经翻篇的窗口，直接作废。5h 的标识是该
		// subject 的消费锚点，窗口内保持不变，跨窗口才失效。
		expected := windows.windowKey(id, period)
		if expected == "" || strings.TrimSpace(windowKey) != expected {
			continue
		}
		if out[id] == nil {
			out[id] = make(map[quota.Period]float64)
		}
		out[id][period] = baseline
	}
	return out, rows.Err()
}

func resetPeriodSpendingForSubject(tenantID string, subject periodSubject, subjectID string, periods []quota.Period, actor PeriodSpendingResetActor, now time.Time) (PeriodSpendingResetResult, error) {
	periods, err := quota.NormalizePeriods(periods)
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return PeriodSpendingResetResult{}, fmt.Errorf("usage: subject_id is required")
	}
	tenantID = normalizeTenantID(tenantID)
	windows, err := loadSubjectPeriodWindows(tenantID, subject, []string{subjectID}, now)
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	rawByID, err := queryRawPeriodSpendingForSubjects(tenantID, subject, []string{subjectID}, now, windows)
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	raw := rawByID[subjectID]
	baselinesByID, err := listEffectivePeriodBaselines(tenantID, subject, []string{subjectID}, windows)
	if err != nil {
		return PeriodSpendingResetResult{}, fmt.Errorf("%w: period baselines: %v", ErrQuotaUsageUnavailable, err)
	}
	before := raw
	for period, baseline := range baselinesByID[subjectID] {
		applyPeriodBaseline(&before, period, baseline)
	}
	after := before
	windowKeys := make(map[quota.Period]string, len(periods))
	costBaselines := make(map[quota.Period]float64, len(periods))
	subjectType, err := periodResetSubjectType(subject)
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	db := getDB()
	if db == nil {
		return PeriodSpendingResetResult{}, fmt.Errorf("usage: database not initialised")
	}
	tx, err := db.Begin()
	if err != nil {
		return PeriodSpendingResetResult{}, fmt.Errorf("usage: begin period reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	resetAt := now.UTC().Format(time.RFC3339Nano)
	for _, period := range periods {
		windowKey := windows.windowKey(subjectID, period)
		baseline := raw.Value(period)
		if _, err := tx.Exec(`
			INSERT INTO period_spending_resets
			 (tenant_id, subject_type, subject_id, period, window_key, cost_baseline, reset_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (tenant_id, subject_type, subject_id, period) DO UPDATE SET
			  window_key = excluded.window_key,
			  cost_baseline = excluded.cost_baseline,
			  reset_at = excluded.reset_at
		`, tenantID, subjectType, subjectID, string(period), windowKey, baseline, resetAt); err != nil {
			return PeriodSpendingResetResult{}, fmt.Errorf("usage: upsert %s period reset: %w", period, err)
		}
		windowKeys[period] = windowKey
		costBaselines[period] = baseline
		setPeriodUsageValue(&after, period, 0)
	}
	event := periodSpendingResetEvent{
		TenantID: tenantID, SubjectType: subjectType, SubjectID: subjectID,
		Periods: periods, WindowKeys: windowKeys, CostBaselines: costBaselines,
		EffectiveUsedBefore: before, Raw: raw, ResetAt: now,
		ActorUserID: actor.UserID, ActorUsername: actor.Username, ActorKind: actor.Kind,
	}
	if err := insertPeriodSpendingResetEventTx(tx, event); err != nil {
		return PeriodSpendingResetResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PeriodSpendingResetResult{}, fmt.Errorf("usage: commit period reset: %w", err)
	}
	return PeriodSpendingResetResult{
		Periods:             periods,
		WindowKeys:          windowKeys,
		CostBaselines:       costBaselines,
		EffectiveUsedBefore: before,
		Raw:                 raw,
		EffectiveAfter:      after,
	}, nil
}

func ResetPeriodSpendingByAPIKeyIDForTenant(tenantID, apiKeyID string, periods []quota.Period, actor PeriodSpendingResetActor) (PeriodSpendingResetResult, error) {
	return resetPeriodSpendingForSubject(tenantID, periodSubjectAPIKey, apiKeyID, periods, actor, time.Now())
}

func ResetPeriodSpendingByEndUserForTenant(tenantID, endUserID string, periods []quota.Period, actor PeriodSpendingResetActor) (PeriodSpendingResetResult, error) {
	return resetPeriodSpendingForSubject(tenantID, periodSubjectEndUser, endUserID, periods, actor, time.Now())
}

func insertPeriodSpendingResetEventTx(tx *sql.Tx, ev periodSpendingResetEvent) error {
	periods, err := quota.NormalizePeriods(ev.Periods)
	if err != nil {
		return err
	}
	if ev.SubjectType != periodResetSubjectAPIKey && ev.SubjectType != periodResetSubjectEndUser {
		return fmt.Errorf("usage: invalid period reset subject type %q", ev.SubjectType)
	}
	ev.SubjectID = strings.TrimSpace(ev.SubjectID)
	if ev.SubjectID == "" {
		return fmt.Errorf("usage: subject_id is required")
	}
	resetAt := ev.ResetAt
	if resetAt.IsZero() {
		resetAt = time.Now().UTC()
	}
	periodsJSON, err := json.Marshal(periods)
	if err != nil {
		return err
	}
	windowKeysJSON, err := json.Marshal(ev.WindowKeys)
	if err != nil {
		return err
	}
	baselinesJSON, err := json.Marshal(ev.CostBaselines)
	if err != nil {
		return err
	}
	beforeJSON, err := json.Marshal(ev.EffectiveUsedBefore)
	if err != nil {
		return err
	}
	rawJSON, err := json.Marshal(ev.Raw)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO period_spending_reset_events
		(tenant_id, subject_type, subject_id, periods, window_keys, cost_baselines,
		 effective_used_before, raw_costs, reset_at, actor_user_id, actor_username, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		normalizeTenantID(ev.TenantID), ev.SubjectType, ev.SubjectID, string(periodsJSON), string(windowKeysJSON),
		string(baselinesJSON), string(beforeJSON), string(rawJSON), resetAt.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(ev.ActorUserID), strings.TrimSpace(ev.ActorUsername), strings.TrimSpace(ev.ActorKind))
	if err != nil {
		return fmt.Errorf("usage: insert period spending reset event: %w", err)
	}
	return nil
}

func CountPeriodSpendingResetEvents(tenantID, subjectType, subjectID string) (int, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM period_spending_reset_events
		WHERE tenant_id = ? AND subject_type = ? AND subject_id = ?`,
		normalizeTenantID(tenantID), subjectType, strings.TrimSpace(subjectID)).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("usage: count period spending reset events: %w", err)
	}
	return count, nil
}

func CountAPIKeyPeriodSpendingResetEvents(tenantID, apiKeyID string) (int, error) {
	return CountPeriodSpendingResetEvents(tenantID, periodResetSubjectAPIKey, apiKeyID)
}

func CountEndUserPeriodSpendingResetEvents(tenantID, endUserID string) (int, error) {
	return CountPeriodSpendingResetEvents(tenantID, periodResetSubjectEndUser, endUserID)
}
