package usage

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

func TestPostgresRuntimeDataStackIntegration(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)

	err := InitPostgres(config.PostgresConfig{
		DSN:          dsn,
		MaxOpenConns: 4,
		MaxIdleConns: 1,
	}, config.RequestLogStorageConfig{StoreContent: true}, time.UTC)
	if err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	db := getDB()
	if db == nil {
		t.Fatal("postgres db is nil")
	}
	dbStats, err := GetDatabaseStats()
	if err != nil {
		t.Fatalf("GetDatabaseStats() error = %v", err)
	}
	if dbStats.Driver != "postgres" {
		t.Fatalf("GetDatabaseStats().Driver = %q, want postgres", dbStats.Driver)
	}
	if dbStats.SizeBytes <= 0 {
		t.Fatalf("GetDatabaseStats().SizeBytes = %d, want > 0", dbStats.SizeBytes)
	}
	if dbPath := GetDBPath(); dbPath != "" {
		t.Fatalf("GetDBPath() = %q, want empty for postgres", dbPath)
	}
	if _, err := db.Exec(`
		TRUNCATE
			request_log_content,
			request_logs,
			api_keys,
			api_key_permission_profiles,
			model_pricing,
			model_configs,
			proxy_pool,
			routing_config,
			runtime_settings,
			identity_fingerprints,
			ccswitch_import_configs
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatalf("truncate runtime tables: %v", err)
	}

	if err := UpsertModelPricingV2("gpt-test", 1, 2, 0.5, 0.25, 0.75); err != nil {
		t.Fatalf("UpsertModelPricingV2() error = %v", err)
	}
	if _, ok := GetModelPricing("gpt-test"); !ok {
		t.Fatal("model pricing was not cached")
	}

	apiRow := APIKeyRow{
		Key:       "sk-postgres-test",
		ID:        "key-postgres-test",
		Name:      "postgres test",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := UpsertAPIKey(apiRow); err != nil {
		t.Fatalf("UpsertAPIKey() error = %v", err)
	}
	if got := GetAPIKeyByID("key-postgres-test"); got == nil || got.Key != apiRow.Key {
		t.Fatalf("GetAPIKeyByID() = %#v", got)
	}
	assertPostgresConfigCRUD(t)

	InsertLogWithDetailsIdentitySubject(
		apiRow.Key,
		apiRow.ID,
		"subject-postgres-test",
		apiRow.Name,
		"gpt-test",
		"codex",
		"codex",
		"auth-1",
		false,
		time.Now().UTC(),
		123,
		45,
		TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		`{"stream":true}`,
		`{"ok":true}`,
		`{"detail":true}`,
	)

	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if logs.Total != 1 || len(logs.Items) != 1 {
		t.Fatalf("QueryLogs() total=%d len=%d", logs.Total, len(logs.Items))
	}
	if !logs.Items[0].HasContent || !logs.Items[0].Streaming {
		t.Fatalf("log content/streaming flags not preserved: %#v", logs.Items[0])
	}
	stats, err := QueryStats(LogQueryParams{Days: 1})
	if err != nil {
		t.Fatalf("QueryStats() error = %v", err)
	}
	if stats.Total != 1 || stats.TotalTokens != 15 {
		t.Fatalf("QueryStats() = %#v", stats)
	}
	assertPostgresRequestLogQueries(t, logs.Items[0].ID, apiRow.Key)
	assertPostgresIdentityAndQuotaCRUD(t)
	assertPostgresDeletes(t, apiRow.Key)

	if redisAddr := os.Getenv("CLIRELAY_REDIS_TEST_ADDR"); redisAddr != "" {
		InitRedis(config.RedisConfig{Enable: true, Addr: redisAddr})
		StopRedis()
	}
}

func TestPostgresRuntimeDataStackConcurrencyConstraintsAndHotPaths(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)

	if err := InitPostgres(config.PostgresConfig{
		DSN:          dsn,
		MaxOpenConns: 8,
		MaxIdleConns: 2,
	}, config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	db := getDB()
	if db == nil {
		t.Fatal("postgres db is nil")
	}
	truncatePostgresRuntimeTables(t, db)

	if err := UpsertAPIKey(APIKeyRow{Key: "sk-hotpath", ID: "hotpath", Name: "Hot Path"}); err != nil {
		t.Fatalf("UpsertAPIKey hotpath: %v", err)
	}
	if err := UpsertModelPricingV2("gpt-hotpath", 1, 2, 0, 0, 0); err != nil {
		t.Fatalf("UpsertModelPricingV2 hotpath: %v", err)
	}

	const workers = 12
	const perWorker = 25
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				InsertLogWithDetailsIdentitySubject(
					"sk-hotpath",
					"hotpath",
					"subject-hotpath",
					"Hot Path",
					"gpt-hotpath",
					"codex",
					"codex",
					"auth-hotpath",
					i%7 == 0,
					time.Now().UTC().Add(time.Duration(worker*perWorker+i)*time.Millisecond),
					100,
					20,
					TokenStats{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
					`{"model":"gpt-hotpath"}`,
					`{"ok":true}`,
					`{"worker":true}`,
				)
			}
		}(worker)
	}
	wg.Wait()

	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 50, Days: 1, APIKeys: []string{"sk-hotpath"}})
	if err != nil {
		t.Fatalf("QueryLogs hotpath: %v", err)
	}
	if want := int64(workers * perWorker); logs.Total != want {
		t.Fatalf("QueryLogs hotpath total = %d, want %d", logs.Total, want)
	}
	stats, err := QueryStats(LogQueryParams{Days: 1, APIKeys: []string{"sk-hotpath"}})
	if err != nil {
		t.Fatalf("QueryStats hotpath: %v", err)
	}
	if wantTokens := int64(workers * perWorker * 7); stats.TotalTokens != wantTokens {
		t.Fatalf("QueryStats total_tokens = %d, want %d", stats.TotalTokens, wantTokens)
	}
	if filters, err := QueryFilters(1); err != nil || len(filters.APIKeys) != 1 || filters.APIKeys[0] != "sk-hotpath" {
		t.Fatalf("QueryFilters filters=%#v err=%v", filters, err)
	}

	if _, err := db.Exec(`INSERT INTO api_keys (key, id, name) VALUES (?, ?, ?)`, "sk-dup-a", "dup-id", "A"); err != nil {
		t.Fatalf("insert duplicate fixture A: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (key, id, name) VALUES (?, ?, ?)`, "sk-dup-b", "dup-id", "B"); err == nil {
		t.Fatal("duplicate api_keys.id insert succeeded, want unique constraint failure")
	}
	if err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{ID: "cc-dup-a", ClientType: "claude", RoutePath: "/cc/dup", EndpointPath: "/v1/messages"},
		{ID: "cc-dup-b", ClientType: "claude", RoutePath: "/cc/dup", EndpointPath: "/v1/messages"},
	}); err == nil {
		t.Fatal("ReplaceAllCcSwitchImportConfigs duplicate route_path succeeded, want constraint/validation failure")
	}

	firstID := logs.Items[0].ID
	if deleted, err := DeleteLogsByAPIKey("sk-hotpath"); err != nil || deleted != int64(workers*perWorker) {
		t.Fatalf("DeleteLogsByAPIKey deleted=%d err=%v", deleted, err)
	}
	var contentRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log_content WHERE log_id = ?`, firstID).Scan(&contentRows); err != nil {
		t.Fatalf("count cascaded request_log_content: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("request_log_content rows after cascade = %d, want 0", contentRows)
	}

	assertPostgresExplainUsesIndex(t, db, "api_key timestamp", `
		EXPLAIN (FORMAT TEXT)
		SELECT id FROM request_logs
		 WHERE api_key = ? AND timestamp >= ?
		 ORDER BY timestamp DESC
		 LIMIT 20
	`, "idx_logs_api_key", "sk-hotpath", time.Now().UTC().Add(-24*time.Hour))
	assertPostgresExplainUsesIndex(t, db, "api_key_id timestamp", `
		EXPLAIN (FORMAT TEXT)
		SELECT id FROM request_logs
		 WHERE api_key_id = ? AND timestamp >= ?
		 ORDER BY timestamp DESC
		 LIMIT 20
	`, "idx_logs_api_key_id", "hotpath", time.Now().UTC().Add(-24*time.Hour))
}

func truncatePostgresRuntimeTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		TRUNCATE
			request_log_content,
			request_logs,
			api_keys,
			api_key_permission_profiles,
			model_pricing,
			model_configs,
			proxy_pool,
			routing_config,
			runtime_settings,
			identity_fingerprints,
			ccswitch_import_configs
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatalf("truncate runtime tables: %v", err)
	}
}

func assertPostgresExplainUsesIndex(t *testing.T, db interface {
	Conn(context.Context) (*sql.Conn, error)
}, label, query, indexFragment string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("postgres conn for EXPLAIN %s: %v", label, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan for EXPLAIN %s: %v", label, err)
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", label, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN %s: %v", label, err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows %s: %v", label, err)
	}
	if !strings.Contains(plan.String(), indexFragment) {
		t.Fatalf("EXPLAIN %s did not use %s:\n%s", label, indexFragment, plan.String())
	}
}

func assertPostgresConfigCRUD(t *testing.T) {
	t.Helper()

	profiles := []APIKeyPermissionProfileRow{{
		ID:                   "profile-postgres-test",
		Name:                 "Postgres Test",
		AllowedModels:        []string{"gpt-test"},
		AllowedChannels:      []string{"codex"},
		AllowedChannelGroups: []string{"default"},
		SystemPrompt:         "test prompt",
	}}
	if err := ReplaceAllAPIKeyPermissionProfiles(profiles); err != nil {
		t.Fatalf("ReplaceAllAPIKeyPermissionProfiles() error = %v", err)
	}
	if got := ListAPIKeyPermissionProfiles(); len(got) != 1 || got[0].ID != profiles[0].ID {
		t.Fatalf("ListAPIKeyPermissionProfiles() = %#v", got)
	}

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:                  "gpt-test",
		OwnedBy:                  "openai",
		Description:              "postgres test model",
		Enabled:                  true,
		InputModalities:          []string{"text"},
		OutputModalities:         []string{"text"},
		PricingMode:              "token",
		InputPricePerMillion:     1,
		OutputPricePerMillion:    2,
		CachedPricePerMillion:    0.5,
		CacheReadPricePerMillion: 0.25,
		UpdatedAt:                time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}
	if got, ok := GetModelConfig("gpt-test"); !ok || got.OwnedBy != "openai" {
		t.Fatalf("GetModelConfig() = %#v ok=%v", got, ok)
	}
	if err := UpsertModelOwnerPreset(ModelOwnerPresetRow{Value: "local", Label: "Local", Enabled: true}); err != nil {
		t.Fatalf("UpsertModelOwnerPreset() error = %v", err)
	}
	if _, ok := GetModelOwnerPreset("local"); !ok {
		t.Fatal("GetModelOwnerPreset() missing local")
	}
	if err := UpsertAuthGroupOwnerMapping(AuthGroupOwnerMappingRow{AuthGroup: "Team A", Owner: "OpenAI"}); err != nil {
		t.Fatalf("UpsertAuthGroupOwnerMapping() error = %v", err)
	}
	if got, ok := GetAuthGroupOwnerMapping("team-a"); !ok || got.Owner != "openai" {
		t.Fatalf("GetAuthGroupOwnerMapping() = %#v ok=%v", got, ok)
	}

	proxies := []config.ProxyPoolEntry{{ID: "pg-proxy", Name: "PG Proxy", URL: "http://127.0.0.1:7890", Enabled: true}}
	if err := ReplaceProxyPool(proxies); err != nil {
		t.Fatalf("ReplaceProxyPool() error = %v", err)
	}
	if got := GetProxyPoolEntry("pg-proxy"); got == nil || got.URL != proxies[0].URL {
		t.Fatalf("GetProxyPoolEntry() = %#v", got)
	}

	routing := config.RoutingConfig{
		Strategy:            "fill-first",
		IncludeDefaultGroup: true,
		ChannelGroups: []config.RoutingChannelGroup{{
			Name:     "codex-group",
			Strategy: "round-robin",
			Match:    config.ChannelGroupMatch{Channels: []string{"codex"}},
		}},
		PathRoutes: []config.RoutingPathRoute{{Path: "/codex", Group: "codex-group"}},
	}
	if err := UpsertRoutingConfig(routing); err != nil {
		t.Fatalf("UpsertRoutingConfig() error = %v", err)
	}
	if got := GetRoutingConfig(); got == nil || len(got.PathRoutes) != 1 || got.PathRoutes[0].Path != "/codex" {
		t.Fatalf("GetRoutingConfig() = %#v", got)
	}

	if err := UpsertRuntimeSetting(RuntimeSettingOAuthModelAlias, []config.OAuthModelAlias{{Name: "gpt-test", Alias: "gpt-test-alias"}}); err != nil {
		t.Fatalf("UpsertRuntimeSetting() error = %v", err)
	}
	if payload, ok := GetRuntimeSettingPayload(RuntimeSettingOAuthModelAlias); !ok || len(payload) == 0 {
		t.Fatalf("GetRuntimeSettingPayload() payload=%s ok=%v", string(payload), ok)
	}
	if err := UpsertRuntimeSetting(RuntimeSettingClineKeys, []config.ClineKey{{
		APIKey: "sk-cline-pg",
		Models: []config.ClineModel{{
			Name:  "cline-pass/qwen3.7-max",
			Alias: "qwen3.7-max",
		}},
	}}); err != nil {
		t.Fatalf("UpsertRuntimeSetting(ClineKey) error = %v", err)
	}
	cfg := &config.Config{}
	if !ApplyStoredRuntimeSettings(cfg) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if len(cfg.ClineKey) != 1 || cfg.ClineKey[0].Models[0].Alias != "qwen3.7-max" {
		t.Fatalf("ClineKey after postgres runtime apply = %#v", cfg.ClineKey)
	}

	imports := []CcSwitchImportConfigRow{{
		ID:                   "cc-postgres-test",
		ClientType:           "claude",
		ProviderName:         "postgres",
		DefaultModel:         "claude-sonnet",
		ModelMappings:        []CcSwitchModelMappingRow{{RequestModel: "sonnet", TargetModel: "claude-sonnet"}},
		AllowedChannelGroups: []string{"codex-group"},
		RoutePath:            "/cc/pg",
		EndpointPath:         "/v1/messages",
		APIKeyField:          "ANTHROPIC_API_KEY",
	}}
	if err := ReplaceAllCcSwitchImportConfigs(imports); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}
	if got, ok := FindCcSwitchImportConfigByRoutePath("/cc/pg"); !ok || got.ID != imports[0].ID {
		t.Fatalf("FindCcSwitchImportConfigByRoutePath() = %#v ok=%v", got, ok)
	}
}

func assertPostgresRequestLogQueries(t *testing.T, logID int64, apiKey string) {
	t.Helper()

	if row, err := QueryLogRowByID(logID); err != nil || row.ID != logID {
		t.Fatalf("QueryLogRowByID() row=%#v err=%v", row, err)
	}
	if content, err := QueryLogContent(logID); err != nil || content.InputContent == "" || content.OutputContent == "" {
		t.Fatalf("QueryLogContent() content=%#v err=%v", content, err)
	}
	if part, err := QueryLogContentPart(logID, "details"); err != nil || part.Content == "" {
		t.Fatalf("QueryLogContentPart(details) part=%#v err=%v", part, err)
	}
	if content, err := QueryLogContentForKey(logID, apiKey); err != nil || content.ID != logID {
		t.Fatalf("QueryLogContentForKey() content=%#v err=%v", content, err)
	}
	if part, err := QueryLogContentPartForKey(logID, apiKey, "input"); err != nil || part.Content == "" {
		t.Fatalf("QueryLogContentPartForKey(input) part=%#v err=%v", part, err)
	}
	if filters, err := QueryFilters(1); err != nil || len(filters.APIKeys) == 0 || len(filters.Models) == 0 {
		t.Fatalf("QueryFilters() filters=%#v err=%v", filters, err)
	}
	if series, err := QueryDailySeries(apiKey, 1); err != nil || len(series) == 0 {
		t.Fatalf("QueryDailySeries() series=%#v err=%v", series, err)
	}
	if heatmap, err := QueryDailyHeatmapSeries(apiKey, 1); err != nil || len(heatmap) == 0 {
		t.Fatalf("QueryDailyHeatmapSeries() heatmap=%#v err=%v", heatmap, err)
	}
	if tokens, models, err := QueryHourlySeries(apiKey, 24); err != nil || len(tokens) == 0 || len(models) == 0 {
		t.Fatalf("QueryHourlySeries() tokens=%#v models=%#v err=%v", tokens, models, err)
	}
	if kpi, err := QueryDashboardKPI(1); err != nil || kpi.TotalRequests != 1 {
		t.Fatalf("QueryDashboardKPI() kpi=%#v err=%v", kpi, err)
	}
	if trends, err := QueryDashboardTrends(1); err != nil || len(trends.RequestVolume) == 0 {
		t.Fatalf("QueryDashboardTrends() trends=%#v err=%v", trends, err)
	}
	if daily, err := QueryDailyCallsByAuthIndexes([]string{"auth-1"}, 1); err != nil || len(daily) == 0 {
		t.Fatalf("QueryDailyCallsByAuthIndexes() daily=%#v err=%v", daily, err)
	}
	if hourly, err := QueryHourlyCallsByAuthIndex("auth-1", 24); err != nil || len(hourly) == 0 {
		t.Fatalf("QueryHourlyCallsByAuthIndex() hourly=%#v err=%v", hourly, err)
	}
	matcher := AuthSubjectMatcher{SubjectID: "subject-postgres-test", AuthIndexes: []string{"auth-1"}}
	if daily, err := QueryDailyUsageByAuthSubject(matcher, 1); err != nil || len(daily) == 0 {
		t.Fatalf("QueryDailyUsageByAuthSubject() daily=%#v err=%v", daily, err)
	}
	if hourly, err := QueryHourlyUsageByAuthSubject(matcher, 24); err != nil || len(hourly) == 0 {
		t.Fatalf("QueryHourlyUsageByAuthSubject() hourly=%#v err=%v", hourly, err)
	}
	if entities, err := QueryEntityStats("", 1, "auth_index", []string{"auth-1"}); err != nil || len(entities) == 0 {
		t.Fatalf("QueryEntityStats() entities=%#v err=%v", entities, err)
	}
}

func assertPostgresIdentityAndQuotaCRUD(t *testing.T) {
	t.Helper()

	observedAt := time.Now().UTC()
	record, merge, err := ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderCodex,
		AccountKey:    "acct-postgres-test",
		AuthSubjectID: "subject-postgres-test",
		Headers: map[string][]string{
			"User-Agent": {"codex_cli_rs/1.2.3 (Mac OS; arm64)"},
			"Originator": {"codex_cli_rs"},
		},
		ObservedAt: observedAt,
	})
	if err != nil || record == nil || !merge.Changed {
		t.Fatalf("ObserveIdentityFingerprint() record=%#v merge=%#v err=%v", record, merge, err)
	}
	if got, err := GetIdentityFingerprint(identityfingerprint.ProviderCodex, "acct-postgres-test"); err != nil || got == nil {
		t.Fatalf("GetIdentityFingerprint() got=%#v err=%v", got, err)
	}
	if list, err := ListIdentityFingerprints(identityfingerprint.ProviderCodex, 10); err != nil || len(list) == 0 {
		t.Fatalf("ListIdentityFingerprints() list=%#v err=%v", list, err)
	}

	percent := 42.5
	resetAt := observedAt.Add(time.Hour)
	if err := RecordDailyQuotaSnapshotIdentity("auth-1", "subject-postgres-test", "codex", map[string]*float64{"weekly": &percent}); err != nil {
		t.Fatalf("RecordDailyQuotaSnapshotIdentity() error = %v", err)
	}
	if err := RecordQuotaSnapshotPointsIdentity("auth-1", "subject-postgres-test", "codex", []QuotaSnapshotPoint{{
		RecordedAt:    observedAt,
		QuotaKey:      "weekly",
		QuotaLabel:    "Weekly",
		Percent:       &percent,
		ResetAt:       &resetAt,
		WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
	}}); err != nil {
		t.Fatalf("RecordQuotaSnapshotPointsIdentity() error = %v", err)
	}
	if daily, err := QueryDailyQuotaByAuthIndexes([]string{"auth-1"}, "weekly", 1); err != nil || len(daily) == 0 {
		t.Fatalf("QueryDailyQuotaByAuthIndexes() daily=%#v err=%v", daily, err)
	}
	if series, err := QueryQuotaSnapshotSeriesByAuthSubject(AuthSubjectMatcher{SubjectID: "subject-postgres-test"}, observedAt.Add(-time.Hour), observedAt.Add(time.Hour)); err != nil || len(series) == 0 {
		t.Fatalf("QueryQuotaSnapshotSeriesByAuthSubject() series=%#v err=%v", series, err)
	}
	if cycle, err := QueryLatestWeeklyQuotaCycleByAuthSubject("subject-postgres-test", "weekly"); err != nil || cycle == nil {
		t.Fatalf("QueryLatestWeeklyQuotaCycleByAuthSubject() cycle=%#v err=%v", cycle, err)
	}
	if deleted, err := DeleteIdentityFingerprint(identityfingerprint.ProviderCodex, "acct-postgres-test"); err != nil || deleted != 1 {
		t.Fatalf("DeleteIdentityFingerprint() deleted=%d err=%v", deleted, err)
	}
}

func assertPostgresDeletes(t *testing.T, apiKey string) {
	t.Helper()

	if deleted, err := DeleteLogsByAPIKey(apiKey); err != nil || deleted != 1 {
		t.Fatalf("DeleteLogsByAPIKey() deleted=%d err=%v", deleted, err)
	}
	if logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1}); err != nil || logs.Total != 0 {
		t.Fatalf("QueryLogs() after delete logs=%#v err=%v", logs, err)
	}
}
