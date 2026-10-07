package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func initMonitorHandlerDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func serveMonitor(t *testing.T, h *Handler, target string, principal *identity.Principal, realtime bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	if principal != nil {
		c.Set(managementPrincipalKey, *principal)
	}
	if realtime {
		h.UsageLogs().GetUsageMonitorRealtime(c)
	} else {
		h.UsageLogs().GetUsageMonitorOverview(c)
	}
	return rec
}

func TestGetUsageMonitorOverviewRejectsUnknownRange(t *testing.T) {
	initMonitorHandlerDB(t)
	rec := serveMonitor(t, &Handler{cfg: &config.Config{}}, "/usage/monitor/overview?range=90d", nil, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "invalid_range" {
		t.Fatalf("code = %q, want invalid_range", body.Code)
	}
}

func TestGetUsageMonitorOverviewDefaultsToTwentyFourHours(t *testing.T) {
	initMonitorHandlerDB(t)
	for _, realtime := range []bool{false, true} {
		rec := serveMonitor(t, &Handler{cfg: &config.Config{}}, "/usage/monitor/overview", nil, realtime)
		if rec.Code != http.StatusOK {
			t.Fatalf("realtime=%v status = %d; body=%s", realtime, rec.Code, rec.Body.String())
		}
	}
	rec := serveMonitor(t, &Handler{cfg: &config.Config{}}, "/usage/monitor/overview", nil, false)
	var body usage.MonitorOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Range.Key != usage.MonitorRange24h || len(body.Series) != 24 {
		t.Fatalf("range = %+v with %d points, want 24h / 24", body.Range, len(body.Series))
	}
}

func TestGetUsageMonitorOverviewUsesLiveAccountProvider(t *testing.T) {
	initMonitorHandlerDB(t)
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-pool",
		FileName: "claude-pool.json",
		Provider: "claude",
		Label:    "Team Pool",
		Metadata: map[string]any{"email": "pool@example.com", "account_id": "acct-pool"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	subject := usage.ResolveAuthSubjectIdentity(auth)
	if subject == nil || subject.ID == "" {
		t.Fatal("auth has no subject identity")
	}
	// "Team Pool" carries no provider hint, so only the live account can say
	// it is Claude.
	usage.InsertLogWithDetailsIdentitySubject(
		"", "", subject.ID, "", "claude-test", "pool@example.com", "Team Pool", auth.Index,
		false, time.Now().UTC(), 900, 200, usage.TokenStats{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
		"", "", "",
	)

	rec := serveMonitor(t, &Handler{cfg: &config.Config{}, authManager: manager}, "/usage/monitor/overview?range=1h", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body usage.MonitorOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Channels.Rows) != 1 {
		t.Fatalf("channels = %+v", body.Channels.Rows)
	}
	row := body.Channels.Rows[0]
	if row.Label != "Team Pool" || row.Provider != "claude" || row.AuthType != "oauth" {
		t.Fatalf("channel row = %+v, want Team Pool / claude / oauth", row)
	}
	if len(body.Filters.Channels) != 1 || body.Filters.Channels[0].Provider != "claude" {
		t.Fatalf("channel option = %+v, want claude provider", body.Filters.Channels)
	}
}

func TestGetUsageMonitorOverviewKeepsCommasInsideValues(t *testing.T) {
	initMonitorHandlerDB(t)
	now := time.Now().UTC()
	for _, channel := range []string{"Team A, Codex", "Team B"} {
		usage.InsertLog("", "", "gpt-test", "src", channel, "", false, now, 100, 0, usage.TokenStats{TotalTokens: 1}, "", "")
	}
	h := &Handler{cfg: &config.Config{}}
	cases := map[string]int64{
		"/usage/monitor/overview?range=1h&channel=Team+A%2C+Codex":                1,
		"/usage/monitor/overview?range=1h&channel=Team+A%2C+Codex&channel=team+b": 2,
	}
	for target, want := range cases {
		var body usage.MonitorOverview
		rec := serveMonitor(t, h, target, nil, false)
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s decode: %v", target, err)
		}
		if body.Summary.Current.Requests != want {
			t.Fatalf("%s requests = %d, want %d", target, body.Summary.Current.Requests, want)
		}
	}
}

func TestGetUsageMonitorOverviewIsScopedToEffectiveTenant(t *testing.T) {
	initMonitorHandlerDB(t)
	const businessTenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	now := time.Now().UTC()
	usage.InsertRequestLog(usage.RequestLogEntry{
		TrustedTenantID: businessTenant, Model: "gpt-business", Timestamp: now, LatencyMs: 100,
		Tokens: usage.TokenStats{TotalTokens: 1},
	})
	usage.InsertLog("", "", "gpt-system", "src", "chan", "", false, now, 100, 0, usage.TokenStats{TotalTokens: 1}, "", "")

	principal := identity.Principal{EffectiveTenant: identity.Tenant{ID: businessTenant}}
	rec := serveMonitor(t, &Handler{cfg: &config.Config{}}, "/usage/monitor/overview?range=1h", &principal, false)
	var body usage.MonitorOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Models.Rows) != 1 || body.Models.Rows[0].Key != "gpt-business" {
		t.Fatalf("business tenant models = %+v, want only gpt-business", body.Models.Rows)
	}
}
