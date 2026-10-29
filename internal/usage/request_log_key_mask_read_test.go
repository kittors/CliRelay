package usage

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestSystemCallsAreOneEntryInTheKeyFacetAndFilter(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	assertSystemCallsAreOneEntryInTheKeyFacetAndFilter(t)
}

// assertSystemCallsAreOneEntryInTheKeyFacetAndFilter runs on SQLite and on PostgreSQL.
func assertSystemCallsAreOneEntryInTheKeyFacetAndFilter(t *testing.T) {
	t.Helper()
	registerTestAPIKey(t, "read-key-id", "sk-read-secret-0001", "Reader")

	now := time.Now().UTC()
	stats := TokenStats{TotalTokens: 1}
	InsertLog("sk-read-secret-0001", "", "m1", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("sk-read-secret-0001", "", "m2", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	// System calls: no client key at all, a route, an internal origin.
	InsertLog("POST /auth-files/warmup", "", "m3", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("POST /auth-files/warmup", "", "m4", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("warmup", "", "m5", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("", "", "m6", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	// A named key that no longer exists is a person's usage, not a system call.
	InsertLog("sk-read-forgotten-0002", "Forgotten", "m7", "src", "ch", "auth", false, now, 1, 1, stats, "", "")

	filters, err := QueryFiltersForLogs(LogQueryParams{Days: 7})
	if err != nil {
		t.Fatalf("QueryFiltersForLogs: %v", err)
	}
	if !slices.Equal(filters.APIKeyIDs, []string{"read-key-id", SystemRequestLogFilterValue}) {
		t.Fatalf("filters.APIKeyIDs = %#v, want the key then the system entry", filters.APIKeyIDs)
	}
	if filters.APIKeyIDCounts["read-key-id"] != 2 || filters.APIKeyIDCounts[SystemRequestLogFilterValue] != 4 {
		t.Fatalf("filters.APIKeyIDCounts = %#v, want 2 for the key and 4 system calls", filters.APIKeyIDCounts)
	}
	if filters.APIKeyIDNames["read-key-id"] != "Reader" {
		t.Fatalf("filters.APIKeyIDNames = %#v", filters.APIKeyIDNames)
	}

	total := func(params LogQueryParams) int64 {
		t.Helper()
		params.Page, params.Size, params.Days = 1, 50, 7
		result, err := QueryLogs(params)
		if err != nil {
			t.Fatalf("QueryLogs(%+v): %v", params, err)
		}
		return result.Total
	}
	for _, tc := range []struct {
		name   string
		params LogQueryParams
		want   int64
	}{
		{"no filter", LogQueryParams{}, 7},
		{"the key", LogQueryParams{APIKeyIDs: []string{"read-key-id"}}, 2},
		{"system calls", LogQueryParams{APIKeyIDs: []string{SystemRequestLogFilterValue}}, 4},
		{"key and system calls", LogQueryParams{APIKeyIDs: []string{"read-key-id", SystemRequestLogFilterValue}}, 6},
		{"system calls by selector", LogQueryParams{APIKeys: []string{SystemRequestLogFilterValue}}, 4},
		{"an id nobody has", LogQueryParams{APIKeyIDs: []string{"no-such-key"}}, 0},
		{"explicit empty selection", LogQueryParams{MatchNoAPIKeyIDs: true}, 0},
		// The api_key column holds a mask; a secret the system does not know cannot
		// select anything through it.
		{"an unknown secret", LogQueryParams{APIKeys: []string{"sk-read-forgotten-0002"}}, 0},
		{"the key by its secret", LogQueryParams{APIKeys: []string{"sk-read-secret-0001"}}, 2},
	} {
		if got := total(tc.params); got != tc.want {
			t.Errorf("%s: total = %d, want %d", tc.name, got, tc.want)
		}
	}

	// The facet narrows with the other filters like any other.
	narrowed, err := QueryFiltersForLogs(LogQueryParams{Days: 7, Models: []string{"m3"}})
	if err != nil {
		t.Fatalf("QueryFiltersForLogs(models): %v", err)
	}
	if !slices.Equal(narrowed.APIKeyIDs, []string{SystemRequestLogFilterValue}) || narrowed.APIKeyIDCounts[SystemRequestLogFilterValue] != 1 {
		t.Fatalf("narrowed facet = %#v / %#v", narrowed.APIKeyIDs, narrowed.APIKeyIDCounts)
	}
}

func TestKeyFacetLabelsAnUnnamedKeyByItsMask(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	assertKeyFacetLabelsAnUnnamedKeyByItsMask(t)
}

// assertKeyFacetLabelsAnUnnamedKeyByItsMask runs on SQLite and on PostgreSQL.
func assertKeyFacetLabelsAnUnnamedKeyByItsMask(t *testing.T) {
	t.Helper()
	const (
		unnamed = "sk-facet-unnamed-0001"
		legacy  = "sk-facet-legacy-0002"
	)
	registerTestAPIKey(t, "facet-unnamed-id", unnamed, "")
	registerTestAPIKey(t, "facet-legacy-id", legacy, "")
	registerTestAPIKey(t, "facet-named-id", "sk-facet-named-0003", "Named")

	now := time.Now().UTC()
	stats := TokenStats{TotalTokens: 1}
	InsertLog(unnamed, "", "m1", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("sk-facet-named-0003", "", "m2", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	// A row an older build wrote still holds the secret; the label is built from
	// the display form, never from the stored text.
	insertRawRequestLog(t, legacy, "facet-legacy-id", "", now)
	// A key that has been deleted keeps its history under the id; the mask its
	// rows show is all that is left to call it by.
	insertRawRequestLog(t, MaskAPIKey("sk-facet-deleted-0004"), "facet-deleted-id", "", now)
	// A route that carries an id by accident names no key.
	insertRawRequestLog(t, "POST /v1/models", "facet-route-id", "", now)

	filters, err := QueryFiltersForLogs(LogQueryParams{Days: 7})
	if err != nil {
		t.Fatalf("QueryFiltersForLogs: %v", err)
	}
	want := map[string]string{
		"facet-unnamed-id": MaskAPIKey(unnamed),
		"facet-legacy-id":  MaskAPIKey(legacy),
		"facet-deleted-id": MaskAPIKey("sk-facet-deleted-0004"),
		"facet-named-id":   "Named",
	}
	for id, label := range want {
		if got := filters.APIKeyIDNames[id]; got != label {
			t.Errorf("label of %s = %q, want %q", id, got, label)
		}
	}
	if label, ok := filters.APIKeyIDNames["facet-route-id"]; ok {
		t.Errorf("a route was offered as the name of a key: %q", label)
	}
	encoded, err := json.Marshal(filters)
	if err != nil {
		t.Fatalf("marshal filters: %v", err)
	}
	for _, secret := range []string{unnamed, legacy, "sk-facet-deleted-0004"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the facet carries the secret %q: %s", secret, encoded)
		}
	}
}

func TestRowsThatStillHoldASecretAreReturnedMasked(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	assertRowsThatStillHoldASecretAreReturnedMasked(t)
}

// assertRowsThatStillHoldASecretAreReturnedMasked runs on SQLite and on PostgreSQL.
func assertRowsThatStillHoldASecretAreReturnedMasked(t *testing.T) {
	t.Helper()
	registerTestAPIKey(t, "legacy-read-key", "sk-legacy-read-0001", "Legacy reader")

	const unknown = "sk-legacy-unknown-0002"
	now := time.Now().UTC()
	// Rows an older build wrote: the secret is in api_key, with and without an id.
	insertRawRequestLog(t, "sk-legacy-read-0001", "", "", now)
	insertRawRequestLog(t, "sk-legacy-read-0001", "legacy-read-key", "Legacy reader", now.Add(time.Second))
	insertRawRequestLog(t, unknown, "", "Ghost", now.Add(2*time.Second))
	insertRawRequestLog(t, "POST /v1/models", "", "", now.Add(3*time.Second))

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 50, Days: 7})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(result.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(result.Items))
	}
	want := map[string]bool{
		MaskAPIKey("sk-legacy-read-0001"): false,
		MaskAPIKey(unknown):               false,
		"POST /v1/models":                 false,
	}
	for _, item := range result.Items {
		if item.APIKey != item.APIKeyMasked {
			t.Fatalf("api_key %q and api_key_masked %q differ", item.APIKey, item.APIKeyMasked)
		}
		if _, ok := want[item.APIKey]; !ok {
			t.Fatalf("unexpected api_key %q in %#v", item.APIKey, item)
		}
		want[item.APIKey] = true
	}
	for value, seen := range want {
		if !seen {
			t.Errorf("no row shows %q", value)
		}
	}
	// A legacy row without an id is attributed to the key that owns its secret,
	// so it reads like the rest.
	for _, item := range result.Items {
		if item.APIKey == MaskAPIKey("sk-legacy-read-0001") && item.APIKeyID != "legacy-read-key" {
			t.Fatalf("legacy row not attributed: %#v", item)
		}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"sk-legacy-read-0001", unknown} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("QueryLogs result carries the secret %q: %s", secret, encoded)
		}
	}

	// The single-row read used by detail views and the egress probe.
	for _, item := range result.Items {
		row, err := QueryLogRowByID(item.ID)
		if err != nil {
			t.Fatalf("QueryLogRowByID(%d): %v", item.ID, err)
		}
		if strings.Contains(row.APIKey, "legacy-read-0001") || strings.Contains(row.APIKey, "legacy-unknown-0002") {
			t.Fatalf("QueryLogRowByID returned %q", row.APIKey)
		}
	}
}

func TestAPIKeyDistributionKeepsRowsWithoutAKeyIDAsTheirOwnSlices(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	assertAPIKeyDistributionKeepsRowsWithoutAKeyIDAsTheirOwnSlices(t)
}

// assertAPIKeyDistributionKeepsRowsWithoutAKeyIDAsTheirOwnSlices runs on SQLite and on PostgreSQL.
func assertAPIKeyDistributionKeepsRowsWithoutAKeyIDAsTheirOwnSlices(t *testing.T) {
	t.Helper()
	registerTestAPIKey(t, "dist-key-id", "sk-dist-secret-0001", "Dist")

	now := time.Now().UTC()
	insertRawRequestLog(t, "sk-dist-secret-0001", "dist-key-id", "Dist", now)
	insertRawRequestLog(t, "sk-dist-secret-0001", "dist-key-id", "Dist", now.Add(time.Second))
	// A deleted key's history after the pass: a mask, no id, its name snapshot.
	insertRawRequestLog(t, MaskAPIKey("sk-dist-deleted-0002"), "", "Deleted user", now.Add(2*time.Second))
	// Routes and origins have always been their own slices.
	insertRawRequestLog(t, "POST /auth-files/warmup", "", "", now.Add(3*time.Second))
	insertRawRequestLog(t, "", "", "", now.Add(4*time.Second))

	dist, err := QueryAPIKeyDistribution(7)
	if err != nil {
		t.Fatalf("QueryAPIKeyDistribution: %v", err)
	}
	type slice struct {
		id, key, name string
		requests      int64
	}
	var got []slice
	for _, p := range dist {
		got = append(got, slice{p.APIKeyID, p.APIKey, p.Name, p.Requests})
	}
	want := []slice{
		{"dist-key-id", MaskAPIKey("sk-dist-secret-0001"), "Dist", 2},
		{"", MaskAPIKey("sk-dist-deleted-0002"), "Deleted user", 1},
		{"", "POST /auth-files/warmup", "", 1},
	}
	if len(got) != len(want) {
		t.Fatalf("distribution = %#v, want %#v", got, want)
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("distribution = %#v, missing %#v", got, w)
		}
	}
	if got[0] != want[0] {
		t.Errorf("first slice = %#v, want the busiest key %#v", got[0], want[0])
	}
}

func TestEndUserPredicateMatchesTheAccountsKeyIDsOnly(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Now().UTC().Format(time.RFC3339)
	for _, row := range []APIKeyRow{
		{ID: "acct-key-1", Key: "sk-acct-secret-0001", Name: "One", EndUserID: "acct-user", CreatedAt: now, UpdatedAt: now},
		{ID: "acct-key-2", Key: "sk-acct-secret-0002", Name: "Two", EndUserID: "acct-user", CreatedAt: now, UpdatedAt: now},
		{ID: "other-key", Key: "sk-other-secret-0003", Name: "Other", EndUserID: "other-user", CreatedAt: now, UpdatedAt: now},
	} {
		if err := UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", row.ID, err)
		}
	}

	predicate, args := buildEndUserAPIKeySelectorPredicate(systemTenantID, "acct-user")
	if predicate != "api_key_id IN (?,?)" {
		t.Fatalf("predicate = %q, want an id list", predicate)
	}
	for _, arg := range args {
		if s, _ := arg.(string); strings.HasPrefix(s, "sk-") {
			t.Fatalf("a secret is bound into the predicate: %v", args)
		}
	}
	if predicate, args := buildEndUserAPIKeySelectorPredicate(systemTenantID, "nobody"); predicate != "1 = 0" || len(args) != 0 {
		t.Fatalf("account without keys: predicate %q args %v, want a match-nothing", predicate, args)
	}

	stats := TokenStats{TotalTokens: 1}
	InsertLog("sk-acct-secret-0001", "", "m", "src", "ch", "auth", false, time.Now().UTC(), 1, 1, stats, "", "")
	InsertLog("sk-acct-secret-0002", "", "m", "src", "ch", "auth", false, time.Now().UTC(), 1, 1, stats, "", "")
	InsertLog("sk-other-secret-0003", "", "m", "src", "ch", "auth", false, time.Now().UTC(), 1, 1, stats, "", "")
	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 50, Days: 7, EndUserID: "acct-user"})
	if err != nil || result.Total != 2 {
		t.Fatalf("account logs: total=%d err=%v, want the account's two keys", result.Total, err)
	}
}
