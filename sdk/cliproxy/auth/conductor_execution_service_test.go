package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type executionMetadataExecutor struct {
	mu                 sync.Mutex
	seenModel          string
	seenRequestedModel string
	seenSelectedAuthID string
}

func (e *executionMetadataExecutor) Identifier() string { return "codex" }

func (e *executionMetadataExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seenModel = req.Model
	if value, _ := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(string); value != "" {
		e.seenRequestedModel = value
	}
	if value, _ := opts.Metadata[cliproxyexecutor.SelectedAuthMetadataKey].(string); value != "" {
		e.seenSelectedAuthID = value
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *executionMetadataExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *executionMetadataExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *executionMetadataExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *executionMetadataExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

type streamChunkFailureExecutor struct{}

func (e *streamChunkFailureExecutor) Identifier() string { return "codex" }

func (e *streamChunkFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *streamChunkFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{
		Err: &Error{
			Code:       "stream_failed",
			Message:    "stream chunk failed",
			HTTPStatus: http.StatusBadGateway,
		},
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *streamChunkFailureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *streamChunkFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *streamChunkFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

type claudeOAuthHeadersExecutor struct {
	executeResponse cliproxyexecutor.Response
	executeErr      error
	countResponse   cliproxyexecutor.Response
	countErr        error
	streamHeaders   http.Header
	streamChunkErr  error
}

func (e *claudeOAuthHeadersExecutor) Identifier() string { return "claude" }

func (e *claudeOAuthHeadersExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.executeResponse, e.executeErr
}

func (e *claudeOAuthHeadersExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	if e.streamChunkErr != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: e.streamChunkErr}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{
		Headers: e.streamHeaders.Clone(),
		Chunks:  chunks,
	}, nil
}

func (e *claudeOAuthHeadersExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *claudeOAuthHeadersExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.countResponse, e.countErr
}

func (e *claudeOAuthHeadersExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

type resultRecordingHook struct {
	NoopHook
	mu      sync.Mutex
	results []Result
}

func (h *resultRecordingHook) OnResult(_ context.Context, result Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, result)
}

func (h *resultRecordingHook) snapshot() []Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Result, len(h.results))
	copy(out, h.results)
	return out
}

func TestManagerExecute_PublishesRequestedModelMetadataAndSelectedAuth(t *testing.T) {
	t.Parallel()

	executor := &executionMetadataExecutor{}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
		Prefix:   "team",
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	selectedByCallback := ""
	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "team/gpt-5-codex",
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(id string) {
				selectedByCallback = id
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(resp.Payload) != "ok" {
		t.Fatalf("Execute() payload = %q, want ok", string(resp.Payload))
	}

	executor.mu.Lock()
	seenModel := executor.seenModel
	seenRequestedModel := executor.seenRequestedModel
	seenSelectedAuthID := executor.seenSelectedAuthID
	executor.mu.Unlock()

	if seenModel != "gpt-5-codex" {
		t.Fatalf("executor req.Model = %q, want gpt-5-codex", seenModel)
	}
	if seenRequestedModel != "team/gpt-5-codex" {
		t.Fatalf("requested model metadata = %q, want team/gpt-5-codex", seenRequestedModel)
	}
	if seenSelectedAuthID != auth.ID {
		t.Fatalf("selected auth metadata = %q, want %q", seenSelectedAuthID, auth.ID)
	}
	if selectedByCallback != auth.ID {
		t.Fatalf("selected auth callback = %q, want %q", selectedByCallback, auth.ID)
	}
}

func TestManagerExecuteStream_RecordsFailureFromChunkError(t *testing.T) {
	t.Parallel()

	hook := &resultRecordingHook{}
	manager := NewManager(nil, &FillFirstSelector{}, hook)
	manager.RegisterExecutor(&streamChunkFailureExecutor{})

	auth := &Auth{
		ID:       "stream-auth",
		Provider: "codex",
		Status:   StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5-codex",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if stream == nil {
		t.Fatal("ExecuteStream() stream = nil")
	}

	var chunkErr error
	for chunk := range stream.Chunks {
		chunkErr = chunk.Err
	}
	if chunkErr == nil {
		t.Fatal("expected stream chunk error")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		results := hook.snapshot()
		if len(results) == 1 {
			result := results[0]
			if result.Success {
				t.Fatalf("hook result Success = true, want false")
			}
			if result.AuthID != auth.ID {
				t.Fatalf("hook result AuthID = %q, want %q", result.AuthID, auth.ID)
			}
			if result.Provider != "codex" {
				t.Fatalf("hook result Provider = %q, want codex", result.Provider)
			}
			if result.Model != "gpt-5-codex" {
				t.Fatalf("hook result Model = %q, want gpt-5-codex", result.Model)
			}
			if result.Error == nil || result.Error.Message != chunkErr.Error() {
				t.Fatalf("hook result Error = %+v, want %q", result.Error, chunkErr.Error())
			}
			if result.Error.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("hook result HTTPStatus = %d, want %d", result.Error.HTTPStatus, http.StatusBadGateway)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected 1 hook result, got %d", len(hook.snapshot()))
}

func TestManagerExecute_ClaudeOAuth401WithRefreshTokenRecordsHealthFromUpstreamError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "claude-sonnet-4-5"
	headers := http.Header{
		"X-Claude-Request-Id": []string{"req-401"},
	}
	hook := &resultRecordingHook{}
	manager := NewManager(nil, &FillFirstSelector{}, hook)
	manager.RegisterExecutor(&claudeOAuthHeadersExecutor{
		executeErr: &statusQuotaErrorStub{
			message: "invalid oauth token",
			status:  http.StatusUnauthorized,
			headers: headers,
		},
	})
	auth := newClaudeOAuthTestAuth("claude-oauth")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 401")
	}

	results := hook.snapshot()
	if len(results) != 1 {
		t.Fatalf("hook results = %d, want 1", len(results))
	}
	result := results[0]
	if result.Success {
		t.Fatal("hook result Success = true, want false")
	}
	if result.Error == nil || result.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("hook result Error = %#v, want 401", result.Error)
	}
	if result.Headers.Get("X-Claude-Request-Id") != "req-401" {
		t.Fatalf("hook result headers = %#v, want request id from upstream error", result.Headers)
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got.Metadata["access_token"] != "old-access-token" || got.Metadata["refresh_token"] != "stable-refresh-token" {
		t.Fatalf("tokens were mutated: %#v", got.Metadata)
	}
	if !manager.shouldRefresh(got, time.Now()) {
		t.Fatal("shouldRefresh = false, want true for OAuth refresh_pending")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Status != StatusActive || state.StatusMessage != claudeOAuthRefreshPendingMessage {
		t.Fatalf("model status = %q/%q, want active/%q", state.Status, state.StatusMessage, claudeOAuthRefreshPendingMessage)
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != claudeOAuthHealthStatusRefreshPending ||
		health["temporary_unschedulable_reason"] != claudeOAuthReasonOAuth401 ||
		health["refresh_available"] != true {
		t.Fatalf("health = %#v, want refresh_pending oauth_401 with refresh available", health)
	}
}

func TestManagerExecute_ClaudeOAuth429WithAnthropicHeadersPrefersSevenDayWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "claude-sonnet-4-5"
	fiveHourReset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	sevenDayReset := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":              []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":               []string{formatInt64(fiveHourReset.Unix())},
		"Anthropic-Ratelimit-Unified-5h-Utilization":         []string{"1.00"},
		"Anthropic-Ratelimit-Unified-7d-Status":              []string{"allowed_warning"},
		"Anthropic-Ratelimit-Unified-7d-Reset":               []string{formatInt64(sevenDayReset.Unix() * 1000)},
		"Anthropic-Ratelimit-Unified-7d-Surpassed-Threshold": []string{"true"},
	}
	hook := &resultRecordingHook{}
	manager := NewManager(nil, &FillFirstSelector{}, hook)
	manager.RegisterExecutor(&claudeOAuthHeadersExecutor{
		executeErr: &statusQuotaErrorStub{
			message: "rate limited",
			status:  http.StatusTooManyRequests,
			headers: headers,
		},
	})
	auth := newClaudeOAuthTestAuth("claude-oauth")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 429")
	}

	results := hook.snapshot()
	if len(results) != 1 {
		t.Fatalf("hook results = %d, want 1", len(results))
	}
	if gotHeader := results[0].Headers.Get("Anthropic-Ratelimit-Unified-7d-Status"); gotHeader != "allowed_warning" {
		t.Fatalf("hook 7d header = %q, want allowed_warning", gotHeader)
	}
	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Window != "7d" || state.Quota.WindowMinutes != 10080 {
		t.Fatalf("quota = %#v, want 7d/10080", state.Quota)
	}
	if !state.NextRetryAfter.Equal(sevenDayReset) || !state.Quota.NextRecoverAt.Equal(sevenDayReset) {
		t.Fatalf("recover = %v/%v, want %v", state.NextRetryAfter, state.Quota.NextRecoverAt, sevenDayReset)
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != claudeOAuthHealthStatusExhausted ||
		health["temporary_unschedulable_reason"] != claudeOAuthReasonAnthropic7D {
		t.Fatalf("health = %#v, want exhausted 7d", health)
	}
	windows, ok := health["windows"].(map[string]any)
	if !ok {
		t.Fatalf("health windows missing in %#v", health)
	}
	sevenDay, ok := windows["seven_day"].(map[string]any)
	if !ok || sevenDay["exceeded"] != true || sevenDay["surpassed_threshold"] != true {
		t.Fatalf("seven_day window = %#v, want exceeded surpassed threshold", sevenDay)
	}
}

func TestManagerExecuteCount_ClaudeOAuthFailureDoesNotMutateOAuthHealth(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "claude-sonnet-4-5"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&claudeOAuthHeadersExecutor{
		executeResponse: cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)},
		countErr: &statusQuotaErrorStub{
			message: "invalid oauth token from count_tokens",
			status:  http.StatusUnauthorized,
			headers: http.Header{
				"X-Claude-Request-Id": []string{"req-count-401"},
			},
		},
	})
	auth := newClaudeOAuthTestAuth("claude-oauth")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, err := manager.ExecuteCount(ctx, []string{"claude"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("ExecuteCount() error = nil, want upstream 401")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	if _, ok := got.Metadata[ClaudeOAuthHealthMetadataKey]; ok {
		t.Fatalf("claude_oauth_health = %#v, want count_tokens failure not to mutate runtime health", got.Metadata[ClaudeOAuthHealthMetadataKey])
	}
	if manager.shouldRefresh(got, time.Now()) {
		t.Fatal("shouldRefresh = true, want count_tokens failure not to schedule refresh")
	}
	resp, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() after count_tokens failure error = %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("Execute() payload = %q, want ok response", string(resp.Payload))
	}
}

func TestManagerExecuteStream_ClaudeOAuthChunkErrorFallsBackToInitialAnthropicHeaders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "claude-sonnet-4-5"
	resetAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":      []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{formatInt64(resetAt.Unix())},
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"1.01"},
	}
	hook := &resultRecordingHook{}
	manager := NewManager(nil, &FillFirstSelector{}, hook)
	manager.RegisterExecutor(&claudeOAuthHeadersExecutor{
		streamHeaders:  headers,
		streamChunkErr: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stream rate limited"},
	})
	auth := newClaudeOAuthTestAuth("claude-oauth")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	stream, err := manager.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if stream == nil {
		t.Fatal("ExecuteStream() stream = nil")
	}
	var chunkErr error
	for chunk := range stream.Chunks {
		chunkErr = chunk.Err
	}
	if chunkErr == nil {
		t.Fatal("expected stream chunk error")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		results := hook.snapshot()
		if len(results) != 1 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		result := results[0]
		if result.Success {
			t.Fatal("hook result Success = true, want false")
		}
		if result.Headers.Get("Anthropic-Ratelimit-Unified-5h-Status") != "rejected" {
			t.Fatalf("hook headers = %#v, want initial stream rate-limit headers", result.Headers)
		}
		got, ok := manager.GetByID(auth.ID)
		if !ok {
			t.Fatal("updated auth missing")
		}
		state := got.ModelStates[model]
		if state == nil {
			t.Fatal("model state missing")
		}
		if state.Quota.Window != "5h" || state.Quota.WindowMinutes != 300 {
			t.Fatalf("quota = %#v, want 5h/300 from initial stream headers", state.Quota)
		}
		if !state.NextRetryAfter.Equal(resetAt) {
			t.Fatalf("NextRetryAfter = %v, want %v", state.NextRetryAfter, resetAt)
		}
		return
	}

	t.Fatalf("expected 1 hook result, got %d", len(hook.snapshot()))
}
