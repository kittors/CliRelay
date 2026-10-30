package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// scriptedStreamExecutor answers each stream attempt with the step its call
// number selects, so a test describes "the first two attempts fail, the third
// works" without depending on which account the scheduler picks first.
type scriptedStreamExecutor struct {
	provider string
	steps    []streamStep

	mu      sync.Mutex
	authIDs []string
}

type streamStep struct {
	payloads [][]byte
	err      error
}

func failBeforeFirstByte(status int) streamStep {
	return streamStep{err: &coreauth.Error{Code: "upstream_error", Message: "upstream broke", HTTPStatus: status}}
}

func succeed(payload string) streamStep {
	return streamStep{payloads: [][]byte{[]byte(payload)}}
}

func (e *scriptedStreamExecutor) Identifier() string { return e.provider }

func (e *scriptedStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *scriptedStreamExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	call := len(e.authIDs)
	e.mu.Unlock()

	step := succeed("ok")
	if call <= len(e.steps) {
		step = e.steps[call-1]
	}
	ch := make(chan coreexecutor.StreamChunk, len(step.payloads)+1)
	for _, payload := range step.payloads {
		ch <- coreexecutor.StreamChunk{Payload: payload}
	}
	if step.err != nil {
		ch <- coreexecutor.StreamChunk{Err: step.err}
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *scriptedStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *scriptedStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *scriptedStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *scriptedStreamExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

// newFailoverTestManager registers one executor and the given accounts, each
// serving test-model, and removes them from the global registry afterwards.
func newFailoverTestManager(t *testing.T, executor coreauth.ProviderExecutor, provider string, authIDs ...string) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, id := range authIDs {
		auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: "test-model"}})
	}
	t.Cleanup(func() {
		for _, id := range authIDs {
			registry.GetGlobalRegistry().UnregisterClient(id)
		}
	})
	return manager
}

type streamOutcome struct {
	payload string
	status  int
	err     error
}

func drainStream(t *testing.T, dataChan <-chan []byte, errChan <-chan *StreamErrorMessage) streamOutcome {
	t.Helper()
	if dataChan == nil || errChan == nil {
		t.Fatalf("expected non-nil channels")
	}
	var out streamOutcome
	var payload []byte
	for chunk := range dataChan {
		payload = append(payload, chunk...)
	}
	for msg := range errChan {
		if msg != nil && msg.Error != nil {
			out.err = msg.Error
			out.status = msg.StatusCode
		}
	}
	out.payload = string(payload)
	return out
}

func distinct(ids []string) bool {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func TestStreamingBootstrapRetriesTellsUnsetFromZero(t *testing.T) {
	cases := []struct {
		name string
		cfg  *sdkconfig.SDKConfig
		want int
	}{
		{name: "no config", cfg: nil, want: defaultStreamingBootstrapRetries},
		{name: "unset", cfg: &sdkconfig.SDKConfig{}, want: defaultStreamingBootstrapRetries},
		{name: "explicit zero turns it off", cfg: &sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: new(0)}}, want: 0},
		{name: "negative turns it off", cfg: &sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: new(-3)}}, want: 0},
		{name: "explicit value kept", cfg: &sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: new(1)}}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StreamingBootstrapRetries(tc.cfg); got != tc.want {
				t.Fatalf("StreamingBootstrapRetries = %d, want %d", got, tc.want)
			}
		})
	}
	if defaultStreamingBootstrapRetries <= 0 {
		t.Fatalf("the default must keep failover on, got %d", defaultStreamingBootstrapRetries)
	}
}

// With nothing configured, a stream that fails before its first byte moves to
// another account, and never back to one that already failed.
func TestStreamFailoverIsOnByDefaultAndSkipsFailedAccounts(t *testing.T) {
	executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{
		failBeforeFirstByte(http.StatusBadGateway),
		failBeforeFirstByte(http.StatusServiceUnavailable),
		succeed("ok"),
	}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-default-a", "sf-default-b", "sf-default-c")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{"model":"test-model"}`), "")
	out := drainStream(t, dataChan, errChan)

	if out.err != nil || out.payload != "ok" {
		t.Fatalf("outcome = %+v, want ok after two failovers", out)
	}
	ids := executor.AuthIDs()
	if len(ids) != 3 || !distinct(ids) {
		t.Fatalf("attempts = %v, want three different accounts", ids)
	}
}

// The candidate count bounds the retries: with two accounts there is only one
// retry, and the client gets the last upstream failure, not "no auth".
func TestStreamFailoverStopsWhenEveryAccountFailed(t *testing.T) {
	executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{
		failBeforeFirstByte(http.StatusBadGateway),
		failBeforeFirstByte(http.StatusServiceUnavailable),
	}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-few-a", "sf-few-b")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{"model":"test-model"}`), "")
	out := drainStream(t, dataChan, errChan)

	ids := executor.AuthIDs()
	if len(ids) != 2 || !distinct(ids) {
		t.Fatalf("attempts = %v, want each of the two accounts once", ids)
	}
	if out.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v), want the second account's 503", out.status, out.err)
	}
}

func TestStreamFailoverExplicitZeroTurnsItOff(t *testing.T) {
	executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{failBeforeFirstByte(http.StatusBadGateway)}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-off-a", "sf-off-b")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: new(0)}}, manager)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{"model":"test-model"}`), "")
	out := drainStream(t, dataChan, errChan)

	if got := executor.AuthIDs(); len(got) != 1 {
		t.Fatalf("attempts = %v, want exactly one with retries switched off", got)
	}
	if out.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", out.status)
	}
}

// Once a byte reached the client the response is committed: a later failure
// is reported, never replayed on another account, default config included.
func TestStreamFailoverNeverRetriesAfterTheFirstByte(t *testing.T) {
	executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{{
		payloads: [][]byte{[]byte("partial")},
		err:      &coreauth.Error{Code: "upstream_error", Message: "broke mid-stream", HTTPStatus: http.StatusBadGateway},
	}}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-sent-a", "sf-sent-b")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{"model":"test-model"}`), "")
	out := drainStream(t, dataChan, errChan)

	if got := executor.AuthIDs(); len(got) != 1 {
		t.Fatalf("attempts = %v, want one: the response was already committed", got)
	}
	if out.payload != "partial" || out.status != http.StatusBadGateway {
		t.Fatalf("outcome = %+v, want the partial payload then the 502", out)
	}
}

// A config reload takes effect on the next request without a restart.
func TestStreamFailoverFollowsAConfigReload(t *testing.T) {
	executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{
		failBeforeFirstByte(http.StatusBadGateway),
		succeed("ok"),
		failBeforeFirstByte(http.StatusBadGateway),
	}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-reload-a", "sf-reload-b")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	stream := func() streamOutcome {
		dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{}`), "")
		return drainStream(t, dataChan, errChan)
	}
	first := stream()
	if first.err != nil {
		t.Fatalf("default config should fail over, got %+v", first)
	}

	handler.UpdateClients(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: new(0)}})
	second := stream()
	if second.status != http.StatusBadGateway {
		t.Fatalf("after reloading bootstrap-retries: 0 the failure must surface, got %+v", second)
	}
	if got := executor.AuthIDs(); len(got) != 3 {
		t.Fatalf("attempts = %v, want 2 for the first request and 1 after the reload", got)
	}
}

// Every switch is written to the request log next to the captured upstream
// exchange, so a request that succeeded on its second account still shows
// why the first one was dropped.
func TestStreamFailoverRecordsTheSwitchInTheRequestLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{
		failBeforeFirstByte(http.StatusBadGateway),
		succeed("ok"),
	}}
	manager := newFailoverTestManager(t, executor, "codex", "sf-log-a", "sf-log-b")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, manager)

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "test-model", []byte(`{}`), "")
	if out := drainStream(t, dataChan, errChan); out.err != nil {
		t.Fatalf("unexpected failure: %+v", out)
	}

	raw, _ := ginCtx.Get("API_RESPONSE")
	logged, _ := raw.([]byte)
	failed := executor.AuthIDs()[0]
	if !strings.Contains(string(logged), "[stream failover] account "+failed) || !strings.Contains(string(logged), "status 502") {
		t.Fatalf("request log = %q, want the failed account and its 502", logged)
	}
}
