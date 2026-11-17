package usage

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// The production request log is PostgreSQL, where the client columns come from
// a migration and the filter and the counted facet run on another engine than
// the SQLite the unit tests use.
func TestPostgresRequestLogClient(t *testing.T) {
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
	// Days counts calendar days from midnight, not hours: between 00:00 and
	// 02:00 the rows above fall on yesterday, so the queries look back two.
	const days = 2
	insert := func(model, name, version, ua string, failed bool, offset time.Duration) {
		t.Helper()
		InsertRequestLog(RequestLogEntry{
			APIKey: "sk-client-pg", APIKeyID: "key-client-pg", APIKeyName: "Client", Model: model, Source: "codex",
			Failed: failed, ClientName: name, ClientVersion: version, UserAgent: ua, Timestamp: at(offset),
			Tokens: TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}
	insert("pg-desktop", "codex-desktop", "0.169.0", "Codex Desktop/0.169.0 (Windows 10.0.26200; x86_64)", false, 1*time.Millisecond)
	insert("pg-desktop", "codex-desktop", "0.169.0", "Codex Desktop/0.169.0 (Windows 10.0.26200; x86_64)", true, 2*time.Millisecond)
	insert("pg-curl", "curl", "8.7.1", "curl/8.7.1", false, 3*time.Millisecond)
	insert("pg-none", "", "", "", false, 4*time.Millisecond)

	rows := func(params LogQueryParams) []string {
		t.Helper()
		params.Days = days
		params.Size = 50
		result, err := QueryLogs(params)
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		var out []string
		for _, row := range result.Items {
			out = append(out, row.Model+"="+row.ClientName+"/"+row.ClientVersion)
		}
		return out
	}

	t.Run("rows carry the caller", func(t *testing.T) {
		got := rows(LogQueryParams{})
		want := []string{"pg-none=/", "pg-curl=curl/8.7.1", "pg-desktop=codex-desktop/0.169.0", "pg-desktop=codex-desktop/0.169.0"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
	t.Run("the filter selects by client", func(t *testing.T) {
		if got := rows(LogQueryParams{ClientNames: []string{"curl", ClientNameUnknown}}); len(got) != 2 {
			t.Fatalf("curl and unknown = %v", got)
		}
		if got := rows(LogQueryParams{ClientNames: []string{"codex-desktop"}, Statuses: []string{"failed"}}); len(got) != 1 {
			t.Fatalf("failed desktop rows = %v", got)
		}
		if got := rows(LogQueryParams{MatchNoClientNames: true}); len(got) != 0 {
			t.Fatalf("an empty selection listed %v", got)
		}
	})
	t.Run("stats and the facet follow", func(t *testing.T) {
		stats, err := QueryStats(LogQueryParams{Days: days, ClientNames: []string{"codex-desktop"}})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Total != 2 || stats.SuccessRate != 50 {
			t.Fatalf("stats = %+v", stats)
		}
		filters, err := QueryFiltersForLogs(LogQueryParams{Days: days, ClientNames: []string{"curl"}})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"codex-desktop", ClientNameUnknown, "curl"}; !reflect.DeepEqual(filters.ClientNames, want) {
			t.Fatalf("facet = %v, want %v", filters.ClientNames, want)
		}
		if want := map[string]int64{"codex-desktop": 2, "curl": 1, ClientNameUnknown: 1}; !reflect.DeepEqual(filters.ClientNameCounts, want) {
			t.Fatalf("counts = %v, want %v", filters.ClientNameCounts, want)
		}
	})
}
