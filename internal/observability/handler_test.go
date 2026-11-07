package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func scrape(t *testing.T, e *Exporter, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(MetricsPath, e.Handler())
	req := httptest.NewRequest(http.MethodGet, MetricsPath, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestMetricsEndpointIsHiddenWhileOff(t *testing.T) {
	e, _ := newPersistedExporter(t)
	if rec := scrape(t, e, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("off: status %d, want 404", rec.Code)
	}
	_, token, _ := e.SetEnabled(true)
	_, _, _ = e.SetEnabled(false)
	// Switched off, even the right token gets the same 404.
	if rec := scrape(t, e, "Bearer "+token); rec.Code != http.StatusNotFound {
		t.Fatalf("off with token: status %d, want 404", rec.Code)
	}
}

func TestMetricsEndpointRequiresTheBearerToken(t *testing.T) {
	e, _ := newPersistedExporter(t)
	_, token, _ := e.SetEnabled(true)
	for _, header := range []string{"", "Bearer", "Bearer wrong", "Basic " + token, token} {
		rec := scrape(t, e, header)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: status %d, want 401", header, rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("Authorization %q: missing WWW-Authenticate challenge", header)
		}
	}

	// The token is never accepted from the query string, where access logs
	// would keep it.
	router := gin.New()
	router.GET(MetricsPath, e.Handler())
	req := httptest.NewRequest(http.MethodGet, MetricsPath+"?token="+token, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token: status %d, want 401", rec.Code)
	}
}

func TestMetricsEndpointServesTheExposition(t *testing.T) {
	e, _ := newPersistedExporter(t)
	_, token, _ := e.SetEnabled(true)
	e.ObserveUsage(coreusage.Record{Provider: "codex", Model: "gpt-5.5", AuthIndex: "a1b2"})
	e.ObserveHTTP(http.MethodPost, "/v1/responses", http.StatusOK, time.Second)

	rec := scrape(t, e, "bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`clirelay_upstream_requests_total{model="gpt-5.5",outcome="success",provider="codex"} 1`,
		`clirelay_upstream_account_requests_total{auth_index="a1b2",outcome="success",provider="codex"} 1`,
		`clirelay_http_requests_total{method="POST",route="/v1/responses",status_code="200"} 1`,
		"clirelay_build_info{",
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition lacks %q", want)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":     "abc",
		"bearer  abc ":   "abc",
		"BEARER abc":     "abc",
		"Basic abc":      "",
		"Bearer":         "",
		"":               "",
		"abc":            "",
		"Bearer abc def": "abc def",
	}
	for header, want := range cases {
		if got := bearerToken(header); got != want {
			t.Fatalf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestHTTPMiddlewareCountsRecoveredPanicsAsServerErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := NewExporter()
	router := gin.New()
	// The order the server installs them in: metrics outside recovery.
	router.Use(e.HTTPMiddleware())
	router.Use(gin.Recovery())
	router.GET("/boom/:id", func(*gin.Context) { panic("handler bug") })
	router.GET("/ok", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for _, path := range []string{"/boom/1", "/boom/2", "/ok", "/nowhere"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if got := value(t, e, "clirelay_http_requests_total", map[string]string{"route": "/boom/:id", "status_code": "500"}).value; got != 2 {
		t.Fatalf("panics counted as 500 = %v, want 2 (the route template, not the path)", got)
	}
	value(t, e, "clirelay_http_requests_total", map[string]string{"route": "/ok", "status_code": "204"})
	value(t, e, "clirelay_http_requests_total", map[string]string{"route": "unmatched", "status_code": "404"})
}
