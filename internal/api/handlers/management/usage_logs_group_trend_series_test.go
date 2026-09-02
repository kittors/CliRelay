package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetAuthFileGroupTrendReturnsEveryWeeklySeries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "ag-trend", FileName: "ag.json", Provider: "antigravity", Label: "AG",
	})
	if err != nil {
		t.Fatal(err)
	}

	gemini, third, fiveHour := 40.0, 80.0, 10.0
	week := usage.WeeklyQuotaWindowSeconds
	now := time.Now().UTC()
	if err := usage.RecordQuotaSnapshotPoints(auth.Index, "antigravity", []usage.QuotaSnapshotPoint{
		{RecordedAt: now, QuotaKey: "antigravity:gemini_weekly", QuotaLabel: "Gemini Models", Percent: &gemini, WindowSeconds: week},
		{RecordedAt: now, QuotaKey: "antigravity:3p_weekly", QuotaLabel: "Claude and GPT models", Percent: &third, WindowSeconds: week},
		{RecordedAt: now, QuotaKey: "antigravity:gemini_5h", QuotaLabel: "Gemini Models", Percent: &fiveHour, WindowSeconds: 5 * 60 * 60},
	}); err != nil {
		t.Fatal(err)
	}
	if err := usage.RecordDailyQuotaSnapshot(auth.Index, "antigravity", map[string]*float64{
		"antigravity:gemini_weekly": &gemini,
		"antigravity:3p_weekly":     &third,
		"antigravity:gemini_5h":     &fiveHour,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/auth-file-group-trend?group=antigravity&days=7", nil)
	(&Handler{cfg: &config.Config{}, authManager: manager}).UsageLogs().GetAuthFileGroupTrend(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		QuotaSeries []struct {
			QuotaKey string `json:"quota_key"`
		} `json:"quota_series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(payload.QuotaSeries))
	for _, item := range payload.QuotaSeries {
		got = append(got, item.QuotaKey)
	}
	if len(got) != 2 || got[0] == "antigravity:gemini_5h" || got[1] == "antigravity:gemini_5h" {
		t.Fatalf("quota_series = %v, want both weeklies and no 5h", got)
	}
}
