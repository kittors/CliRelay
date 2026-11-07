package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// ErrorFormat is the error body shape of the API a client called. Clients parse
// errors in their own protocol's shape: Claude Code reads error.type and the
// "prompt is too long" message of an Anthropic error to decide whether to retry
// or compact the conversation, the Gemini SDKs read error.status, and OpenAI
// SDKs read error.code. An upstream of another protocol (Claude Code routed to
// Codex or Gemini, a Chat client routed to Claude) answers in its own shape,
// so the body is rewritten for the client.
type ErrorFormat int

const (
	// ErrorFormatOpenAI is {"error":{"message","type","param","code"}}, for
	// Chat Completions, Completions and Responses.
	ErrorFormatOpenAI ErrorFormat = iota
	// ErrorFormatClaude is {"type":"error","error":{"type","message"}}.
	ErrorFormatClaude
	// ErrorFormatGemini is {"error":{"code","message","status"}}.
	ErrorFormatGemini
)

// upstreamError is what an upstream error body says, whatever its shape.
type upstreamError struct {
	// shape is the format the body already has, or -1 when it has none of
	// them (plain text, other JSON).
	shape   ErrorFormat
	message string
	// code and errType are the machine-readable identifiers the body carries
	// (OpenAI code/type, Anthropic error.type, Gemini status, xAI code).
	code    string
	errType string
	param   string
}

const noErrorFormat ErrorFormat = -1

// parseUpstreamError reads errText, an upstream error body or plain message.
func parseUpstreamError(errText string) upstreamError {
	text := strings.TrimSpace(errText)
	parsed := upstreamError{shape: noErrorFormat, message: text}
	if text == "" || !json.Valid([]byte(text)) {
		return parsed
	}
	root := gjson.Parse(text)
	if !root.IsObject() {
		return parsed
	}
	errObj := root.Get("error")
	switch {
	case root.Get("type").String() == "error" && errObj.IsObject() && errObj.Get("type").Exists():
		parsed.shape = ErrorFormatClaude
		parsed.errType = errObj.Get("type").String()
		parsed.message = errObj.Get("message").String()
	case errObj.IsObject() && errObj.Get("code").Type == gjson.Number && errObj.Get("status").Type == gjson.String:
		parsed.shape = ErrorFormatGemini
		parsed.code = errObj.Get("status").String()
		parsed.message = errObj.Get("message").String()
	case errObj.IsObject() && errObj.Get("message").Exists():
		parsed.shape = ErrorFormatOpenAI
		parsed.message = errObj.Get("message").String()
		parsed.errType = errObj.Get("type").String()
		parsed.code = jsonScalarString(errObj.Get("code"))
		parsed.param = errObj.Get("param").String()
	default:
		// Other shapes: {"message":...}, {"detail":...}, xAI's
		// {"code":...,"error":"..."}, Responses stream errors.
		for _, path := range []string{"error.message", "message", "detail", "error"} {
			if value := root.Get(path); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				parsed.message = value.String()
				break
			}
		}
		parsed.code = jsonScalarString(root.Get("code"))
		parsed.errType = root.Get("type").String()
		if parsed.errType == "error" {
			parsed.errType = ""
		}
	}
	if strings.TrimSpace(parsed.message) == "" {
		parsed.message = text
	}
	return parsed
}

func jsonScalarString(value gjson.Result) string {
	switch value.Type {
	case gjson.String:
		return value.String()
	case gjson.Number:
		return value.Raw
	}
	return ""
}

// FormatErrorBody returns the status and body to send a client of format for
// an upstream error with status and errText.
//
// A body already in the client's shape is returned as it is. Any other body is
// rewritten into that shape, with the upstream's message kept as the message
// (and its code appended when the message does not name it), so clients that
// match on the upstream's text keep working. A request rejected for exceeding
// the model's context window becomes a 400 that each protocol's clients
// recognise: OpenAI code context_length_exceeded, and for Claude a message that
// starts with "prompt is too long", which makes Claude Code compact the
// conversation and retry.
func FormatErrorBody(format ErrorFormat, status int, errText string) (int, []byte) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}
	parsed := parseUpstreamError(errText)
	contextExceeded := isContextLengthExceeded(status, parsed)
	if contextExceeded {
		status = http.StatusBadRequest
	}
	if parsed.shape == format && !needsContextRewrite(format, parsed, contextExceeded) {
		return status, []byte(strings.TrimSpace(errText))
	}
	message := strings.TrimSpace(parsed.message)
	// An OpenAI body carries the upstream's code and type in fields of its
	// own; the other shapes have no place for them, so the message names them.
	carried := format == ErrorFormatOpenAI && (parsed.shape == noErrorFormat || parsed.shape == ErrorFormatOpenAI)
	if id := firstNonEmpty(parsed.code, parsed.errType); id != "" && !carried && !isDigits(id) && !strings.Contains(message, id) {
		message += " (" + id + ")"
	}
	switch format {
	case ErrorFormatClaude:
		return status, claudeErrorBody(status, message, contextExceeded)
	case ErrorFormatGemini:
		return status, geminiErrorBody(status, message)
	default:
		return status, openAIErrorBody(status, message, parsed, contextExceeded)
	}
}

// needsContextRewrite reports whether a body already in the client's shape
// still has to be rewritten so that the client recognises an over-long prompt.
func needsContextRewrite(format ErrorFormat, parsed upstreamError, contextExceeded bool) bool {
	if !contextExceeded {
		return false
	}
	switch format {
	case ErrorFormatClaude:
		return !claudeContextMessage(parsed.message)
	case ErrorFormatOpenAI:
		return parsed.code != "context_length_exceeded"
	}
	return false
}

func claudeErrorBody(status int, message string, contextExceeded bool) []byte {
	errType := claudeErrorType(status)
	if contextExceeded {
		errType = "invalid_request_error"
		if !claudeContextMessage(message) {
			message = claudeContextPrefix(message) + message
		}
	}
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": message},
	})
	return body
}

// claudeContextMessage reports whether message already reads the way Claude
// Code detects an over-long prompt.
func claudeContextMessage(message string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(message)), "prompt is too long")
}

// claudeContextPrefix is what goes before an upstream's over-long-prompt
// message for Claude clients. It reads like Anthropic's own "prompt is too
// long: 210450 tokens > 200000 maximum": Claude Code takes the two numbers
// from it to work out how much of the conversation to compact away, so they
// are filled in when the upstream's message gives them.
func claudeContextPrefix(message string) string {
	for _, pattern := range contextTokenCountPatterns {
		actual := pattern.actual.FindStringSubmatch(message)
		limit := pattern.limit.FindStringSubmatch(message)
		if actual == nil || limit == nil {
			continue
		}
		actualTokens, errActual := strconv.ParseInt(actual[1], 10, 64)
		limitTokens, errLimit := strconv.ParseInt(limit[1], 10, 64)
		if errActual == nil && errLimit == nil && actualTokens > limitTokens && limitTokens > 0 {
			return fmt.Sprintf("prompt is too long: %d tokens > %d maximum. ", actualTokens, limitTokens)
		}
	}
	return "prompt is too long: "
}

// contextTokenCountPatterns read the prompt size and the context window out of
// the over-long-prompt messages that carry them.
var contextTokenCountPatterns = []struct{ actual, limit *regexp.Regexp }{
	// OpenAI-compatible: "maximum context length is 128000 tokens. However,
	// your messages resulted in 130512 tokens" (or "you requested").
	{regexp.MustCompile(`(?i)(?:resulted in|requested) (\d+) tokens`), regexp.MustCompile(`(?i)maximum context length is (\d+) tokens`)},
	// Gemini: "The input token count (1048577) exceeds the maximum number of
	// tokens allowed (1048576)."
	{regexp.MustCompile(`(?i)input token count \((\d+)\)`), regexp.MustCompile(`(?i)maximum number of tokens allowed \(?(\d+)`)},
	// Grok: "maximum prompt length is 500000 but the request contains 816381
	// tokens"
	{regexp.MustCompile(`(?i)request contains (\d+) tokens`), regexp.MustCompile(`(?i)maximum prompt length is (\d+)`)},
}

func claudeErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	if status >= http.StatusInternalServerError {
		return "api_error"
	}
	return "invalid_request_error"
}

func geminiErrorBody(status int, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": status, "message": message, "status": geminiErrorStatus(status)},
	})
	return body
}

func geminiErrorStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden, http.StatusPaymentRequired:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "ABORTED"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusNotImplemented:
		return "UNIMPLEMENTED"
	case http.StatusServiceUnavailable, 529:
		return "UNAVAILABLE"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	}
	if status >= http.StatusInternalServerError {
		return "INTERNAL"
	}
	return "FAILED_PRECONDITION"
}

func openAIErrorBody(status int, message string, parsed upstreamError, contextExceeded bool) []byte {
	errType, code := openAIErrorTypeAndCode(status)
	// The upstream's own identifiers win: clients match on them.
	if parsed.shape == noErrorFormat || parsed.shape == ErrorFormatOpenAI {
		if parsed.errType != "" {
			errType = parsed.errType
		}
		if parsed.code != "" {
			code = parsed.code
		}
	}
	if contextExceeded {
		errType, code = "invalid_request_error", "context_length_exceeded"
	}
	detail := map[string]any{"message": message, "type": errType, "code": nilIfEmpty(code)}
	if parsed.param != "" {
		detail["param"] = parsed.param
	} else {
		detail["param"] = nil
	}
	body, _ := json.Marshal(map[string]any{"error": detail})
	return body
}

// openAIErrorTypeAndCode is the type and code BuildErrorResponseBody has always
// used for a status.
func openAIErrorTypeAndCode(status int) (string, string) {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error", "invalid_api_key"
	case http.StatusForbidden:
		return "permission_error", "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_error", "rate_limit_exceeded"
	case http.StatusNotFound:
		return "invalid_request_error", "model_not_found"
	case http.StatusRequestTimeout:
		return "request_timeout", "request_timeout"
	}
	if status >= http.StatusInternalServerError {
		return "server_error", "internal_server_error"
	}
	return "invalid_request_error", ""
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func isDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// isContextLengthExceeded reports whether the upstream rejected the request for
// not fitting the model's context window. A 429 never is one, whatever its
// message says about tokens: it is a rate limit, and a client told otherwise
// would compact its conversation for nothing.
func isContextLengthExceeded(status int, parsed upstreamError) bool {
	if status == http.StatusTooManyRequests {
		return false
	}
	return coreauth.IsContextLengthExceeded(parsed.code + " " + parsed.errType + " " + parsed.message)
}
