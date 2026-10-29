package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

type statusQuotaErrorStub struct {
	message      string
	status       int
	quotaWindow  string
	quotaMinutes int
	headers      http.Header
}

func (e *statusQuotaErrorStub) Error() string {
	return e.message
}

func (e *statusQuotaErrorStub) StatusCode() int {
	return e.status
}

func (e *statusQuotaErrorStub) QuotaWindow() (string, int) {
	return e.quotaWindow, e.quotaMinutes
}

func (e *statusQuotaErrorStub) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

func TestErrorFromExecution_ExtractsStatusAndQuotaWindow(t *testing.T) {
	t.Parallel()

	err := &statusQuotaErrorStub{
		message:      "quota exceeded",
		status:       http.StatusTooManyRequests,
		quotaWindow:  "5h",
		quotaMinutes: 300,
	}

	got := errorFromExecution(err)
	if got == nil {
		t.Fatal("errorFromExecution() = nil")
	}
	if got.Message != "quota exceeded" {
		t.Fatalf("Message = %q, want quota exceeded", got.Message)
	}
	if got.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, http.StatusTooManyRequests)
	}
	if got.QuotaWindow != "5h" || got.QuotaWindowMinutes != 300 {
		t.Fatalf("QuotaWindow = %q/%d, want 5h/300", got.QuotaWindow, got.QuotaWindowMinutes)
	}
}

func TestErrorFromExecution_PreservesAuthErrorCode(t *testing.T) {
	t.Parallel()

	err := &Error{
		Code:       "response_stream_incomplete",
		Message:    "upstream responses stream closed before response.completed",
		HTTPStatus: http.StatusBadGateway,
		Retryable:  true,
	}

	got := errorFromExecution(err)
	if got == nil {
		t.Fatal("errorFromExecution() = nil")
	}
	if got == err {
		t.Fatal("errorFromExecution() returned source error pointer, want clone")
	}
	if got.Code != err.Code {
		t.Fatalf("Code = %q, want %q", got.Code, err.Code)
	}
	if got.Message != err.Message {
		t.Fatalf("Message = %q, want %q", got.Message, err.Message)
	}
	if got.HTTPStatus != err.HTTPStatus {
		t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, err.HTTPStatus)
	}
	if !got.Retryable {
		t.Fatal("Retryable = false, want true")
	}
}

func TestHeadersFromError_ClonesHeaders(t *testing.T) {
	t.Parallel()

	err := &statusQuotaErrorStub{
		message: "quota exceeded",
		status:  http.StatusTooManyRequests,
		headers: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
		},
	}

	got := headersFromError(err)
	if got.Get("Anthropic-Ratelimit-Unified-5h-Status") != "rejected" {
		t.Fatalf("headersFromError() = %#v, want rate-limit header", got)
	}
	got.Set("Anthropic-Ratelimit-Unified-5h-Status", "mutated")
	if err.headers.Get("Anthropic-Ratelimit-Unified-5h-Status") != "rejected" {
		t.Fatalf("source headers mutated to %q", err.headers.Get("Anthropic-Ratelimit-Unified-5h-Status"))
	}
}

func TestHeadersFromError_ExtractsHeadersFromWrappedError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("upstream wrapper: %w", &statusQuotaErrorStub{
		message: "quota exceeded",
		status:  http.StatusTooManyRequests,
		headers: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status": []string{"rejected"},
		},
	})

	got := headersFromError(err)
	if got.Get("Anthropic-Ratelimit-Unified-7d-Status") != "rejected" {
		t.Fatalf("headersFromError() = %#v, want wrapped rate-limit header", got)
	}
}

func TestIsRequestInvalidError_RecognizesUnsupportedCodexModelPayload(t *testing.T) {
	t.Parallel()

	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"detail":"The 'gpt-5.1-codex' model is not supported when using Codex with a ChatGPT account."}`,
	}

	if !isRequestInvalidError(err) {
		t.Fatal("expected unsupported codex model payload to be treated as invalid request")
	}
}

func TestIsRequestInvalidError_RecognizesXAIInvalidArgumentImageTooSmall(t *testing.T) {
	t.Parallel()

	// xAI vision rejects undersized images with a top-level code/error payload.
	// Failover to another auth cannot fix the request body.
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"code":"invalid-argument","error":"Image has 420 total pixels (21x20), which is below the minimum of 512 pixels."}`,
	}

	if !isRequestInvalidError(err) {
		t.Fatal("expected xAI invalid-argument image size payload to be treated as invalid request")
	}
}

func TestIsRequestInvalidError_RecognizesInvalidArgumentCodeField(t *testing.T) {
	t.Parallel()

	err := &Error{
		Code:       "invalid-argument",
		HTTPStatus: http.StatusBadRequest,
		Message:    "Image is too small",
	}

	if !isRequestInvalidError(err) {
		t.Fatal("expected explicit invalid-argument code to be treated as invalid request")
	}
}

func TestIsRequestInvalidError_IgnoresNonBadRequest(t *testing.T) {
	t.Parallel()

	err := &Error{
		HTTPStatus: http.StatusBadGateway,
		Message:    "upstream failed",
	}

	if isRequestInvalidError(err) {
		t.Fatal("expected non-400 upstream error to remain retryable/failover-eligible")
	}
}

func TestIsRequestInvalidError_IgnoresGenericBadRequestWithoutInvalidSignal(t *testing.T) {
	t.Parallel()

	// Keep failover for ambiguous 400s that are not clearly request-shaped.
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"message":"temporary provider rejection"}}`,
	}

	if isRequestInvalidError(err) {
		t.Fatal("expected generic 400 without invalid-request signal to remain failover-eligible")
	}
}

func TestIsRequestInvalidError_RecognizesPromptTooLongText(t *testing.T) {
	t.Parallel()

	// Observed Grok/cli-chat-proxy rejection on multi-MB Claude→Codex traffic.
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    "This model's maximum prompt length is 500000 but the request contains 816381 tokens.",
	}
	if !isRequestInvalidError(err) {
		t.Fatal("expected prompt-length 400 to be treated as invalid request (no auth failover)")
	}
}

func TestIsRequestInvalidError_RecognizesContextLengthExceededCode(t *testing.T) {
	t.Parallel()

	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too many tokens"}}`,
	}
	if !isRequestInvalidError(err) {
		t.Fatal("expected context_length_exceeded payload to be treated as invalid request")
	}
}

func TestIsContextLengthExceeded(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
		`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210450 tokens > 200000 maximum"}}`,
		`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`,
		`{"error":{"code":400,"message":"The input token count (1048577) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
		`{"message":"Input is too long for requested model."}`,
		"This model's maximum prompt length is 500000 but the request contains 816381 tokens.",
	} {
		if !IsContextLengthExceeded(text) {
			t.Errorf("IsContextLengthExceeded(%s) = false", text)
		}
	}
	for _, text := range []string{
		`{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","code":"insufficient_quota"}}`,
		`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed the rate limit for your organization of 40,000 input tokens per minute."}}`,
		"invalid request: unknown field",
	} {
		if IsContextLengthExceeded(text) {
			t.Errorf("IsContextLengthExceeded(%s) = true", text)
		}
	}
}

// Bedrock's context error carries no invalid-request type or code, so only its
// message says that no other account can serve the request.
func TestIsRequestInvalidError_RecognizesBedrockInputTooLong(t *testing.T) {
	t.Parallel()

	err := &Error{HTTPStatus: http.StatusBadRequest, Message: `{"message":"Input is too long for requested model."}`}
	if !isRequestInvalidError(err) {
		t.Fatal("expected Bedrock's over-long input 400 to be treated as invalid request")
	}
}

func TestMarkResult_RequestInvalidErrorDoesNotCooldownAuth(t *testing.T) {
	t.Parallel()

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "xai-1",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusBadRequest,
			Message:    `{"code":"invalid-argument","error":"Image has 420 total pixels (21x20), which is below the minimum of 512 pixels."}`,
		},
	})

	got, ok := m.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("auth missing after MarkResult")
	}
	if got.Status != StatusActive {
		t.Fatalf("Status = %q, want %q", got.Status, StatusActive)
	}
	if got.Unavailable {
		t.Fatal("Unavailable = true, want false for request-invalid error")
	}
	if got.Quota.Exceeded {
		t.Fatal("Quota.Exceeded = true, want false")
	}
	if !got.NextRetryAfter.IsZero() {
		t.Fatalf("NextRetryAfter = %v, want zero", got.NextRetryAfter)
	}
	if state := got.ModelStates["grok-4.5"]; state != nil {
		t.Fatalf("ModelStates[grok-4.5] = %+v, want nil/unset for request-invalid error", state)
	}
}
