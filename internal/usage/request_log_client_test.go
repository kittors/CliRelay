package usage

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestClientNameCondition(t *testing.T) {
	for name, tc := range map[string]struct {
		names     []string
		condition string
		args      []any
	}{
		"no filter selects every request":             {nil, "", nil},
		"blank values are no filter":                  {[]string{"", "  "}, "", nil},
		"one client":                                  {[]string{"codex-desktop"}, "client_name IN (?)", []any{"codex-desktop"}},
		"names are compared in lower case, once each": {[]string{" Codex-CLI", "codex-cli"}, "client_name IN (?)", []any{"codex-cli"}},
		"unknown selects the rows without a name":     {[]string{ClientNameUnknown}, "client_name = ''", nil},
		"unknown beside a name selects both": {
			[]string{"curl", ClientNameUnknown}, "(client_name IN (?) OR client_name = '')", []any{"curl"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			condition, args := clientNameCondition(tc.names)
			if condition != tc.condition || !reflect.DeepEqual(args, tc.args) {
				t.Fatalf("clientNameCondition(%q) = (%q, %v), want (%q, %v)", tc.names, condition, args, tc.condition, tc.args)
			}
		})
	}
}

func TestNormalizeRequestLogClientBoundsWhatIsStored(t *testing.T) {
	entry := normalizeRequestLogClient(RequestLogEntry{
		ClientName:    " Codex-Desktop ",
		ClientVersion: strings.Repeat("9", 100),
		UserAgent:     strings.Repeat("a", 300),
	})
	if entry.ClientName != "codex-desktop" || len(entry.ClientVersion) != maxClientVersionBytes || len(entry.UserAgent) != maxUserAgentBytes {
		t.Fatalf("entry = %q, %d-byte version, %d-byte user agent", entry.ClientName, len(entry.ClientVersion), len(entry.UserAgent))
	}
	// The facet's placeholder is never a stored name.
	if got := normalizeRequestLogClient(RequestLogEntry{ClientName: ClientNameUnknown}).ClientName; got != "" {
		t.Fatalf("ClientNameUnknown stored as %q", got)
	}
}

func insertClientRow(t *testing.T, model, name, version, ua string, failed bool, at time.Time) {
	t.Helper()
	InsertRequestLog(RequestLogEntry{
		APIKey: "sk-client-test", APIKeyID: "key-client", APIKeyName: "Client", Model: model, Source: "codex",
		Failed: failed, ClientName: name, ClientVersion: version, UserAgent: ua, Timestamp: at,
		Tokens: TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
}

func TestRequestLogClientIsStoredFilteredAndFaceted(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-client", Key: "sk-client-test", Name: "Client"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	// Days counts calendar days from midnight: in the first minute of a day the
	// rows fall on yesterday, so the queries look back two.
	const days = 2
	desktopUA := "Codex Desktop/0.169.0 (Mac OS 27.2.1; arm64) unknown (Codex Desktop; 26.1111.21308)"
	insertClientRow(t, "m-desktop-1", "codex-desktop", "0.169.0", desktopUA, false, now)
	insertClientRow(t, "m-desktop-2", "codex-desktop", "0.169.0", desktopUA, true, now.Add(time.Millisecond))
	insertClientRow(t, "m-cli", "codex-cli", "0.169.0", "codex-tui/0.169.0 (Mac OS 27.0.1; arm64)", false, now.Add(2*time.Millisecond))
	insertClientRow(t, "m-none", "", "", "", false, now.Add(3*time.Millisecond))

	rows := func(params LogQueryParams) []string {
		t.Helper()
		params.Days = days
		result, err := QueryLogs(params)
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		var out []string
		for _, row := range result.Items {
			out = append(out, row.Model+"="+row.ClientName)
		}
		return out
	}

	t.Run("rows carry the caller", func(t *testing.T) {
		result, err := QueryLogs(LogQueryParams{Days: days})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Items) != 4 {
			t.Fatalf("rows = %d", len(result.Items))
		}
		cli := result.Items[1]
		if cli.ClientName != "codex-cli" || cli.ClientVersion != "0.169.0" || cli.UserAgent != "codex-tui/0.169.0 (Mac OS 27.0.1; arm64)" {
			t.Fatalf("row = %q %q %q", cli.ClientName, cli.ClientVersion, cli.UserAgent)
		}
		one, err := QueryLogRowByID(cli.ID)
		if err != nil {
			t.Fatal(err)
		}
		if one.ClientName != cli.ClientName || one.ClientVersion != cli.ClientVersion || one.UserAgent != cli.UserAgent {
			t.Fatalf("by id = %q %q %q", one.ClientName, one.ClientVersion, one.UserAgent)
		}
	})
	t.Run("filter by client", func(t *testing.T) {
		if got := rows(LogQueryParams{ClientNames: []string{"codex-cli"}}); !reflect.DeepEqual(got, []string{"m-cli=codex-cli"}) {
			t.Fatalf("rows = %v", got)
		}
		if got := rows(LogQueryParams{ClientNames: []string{ClientNameUnknown}}); !reflect.DeepEqual(got, []string{"m-none="}) {
			t.Fatalf("unknown rows = %v", got)
		}
		if got := rows(LogQueryParams{ClientNames: []string{"codex-cli", ClientNameUnknown}}); len(got) != 2 {
			t.Fatalf("cli and unknown = %v", got)
		}
		if got := rows(LogQueryParams{ClientNames: []string{"cursor"}}); len(got) != 0 {
			t.Fatalf("a client with no requests listed %v", got)
		}
		if got := rows(LogQueryParams{MatchNoClientNames: true}); len(got) != 0 {
			t.Fatalf("an empty selection listed %v", got)
		}
	})
	t.Run("stats follow the filter", func(t *testing.T) {
		stats, err := QueryStats(LogQueryParams{Days: days, ClientNames: []string{"codex-desktop"}})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Total != 2 || stats.SuccessRate != 50 {
			t.Fatalf("stats = %+v", stats)
		}
		empty, err := QueryStats(LogQueryParams{Days: days, MatchNoClientNames: true})
		if err != nil {
			t.Fatal(err)
		}
		if empty.Total != 0 {
			t.Fatalf("stats of an empty selection = %+v", empty)
		}
	})
	t.Run("the facet lists the clients by requests and ignores its own selection", func(t *testing.T) {
		for _, selected := range [][]string{nil, {"codex-cli"}} {
			filters, err := QueryFiltersForLogs(LogQueryParams{Days: days, ClientNames: selected})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"codex-desktop", ClientNameUnknown, "codex-cli"}; !reflect.DeepEqual(filters.ClientNames, want) {
				t.Fatalf("selected %v: facet = %v, want %v", selected, filters.ClientNames, want)
			}
			if want := map[string]int64{"codex-desktop": 2, "codex-cli": 1, ClientNameUnknown: 1}; !reflect.DeepEqual(filters.ClientNameCounts, want) {
				t.Fatalf("counts = %v, want %v", filters.ClientNameCounts, want)
			}
		}
		// Other facets narrow it.
		filters, err := QueryFiltersForLogs(LogQueryParams{Days: days, Models: []string{"m-cli"}})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(filters.ClientNames, []string{"codex-cli"}) {
			t.Fatalf("facet = %v", filters.ClientNames)
		}
	})
}

func TestRequestLogClientSurvivesTheSpool(t *testing.T) {
	entry := normalizeRequestLogEntry(RequestLogEntry{
		APIKey: "sk-client-test", Model: "m-spool", ClientName: "claude-code", ClientVersion: "2.0.14",
		UserAgent: "claude-cli/2.0.14 (external, cli)", Timestamp: time.Now(),
	})
	line, err := encodeUsageSpoolRecord(entry, usageSpoolReasonWriteFailed, time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeUsageSpoolRecord(line)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ClientName != "claude-code" || decoded.ClientVersion != "2.0.14" || decoded.UserAgent != entry.UserAgent {
		t.Fatalf("decoded = %q %q %q", decoded.ClientName, decoded.ClientVersion, decoded.UserAgent)
	}
	// A line spooled before the fields existed replays as an unknown client.
	legacy := []byte(strings.Replace(string(line), `"client_name":"claude-code",`, "", 1))
	if decoded, _, err := decodeUsageSpoolRecord(legacy); err != nil || decoded.ClientName != "" {
		t.Fatalf("legacy line: %q, %v", decoded.ClientName, err)
	}
}

func TestUsageRecordCarriesTheClientIntoTheLogEntry(t *testing.T) {
	stats := NewRequestStatistics()
	entry := stats.ingest(context.Background(), coreusage.Record{
		APIKey: "sk-client-test", Model: "m-record", RequestedAt: time.Now(),
		ClientName: "codex-desktop", ClientVersion: "0.169.0", UserAgent: "Codex Desktop/0.169.0",
	})
	if entry.ClientName != "codex-desktop" || entry.ClientVersion != "0.169.0" || entry.UserAgent != "Codex Desktop/0.169.0" {
		t.Fatalf("entry = %q %q %q", entry.ClientName, entry.ClientVersion, entry.UserAgent)
	}
}
