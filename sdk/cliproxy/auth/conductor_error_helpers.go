package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/egressbreaker"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func errorFromExecution(err error) *Error {
	// A failure of the account's proxy is named by its code, so that what
	// records the result can tell it from a failure of the account. The
	// message is the breaker's own: the error an http.Client wraps it in
	// starts with the upstream URL.
	if egressErr, ok := egressbreaker.AsError(err); ok {
		return &Error{Code: EgressUnavailableCode, Message: egressErr.Error()}
	}
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

// IsRequestFault reports whether err is the upstream refusing the request
// itself: a malformed request, a content-policy block, or an upstream safety
// classifier refusing the content. Another account would refuse it as well,
// and so would another model; a virtual model does not move on to its next
// member after one.
func IsRequestFault(err error) bool {
	return isRequestFault(err)
}

// isRequestFault is a failure of the request rather than of the account that
// ran it: it is not failed over to another account, not waited out, and does
// not cool the account down. An upstream content refusal is one even though
// the upstream may call it invalid_request without the usual
// invalid_request_error signal: failing it over would only send the same
// content to more of the pool's accounts.
func isRequestFault(err error) bool {
	return isRequestInvalidError(err) || isContentPolicyViolation(err) || isUpstreamContentRefusal(err)
}

// FailureCause classifies a failed execution as the route trace does an
// attempt (a routetrace Cause* value, a cooldown reason, or empty), for a caller
// that records why it gave up on one.
func FailureCause(err error) string {
	if err == nil {
		return ""
	}
	return failureCause(err)
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried or failed over to another auth. Switching
// credentials cannot fix a malformed request body (for example an image that is
// too small for the upstream vision API).

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
