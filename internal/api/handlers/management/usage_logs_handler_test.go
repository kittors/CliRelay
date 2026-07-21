package management

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetUsageLogsResolvesLegacySourceChannelName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "oauth-auth-logs",
		FileName: "codex-test.json",
		Provider: "codex",
		Label:    "GPT1",
		Metadata: map[string]any{
			"label": "GPT1",
			"email": "pcamtu927@gmail.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	usage.InsertLog(
		"", "", "gpt-5.4", "pcamtu927@gmail.com", "pcamtu927@gmail.com", auth.Index,
		false, time.Now().UTC(), 123, 45,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50", nil)

	h.UsageLogs().GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			ChannelName  string `json:"channel_name"`
			AuthIndex    string `json:"auth_index"`
			Streaming    bool   `json:"streaming"`
			FirstTokenMs int64  `json:"first_token_ms"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}
	if payload.Items[0].AuthIndex != auth.Index {
		t.Fatalf("auth_index = %q, want %q", payload.Items[0].AuthIndex, auth.Index)
	}
	if payload.Items[0].ChannelName != "GPT1" {
		t.Fatalf("channel_name = %q, want %q", payload.Items[0].ChannelName, "GPT1")
	}
	if payload.Items[0].FirstTokenMs != 45 {
		t.Fatalf("first_token_ms = %d, want %d", payload.Items[0].FirstTokenMs, 45)
	}
	if payload.Items[0].Streaming {
		t.Fatalf("streaming = true, want false")
	}
}

func TestGetUsageLogsKeepsStoredChannelNameWhenCurrentAuthNameDiffers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "tabcode-auth",
		FileName: "tabcode.json",
		Provider: "codex",
		Label:    "tabcode-pro",
		Metadata: map[string]any{"label": "tabcode-pro"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	usage.InsertLog(
		"", "", "gpt-5.4", "tabcode-plus", "tabcode-plus", auth.Index,
		false, time.Now().UTC(), 123, 45,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50", nil)

	h.UsageLogs().GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			ChannelName   string `json:"channel_name"`
			AuthIndex     string `json:"auth_index"`
			AuthSubjectID string `json:"auth_subject_id"`
		} `json:"items"`
		Filters struct {
			Channels       []string `json:"channels"`
			ChannelOptions []struct {
				Value         string `json:"value"`
				Label         string `json:"label"`
				AuthIndex     string `json:"auth_index"`
				AuthSubjectID string `json:"auth_subject_id"`
			} `json:"channel_options"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}
	// Historical rows keep the channel_name snapshot written at request time.
	if payload.Items[0].ChannelName != "tabcode-plus" {
		t.Fatalf("channel_name = %q, want %q", payload.Items[0].ChannelName, "tabcode-plus")
	}
	// Filter facets use the live auth label / account subject so renamed channels
	// stay selectable as one account, while still matching historical rows.
	if len(payload.Filters.ChannelOptions) != 1 {
		t.Fatalf("channel_options = %#v, want one option", payload.Filters.ChannelOptions)
	}
	opt := payload.Filters.ChannelOptions[0]
	if opt.Label != "tabcode-pro" {
		t.Fatalf("channel_options[0].label = %q, want tabcode-pro", opt.Label)
	}
	if opt.AuthIndex != auth.Index {
		t.Fatalf("channel_options[0].auth_index = %q, want %q", opt.AuthIndex, auth.Index)
	}
	if !strings.HasPrefix(opt.Value, "authsub_") || opt.AuthSubjectID != opt.Value {
		t.Fatalf("channel_options[0] value/subject = %#v, want authsub_* subject", opt)
	}
	if len(payload.Filters.Channels) != 1 || payload.Filters.Channels[0] != "tabcode-pro" {
		t.Fatalf("filters.channels = %#v, want [tabcode-pro]", payload.Filters.Channels)
	}

	// Legacy clients can still filter by the historical channel_name string.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50&channel=tabcode-plus", nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal filtered response: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ChannelName != "tabcode-plus" {
		t.Fatalf("filtered items = %#v, want one tabcode-plus item", payload.Items)
	}

	// Selecting the live label resolves to auth_index and still returns history.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50&channel=tabcode-pro", nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("live-label filtered expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal live-label filtered response: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].AuthIndex != auth.Index {
		t.Fatalf("live-label filtered items = %#v, want one item for auth %q", payload.Items, auth.Index)
	}

	// Filtering by auth_index value (new clients) also returns the historical row.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50&channel="+auth.Index, nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth-index filtered expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal auth-index filtered response: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ChannelName != "tabcode-plus" {
		t.Fatalf("auth-index filtered items = %#v, want one tabcode-plus item", payload.Items)
	}
}

func TestGetUsageLogsResolvesGenericKimiChannelByAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	authA, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kimi-auth-a",
		FileName: "kimi-a.json",
		Provider: "kimi",
		Label:    "kimi-a",
		Metadata: map[string]any{
			"label":         "kimi-a",
			"refresh_token": "refresh-a",
		},
	})
	if err != nil {
		t.Fatalf("register auth A: %v", err)
	}
	authB, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kimi-auth-b",
		FileName: "kimi-b.json",
		Provider: "kimi",
		Label:    "kimi-b",
		Metadata: map[string]any{
			"label":         "kimi-b",
			"refresh_token": "refresh-b",
		},
	})
	if err != nil {
		t.Fatalf("register auth B: %v", err)
	}

	now := time.Now().UTC()
	usage.InsertLog(
		"", "", "kimi-k2.6", "kimi", "kimi", authA.Index,
		false, now.Add(-time.Minute), 123, 45,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)
	usage.InsertLog(
		"", "", "kimi-k2.6", "kimi", "kimi", authB.Index,
		false, now, 123, 45,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50", nil)

	h.UsageLogs().GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			ChannelName string `json:"channel_name"`
			AuthIndex   string `json:"auth_index"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
	gotByIndex := make(map[string]string, len(payload.Items))
	for _, item := range payload.Items {
		gotByIndex[item.AuthIndex] = item.ChannelName
	}
	if gotByIndex[authA.Index] != "kimi-a" {
		t.Fatalf("channel_name for auth A = %q, want kimi-a", gotByIndex[authA.Index])
	}
	if gotByIndex[authB.Index] != "kimi-b" {
		t.Fatalf("channel_name for auth B = %q, want kimi-b", gotByIndex[authB.Index])
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50&channel=kimi-b", nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal filtered response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("filtered item count = %d, want 1", len(payload.Items))
	}
	if payload.Items[0].AuthIndex != authB.Index || payload.Items[0].ChannelName != "kimi-b" {
		t.Fatalf("filtered item = %+v, want auth B kimi-b", payload.Items[0])
	}
}

func TestGetUsageLogs_EmptyDB_DoesNotReturnNullSlices(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	h := &Handler{
		cfg: &config.Config{},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50", nil)

	h.UsageLogs().GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items   []any `json:"items"`
		Filters struct {
			APIKeys      []string          `json:"api_keys"`
			APIKeyNames  map[string]string `json:"api_key_names"`
			APIKeyCounts map[string]int64  `json:"api_key_counts"`
			Models       []string          `json:"models"`
			Channels     []string          `json:"channels"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.Items == nil {
		t.Fatalf("items is null; expected []")
	}
	if payload.Filters.APIKeys == nil {
		t.Fatalf("filters.api_keys is null; expected []")
	}
	if payload.Filters.Models == nil {
		t.Fatalf("filters.models is null; expected []")
	}
	if payload.Filters.Channels == nil {
		t.Fatalf("filters.channels is null; expected []")
	}
	if payload.Filters.APIKeyNames == nil {
		t.Fatalf("filters.api_key_names is null; expected {}")
	}
	if payload.Filters.APIKeyCounts == nil {
		t.Fatalf("filters.api_key_counts is null; expected {}")
	}
}

func TestGetUsageLogsSupportsExplicitEmptyFilterSelections(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	usage.InsertLog(
		"sk-live-123", "Primary", "gpt-5.4", "codex", "Codex", "auth-1",
		false, time.Now().UTC(), 123, 45,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)

	h := &Handler{cfg: &config.Config{}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/usage/logs?days=7&page=1&size=50&api_keys_empty=1",
		nil,
	)

	h.UsageLogs().GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []any `json:"items"`
		Total int64 `json:"total"`
		Stats struct {
			Total int64 `json:"total"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 0 {
		t.Fatalf("items len = %d, want 0", len(payload.Items))
	}
	if payload.Total != 0 {
		t.Fatalf("total = %d, want 0", payload.Total)
	}
	if payload.Stats.Total != 0 {
		t.Fatalf("stats.total = %d, want 0", payload.Stats.Total)
	}
}

func TestGetUsageLogsCollapsesRenamedAPIKeysByStableIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	stableID := "api-key-stable-1"
	if err := usage.UpsertAPIKey(usage.APIKeyRow{ID: stableID, Key: "sk-old", Name: "袁蔚"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-old): %v", err)
	}
	now := time.Now().UTC()
	usage.InsertLog("sk-old", "袁蔚", "gpt-5.4", "codex", "Codex", "auth-1", false, now, 100, 10, usage.TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")

	if err := usage.UpdateAPIKeyByID(usage.APIKeyRow{ID: stableID, Key: "sk-new", Name: "袁蔚"}); err != nil {
		t.Fatalf("UpdateAPIKeyByID(sk-new): %v", err)
	}
	usage.InsertLog("sk-new", "袁蔚", "gpt-5.4", "codex", "Codex", "auth-1", false, now.Add(time.Second), 120, 12, usage.TokenStats{
		InputTokens: 2, OutputTokens: 2, TotalTokens: 4,
	}, "", "")

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50", nil)

	h.UsageLogs().GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Filters struct {
			APIKeys     []string          `json:"api_keys"`
			APIKeyNames map[string]string `json:"api_key_names"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(payload.Filters.APIKeys) != 1 || payload.Filters.APIKeys[0] != "sk-new" {
		t.Fatalf("filters.api_keys = %#v, want [sk-new]", payload.Filters.APIKeys)
	}
	if payload.Filters.APIKeyNames["sk-new"] != "袁蔚" {
		t.Fatalf("filters.api_key_names[sk-new] = %q, want 袁蔚", payload.Filters.APIKeyNames["sk-new"])
	}
}

func TestGetLogContent_ReturnsRequestDetailsPart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	details := `{"client":{"headers":{"Authorization":"Bearer sk-client-plaintext"}},"upstream":{"headers":{"Authorization":"Bearer sk-upstream-plaintext"}},"response":{"headers":{"X-Request-Id":"req-plaintext"}}}`
	usage.InsertLogWithDetails(
		"sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1",
		false, time.Now().UTC(), 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		`{"messages":[]}`, `{"choices":[]}`, details,
	)
	result, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected one log row, got %d", len(result.Items))
	}

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(result.Items[0].ID, 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs/1/content?part=details&format=json", nil)

	h.UsageLogs().GetLogContent(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload struct {
		Part    string `json:"part"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Part != "details" || payload.Content != details {
		t.Fatalf("unexpected details payload: %+v", payload)
	}
}

func TestGetPublicLogContent_RejectsRequestDetailsPart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/logs/1/content",
		bytes.NewReader([]byte(`{"api_key":"sk-test","part":"details","format":"json"}`)),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicLogContent(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusForbidden, rec.Code, rec.Body.String())
	}
}

func TestGetAuthFileGroupTrendAggregatesByProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	codexAuth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth-trend",
		FileName: "codex.json",
		Provider: "codex",
		Label:    "GptPlus1",
	})
	if err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	otherAuth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kimi-auth-trend",
		FileName: "kimi.json",
		Provider: "kimi",
		Label:    "Kimi",
	})
	if err != nil {
		t.Fatalf("register kimi auth: %v", err)
	}

	now := time.Now().UTC()
	usage.InsertLog(
		"", "", "gpt-5.4", "codex-source", "GptPlus1", codexAuth.Index,
		false, now, 1, 1, usage.TokenStats{TotalTokens: 1}, "", "",
	)
	usage.InsertLog(
		"", "", "kimi-k2.5", "kimi-source", "Kimi", otherAuth.Index,
		false, now, 1, 1, usage.TokenStats{TotalTokens: 1}, "", "",
	)
	codexWeekly := 70.0
	kimiWeekly := 30.0
	if err := usage.RecordDailyQuotaSnapshot(codexAuth.Index, "codex", map[string]*float64{"code_week": &codexWeekly}); err != nil {
		t.Fatalf("record codex quota snapshot: %v", err)
	}
	if err := usage.RecordDailyQuotaSnapshot(otherAuth.Index, "kimi", map[string]*float64{"code_week": &kimiWeekly}); err != nil {
		t.Fatalf("record kimi quota snapshot: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-group-trend?group=codex&days=7", nil)

	h.UsageLogs().GetAuthFileGroupTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Group  string `json:"group"`
		Points []struct {
			Date     string `json:"date"`
			Requests int64  `json:"requests"`
		} `json:"points"`
		QuotaPoints []struct {
			Date    string   `json:"date"`
			Percent *float64 `json:"percent"`
			Samples int64    `json:"samples"`
		} `json:"quota_points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Group != "codex" {
		t.Fatalf("group = %q, want codex", payload.Group)
	}
	var total int64
	for _, point := range payload.Points {
		total += point.Requests
	}
	if total != 1 {
		t.Fatalf("total codex requests = %d, want 1", total)
	}
	if len(payload.QuotaPoints) != 1 {
		t.Fatalf("quota point count = %d, want 1", len(payload.QuotaPoints))
	}
	if payload.QuotaPoints[0].Percent == nil || *payload.QuotaPoints[0].Percent != 70 {
		t.Fatalf("codex quota percent = %v, want 70", payload.QuotaPoints[0].Percent)
	}
	if payload.QuotaPoints[0].Samples != 1 {
		t.Fatalf("codex quota samples = %d, want 1", payload.QuotaPoints[0].Samples)
	}
}

func TestGetEntityUsageStatsScopesAuthIndexesAndSources(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	now := time.Now().UTC()
	usage.InsertLog("", "", "gpt-5.4", "codex-a", "Codex A", "auth-a", false, now, 10, 1, usage.TokenStats{TotalTokens: 11}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex-b", "Codex B", "auth-b", true, now, 20, 1, usage.TokenStats{TotalTokens: 21}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex-c", "Codex C", "auth-c", false, now, 30, 1, usage.TokenStats{TotalTokens: 31}, "", "")

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/usage/entity-stats?days=30&auth_index=auth-a&auth_index=auth-b&source=codex-b",
		nil,
	)

	h.UsageLogs().GetEntityUsageStats(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Source []struct {
			EntityName string `json:"entity_name"`
			Requests   int64  `json:"requests"`
			Failed     int64  `json:"failed"`
		} `json:"source"`
		AuthIndex []struct {
			EntityName string `json:"entity_name"`
			Requests   int64  `json:"requests"`
			Failed     int64  `json:"failed"`
		} `json:"auth_index"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Source) != 1 || payload.Source[0].EntityName != "codex-b" {
		t.Fatalf("source payload = %+v, want only codex-b", payload.Source)
	}
	if len(payload.AuthIndex) != 2 {
		t.Fatalf("auth_index payload len = %d, want 2: %+v", len(payload.AuthIndex), payload.AuthIndex)
	}
	for _, point := range payload.AuthIndex {
		if point.EntityName == "auth-c" {
			t.Fatalf("auth_index payload included unrequested auth-c: %+v", payload.AuthIndex)
		}
	}
}

func TestGetAuthFileTrendUsesWeeklyResetCycleForRequestTotal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth-file-trend",
		FileName: "codex.json",
		Provider: "codex",
		Label:    "GptPro2",
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	resetAt := now.Add(4 * 24 * time.Hour)
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)

	if err := usage.UpsertModelPricing("gpt-5.4", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}
	usage.InsertLog("", "", "gpt-5.4", "codex", "GptPro2", auth.Index, false, cycleStart.Add(-time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex", "GptPro2", auth.Index, false, cycleStart.Add(time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex", "GptPro2", auth.Index, false, now.Add(-time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "")

	weeklyRemaining := 93.0
	if err := usage.RecordQuotaSnapshotPoints(auth.Index, "codex", []usage.QuotaSnapshotPoint{
		{
			RecordedAt:    now,
			QuotaKey:      "code_week",
			QuotaLabel:    "m_quota.code_weekly",
			Percent:       &weeklyRemaining,
			ResetAt:       &resetAt,
			WindowSeconds: 604800,
		},
	}); err != nil {
		t.Fatalf("record quota snapshot point: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-trend?auth_index="+auth.Index+"&days=7&hours=5", nil)

	h.UsageLogs().GetAuthFileTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		AuthIndex         string   `json:"auth_index"`
		RequestTotal      int64    `json:"request_total"`
		CycleRequestTotal int64    `json:"cycle_request_total"`
		CycleCostTotal    float64  `json:"cycle_cost_total"`
		WeeklyQuotaUsed   *float64 `json:"weekly_quota_used_percent"`
		CycleStart        string   `json:"cycle_start"`
		DailyUsage        []struct {
			Date     string  `json:"date"`
			Requests int64   `json:"requests"`
			Cost     float64 `json:"cost"`
		} `json:"daily_usage"`
		HourlyUsage []struct {
			Hour     string  `json:"hour"`
			Requests int64   `json:"requests"`
			Cost     float64 `json:"cost"`
		} `json:"hourly_usage"`
		QuotaSeries []struct {
			QuotaKey      string `json:"quota_key"`
			QuotaLabel    string `json:"quota_label"`
			WindowSeconds int64  `json:"window_seconds"`
			Points        []struct {
				Timestamp string   `json:"timestamp"`
				Percent   *float64 `json:"percent"`
				ResetAt   string   `json:"reset_at"`
			} `json:"points"`
		} `json:"quota_series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.AuthIndex != auth.Index {
		t.Fatalf("auth_index = %q, want %q", payload.AuthIndex, auth.Index)
	}
	if payload.RequestTotal != 3 {
		t.Fatalf("request_total = %d, want 3", payload.RequestTotal)
	}
	if payload.CycleRequestTotal != 2 {
		t.Fatalf("cycle_request_total = %d, want 2", payload.CycleRequestTotal)
	}
	if math.Abs(payload.CycleCostTotal-0.008) > 1e-12 {
		t.Fatalf("cycle_cost_total = %v, want 0.008", payload.CycleCostTotal)
	}
	if payload.WeeklyQuotaUsed == nil || math.Abs(*payload.WeeklyQuotaUsed-7) > 1e-12 {
		t.Fatalf("weekly_quota_used_percent = %v, want 7", payload.WeeklyQuotaUsed)
	}
	if payload.CycleStart != cycleStart.Format(time.RFC3339) {
		t.Fatalf("cycle_start = %q, want %q", payload.CycleStart, cycleStart.Format(time.RFC3339))
	}
	if len(payload.DailyUsage) != 7 {
		t.Fatalf("daily_usage len = %d, want 7", len(payload.DailyUsage))
	}
	var dailyCostTotal float64
	for _, point := range payload.DailyUsage {
		dailyCostTotal += point.Cost
	}
	if math.Abs(dailyCostTotal-0.013) > 1e-12 {
		t.Fatalf("daily_usage cost total = %v, want 0.013", dailyCostTotal)
	}
	var hourlyCostTotal float64
	for _, point := range payload.HourlyUsage {
		hourlyCostTotal += point.Cost
	}
	if math.Abs(hourlyCostTotal-0.003) > 1e-12 {
		t.Fatalf("hourly_usage cost total = %v, want 0.003", hourlyCostTotal)
	}
	if len(payload.QuotaSeries) != 1 {
		t.Fatalf("quota_series len = %d, want 1", len(payload.QuotaSeries))
	}
	if payload.QuotaSeries[0].QuotaKey != "code_week" {
		t.Fatalf("quota key = %q, want code_week", payload.QuotaSeries[0].QuotaKey)
	}
	if payload.QuotaSeries[0].WindowSeconds != 604800 {
		t.Fatalf("window seconds = %d, want 604800", payload.QuotaSeries[0].WindowSeconds)
	}
	if len(payload.QuotaSeries[0].Points) != 1 || payload.QuotaSeries[0].Points[0].Percent == nil || *payload.QuotaSeries[0].Points[0].Percent != 93 {
		t.Fatalf("quota point = %+v, want one 93%% point", payload.QuotaSeries[0].Points)
	}
}

func TestGetAuthFileTrendSharesCycleAcrossTenantsForStableAccountID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	const (
		tenantA = "11111111-1111-1111-1111-111111111111"
		tenantB = "22222222-2222-2222-2222-222222222222"
	)
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	authA, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-shared-a",
		TenantID: tenantA,
		FileName: "a.json",
		Provider: "codex",
		Label:    "Shared A",
		Metadata: map[string]any{"account_id": "acct-shared-trend", "email": "tyktgyk@gmail.com"},
	})
	if err != nil {
		t.Fatalf("register auth A: %v", err)
	}
	authB, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-shared-b",
		TenantID: tenantB,
		FileName: "b.json",
		Provider: "codex",
		Label:    "Shared B",
		Metadata: map[string]any{"account_id": "acct-shared-trend", "email": "tyktgyk@gmail.com"},
	})
	if err != nil {
		t.Fatalf("register auth B: %v", err)
	}
	identityA := usage.ResolveAuthSubjectIdentity(authA)
	identityB := usage.ResolveAuthSubjectIdentity(authB)
	if identityA == nil || identityB == nil || identityA.ID != identityB.ID || !identityA.ShareEligible {
		t.Fatalf("expected shared subject, got A=%+v B=%+v", identityA, identityB)
	}
	if err := usage.UpsertAIAccountTenantBinding(authA, identityA); err != nil {
		t.Fatal(err)
	}
	if err := usage.UpsertAIAccountTenantBinding(authB, identityB); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	resetAt := now.Add(4 * 24 * time.Hour)
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)
	if err := usage.UpsertModelPricing("gpt-5.4", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}
	// Cycle cache must exist before request projection writes cycle buckets.
	remaining := 93.0
	if err := usage.RecordQuotaSnapshotPointsIdentityForTenant(tenantA, authA.Index, identityA.ID, "codex", []usage.QuotaSnapshotPoint{{
		RecordedAt: now, QuotaKey: "code_week", QuotaLabel: "Weekly",
		Percent: &remaining, ResetAt: &resetAt, WindowSeconds: 604800,
	}}); err != nil {
		t.Fatalf("record quota: %v", err)
	}
	// Only tenant A has request logs; B must still see shared cycle totals.
	usage.InsertLogWithDetailsIdentitySubject("sk-a", "", identityA.ID, "A", "gpt-5.4", "codex", "A", authA.Index, false, cycleStart.Add(time.Hour), 1, 1, usage.TokenStats{
		InputTokens: 1000, OutputTokens: 2000, TotalTokens: 3000,
	}, "", "", "")
	usage.InsertLogWithDetailsIdentitySubject("sk-a", "", identityA.ID, "A", "gpt-5.4", "codex", "A", authA.Index, false, now, 1, 1, usage.TokenStats{
		InputTokens: 1000, OutputTokens: 1000, TotalTokens: 2000,
	}, "", "", "")

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantB}})
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-trend?auth_index="+authB.Index+"&days=7&hours=5", nil)
	h.UsageLogs().GetAuthFileTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		CycleRequestTotal int64    `json:"cycle_request_total"`
		CycleCostTotal    float64  `json:"cycle_cost_total"`
		RequestTotal      int64    `json:"request_total"`
		WeeklyQuotaUsed   *float64 `json:"weekly_quota_used_percent"`
		CycleKnown        bool     `json:"cycle_known"`
		CycleStart        string   `json:"cycle_start"`
		HourlyUsage       []struct {
			Hour     string  `json:"hour"`
			Requests int64   `json:"requests"`
			Cost     float64 `json:"cost"`
		} `json:"hourly_usage"`
		QuotaSeries []struct {
			QuotaKey string `json:"quota_key"`
			Points   []struct {
				Percent *float64 `json:"percent"`
			} `json:"points"`
		} `json:"quota_series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !payload.CycleKnown || payload.CycleRequestTotal != 2 {
		t.Fatalf("tenant B cycle = known=%v total=%d, want 2 shared requests", payload.CycleKnown, payload.CycleRequestTotal)
	}
	if math.Abs(payload.CycleCostTotal-0.008) > 1e-12 {
		t.Fatalf("tenant B cycle_cost_total=%v, want 0.008", payload.CycleCostTotal)
	}
	if payload.RequestTotal != 2 {
		t.Fatalf("tenant B request_total=%d, want 2", payload.RequestTotal)
	}
	if payload.CycleStart != cycleStart.Format(time.RFC3339) {
		t.Fatalf("cycle_start=%q want %q", payload.CycleStart, cycleStart.Format(time.RFC3339))
	}
	if payload.WeeklyQuotaUsed == nil || math.Abs(*payload.WeeklyQuotaUsed-7) > 1e-12 {
		t.Fatalf("weekly_quota_used=%v, want 7", payload.WeeklyQuotaUsed)
	}
	if len(payload.QuotaSeries) != 1 || len(payload.QuotaSeries[0].Points) != 1 {
		t.Fatalf("quota_series=%+v", payload.QuotaSeries)
	}
	var hourlyRequests int64
	for _, point := range payload.HourlyUsage {
		hourlyRequests += point.Requests
	}
	if hourlyRequests < 1 {
		t.Fatalf("tenant B hourly_usage requests=%d, want shared recent hours from tenant A", hourlyRequests)
	}
}

func TestGetAuthFileTrendKeepsWeeklyCycleAcrossCodexPlanRename(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth-file-pro",
		FileName: "codex-user@example.com-pro.json",
		Provider: "codex",
		Label:    "user@example.com",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct_same_user",
			"plan_type":  "pro",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || identity.ID == "" {
		t.Fatalf("ResolveAuthSubjectIdentity() returned empty identity: %+v", identity)
	}

	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	resetAt := now.Add(4 * 24 * time.Hour)
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)

	if err := usage.UpsertModelPricing("gpt-5.4", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	legacyPlusIndex := "legacy-plus-index"
	usage.InsertLog("", "", "gpt-5.4", "user@example.com", "user@example.com", legacyPlusIndex, false, cycleStart.Add(-time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "user@example.com", "user@example.com", legacyPlusIndex, false, cycleStart.Add(time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")
	usage.InsertLogWithDetailsIdentitySubject("", "", identity.ID, "", "gpt-5.4", "user@example.com", "user@example.com", auth.Index, false, now.Add(-time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "", "")

	weeklyRemaining := 93.0
	if err := usage.RecordQuotaSnapshotPointsIdentity(auth.Index, identity.ID, "codex", []usage.QuotaSnapshotPoint{
		{
			RecordedAt:    now,
			QuotaKey:      "code_week",
			QuotaLabel:    "m_quota.code_weekly",
			Percent:       &weeklyRemaining,
			ResetAt:       &resetAt,
			WindowSeconds: 604800,
		},
	}); err != nil {
		t.Fatalf("record quota snapshot point: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-trend?auth_index="+auth.Index+"&days=7&hours=5", nil)

	h.UsageLogs().GetAuthFileTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		RequestTotal      int64   `json:"request_total"`
		CycleRequestTotal int64   `json:"cycle_request_total"`
		CycleCostTotal    float64 `json:"cycle_cost_total"`
		CycleKnown        bool    `json:"cycle_known"`
		CycleStart        string  `json:"cycle_start"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.RequestTotal != 3 {
		t.Fatalf("request_total = %d, want 3", payload.RequestTotal)
	}
	if payload.CycleRequestTotal != 2 {
		t.Fatalf("cycle_request_total = %d, want 2", payload.CycleRequestTotal)
	}
	if math.Abs(payload.CycleCostTotal-0.008) > 1e-12 {
		t.Fatalf("cycle_cost_total = %v, want 0.008", payload.CycleCostTotal)
	}
	if !payload.CycleKnown {
		t.Fatalf("cycle_known = false, want true")
	}
	if payload.CycleStart != cycleStart.Format(time.RFC3339) {
		t.Fatalf("cycle_start = %q, want %q", payload.CycleStart, cycleStart.Format(time.RFC3339))
	}
}

func TestGetAuthFileTrendPrefersPrimaryCodeWeekOverAdditionalWeeklyCycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth-file-primary-week",
		FileName: "codex-user@example.com-pro.json",
		Provider: "codex",
		Label:    "user@example.com",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct_same_user",
			"plan_type":  "pro",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || identity.ID == "" {
		t.Fatalf("ResolveAuthSubjectIdentity() returned empty identity: %+v", identity)
	}

	recordedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	codeResetAt := recordedAt.Add(4 * 24 * time.Hour)
	codeCycleStart := codeResetAt.Add(-7 * 24 * time.Hour)
	additionalResetAt := codeResetAt.Add(2 * time.Hour)
	additionalCycleStart := additionalResetAt.Add(-7 * 24 * time.Hour)

	if err := usage.UpsertModelPricing("gpt-5.4", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	usage.InsertLogWithDetailsIdentitySubject("", "", identity.ID, "", "gpt-5.4", "user@example.com", "user@example.com", auth.Index, false, codeCycleStart.Add(-time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "", "")
	usage.InsertLogWithDetailsIdentitySubject("", "", identity.ID, "", "gpt-5.4", "user@example.com", "user@example.com", auth.Index, false, codeCycleStart.Add(30*time.Minute), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "", "")
	usage.InsertLogWithDetailsIdentitySubject("", "", identity.ID, "", "gpt-5.4", "user@example.com", "user@example.com", auth.Index, false, additionalCycleStart.Add(30*time.Minute), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "", "")

	codeRemaining := 99.0
	additionalRemaining := 100.0
	if err := usage.RecordQuotaSnapshotPointsIdentity(auth.Index, identity.ID, "codex", []usage.QuotaSnapshotPoint{
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "code_week",
			QuotaLabel:    "m_quota.code_weekly",
			Percent:       &codeRemaining,
			ResetAt:       &codeResetAt,
			WindowSeconds: 604800,
		},
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "additional:codex_bengalfox:week",
			QuotaLabel:    "GPT-5.3-Codex-Spark: Weekly",
			Percent:       &additionalRemaining,
			ResetAt:       &additionalResetAt,
			WindowSeconds: 604800,
		},
	}); err != nil {
		t.Fatalf("record quota snapshot point: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-trend?auth_index="+auth.Index+"&days=7&hours=5", nil)

	h.UsageLogs().GetAuthFileTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		CycleKnown        bool    `json:"cycle_known"`
		CycleStart        string  `json:"cycle_start"`
		CycleRequestTotal int64   `json:"cycle_request_total"`
		CycleCostTotal    float64 `json:"cycle_cost_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !payload.CycleKnown {
		t.Fatalf("cycle_known = false, want true")
	}
	if payload.CycleStart != codeCycleStart.Format(time.RFC3339) {
		t.Fatalf("cycle_start = %q, want %q", payload.CycleStart, codeCycleStart.Format(time.RFC3339))
	}
	if payload.CycleRequestTotal != 2 {
		t.Fatalf("cycle_request_total = %d, want 2", payload.CycleRequestTotal)
	}
	if math.Abs(payload.CycleCostTotal-0.006) > 1e-12 {
		t.Fatalf("cycle_cost_total = %v, want 0.006", payload.CycleCostTotal)
	}
}

func TestGetAuthFileTrendFallbackSeriesPrefersPrimaryCodeWeekOverAdditionalWeeklyCycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth-file-fallback-week",
		FileName: "codex-fallback.json",
		Provider: "codex",
		Label:    "fallback",
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	recordedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	codeResetAt := recordedAt.Add(4 * 24 * time.Hour)
	codeCycleStart := codeResetAt.Add(-7 * 24 * time.Hour)
	additionalResetAt := codeResetAt.Add(2 * time.Hour)
	additionalCycleStart := additionalResetAt.Add(-7 * 24 * time.Hour)

	if err := usage.UpsertModelPricing("gpt-5.4", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	usage.InsertLog("", "", "gpt-5.4", "codex", "fallback", auth.Index, false, codeCycleStart.Add(-time.Hour), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex", "fallback", auth.Index, false, codeCycleStart.Add(30*time.Minute), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex", "fallback", auth.Index, false, additionalCycleStart.Add(30*time.Minute), 1, 1, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "")

	codeRemaining := 99.0
	additionalRemaining := 100.0
	if err := usage.RecordQuotaSnapshotPoints(auth.Index, "codex", []usage.QuotaSnapshotPoint{
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "code_week",
			QuotaLabel:    "m_quota.code_weekly",
			Percent:       &codeRemaining,
			ResetAt:       &codeResetAt,
			WindowSeconds: 604800,
		},
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "additional:codex_bengalfox:week",
			QuotaLabel:    "GPT-5.3-Codex-Spark: Weekly",
			Percent:       &additionalRemaining,
			ResetAt:       &additionalResetAt,
			WindowSeconds: 604800,
		},
	}); err != nil {
		t.Fatalf("record quota snapshot point: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-trend?auth_index="+auth.Index+"&days=7&hours=5", nil)

	h.UsageLogs().GetAuthFileTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		CycleKnown        bool    `json:"cycle_known"`
		CycleStart        string  `json:"cycle_start"`
		CycleRequestTotal int64   `json:"cycle_request_total"`
		CycleCostTotal    float64 `json:"cycle_cost_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !payload.CycleKnown {
		t.Fatalf("cycle_known = false, want true")
	}
	if payload.CycleStart != codeCycleStart.Format(time.RFC3339) {
		t.Fatalf("cycle_start = %q, want %q", payload.CycleStart, codeCycleStart.Format(time.RFC3339))
	}
	if payload.CycleRequestTotal != 2 {
		t.Fatalf("cycle_request_total = %d, want 2", payload.CycleRequestTotal)
	}
	if math.Abs(payload.CycleCostTotal-0.006) > 1e-12 {
		t.Fatalf("cycle_cost_total = %v, want 0.006", payload.CycleCostTotal)
	}
}

func TestPostAuthFileQuotaSnapshotStoresFineGrainedPoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	body := []byte(`{
		"auth_index":"auth-1",
		"provider":"codex",
		"quotas":{"code_week":93},
		"quota_points":[
			{
				"quota_key":"additional:codex_bengalfox:5h",
				"quota_label":"GPT-5.3-Codex-Spark: 5h",
				"percent":100,
				"reset_at":"2026-04-30T21:00:00Z",
				"window_seconds":18000
			}
		]
	}`)

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/usage/auth-file-quota-snapshot", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PostAuthFileQuotaSnapshot(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	points, err := usage.QueryQuotaSnapshotPoints("auth-1", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryQuotaSnapshotPoints() error = %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	if points[0].QuotaKey != "additional:codex_bengalfox:5h" {
		t.Fatalf("quota key = %q", points[0].QuotaKey)
	}
	if points[0].ResetAt == nil || points[0].ResetAt.Format(time.RFC3339) != "2026-04-30T21:00:00Z" {
		t.Fatalf("reset_at = %v", points[0].ResetAt)
	}
}

func TestGetPublicUsageLogs_EmptyDB_DoesNotReturnNullModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	h := &Handler{
		cfg: &config.Config{},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/logs",
		bytes.NewReader([]byte(`{"api_key":"sk-test","days":7,"page":1,"size":50}`)),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Filters struct {
			Models   []string `json:"models"`
			Channels []string `json:"channels"`
			Statuses []string `json:"statuses"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Filters.Models == nil {
		t.Fatalf("filters.models is null; expected []")
	}
	if payload.Filters.Channels == nil {
		t.Fatalf("filters.channels is null; expected []")
	}
	if payload.Filters.Statuses == nil {
		t.Fatalf("filters.statuses is null; expected []")
	}
}

func TestGetPublicUsageLogs_AcceptsPOSTBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	h := &Handler{
		cfg: &config.Config{},
	}

	body := []byte(`{"api_key":"sk-test","days":7,"page":1,"size":50}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/logs",
		bytes.NewReader(body),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Filters struct {
			Models   []string `json:"models"`
			Channels []string `json:"channels"`
			Statuses []string `json:"statuses"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Filters.Models == nil {
		t.Fatalf("filters.models is null; expected []")
	}
	if payload.Filters.Channels == nil {
		t.Fatalf("filters.channels is null; expected []")
	}
	if payload.Filters.Statuses == nil {
		t.Fatalf("filters.statuses is null; expected []")
	}
}

func TestGetPublicUsageLogs_ReturnsCurrentAPIKeyName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})
	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: "sk-test", Name: "Primary"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	h := &Handler{
		cfg: &config.Config{},
	}

	body := []byte(`{"api_key":"sk-test","days":7,"page":1,"size":50}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/logs",
		bytes.NewReader(body),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		APIKeyName string `json:"api_key_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.APIKeyName != "Primary" {
		t.Fatalf("api_key_name = %q, want Primary", payload.APIKeyName)
	}
}

func TestGetPublicUsageLogs_ReturnsChannelNameWithoutSensitiveFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "public-lookup-auth",
		FileName: "codex-test.json",
		Provider: "codex",
		Label:    "Codex 主渠道",
		Metadata: map[string]any{"email": "owner@example.com"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	usage.InsertLog(
		"sk-test", "Primary", "gpt-5.5", "owner@example.com", "owner@example.com", auth.Index,
		false, time.Now().UTC(), 123, 45,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body := []byte(`{"api_key":"sk-test","days":7,"page":1,"size":50}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/logs",
		bytes.NewReader(body),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			APIKey      string `json:"api_key"`
			APIKeyName  string `json:"api_key_name"`
			Source      string `json:"source"`
			AuthIndex   string `json:"auth_index"`
			ChannelName string `json:"channel_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}
	item := payload.Items[0]
	if item.ChannelName != "Codex 主渠道" {
		t.Fatalf("channel_name = %q, want Codex 主渠道", item.ChannelName)
	}
	// api_key_name is non-secret and needed so multi-key portal users can tell keys apart.
	// Raw api_key / source / auth_index must still be scrubbed.
	if item.APIKey != "" || item.Source != "" || item.AuthIndex != "" {
		t.Fatalf("sensitive fields not scrubbed: %+v", item)
	}
}

func TestGetPublicUsageLogs_FiltersByDisplayedChannelName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "public-lookup-filter-auth",
		FileName: "codex-test.json",
		Provider: "codex",
		Label:    "Codex 主渠道",
		Metadata: map[string]any{"email": "owner@example.com"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	now := time.Now().UTC()
	usage.InsertLog("sk-test", "Primary", "gpt-5.5", "owner@example.com", "owner@example.com", auth.Index, false, now, 123, 45, usage.TokenStats{TotalTokens: 3}, "", "")
	usage.InsertLog("sk-test", "Primary", "gpt-5.5", "other", "OpenCode", "auth-other", false, now.Add(time.Second), 123, 45, usage.TokenStats{TotalTokens: 3}, "", "")

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	body := []byte(`{"api_key":"sk-test","days":7,"page":1,"size":50,"channels":["Codex 主渠道"],"statuses":["success"]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/usage/logs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			ChannelName string `json:"channel_name"`
		} `json:"items"`
		Filters struct {
			Channels []string `json:"channels"`
			Statuses []string `json:"statuses"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ChannelName != "Codex 主渠道" {
		t.Fatalf("items = %+v, want only Codex 主渠道", payload.Items)
	}
	if !containsString(payload.Filters.Channels, "Codex 主渠道") || !containsString(payload.Filters.Channels, "OpenCode") {
		t.Fatalf("filters.channels = %#v, want linked channel options", payload.Filters.Channels)
	}
	if !containsString(payload.Filters.Statuses, "success") {
		t.Fatalf("filters.statuses = %#v, want success", payload.Filters.Statuses)
	}
}

func TestGetPublicUsageLogs_DoesNotReadAPIKeyFromQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	h := &Handler{
		cfg: &config.Config{},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/public/usage/logs?api_key=sk-test&days=7&page=1&size=50",
		nil,
	)

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "api_key parameter is required") {
		t.Fatalf("expected query api_key to be ignored, body=%s", rec.Body.String())
	}
}

func TestGetPublicUsageLogs_RejectsOversizedPOSTBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	h := &Handler{
		cfg: &config.Config{},
	}

	body := bytes.Repeat([]byte("a"), int(publicLookupBodyLimit)+1)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/logs",
		bytes.NewReader(body),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("expected oversized body rejection, body=%s", rec.Body.String())
	}
}

func TestDeleteUsageLogsClearsRequestLogDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	now := time.Now().UTC()
	usage.InsertLog("sk-target", "", "gpt-5.4", "codex", "Codex", "auth-1", false, now, 123, 45, usage.TokenStats{
		InputTokens: 1, OutputTokens: 2, TotalTokens: 3,
	}, `{"messages":[{"role":"user","content":"hello"}]}`, `{"id":"resp_1"}`)

	h := &Handler{cfg: &config.Config{}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/usage/logs", nil)

	h.UsageLogs().DeleteUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		DeletedLogs     int64 `json:"deleted_logs"`
		DeletedContents int64 `json:"deleted_contents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.DeletedLogs != 1 {
		t.Fatalf("DeletedLogs = %d, want 1", payload.DeletedLogs)
	}
	if payload.DeletedContents != 1 {
		t.Fatalf("DeletedContents = %d, want 1", payload.DeletedContents)
	}

	result, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after delete error = %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("expected 0 request logs after delete, got %d", len(result.Items))
	}
}

func TestDeleteUsageLogsSupportsSelectiveBodyCleanup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	now := time.Now().UTC()
	usage.InsertLogWithDetails("sk-target", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, now, 123, 45, usage.TokenStats{
		InputTokens: 1, OutputTokens: 2, TotalTokens: 3,
	}, `{"messages":[{"role":"user","content":"hello"}]}`, `{"id":"resp_1"}`, `{"request_id":"req-1"}`)

	h := &Handler{cfg: &config.Config{}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/usage/logs", strings.NewReader(`{"clear_body_content":true,"clear_detail_content":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().DeleteUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	result, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after selective delete error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 request log after selective cleanup, got %d", len(result.Items))
	}
	if result.Items[0].HasContent {
		t.Fatalf("HasContent = true, want false after selective cleanup")
	}
}

func TestGetUsageLogsFiltersByOrphanAuthIndexWithoutLiveMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	// Live auth uses the file: seed index (current EnsureIndex behavior).
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	liveAuth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "xai-asherandersenloqv@outlook.com.json",
		FileName: "xai-asherandersenloqv@outlook.com.json",
		Provider: "xai",
		Label:    "asherandersenloqv@outlook.com",
		Metadata: map[string]any{
			"email":     "asherandersenloqv@outlook.com",
			"auth_kind": "oauth",
		},
	})
	if err != nil {
		t.Fatalf("register live auth: %v", err)
	}

	// Historical rows were written under the id: seed index before FileName was
	// consistently set. That orphan index no longer exists in live auth meta.
	orphanIndex := "69e8946f1ffc2d23"
	now := time.Now().UTC()
	usage.InsertLog(
		"", "", "grok-4.5", "asherandersenloqv@outlook.com", "asherandersenloqv@outlook.com", orphanIndex,
		false, now.Add(-time.Minute), 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)
	usage.InsertLog(
		"", "", "grok-4.5", "asherandersenloqv@outlook.com", "asherandersenloqv@outlook.com", liveAuth.Index,
		false, now, 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		"", "",
	)

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}

	// Facet list should collapse the live and historical aliases into one option.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50", nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var listPayload struct {
		Filters struct {
			ChannelOptions []struct {
				Value         string `json:"value"`
				Label         string `json:"label"`
				Provider      string `json:"provider"`
				AuthType      string `json:"auth_type"`
				AuthIndex     string `json:"auth_index"`
				AuthSubjectID string `json:"auth_subject_id"`
			} `json:"channel_options"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(listPayload.Filters.ChannelOptions) != 1 {
		t.Fatalf("channel_options = %#v, want one merged option", listPayload.Filters.ChannelOptions)
	}
	option := listPayload.Filters.ChannelOptions[0]
	if option.AuthIndex != liveAuth.Index {
		t.Fatalf("merged option auth_index = %q, want live index %q", option.AuthIndex, liveAuth.Index)
	}
	if !strings.HasPrefix(option.Value, "authsub_") || option.AuthSubjectID != option.Value {
		t.Fatalf("merged option value/subject = %#v, want authsub_* subject", option)
	}
	if option.Label != "asherandersenloqv@outlook.com" || option.Provider != "xai" || option.AuthType != "oauth" {
		t.Fatalf("merged option metadata = %#v", option)
	}

	// Old deep links using the orphan index expand to the complete alias group.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?days=7&page=1&size=50&channel="+orphanIndex, nil)
	h.UsageLogs().GetUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("orphan filter expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var filtered struct {
		Items []struct {
			AuthIndex   string `json:"auth_index"`
			ChannelName string `json:"channel_name"`
			Model       string `json:"model"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("unmarshal orphan filtered response: %v", err)
	}
	if filtered.Total != 2 || len(filtered.Items) != 2 {
		t.Fatalf("orphan filtered total/items = %d/%d, want 2/2; body=%s", filtered.Total, len(filtered.Items), rec.Body.String())
	}
	found := map[string]bool{}
	for _, item := range filtered.Items {
		found[item.AuthIndex] = true
		if item.ChannelName != "asherandersenloqv@outlook.com" {
			t.Fatalf("orphan filtered channel_name = %q", item.ChannelName)
		}
	}
	if !found[orphanIndex] || !found[liveAuth.Index] {
		t.Fatalf("orphan filter missing alias rows: %#v", filtered.Items)
	}
}

func TestGetPublicUsageLogs_AggregatesOwnedBusinessTenantKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	tenantID := "00000000-0000-0000-0000-0000000000ac"
	endUserID := "00000000-0000-0000-0000-0000000000bd"
	now := time.Now().UTC().Format(time.RFC3339)
	for _, row := range []usage.APIKeyRow{
		{ID: "00000000-0000-0000-0000-0000000000a2", Key: "sk-owned-log-a", Name: "Laptop", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
		{ID: "00000000-0000-0000-0000-0000000000b2", Key: "sk-owned-log-b", Name: "Automation", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
	} {
		if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Key, err)
		}
		usage.InsertLog(row.Key, row.Name, "gpt-test", "test", "channel", "auth", false, time.Now().UTC(), 1, 0, usage.TokenStats{TotalTokens: 1}, "", "")
	}

	h := &Handler{cfg: &config.Config{}}
	body := []byte(`{"api_key":"sk-owned-log-b","days":7,"page":1,"size":50}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/usage/logs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Total int64 `json:"total"`
		Items []struct {
			APIKey        string `json:"api_key"`
			APIKeyID      string `json:"api_key_id"`
			APIKeyMasked  string `json:"api_key_masked"`
			APIKeyOwnName string `json:"api_key_own_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 2 || len(payload.Items) != 2 {
		t.Fatalf("public logs total/items = %d/%d, want 2/2", payload.Total, len(payload.Items))
	}
	for _, item := range payload.Items {
		if item.APIKey != "" || item.APIKeyID != "" {
			t.Fatalf("public item leaked raw key identity: %+v", item)
		}
		if item.APIKeyMasked == "" {
			t.Fatalf("public item missing masked key fallback: %+v", item)
		}
		if item.APIKeyOwnName != "Laptop" && item.APIKeyOwnName != "Automation" {
			t.Fatalf("api_key_own_name = %q, want concrete key name", item.APIKeyOwnName)
		}
	}
}

func TestGetPublicUsageLogs_FiltersByAPIKeyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	tenantID := "00000000-0000-0000-0000-0000000000ac"
	endUserID := "00000000-0000-0000-0000-0000000000bd"
	keyAID := "00000000-0000-0000-0000-0000000000a2"
	keyBID := "00000000-0000-0000-0000-0000000000b2"
	now := time.Now().UTC().Format(time.RFC3339)
	for _, row := range []usage.APIKeyRow{
		{ID: keyAID, Key: "sk-owned-log-a", Name: "Laptop", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
		{ID: keyBID, Key: "sk-owned-log-b", Name: "Automation", EndUserID: endUserID, CreatedAt: now, UpdatedAt: now},
	} {
		if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Key, err)
		}
		usage.InsertLog(row.Key, row.Name, "gpt-test", "test", "channel", "auth", false, time.Now().UTC(), 1, 0, usage.TokenStats{TotalTokens: 1}, "", "")
	}

	h := &Handler{cfg: &config.Config{}}
	body, err := json.Marshal(map[string]any{
		"api_key":     "sk-owned-log-b",
		"days":        7,
		"page":        1,
		"size":        50,
		"api_key_ids": []string{keyAID},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/usage/logs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UsageLogs().GetPublicUsageLogs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Total int64 `json:"total"`
		Items []struct {
			APIKeyOwnName string `json:"api_key_own_name"`
		} `json:"items"`
		Filters struct {
			APIKeyIDs      []string          `json:"api_key_ids"`
			APIKeyIDNames  map[string]string `json:"api_key_id_names"`
			APIKeyIDCounts map[string]int64  `json:"api_key_id_counts"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 {
		t.Fatalf("filtered total/items = %d/%d, want 1/1; body=%s", payload.Total, len(payload.Items), rec.Body.String())
	}
	if payload.Items[0].APIKeyOwnName != "Laptop" {
		t.Fatalf("api_key_own_name = %q, want Laptop", payload.Items[0].APIKeyOwnName)
	}
	if len(payload.Filters.APIKeyIDs) != 2 {
		t.Fatalf("filters.api_key_ids = %#v, want 2 options", payload.Filters.APIKeyIDs)
	}
	if payload.Filters.APIKeyIDNames[keyAID] != "Laptop" {
		t.Fatalf("filters.api_key_id_names[%s] = %q, want Laptop", keyAID, payload.Filters.APIKeyIDNames[keyAID])
	}
	if payload.Filters.APIKeyIDNames[keyBID] != "Automation" {
		t.Fatalf("filters.api_key_id_names[%s] = %q, want Automation", keyBID, payload.Filters.APIKeyIDNames[keyBID])
	}
	if payload.Filters.APIKeyIDCounts[keyAID] != 1 || payload.Filters.APIKeyIDCounts[keyBID] != 1 {
		t.Fatalf("filters.api_key_id_counts = %#v, want one request per owned key", payload.Filters.APIKeyIDCounts)
	}
}
