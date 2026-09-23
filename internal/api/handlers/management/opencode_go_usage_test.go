package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestParseOpenCodeGoUsage(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	percent := func(v float64) *float64 { return &v }
	payload := openCodeGoUsageResponse{}
	payload.Usage.Rolling = &openCodeGoUsageWindow{Status: "ok", Percent: percent(12.4), ResetsAt: now.Add(62 * time.Minute).Format(time.RFC3339Nano)}
	payload.Usage.Weekly = &openCodeGoUsageWindow{Status: "ok", Percent: percent(34), ResetsAt: now.Add(120 * time.Hour).Format(time.RFC3339Nano)}
	payload.Usage.Monthly = &openCodeGoUsageWindow{Status: "rate-limited", Percent: percent(100), ResetsAt: now.Add(696 * time.Hour).Format(time.RFC3339Nano)}

	items := parseOpenCodeGoUsageAt(payload, now)
	if len(items) != 3 {
		t.Fatalf("usage item count = %d, want 3: %+v", len(items), items)
	}
	if items[0].Type != "rolling" || items[0].Label != "Rolling" || items[0].Percentage != 12.4 || items[0].ResetsIn != "1 hour 2 minutes" {
		t.Fatalf("rolling item = %+v", items[0])
	}
	if items[1].Type != "weekly" || items[1].Percentage != 34 || items[1].ResetsIn != "5 days" {
		t.Fatalf("weekly item = %+v", items[1])
	}
	if items[2].Type != "monthly" || items[2].Percentage != 100 || items[2].ResetsIn != "29 days" {
		t.Fatalf("monthly item = %+v", items[2])
	}
}

// A window the account does not have must be dropped rather than rendered as
// 0%, which would read as "plenty left" on a plan that has none.
func TestParseOpenCodeGoUsageSkipsWindowsWithoutPercent(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	percent := 5.0
	payload := openCodeGoUsageResponse{}
	payload.Usage.Rolling = &openCodeGoUsageWindow{Status: "ok", Percent: &percent, ResetsAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
	payload.Usage.Weekly = &openCodeGoUsageWindow{Status: "ok"}

	items := parseOpenCodeGoUsageAt(payload, now)
	if len(items) != 1 || items[0].Type != "rolling" {
		t.Fatalf("items = %+v, want rolling only", items)
	}
}

func TestNormalizeDashboardCookieForUsageChecks(t *testing.T) {
	tests := map[string]string{
		"token":                  "token",
		" auth=abc123; extra=1 ": "auth=abc123; extra=1",
		"Cookie: foo=bar":        "foo=bar",
		"bad\r\ninjection":       "",
	}
	for input, want := range tests {
		if got := normalizeDashboardCookie(input); got != want {
			t.Fatalf("normalizeDashboardCookie(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseOllamaCloudUsageHTML(t *testing.T) {
	html := `<html><body>
		<div>Session usage <strong>0% used</strong><span>Resets in 4 hours.</span></div>
		<div>Weekly usage <strong>1.6% used</strong><span>Resets in 4 days.</span></div>
	</body></html>`

	items := parseOllamaCloudUsageHTML(html)
	if len(items) != 2 {
		t.Fatalf("usage item count = %d, want 2: %+v", len(items), items)
	}
	if items[0].Type != "session" || items[0].Percentage != 0 || items[0].ResetsIn != "4 hours" {
		t.Fatalf("session item = %+v", items[0])
	}
	if items[1].Type != "weekly" || items[1].Percentage != 1.6 || items[1].ResetsIn != "4 days" {
		t.Fatalf("weekly item = %+v", items[1])
	}
}

func TestQueryOpenCodeGoUsageFetchesUsageAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resetAt := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-go" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Fatalf("cookie = %q, want none: usage must authenticate with the API key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":12,"resetsAt":"` + resetAt + `"},` +
			`"weekly":{"status":"ok","percent":34,"resetsAt":"` + resetAt + `"},` +
			`"monthly":{"status":"ok","percent":56,"resetsAt":"` + resetAt + `"}}}`))
	}))
	defer upstream.Close()

	prevURL := openCodeGoUsageAPIURL
	openCodeGoUsageAPIURL = upstream.URL
	defer func() { openCodeGoUsageAPIURL = prevURL }()

	h := &Handler{cfg: &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{APIKey: "sk-go", Name: "OpenCode Go"}},
	}}

	body := []byte(`{"index":0}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/opencode-go-api-key/usage", bytes.NewReader(body))

	h.QueryOpenCodeGoUsage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var decoded struct {
		Usage []openCodeGoUsageItem `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Usage) != 3 || decoded.Usage[0].Label != "Rolling" || decoded.Usage[2].Percentage != 56 {
		t.Fatalf("response = %+v", decoded)
	}
}

// An account that never subscribed to Go must not be reported as a broken
// credential: the key still serves inference, and the operator needs to be told
// what is actually missing.
func TestQueryOpenCodeGoUsageReportsMissingSubscription(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"EntitlementError","message":"OpenCode Go subscription required."}}`))
	}))
	defer upstream.Close()

	prevURL := openCodeGoUsageAPIURL
	openCodeGoUsageAPIURL = upstream.URL
	defer func() { openCodeGoUsageAPIURL = prevURL }()

	h := &Handler{cfg: &config.Config{}}
	body := []byte(`{"api-key":"sk-go"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/opencode-go-api-key/usage", bytes.NewReader(body))

	h.QueryOpenCodeGoUsage(c)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("subscription required")) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestQueryOpenCodeGoUsageReportsInvalidKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}`))
	}))
	defer upstream.Close()

	prevURL := openCodeGoUsageAPIURL
	openCodeGoUsageAPIURL = upstream.URL
	defer func() { openCodeGoUsageAPIURL = prevURL }()

	h := &Handler{cfg: &config.Config{}}
	body := []byte(`{"api-key":"sk-bad"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/opencode-go-api-key/usage", bytes.NewReader(body))

	h.QueryOpenCodeGoUsage(c)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("Unauthorized")) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// A panel that has not been redeployed still posts workspace-id and
// auth-cookie. Those must not be mistaken for credentials: without an api-key
// the request is incomplete, and saying so beats scraping with a cookie.
func TestQueryOpenCodeGoUsageRequiresAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{cfg: &config.Config{}}
	body := []byte(`{"workspace-id":"wrk_test","auth-cookie":"token"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/opencode-go-api-key/usage", bytes.NewReader(body))

	h.QueryOpenCodeGoUsage(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("api-key is required")) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestQueryClineUsageFetchesDashboardAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resetAt := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/me/plan/usage-limits" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "session=cline" {
			t.Fatalf("cookie = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"limits":[{"type":"five_hour","percentUsed":2,"resetsAt":"` + resetAt + `"},{"type":"weekly","percentUsed":3,"resetsAt":"` + resetAt + `"},{"type":"monthly","percentUsed":39,"resetsAt":"` + resetAt + `"}]},"success":true}`))
	}))
	defer upstream.Close()

	prevBaseURL := clineUsageAPIBaseURL
	clineUsageAPIBaseURL = upstream.URL
	defer func() { clineUsageAPIBaseURL = prevBaseURL }()

	h := &Handler{cfg: &config.Config{
		ClineKey: []config.ClineKey{{
			APIKey:     "sk-cline",
			Name:       "ClinePass",
			AuthCookie: "Cookie: session=cline",
		}},
	}}

	body := []byte(`{"index":0}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/cline-api-key/usage", bytes.NewReader(body))

	h.QueryClineUsage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var decoded struct {
		Usage []openCodeGoUsageItem `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Usage) != 3 || decoded.Usage[0].Type != "five_hour" || decoded.Usage[2].Percentage != 39 {
		t.Fatalf("response = %+v", decoded)
	}
}

func TestQueryOllamaCloudUsageFetchesSettingsPage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settings" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "ollama_session=ok" {
			t.Fatalf("cookie = %q", got)
		}
		_, _ = w.Write([]byte(`Session usage 0% used Resets in 4 hours. Weekly usage 1.6% used Resets in 4 days.`))
	}))
	defer upstream.Close()

	prevSettingsURL := ollamaCloudSettingsURL
	ollamaCloudSettingsURL = upstream.URL + "/settings"
	defer func() { ollamaCloudSettingsURL = prevSettingsURL }()

	h := &Handler{cfg: &config.Config{
		OllamaCloudKey: []config.OllamaCloudKey{{
			APIKey:     "sk-ollama",
			Name:       "Ollama Cloud",
			AuthCookie: "ollama_session=ok",
		}},
	}}

	body := []byte(`{"index":0}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/ollama-cloud-api-key/usage", bytes.NewReader(body))

	h.QueryOllamaCloudUsage(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var decoded struct {
		Usage []openCodeGoUsageItem `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Usage) != 2 || decoded.Usage[0].Type != "session" || decoded.Usage[1].Percentage != 1.6 {
		t.Fatalf("response = %+v", decoded)
	}
}
