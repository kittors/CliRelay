package management

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// updateProgressFailures maps each progress endpoint to the error it reports when
// the updater answers badly.
var updateProgressFailures = map[string]string{
	"/v0/management/update/progress": "update_progress_failed",
	"/v0/management/update/events":   "update_events_failed",
}

// autoUpdateEnabledConfig is a config whose progress endpoints talk to the updater.
// With auto-update off they answer updater_unavailable without dialling it.
func autoUpdateEnabledConfig() *config.Config {
	return &config.Config{AutoUpdate: config.AutoUpdateConfig{Enabled: true}}
}

func serveUpdateProgress(t *testing.T, handler *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	switch path {
	case "/v0/management/update/progress":
		handler.GetUpdateProgress(ctx)
	case "/v0/management/update/events":
		handler.StreamUpdateProgress(ctx)
	default:
		t.Fatalf("unknown progress endpoint %q", path)
	}
	return recorder
}

func assertUpdateProgressError(t *testing.T, path string, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != status || body.Error != code {
		t.Fatalf("%s: status = %d, body = %s, want %d %s", path, recorder.Code, recorder.Body.String(), status, code)
	}
}

func TestUpdateProgressEndpointsSkipTheUpdaterWhenAutoUpdateIsDisabled(t *testing.T) {
	var calls atomic.Int32
	updater := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(updater.Close)
	t.Setenv("CLIRELAY_UPDATER_URL", updater.URL)
	t.Setenv("CLIRELAY_UPDATER_TOKEN", "test-token")

	handler := &Handler{cfg: &config.Config{AutoUpdate: config.AutoUpdateConfig{Enabled: false}}}
	for path := range updateProgressFailures {
		recorder := serveUpdateProgress(t, handler, path)
		assertUpdateProgressError(t, path, recorder, http.StatusServiceUnavailable, "updater_unavailable")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("updater received %d requests, want none while auto-update is disabled", got)
	}
}

func TestUpdateProgressEndpointsReportAnUnreachableUpdaterAsUnavailable(t *testing.T) {
	// A port that was just released refuses connections: nothing is listening at
	// the configured updater address, as on a deployment without the sidecar.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	t.Setenv("CLIRELAY_UPDATER_URL", "http://"+addr)
	t.Setenv("CLIRELAY_UPDATER_TOKEN", "test-token")

	handler := &Handler{cfg: autoUpdateEnabledConfig()}
	for path := range updateProgressFailures {
		recorder := serveUpdateProgress(t, handler, path)
		assertUpdateProgressError(t, path, recorder, http.StatusServiceUnavailable, "updater_unavailable")
	}
}

func TestUpdateProgressEndpointsKeepBadGatewayForAnUpdaterThatAnswersBadly(t *testing.T) {
	updater := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "update engine failed", http.StatusInternalServerError)
	}))
	t.Cleanup(updater.Close)
	t.Setenv("CLIRELAY_UPDATER_URL", updater.URL)
	t.Setenv("CLIRELAY_UPDATER_TOKEN", "test-token")

	handler := &Handler{cfg: autoUpdateEnabledConfig()}
	for path, failure := range updateProgressFailures {
		recorder := serveUpdateProgress(t, handler, path)
		assertUpdateProgressError(t, path, recorder, http.StatusBadGateway, failure)
	}
}
