package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// The client API keys must never leave the server through the usage endpoints.
// This fixture stores keys of every kind, writes their requests the way the
// runtime does, adds the rows an older build left behind (the secret in
// request_logs.api_key), and then reads every endpoint that reports on keys.
type plaintextKeyFixture struct {
	handler *Handler
	// secrets are every client secret the fixture ever stored or logged.
	secrets []string
}

const (
	fixtureStandalone = "sk-api-standalone-0001"
	fixtureOwnedA     = "sk-api-owned-a-0002"
	fixtureOwnedB     = "sk-api-owned-b-0003"
	fixtureForgotten  = "sk-api-forgotten-0004"
	fixtureLegacy     = "sk-api-legacy-0005"
)

func newPlaintextKeyFixture(t *testing.T) *plaintextKeyFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setupOwnedPublicUsageDB(t)

	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	for _, row := range []usage.APIKeyRow{
		{ID: "api-standalone", Key: fixtureStandalone, Name: "Standalone", CreatedAt: stamp, UpdatedAt: stamp},
		{ID: "api-owned-a", Key: fixtureOwnedA, Name: "Owned A", EndUserID: "api-user", IsDefault: true, CreatedAt: stamp, UpdatedAt: stamp},
		{ID: "api-owned-b", Key: fixtureOwnedB, Name: "Owned B", EndUserID: "api-user", CreatedAt: stamp, UpdatedAt: stamp},
	} {
		if err := usage.UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", row.ID, err)
		}
	}

	tokens := usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}
	at := func(i int) time.Time { return now.Add(-time.Duration(i) * time.Minute) }
	usage.InsertLog(fixtureStandalone, "", "gpt-test", "src", "ch", "auth", false, at(1), 10, 5, tokens, "", "")
	usage.InsertLog(fixtureStandalone, "", "gpt-test", "src", "ch", "auth", false, at(2), 10, 5, tokens, "", "")
	usage.InsertLog(fixtureOwnedA, "", "gpt-test", "src", "ch", "auth", false, at(3), 10, 5, tokens, "", "")
	usage.InsertLog(fixtureOwnedB, "", "gpt-test", "src", "ch", "auth", false, at(4), 10, 5, tokens, "", "")
	usage.InsertLog("POST /v1/models", "", "gpt-test", "src", "ch", "auth", false, at(5), 10, 5, tokens, "", "")
	usage.InsertLog(fixtureForgotten, "Forgotten", "gpt-test", "src", "ch", "auth", false, at(6), 10, 5, tokens, "", "")
	// Rows from before the key was masked: the secret in the column, with and
	// without a key id.
	for i, legacy := range []struct{ key, id, name string }{
		{fixtureStandalone, "", ""},
		{fixtureStandalone, "api-standalone", "Standalone"},
		{fixtureLegacy, "", "Legacy"},
	} {
		if _, err := usage.RuntimeDB().Exec(
			`INSERT INTO request_logs
				(timestamp, api_key, api_key_id, api_key_name, model, source, channel_name, auth_index,
				 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
			 VALUES (?, ?, ?, ?, 'gpt-test', 'src', 'ch', 'auth', 0, 10, 5, 1, 2, 0, 0, 3, 0)`,
			at(10+i).Format(time.RFC3339Nano), legacy.key, legacy.id, legacy.name,
		); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}

	// The in-memory statistics, as the executors feed them.
	wasEnabled := usage.StatisticsEnabled()
	usage.SetStatisticsEnabled(true)
	t.Cleanup(func() { usage.SetStatisticsEnabled(wasEnabled) })
	stats := usage.NewRequestStatistics()
	for _, rec := range []struct{ key, id string }{
		{fixtureStandalone, "api-standalone"},
		{fixtureOwnedA, "api-owned-a"},
		{fixtureForgotten, ""},
	} {
		stats.Record(context.Background(), coreusage.Record{
			APIKey: rec.key, APIKeyID: rec.id, Model: "gpt-test", RequestedAt: now,
			Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		})
	}

	h := NewHandler(&config.Config{}, "", nil)
	h.SetUsageStatistics(stats)
	return &plaintextKeyFixture{
		handler: h,
		secrets: []string{fixtureStandalone, fixtureOwnedA, fixtureOwnedB, fixtureForgotten, fixtureLegacy},
	}
}

func (f *plaintextKeyFixture) call(t *testing.T, handler gin.HandlerFunc, method, target, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	if body != "" {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	handler(c)
	return rec.Code, rec.Body.String()
}

func (f *plaintextKeyFixture) assertNoSecrets(t *testing.T, what, body string) {
	t.Helper()
	for _, secret := range f.secrets {
		if strings.Contains(body, secret) {
			t.Fatalf("%s carries the client secret %q:\n%s", what, secret, body)
		}
	}
}

func TestManagementUsageEndpointsNeverReturnClientSecrets(t *testing.T) {
	f := newPlaintextKeyFixture(t)
	logs := f.handler.UsageLogs()

	requests := []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		target  string
		body    string
	}{
		{"usage/logs", logs.GetUsageLogs, http.MethodGet, "/usage/logs?days=7&page=1&size=50", ""},
		{"usage/logs by key id", logs.GetUsageLogs, http.MethodGet, "/usage/logs?days=7&api_key_ids=api-standalone", ""},
		{"usage/logs system calls", logs.GetUsageLogs, http.MethodGet, "/usage/logs?days=7&api_key_ids=__system__", ""},
		{"usage/logs by account", logs.GetUsageLogs, http.MethodGet, "/usage/logs?days=7&end_user_id=api-user", ""},
		// Programmatic callers may still pass a secret as a selector; it is never
		// echoed back.
		{"usage/logs by secret selector", logs.GetUsageLogs, http.MethodGet, "/usage/logs?days=7&api_keys=" + url.QueryEscape(fixtureStandalone), ""},
		{"usage/chart-data", logs.GetUsageChartData, http.MethodGet, "/usage/chart-data?days=7", ""},
		{"usage/chart-data by secret", logs.GetUsageChartData, http.MethodGet, "/usage/chart-data?days=7&api_key=" + url.QueryEscape(fixtureStandalone), ""},
		{"usage/monitor/overview", logs.GetUsageMonitorOverview, http.MethodGet, "/usage/monitor/overview?range=7d", ""},
		{"usage", f.handler.GetUsageStatistics, http.MethodGet, "/usage", ""},
		{"usage/export", f.handler.ExportUsageStatistics, http.MethodGet, "/usage/export", ""},
		{"public/usage", f.handler.GetPublicUsageByAPIKey, http.MethodPost, "/public/usage", `{"api_key":"` + fixtureStandalone + `"}`},
		{"public/usage/logs", logs.GetPublicUsageLogs, http.MethodPost, "/public/usage/logs", `{"api_key":"` + fixtureStandalone + `","days":7}`},
		{"public/usage/logs owned", logs.GetPublicUsageLogs, http.MethodPost, "/public/usage/logs", `{"api_key":"` + fixtureOwnedA + `","days":7}`},
		{"public/usage/chart-data", logs.GetPublicUsageChartData, http.MethodPost, "/public/usage/chart-data", `{"api_key":"` + fixtureStandalone + `","days":7}`},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := f.call(t, tc.handler, tc.method, tc.target, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, body = %s", status, body)
			}
			f.assertNoSecrets(t, tc.name, body)
		})
	}
}

func TestManagementUsageLogsShowOnlyMasksAndKeyIDs(t *testing.T) {
	f := newPlaintextKeyFixture(t)
	status, body := f.call(t, f.handler.UsageLogs().GetUsageLogs, http.MethodGet, "/usage/logs?days=7&page=1&size=50", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var payload struct {
		Items []struct {
			APIKey       string `json:"api_key"`
			APIKeyMasked string `json:"api_key_masked"`
			APIKeyID     string `json:"api_key_id"`
		} `json:"items"`
		Filters struct {
			APIKeyIDs      []string         `json:"api_key_ids"`
			APIKeyIDCounts map[string]int64 `json:"api_key_id_counts"`
		} `json:"filters"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 6 logged requests, 3 legacy rows, and the 3 requests recorded with the statistics.
	if len(payload.Items) != 12 {
		t.Fatalf("items = %d, want every row of the fixture", len(payload.Items))
	}
	masks := map[string]bool{
		usage.MaskAPIKey(fixtureStandalone): true,
		usage.MaskAPIKey(fixtureOwnedA):     true,
		usage.MaskAPIKey(fixtureOwnedB):     true,
		usage.MaskAPIKey(fixtureForgotten):  true,
		usage.MaskAPIKey(fixtureLegacy):     true,
		"POST /v1/models":                   true,
	}
	for _, item := range payload.Items {
		if !masks[item.APIKey] || item.APIKeyMasked != item.APIKey {
			t.Fatalf("row shows api_key %q / api_key_masked %q, want a mask or a label", item.APIKey, item.APIKeyMasked)
		}
	}
	// The facets list keys by id, with the system calls as one entry.
	for _, want := range []string{"api-standalone", "api-owned-a", "__system__"} {
		found := false
		for _, id := range payload.Filters.APIKeyIDs {
			found = found || id == want
		}
		if !found {
			t.Fatalf("filters.api_key_ids = %v, missing %q", payload.Filters.APIKeyIDs, want)
		}
	}
	// api-owned-b is folded into the account's default key: 2 requests of key A
	// (one logged, one recorded) and 1 of key B.
	if payload.Filters.APIKeyIDCounts["api-owned-a"] != 3 {
		t.Fatalf("account count = %d, want both owned keys", payload.Filters.APIKeyIDCounts["api-owned-a"])
	}
	if !payload.Capabilities["api-key-id-filters"] {
		t.Fatalf("capabilities = %v", payload.Capabilities)
	}
}

func TestManagementUsageStatisticsAreKeyedByKeyID(t *testing.T) {
	f := newPlaintextKeyFixture(t)
	for _, tc := range []struct {
		name    string
		handler gin.HandlerFunc
		target  string
	}{
		{"usage", f.handler.GetUsageStatistics, "/usage"},
		{"usage/export", f.handler.ExportUsageStatistics, "/usage/export"},
	} {
		status, body := f.call(t, tc.handler, http.MethodGet, tc.target, "")
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.name, status)
		}
		var payload struct {
			Usage struct {
				APIs map[string]json.RawMessage `json:"apis"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		for _, bucket := range []string{"api-standalone", "api-owned-a", usage.MaskAPIKey(fixtureForgotten)} {
			if _, ok := payload.Usage.APIs[bucket]; !ok {
				t.Errorf("%s: apis = %v, missing bucket %q", tc.name, payload.Usage.APIs, bucket)
			}
		}
	}
}

func TestPublicUsageLookupAnswersToTheKeysOwnID(t *testing.T) {
	f := newPlaintextKeyFixture(t)
	status, body := f.call(t, f.handler.GetPublicUsageByAPIKey, http.MethodPost, "/public/usage", `{"api_key":"`+fixtureStandalone+`"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var payload struct {
		Found bool `json:"found"`
		Usage struct {
			APIs map[string]usage.APISnapshot `json:"apis"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !payload.Found || len(payload.Usage.APIs) != 1 {
		t.Fatalf("response = %s", body)
	}
	for _, snapshot := range payload.Usage.APIs {
		if snapshot.TotalRequests != 1 {
			t.Fatalf("snapshot = %+v, want the one recorded request of this key", snapshot)
		}
	}

	// A secret the system does not store has nothing to look up, however much
	// usage was once recorded under it.
	status, body = f.call(t, f.handler.GetPublicUsageByAPIKey, http.MethodPost, "/public/usage", `{"api_key":"`+fixtureForgotten+`"}`)
	if status != http.StatusOK || !strings.Contains(body, `"found":false`) {
		t.Fatalf("unknown key: status %d, body %s", status, body)
	}
}

func TestSystemStatsMaskTheKeysOfActiveConcurrency(t *testing.T) {
	snapshots := []middleware.ConcurrencySnapshot{
		{APIKey: fixtureStandalone, RPM: 3},
		{APIKey: "short", RPM: 1},
	}
	maskConcurrencyKeys(snapshots)
	if snapshots[0].APIKey != usage.MaskAPIKey(fixtureStandalone) || snapshots[1].APIKey != usage.MaskAPIKey("short") {
		t.Fatalf("snapshots = %+v, want masked keys", snapshots)
	}
	encoded, err := json.Marshal(snapshots)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), fixtureStandalone) {
		t.Fatalf("snapshot JSON carries the secret: %s", encoded)
	}
}
