package auth

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// egressAwareExecutor is a scriptedExecutor that names the proxy each account
// leaves through, as the real executors do.
type egressAwareExecutor struct {
	scriptedExecutor
	proxies map[string]string
}

func (e *egressAwareExecutor) EgressProxyURL(auth *Auth) string { return e.proxies[auth.ID] }

const (
	brokenProxy  = "socks5://tenant-a:expired@203.0.113.7:443"
	workingProxy = "socks5://tenant-b:valid@203.0.113.7:443"
)

// newEgressManager gives the manager a breaker of its own on a clock the test
// moves, and returns the function that moves it.
func newEgressManager(t *testing.T, exec ProviderExecutor, ids ...string) (*Manager, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 11, 11, 16, 16, 0, 0, time.UTC)
	mgr := newPlainManager(t, exec, ids...)
	mgr.egress = egressbreaker.New(egressbreaker.Config{Now: func() time.Time { return now }})
	return mgr, func(d time.Duration) { now = now.Add(d) }
}

// rejectAt makes the proxy fail one handshake, as the transports report it,
// and returns the error that request ends with.
func rejectAt(t *testing.T, mgr *Manager, proxyURL string) error {
	t.Helper()
	permit, err := mgr.egress.Allow(proxyURL)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	return permit.Done(egressbreaker.AuthRejected(errors.New("username/password authentication failed")))
}

// breakProxy opens the manager's breaker for the proxy.
func breakProxy(t *testing.T, mgr *Manager, proxyURL string) {
	t.Helper()
	for range egressbreaker.DefaultRejectionThreshold {
		_ = rejectAt(t, mgr, proxyURL)
	}
}

// wantUntouched fails unless the account looks as it did when it was
// registered: no error, no cooldown, nothing recorded against a model.
func wantUntouched(t *testing.T, mgr *Manager, id string) {
	t.Helper()
	account, ok := mgr.GetByID(id)
	if !ok {
		t.Fatalf("account %s is gone", id)
	}
	if account.Status != StatusActive || account.StatusMessage != "" || account.LastError != nil {
		t.Fatalf("account %s: status=%q message=%q lastError=%v, want it left active and clean",
			id, account.Status, account.StatusMessage, account.LastError)
	}
	if account.Unavailable || !account.NextRetryAfter.IsZero() {
		t.Fatalf("account %s was put on a cooldown until %v", id, account.NextRetryAfter)
	}
	if len(account.ModelStates) != 0 {
		t.Fatalf("account %s has model states %+v, want none", id, account.ModelStates)
	}
}

// An account whose proxy is out is skipped before its executor is asked to do
// anything; the request goes to an account whose proxy works.
func TestAccountBehindABrokenProxyIsSkippedBeforeItsExecutorRuns(t *testing.T) {
	exec := &egressAwareExecutor{proxies: map[string]string{"eg-a": brokenProxy, "eg-b": workingProxy}}
	mgr, _ := newEgressManager(t, exec, "eg-a", "eg-b")
	breakProxy(t, mgr, brokenProxy)

	for range 6 {
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	for _, id := range exec.Calls() {
		if id != "eg-b" {
			t.Fatalf("the executor was called for %s, whose proxy is out; calls = %v", id, exec.Calls())
		}
	}
	if len(exec.Calls()) != 6 {
		t.Fatalf("calls = %v, want the six requests served by the working account", exec.Calls())
	}
	wantUntouched(t, mgr, "eg-a")
}

// With every proxy out the request fails at once with the egress error, on
// all three execution paths, and no executor is called.
func TestRequestFailsFastWhenEveryProxyIsOut(t *testing.T) {
	exec := &egressAwareExecutor{proxies: map[string]string{"eg-a": brokenProxy, "eg-b": brokenProxy}}
	mgr, _ := newEgressManager(t, exec, "eg-a", "eg-b")
	breakProxy(t, mgr, brokenProxy)

	req := cliproxyexecutor.Request{Model: "test-model"}
	_, errExecute := mgr.Execute(context.Background(), []string{"flaky"}, req, cliproxyexecutor.Options{})
	_, errCount := mgr.ExecuteCount(context.Background(), []string{"flaky"}, req, cliproxyexecutor.Options{})
	_, errStream := mgr.ExecuteStream(context.Background(), []string{"flaky"}, req, cliproxyexecutor.Options{Stream: true})

	for name, err := range map[string]error{"Execute": errExecute, "ExecuteCount": errCount, "ExecuteStream": errStream} {
		refusal, ok := egressbreaker.AsError(err)
		if !ok || !refusal.Refused || refusal.Fault != egressbreaker.FaultAuthRejected {
			t.Fatalf("%s error = %v, want the breaker's refusal", name, err)
		}
		if refusal.RetryAfterSeconds() != int(egressbreaker.DefaultBaseOpen/time.Second) {
			t.Fatalf("%s: RetryAfter = %s, want the breaker's wait", name, refusal.RetryAfter)
		}
	}
	if calls := exec.Calls(); len(calls) != 0 {
		t.Fatalf("the executor was called for %v although every proxy is out", calls)
	}
	wantUntouched(t, mgr, "eg-a")
	wantUntouched(t, mgr, "eg-b")
}

// Once the breaker's wait is over the scheduler lets the account through
// again; trying the proxy is the transport's job, not the scheduler's.
func TestSchedulerLeavesTheProbeToTheExecutor(t *testing.T) {
	exec := &egressAwareExecutor{proxies: map[string]string{"eg-a": brokenProxy}}
	mgr, advance := newEgressManager(t, exec, "eg-a")
	breakProxy(t, mgr, brokenProxy)

	if err := execute(mgr, context.Background()); err == nil {
		t.Fatal("Execute succeeded through a proxy that is out")
	}
	advance(egressbreaker.DefaultBaseOpen)
	for range 3 {
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute after the wait: %v", err)
		}
	}
	if calls := exec.Calls(); len(calls) != 3 {
		t.Fatalf("calls = %v, want the executor reached once the wait was over", calls)
	}
}

// The request that finds the proxy broken gets the failure from its executor.
// That, too, is about the proxy: the account gets no error and no cooldown.
// A network failure that is not a known proxy fault still marks the account,
// as it always has.
func TestProxyFailureIsNotHeldAgainstTheAccount(t *testing.T) {
	exec := &egressAwareExecutor{proxies: map[string]string{}}
	mgr, _ := newEgressManager(t, exec, "eg-a")

	// What an http.Client returns when its transport reports a proxy fault.
	proxyFailure := &url.Error{Op: "Post", URL: "https://chatgpt.com/backend-api/codex/responses", Err: rejectAt(t, mgr, workingProxy)}
	exec.script = []error{proxyFailure}
	err := execute(mgr, context.Background())
	if failure, ok := egressbreaker.AsError(err); !ok || failure.Refused {
		t.Fatalf("Execute error = %v, want the proxy failure passed on", err)
	}
	wantUntouched(t, mgr, "eg-a")

	exec.script = append(exec.script, errors.New("read tcp 10.0.0.1:1->10.0.0.2:443: connection reset by peer"))
	if err = execute(mgr, context.Background()); err == nil {
		t.Fatal("Execute succeeded on a scripted network failure")
	}
	account, _ := mgr.GetByID("eg-a")
	if account.Status != StatusError || account.LastError == nil {
		t.Fatalf("an ordinary network failure left the account at status=%q lastError=%v, want it recorded as before",
			account.Status, account.LastError)
	}
}

func TestExecutionErrorNamesTheProxyFailureByItsCode(t *testing.T) {
	breaker := egressbreaker.New(egressbreaker.Config{})
	permit, _ := breaker.Allow(brokenProxy)
	failure := permit.Done(egressbreaker.AuthRejected(errors.New("username/password authentication failed")))
	wrapped := &url.Error{Op: "Post", URL: "https://chatgpt.com/backend-api/codex/responses", Err: failure}

	got := errorFromExecution(wrapped)
	if got.Code != EgressUnavailableCode || got.HTTPStatus != 0 {
		t.Fatalf("errorFromExecution = %+v, want code %q and no HTTP status", got, EgressUnavailableCode)
	}
	if got.Message != "egress proxy unavailable: proxy authentication rejected" {
		t.Fatalf("message = %q, want the breaker's own text without the upstream URL", got.Message)
	}
	if !isEgressUnavailable(got) || isEgressUnavailable(&Error{HTTPStatus: 503, Message: "upstream unavailable"}) {
		t.Fatal("isEgressUnavailable does not tell a proxy failure from an upstream one")
	}
	// The scheduler must keep reading it as a failure without a status: a
	// status would start the cooldown an upstream 503 starts.
	if status := statusCodeFromError(wrapped); status != 0 {
		t.Fatalf("statusCodeFromError = %d, want 0", status)
	}
}

func TestEgressProxyURLAsksTheExecutorOfTheAccount(t *testing.T) {
	exec := &egressAwareExecutor{proxies: map[string]string{"proxied": " " + brokenProxy + " "}}
	mgr, _ := newEgressManager(t, exec, "proxied", "direct")
	proxied, _ := mgr.GetByID("proxied")
	if got := mgr.EgressProxyURL(proxied); got != brokenProxy {
		t.Fatalf("EgressProxyURL(proxied) = %q, want %q", got, brokenProxy)
	}
	direct, _ := mgr.GetByID("direct")
	if got := mgr.EgressProxyURL(direct); got != "" {
		t.Fatalf("EgressProxyURL(direct) = %q, want empty", got)
	}
	// No executor for the provider, or one that cannot name a proxy: nothing.
	if got := mgr.EgressProxyURL(&Auth{ID: "orphan", Provider: "nobody"}); got != "" {
		t.Fatalf("EgressProxyURL(orphan) = %q, want empty", got)
	}
	plain := newPlainManager(t, &scriptedExecutor{}, "proxied")
	if got := plain.EgressProxyURL(proxied); got != "" {
		t.Fatalf("EgressProxyURL through an executor without a resolver = %q, want empty", got)
	}
	var none *Manager
	if got := none.EgressProxyURL(proxied); got != "" {
		t.Fatalf("nil manager EgressProxyURL = %q", got)
	}
}
