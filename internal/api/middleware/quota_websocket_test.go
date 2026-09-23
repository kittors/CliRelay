package middleware

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// GET /v1/responses is also the Responses WebSocket upgrade, and every turn sent
// over the connection reaches the executor. The quota middleware used to skip
// every non-POST request, so these tests pin what the handshake now checks.

func websocketHandshakeRequest(mutate func(http.Header)) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if mutate != nil {
		mutate(req.Header)
	}
	return req
}

func TestQuotaMiddlewareRefusesExhaustedWebsocketHandshakeLikePOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases, budgetOnly := quotaRefusalContractCases()
	for _, tc := range cases {
		if !budgetOnly[tc.name] {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			tc.setup(t)
			recorder, handled, diag := serveQuotaContract(t, tc.metadata, websocketHandshakeRequest(nil))
			assertQuotaContract(t, tc, recorder, handled, diag)
		})
	}
}

// Every request the gorilla upgrader would accept must be checked, whatever
// form its Connection/Upgrade headers take.
func TestQuotaMiddlewareChecksEveryHandshakeTheUpgraderAccepts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	variants := []struct {
		name   string
		mutate func(http.Header)
	}{
		{name: "canonical"},
		{name: "mixed case tokens", mutate: func(h http.Header) {
			h.Set("Connection", "keep-alive, UPGRADE")
			h.Set("Upgrade", "WebSocket")
		}},
		{name: "websocket listed after another protocol", mutate: func(h http.Header) {
			h.Set("Upgrade", "h2c, websocket")
		}},
		{name: "websocket on a second Upgrade line", mutate: func(h http.Header) {
			h.Set("Upgrade", "h2c")
			h.Add("Upgrade", "websocket")
		}},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			countTodayByKeyFunc = func(string) (int64, error) { return 10, nil }
			req := websocketHandshakeRequest(variant.mutate)
			if !websocket.IsWebSocketUpgrade(req) {
				t.Fatalf("test request is not a handshake the upgrader accepts")
			}
			recorder, handled, _ := serveQuotaContract(t, map[string]string{"daily-limit": "10"}, req)
			if handled || recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d handled = %v, want 429 before the handler", recorder.Code, handled)
			}
		})
	}
}

// The handshake opens a connection; it is not a request. It must not count
// toward RPM or hold a concurrency slot, and throttles that describe the moment
// of a request (RPM, TPM, concurrency) are left to each turn.
func TestQuotaMiddlewareHandshakeIsNotARequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	metadata := map[string]string{"rpm-limit": "1", "tpm-limit": "100", "concurrency-limit": "1", "daily-limit": "10"}
	rpm := getRPMTracker(contractKey)
	rpm.add()
	rpm.add()
	RecordTokenUsage(contractKey, 150)
	if _, ok := acquireKeyConcurrency(contractKey, 1); !ok {
		t.Fatal("could not occupy the only slot")
	}

	recorder, handled, _ := serveQuotaContract(t, metadata, websocketHandshakeRequest(nil))
	if !handled {
		t.Fatalf("handshake refused with %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := rpm.count(); got != 2 {
		t.Fatalf("rpm count after handshake = %d, want 2 (unchanged)", got)
	}
	if got := keyConcurrencyCount(contractKey); got != 1 {
		t.Fatalf("in-flight after handshake = %d, want 1 (unchanged)", got)
	}
}

// Plain GETs (/models, a /responses GET that is not an upgrade) never reach an
// executor, so they stay unchecked and uncounted even for an exhausted key.
func TestQuotaMiddlewareLeavesPlainGETsAlone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/models", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			countTodayByKeyFunc = func(string) (int64, error) {
				t.Error("usage queried for a GET that is not a handshake")
				return 10, nil
			}
			recorder, handled, diag := serveQuotaContract(t, map[string]string{"daily-limit": "10"}, httptest.NewRequest(http.MethodGet, path, nil))
			if !handled || recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d handled = %v, want 204 and handled", recorder.Code, handled)
			}
			if got := getRPMTracker(contractKey).count(); got != 0 {
				t.Fatalf("rpm count = %d, want 0", got)
			}
			if snapshot := diag.Snapshot(); snapshot.Quota != nil {
				t.Fatalf("diagnostics quota = %+v, want none", snapshot.Quota)
			}
		})
	}
}

// handshakeTurnGate runs a WebSocket handshake for the key and returns the gate
// published for its turns, if any.
func handshakeTurnGate(t *testing.T, metadata map[string]string) handlers.QuotaGate {
	t.Helper()
	var gate handlers.QuotaGate
	recorder, handled, _ := serveQuotaContractWith(t, metadata, websocketHandshakeRequest(nil), func(c *gin.Context) {
		gate = handlers.QuotaGateFromGin(c)
	})
	if !handled {
		t.Fatalf("handshake refused with %d: %s", recorder.Code, recorder.Body.String())
	}
	return gate
}

func TestQuotaMiddlewarePublishesTurnGateOnlyForKeysWithLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name     string
		metadata map[string]string
		want     bool
	}{
		{name: "no metadata", metadata: nil, want: false},
		{name: "metadata without limits", metadata: map[string]string{"tenant-id": "tenant-a"}, want: false},
		{name: "rpm limit", metadata: map[string]string{"rpm-limit": "5"}, want: true},
		{name: "period limit", metadata: map[string]string{"key-period-spending-limit-month": "5"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			if got := handshakeTurnGate(t, tc.metadata) != nil; got != tc.want {
				t.Fatalf("gate published = %v, want %v", got, tc.want)
			}
		})
	}
}

// Each turn is refused exactly as a POST would be at that moment: same status,
// same body, same headers — and a refused turn keeps no concurrency slot.
func TestQuotaTurnGateRefusesLikePOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases, _ := quotaRefusalContractCases()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			gate := handshakeTurnGate(t, tc.metadata)
			if gate == nil {
				t.Fatal("no turn gate published")
			}
			tc.setup(t) // exhaust the limit after the connection is open
			subject := quotaSubjectKey(contractKey, tc.metadata)
			inFlight := keyConcurrencyCount(subject)

			release, rejection := gate()
			if release != nil || rejection == nil {
				t.Fatalf("turn admitted, want it refused")
			}
			if rejection.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rejection.StatusCode, tc.wantStatus)
			}
			if got := string(rejection.Body); got != tc.wantBody {
				t.Fatalf("body mismatch\n got: %s\nwant: %s", got, tc.wantBody)
			}
			if got, want := quotaHeaders(rejection.Headers), canonicalHeaders(tc.wantHeader); !reflect.DeepEqual(got, want) {
				t.Fatalf("headers = %v, want %v", got, want)
			}
			if got := keyConcurrencyCount(subject); got != inFlight {
				t.Fatalf("in-flight after refusal = %d, want %d", got, inFlight)
			}
		})
	}
}

func TestQuotaTurnGateCountsTurnsAndHoldsSlotUntilRelease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	gate := handshakeTurnGate(t, map[string]string{"concurrency-limit": "1", "rpm-limit": "10", "daily-limit": "10"})
	if gate == nil {
		t.Fatal("no turn gate published")
	}
	rpm := getRPMTracker(contractKey)
	if got := rpm.count(); got != 0 {
		t.Fatalf("rpm count after handshake = %d, want 0", got)
	}

	release, rejection := gate()
	if rejection != nil {
		t.Fatalf("first turn refused: %s", rejection.Body)
	}
	if got := rpm.count(); got != 1 {
		t.Fatalf("rpm count after one turn = %d, want 1", got)
	}
	if got := keyConcurrencyCount(contractKey); got != 1 {
		t.Fatalf("in-flight during the turn = %d, want 1", got)
	}

	// The running turn holds the only slot; a refused turn still counts toward
	// RPM, as a refused POST does.
	_, second := gate()
	if second == nil || !strings.Contains(string(second.Body), "concurrency_limit_exceeded") {
		t.Fatalf("second turn = %v, want a concurrency refusal", second)
	}
	if got := rpm.count(); got != 2 {
		t.Fatalf("rpm count after a refused turn = %d, want 2", got)
	}

	release()
	if got := keyConcurrencyCount(contractKey); got != 0 {
		t.Fatalf("in-flight after release = %d, want 0", got)
	}

	// A turn refused by a later check gives back the slot it took first.
	countTodayByKeyFunc = func(string) (int64, error) { return 10, nil }
	if _, third := gate(); third == nil || !strings.Contains(string(third.Body), "daily_limit_exceeded") {
		t.Fatalf("third turn = %v, want a daily limit refusal", third)
	}
	if got := keyConcurrencyCount(contractKey); got != 0 {
		t.Fatalf("in-flight after a refused turn = %d, want 0", got)
	}
}
