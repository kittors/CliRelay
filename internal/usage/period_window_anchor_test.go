package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

func seedFiveHourAnchor(t *testing.T, tenantID, subjectType, subjectID, windowStart string) {
	t.Helper()
	db := getDB()
	if db == nil {
		t.Fatal("usage db not initialised")
	}
	if _, err := db.Exec(`INSERT INTO period_window_anchors
		(tenant_id, subject_type, subject_id, period, window_start, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, subject_type, subject_id, period) DO UPDATE SET
		 window_start = excluded.window_start`,
		tenantID, subjectType, subjectID, string(quota.PeriodFiveHour), windowStart,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed 5h anchor: %v", err)
	}
}

func readFiveHourAnchor(t *testing.T, tenantID, subjectType, subjectID string) string {
	t.Helper()
	var windowStart string
	err := getDB().QueryRow(`SELECT window_start FROM period_window_anchors
		WHERE tenant_id = ? AND subject_type = ? AND subject_id = ? AND period = ?`,
		tenantID, subjectType, subjectID, string(quota.PeriodFiveHour)).Scan(&windowStart)
	if err != nil {
		return ""
	}
	return windowStart
}

func recordBilledUsage(t *testing.T, tenantID, apiKeyID, endUserID string, cost float64, at time.Time) {
	t.Helper()
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := commitLogWithProjections(tx, rollupEvent{
		TenantID: tenantID, APIKeyID: apiKeyID, EndUserID: endUserID,
		Model: "test-model", Cost: cost, At: at,
	}); err != nil {
		t.Fatalf("commit usage: %v", err)
	}
}

// 首笔计费消费开窗；同一窗口内的后续消费不得推移窗口起点，否则窗口会被
// 持续续期，退化成永不结束的滚动窗口。
func TestFiveHourWindowAnchorOpensOnFirstBilledSpendAndHolds(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID := "anchor-key"
	first := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	recordBilledUsage(t, systemTenantID, keyID, "", 1, first)
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID); got != "2026-07-22T12:00" {
		t.Fatalf("anchor after first spend = %q, want 2026-07-22T12:00", got)
	}

	recordBilledUsage(t, systemTenantID, keyID, "", 1, first.Add(3*time.Hour))
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID); got != "2026-07-22T12:00" {
		t.Fatalf("anchor after in-window spend = %q, want it to stay at 2026-07-22T12:00", got)
	}
}

// 窗口过期后由下一笔消费重新开窗（间隔期不计时）。
func TestFiveHourWindowAnchorReopensAfterExpiry(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID := "reopen-key"
	first := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	recordBilledUsage(t, systemTenantID, keyID, "", 1, first)
	// 恰好 5 小时后窗口已过期，属于新窗口的第一笔消费。
	recordBilledUsage(t, systemTenantID, keyID, "", 1, first.Add(5*time.Hour))
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID); got != "2026-07-22T17:00" {
		t.Fatalf("anchor after expiry = %q, want 2026-07-22T17:00", got)
	}
}

// 5h 是美元额度，零成本请求不应提前吃掉窗口。
func TestFiveHourWindowAnchorIgnoresZeroCostRequests(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID := "free-key"
	at := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	recordBilledUsage(t, systemTenantID, keyID, "", 0, at)
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID); got != "" {
		t.Fatalf("anchor after free request = %q, want none", got)
	}

	recordBilledUsage(t, systemTenantID, keyID, "", 0.5, at.Add(time.Minute))
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID); got != "2026-07-22T12:01" {
		t.Fatalf("anchor after billed request = %q, want 2026-07-22T12:01", got)
	}
}

// 账号（end_user）与 Key 各自独立开窗。
func TestFiveHourWindowAnchorTracksKeyAndAccountSeparately(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID, userID := "dual-key", "dual-user"
	at := time.Date(2026, 7, 22, 9, 15, 0, 0, time.UTC)

	recordBilledUsage(t, systemTenantID, keyID, userID, 2, at)
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID); got != "2026-07-22T09:15" {
		t.Fatalf("key anchor = %q", got)
	}
	if got := readFiveHourAnchor(t, systemTenantID, periodResetSubjectEndUser, userID); got != "2026-07-22T09:15" {
		t.Fatalf("account anchor = %q", got)
	}
}

// 固定窗口的核心行为：窗口一到期，用量整体归零，而不是逐分钟滑出。
func TestFiveHourUsageResetsWhenWindowExpires(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID := "expiry-key"
	windowStart := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	recordBilledUsage(t, systemTenantID, keyID, "", 30, windowStart)
	recordBilledUsage(t, systemTenantID, keyID, "", 12, windowStart.Add(2*time.Hour))

	at := func(d time.Duration) quota.PeriodSpendingUsage {
		t.Helper()
		got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, windowStart.Add(d))
		if err != nil {
			t.Fatalf("query +%v: %v", d, err)
		}
		return got[keyID]
	}

	if used := at(4 * time.Hour); used.FiveHour != 42 {
		t.Fatalf("in-window used = %v, want 42", used.FiveHour)
	}
	// 4h59m 仍在窗口内：滚动窗口在这里会只剩 12，固定窗口必须仍是 42。
	if used := at(4*time.Hour + 59*time.Minute); used.FiveHour != 42 {
		t.Fatalf("late-window used = %v, want 42", used.FiveHour)
	}
	if used := at(5 * time.Hour); used.FiveHour != 0 || !used.FiveHourWindowStart.IsZero() {
		t.Fatalf("expired used = %+v, want 0 with no window", used)
	}
}

// 回归：5h 重置基线曾以滚动窗口下界作为窗口标识，导致重置在 1 分钟后失效。
// 锚定窗口内的标识恒定，重置必须在整个窗口内持续有效。
func TestPeriodSpendingResetFiveHourHoldsForWholeWindow(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID := "5h-reset-key"
	windowStart := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	resetAt := windowStart.Add(30 * time.Minute)

	recordBilledUsage(t, systemTenantID, keyID, "", 42, windowStart)
	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectAPIKey, keyID,
		[]quota.Period{quota.PeriodFiveHour}, PeriodSpendingResetActor{Kind: "test"}, resetAt); err != nil {
		t.Fatalf("reset 5h: %v", err)
	}

	usedAt := func(at time.Time) float64 {
		t.Helper()
		got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, at)
		if err != nil {
			t.Fatalf("query at %v: %v", at, err)
		}
		return got[keyID].FiveHour
	}

	for _, delta := range []time.Duration{0, time.Minute, 10 * time.Minute, 4 * time.Hour} {
		if got := usedAt(resetAt.Add(delta)); got != 0 {
			t.Fatalf("used at reset+%v = %v, want 0 (baseline must survive the whole window)", delta, got)
		}
	}

	// 重置后新增的消费仍要照常计入当前窗口。
	recordBilledUsage(t, systemTenantID, keyID, "", 5, resetAt.Add(time.Hour))
	if got := usedAt(resetAt.Add(2 * time.Hour)); got != 5 {
		t.Fatalf("post-reset spend = %v, want 5", got)
	}
}

// 窗口翻篇后旧基线必须失效，不能继续抵扣新窗口的消费。
func TestPeriodSpendingResetFiveHourExpiresWithWindow(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	keyID := "5h-reset-expiry-key"
	windowStart := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	recordBilledUsage(t, systemTenantID, keyID, "", 42, windowStart)
	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectAPIKey, keyID,
		[]quota.Period{quota.PeriodFiveHour}, PeriodSpendingResetActor{Kind: "test"}, windowStart.Add(time.Minute)); err != nil {
		t.Fatalf("reset 5h: %v", err)
	}

	// 窗口过期后的新消费开启新窗口，42 的旧基线不应再参与抵扣。
	next := windowStart.Add(6 * time.Hour)
	recordBilledUsage(t, systemTenantID, keyID, "", 7, next)
	got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, next.Add(time.Minute))
	if err != nil {
		t.Fatalf("query new window: %v", err)
	}
	if used := got[keyID]; used.FiveHour != 7 {
		t.Fatalf("new window used = %v, want 7", used.FiveHour)
	}
}

// 批量查询下每个 subject 用自己的窗口起点求和，不能互相串味。
func TestQueryFiveHourUsesPerSubjectAnchors(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	early, late, stale := "early-key", "late-key", "stale-key"

	// early 的窗口 [10:00,15:00) 覆盖两笔；late 的窗口 [13:00,18:00) 只覆盖第二笔。
	for _, keyID := range []string{early, late, stale} {
		recordBilledUsage(t, systemTenantID, keyID, "", 10, time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC))
		recordBilledUsage(t, systemTenantID, keyID, "", 4, time.Date(2026, 7, 22, 13, 30, 0, 0, time.UTC))
	}
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, early, "2026-07-22T10:00")
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, late, "2026-07-22T13:00")
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, stale, "2026-07-22T08:00") // 已过期

	got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{early, late, stale}, now)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got[early].FiveHour != 14 {
		t.Fatalf("early used = %v, want 14", got[early].FiveHour)
	}
	if got[late].FiveHour != 4 {
		t.Fatalf("late used = %v, want 4", got[late].FiveHour)
	}
	if got[stale].FiveHour != 0 {
		t.Fatalf("stale used = %v, want 0 (window expired)", got[stale].FiveHour)
	}
}
