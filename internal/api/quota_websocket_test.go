package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// A Responses WebSocket turn reaches the executor exactly like a POST
// /v1/responses does, so it has to pass the same quota checks. These tests drive
// the real QuotaMiddleware, model gate and WebSocket handler against an executor
// that counts every upstream call.

const (
	quotaWSModel      = "quota-ws-model"
	quotaWSOtherModel = "quota-ws-other-model"
)

// quotaWSUsage stands in for the usage database behind the budget checks. Tests
// change it between turns to exhaust a budget while a connection stays open.
type quotaWSUsage struct {
	mu        sync.Mutex
	today     int64
	total     int64
	totalCost float64
	todayCost float64
	keyPeriod quota.PeriodSpendingUsage
	fail      bool
}

func (u *quotaWSUsage) set(change func(*quotaWSUsage)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	change(u)
}

var errQuotaWSUsageUnavailable = errors.New("usage database unavailable")

func installQuotaWSUsage(t *testing.T) *quotaWSUsage {
	t.Helper()
	u := &quotaWSUsage{}
	count := func(field func(*quotaWSUsage) int64) func(string) (int64, error) {
		return func(string) (int64, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.fail {
				return 0, errQuotaWSUsageUnavailable
			}
			return field(u), nil
		}
	}
	cost := func(field func(*quotaWSUsage) float64) func(string) (float64, error) {
		return func(string) (float64, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.fail {
				return 0, errQuotaWSUsageUnavailable
			}
			return field(u), nil
		}
	}
	period := func(string, string) (quota.PeriodSpendingUsage, error) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.fail {
			return quota.PeriodSpendingUsage{}, errQuotaWSUsageUnavailable
		}
		return u.keyPeriod, nil
	}
	today := count(func(u *quotaWSUsage) int64 { return u.today })
	total := count(func(u *quotaWSUsage) int64 { return u.total })
	totalCost := cost(func(u *quotaWSUsage) float64 { return u.totalCost })
	todayCost := cost(func(u *quotaWSUsage) float64 { return u.todayCost })
	middleware.InitQuotaUsageFuncs(today, total, totalCost, todayCost)
	middleware.InitQuotaEndUserUsageFuncs(today, total, totalCost, todayCost)
	middleware.InitQuotaPeriodUsageFuncs(period, period)
	t.Cleanup(func() {
		// Back to the package defaults, which fail closed.
		unavailableCount := func(string) (int64, error) { return 0, errQuotaWSUsageUnavailable }
		unavailableCost := func(string) (float64, error) { return 0, errQuotaWSUsageUnavailable }
		unavailablePeriod := func(string, string) (quota.PeriodSpendingUsage, error) {
			return quota.PeriodSpendingUsage{}, errQuotaWSUsageUnavailable
		}
		middleware.InitQuotaUsageFuncs(unavailableCount, unavailableCount, unavailableCost, unavailableCost)
		middleware.InitQuotaEndUserUsageFuncs(unavailableCount, unavailableCount, unavailableCost, unavailableCost)
		middleware.InitQuotaPeriodUsageFuncs(unavailablePeriod, unavailablePeriod)
	})
	return u
}

// quotaWSExecutor counts upstream calls. holdNextTurns makes calls block before
// returning, so a test can observe what a running turn holds.
type quotaWSExecutor struct {
	provider string
	calls    atomic.Int64
	started  chan struct{}

	mu         sync.Mutex
	hold       chan struct{}
	failStart  bool
	failStream bool
}

func (e *quotaWSExecutor) Identifier() string { return e.provider }

func (e *quotaWSExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *quotaWSExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls.Add(1)
	e.mu.Lock()
	hold, failStart, failStream := e.hold, e.failStart, e.failStream
	e.mu.Unlock()
	select {
	case e.started <- struct{}{}:
	default:
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failStart {
		return nil, errors.New("upstream refused the request")
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	if failStream {
		chunks <- coreexecutor.StreamChunk{Err: errors.New("upstream stream broke")}
	} else {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp_quota_ws","output":[]}}`)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *quotaWSExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *quotaWSExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *quotaWSExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *quotaWSExecutor) configure(change func(*quotaWSExecutor)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	change(e)
}

func (e *quotaWSExecutor) holdNextTurns(t *testing.T) (release func()) {
	t.Helper()
	hold := make(chan struct{})
	e.configure(func(e *quotaWSExecutor) { e.hold = hold })
	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	return release
}

func (e *quotaWSExecutor) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-e.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the executor")
	}
}

type quotaWSHarness struct {
	t      *testing.T
	base   string
	apiKey string
	exec   *quotaWSExecutor
	usage  *quotaWSUsage
}

func newQuotaWSHarness(t *testing.T, metadata map[string]string) *quotaWSHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	initDisabledModelTestDB(t) // the model gate reads the model catalog
	usage := installQuotaWSUsage(t)

	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	exec := &quotaWSExecutor{provider: "quota-ws-" + name, started: make(chan struct{}, 16)}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "auth-" + exec.provider, Provider: exec.provider, Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, exec.provider, []*registry.ModelInfo{{ID: quotaWSModel}, {ID: quotaWSOtherModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	server := &Server{cfg: cfg, handlers: base}
	responses := openai.NewOpenAIResponsesAPIHandler(base)
	apiKey := "sk-" + exec.provider

	engine := gin.New()
	v1 := engine.Group("/v1")
	// Stands in for AuthMiddleware; the rest mirrors the chain in routes_public.go.
	v1.Use(func(c *gin.Context) {
		c.Set("apiKey", apiKey)
		if metadata != nil {
			c.Set("accessMetadata", metadata)
		}
		c.Next()
	})
	v1.Use(middleware.QuotaMiddleware())
	v1.Use(server.modelRestrictionMiddleware())
	v1.GET("/responses", responses.ResponsesWebsocket)
	v1.POST("/responses", responses.Responses)
	// A POST with no executor behind it: shows whether the key's concurrency
	// slot is free without spending an upstream call.
	v1.POST("/quota-probe", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	return &quotaWSHarness{t: t, base: srv.URL, apiKey: apiKey, exec: exec, usage: usage}
}

func (h *quotaWSHarness) dial() (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.base, "http")+"/v1/responses", nil)
}

func (h *quotaWSHarness) mustDial() *websocket.Conn {
	h.t.Helper()
	conn, resp, err := h.dial()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		h.t.Fatalf("dial: %v (status %d)", err, status)
	}
	h.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (h *quotaWSHarness) send(conn *websocket.Conn, model string) {
	h.t.Helper()
	frame := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, model)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		h.t.Fatalf("write frame: %v", err)
	}
}

func (h *quotaWSHarness) read(conn *websocket.Conn) []byte {
	h.t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, reply, err := conn.ReadMessage()
	if err != nil {
		h.t.Fatalf("read reply: %v", err)
	}
	return reply
}

func (h *quotaWSHarness) turn(conn *websocket.Conn, model string) []byte {
	h.t.Helper()
	h.send(conn, model)
	return h.read(conn)
}

func (h *quotaWSHarness) mustComplete(conn *websocket.Conn) {
	h.t.Helper()
	if reply := h.turn(conn, quotaWSModel); gjson.GetBytes(reply, "type").String() != "response.done" {
		h.t.Fatalf("turn reply = %s, want response.done", reply)
	}
}

// post sends the same turn as a POST /v1/responses.
func (h *quotaWSHarness) post() (int, []byte, http.Header) {
	h.t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, quotaWSModel)
	resp, err := http.Post(h.base+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("POST /v1/responses: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func (h *quotaWSHarness) probe() (int, string) {
	h.t.Helper()
	resp, err := http.Post(h.base+"/v1/quota-probe", "application/json", nil)
	if err != nil {
		h.t.Fatalf("POST /v1/quota-probe: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-CliRelay-Quota-Code")
}

// waitSlotFree polls until a POST is admitted again. A dropped client is only
// noticed on the next write, so the release after a disconnect is not instant.
func (h *quotaWSHarness) waitSlotFree() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, code := h.probe()
		if status == http.StatusNoContent {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("slot never freed: probe = %d (%s)", status, code)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startHeldTurn sends a turn whose executor call blocks, and proves the running
// turn holds the key's only concurrency slot the way a running POST does.
func (h *quotaWSHarness) startHeldTurn(conn *websocket.Conn) (release func()) {
	h.t.Helper()
	release = h.exec.holdNextTurns(h.t)
	h.send(conn, quotaWSModel)
	h.exec.waitStarted(h.t)
	if status, code := h.probe(); status != http.StatusTooManyRequests || code != "concurrency_limit_exceeded" {
		h.t.Fatalf("probe during a running turn = %d (%s), want 429 concurrency_limit_exceeded", status, code)
	}
	return release
}

func quotaHeaderSet(h http.Header) map[string]string {
	out := map[string]string{}
	for key, values := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "X-Clirelay-") && len(values) > 0 {
			out[http.CanonicalHeaderKey(key)] = values[0]
		}
	}
	return out
}

func jsonValue(t *testing.T, raw string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

// assertRefusedLikePOST checks a turn's error event carries the POST verdict:
// same status, same error object, same diagnostic headers.
func assertRefusedLikePOST(t *testing.T, reply []byte, postStatus int, postBody []byte, postHeader http.Header) {
	t.Helper()
	if typ := gjson.GetBytes(reply, "type").String(); typ != "error" {
		t.Fatalf("turn reply = %s, want an error event", reply)
	}
	if status := int(gjson.GetBytes(reply, "status").Int()); status != postStatus {
		t.Fatalf("event status = %d, want the POST status %d (event %s)", status, postStatus, reply)
	}
	if got, want := jsonValue(t, gjson.GetBytes(reply, "error").Raw), jsonValue(t, gjson.GetBytes(postBody, "error").Raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("event error = %v, want the POST error %v", got, want)
	}
	eventHeaders := http.Header{}
	gjson.GetBytes(reply, "headers").ForEach(func(key, value gjson.Result) bool {
		eventHeaders.Set(key.String(), value.String())
		return true
	})
	if got, want := quotaHeaderSet(eventHeaders), quotaHeaderSet(postHeader); !reflect.DeepEqual(got, want) {
		t.Fatalf("event headers = %v, want the POST headers %v", got, want)
	}
}

func TestResponsesWebsocketHandshakeRefusedLikePOSTWhenBudgetExhausted(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]string
		exhaust  func(*quotaWSUsage)
		status   int
		code     string
	}{
		{"daily requests", map[string]string{"daily-limit": "5"}, func(u *quotaWSUsage) { u.today = 5 }, http.StatusTooManyRequests, "daily_limit_exceeded"},
		{"total quota", map[string]string{"total-quota": "5"}, func(u *quotaWSUsage) { u.total = 5 }, http.StatusTooManyRequests, "total_quota_exceeded"},
		{"lifetime spending", map[string]string{"spending-limit": "10"}, func(u *quotaWSUsage) { u.totalCost = 10 }, http.StatusTooManyRequests, "spending_limit_exceeded"},
		{"daily spending", map[string]string{"daily-spending-limit": "10"}, func(u *quotaWSUsage) { u.todayCost = 10 }, http.StatusTooManyRequests, "daily_spending_limit_exceeded"},
		{"key week period", map[string]string{"tenant-id": "tenant-a", "api-key-id": "key-a", "key-period-spending-limit-week": "10"}, func(u *quotaWSUsage) { u.keyPeriod.Week = 10 }, http.StatusTooManyRequests, "period_spending_limit_exceeded"},
		{"usage unavailable", map[string]string{"daily-limit": "5"}, func(u *quotaWSUsage) { u.fail = true }, http.StatusServiceUnavailable, "quota_usage_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newQuotaWSHarness(t, tc.metadata)
			h.usage.set(tc.exhaust)

			postStatus, postBody, postHeader := h.post()
			if postStatus != tc.status || gjson.GetBytes(postBody, "error.code").String() != tc.code {
				t.Fatalf("control: POST = %d %s, want %d %s", postStatus, postBody, tc.status, tc.code)
			}

			conn, resp, err := h.dial()
			if err == nil {
				_ = conn.Close()
				t.Fatalf("handshake upgraded although the POST was refused with %d", postStatus)
			}
			if resp == nil {
				t.Fatalf("dial failed without a response: %v", err)
			}
			handshakeBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != postStatus {
				t.Fatalf("handshake status = %d, want the POST status %d", resp.StatusCode, postStatus)
			}
			if string(handshakeBody) != string(postBody) {
				t.Fatalf("handshake body = %s, want the POST body %s", handshakeBody, postBody)
			}
			if got, want := quotaHeaderSet(resp.Header), quotaHeaderSet(postHeader); !reflect.DeepEqual(got, want) {
				t.Fatalf("handshake headers = %v, want the POST headers %v", got, want)
			}
			if calls := h.exec.calls.Load(); calls != 0 {
				t.Fatalf("executor calls = %d, want 0", calls)
			}
		})
	}
}

func TestResponsesWebsocketTurnRefusedOnceBudgetRunsOut(t *testing.T) {
	h := newQuotaWSHarness(t, map[string]string{"daily-limit": "5"})
	conn := h.mustDial()
	h.mustComplete(conn)

	h.usage.set(func(u *quotaWSUsage) { u.today = 5 })
	postStatus, postBody, postHeader := h.post()
	if postStatus != http.StatusTooManyRequests {
		t.Fatalf("control: POST = %d %s, want 429", postStatus, postBody)
	}
	assertRefusedLikePOST(t, h.turn(conn, quotaWSModel), postStatus, postBody, postHeader)
	if calls := h.exec.calls.Load(); calls != 1 {
		t.Fatalf("executor calls = %d, want 1 (the refused turn must not reach it)", calls)
	}

	// Only that turn failed: the connection stays usable once there is budget.
	h.usage.set(func(u *quotaWSUsage) { u.today = 0 })
	h.mustComplete(conn)
	if calls := h.exec.calls.Load(); calls != 2 {
		t.Fatalf("executor calls = %d, want 2", calls)
	}
}

func TestResponsesWebsocketTurnsCountTowardRPMAndTPM(t *testing.T) {
	t.Run("rpm", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"rpm-limit": "2"})
		conn := h.mustDial()
		// The handshake is not a request, so two turns fit in a limit of two.
		h.mustComplete(conn)
		h.mustComplete(conn)
		reply := h.turn(conn, quotaWSModel)
		if gjson.GetBytes(reply, "status").Int() != http.StatusTooManyRequests || gjson.GetBytes(reply, "error.code").String() != "rpm_limit_exceeded" {
			t.Fatalf("third turn reply = %s, want 429 rpm_limit_exceeded", reply)
		}
		if calls := h.exec.calls.Load(); calls != 2 {
			t.Fatalf("executor calls = %d, want 2", calls)
		}
	})
	t.Run("tpm", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"tpm-limit": "100"})
		conn := h.mustDial()
		h.mustComplete(conn)
		middleware.RecordTokenUsage(h.apiKey, 100)
		reply := h.turn(conn, quotaWSModel)
		if gjson.GetBytes(reply, "status").Int() != http.StatusTooManyRequests || gjson.GetBytes(reply, "error.code").String() != "tpm_limit_exceeded" {
			t.Fatalf("turn reply = %s, want 429 tpm_limit_exceeded", reply)
		}
		if calls := h.exec.calls.Load(); calls != 1 {
			t.Fatalf("executor calls = %d, want 1", calls)
		}
	})
}

func TestResponsesWebsocketTurnHoldsConcurrencySlotOnlyWhileRunning(t *testing.T) {
	h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1"})
	conn := h.mustDial()
	// An open connection with no turn running holds nothing.
	if status, code := h.probe(); status != http.StatusNoContent {
		t.Fatalf("probe on an idle connection = %d (%s), want 204", status, code)
	}

	release := h.startHeldTurn(conn)
	// A turn on a second connection competes for the same slot.
	other := h.mustDial()
	reply := h.turn(other, quotaWSModel)
	if gjson.GetBytes(reply, "status").Int() != http.StatusTooManyRequests || gjson.GetBytes(reply, "error.code").String() != "concurrency_limit_exceeded" {
		t.Fatalf("competing turn reply = %s, want 429 concurrency_limit_exceeded", reply)
	}

	release()
	if reply := h.read(conn); gjson.GetBytes(reply, "type").String() != "response.done" {
		t.Fatalf("held turn reply = %s, want response.done", reply)
	}
	h.waitSlotFree()
	if calls := h.exec.calls.Load(); calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
}

func TestResponsesWebsocketTurnReleasesConcurrencySlotOnEveryExit(t *testing.T) {
	t.Run("turn completes", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1"})
		conn := h.mustDial()
		release := h.startHeldTurn(conn)
		release()
		if reply := h.read(conn); gjson.GetBytes(reply, "type").String() != "response.done" {
			t.Fatalf("reply = %s, want response.done", reply)
		}
		h.waitSlotFree()
	})

	t.Run("executor fails before streaming", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1"})
		h.exec.configure(func(e *quotaWSExecutor) { e.failStart = true })
		conn := h.mustDial()
		release := h.startHeldTurn(conn)
		release()
		if reply := h.read(conn); gjson.GetBytes(reply, "type").String() != "error" {
			t.Fatalf("reply = %s, want an error event", reply)
		}
		h.waitSlotFree()
	})

	t.Run("stream fails mid-turn", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1"})
		h.exec.configure(func(e *quotaWSExecutor) { e.failStream = true })
		conn := h.mustDial()
		release := h.startHeldTurn(conn)
		release()
		if reply := h.read(conn); gjson.GetBytes(reply, "type").String() != "error" {
			t.Fatalf("reply = %s, want an error event", reply)
		}
		h.waitSlotFree()
	})

	t.Run("client disconnects mid-turn", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1"})
		conn := h.mustDial()
		release := h.startHeldTurn(conn)
		_ = conn.Close()
		release()
		h.waitSlotFree()
	})

	t.Run("model refused after admission", func(t *testing.T) {
		h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1", "allowed-models": quotaWSModel})
		conn := h.mustDial()
		release := h.startHeldTurn(conn)
		release()
		if reply := h.read(conn); gjson.GetBytes(reply, "type").String() != "response.done" {
			t.Fatalf("reply = %s, want response.done", reply)
		}
		reply := h.turn(conn, quotaWSOtherModel)
		if gjson.GetBytes(reply, "status").Int() != http.StatusForbidden {
			t.Fatalf("reply = %s, want the 403 model refusal", reply)
		}
		h.waitSlotFree()
		if calls := h.exec.calls.Load(); calls != 1 {
			t.Fatalf("executor calls = %d, want 1", calls)
		}
	})

	t.Run("turn refused by a later check", func(t *testing.T) {
		// The slot is taken first, then RPM refuses the turn; the slot must come
		// back, so the probe is refused by RPM rather than by concurrency.
		h := newQuotaWSHarness(t, map[string]string{"concurrency-limit": "1", "rpm-limit": "2"})
		conn := h.mustDial()
		release := h.startHeldTurn(conn) // turn 1 and the probe: 2 requests
		release()
		if reply := h.read(conn); gjson.GetBytes(reply, "type").String() != "response.done" {
			t.Fatalf("reply = %s, want response.done", reply)
		}
		reply := h.turn(conn, quotaWSModel) // request 3
		if gjson.GetBytes(reply, "error.code").String() != "rpm_limit_exceeded" {
			t.Fatalf("reply = %s, want rpm_limit_exceeded", reply)
		}
		if status, code := h.probe(); status != http.StatusTooManyRequests || code != "rpm_limit_exceeded" {
			t.Fatalf("probe = %d (%s), want 429 rpm_limit_exceeded: the refused turn kept its slot", status, code)
		}
	})
}

// Keys without limits keep the old path: nothing is looked up or refused.
func TestResponsesWebsocketWithoutLimitsIsUnchanged(t *testing.T) {
	for name, metadata := range map[string]map[string]string{
		"no metadata":        nil,
		"metadata no limits": {"tenant-id": "tenant-a", "api-key-id": "key-a"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newQuotaWSHarness(t, metadata)
			h.usage.set(func(u *quotaWSUsage) { u.fail = true })
			conn := h.mustDial()
			for i := 0; i < 3; i++ {
				h.mustComplete(conn)
			}
			if calls := h.exec.calls.Load(); calls != 3 {
				t.Fatalf("executor calls = %d, want 3", calls)
			}
		})
	}
}
