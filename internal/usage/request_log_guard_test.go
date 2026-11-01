package usage

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestNormalizeGuardKind(t *testing.T) {
	for in, want := range map[string]string{
		"":                "",
		"reply_loop":      "reply_loop",
		"tool_loop":       "tool_loop",
		" tool_loop ":     "tool_loop",
		"Tool_Loop":       "", // the values are case-exact: the panel compares them
		"something_else":  "",
		"reply_loop,tool": "",
	} {
		if got := normalizeGuardKind(in); got != want {
			t.Fatalf("normalizeGuardKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuardKindCondition(t *testing.T) {
	for name, tc := range map[string]struct {
		kinds     []string
		condition string
		args      []any
	}{
		"no filter selects every request":  {nil, "", nil},
		"blank values are no filter":       {[]string{"", "  "}, "", nil},
		"one kind":                         {[]string{"reply_loop"}, "guard_kind IN (?)", []any{"reply_loop"}},
		"two kinds":                        {[]string{"reply_loop", "tool_loop"}, "guard_kind IN (?,?)", []any{"reply_loop", "tool_loop"}},
		"duplicates and spaces collapse":   {[]string{" tool_loop", "tool_loop "}, "guard_kind IN (?)", []any{"tool_loop"}},
		"an unknown kind selects nothing":  {[]string{"bogus"}, "1 = 0", nil},
		"unknown beside known keeps known": {[]string{"bogus", "tool_loop"}, "guard_kind IN (?)", []any{"tool_loop"}},
	} {
		t.Run(name, func(t *testing.T) {
			condition, args := guardKindCondition(tc.kinds)
			if condition != tc.condition || !reflect.DeepEqual(args, tc.args) {
				t.Fatalf("guardKindCondition(%q) = (%q, %v), want (%q, %v)", tc.kinds, condition, args, tc.condition, tc.args)
			}
		})
	}
}

func insertGuardRow(t *testing.T, model, kind string, failed bool, at time.Time) {
	t.Helper()
	InsertRequestLog(RequestLogEntry{
		APIKey: "sk-guard-test", APIKeyID: "key-guard", APIKeyName: "Guard", Model: model, Source: "claude",
		Failed: failed, GuardKind: kind, Timestamp: at,
		Tokens: TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
}

func TestGuardKindIsStoredFilteredAndFaceted(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-guard", Key: "sk-guard-test", Name: "Guard"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	// Days counts calendar days from midnight: in the first minute of a day the
	// rows fall on yesterday, so the queries look back two.
	const days = 2
	insertGuardRow(t, "m-plain", "", false, now)
	insertGuardRow(t, "m-reply", "reply_loop", true, now.Add(time.Millisecond))
	insertGuardRow(t, "m-tool", "tool_loop", true, now.Add(2*time.Millisecond))
	insertGuardRow(t, "m-plain-failed", "", true, now.Add(3*time.Millisecond))
	// A kind this build does not know is not stored as one.
	insertGuardRow(t, "m-bogus", "bogus", true, now.Add(4*time.Millisecond))

	models := func(params LogQueryParams) []string {
		t.Helper()
		params.Days = days
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
		want := []string{"m-bogus=", "m-plain-failed=", "m-tool=tool_loop", "m-reply=reply_loop", "m-plain="}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
	t.Run("filter by one kind", func(t *testing.T) {
		if got := models(LogQueryParams{GuardKinds: []string{"reply_loop"}}); !reflect.DeepEqual(got, []string{"m-reply=reply_loop"}) {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("filter by both kinds", func(t *testing.T) {
		if got := models(LogQueryParams{GuardKinds: []string{"reply_loop", "tool_loop"}}); len(got) != 2 {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("an unknown kind selects nothing", func(t *testing.T) {
		if got := models(LogQueryParams{GuardKinds: []string{"bogus"}}); len(got) != 0 {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("combines with the status filter", func(t *testing.T) {
		if got := models(LogQueryParams{GuardKinds: []string{"tool_loop"}, Statuses: []string{"success"}}); len(got) != 0 {
			t.Fatalf("a loop-ended request is a failure, but the success filter returned %v", got)
		}
		if got := models(LogQueryParams{GuardKinds: []string{"tool_loop"}, Statuses: []string{"failed"}}); len(got) != 1 {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("stats follow the filter", func(t *testing.T) {
		stats, err := QueryStats(LogQueryParams{Days: days, GuardKinds: []string{"reply_loop", "tool_loop"}})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Total != 2 || stats.SuccessRate != 0 {
			t.Fatalf("stats = %+v", stats)
		}
	})
	t.Run("the facet lists the kinds present and ignores its own selection", func(t *testing.T) {
		for _, selected := range [][]string{nil, {"reply_loop"}} {
			filters, err := QueryFiltersForLogs(LogQueryParams{Days: days, GuardKinds: selected})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"reply_loop", "tool_loop"}; !reflect.DeepEqual(filters.GuardKinds, want) {
				t.Fatalf("selected %v: facet = %v, want %v", selected, filters.GuardKinds, want)
			}
		}
		// Other facets narrow it: the window of one model holds no loop-ended request.
		filters, err := QueryFiltersForLogs(LogQueryParams{Days: days, Models: []string{"m-plain"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(filters.GuardKinds) != 0 {
			t.Fatalf("facet = %v", filters.GuardKinds)
		}
	})
}

func TestGuardKindSurvivesTheSpool(t *testing.T) {
	entry := normalizeRequestLogEntry(RequestLogEntry{
		APIKey: "sk-guard-test", Model: "m-spool", Failed: true, GuardKind: "reply_loop",
		Timestamp: time.Now(),
	})
	line, err := encodeUsageSpoolRecord(entry, usageSpoolReasonWriteFailed, time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeUsageSpoolRecord(line)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.GuardKind != "reply_loop" || !decoded.Failed {
		t.Fatalf("decoded = %+v", decoded)
	}
	// A line spooled before the field existed replays as a request nothing ended.
	legacy, _ := encodeUsageSpoolRecord(normalizeRequestLogEntry(RequestLogEntry{Model: "m-old", Timestamp: time.Now()}), usageSpoolReasonQueueFull, time.Time{}, time.Now())
	if decoded, _, err := decodeUsageSpoolRecord(legacy); err != nil || decoded.GuardKind != "" {
		t.Fatalf("legacy line: %+v, %v", decoded, err)
	}
}

func TestUsageRecordCarriesGuardKindIntoTheLogEntry(t *testing.T) {
	stats := NewRequestStatistics()
	entry := stats.ingest(context.Background(), coreusage.Record{
		APIKey: "sk-guard-test", Model: "m-record", Failed: true, GuardKind: "tool_loop",
		RequestedAt: time.Now(),
	})
	if entry.GuardKind != "tool_loop" || !entry.Failed {
		t.Fatalf("entry = failed %v kind %q", entry.Failed, entry.GuardKind)
	}
}
