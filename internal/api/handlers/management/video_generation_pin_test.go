package management

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// consoleVideoExecutor plays xAI for the console test: a request id only
// resolves on the account that created it.
type consoleVideoExecutor struct {
	mu      sync.Mutex
	owners  map[string]string
	polls   []string
	submits []string
}

func (e *consoleVideoExecutor) Identifier() string { return "xai" }

func (e *consoleVideoExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch opts.Alt {
	case videoGenerationAlt:
		id := "req-" + auth.ID
		e.owners[id] = auth.ID
		e.submits = append(e.submits, auth.ID)
		return coreexecutor.Response{Payload: []byte(`{"request_id":"` + id + `"}`)}, nil
	case videoStatusAlt:
		e.polls = append(e.polls, auth.ID)
		id := strings.TrimSuffix(strings.TrimPrefix(string(req.Payload), `{"request_id":"`), `"}`)
		if e.owners[id] != auth.ID {
			return coreexecutor.Response{}, errors.New("request id belongs to another account")
		}
		if len(e.polls) < 3 {
			return coreexecutor.Response{Payload: []byte(`{"status":"pending"}`)}, nil
		}
		return coreexecutor.Response{Payload: []byte(`{"status":"done","video":{"url":"https://video.example/clip.mp4"}}`)}, nil
	}
	return coreexecutor.Response{}, errors.New("unexpected alt " + opts.Alt)
}

func (e *consoleVideoExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *consoleVideoExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *consoleVideoExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *consoleVideoExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// With two xAI accounts the console test used to poll whichever account the
// scheduler picked, and a poll on the sibling account got a 404 for an id it
// never issued.
func TestConsoleVideoPollsStayOnSubmittingAccount(t *testing.T) {
	previous := videoPollInterval
	videoPollInterval = time.Millisecond
	t.Cleanup(func() { videoPollInterval = previous })

	const model = "grok-imagine-video-1.5"
	executor := &consoleVideoExecutor{owners: make(map[string]string)}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, id := range []string{"xai-a.json", "xai-b.json"} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID: id, Provider: "xai", Status: coreauth.StatusActive,
			Metadata: map[string]any{"api_key": "xai-test-key-" + id},
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}

	result, err := h.executeVideoGenerationTestForTenant(context.Background(), "", []byte(`{"model":"`+model+`","prompt":"a fox"}`), videoGenerationAlt)
	if err != nil {
		t.Fatalf("console video test failed: %v", err)
	}
	if !strings.Contains(string(result), `"done"`) {
		t.Fatalf("result = %s", result)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.submits) != 1 || len(executor.polls) < 3 {
		t.Fatalf("submits=%v polls=%v", executor.submits, executor.polls)
	}
	for _, auth := range executor.polls {
		if auth != executor.submits[0] {
			t.Fatalf("poll went to %s, submission was on %s", auth, executor.submits[0])
		}
	}
}
