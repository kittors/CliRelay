package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// enableBusyWait lets every request of the manager's default tenant wait up to
// the given number of seconds for a cooling account. Waiting is off unless a
// group asks for it, so the tests about waiting ask.
func enableBusyWait(mgr *Manager, seconds int) {
	mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
		ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "default", BusyWaitMaxSeconds: new(seconds)}},
	}})
}

// fixedSpread makes every waiter resume the same amount after the soonest
// recovery, so a test can state the exact time it expects.
func fixedSpread(d time.Duration) func(time.Duration) time.Duration {
	return func(window time.Duration) time.Duration { return min(d, window) }
}

// retryAfterError is an upstream failure that names how long to back off.
type retryAfterError struct {
	status     int
	message    string
	retryAfter time.Duration
}

func (e *retryAfterError) Error() string   { return e.message }
func (e *retryAfterError) StatusCode() int { return e.status }
func (e *retryAfterError) RetryAfter() *time.Duration {
	d := e.retryAfter
	return &d
}

// scriptedExecutor answers each call with the next scripted error, or "ok"
// once the script runs out, and records which account each call went to.
type scriptedExecutor struct {
	mu     sync.Mutex
	script []error
	calls  []string
}

func (e *scriptedExecutor) Identifier() string { return "flaky" }

func (e *scriptedExecutor) next(auth *Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, auth.ID)
	if len(e.calls) <= len(e.script) {
		return e.script[len(e.calls)-1]
	}
	return nil
}

func (e *scriptedExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.next(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *scriptedExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.next(auth); err != nil {
		return nil, err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *scriptedExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *scriptedExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *scriptedExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *scriptedExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// newBusyWaitManager is a manager whose requests may busy-wait for up to a
// minute, the usual setting of a group that opts in.
func newBusyWaitManager(t *testing.T, exec ProviderExecutor, ids ...string) *Manager {
	t.Helper()
	mgr := newPlainManager(t, exec, ids...)
	enableBusyWait(mgr, 60)
	return mgr
}

// newPlainManager has no routing configuration at all: whatever it does is what
// a deployment does that never touched the stream guards.
func newPlainManager(t *testing.T, exec ProviderExecutor, ids ...string) *Manager {
	t.Helper()
	mgr := NewManager(nil, nil, nil)
	mgr.RegisterExecutor(exec)
	for _, id := range ids {
		if _, err := mgr.Register(context.Background(), &Auth{ID: id, Provider: "flaky", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	return mgr
}

// park puts an account into the cooldown the failure leaves behind.
func park(mgr *Manager, id string, failure *Error, retryAfter time.Duration) {
	result := Result{AuthID: id, Provider: "flaky", Model: "test-model", Error: failure}
	if retryAfter > 0 {
		result.RetryAfter = &retryAfter
	}
	mgr.MarkResult(context.Background(), result)
}

var (
	upstreamDown = &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"}
	throttled    = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit: too many requests per minute"}
	quotaGone    = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota_exhausted"}
	revoked      = &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid api key"}
)

func execute(mgr *Manager, ctx context.Context) error {
	_, err := mgr.Execute(ctx, []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	return err
}

// Every account is cooling after recoverable failures (a 5xx and a short 429):
// the request waits for the soonest one and is served by it.
func TestBusyWaitServesTheRequestOnceTheSoonestAccountRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-a", "bw-b")
		park(mgr, "bw-a", upstreamDown, 15*time.Second)
		park(mgr, "bw-b", throttled, 40*time.Second)

		start := time.Now()
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		waited := time.Since(start)
		if waited < 15*time.Second || waited >= 15*time.Second+busyWaitJitter {
			t.Fatalf("waited %s, want the 15s until bw-a recovers plus less than the %s wake-up window", waited, busyWaitJitter)
		}
		if calls := exec.Calls(); len(calls) != 1 || calls[0] != "bw-a" {
			t.Fatalf("calls = %v, want one call on the account that recovered first", calls)
		}
	})
}

// Past the cap nothing is waited for, and the error carries the Retry-After
// the scheduler always derived from the cooldowns.
func TestBusyWaitGivesUpBeyondTheCapWithRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-far")
		park(mgr, "bw-far", upstreamDown, 2*time.Minute)

		start := time.Now()
		err := execute(mgr, context.Background())
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s for a recovery beyond the 60s cap", waited)
		}
		if statusCodeFromError(err) != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want the 503 the empty pool always gave", err)
		}
		if got := headersFromError(err).Get("Retry-After"); got != strconv.Itoa(120) {
			t.Fatalf("Retry-After = %q, want 120", got)
		}
		if calls := exec.Calls(); len(calls) != 0 {
			t.Fatalf("calls = %v, want none", calls)
		}
	})
}

// A spent quota, a revoked key: none recovers in minutes, so nothing waits.
func TestBusyWaitIgnoresQuotaAndAuthFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-quota", "bw-revoked")
		park(mgr, "bw-quota", quotaGone, 20*time.Second)
		park(mgr, "bw-revoked", revoked, 0)

		start := time.Now()
		if err := execute(mgr, context.Background()); err == nil {
			t.Fatal("expected the request to fail")
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s for accounts that cannot recover in time", waited)
		}
	})
}

func TestBusyWaitStopsAsSoonAsTheClientLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-leave")
		park(mgr, "bw-leave", upstreamDown, 30*time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Second, cancel)
		start := time.Now()
		err := execute(mgr, ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("returned after %s, want the moment the client left", waited)
		}
		if mgr.busyWaiters.Load() != 0 {
			t.Fatalf("waiter slot leaked: %d", mgr.busyWaiters.Load())
		}
	})
}

// A caller with a deadline (a panel probe, an internal check) keeps the
// upstream's error instead of a timeout when the recovery comes too late.
func TestBusyWaitNeverOutlastsTheRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-deadline")
		park(mgr, "bw-deadline", upstreamDown, 15*time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		err := execute(mgr, ctx)
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s for a recovery after the deadline", waited)
		}
		if statusCodeFromError(err) != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want the 503 rather than a timeout", err)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-in-time")
		park(mgr, "bw-in-time", upstreamDown, 15*time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		if err := execute(mgr, ctx); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if waited := time.Since(start); waited < 15*time.Second || waited >= 15*time.Second+busyWaitJitter {
			t.Fatalf("waited %s, want the 15s recovery, which the deadline leaves room for", waited)
		}
	})
}

// The cap bounds the whole request, retries included: after waiting 40s and
// failing again, 20s are left, too few for the next 30s cooldown.
func TestBusyWaitBudgetCoversTheWholeRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{script: []error{&retryAfterError{status: http.StatusBadGateway, message: "bad gateway", retryAfter: 30 * time.Second}}}
		mgr := newBusyWaitManager(t, exec, "bw-budget")
		mgr.busyWaitSpread = fixedSpread(time.Second)
		park(mgr, "bw-budget", upstreamDown, 40*time.Second)

		ctx := WithRetryWaitBudget(context.Background())
		start := time.Now()
		err := execute(mgr, ctx)
		if statusCodeFromError(err) != http.StatusBadGateway {
			t.Fatalf("err = %v, want the second failure", err)
		}
		if waited := time.Since(start); waited != 41*time.Second {
			t.Fatalf("request took %s, want one wait of 40s plus the 1s wake-up offset and no second wait", waited)
		}
		if remaining := retryWaitBudgetFor(ctx).remaining(time.Minute); remaining != 19*time.Second {
			t.Fatalf("budget left = %s, want the first wait (41s) charged", remaining)
		}
	})
}

func TestBusyWaitCanBeTurnedOffAndIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-off")
		enableBusyWait(mgr, 0)
		park(mgr, "bw-off", upstreamDown, 15*time.Second)
		start := time.Now()
		if err := execute(mgr, context.Background()); err == nil || time.Since(start) != 0 {
			t.Fatalf("with busy-wait-max-seconds 0: err = %v after %s, want an immediate failure", err, time.Since(start))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-full")
		park(mgr, "bw-full", upstreamDown, 15*time.Second)
		mgr.SetBusyWaitConfig(2)
		mgr.busyWaiters.Store(2)
		start := time.Now()
		if err := execute(mgr, context.Background()); err == nil || time.Since(start) != 0 {
			t.Fatalf("with every waiter slot taken: err = %v after %s, want an immediate failure", err, time.Since(start))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-count")
		park(mgr, "bw-count", upstreamDown, 15*time.Second)
		start := time.Now()
		_, err := mgr.ExecuteCount(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		if err == nil || time.Since(start) != 0 {
			t.Fatalf("token count: err = %v after %s, want an immediate failure", err, time.Since(start))
		}
	})
}

// An account that is not cooling could serve, so the pool is not busy and
// the failure is reported at once, even with others cooling.
func TestBusyWaitOnlyWhenEveryUsableAccountIsCooling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{script: []error{&Error{Code: "bad_gateway", Message: "no cooldown for this one"}}}
		mgr := newBusyWaitManager(t, exec, "bw-free", "bw-cooling")
		park(mgr, "bw-cooling", upstreamDown, 15*time.Second)
		start := time.Now()
		if err := execute(mgr, context.Background()); err == nil {
			t.Fatal("expected the failure to surface")
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s although an account was free", waited)
		}
	})
}
