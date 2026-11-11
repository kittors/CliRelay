package executor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/proxytest"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

type breakerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *breakerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *breakerClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// useTestEgressBreaker replaces the package's breaker with one on a clock the
// test moves, so that waits of minutes take no time. Tests that call it must
// not run in parallel: the breaker is package state.
func useTestEgressBreaker(t *testing.T) (*egressbreaker.Breaker, *breakerClock) {
	t.Helper()
	clock := &breakerClock{now: time.Date(2026, 11, 11, 16, 16, 0, 0, time.UTC)}
	breaker := egressbreaker.New(egressbreaker.Config{Now: clock.Now})
	previous := egressBreaker
	egressBreaker = breaker
	t.Cleanup(func() { egressBreaker = previous })
	return breaker, clock
}

// credentialGate decides which usernames a test proxy accepts and counts how
// often each one tried, which is the number of handshakes that reached the
// proxy with those credentials.
type credentialGate struct {
	mu       sync.Mutex
	rejected map[string]bool
	attempts map[string]int
	// held makes the authentication of one username wait until release is
	// closed, to keep a handshake in flight.
	held    string
	release chan struct{}
}

func newCredentialGate() *credentialGate {
	return &credentialGate{rejected: map[string]bool{}, attempts: map[string]int{}}
}

func (g *credentialGate) accept(username, _ string) bool {
	g.mu.Lock()
	g.attempts[username]++
	var wait chan struct{}
	if g.held == username {
		wait = g.release
	}
	g.mu.Unlock()
	if wait != nil {
		<-wait
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.rejected[username]
}

func (g *credentialGate) setRejected(username string, rejected bool) {
	g.mu.Lock()
	g.rejected[username] = rejected
	g.mu.Unlock()
}

func (g *credentialGate) attemptsOf(username string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts[username]
}

// hold makes the next authentications of username wait, and returns the
// function that lets them go.
func (g *credentialGate) hold(username string) (release func()) {
	ch := make(chan struct{})
	g.mu.Lock()
	g.held, g.release = username, ch
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.held, g.release = "", nil
		g.mu.Unlock()
		close(ch)
	}
}

func countingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, hits
}

// getThrough sends one request the way an executor does and reports how it
// went.
func getThrough(cfg *config.Config, auth *cliproxyauth.Auth, target string) error {
	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 10*time.Second)
	resp, err := client.Get(target)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream answered %d", resp.StatusCode)
	}
	return nil
}

func wantRefused(t *testing.T, err error) {
	t.Helper()
	refusal, ok := egressbreaker.AsError(err)
	if !ok || !refusal.Refused {
		t.Fatalf("request error = %v, want it refused before being sent", err)
	}
}

func wantProxyFailure(t *testing.T, err error, fault egressbreaker.Fault) {
	t.Helper()
	failure, ok := egressbreaker.AsError(err)
	if !ok || failure.Refused || failure.Fault != fault {
		t.Fatalf("request error = %v, want the request's own failure with fault %q", err, fault)
	}
}

// The 2026-11-11 outage in miniature: a proxy starts rejecting one set of
// credentials while it keeps serving another set on the same host and port.
func TestEgressBreakerStopsSendingThroughAProxyThatRejectsItsCredentials(t *testing.T) {
	breaker, clock := useTestEgressBreaker(t)
	upstream, upstreamHits := countingUpstream(t)

	gate := newCredentialGate()
	gate.setRejected("tenant-a", true)
	proxy := proxytest.NewSOCKS5(t)
	proxy.RequireAuth(gate.accept)

	cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{
		{ID: "expired", Name: "Expired", URL: proxy.URL("tenant-a", "expired"), Enabled: true},
		{ID: "valid", Name: "Valid", URL: proxy.URL("tenant-b", "valid"), Enabled: true},
	}}
	accounts := make([]*cliproxyauth.Auth, 20)
	for i := range accounts {
		accounts[i] = &cliproxyauth.Auth{ID: fmt.Sprintf("account-%d", i), Provider: "codex", ProxyID: "expired"}
	}
	healthy := &cliproxyauth.Auth{ID: "healthy", Provider: "codex", ProxyID: "valid"}

	healthyRequests := int32(0)
	mustServeHealthy := func() {
		t.Helper()
		if err := getThrough(cfg, healthy, upstream.URL); err != nil {
			t.Fatalf("the proxy entry with valid credentials failed: %v", err)
		}
		healthyRequests++
		if got := upstreamHits.Load(); got != healthyRequests {
			t.Fatalf("upstream saw %d requests, want the %d sent through the working entry", got, healthyRequests)
		}
	}

	// Every account of the pool is tried, five times over. Only the first
	// two requests reach the proxy, the second confirming what the first
	// found; the other 98 never leave.
	for round := range 5 {
		for i, account := range accounts {
			err := getThrough(cfg, account, upstream.URL)
			if round == 0 && i < egressbreaker.DefaultRejectionThreshold {
				wantProxyFailure(t, err, egressbreaker.FaultAuthRejected)
				for _, secret := range []string{proxy.Addr(), "tenant-a", "expired"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("error %q names %q", err, secret)
					}
				}
				continue
			}
			wantRefused(t, err)
		}
		mustServeHealthy()
	}
	if got := gate.attemptsOf("tenant-a"); got != 2 {
		t.Fatalf("the proxy saw %d handshakes with the rejected credentials for 100 requests, want 2", got)
	}

	// The wait is over: one request goes out to try the proxy, is rejected
	// again, and the breaker shuts for twice as long.
	clock.Advance(egressbreaker.DefaultBaseOpen)
	wantProxyFailure(t, getThrough(cfg, accounts[0], upstream.URL), egressbreaker.FaultAuthRejected)
	for _, account := range accounts {
		wantRefused(t, getThrough(cfg, account, upstream.URL))
	}
	clock.Advance(egressbreaker.DefaultBaseOpen)
	wantRefused(t, getThrough(cfg, accounts[1], upstream.URL))
	if got := gate.attemptsOf("tenant-a"); got != 3 {
		t.Fatalf("the proxy saw %d handshakes after one probe, want 3", got)
	}
	mustServeHealthy()

	// The second probe is held in mid-handshake while twenty more requests
	// arrive. None of them may go out next to it.
	clock.Advance(egressbreaker.DefaultBaseOpen)
	release := gate.hold("tenant-a")
	probeDone := make(chan error, 1)
	go func() { probeDone <- getThrough(cfg, accounts[2], upstream.URL) }()
	deadline := time.Now().Add(10 * time.Second)
	for gate.attemptsOf("tenant-a") < 4 {
		if time.Now().After(deadline) {
			release()
			t.Fatal("the probe never reached the proxy")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var concurrent sync.WaitGroup
	refused := atomic.Int32{}
	for _, account := range accounts {
		concurrent.Go(func() {
			if refusal, ok := egressbreaker.AsError(getThrough(cfg, account, upstream.URL)); ok && refusal.Refused {
				refused.Add(1)
			}
		})
	}
	concurrent.Wait()
	if got := refused.Load(); got != int32(len(accounts)) {
		release()
		t.Fatalf("%d of %d requests were refused while the probe was out", got, len(accounts))
	}
	if got := gate.attemptsOf("tenant-a"); got != 4 {
		release()
		t.Fatalf("the proxy saw %d handshakes while one probe was out, want 4", got)
	}

	// The provider restores the account. The probe in flight gets through,
	// and with nobody touching anything the breaker lets traffic flow again.
	gate.setRejected("tenant-a", false)
	release()
	if err := <-probeDone; err != nil {
		t.Fatalf("the probe failed although the proxy accepts the credentials again: %v", err)
	}
	for _, account := range accounts {
		if err := getThrough(cfg, account, upstream.URL); err != nil {
			t.Fatalf("request after recovery failed: %v", err)
		}
	}
	if open := breaker.Open(); len(open) != 0 {
		t.Fatalf("breaker still open after recovery: %+v", open)
	}
	if got, want := upstreamHits.Load(), healthyRequests+1+int32(len(accounts)); got != want {
		t.Fatalf("upstream saw %d requests, want %d", got, want)
	}
	// Four handshakes were spent on the outage: the two requests that found
	// it and two probes, for 162 requests.
	if got := gate.attemptsOf("tenant-a"); got < 4 {
		t.Fatalf("attempts = %d, want at least the 4 of the outage", got)
	}
}

// A proxy that cannot be connected to at all opens the breaker after a few
// failures in a row, and comes back the same way.
func TestEgressBreakerOpensWhenTheProxyCannotBeReached(t *testing.T) {
	breaker, clock := useTestEgressBreaker(t)
	upstream, upstreamHits := countingUpstream(t)

	// A port nothing listens on: reserve one, then free it.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := listener.Addr().String()
	_ = listener.Close()

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: "account", Provider: "codex", ProxyURL: "socks5://" + deadAddr}

	for i := range egressbreaker.DefaultUnreachableThreshold {
		errGet := getThrough(cfg, auth, upstream.URL)
		wantProxyFailure(t, errGet, egressbreaker.FaultUnreachable)
		if strings.Contains(errGet.Error(), deadAddr) {
			t.Fatalf("error %q names the proxy address", errGet)
		}
		if open := breaker.Open(); (len(open) == 1) != (i == egressbreaker.DefaultUnreachableThreshold-1) {
			t.Fatalf("after %d failed connects the breaker reports %+v", i+1, open)
		}
	}
	for range 30 {
		wantRefused(t, getThrough(cfg, auth, upstream.URL))
	}

	// A direct request is none of the breaker's business.
	if errDirect := getThrough(cfg, &cliproxyauth.Auth{ID: "direct"}, upstream.URL); errDirect != nil {
		t.Fatalf("a request without a proxy failed: %v", errDirect)
	}

	// The proxy comes up on the address it had.
	revived, err := net.Listen("tcp4", deadAddr)
	if err != nil {
		t.Skipf("cannot listen on %s again: %v", deadAddr, err)
	}
	proxy := proxytest.NewSOCKS5On(t, revived)
	wantRefused(t, getThrough(cfg, auth, upstream.URL))
	if got := proxy.Handshakes(); got != 0 {
		t.Fatalf("%d handshakes reached the proxy before the wait was over", got)
	}
	clock.Advance(egressbreaker.DefaultBaseOpen)
	if errGet := getThrough(cfg, auth, upstream.URL); errGet != nil {
		t.Fatalf("the probe failed although the proxy is back: %v", errGet)
	}
	if open := breaker.Open(); len(open) != 0 {
		t.Fatalf("breaker still open after recovery: %+v", open)
	}
	if got := upstreamHits.Load(); got != 2 {
		t.Fatalf("upstream saw %d requests, want the direct one and the probe", got)
	}
}

// The Codex websocket dial is a new connection through the account's proxy
// and passes the same breaker.
func TestCodexWebsocketDialPassesTheEgressBreaker(t *testing.T) {
	useTestEgressBreaker(t)
	gate := newCredentialGate()
	gate.setRejected("tenant-a", true)
	proxy := proxytest.NewSOCKS5(t)
	proxy.RequireAuth(gate.accept)

	cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{
		{ID: "expired", Name: "Expired", URL: proxy.URL("tenant-a", "expired"), Enabled: true},
	}}
	auth := &cliproxyauth.Auth{ID: "account", Provider: "codex", ProxyID: "expired"}
	exec := NewCodexWebsocketsExecutor(cfg)

	for range egressbreaker.DefaultRejectionThreshold {
		conn, resp, err := exec.dialCodexWebsocket(context.Background(), auth, "ws://upstream.invalid/responses", http.Header{}) //nolint:bodyclose // a failed dial returns no response.
		if conn != nil || resp != nil {
			t.Fatal("the websocket dial succeeded through a proxy that rejects the credentials")
		}
		wantProxyFailure(t, err, egressbreaker.FaultAuthRejected)
	}
	for range 10 {
		_, _, err := exec.dialCodexWebsocket(context.Background(), auth, "ws://upstream.invalid/responses", http.Header{}) //nolint:bodyclose // a failed dial returns no response.
		wantRefused(t, err)
	}
	if got := gate.attemptsOf("tenant-a"); got != 2 {
		t.Fatalf("the proxy saw %d handshakes for 12 websocket dials, want 2", got)
	}
	// The same account's HTTP requests share the proxy, and so the breaker.
	wantRefused(t, getThrough(cfg, auth, "http://upstream.invalid/"))
}

// The scheduler skips an account when the breaker is open for the URL its
// executor names, so that URL has to be the one the executor's requests are
// keyed by.
func TestExecutorsResolveTheirEgressProxy(t *testing.T) {
	breaker, _ := useTestEgressBreaker(t)

	cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{
		{ID: "pool", Name: "Pool", URL: "socks5://tenant:secret@198.51.100.9:1080", Enabled: true},
	}}
	cfg.ProxyURL = "http://global.example:8080"
	// The pool entry wins over the credential's own proxy-url, which wins
	// over the global one.
	auth := &cliproxyauth.Auth{ID: "account", ProxyID: "pool", ProxyURL: "http://own.example:8080"}
	const poolURL = "socks5://tenant:secret@198.51.100.9:1080"

	executors := map[string]cliproxyauth.ProviderExecutor{
		"antigravity":      NewAntigravityExecutor(cfg),
		"bedrock":          NewBedrockExecutor(cfg),
		"claude":           NewClaudeExecutor(cfg),
		"codex":            NewCodexExecutor(cfg),
		"codex-auto":       NewCodexAutoExecutor(cfg),
		"codex-websockets": NewCodexWebsocketsExecutor(cfg),
		"gemini":           NewGeminiExecutor(cfg),
		"gemini-cli":       NewGeminiCLIExecutor(cfg),
		"iflow":            NewIFlowExecutor(cfg),
		"kimi":             NewKimiExecutor(cfg),
		"minimax":          NewMiniMaxExecutor("minimax", cfg),
		"ollama-cloud":     NewOllamaCloudExecutor(cfg),
		"openai-compat":    NewOpenAICompatExecutor("openrouter", cfg),
		"opencode-go":      NewOpenCodeGoExecutor(cfg),
		"qwen":             NewQwenExecutor(cfg),
		"vertex":           NewGeminiVertexExecutor(cfg),
		"xai":              NewXAIExecutor(cfg),
	}
	for name, exec := range executors {
		resolver, ok := exec.(cliproxyauth.EgressProxyResolver)
		if !ok {
			t.Errorf("%s: the executor does not name its egress proxy", name)
			continue
		}
		if got := resolver.EgressProxyURL(auth); got != poolURL {
			t.Errorf("%s: EgressProxyURL = %q, want the credential's pool entry", name, got)
		}
		if got := resolver.EgressProxyURL(&cliproxyauth.Auth{ID: "plain"}); got != cfg.ProxyURL {
			t.Errorf("%s: EgressProxyURL without a proxy of its own = %q, want the global proxy", name, got)
		}
	}

	// And the URL the executors name is the one their HTTP requests pass the
	// breaker with: opening the breaker for it refuses such a request.
	for range egressbreaker.DefaultRejectionThreshold {
		permit, err := breaker.Allow(poolURL)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		_ = permit.Done(egressbreaker.AuthRejected(fmt.Errorf("rejected")))
	}
	wantRefused(t, getThrough(cfg, auth, "http://upstream.invalid/"))
}

// countingCompatExecutor is the real OpenAI-compatible executor, counting how
// often the scheduler hands it a request.
type countingCompatExecutor struct {
	*OpenAICompatExecutor
	calls atomic.Int32
}

func (e *countingCompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return e.OpenAICompatExecutor.Execute(ctx, auth, req, opts)
}

// The whole path as production wires it: the scheduler, a real executor and
// its transport on the process-wide breaker. Two accounts' attempts find the
// proxy broken; from then on the scheduler does not hand the executor any
// account behind that proxy, so nothing is translated and nothing is sent,
// and none of the accounts is marked as failing.
func TestSchedulerStopsCallingExecutorsBehindABrokenProxy(t *testing.T) {
	if egressBreaker != egressbreaker.Default() {
		t.Fatal("the executors do not use the process-wide breaker the scheduler consults")
	}
	t.Cleanup(egressbreaker.Default().Reset)

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"test-model",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	gate := newCredentialGate()
	gate.setRejected("tenant-a", true)
	proxy := proxytest.NewSOCKS5(t)
	proxy.RequireAuth(gate.accept)

	cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{
		{ID: "expired", Name: "Expired", URL: proxy.URL("tenant-a", "expired"), Enabled: true},
	}}
	exec := &countingCompatExecutor{OpenAICompatExecutor: NewOpenAICompatExecutor("egress-test", cfg)}
	mgr := cliproxyauth.NewManager(nil, nil, nil)
	mgr.RegisterExecutor(exec)
	ids := make([]string, 5)
	for i := range ids {
		ids[i] = fmt.Sprintf("egress-account-%d", i)
		account := &cliproxyauth.Auth{
			ID: ids[i], Provider: "egress-test", Status: cliproxyauth.StatusActive, ProxyID: "expired",
			Attributes: map[string]string{"base_url": upstream.URL, "api_key": "test-key"},
		}
		if _, err := mgr.Register(context.Background(), account); err != nil {
			t.Fatalf("register %s: %v", ids[i], err)
		}
	}

	send := func() error {
		_, err := mgr.Execute(context.Background(), []string{"egress-test"},
			cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
		return err
	}

	// The first request walks the pool. Two accounts reach the proxy, the
	// second confirming the fault; the other three are skipped before the
	// executor is called for them.
	if _, ok := egressbreaker.AsError(send()); !ok {
		t.Fatal("the first request did not fail with the proxy's failure")
	}
	for range 20 {
		wantRefused(t, send())
	}
	if got := exec.calls.Load(); got != 2 {
		t.Fatalf("the executor was called %d times for 21 requests over 5 accounts, want 2", got)
	}
	if got := gate.attemptsOf("tenant-a"); got != 2 {
		t.Fatalf("the proxy saw %d handshakes, want 2", got)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream saw %d requests through a proxy that rejects the credentials", got)
	}
	for _, id := range ids {
		account, ok := mgr.GetByID(id)
		if !ok {
			t.Fatalf("account %s is gone", id)
		}
		if account.Status != cliproxyauth.StatusActive || account.LastError != nil || account.Unavailable || len(account.ModelStates) != 0 {
			t.Fatalf("account %s was marked for its proxy's failure: status=%q lastError=%v states=%v",
				id, account.Status, account.LastError, account.ModelStates)
		}
	}

	// With the proxy accepting the credentials again and the breaker's wait
	// behind it (skipped here by forgetting the state), requests are served.
	gate.setRejected("tenant-a", false)
	egressbreaker.Default().Reset()
	if err := send(); err != nil {
		t.Fatalf("request after recovery failed: %v", err)
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("upstream saw %d requests after recovery, want 1", got)
	}
}
