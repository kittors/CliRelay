package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:               err.Code,
		Message:            err.Message,
		Retryable:          err.Retryable,
		HTTPStatus:         err.HTTPStatus,
		QuotaWindow:        err.QuotaWindow,
		QuotaWindowMinutes: err.QuotaWindowMinutes,
	}
}

func errorFromExecution(err error) *Error {
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		result := cloneError(authErr)
		if result.Message == "" {
			result.Message = err.Error()
		}
		type quotaWindowProvider interface {
			QuotaWindow() (string, int)
		}
		var qwp quotaWindowProvider
		if errors.As(err, &qwp) && qwp != nil && result.QuotaWindow == "" && result.QuotaWindowMinutes == 0 {
			result.QuotaWindow, result.QuotaWindowMinutes = qwp.QuotaWindow()
		}
		return result
	}

	result := &Error{Message: err.Error()}
	if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
		result.HTTPStatus = se.StatusCode()
	}
	type quotaWindowProvider interface {
		QuotaWindow() (string, int)
	}
	var qwp quotaWindowProvider
	if errors.As(err, &qwp) && qwp != nil {
		result.QuotaWindow, result.QuotaWindowMinutes = qwp.QuotaWindow()
	}
	return result
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	rap, ok := err.(retryAfterProvider)
	if !ok || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	return new(*retryAfter)
}

func headersFromError(err error) http.Header {
	if err == nil {
		return nil
	}
	type headerProvider interface {
		Headers() http.Header
	}
	var hp headerProvider
	if errors.As(err, &hp) && hp != nil {
		headers := hp.Headers()
		if len(headers) == 0 {
			return nil
		}
		return headers.Clone()
	}
	return nil
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried or failed over to another auth. Switching
// credentials cannot fix a malformed request body (for example an image that is
// too small for the upstream vision API).
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest {
		return false
	}

	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		if isInvalidRequestSignal(authErr.Code) || isInvalidRequestSignal(authErr.Message) {
			return true
		}
	}

	message := strings.TrimSpace(err.Error())
	if message == "" {
		return false
	}
	if isInvalidRequestSignal(message) {
		return true
	}

	var payload map[string]any
	if !json.Valid([]byte(message)) {
		return false
	}
	if errParse := json.Unmarshal([]byte(message), &payload); errParse != nil {
		return false
	}
	// Providers use different shapes:
	// - OpenAI-style: {"error":{"type":"invalid_request_error",...}}
	// - xAI-style:    {"code":"invalid-argument","error":"..."}
	// - Google-style: {"error":{"status":"INVALID_ARGUMENT","message":"..."}}
	signals := []string{
		stringValue(payload["code"]),
		stringValue(payload["type"]),
		stringValue(payload["status"]),
		stringValue(payload["detail"]),
		stringValue(payload["error"]),
		stringValue(payload["message"]),
		nestedString(payload, "error", "type"),
		nestedString(payload, "error", "code"),
		nestedString(payload, "error", "status"),
		nestedString(payload, "error", "message"),
	}
	for _, signal := range signals {
		if isInvalidRequestSignal(signal) {
			return true
		}
	}
	return false
}

func isInvalidRequestSignal(raw string) bool {
	signal := strings.ToLower(strings.TrimSpace(raw))
	if signal == "" {
		return false
	}
	switch {
	case strings.Contains(signal, "invalid_request_error"),
		strings.Contains(signal, "invalid-argument"),
		strings.Contains(signal, "invalid_argument"),
		strings.Contains(signal, "invalid request"),
		strings.Contains(signal, "model is not supported"),
		strings.Contains(signal, "model not supported"),
		strings.Contains(signal, "not supported when using codex with a chatgpt account"),
		// Upstream vision APIs reject undersized images with a stable message.
		// This is request content, not auth/quota, so failover cannot help.
		strings.Contains(signal, "below the minimum of") && strings.Contains(signal, "pixels"),
		strings.Contains(signal, "total pixels") && strings.Contains(signal, "minimum"),
		// Prompt/context length is a property of the request body. Retrying on
		// another auth only re-runs multi-MB translation without fixing the body.
		// Besides the phrasings IsContextLengthExceeded knows, looser ones are
		// enough here: the status is already a 400.
		IsContextLengthExceeded(signal),
		strings.Contains(signal, "too many tokens"),
		strings.Contains(signal, "tokens > max"),
		strings.Contains(signal, "tokens exceeds"),
		strings.Contains(signal, "prompt length") && strings.Contains(signal, "tokens"):
		return true
	default:
		return false
	}
}

// contextLengthPhrases are what upstreams say when a request does not fit the
// model's context window, for example:
//
//   - OpenAI, vLLM, DeepSeek: "This model's maximum context length is 128000
//     tokens. However, your messages resulted in 130512 tokens." (code
//     context_length_exceeded)
//   - Anthropic: "prompt is too long: 210450 tokens > 200000 maximum"
//   - Codex: "Your input exceeds the context window of this model."
//   - Gemini: "The input token count (1048577) exceeds the maximum number of
//     tokens allowed (1048576)."
//   - Bedrock: "Input is too long for requested model."
//   - Grok: "This model's maximum prompt length is 500000 but the request
//     contains 816381 tokens."
var contextLengthPhrases = []string{
	"context_length_exceeded",
	"prompt_too_long",
	"prompt is too long",
	"prompt too long",
	"maximum context length",
	"maximum prompt length",
	"exceeds the context window",
	"exceeds the maximum number of tokens",
	"input is too long",
}

// IsContextLengthExceeded reports whether an upstream error text says the
// request does not fit the model's context window. No other account can serve
// such a request, and clients react to it by shortening the conversation, so
// the API handlers rewrite it into the wording each protocol's clients
// recognise.
func IsContextLengthExceeded(text string) bool {
	signal := strings.ToLower(text)
	for _, phrase := range contextLengthPhrases {
		if strings.Contains(signal, phrase) {
			return true
		}
	}
	return strings.Contains(signal, "maximum prompt") && strings.Contains(signal, "tokens")
}

func nestedString(payload map[string]any, keys ...string) string {
	if len(payload) == 0 || len(keys) == 0 {
		return ""
	}
	current := any(payload)
	for _, key := range keys {
		asMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = asMap[key]
	}
	return stringValue(current)
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []byte:
		return strings.TrimSpace(string(typed))
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
