package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// codexCyberPolicyBody is the error body the Codex executor builds from the
// response.failed event of a request its cyber classifier flagged (as logged
// on hk-155 on 2026-11-13).
const codexCyberPolicyBody = `{"error":{"code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request.","type":"invalid_request"}}`

type refusalStatusErr struct {
	status int
	body   string
}

func (e refusalStatusErr) Error() string   { return e.body }
func (e refusalStatusErr) StatusCode() int { return e.status }

func TestUpstreamContentRefusalOfReadsKnownShapes(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantOK   bool
		wantCode string
		wantMsg  string
	}{
		{name: "codex executor status error", err: refusalStatusErr{status: http.StatusBadRequest, body: codexCyberPolicyBody}, wantOK: true, wantCode: "cyber_policy", wantMsg: "This content was flagged for possible cybersecurity risk."},
		{name: "recorded result error", err: &Error{Message: codexCyberPolicyBody, HTTPStatus: http.StatusBadRequest}, wantOK: true, wantCode: "cyber_policy", wantMsg: "This content was flagged"},
		{name: "response.failed event", err: errors.New(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"flagged"}}}`), wantOK: true, wantCode: "cyber_policy", wantMsg: "flagged"},
		{name: "coded auth error", err: &Error{Code: "CYBER_POLICY", Message: "flagged"}, wantOK: true, wantCode: "cyber_policy", wantMsg: "flagged"},
		{name: "context length is a malformed request", err: refusalStatusErr{status: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"}}`}},
		{name: "overload", err: errors.New(`{"error":{"code":"server_is_overloaded","message":"busy"}}`)},
		{name: "code only named in the message", err: errors.New(`{"error":{"code":"invalid_request","message":"cyber_policy"}}`)},
		{name: "plain text", err: errors.New("status 400")},
		{name: "nil", err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refusal, ok := UpstreamContentRefusalOf(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("UpstreamContentRefusalOf ok = %v, want %v (refusal %#v)", ok, tt.wantOK, refusal)
			}
			if !ok {
				return
			}
			if refusal.Code != tt.wantCode || !strings.HasPrefix(refusal.Message, tt.wantMsg) {
				t.Fatalf("refusal = %#v, want code %q and message starting %q", refusal, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// refusalExecutor answers every call for its provider with err, or with ok
// when err is nil.
type refusalExecutor struct {
	provider string
	err      error

	mu    sync.Mutex
	calls []string
}

func (e *refusalExecutor) Identifier() string { return e.provider }

func (e *refusalExecutor) record(auth *Auth) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, auth.ID)
}

func (e *refusalExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *refusalExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(auth)
	if e.err != nil {
		return cliproxyexecutor.Response{}, e.err
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *refusalExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.record(auth)
	if e.err != nil {
		return nil, e.err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *refusalExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("count")}, nil
}

func (e *refusalExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *refusalExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type refusalObserverFunc func(context.Context, UpstreamRefusalEvent)

func (f refusalObserverFunc) OnUpstreamContentRefusal(ctx context.Context, event UpstreamRefusalEvent) {
	f(ctx, event)
}

func newRefusalTestManager(t *testing.T, executors []*refusalExecutor, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, orderedAuthSelector{}, nil)
	for _, executor := range executors {
		manager.RegisterExecutor(executor)
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func TestUpstreamContentRefusalIsNeitherFailedOverNorCooledDown(t *testing.T) {
	executor := &refusalExecutor{provider: "codex", err: refusalStatusErr{status: http.StatusBadRequest, body: codexCyberPolicyBody}}
	manager := newRefusalTestManager(t, []*refusalExecutor{executor},
		&Auth{ID: "codex-a", Provider: "codex", Label: "a@example.com", Status: StatusActive},
		&Auth{ID: "codex-b", Provider: "codex", Label: "b@example.com", Status: StatusActive},
	)
	var (
		mu     sync.Mutex
		events []UpstreamRefusalEvent
	)
	manager.SetUpstreamRefusalObserver(refusalObserverFunc(func(_ context.Context, event UpstreamRefusalEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}))

	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6.1-sol"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("Execute succeeded, want the upstream refusal")
	}
	if got := executor.callCount(); got != 1 {
		t.Fatalf("executor calls = %d, want 1: a content refusal must not be handed to another account", got)
	}
	if !IsRequestFault(err) {
		t.Fatalf("IsRequestFault(%v) = false, want true so a virtual model does not move on", err)
	}
	if cause := FailureCause(err); cause != routetrace.CauseUpstreamContentRefusal {
		t.Fatalf("FailureCause = %q, want %q", cause, routetrace.CauseUpstreamContentRefusal)
	}
	for _, id := range []string{"codex-a", "codex-b"} {
		auth, ok := manager.GetByID(id)
		if !ok {
			t.Fatalf("GetByID(%s) missing", id)
		}
		if blocked, reason, _ := isAuthBlockedForModel(auth, "gpt-6.1-sol", time.Now()); blocked {
			t.Fatalf("auth %s blocked (%v) after a content refusal, want it left alone", id, reason)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("observer events = %d, want 1", len(events))
	}
	got := events[0]
	if got.AuthID != "codex-a" || got.AuthLabel != "a@example.com" || got.Provider != "codex" || got.Model != "gpt-6.1-sol" || got.Refusal.Code != "cyber_policy" {
		t.Fatalf("event = %#v", got)
	}
}

func TestOtherFailuresAreNotReportedAsContentRefusals(t *testing.T) {
	executor := &refusalExecutor{provider: "codex", err: refusalStatusErr{status: http.StatusInternalServerError, body: `{"error":{"code":"server_error","message":"boom"}}`}}
	manager := newRefusalTestManager(t, []*refusalExecutor{executor}, &Auth{ID: "codex-a", Provider: "codex", Status: StatusActive})
	observed := 0
	manager.SetUpstreamRefusalObserver(refusalObserverFunc(func(context.Context, UpstreamRefusalEvent) { observed++ }))

	if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6.1-sol"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("Execute succeeded, want the upstream error")
	}
	if observed != 0 {
		t.Fatalf("observer events = %d, want 0 for an ordinary upstream error", observed)
	}
}

func frozenMeta(value any) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.FrozenProvidersMetadataKey: value}}
}

func TestFrozenProviderIsLeftOutWhenAnotherServesTheModel(t *testing.T) {
	frozen := &refusalExecutor{provider: "codex"}
	open := &refusalExecutor{provider: "openai-compat"}
	manager := newRefusalTestManager(t, []*refusalExecutor{frozen, open},
		&Auth{ID: "a-codex", Provider: "codex", Status: StatusActive},
		&Auth{ID: "b-compat", Provider: "openai-compat", Status: StatusActive},
	)

	opts := frozenMeta(`{"codex":"Codex access is frozen."}`)
	if _, err := manager.Execute(context.Background(), []string{"codex", "openai-compat"}, cliproxyexecutor.Request{Model: "gpt-6.1-sol"}, opts); err != nil {
		t.Fatalf("Execute: %v, want the unfrozen provider to serve", err)
	}
	if frozen.callCount() != 0 || open.callCount() != 1 {
		t.Fatalf("calls codex=%d compat=%d, want 0 and 1", frozen.callCount(), open.callCount())
	}
}

func TestFrozenProviderAnswersWithTheNotice(t *testing.T) {
	notice := "Codex access for this account is frozen: an upstream safety check refused one of its requests (cyber_policy). Ask your administrator to restore it."
	for _, value := range []any{`{"Codex":"` + notice + `"}`, map[string]string{"codex": notice}} {
		for _, stream := range []bool{false, true} {
			executor := &refusalExecutor{provider: "codex"}
			manager := newRefusalTestManager(t, []*refusalExecutor{executor},
				&Auth{ID: "codex-a", Provider: "codex", Status: StatusActive},
			)
			opts := frozenMeta(value)
			var err error
			if stream {
				_, err = manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6.1-sol"}, opts)
			} else {
				_, err = manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6.1-sol"}, opts)
			}
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.Code != ProviderAccessFrozenCode || authErr.HTTPStatus != http.StatusForbidden || authErr.Message != notice {
				t.Fatalf("stream=%v value=%T: error = %#v, want %s with the notice", stream, value, err, ProviderAccessFrozenCode)
			}
			if executor.callCount() != 0 {
				t.Fatalf("stream=%v: executor calls = %d, want 0", stream, executor.callCount())
			}
		}
	}
}

func TestFrozenProvidersMetadataIgnoresEmptyAndMalformedValues(t *testing.T) {
	for _, value := range []any{nil, "", "not json", map[string]string{" ": "x"}, 42} {
		if got := frozenProvidersFromMetadata(map[string]any{cliproxyexecutor.FrozenProvidersMetadataKey: value}); got != nil {
			t.Fatalf("frozenProvidersFromMetadata(%#v) = %#v, want nil", value, got)
		}
	}
}
