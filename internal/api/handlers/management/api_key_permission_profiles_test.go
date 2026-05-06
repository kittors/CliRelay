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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
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
