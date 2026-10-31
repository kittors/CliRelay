package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// Requests admitted at the same moment used to be checked against the same
// recorded usage, so a burst near a limit was admitted whole and the key ended
// the burst's size over. These tests keep a request in flight inside the
// handler and look at what the next request is told.

// burst serves POST /v1/responses through the quota and model policy
// middlewares, authenticating each request as the key its X-Key header names.
type burst struct {
	t      *testing.T
	keys   map[string]map[string]string
	router *gin.Engine
	held   sync.Map // X-Hold token → *heldRequest
	n      atomic.Int64
}

// heldRequest stays in the handler until finish.
type heldRequest struct {
	requestID string
	publish   bool
	entered   chan string
	release   chan struct{}
	done      chan *httptest.ResponseRecorder
}

func newBurst(t *testing.T, keys map[string]map[string]string) *burst {
	t.Helper()
	gin.SetMode(gin.TestMode)
	b := &burst{t: t, keys: keys, router: gin.New()}
	b.router.Use(func(c *gin.Context) {
		key := c.GetHeader("X-Key")
		c.Set("apiKey", key)
		c.Set("accessMetadata", b.keys[key])
		c.Next()
	})
	b.router.Use(QuotaMiddleware())
	b.router.Use(ModelPolicyMiddleware(func(_ *gin.Context, model string) string { return model }))
	b.router.POST("/v1/responses", func(c *gin.Context) {
		if value, ok := b.held.Load(c.GetHeader("X-Hold")); ok {
			h := value.(*heldRequest)
			// The id an executor stamps on the request's usage records.
			requestID := logging.GetRequestID(c.Request.Context())
			if h.publish {
				quotaLedger.Published(requestID)
			}
			h.entered <- requestID
			<-h.release
		}
		c.Status(http.StatusNoContent)
	})
	return b
}

func (b *burst) serve(key, model, hold string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
	req.Header.Set("X-Key", key)
	if hold != "" {
		req.Header.Set("X-Hold", hold)
	}
	rec := httptest.NewRecorder()
	b.router.ServeHTTP(rec, req)
	return rec
}

// send serves one request to completion.
func (b *burst) send(key, model string) *httptest.ResponseRecorder { return b.serve(key, model, "") }

// hold starts a request and returns once it is in the handler. A request that
// publishes a usage record keeps its reservation after it ends, until the
// record is settled.
func (b *burst) hold(key, model string, publish bool) *heldRequest {
	b.t.Helper()
	token := fmt.Sprintf("hold-%d", b.n.Add(1))
	h := &heldRequest{publish: publish, entered: make(chan string, 1), release: make(chan struct{}), done: make(chan *httptest.ResponseRecorder, 1)}
	b.held.Store(token, h)
	go func() { h.done <- b.serve(key, model, token) }()
	select {
	case h.requestID = <-h.entered:
	case rec := <-h.done:
		b.t.Fatalf("request meant to stay in flight was answered %d: %s", rec.Code, rec.Body)
	}
	if h.requestID == "" {
		b.t.Fatal("the request reached the handler without a request id")
	}
	return h
}

func (h *heldRequest) finish(t *testing.T) {
	t.Helper()
	close(h.release)
	if rec := <-h.done; rec.Code != http.StatusNoContent {
		t.Fatalf("held request answered %d: %s", rec.Code, rec.Body)
	}
}

// teach makes the billed requests of owner cost cost on average.
func teach(owner inflight.Owner, cost float64, tokens int64) {
	r := quotaLedger.Reserve("teach", owner)
	quotaLedger.Published("teach")
	quotaLedger.Settle("teach", true, cost, tokens)
	r.Finish()
}

func assertRefused(t *testing.T, rec *httptest.ResponseRecorder, current, message string) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), message) {
		t.Fatalf("body = %s\nwant it to contain %q", rec.Body, message)
	}
	if got := rec.Header().Get("X-CliRelay-Quota-Current"); got != current {
		t.Fatalf("X-CliRelay-Quota-Current = %q, want %q", got, current)
	}
}

func assertAdmitted(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want the request admitted: %s", rec.Code, rec.Body)
	}
}

func TestABurstNearASpendingLimitIsAdmittedOneRequestAtATime(t *testing.T) {
	resetQuotaMiddlewareState(t)
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 9.5, nil }
	b := newBurst(t, map[string]map[string]string{
		contractKey: {"tenant-id": "tenant-1", "api-key-id": "key-1", "spending-limit": "10"},
	})
	teach(inflight.Owner{Key: inflight.KeyName("key-1")}, 1, 1000)

	first := b.hold(contractKey, "gpt-5", false)
	assertRefused(t, b.send(contractKey, "gpt-5"), "10.5",
		"Lifetime spending limit exceeded: $10.50 of $10.00 used. This includes about $1.00 reserved by 1 request still in flight. Raise the spending limit to continue.")
	first.finish(t)
	// It published no usage, so nothing of it is left once it ended.
	assertAdmitted(t, b.send(contractKey, "gpt-5"))
}

func TestARequestRunningAloneSeesOnlyRecordedUsage(t *testing.T) {
	resetQuotaMiddlewareState(t)
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 9.5, nil }
	countTodayByKeyFunc = func(string) (int64, error) { return 4, nil }
	b := newBurst(t, map[string]map[string]string{
		contractKey: {"tenant-id": "tenant-1", "api-key-id": "key-1", "spending-limit": "10", "daily-limit": "5"},
	})
	teach(inflight.Owner{Key: inflight.KeyName("key-1")}, 5, 1000)
	// A request never counts its own reservation: one dollar short of the
	// limit and one request short of the daily cap, it is admitted as before.
	assertAdmitted(t, b.send(contractKey, "gpt-5"))
}

func TestAnEstimateOfNothingIsNotMentioned(t *testing.T) {
	resetQuotaMiddlewareState(t)
	spent := 9.5
	queryTotalCostByKeyFunc = func(string) (float64, error) { return spent, nil }
	b := newBurst(t, map[string]map[string]string{
		contractKey: {"tenant-id": "tenant-1", "api-key-id": "key-1", "spending-limit": "10"},
	})
	// No billed history yet: the request in flight reserves no money, so a
	// spending refusal has nothing in flight to point at.
	first := b.hold(contractKey, "gpt-5", false)
	spent = 10
	assertRefused(t, b.send(contractKey, "gpt-5"), "10",
		`"Lifetime spending limit exceeded: $10.00 of $10.00 used. Raise the spending limit to continue."`)
	first.finish(t)
}

func TestRequestCountLimitsCountRequestsInFlight(t *testing.T) {
	t.Run("daily requests of a key", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		countTodayByKeyFunc = func(string) (int64, error) { return 4, nil }
		b := newBurst(t, map[string]map[string]string{
			contractKey: {"tenant-id": "tenant-1", "api-key-id": "key-1", "daily-limit": "5"},
		})
		first := b.hold(contractKey, "gpt-5", false)
		assertRefused(t, b.send(contractKey, "gpt-5"), "5",
			"Daily request limit exceeded: 5/5 requests used today. This includes 1 request still in flight. Raise the daily request limit")
		first.finish(t)
		assertAdmitted(t, b.send(contractKey, "gpt-5"))
	})
	t.Run("lifetime requests shared by the keys of an account", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		countTotalByEndUserFunc = func(string) (int64, error) { return 8, nil }
		account := func(keyID string) map[string]string {
			return map[string]string{"tenant-id": "tenant-1", "api-key-id": keyID, "end-user-id": "user-1", "total-quota": "10"}
		}
		b := newBurst(t, map[string]map[string]string{"sk-a": account("key-a"), "sk-b": account("key-b")})
		first := b.hold("sk-a", "gpt-5", false)
		second := b.hold("sk-b", "gpt-5", false)
		assertRefused(t, b.send("sk-a", "gpt-5"), "10",
			"Total request quota exhausted: 10/10 lifetime requests used. This includes 2 requests still in flight.")
		first.finish(t)
		second.finish(t)
	})
}

func TestPeriodLimitsCountTheirOwnScope(t *testing.T) {
	resetQuotaMiddlewareState(t)
	queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{Day: 4.5}, nil
	}
	queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{Day: 4.5}, nil
	}
	b := newBurst(t, map[string]map[string]string{
		"sk-a":     {"tenant-id": "tenant-1", "api-key-id": "key-a", "end-user-id": "user-1", "account-period-spending-limit-day": "5"},
		"sk-b":     {"tenant-id": "tenant-1", "api-key-id": "key-b", "end-user-id": "user-1", "account-period-spending-limit-day": "5"},
		"sk-b-key": {"tenant-id": "tenant-1", "api-key-id": "key-b", "end-user-id": "user-1", "key-period-spending-limit-day": "5"},
	})
	teach(inflight.Owner{Key: inflight.KeyName("key-a"), Account: inflight.AccountName("user-1")}, 1, 100)

	first := b.hold("sk-a", "gpt-5", false)
	// Key b has no history of its own: it reserves what the account's
	// requests cost, and sees key a's request through the account.
	assertRefused(t, b.send("sk-b", "gpt-5"), "5.5",
		"Account day spending limit exceeded: $5.50 of $5.00 used. This includes about $1.00 reserved by 1 request still in flight.")
	// The key's own period limit is not drawn on by another key's request.
	assertAdmitted(t, b.send("sk-b-key", "gpt-5"))
	first.finish(t)
}

// policyKeyMetadata is what the gateway's auth middleware sets for
// policyTestKey (see policyRouter).
var policyKeyMetadata = map[string]string{"end-user-id": "user-1", "tenant-id": "tenant-1", "api-key-id": "key-1"}

var policyKeyOwner = inflight.Owner{Key: inflight.KeyName("key-1"), Account: inflight.AccountName("user-1")}

func TestPolicyCellsCountRequestsInFlight(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		AccountLimits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 50),
	}, nil, nil, matrixWith(quota.MetricRequests, quota.PeriodWeek, 49), quota.Matrix{})
	b := newBurst(t, map[string]map[string]string{policyTestKey: policyKeyMetadata})

	first := b.hold(policyTestKey, "gpt-5", false)
	assertRefused(t, b.send(policyTestKey, "gpt-5"), "50", "50/50 requests used. This includes 1 request still in flight.")
	first.finish(t)
	assertAdmitted(t, b.send(policyTestKey, "gpt-5"))
}

func TestModelRulesCountRequestsInFlightOfTheirModels(t *testing.T) {
	// The key has no limits besides the rule: the quota middleware still
	// reserves, for the model policy to count.
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		KeyRules: []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: costWeek(5)}},
	}, nil, map[string]quota.Matrix{"claude-opus-4-7": costWeek(4.5)}, quota.Matrix{}, quota.Matrix{})
	b := newBurst(t, map[string]map[string]string{policyTestKey: policyKeyMetadata})
	teach(policyKeyOwner, 1, 100)

	// A request of another model in flight does not draw on the rule.
	other := b.hold(policyTestKey, "gpt-5", false)
	first := b.hold(policyTestKey, "claude-opus-4-7", false)
	rec := b.send(policyTestKey, "claude-opus-4-7")
	assertRefused(t, rec, "5.5", "This includes about $1.00 reserved by 1 request still in flight.")
	if !strings.Contains(rec.Body.String(), "model_quota_exceeded") {
		t.Fatalf("refused by %s, want the model rule", rec.Body)
	}
	assertAdmitted(t, b.send(policyTestKey, "gpt-5"))
	first.finish(t)
	other.finish(t)
	assertAdmitted(t, b.send(policyTestKey, "claude-opus-4-7"))
}

func TestARequestIsHeldUntilItsUsageIsCommitted(t *testing.T) {
	resetQuotaMiddlewareState(t)
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 9.5, nil }
	b := newBurst(t, map[string]map[string]string{
		contractKey: {"tenant-id": "tenant-1", "api-key-id": "key-1", "spending-limit": "10"},
	})
	teach(inflight.Owner{Key: inflight.KeyName("key-1")}, 1, 1000)

	// The upstream answered and the usage record is on its way when the
	// request ends: until it is committed the checks cannot see its cost.
	first := b.hold(contractKey, "gpt-5", true)
	first.finish(t)
	assertRefused(t, b.send(contractKey, "gpt-5"), "10.5", "This includes about $1.00 reserved by 1 request still in flight.")
	quotaLedger.Settle(first.requestID, true, 0.4, 400)
	assertAdmitted(t, b.send(contractKey, "gpt-5"))
}

// websocketGates runs a Responses WebSocket handshake through the quota and
// model policy middlewares and returns the gates published for its turns, and
// the request id its turns' usage records carry.
func websocketGates(t *testing.T, key string, metadata map[string]string) (handlers.QuotaGate, handlers.ModelPolicyGate, string) {
	t.Helper()
	var quotaGate handlers.QuotaGate
	var policyGate handlers.ModelPolicyGate
	var requestID string
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", key)
		c.Set("accessMetadata", metadata)
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.Use(ModelPolicyMiddleware(nil))
	router.GET("/v1/responses", func(c *gin.Context) {
		quotaGate, policyGate = handlers.QuotaGateFromGin(c), handlers.ModelPolicyGateFromGin(c)
		requestID = logging.GetRequestID(c.Request.Context())
		c.Status(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, websocketHandshakeRequest(nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("handshake answered %d: %s", rec.Code, rec.Body)
	}
	return quotaGate, policyGate, requestID
}

func TestWebsocketTurnsSeeRequestsInFlightOnOtherConnections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	countTodayByKeyFunc = func(string) (int64, error) { return 4, nil }
	metadata := map[string]string{"tenant-id": "tenant-1", "api-key-id": "key-1", "daily-limit": "5"}
	gateA, _, connA := websocketGates(t, contractKey, metadata)
	gateB, _, _ := websocketGates(t, contractKey, metadata)
	if connA == "" {
		t.Fatal("the handshake left its turns without a request id")
	}

	releaseA, rejection := gateA()
	if rejection != nil {
		t.Fatalf("first turn refused: %s", rejection.Body)
	}
	if _, rejection = gateB(); rejection == nil || !strings.Contains(string(rejection.Body), "5/5 requests used today. This includes 1 request still in flight.") {
		t.Fatalf("turn on the other connection = %+v, want it refused for the turn in flight", rejection)
	}
	// The turn's usage is on its way under the connection's request id when
	// the turn ends: it is held until the record is committed.
	quotaLedger.Published(connA)
	releaseA()
	if _, rejection = gateB(); rejection == nil {
		t.Fatal("turn admitted while the other connection's usage was not committed yet")
	}
	quotaLedger.Settle(connA, true, 0.1, 10)
	releaseB, rejection := gateB()
	if rejection != nil {
		t.Fatalf("turn refused after the other connection's turn was billed: %s", rejection.Body)
	}
	releaseB()
}

func TestWebsocketTurnsCountForModelRules(t *testing.T) {
	withDailyLimit := map[string]string{"daily-limit": "100"}
	for key, value := range policyKeyMetadata {
		withDailyLimit[key] = value
	}
	// Without other limits the quota gate only reserves; with them it runs the
	// checks too. Either way it hands the turn's reservation on.
	for name, metadata := range map[string]map[string]string{"rules only": policyKeyMetadata, "rules and limits": withDailyLimit} {
		t.Run(name, func(t *testing.T) { websocketTurnsCountForModelRules(t, metadata) })
	}
}

func websocketTurnsCountForModelRules(t *testing.T, metadata map[string]string) {
	gin.SetMode(gin.TestMode)
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		KeyRules: []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: costWeek(5)}},
	}, nil, map[string]quota.Matrix{"claude-opus-4-7": costWeek(4.5)}, quota.Matrix{}, quota.Matrix{})
	teach(policyKeyOwner, 1, 100)
	quotaA, policyA, _ := websocketGates(t, policyTestKey, metadata)
	quotaB, policyB, _ := websocketGates(t, policyTestKey, metadata)
	if quotaA == nil || policyA == nil {
		t.Fatal("no turn gates published")
	}

	// The frame loop runs the quota gate, then the model policy gate.
	releaseA, rejection := quotaA()
	if rejection != nil {
		t.Fatalf("quota gate refused a key without limits: %s", rejection.Body)
	}
	if _, rejection := policyA("claude-opus-4-7"); rejection != nil {
		t.Fatalf("first opus turn refused: %s", rejection.Body)
	}

	releaseB, _ := quotaB()
	if _, rejection := policyB("claude-opus-4-7"); rejection == nil || !strings.Contains(string(rejection.Body), "reserved by 1 request still in flight") {
		t.Fatalf("opus turn on the other connection = %+v, want it refused for the opus turn in flight", rejection)
	}
	releaseB()
	releaseB, _ = quotaB()
	if _, rejection := policyB("gpt-5"); rejection != nil {
		t.Fatalf("a turn of another model was refused: %s", rejection.Body)
	}
	releaseB()

	releaseA()
	releaseB, _ = quotaB()
	if _, rejection := policyB("claude-opus-4-7"); rejection != nil {
		t.Fatalf("opus turn refused after the other turn ended: %s", rejection.Body)
	}
	releaseB()
}

func TestTurnReservationsHandEachReservationOnce(t *testing.T) {
	turns := &turnReservations{}
	r := inflight.NewLedger(0).Reserve("conn", policyKeyOwner)
	turns.put(r)
	if got := turns.take(); got != r {
		t.Fatalf("take = %p, want the turn's reservation %p", got, r)
	}
	if got := turns.take(); got != nil {
		t.Fatal("a reservation was handed out twice")
	}
	var none *turnReservations
	none.put(r)
	if none.take() != nil {
		t.Fatal("no holder must hand out nothing")
	}
}

func TestInFlightNoteLeavesOutWhatDoesNotShow(t *testing.T) {
	cases := []struct {
		metric quota.Metric
		held   inflight.Amounts
		want   string
	}{
		{quota.MetricRequests, inflight.Amounts{}, ""},
		{quota.MetricRequests, inflight.Amounts{Requests: 1}, " This includes 1 request still in flight."},
		{quota.MetricRequests, inflight.Amounts{Requests: 3}, " This includes 3 requests still in flight."},
		{quota.MetricCost, inflight.Amounts{Requests: 2}, ""},
		{quota.MetricCost, inflight.Amounts{Requests: 2, Cost: 0.004}, ""},
		{quota.MetricCost, inflight.Amounts{Requests: 2, Cost: 1.25}, " This includes about $1.25 reserved by 2 requests still in flight."},
		{quota.MetricTokens, inflight.Amounts{Requests: 1}, ""},
		{quota.MetricTokens, inflight.Amounts{Requests: 1, Tokens: 1200}, " This includes about 1200 tokens reserved by 1 request still in flight."},
	}
	for _, tc := range cases {
		if got := inFlightNote(tc.metric, tc.held); got != tc.want {
			t.Errorf("inFlightNote(%s, %+v) = %q, want %q", tc.metric, tc.held, got, tc.want)
		}
	}
}
