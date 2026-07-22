package usage

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func setupEndUserAccountUsageTestDB(t *testing.T) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "enduser-account-usage-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if _, err := getDB().Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			display_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active'
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := EnsureEndUserQuotaColumns(getDB()); err != nil {
		t.Fatalf("EnsureEndUserQuotaColumns: %v", err)
	}
}

func insertEndUserAccountLog(t *testing.T, tenantID, apiKey, apiKeyID, apiKeyName string, cost float64) {
	t.Helper()
	at := CutoffStartUTC(1).Add(time.Hour)
	_, err := getDB().Exec(`
		INSERT INTO request_logs (
			tenant_id, timestamp, api_key, api_key_id, api_key_name, model, source,
			failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens,
			reasoning_tokens, cached_tokens, total_tokens, cost
		) VALUES (?, ?, ?, ?, ?, 'gpt-test', 'test', 0, 0, 1, 0, 1, 1, 0, 0, 2, ?)
	`, tenantID, at.Format(time.RFC3339), apiKey, apiKeyID, apiKeyName, cost)
	if err != nil {
		t.Fatalf("insert request log: %v", err)
	}
	endUserID := ""
	if row := GetAPIKey(apiKey); row != nil {
		endUserID = strings.TrimSpace(row.EndUserID)
	}
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := projectUsageRollupTx(tx, rollupEvent{
		TenantID:  tenantID,
		APIKeyID:  apiKeyID,
		EndUserID: endUserID,
		Model:     "gpt-test",
		Source:    "test",
		Tokens:    TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		Cost:      cost,
		At:        at,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("project: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestEndUserAccountUsageAggregatesBusinessTenantKeysAndSurvivesRotate(t *testing.T) {
	setupEndUserAccountUsageTestDB(t)

	tenantID := uuid.NewString()
	otherTenantID := uuid.NewString()
	endUserID := uuid.NewString()
	otherUserID := uuid.NewString()
	keyAID := uuid.NewString()
	keyBID := uuid.NewString()
	otherKeyID := uuid.NewString()
	crossTenantKeyID := uuid.NewString()
	standaloneKeyID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := getDB().Exec(`INSERT INTO end_users (id, tenant_id, display_name) VALUES (?, ?, ?)`, endUserID, tenantID, "Zhang Bolun"); err != nil {
		t.Fatalf("insert end user: %v", err)
	}
	for _, row := range []APIKeyRow{
		{ID: keyAID, Key: "sk-business-a", Name: "Automation", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
		{ID: keyBID, Key: "sk-business-b", Name: "Laptop", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
		{ID: otherKeyID, Key: "sk-business-other", Name: "Other", EndUserID: otherUserID, CreatedAt: now, UpdatedAt: now},
		{ID: standaloneKeyID, Key: "sk-business-standalone", Name: "Standalone", CreatedAt: now, UpdatedAt: now},
	} {
		if err := UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Key, err)
		}
	}
	if err := UpsertAPIKeyForTenant(otherTenantID, APIKeyRow{
		ID: crossTenantKeyID, Key: "sk-business-cross-tenant", Name: "Cross tenant", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertAPIKeyForTenant(cross tenant): %v", err)
	}

	if row := GetAPIKey("sk-business-a"); row == nil || row.TenantID != tenantID || row.ID != keyAID {
		t.Fatalf("GetAPIKey = %#v, want business-tenant key A", row)
	}
	if row := GetAPIKeyByID(keyBID); row == nil || row.TenantID != tenantID || row.Key != "sk-business-b" {
		t.Fatalf("GetAPIKeyByID = %#v, want business-tenant key B", row)
	}
	expanded := ExpandPublicLookupAPIKeys("sk-business-a")
	if len(expanded) != 1 || expanded[0] != "sk-business-a" {
		t.Fatalf("ExpandPublicLookupAPIKeys = %v, want [sk-business-a] only", expanded)
	}

	// Modern rows match stable key ids; the legacy row matches the current raw secret.
	insertEndUserAccountLog(t, tenantID, "sk-business-a", keyAID, "old snapshot", 1)
	insertEndUserAccountLog(t, tenantID, "sk-business-b", keyBID, "old snapshot", 2)
	insertEndUserAccountLog(t, tenantID, "sk-business-b", "", "legacy snapshot", 3)
	insertEndUserAccountLog(t, tenantID, "sk-business-other", otherKeyID, "other", 50)
	insertEndUserAccountLog(t, tenantID, "sk-business-standalone", standaloneKeyID, "standalone", 60)
	insertEndUserAccountLog(t, otherTenantID, "sk-business-cross-tenant", crossTenantKeyID, "cross tenant", 70)

	params := LogQueryParams{TenantID: tenantID, EndUserID: endUserID, Page: 1, Size: 20, Days: 1}
	result, err := QueryLogs(params)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if result.Total != 3 || len(result.Items) != 3 {
		t.Fatalf("account logs total/items = %d/%d, want 3/3", result.Total, len(result.Items))
	}
	for _, item := range result.Items {
		if item.EndUserDisplayName != "Zhang Bolun" {
			t.Fatalf("end_user_display_name = %q, want Zhang Bolun", item.EndUserDisplayName)
		}
		if item.APIKeyOwnName != "Automation" && item.APIKeyOwnName != "Laptop" {
			t.Fatalf("api_key_own_name = %q, want live key own name", item.APIKeyOwnName)
		}
	}
	stats, err := QueryStats(params)
	if err != nil {
		t.Fatalf("QueryStats: %v", err)
	}
	if stats.Total != 3 || stats.TotalCost != 6 {
		t.Fatalf("account stats = %+v, want total=3 cost=6", stats)
	}
	chart, err := QueryPublicChartData("sk-business-a", 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData: %v", err)
	}
	if chart.Stats.Total != 3 || chart.Stats.TotalCost != 6 {
		t.Fatalf("public chart stats = %+v, want total=3 cost=6", chart.Stats)
	}
	distributionByID := make(map[string]PublicAPIKeyDistributionPoint, len(chart.APIKeyDistribution))
	for _, point := range chart.APIKeyDistribution {
		distributionByID[point.APIKeyID] = point
	}
	if len(distributionByID) != 2 {
		t.Fatalf("api key distribution = %+v, want exactly key A and key B", chart.APIKeyDistribution)
	}
	if point := distributionByID[keyAID]; point.Name != "Automation" || point.Requests != 1 || point.Tokens != 2 {
		t.Fatalf("key A distribution = %+v, want own name and requests/tokens 1/2", point)
	}
	if point := distributionByID[keyBID]; point.Name != "Laptop" || point.Requests != 1 || point.Tokens != 2 {
		t.Fatalf("key B distribution = %+v, want stable-id rollup only and requests/tokens 1/2", point)
	}
	if _, exists := distributionByID[otherKeyID]; exists {
		t.Fatalf("api key distribution included other end user: %+v", chart.APIKeyDistribution)
	}
	if _, exists := distributionByID[crossTenantKeyID]; exists {
		t.Fatalf("api key distribution included other tenant: %+v", chart.APIKeyDistribution)
	}
	if _, exists := distributionByID[standaloneKeyID]; exists {
		t.Fatalf("api key distribution included standalone key: %+v", chart.APIKeyDistribution)
	}

	emptyChart, err := QueryPublicChartDataForEndUser(tenantID, uuid.NewString(), 7)
	if err != nil {
		t.Fatalf("QueryPublicChartDataForEndUser(empty): %v", err)
	}
	if emptyChart.APIKeyDistribution == nil || len(emptyChart.APIKeyDistribution) != 0 {
		t.Fatalf("empty api key distribution = %#v, want non-nil []", emptyChart.APIKeyDistribution)
	}
	standaloneChart, err := QueryPublicChartData("sk-business-standalone", 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData(standalone): %v", err)
	}
	if standaloneChart.APIKeyDistribution == nil || len(standaloneChart.APIKeyDistribution) != 0 {
		t.Fatalf("standalone api key distribution = %#v, want non-nil []", standaloneChart.APIKeyDistribution)
	}

	// Rotate changes only the secret. Stable-id rows remain in the account selector.
	if _, err := getDB().Exec(`UPDATE api_keys SET key = ?, name = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`, "sk-business-a-rotated", "Automation Renamed", now, tenantID, keyAID); err != nil {
		t.Fatalf("rotate key A: %v", err)
	}
	if row := GetAPIKey("sk-business-a-rotated"); row == nil || row.ID != keyAID {
		t.Fatalf("rotated lookup = %#v, want stable id %s", row, keyAID)
	}
	rotatedResult, err := QueryLogs(params)
	if err != nil {
		t.Fatalf("QueryLogs after rotate: %v", err)
	}
	if rotatedResult.Total != 3 {
		t.Fatalf("account logs after rotate = %d, want 3", rotatedResult.Total)
	}
	rotatedChart, err := QueryPublicChartData("sk-business-a-rotated", 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData after rotate: %v", err)
	}
	if rotatedChart.Stats.Total != 3 || rotatedChart.Stats.TotalCost != 6 {
		t.Fatalf("chart after rotate = %+v, want total=3 cost=6", rotatedChart.Stats)
	}
	rotatedDistributionByID := make(map[string]PublicAPIKeyDistributionPoint, len(rotatedChart.APIKeyDistribution))
	for _, point := range rotatedChart.APIKeyDistribution {
		rotatedDistributionByID[point.APIKeyID] = point
	}
	if point := rotatedDistributionByID[keyAID]; point.Name != "Automation Renamed" || point.Requests != 1 || point.Tokens != 2 {
		t.Fatalf("rotated key distribution = %+v, want stable history with current own name", point)
	}
}

func TestResetTodayCostByEndUserUsesAccountBaselineWithoutDeletingLogs(t *testing.T) {
	setupEndUserAccountUsageTestDB(t)

	tenantID := uuid.NewString()
	endUserID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, row := range []APIKeyRow{
		{ID: uuid.NewString(), Key: "sk-reset-a", Name: "A", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), Key: "sk-reset-b", Name: "B", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
	} {
		if err := UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("upsert %s: %v", row.Key, err)
		}
		insertEndUserAccountLog(t, tenantID, row.Key, row.ID, row.Name, 2.5)
	}

	before, err := QueryTodayEffectiveCostByEndUserForTenant(tenantID, endUserID)
	if err != nil || before != 5 {
		t.Fatalf("effective before reset = %v, %v; want 5, nil", before, err)
	}
	usedBefore, rawToday, err := ResetTodayCostByEndUser(tenantID, endUserID)
	if err != nil {
		t.Fatalf("ResetTodayCostByEndUser: %v", err)
	}
	if usedBefore != 5 || rawToday != 5 {
		t.Fatalf("reset result used/raw = %v/%v, want 5/5", usedBefore, rawToday)
	}
	after, err := QueryTodayEffectiveCostByEndUserForTenant(tenantID, endUserID)
	if err != nil || after != 0 {
		t.Fatalf("effective after reset = %v, %v; want 0, nil", after, err)
	}
	var logCount int
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_logs WHERE tenant_id = ?`, tenantID).Scan(&logCount); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if logCount != 2 {
		t.Fatalf("logs after reset = %d, want 2 (baseline must not delete history)", logCount)
	}

	row := GetAPIKey("sk-reset-a")
	insertEndUserAccountLog(t, tenantID, row.Key, row.ID, row.Name, 1.25)
	incremental, err := QueryTodayEffectiveCostByEndUserForTenant(tenantID, endUserID)
	if err != nil || incremental != 1.25 {
		t.Fatalf("effective after new request = %v, %v; want 1.25, nil", incremental, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestEndUserDailySpendingResetEventHistory(t *testing.T) {
	setupEndUserAccountUsageTestDB(t)

	tenantID := uuid.NewString()
	endUserID := uuid.NewString()
	otherUserID := uuid.NewString()
	firstAt := time.Date(2026, time.July, 20, 8, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(2 * time.Hour)
	if err := InsertEndUserDailySpendingResetEvent(EndUserDailySpendingResetEvent{
		TenantID:            tenantID,
		EndUserID:           endUserID,
		DayKey:              "2026-07-20",
		ResetAt:             firstAt,
		ActorUserID:         "actor-1",
		ActorUsername:       "admin",
		ActorKind:           "user",
		CostBaseline:        12,
		EffectiveUsedBefore: 7,
		RawTodayCost:        12,
	}); err != nil {
		t.Fatalf("insert first event: %v", err)
	}
	if err := InsertEndUserDailySpendingResetEvent(EndUserDailySpendingResetEvent{
		TenantID:            tenantID,
		EndUserID:           endUserID,
		DayKey:              "2026-07-20",
		ResetAt:             secondAt,
		EffectiveUsedBefore: 3,
		RawTodayCost:        15,
		CostBaseline:        15,
	}); err != nil {
		t.Fatalf("insert second event: %v", err)
	}
	if err := InsertEndUserDailySpendingResetEvent(EndUserDailySpendingResetEvent{
		TenantID:  tenantID,
		EndUserID: otherUserID,
		ResetAt:   secondAt,
	}); err != nil {
		t.Fatalf("insert other-user event: %v", err)
	}

	count, err := CountEndUserDailySpendingResetEvents(tenantID, endUserID)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	counts, err := ListEndUserDailySpendingResetEventCounts(tenantID, []string{endUserID, otherUserID, "missing", endUserID})
	if err != nil {
		t.Fatalf("list counts: %v", err)
	}
	if counts[endUserID] != 2 || counts[otherUserID] != 1 || counts["missing"] != 0 {
		t.Fatalf("counts = %#v, want user=2 other=1 missing=0", counts)
	}

	events, err := ListEndUserDailySpendingResetEvents(tenantID, endUserID, 1)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].ResetAt.UnixNano() != secondAt.UnixNano() || events[0].RawTodayCost != 15 {
		t.Fatalf("events = %#v, want newest second event", events)
	}

	if err := DeleteEndUserDailySpendingResetEvents(tenantID, endUserID); err != nil {
		t.Fatalf("delete events: %v", err)
	}
	count, err = CountEndUserDailySpendingResetEvents(tenantID, endUserID)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("count after delete = %d, want 0", count)
	}
}

func TestEnsureEndUserDailySpendingResetEventsTableSQLHasNoDATETIME(t *testing.T) {
	if strings.Contains(strings.ToUpper(endUserDailySpendingResetEventsTableSQL), "DATETIME") {
		t.Fatalf("bootstrap SQL must not use DATETIME: %s", endUserDailySpendingResetEventsTableSQL)
	}
	if !strings.Contains(strings.ToUpper(endUserDailySpendingResetEventsTableSQL), "TIMESTAMP") {
		t.Fatalf("bootstrap SQL should use TIMESTAMP: %s", endUserDailySpendingResetEventsTableSQL)
	}
}
