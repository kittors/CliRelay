package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func initClientLogsDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})
}

type clientLogsPayload struct {
	Items []map[string]any `json:"items"`
	Total int64            `json:"total"`
	// Pointers tell a missing key from an empty one.
	Filters struct {
		ClientNames      *[]string         `json:"client_names"`
		ClientNameCounts *map[string]int64 `json:"client_name_counts"`
	} `json:"filters"`
	Capabilities map[string]bool `json:"capabilities"`
}

func getManagementLogs(t *testing.T, query string) clientLogsPayload {
	t.Helper()
	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50"+query, nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var payload clientLogsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return payload
}

func TestGetUsageLogsShowsAndFiltersTheCaller(t *testing.T) {
	initClientLogsDB(t)
	now := time.Now().UTC().Add(-time.Minute)
	for i, row := range []struct{ model, name, version, ua string }{
		{"m-desktop", "codex-desktop", "0.169.0", "Codex Desktop/0.169.0 (Mac OS 27.2.1; arm64)"},
		{"m-cli", "codex-cli", "0.169.0", "codex-tui/0.169.0 (Mac OS 27.0.1; arm64)"},
		{"m-none", "", "", ""},
	} {
		usage.InsertRequestLog(usage.RequestLogEntry{
			Model: row.model, Source: "codex", ClientName: row.name, ClientVersion: row.version, UserAgent: row.ua,
			Timestamp: now.Add(time.Duration(i) * time.Millisecond),
			Tokens:    usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		})
	}

	all := getManagementLogs(t, "")
	if len(all.Items) != 3 {
		t.Fatalf("items = %d", len(all.Items))
	}
	// The keys are there on every row, empty or not: the panel reads them to
	// tell this backend from an older one.
	for _, item := range all.Items {
		for _, key := range []string{"client_name", "client_version", "user_agent"} {
			if _, ok := item[key]; !ok {
				t.Fatalf("row %v has no %s", item["model"], key)
			}
		}
	}
	if all.Items[1]["client_name"] != "codex-cli" || all.Items[1]["user_agent"] != "codex-tui/0.169.0 (Mac OS 27.0.1; arm64)" {
		t.Fatalf("row = %v", all.Items[1])
	}
	if !all.Capabilities["request-log-clients"] {
		t.Fatalf("capabilities = %v", all.Capabilities)
	}
	if all.Filters.ClientNames == nil || all.Filters.ClientNameCounts == nil {
		t.Fatalf("filters carry no client facet")
	}
	// One request each: ties go by name, and the rows without one ("") first.
	if want := []string{usage.ClientNameUnknown, "codex-cli", "codex-desktop"}; !reflect.DeepEqual(*all.Filters.ClientNames, want) {
		t.Fatalf("client_names = %v, want %v", *all.Filters.ClientNames, want)
	}

	if got := getManagementLogs(t, "&client_names=codex-cli"); got.Total != 1 || got.Items[0]["model"] != "m-cli" {
		t.Fatalf("client_names=codex-cli: %d rows", got.Total)
	}
	if got := getManagementLogs(t, "&client_name=codex-desktop&client_names="+usage.ClientNameUnknown); got.Total != 2 {
		t.Fatalf("desktop and unknown: %d rows", got.Total)
	}
	if got := getManagementLogs(t, "&client_names_empty=true"); got.Total != 0 {
		t.Fatalf("an explicit empty selection listed %d rows", got.Total)
	}
}

func TestUsageLogsClientFacetIsNeverNull(t *testing.T) {
	initClientLogsDB(t)
	payload := getManagementLogs(t, "")
	if payload.Filters.ClientNames == nil || *payload.Filters.ClientNames == nil || payload.Filters.ClientNameCounts == nil || *payload.Filters.ClientNameCounts == nil {
		t.Fatalf("management filters = %+v", payload.Filters)
	}

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/usage/logs", bytes.NewReader([]byte(`{"api_key":"sk-test","days":7}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UsageLogs().GetPublicUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var public clientLogsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if public.Filters.ClientNames == nil || *public.Filters.ClientNames == nil || public.Filters.ClientNameCounts == nil || *public.Filters.ClientNameCounts == nil {
		t.Fatalf("public filters = %s", rec.Body.String())
	}
}

func TestPublicLookupRequestReadsTheClientFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/usage/logs?client_name=curl&client_names_empty=1",
		bytes.NewReader([]byte(`{"api_key":"sk-test","client_names":["codex-cli","curl"]}`)))
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		t.Fatalf("status %d: %s", status, message)
	}
	if want := []string{"codex-cli", "curl"}; !reflect.DeepEqual(req.ClientNames, want) || !req.ClientNamesEmpty {
		t.Fatalf("client names %v, empty %v", req.ClientNames, req.ClientNamesEmpty)
	}
}
