package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// defaultSeries reads one series of the process-wide exporter, 0 when absent.
func defaultSeries(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := observability.Default().Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, metric := range family.GetMetric() {
			got := map[string]string{}
			for _, pair := range metric.GetLabel() {
				got[pair.GetName()] = pair.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metrics
				}
			}
			switch family.GetType() {
			case dto.MetricType_COUNTER:
				return metric.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func TestMetricsEndpointIsOffByDefault(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, observability.MetricsPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics with nothing configured: status %d, want 404", rec.Code)
	}
}

func TestServerCountsRequestsByRouteTemplate(t *testing.T) {
	server := newTestServer(t)
	labels := map[string]string{"method": http.MethodGet, "route": "/healthz", "status_code": "204"}
	before := defaultSeries(t, "clirelay_http_requests_total", labels)
	for range 3 {
		server.engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if got := defaultSeries(t, "clirelay_http_requests_total", labels) - before; got != 3 {
		t.Fatalf("healthz requests counted = %v, want 3", got)
	}

	// An unknown path is one fixed label value, however many paths are probed.
	unmatched := map[string]string{"route": "unmatched", "status_code": "404"}
	before = defaultSeries(t, "clirelay_http_requests_total", unmatched)
	for _, path := range []string{"/wp-login.php", "/.env", "/a/b/c"} {
		server.engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if got := defaultSeries(t, "clirelay_http_requests_total", unmatched) - before; got != 3 {
		t.Fatalf("unmatched requests counted = %v, want 3", got)
	}
}

func TestServerExportsItsAccountsAtScrapeTime(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.handlers.AuthManager.Register(context.Background(), &auth.Auth{
		ID:       "metrics-test-account",
		Provider: "metrics-test-provider",
		Status:   auth.StatusActive,
		Disabled: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := defaultSeries(t, "clirelay_accounts", map[string]string{"provider": "metrics-test-provider", "state": "disabled"}); got != 1 {
		t.Fatalf("disabled accounts of the test provider = %v, want 1", got)
	}
}
