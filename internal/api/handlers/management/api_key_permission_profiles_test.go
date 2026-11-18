package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func setupPermissionProfilesTestDB(t *testing.T) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "usage-permission-profiles-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if _, err := usage.RuntimeDB().Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			permission_profile_id TEXT NOT NULL DEFAULT '',
			daily_limit INTEGER NOT NULL DEFAULT 0,
			total_quota INTEGER NOT NULL DEFAULT 0,
			spending_limit REAL NOT NULL DEFAULT 0,
			daily_spending_limit REAL NOT NULL DEFAULT 0,
			concurrency_limit INTEGER NOT NULL DEFAULT 0,
			rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_channels TEXT NOT NULL DEFAULT '[]',
			allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
			system_prompt TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
}

func TestPutAPIKeyPermissionProfilesSyncsBoundAccountsAtomically(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)
	tenantID := "00000000-0000-0000-0000-000000000001"
	for _, row := range []struct {
		id        string
		profileID string
	}{
		{id: "user-a", profileID: "standard"},
		{id: "user-b", profileID: "other"},
		{id: "user-c", profileID: "standard"},
	} {
		if _, err := usage.RuntimeDB().Exec(`
			INSERT INTO end_users (id, tenant_id, permission_profile_id, daily_limit, updated_at)
			VALUES (?, ?, ?, 1, '2026-01-01T00:00:00Z')
		`, row.id, tenantID, row.profileID); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	body := []byte(`{
	  "sync-accounts": true,
	  "items": [{
	    "id": "standard",
	    "name": "Standard",
	    "daily-limit": 16000,
	    "total-quota": 32000,
	    "daily-spending-limit": 50,
	    "concurrency-limit": 2,
	    "rpm-limit": 30,
	    "tpm-limit": 40000,
	    "allowed-models": ["gpt-5.4"],
	    "allowed-channels": ["kimi-B"],
	    "allowed-channel-groups": ["pro"],
	    "system-prompt": "Account prompt"
	  }]
	}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-key-permission-profiles", bytes.NewReader(body))
	h := NewHandler(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "kimi-B", BaseURL: "https://example.invalid"}},
		Routing:             config.RoutingConfig{ChannelGroups: []config.RoutingChannelGroup{{Name: "pro"}}},
	}, "", nil)
	h.PutAPIKeyPermissionProfiles(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		AppliedCount int64 `json:"applied_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.AppliedCount != 3 {
		t.Fatalf("applied_count = %d, want 3 (two synced + one removed binding)", response.AppliedCount)
	}

	for _, userID := range []string{"user-a", "user-c"} {
		var profileID, modelsJSON, groupsJSON, prompt string
		var dailyLimit, totalQuota, concurrency, rpm, tpm int
		var dailySpending float64
		if err := usage.RuntimeDB().QueryRow(`
			SELECT permission_profile_id, daily_limit, total_quota, daily_spending_limit,
				concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channel_groups, system_prompt
			FROM end_users WHERE id = ?
		`, userID).Scan(&profileID, &dailyLimit, &totalQuota, &dailySpending, &concurrency, &rpm, &tpm, &modelsJSON, &groupsJSON, &prompt); err != nil {
			t.Fatalf("query %s: %v", userID, err)
		}
		if profileID != "standard" || dailyLimit != 16000 || totalQuota != 32000 || dailySpending != 50 || concurrency != 2 || rpm != 30 || tpm != 40000 || modelsJSON != `["gpt-5.4"]` || groupsJSON != `["pro"]` || prompt != "Account prompt" {
			t.Fatalf("synced %s = profile:%q daily:%d total:%d spend:%v concurrency:%d rpm:%d tpm:%d models:%s groups:%s prompt:%q", userID, profileID, dailyLimit, totalQuota, dailySpending, concurrency, rpm, tpm, modelsJSON, groupsJSON, prompt)
		}
	}
	var removedProfileID string
	if err := usage.RuntimeDB().QueryRow(`SELECT permission_profile_id FROM end_users WHERE id = 'user-b'`).Scan(&removedProfileID); err != nil {
		t.Fatalf("query user-b: %v", err)
	}
	if removedProfileID != "" {
		t.Fatalf("removed profile binding = %q, want empty", removedProfileID)
	}
}

func TestAPIKeyPermissionProfilesManagementHandlersUseDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	body := []byte(`[
  {
    "id": "mixed-gpt-opencode",
    "name": "混合 gpt+opencode 模型",
    "daily-limit": 15000,
    "total-quota": 0,
    "concurrency-limit": 0,
    "rpm-limit": 0,
    "tpm-limit": 0,
    "allowed-channel-groups": ["chatgpt-mix", "opencode"],
    "allowed-channels": [],
    "allowed-models": [],
    "system-prompt": ""
  }
]`)

	putRec := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRec)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/api-key-permission-profiles", bytes.NewReader(body))

	h := NewHandler(&config.Config{}, "", nil)
	h.PutAPIKeyPermissionProfiles(putCtx)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/api-key-permission-profiles", nil)

	h.GetAPIKeyPermissionProfiles(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}

	var got struct {
		Profiles []usage.APIKeyPermissionProfileRow `json:"api-key-permission-profiles"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if len(got.Profiles) != 1 {
		t.Fatalf("profiles len = %d, want 1", len(got.Profiles))
	}
	if got.Profiles[0].ID != "mixed-gpt-opencode" || got.Profiles[0].DailyLimit != 15000 {
		t.Fatalf("profile = %#v", got.Profiles[0])
	}
	if len(got.Profiles[0].AllowedChannelGroups) != 2 || got.Profiles[0].AllowedChannelGroups[1] != "opencode" {
		t.Fatalf("allowed-channel-groups = %#v", got.Profiles[0].AllowedChannelGroups)
	}
}

func TestPutAPIKeyPermissionProfilesRejectsMissingIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-key-permission-profiles", bytes.NewReader([]byte(`[{"id":"","name":""}]`)))

	h := NewHandler(&config.Config{}, "", nil)
	h.PutAPIKeyPermissionProfiles(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPutAPIKeyPermissionProfilesPrunesUnknownChannelsBeforeSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	body := []byte(`[
  {
    "id": "standard",
    "name": "Standard",
    "allowed-channels": ["kimi-A", "kimi-B"]
  }
]`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api-key-permission-profiles", bytes.NewReader(body))

	h := NewHandler(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "kimi-B", BaseURL: "https://example.invalid"},
		},
	}, "", nil)
	h.PutAPIKeyPermissionProfiles(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	profiles := usage.ListAPIKeyPermissionProfiles()
	if len(profiles) != 1 {
		t.Fatalf("profiles len = %d, want 1", len(profiles))
	}
	if containsString(profiles[0].AllowedChannels, "kimi-A") {
		t.Fatalf("allowed-channels = %v, should not keep unknown channel", profiles[0].AllowedChannels)
	}
	if !containsString(profiles[0].AllowedChannels, "kimi-B") {
		t.Fatalf("allowed-channels = %v, should keep known channel", profiles[0].AllowedChannels)
	}
}

func TestPatchAPIKeyEntryPersistsPermissionProfileID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: "sk-bound-profile", Name: "Bound"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	body := []byte(`{
  "match": "sk-bound-profile",
  "value": {
    "permission-profile-id": "standard",
    "daily-limit": 15000,
    "allowed-channel-groups": ["pro"]
  }
}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api-key-entries", bytes.NewReader(body))

	h := NewHandler(&config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{{Name: "pro"}},
		},
	}, "", nil)
	h.PatchAPIKeyEntry(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got := usage.GetAPIKey("sk-bound-profile")
	if got == nil {
		t.Fatal("expected API key after PATCH")
	}
	if got.PermissionProfileID != "standard" {
		t.Fatalf("permission profile id = %q, want standard", got.PermissionProfileID)
	}
}
