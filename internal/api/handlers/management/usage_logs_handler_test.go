package management

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetUsageLogsPrefersCurrentAuthChannelNameByAuthIndex(t *testing.T) {
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

	h.GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			ChannelName  string `json:"channel_name"`
			AuthIndex    string `json:"auth_index"`
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

	h.GetUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Items   []any `json:"items"`
		Filters struct {
			APIKeys     []string          `json:"api_keys"`
			APIKeyNames map[string]string `json:"api_key_names"`
			Models      []string          `json:"models"`
			Channels    []string          `json:"channels"`
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

	h.GetLogContent(c)

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

	h.GetPublicLogContent(c)

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

	h.GetAuthFileGroupTrend(c)

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

	now := time.Date(2026, 4, 30, 16, 0, 0, 0, time.UTC)
	resetAt := now.Add(4 * 24 * time.Hour)
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)

	usage.InsertLog("", "", "gpt-5.4", "codex", "GptPro2", auth.Index, false, cycleStart.Add(-time.Hour), 1, 1, usage.TokenStats{TotalTokens: 1}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex", "GptPro2", auth.Index, false, cycleStart.Add(time.Hour), 1, 1, usage.TokenStats{TotalTokens: 1}, "", "")
	usage.InsertLog("", "", "gpt-5.4", "codex", "GptPro2", auth.Index, false, now.Add(-time.Hour), 1, 1, usage.TokenStats{TotalTokens: 1}, "", "")

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

	h.GetAuthFileTrend(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		AuthIndex         string `json:"auth_index"`
		RequestTotal      int64  `json:"request_total"`
		CycleRequestTotal int64  `json:"cycle_request_total"`
		CycleStart        string `json:"cycle_start"`
		DailyUsage        []struct {
			Date     string `json:"date"`
			Requests int64  `json:"requests"`
		} `json:"daily_usage"`
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
	if payload.CycleStart != cycleStart.Format(time.RFC3339) {
		t.Fatalf("cycle_start = %q, want %q", payload.CycleStart, cycleStart.Format(time.RFC3339))
	}
	if len(payload.DailyUsage) != 7 {
		t.Fatalf("daily_usage len = %d, want 7", len(payload.DailyUsage))
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

	h.GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Filters struct {
			Models []string `json:"models"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Filters.Models == nil {
		t.Fatalf("filters.models is null; expected []")
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

	h.GetPublicUsageLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Filters struct {
			Models []string `json:"models"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Filters.Models == nil {
		t.Fatalf("filters.models is null; expected []")
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

	h.GetPublicUsageLogs(c)

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

	h.GetPublicUsageLogs(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("expected oversized body rejection, body=%s", rec.Body.String())
	}
}
