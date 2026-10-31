package usage

import (
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// The production request log is PostgreSQL, where the guard_kind column is added
// by a migration and the filter, the facet and the monitor counts are evaluated
// by an engine other than the SQLite the unit tests use.
func TestPostgresRequestLogGuardKind(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()
	truncatePostgresRuntimeTables(t, getDB())
	// The rows are this test's: leave the shared database as it was found. This
	// runs before the CloseDB registered above.
	t.Cleanup(func() { truncatePostgresRuntimeTables(t, getDB()) })

	now := time.Now().UTC()
	at := func(offset time.Duration) time.Time { return now.Add(-2*time.Hour + offset) }
	// The rows loop protection ends go through the write path itself.
	insert := func(model, kind string, failed bool, offset time.Duration) {
		t.Helper()
		InsertRequestLog(RequestLogEntry{
			APIKey: "sk-guard-pg", APIKeyID: "key-guard-pg", APIKeyName: "Guard", Model: model, Source: "claude",
			Failed: failed, GuardKind: kind, Timestamp: at(offset),
			Tokens: TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}
	insert("pg-reply", "reply_loop", true, 2*time.Millisecond)
	insert("pg-plain", "tool_loop", true, 3*time.Millisecond)
	insert("pg-plain", "tool_loop", true, 4*time.Millisecond)
	// A kind this build does not know is stored as none.
	insert("pg-bogus", "bogus", true, 5*time.Millisecond)
	// The priced successes the saved cost is averaged from carry the cost the
	// pricing table would have given them.
	seedGuardMonitorRows(t,
		guardMonitorRow{keyID: "key-guard-pg", model: "pg-plain", cost: 0.4, at: at(0)},
		guardMonitorRow{keyID: "key-guard-pg", model: "pg-plain", cost: 0.6, at: at(time.Millisecond)},
	)

	models := func(params LogQueryParams) []string {
		t.Helper()
		params.Days = 1
		params.Size = 50
		result, err := QueryLogs(params)
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		var out []string
		for _, row := range result.Items {
			out = append(out, row.Model+"="+row.GuardKind)
		}
		return out
	}

	t.Run("rows carry their kind", func(t *testing.T) {
		got := models(LogQueryParams{})
		want := []string{"pg-bogus=", "pg-plain=tool_loop", "pg-plain=tool_loop", "pg-reply=reply_loop", "pg-plain=", "pg-plain="}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
	t.Run("the filter selects by kind", func(t *testing.T) {
		if got := models(LogQueryParams{GuardKinds: []string{"tool_loop"}}); len(got) != 2 {
			t.Fatalf("tool_loop rows = %v", got)
		}
		if got := models(LogQueryParams{GuardKinds: []string{"reply_loop", "tool_loop"}}); len(got) != 3 {
			t.Fatalf("both kinds = %v", got)
		}
		if got := models(LogQueryParams{GuardKinds: []string{"bogus"}}); len(got) != 0 {
			t.Fatalf("an unknown kind selected %v", got)
		}
		if got := models(LogQueryParams{GuardKinds: []string{"tool_loop"}, Statuses: []string{"success"}}); len(got) != 0 {
			t.Fatalf("a loop-ended request listed as a success: %v", got)
		}
	})
	t.Run("stats and the facet follow", func(t *testing.T) {
		stats, err := QueryStats(LogQueryParams{Days: 1, GuardKinds: []string{"reply_loop", "tool_loop"}})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Total != 3 || stats.SuccessRate != 0 {
			t.Fatalf("stats = %+v", stats)
		}
		filters, err := QueryFiltersForLogs(LogQueryParams{Days: 1, GuardKinds: []string{"reply_loop"}})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"reply_loop", "tool_loop"}; !reflect.DeepEqual(filters.GuardKinds, want) {
			t.Fatalf("facet = %v, want %v", filters.GuardKinds, want)
		}
	})
	t.Run("the monitor counts them and prices the tool loops it saved", func(t *testing.T) {
		overview, err := QueryMonitorOverview(MonitorFilter{TenantID: systemTenantID}, "24h", now)
		if err != nil {
			t.Fatalf("QueryMonitorOverview: %v", err)
		}
		got := overview.LoopGuard
		if got.ReplyLoops != 1 || got.ToolLoops != 2 {
			t.Fatalf("counts = %+v", got)
		}
		// Two answered requests at the 0.5 average of pg-plain's priced successes.
		if want := 2 * 0.5; math.Abs(got.EstimatedSavedCost-want) > 1e-9 {
			t.Fatalf("estimated saved cost = %v, want %v", got.EstimatedSavedCost, want)
		}
	})
}
