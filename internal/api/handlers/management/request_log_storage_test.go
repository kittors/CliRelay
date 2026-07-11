package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestPutRequestLogBodyStorageRequiresExplicitCleanupConfirmation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/request-log-storage/store-content", bytes.NewBufferString(`{"value":false}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutRequestLogBodyStorage(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPutRequestLogBodyStorageDisablesWritesAndClearsBodiesOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	usage.InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, time.Now().UTC(), 100, 10, usage.TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, `{"request":"secret"}`, `{"response":"secret"}`, `{"client":{"ip":"203.0.113.8"}}`)
	logs, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil || len(logs.Items) != 1 {
		t.Fatalf("QueryLogs() result=%+v error=%v", logs, err)
	}
	logID := logs.Items[0].ID

	cfg := &config.Config{}
	cfg.RequestLogStorage.StoreContent = true
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(cfg, configPath, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/request-log-storage/store-content", bytes.NewBufferString(`{"value":false,"clear_existing":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutRequestLogBodyStorage(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if cfg.RequestLogStorage.StoreContent || usage.RequestLogBodyStorageEnabled() {
		t.Fatal("request log body storage remained enabled")
	}
	input, err := usage.QueryLogContentPart(logID, "input")
	if err != nil || input.Content != "" {
		t.Fatalf("input content=%q error=%v, want cleared", input.Content, err)
	}
	details, err := usage.QueryLogContentPart(logID, "details")
	if err != nil || details.Content == "" {
		t.Fatalf("details content=%q error=%v, want preserved", details.Content, err)
	}
}
