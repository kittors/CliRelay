package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v8/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestInvalidateAIAccountCachesOnlyAccountTrend(t *testing.T) {
	h := &Handler{trendCache: map[string]trendCacheEntry{
		"dashboard-summary:tenant:7:all":    {expiresAt: time.Now().Add(time.Minute), payload: "dash"},
		"entity-stats:tenant:key:7:":        {expiresAt: time.Now().Add(time.Minute), payload: "entity"},
		"auth-file-trend:tenant:auth-1:7:5": {expiresAt: time.Now().Add(time.Minute), payload: "trend"},
		"auth-file-trend:tenant:auth-2:7:5": {expiresAt: time.Now().Add(time.Minute), payload: "other"},
		"tenant:codex:7":                    {expiresAt: time.Now().Add(time.Minute), payload: "group"},
	}}
	h.invalidateAIAccountCaches("tenant", "auth-1", "")

	if _, ok := h.trendCache["dashboard-summary:tenant:7:all"]; !ok {
		t.Fatal("dashboard must survive")
	}
	if _, ok := h.trendCache["entity-stats:tenant:key:7:"]; !ok {
		t.Fatal("entity-stats must survive")
	}
	if _, ok := h.trendCache["tenant:codex:7"]; !ok {
		t.Fatal("group trend must survive")
	}
	if _, ok := h.trendCache["auth-file-trend:tenant:auth-1:7:5"]; ok {
		t.Fatal("target account trend should clear")
	}
	if _, ok := h.trendCache["auth-file-trend:tenant:auth-2:7:5"]; !ok {
		t.Fatal("other account trend must survive")
	}
}

func TestInvalidateAIAccountCachesClearsAllSubjectAliases(t *testing.T) {
	// Two aliases same account_id => same subject; one unrelated.
	aliasA := &coreauth.Auth{
		ID: "id-a", TenantID: "tenant", Provider: "codex", FileName: "a.json",
		Metadata: map[string]any{"account_id": "shared"},
	}
	aliasB := &coreauth.Auth{
		ID: "id-b", TenantID: "tenant", Provider: "codex", FileName: "b.json",
		Metadata: map[string]any{"account_id": "shared"},
	}
	other := &coreauth.Auth{
		ID: "id-c", TenantID: "tenant", Provider: "codex", FileName: "c.json",
		Metadata: map[string]any{"account_id": "other"},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, a := range []*coreauth.Auth{aliasA, aliasB, other} {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	// Re-list to get assigned indexes.
	var idxA, idxB, idxC, subject string
	for _, a := range manager.ListForTenant("tenant") {
		id := usage.ResolveAuthSubjectIdentity(a)
		switch a.ID {
		case "id-a":
			idxA = a.Index
			subject = id.ID
		case "id-b":
			idxB = a.Index
		case "id-c":
			idxC = a.Index
		}
	}
	if idxA == "" || idxB == "" || idxC == "" || subject == "" {
		t.Fatalf("indexes a=%q b=%q c=%q subject=%q", idxA, idxB, idxC, subject)
	}

	h := &Handler{
		authManager: manager,
		trendCache: map[string]trendCacheEntry{
			"dashboard-summary:tenant:7:all":          {expiresAt: time.Now().Add(time.Minute), payload: "dash"},
			"entity-stats:tenant:key:7:":              {expiresAt: time.Now().Add(time.Minute), payload: "entity"},
			"auth-file-trend:tenant:" + idxA + ":7:5": {expiresAt: time.Now().Add(time.Minute), payload: "a"},
			"auth-file-trend:tenant:" + idxB + ":7:5": {expiresAt: time.Now().Add(time.Minute), payload: "b"},
			"auth-file-trend:tenant:" + idxC + ":7:5": {expiresAt: time.Now().Add(time.Minute), payload: "c"},
			"tenant:codex:7":                          {expiresAt: time.Now().Add(time.Minute), payload: "group"},
		},
	}
	// Refresh only reports alias A index; subject expansion must clear B too.
	h.invalidateAIAccountCaches("tenant", idxA, subject)

	if _, ok := h.trendCache["dashboard-summary:tenant:7:all"]; !ok {
		t.Fatal("dashboard must survive")
	}
	if _, ok := h.trendCache["entity-stats:tenant:key:7:"]; !ok {
		t.Fatal("entity-stats must survive")
	}
	if _, ok := h.trendCache["tenant:codex:7"]; !ok {
		t.Fatal("group trend must survive")
	}
	if _, ok := h.trendCache["auth-file-trend:tenant:"+idxA+":7:5"]; ok {
		t.Fatal("alias A trend should clear")
	}
	if _, ok := h.trendCache["auth-file-trend:tenant:"+idxB+":7:5"]; ok {
		t.Fatal("alias B trend should clear (same subject)")
	}
	if _, ok := h.trendCache["auth-file-trend:tenant:"+idxC+":7:5"]; !ok {
		t.Fatal("unrelated alias C trend must survive")
	}
}

func TestGetAIAccountStatusEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
	})

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/ai-accounts/status", nil)
	h.GetAIAccountStatus(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAIAccountStatusPermissionMapping(t *testing.T) {
	if got := ManagementRequestPermission(http.MethodGet, "/v0/management/ai-accounts/status"); got != "auth_files.read" {
		t.Fatalf("GET status perm = %q", got)
	}
	if got := ManagementRequestPermission(http.MethodPost, "/v0/management/ai-accounts/status-refresh"); got != "auth_files.write" {
		t.Fatalf("POST refresh perm = %q", got)
	}
	if got := ManagementRequestPermission(http.MethodGet, "/v0/management/ai-accounts/status-refresh/job-1"); got != "auth_files.read" {
		t.Fatalf("GET job perm = %q", got)
	}
}

func TestListAuthFilesSubjectComesFromBuildEntryOnly(t *testing.T) {
	// auth_subject_id is produced by BuildEntry/ListEntries — no second enrichment pass.
	authA := &coreauth.Auth{
		ID: "auth-a", TenantID: "tenant-a", Provider: "codex", FileName: "a.json",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"account_id": "acct-a"},
	}
	entry := managementauthfiles.BuildEntry(authA, managementauthfiles.EntryOptions{})
	if entry == nil {
		t.Fatal("nil entry")
	}
	identityA := usage.ResolveAuthSubjectIdentity(authA)
	if identityA == nil || entry["auth_subject_id"] != identityA.ID {
		t.Fatalf("entry=%+v identity=%+v", entry, identityA)
	}
}
