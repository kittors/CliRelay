package usage_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// TestPostgresCcSwitchPresetKeepsCodexContextWindow round-trips the explicit
// Codex context window through PostgreSQL. A list query that fails on a
// missing column only logs and returns nothing, so the route smoke would not
// notice; this reads the value back.
func TestPostgresCcSwitchPresetKeepsCodexContextWindow(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	const tenant = "00000000-0000-0000-0000-000000000001"
	previous := usage.ListCcSwitchImportConfigsForTenant(tenant)
	t.Cleanup(func() { _ = usage.ReplaceAllCcSwitchImportConfigsForTenant(tenant, previous) })

	rows := []usage.CcSwitchImportConfigRow{
		{ID: "pg-ctx-codex", ClientType: "codex", ProviderName: "Relay Codex", DefaultModel: "gpt-5.5", CodexContextWindow: 200000, RoutePath: "/pg-ctx/cs_codex"},
		{ID: "pg-ctx-claude", ClientType: "claude", ProviderName: "Relay Claude", DefaultModel: "claude-sonnet-4-5", CodexContextWindow: 200000, RoutePath: "/pg-ctx/cs_claude"},
	}
	if _, err := usage.ReplaceAllCcSwitchImportConfigsForTenantExpect(context.Background(), tenant, rows, configsync.AnyVersion); err != nil {
		t.Fatalf("replace presets: %v", err)
	}
	stored := map[string]usage.CcSwitchImportConfigRow{}
	for _, row := range usage.ListCcSwitchImportConfigsForTenant(tenant) {
		stored[row.ID] = row
	}
	if got := stored["pg-ctx-codex"]; got.ID == "" || got.CodexContextWindow != 200000 {
		t.Fatalf("codex preset = %+v, want codex context window 200000", got)
	}
	if got := stored["pg-ctx-claude"]; got.ID == "" || got.CodexContextWindow != 0 {
		t.Fatalf("claude preset = %+v, want no codex context window", got)
	}
	if found, ok := usage.FindCcSwitchImportConfigByRoutePathForTenant(tenant, "/pg-ctx/cs_codex"); !ok || found.CodexContextWindow != 200000 {
		t.Fatalf("route lookup = %+v (found %v)", found, ok)
	}
}
