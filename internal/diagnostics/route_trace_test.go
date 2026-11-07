package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// The in-flight set is global, so each test lists the requests of its own
// tenant only.

func TestMain(m *testing.M) {
	// Once, before any test runs: gin's mode is a global the concurrent tests
	// would race on.
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

func newGatewayRequest(t *testing.T, requestID string) (*gin.Context, *RequestDiagnostic) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, EnsureGin(c, requestID)
}

func authenticate(c *gin.Context, tenantID string) {
	SetAuth(c, "openai", "sk-live-0123456789abcdef", "key-7", "ci key")
	SetTenant(c, tenantID)
}

func tenantOnly(tenantID string) func(string) bool {
	return func(id string) bool { return id == tenantID }
}

func listed(tenantID string) []InFlightRequest {
	out, _ := InFlight(tenantOnly(tenantID), time.Now())
	return out
}

func TestEnsureGinPutsTheRouteTraceInTheRequestContext(t *testing.T) {
	c, d := newGatewayRequest(t, "req-ctx")
	rec := routetrace.FromContext(c.Request.Context())
	if rec == nil || rec != d.trace {
		t.Fatal("the scheduler must find the diagnostic's route trace in the request context")
	}
	if FromContext(c.Request.Context()) != d {
		t.Fatal("the diagnostic must stay in the request context")
	}
	if again := EnsureGin(c, "req-ctx"); again != d || routetrace.FromContext(c.Request.Context()) != rec {
		t.Fatal("a second EnsureGin must keep the diagnostic and its trace")
	}
}

func TestSnapshotCarriesTheRouteTrace(t *testing.T) {
	_, d := newGatewayRequest(t, "req-snap")
	if d.Snapshot().RouteTrace != nil {
		t.Fatal("a request nothing was scheduled for has no trace")
	}
	d.trace.BeginAttempt(routetrace.AttemptInfo{AuthID: "acct-1"}).Finish(routetrace.Outcome{Outcome: routetrace.OutcomeSuccess})
	snapshot := d.Snapshot()
	if snapshot.RouteTrace == nil || len(snapshot.RouteTrace.Steps) != 1 {
		t.Fatalf("route trace = %+v", snapshot.RouteTrace)
	}
	raw, err := snapshot.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"route_trace"`) {
		t.Fatalf("snapshot JSON lacks the route trace: %s", raw)
	}
	if (Snapshot{RouteTrace: snapshot.RouteTrace}).IsZero() {
		t.Fatal("a snapshot with only a trace is not empty")
	}
}

func TestInFlightListsAuthenticatedRequestsOfTheTenant(t *testing.T) {
	const tenant = "tenant-inflight-list"
	c, d := newGatewayRequest(t, "req-list")
	end := BeginInFlight(d)
	defer end()

	everyone, _ := InFlight(nil, time.Now())
	for _, entry := range everyone {
		if entry.RequestID == "req-list" {
			t.Fatalf("an unauthenticated request must not be listed, for any tenant: %+v", entry)
		}
	}
	authenticate(c, tenant)
	if got := listed("tenant-someone-else"); len(got) != 0 {
		t.Fatalf("another tenant's request is listed: %+v", got)
	}
	got := listed(tenant)
	if len(got) != 1 {
		t.Fatalf("listed = %+v, want the request", got)
	}
	entry := got[0]
	if entry.RequestID != "req-list" || entry.Method != http.MethodPost || entry.Path != "/v1/chat/completions" || entry.Stage != routetrace.StagePreparing || entry.Account != nil {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.APIKeyID != "key-7" || entry.APIKeyName != "ci key" {
		t.Fatalf("entry key = %q %q", entry.APIKeyID, entry.APIKeyName)
	}
	raw, _ := json.Marshal(entry)
	if strings.Contains(string(raw), "sk-live") || strings.Contains(string(raw), "abcdef") {
		t.Fatalf("the in-flight view must not show the key: %s", raw)
	}

	d.trace.SetRequest("gpt-5", true, false)
	d.trace.BeginAttempt(routetrace.AttemptInfo{Group: "default", AuthID: "acct-1", AuthLabel: "Primary", Provider: "codex", Model: "gpt-5-codex"})
	entry = listed(tenant)[0]
	if entry.Stage != routetrace.StageUpstream || entry.Model != "gpt-5" || !entry.Stream || entry.Attempt != 1 {
		t.Fatalf("entry = %+v", entry)
	}
	if a := entry.Account; a == nil || a.AuthID != "acct-1" || a.Label != "Primary" || a.Provider != "codex" || a.Group != "default" || a.Model != "gpt-5-codex" {
		t.Fatalf("account = %+v", entry.Account)
	}

	end()
	end()
	if got := listed(tenant); len(got) != 0 {
		t.Fatalf("an ended request is still listed: %+v", got)
	}
}

func TestBeginInFlightTwiceKeepsOneOwner(t *testing.T) {
	const tenant = "tenant-inflight-owner"
	c, d := newGatewayRequest(t, "req-owner")
	authenticate(c, tenant)
	end := BeginInFlight(d)
	nested := BeginInFlight(d)
	nested()
	if len(listed(tenant)) != 1 {
		t.Fatal("only the first owner ends the request")
	}
	end()
	if len(listed(tenant)) != 0 {
		t.Fatal("the request must leave once its owner ends it")
	}
}

// The middleware defers the end, so a request leaves the list however it
// ends; ending it also publishes the usage records its scheduler still holds.
func TestInFlightEndsHoweverTheRequestEnds(t *testing.T) {
	const tenant = "tenant-inflight-end"
	serve := func(handler func(ctx context.Context)) {
		c, d := newGatewayRequest(t, "req-end")
		authenticate(c, tenant)
		ctx := c.Request.Context()
		func() {
			defer func() { _ = recover() }()
			end := BeginInFlight(d)
			defer end()
			handler(ctx)
		}()
	}
	var held []string
	var mu sync.Mutex
	hold := func(ctx context.Context, name string) {
		attempt := routetrace.FromContext(ctx).BeginAttempt(routetrace.AttemptInfo{AuthID: "acct"})
		routetrace.DeferPublish(attempt.Context(ctx), func() {
			mu.Lock()
			held = append(held, name)
			mu.Unlock()
		})
	}
	serve(func(ctx context.Context) {
		hold(ctx, "panic")
		panic("handler failed")
	})
	serve(func(ctx context.Context) {
		hold(ctx, "canceled")
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		<-ctx.Done()
	})
	serve(func(ctx context.Context) {
		hold(ctx, "timeout")
		ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		<-ctx.Done()
	})
	if got := listed(tenant); len(got) != 0 {
		t.Fatalf("ended requests are still listed: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(held, ",") != "panic,canceled,timeout" {
		t.Fatalf("records published = %v, want every held record out when its request ended", held)
	}
}

// A WebSocket connection between two of its requests is not in flight; its
// next request starts over.
func TestInFlightSkipsAnIdleWebSocket(t *testing.T) {
	const tenant = "tenant-inflight-ws"
	c, d := newGatewayRequest(t, "req-ws")
	authenticate(c, tenant)
	end := BeginInFlight(d)
	defer end()

	d.trace.SetRequest("gpt-5", true, false)
	d.trace.BeginAttempt(routetrace.AttemptInfo{AuthID: "acct-1"}).Finish(routetrace.Outcome{Outcome: routetrace.OutcomeSuccess})
	d.trace.Idle()
	if got := listed(tenant); len(got) != 0 {
		t.Fatalf("an idle connection is listed: %+v", got)
	}
	time.Sleep(5 * time.Millisecond)
	d.trace.Restart()
	got := listed(tenant)
	if len(got) != 1 || got[0].Attempt != 0 || got[0].Account != nil || got[0].Model != "" {
		t.Fatalf("listed = %+v, want the connection's new request from its start", got)
	}
	if !got[0].StartedAt.After(d.startedAt.Truncate(time.Millisecond)) {
		t.Fatalf("started at %v, want the new request's start, after the connection's %v", got[0].StartedAt, d.startedAt)
	}
}

func TestInFlightListsTheOldestFirstAndBoundsTheList(t *testing.T) {
	const tenant = "tenant-inflight-bound"
	total := MaxInFlightListed + 3
	for i := 0; i < total; i++ {
		c, d := newGatewayRequest(t, fmt.Sprintf("req-%04d", i))
		authenticate(c, tenant)
		defer BeginInFlight(d)()
	}
	got, count := InFlight(tenantOnly(tenant), time.Now())
	if count != total || len(got) != MaxInFlightListed {
		t.Fatalf("listed %d of %d, want %d of %d", len(got), count, MaxInFlightListed, total)
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.StartedAt.Before(prev.StartedAt) || (cur.StartedAt.Equal(prev.StartedAt) && cur.RequestID < prev.RequestID) {
			t.Fatalf("entry %d (%s at %v) comes after %s at %v", i, cur.RequestID, cur.StartedAt, prev.RequestID, prev.StartedAt)
		}
	}
	if got[0].RequestID != "req-0000" {
		t.Fatalf("the oldest request is %s, want req-0000", got[0].RequestID)
	}
}

// Requests enter and leave the set from every handler goroutine while the
// management API lists it; run with -race.
func TestInFlightIsSafeForConcurrentUse(t *testing.T) {
	const tenant = "tenant-inflight-race"
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = InFlight(tenantOnly(tenant), time.Now())
			}
		}
	}()
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c, d := newGatewayRequest(t, fmt.Sprintf("req-%d-%d", g, i))
				end := BeginInFlight(d)
				authenticate(c, tenant)
				d.trace.BeginAttempt(routetrace.AttemptInfo{AuthID: "acct"})
				end()
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	if got := listed(tenant); len(got) != 0 {
		t.Fatalf("%d requests left in the set", len(got))
	}
}

// The per-request cost of the trace and the in-flight set: a diagnostic with
// its recorder, entering the set and leaving it.
func BenchmarkInFlightBeginEnd(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			d := newRequestDiagnostic()
			BeginInFlight(d)()
		}
	})
}
