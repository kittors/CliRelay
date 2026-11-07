package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestModeratorFunc func(context.Context, *Auth, cliproxyexecutor.Options) RequestModerationResult

func (f requestModeratorFunc) Moderate(ctx context.Context, auth *Auth, opts cliproxyexecutor.Options) RequestModerationResult {
	return f(ctx, auth, opts)
}

type moderationCountingExecutor struct {
	mu          sync.Mutex
	execute     int
	stream      int
	countTokens int
	failAuthID  string
}

func (e *moderationCountingExecutor) Identifier() string { return "moderation-test" }

func (e *moderationCountingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.execute++
	if auth != nil && auth.ID == e.failAuthID {
		return cliproxyexecutor.Response{}, errors.New("upstream unavailable")
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *moderationCountingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.stream++
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *moderationCountingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.countTokens++
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte("count")}, nil
}

func (e *moderationCountingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *moderationCountingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type orderedAuthSelector struct{}

func (orderedAuthSelector) Pick(_ context.Context, _ string, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if len(auths) == 0 {
		return nil, errors.New("no auth")
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return auths[0], nil
}

func newModerationTestManager(t *testing.T, executor *moderationCountingExecutor, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, orderedAuthSelector{}, nil)
	manager.RegisterExecutor(executor)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func TestRequestModeratorBlocksBeforeEveryExecutorPath(t *testing.T) {
	paths := []struct {
		name string
		run  func(*Manager) error
	}{
		{name: "execute", run: func(manager *Manager) error {
			_, err := manager.Execute(context.Background(), []string{"moderation-test"}, cliproxyexecutor.Request{Model: "model"}, cliproxyexecutor.Options{})
			return err
		}},
		{name: "stream", run: func(manager *Manager) error {
			_, err := manager.ExecuteStream(context.Background(), []string{"moderation-test"}, cliproxyexecutor.Request{Model: "model"}, cliproxyexecutor.Options{})
			return err
		}},
		{name: "count", run: func(manager *Manager) error {
			_, err := manager.ExecuteCount(context.Background(), []string{"moderation-test"}, cliproxyexecutor.Request{Model: "model"}, cliproxyexecutor.Options{})
			return err
		}},
	}
	for _, tt := range paths {
		t.Run(tt.name, func(t *testing.T) {
			executor := &moderationCountingExecutor{}
			manager := newModerationTestManager(t, executor, &Auth{ID: "auth-a", Provider: executor.Identifier(), Status: StatusActive})
			manager.SetRequestModerator(requestModeratorFunc(func(context.Context, *Auth, cliproxyexecutor.Options) RequestModerationResult {
				return RequestModerationResult{Blocked: true, Message: "blocked", HTTPStatus: http.StatusUnavailableForLegalReasons}
			}))

			err := tt.run(manager)
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.Code != contentPolicyViolationCode || authErr.HTTPStatus != http.StatusUnavailableForLegalReasons {
				t.Fatalf("error = %#v, want content policy violation", err)
			}
			executor.mu.Lock()
			defer executor.mu.Unlock()
			if executor.execute != 0 || executor.stream != 0 || executor.countTokens != 0 {
				t.Fatalf("executor calls execute=%d stream=%d count=%d, want zero", executor.execute, executor.stream, executor.countTokens)
			}
		})
	}
}

func TestRequestModeratorRunsAgainForFallbackCandidate(t *testing.T) {
	executor := &moderationCountingExecutor{failAuthID: "auth-a"}
	manager := newModerationTestManager(t, executor,
		&Auth{ID: "auth-a", Provider: executor.Identifier(), Status: StatusActive},
		&Auth{ID: "auth-b", Provider: executor.Identifier(), Status: StatusActive},
	)
	var mu sync.Mutex
	var moderated []string
	manager.SetRequestModerator(requestModeratorFunc(func(_ context.Context, auth *Auth, _ cliproxyexecutor.Options) RequestModerationResult {
		mu.Lock()
		moderated = append(moderated, auth.ID)
		mu.Unlock()
		return RequestModerationResult{}
	}))

	if _, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "model"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(moderated) != 2 || moderated[0] != "auth-a" || moderated[1] != "auth-b" {
		t.Fatalf("moderated candidates = %#v, want auth-a then auth-b", moderated)
	}
}

// A shared credential is moderated like any other candidate, and the moderator
// is told both whose credential it is and who is asking: the owner's TenantID on
// auth, the requesting tenant in the request metadata.
func TestRequestModeratorSeesSharedCredentialOwnerAndRequestingTenant(t *testing.T) {
	executor := &moderationCountingExecutor{}
	manager := newModerationTestManager(t, executor,
		&Auth{ID: "system-shared", TenantID: defaultTenantID, Provider: executor.Identifier(), Status: StatusActive},
	)
	manager.SetSharedAuthGrants(map[string][]string{"system-shared": {sharedTestTenantB}})
	var mu sync.Mutex
	var owner, requester string
	manager.SetRequestModerator(requestModeratorFunc(func(_ context.Context, auth *Auth, opts cliproxyexecutor.Options) RequestModerationResult {
		mu.Lock()
		defer mu.Unlock()
		owner = auth.TenantID
		requester, _ = opts.Metadata[cliproxyexecutor.TenantMetadataKey].(string)
		return RequestModerationResult{Blocked: true, Message: "blocked", HTTPStatus: http.StatusForbidden}
	}))

	_, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "model"}, tenantOptions(sharedTestTenantB))
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != contentPolicyViolationCode {
		t.Fatalf("error = %#v, want content policy violation", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if owner != defaultTenantID || requester != sharedTestTenantB {
		t.Fatalf("moderator saw owner=%q requester=%q, want owner=%q requester=%q", owner, requester, defaultTenantID, sharedTestTenantB)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.execute != 0 {
		t.Fatalf("executor calls = %d, want the blocked shared credential never reached", executor.execute)
	}
}
