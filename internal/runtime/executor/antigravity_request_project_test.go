package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// projectUpstream stands in for CloudCode: loadCodeAssist and the token
// endpoint are answered in process, generation goes to an httptest server
// that records which project each request named.
type projectUpstream struct {
	load      string
	loadGate  chan struct{}
	loads     atomic.Int32
	generates atomic.Int32
	mu        sync.Mutex
	projects  []string
	server    *httptest.Server
}

func newProjectUpstream(t *testing.T, load string) *projectUpstream {
	t.Helper()
	upstream := &projectUpstream{load: load}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.generates.Add(1)
		body, _ := io.ReadAll(r.Body)
		project := gjson.GetBytes(body, "project")
		upstream.mu.Lock()
		if project.Exists() {
			upstream.projects = append(upstream.projects, project.String())
		} else {
			upstream.projects = append(upstream.projects, "<none>")
		}
		upstream.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1,\"totalTokenCount\":3}}}\n\n")
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *projectUpstream) context() context.Context {
	return context.WithValue(context.Background(), util.ContextKeyRoundTripper, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case antigravityauth.APIEndpoint + "/" + antigravityauth.APIVersion + ":loadCodeAssist":
			u.loads.Add(1)
			if u.loadGate != nil {
				<-u.loadGate
			}
			return jsonResponse(u.load), nil
		case "https://oauth2.googleapis.com/token":
			return jsonResponse(`{"access_token":"fresh-token","expires_in":3599,"token_type":"Bearer"}`), nil
		}
		return http.DefaultTransport.RoundTrip(req)
	}))
}

func (u *projectUpstream) namedProjects() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.projects...)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// projectlessAuth is a credential the way production stores many of them: a
// valid token and no project.
func projectlessAuth(id, baseURL string) *cliproxyauth.Auth {
	auth := antigravityTestAuth(baseURL)
	auth.ID = id
	delete(auth.Metadata, "project_id")
	auth.Metadata["refresh_token"] = "refresh-" + id
	return auth
}

func executeAntigravity(ctx context.Context, auth *cliproxyauth.Auth) error {
	_, err := discoveryTestExecutor().Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gemini-2.5-pro",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"Reply with exactly OK."}]}]}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("antigravity")})
	return err
}

// A credential without a project is asked for it before anything is sent, the
// request names what came back, and later requests and the background refresh
// reuse it instead of asking again.
func TestAntigravityExecuteDiscoversProjectBeforeSending(t *testing.T) {
	upstream := newProjectUpstream(t, `{"cloudaicompanionProject":"real-project","currentTier":{"id":"free-tier"}}`)
	auth := projectlessAuth("antigravity-request-discovers.json", upstream.server.URL)
	ctx := upstream.context()

	for i := 0; i < 2; i++ {
		if err := executeAntigravity(ctx, auth); err != nil {
			t.Fatalf("Execute #%d: %v", i+1, err)
		}
	}
	if got := upstream.namedProjects(); len(got) != 2 || got[0] != "real-project" || got[1] != "real-project" {
		t.Fatalf("projects named upstream = %v, want the discovered one on every request", got)
	}
	if got := upstream.loads.Load(); got != 1 {
		t.Fatalf("loadCodeAssist calls = %d, want 1", got)
	}
	if _, ok := auth.Metadata["project_id"]; ok {
		t.Fatal("a request must not write into the manager's copy of the credential")
	}

	updated, err := discoveryTestExecutor().Refresh(ctx, auth)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := metaStringValue(updated.Metadata, "project_id"); got != "real-project" {
		t.Fatalf("refreshed project_id = %q, want the one requests found", got)
	}
	if got := upstream.loads.Load(); got != 1 {
		t.Fatalf("loadCodeAssist calls after refresh = %d, want still 1", got)
	}
}

// When Google gives no project, nothing is sent: no invented name, and a clear
// error instead of the 403 SUBSCRIPTION_REQUIRED a made-up project drew
// (#1032). The verdict is remembered, so the next request fails fast.
func TestAntigravityExecuteRefusesToSendWithoutProject(t *testing.T) {
	upstream := newProjectUpstream(t, `{"allowedTiers":[{"id":"standard-tier","isDefault":true}],"ineligibleTiers":[{"reasonCode":"UNSUPPORTED_CLIENT","reasonMessage":"unsupported"}]}`)
	auth := projectlessAuth("antigravity-request-refused.json", upstream.server.URL)
	ctx := upstream.context()

	for i := 0; i < 2; i++ {
		err := executeAntigravity(ctx, auth)
		var status statusErr
		if !errors.As(err, &status) || status.StatusCode() != http.StatusServiceUnavailable {
			t.Fatalf("Execute #%d error = %v, want a 503 the manager fails over on", i+1, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "no Cloud Code project") || !strings.Contains(msg, "UNSUPPORTED_CLIENT") {
			t.Fatalf("Execute #%d error = %q, want it to name the missing project and Google's reason", i+1, msg)
		}
	}
	if got := upstream.generates.Load(); got != 0 {
		t.Fatalf("generation requests = %d, want 0", got)
	}
	if got := upstream.loads.Load(); got != 1 {
		t.Fatalf("loadCodeAssist calls = %d, want 1: the verdict should hold", got)
	}
}

// The background refresh writes its verdict on the credential; requests honour
// it rather than probing on every call.
func TestAntigravityExecuteHonoursStoredVerdict(t *testing.T) {
	upstream := newProjectUpstream(t, `{"cloudaicompanionProject":"real-project"}`)
	auth := projectlessAuth("antigravity-request-stored-verdict.json", upstream.server.URL)
	auth.Metadata[antigravityauth.MetaProjectIDUnavailable] = true
	auth.Metadata[antigravityauth.MetaProjectIDProbedAt] = time.Now().Add(-time.Hour).UnixMilli()
	auth.Metadata[antigravityauth.MetaProjectIDProbeErr] = "no project_id in response"

	err := executeAntigravity(upstream.context(), auth)
	if err == nil || !strings.Contains(err.Error(), "last discovery: no project_id in response") {
		t.Fatalf("Execute error = %v, want the stored verdict", err)
	}
	if loads, generates := upstream.loads.Load(), upstream.generates.Load(); loads != 0 || generates != 0 {
		t.Fatalf("loadCodeAssist calls = %d, generation requests = %d, want 0 and 0", loads, generates)
	}
}

// Requests that arrive together while the project is unknown share one
// discovery.
func TestAntigravityConcurrentRequestsShareOneDiscovery(t *testing.T) {
	upstream := newProjectUpstream(t, `{"cloudaicompanionProject":"real-project"}`)
	upstream.loadGate = make(chan struct{})
	auth := projectlessAuth("antigravity-request-concurrent.json", upstream.server.URL)
	ctx := upstream.context()

	const callers = 4
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { errs <- executeAntigravity(ctx, auth) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for upstream.loads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the other callers join the discovery
	close(upstream.loadGate)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if got := upstream.loads.Load(); got != 1 {
		t.Fatalf("loadCodeAssist calls = %d, want 1", got)
	}
	for _, project := range upstream.namedProjects() {
		if project != "real-project" {
			t.Fatalf("projects named upstream = %v, want real-project throughout", upstream.namedProjects())
		}
	}
}

func TestAntigravityBuildRequestRefusesMissingProject(t *testing.T) {
	_, _, err := (&AntigravityExecutor{}).buildRequest(context.Background(), &cliproxyauth.Auth{}, "token", "gemini-2.5-pro", []byte(`{"request":{}}`), true, "", "https://example.com", 0)
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("buildRequest error = %v, want a 503 instead of an invented project", err)
	}
	if body := geminiToAntigravity("gemini-2.5-pro", []byte(`{"request":{}}`), "", 0); gjson.GetBytes(body, "project").Exists() {
		t.Fatalf("payload without a project carries one: %s", body)
	}
}

// A request stops waiting after its budget, but the discovery it started runs
// on and its result serves whoever asks next.
func TestAntigravityProjectDiscoveryOutlivesAnImpatientRequest(t *testing.T) {
	registry := newAntigravityProjectRegistry()
	auth := &cliproxyauth.Auth{ID: "antigravity-slow-discovery.json"}
	release := make(chan struct{})
	done := make(chan struct{})
	fetch := func(context.Context) (string, error) {
		defer close(done)
		<-release
		return "late-project", nil
	}

	if _, err := registry.discover(context.Background(), auth, 10*time.Millisecond, fetch); !errors.Is(err, errAntigravityDiscoveryPending) {
		t.Fatalf("discover error = %v, want errAntigravityDiscoveryPending", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.discover(cancelled, auth, 0, fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("discover with a cancelled caller error = %v, want context.Canceled", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the discovery did not finish")
	}
	deadline := time.Now().Add(5 * time.Second)
	for registry.known(auth) == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := registry.known(auth); got != "late-project" {
		t.Fatalf("known project = %q, want late-project", got)
	}
}

// A new login of the same account keeps the file name but not the refresh
// token; an old verdict must not follow it.
func TestAntigravityProjectRegistryKeysOnTheCredential(t *testing.T) {
	registry := newAntigravityProjectRegistry()
	old := &cliproxyauth.Auth{ID: "antigravity-relogin.json", Metadata: map[string]any{"refresh_token": "old"}}
	_, err := registry.discover(context.Background(), old, 0, func(context.Context) (string, error) {
		return "", antigravityauth.ErrNoProjectID
	})
	if !errors.Is(err, antigravityauth.ErrNoProjectID) {
		t.Fatalf("discover error = %v, want ErrNoProjectID", err)
	}
	if registry.failure(old, time.Now()) == nil {
		t.Fatal("the verdict should hold for the credential it was about")
	}
	relogged := &cliproxyauth.Auth{ID: "antigravity-relogin.json", Metadata: map[string]any{"refresh_token": "new"}}
	if err := registry.failure(relogged, time.Now()); err != nil {
		t.Fatalf("the new credential inherited the old verdict: %v", err)
	}
}
