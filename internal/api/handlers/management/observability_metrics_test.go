package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
)

type observabilityTestStore struct {
	stored  *observability.Settings
	saveErr error
}

func (s *observabilityTestStore) Load() (observability.Settings, bool, error) {
	if s.stored == nil {
		return observability.Settings{}, false, nil
	}
	return *s.stored, true, nil
}

func (s *observabilityTestStore) Save(settings observability.Settings) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	copied := settings
	s.stored = &copied
	return nil
}

func observabilityRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/observability/metrics", h.GetObservabilityMetrics)
	router.PUT("/observability/metrics", h.PutObservabilityMetrics)
	router.POST("/observability/metrics/token", h.PostObservabilityMetricsToken)
	return router
}

func callObservability(t *testing.T, router *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s %s: body is not JSON: %s", method, path, rec.Body.String())
	}
	return rec, decoded
}

func TestObservabilityMetricsSettingsLifecycle(t *testing.T) {
	exporter := observability.NewExporter()
	store := &observabilityTestStore{}
	if err := exporter.SetPersister(store); err != nil {
		t.Fatalf("SetPersister: %v", err)
	}
	router := observabilityRouter(&Handler{metrics: exporter})

	rec, body := callObservability(t, router, http.MethodGet, "/observability/metrics", "")
	if rec.Code != http.StatusOK || body["enabled"] != false || body["token_configured"] != false || body["path"] != "/metrics" || body["persistent"] != true {
		t.Fatalf("initial GET: %d %v", rec.Code, body)
	}

	rec, body = callObservability(t, router, http.MethodPut, "/observability/metrics", `{"enabled":true}`)
	token, _ := body["token"].(string)
	if rec.Code != http.StatusOK || body["enabled"] != true || !strings.HasPrefix(token, "crm_") {
		t.Fatalf("enable: %d %v", rec.Code, body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("a response carrying the token has Cache-Control %q", got)
	}
	if body["token_hint"] != token[len(token)-4:] {
		t.Fatalf("token_hint = %v, want the token's last four characters", body["token_hint"])
	}

	// The token is shown once; the hash is never shown.
	rec, body = callObservability(t, router, http.MethodGet, "/observability/metrics", "")
	if _, ok := body["token"]; ok {
		t.Fatalf("GET returned the token: %v", body)
	}
	if strings.Contains(rec.Body.String(), store.stored.TokenSHA256) {
		t.Fatal("GET returned the token hash")
	}

	rec, body = callObservability(t, router, http.MethodPost, "/observability/metrics/token", "")
	rotated, _ := body["token"].(string)
	if rec.Code != http.StatusOK || rotated == "" || rotated == token {
		t.Fatalf("rotate: %d %v", rec.Code, body)
	}

	rec, body = callObservability(t, router, http.MethodPut, "/observability/metrics", `{"enabled":false}`)
	if rec.Code != http.StatusOK || body["enabled"] != false || body["token_configured"] != true {
		t.Fatalf("disable: %d %v (the token must survive)", rec.Code, body)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("disable returned a token: %v", body)
	}
}

func TestObservabilityMetricsRejectsBadBodies(t *testing.T) {
	exporter := observability.NewExporter()
	_ = exporter.SetPersister(&observabilityTestStore{})
	router := observabilityRouter(&Handler{metrics: exporter})
	for _, body := range []string{``, `{}`, `{"enabled":"yes"}`, `not json`} {
		rec, decoded := callObservability(t, router, http.MethodPut, "/observability/metrics", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status %d, want 400 (%v)", body, rec.Code, decoded)
		}
	}
	if exporter.Settings().Enabled {
		t.Fatal("a rejected body changed the settings")
	}
}

func TestObservabilityMetricsWriteFailures(t *testing.T) {
	// No store: refused, never applied in memory only.
	router := observabilityRouter(&Handler{metrics: observability.NewExporter()})
	rec, body := callObservability(t, router, http.MethodPut, "/observability/metrics", `{"enabled":true}`)
	if rec.Code != http.StatusServiceUnavailable || errorCode(body) != "observability_unavailable" {
		t.Fatalf("no store: %d %v", rec.Code, body)
	}
	rec, body = callObservability(t, router, http.MethodPost, "/observability/metrics/token", "")
	if rec.Code != http.StatusServiceUnavailable || errorCode(body) != "observability_unavailable" {
		t.Fatalf("no store, rotate: %d %v", rec.Code, body)
	}

	exporter := observability.NewExporter()
	_ = exporter.SetPersister(&observabilityTestStore{saveErr: errors.New("disk full")})
	router = observabilityRouter(&Handler{metrics: exporter})
	rec, body = callObservability(t, router, http.MethodPut, "/observability/metrics", `{"enabled":true}`)
	if rec.Code != http.StatusInternalServerError || errorCode(body) != "observability_save_failed" {
		t.Fatalf("save failure: %d %v", rec.Code, body)
	}
	if _, ok := body["token"]; ok || exporter.Settings().Enabled {
		t.Fatalf("a failed save still issued a token or enabled the endpoint: %v", body)
	}
}

func errorCode(body map[string]any) string {
	inner, _ := body["error"].(map[string]any)
	code, _ := inner["code"].(string)
	return code
}
