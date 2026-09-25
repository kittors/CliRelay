package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

const testVideoModel = "grok-imagine-video-1.5"

// fakeXAI plays the upstream every node talks to: a request id only resolves
// on the account that created it, the rule that makes pinning necessary.
type fakeXAI struct {
	mu     sync.Mutex
	owners map[string]string
}

func newFakeXAI() *fakeXAI { return &fakeXAI{owners: make(map[string]string)} }

// videoRecordingExecutor is one node's xAI executor over the shared upstream.
type videoRecordingExecutor struct {
	imageCaptureExecutor
	upstream *fakeXAI
	mu       sync.Mutex
	polls    []string
	submits  []string
}

func (e *videoRecordingExecutor) Identifier() string { return "xai" }

func (e *videoRecordingExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.upstream.mu.Lock()
	defer e.upstream.mu.Unlock()
	switch opts.Alt {
	case openAIVideoGenerationAlt:
		id := "req-" + auth.ID
		e.upstream.owners[id] = auth.ID
		e.submits = append(e.submits, auth.ID)
		return coreexecutor.Response{Payload: []byte(`{"request_id":"` + id + `"}`)}, nil
	case openAIVideoStatusAlt:
		e.polls = append(e.polls, auth.ID)
		id := strings.TrimSuffix(strings.TrimPrefix(string(req.Payload), `{"request_id":"`), `"}`)
		if e.upstream.owners[id] != auth.ID {
			return coreexecutor.Response{}, errors.New("request id belongs to another account")
		}
		return coreexecutor.Response{Payload: []byte(`{"status":"pending"}`)}, nil
	}
	return coreexecutor.Response{}, errors.New("unexpected alt " + opts.Alt)
}

func (e *videoRecordingExecutor) calls() (submits, polls []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.submits...), append([]string(nil), e.polls...)
}

// videoNode builds one serving process with its own auth manager over the
// same two xAI accounts, as every node of a cluster loads the same
// credentials.
func videoNode(t *testing.T, upstream *fakeXAI) (*gin.Engine, *videoRecordingExecutor) {
	t.Helper()
	executor := &videoRecordingExecutor{upstream: upstream}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, id := range []string{"xai-a.json", "xai-b.json"} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID: id, Provider: "xai", Status: coreauth.StatusActive,
			Metadata: map[string]any{"api_key": "xai-test-key-" + id},
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: testVideoModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	h := NewOpenAIVideosAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if tenant := c.GetHeader("X-Test-Tenant"); tenant != "" {
			c.Set("tenantID", tenant)
		}
		c.Next()
	})
	router.POST("/v1/videos/generations", h.Generations)
	router.GET("/v1/videos/:request_id", h.Status)
	return router, executor
}

func submitVideo(t *testing.T, router *gin.Engine) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{"model":"`+testVideoModel+`","prompt":"a fox"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d body=%s", rec.Code, rec.Body.String())
	}
	id := strings.TrimSuffix(strings.TrimPrefix(rec.Body.String(), `{"request_id":"`), `"}`)
	if id == "" {
		t.Fatalf("no request id in %s", rec.Body.String())
	}
	return id
}

func pollVideo(router *gin.Engine, id, tenant string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+id, nil)
	if tenant != "" {
		req.Header.Set("X-Test-Tenant", tenant)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func resetVideoRegistry(t *testing.T) {
	t.Helper()
	videoJobRegistry.Range(func(key, _ any) bool {
		videoJobRegistry.Delete(key)
		return true
	})
	t.Cleanup(func() { SetVideoJobRouteStore(nil) })
}

// With two accounts for the same provider, every poll must go to the account
// that created the request; letting the scheduler pick sent some polls to the
// sibling account, which answers 404 for an id it never issued.
func TestVideoStatusPollsStayOnSubmittingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetVideoRegistry(t)
	router, executor := videoNode(t, newFakeXAI())

	id := submitVideo(t, router)
	for i := 0; i < 6; i++ {
		if rec := pollVideo(router, id, ""); rec.Code != http.StatusOK {
			t.Fatalf("poll %d status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	submits, polls := executor.calls()
	if len(submits) != 1 {
		t.Fatalf("submits = %v", submits)
	}
	for _, auth := range polls {
		if auth != submits[0] {
			t.Fatalf("poll went to %s, submission was on %s (polls=%v)", auth, submits[0], polls)
		}
	}
}

func TestVideoStatusIsHiddenFromOtherTenants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetVideoRegistry(t)
	router, _ := videoNode(t, newFakeXAI())

	id := submitVideo(t, router)
	rec := pollVideo(router, id, "11111111-1111-1111-1111-111111111111")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not known") {
		t.Fatalf("another tenant's poll = %d %s, want the unknown-id answer", rec.Code, rec.Body.String())
	}
}

type sharedVideoRoutes struct {
	mu     sync.Mutex
	routes map[string]VideoJobRoute
	fail   bool
}

func (s *sharedVideoRoutes) RememberVideoJob(_ context.Context, id string, route VideoJobRoute, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[id] = route
	return nil
}

func (s *sharedVideoRoutes) LookupVideoJob(_ context.Context, id string) (VideoJobRoute, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return VideoJobRoute{}, false, errors.New("database unavailable")
	}
	route, ok := s.routes[id]
	return route, ok, nil
}

func (s *sharedVideoRoutes) ForgetVideoJob(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.routes, id)
	return nil
}

// Node A takes the submission, node B answers the poll: B finds the route in
// the shared store and polls the account A submitted on.
func TestVideoStatusOnAnotherNodeUsesSharedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetVideoRegistry(t)
	shared := &sharedVideoRoutes{routes: make(map[string]VideoJobRoute)}
	SetVideoJobRouteStore(shared)

	upstream := newFakeXAI()
	nodeA, executorA := videoNode(t, upstream)
	nodeB, executorB := videoNode(t, upstream)

	id := submitVideo(t, nodeA)
	submits, _ := executorA.calls()
	if route := shared.routes[id]; route.AuthID != submits[0] || route.Provider != "xai" || route.Model != testVideoModel {
		t.Fatalf("shared route = %+v, want auth %s", route, submits[0])
	}
	if _, ok := videoJobRegistry.Load(id); ok {
		t.Fatal("with a shared store the in-process registry must stay unused")
	}
	for i := 0; i < 4; i++ {
		if rec := pollVideo(nodeB, id, ""); rec.Code != http.StatusOK {
			t.Fatalf("poll on node B = %d %s", rec.Code, rec.Body.String())
		}
	}
	_, polls := executorB.calls()
	for _, auth := range polls {
		if auth != submits[0] {
			t.Fatalf("node B polled %s, submission was on %s", auth, submits[0])
		}
	}

	shared.fail = true
	if rec := pollVideo(nodeB, id, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup failure = %d %s, want 503 rather than a guessed account", rec.Code, rec.Body.String())
	}
}
