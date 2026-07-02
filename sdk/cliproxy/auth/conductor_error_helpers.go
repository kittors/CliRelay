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
// error that should not be retried. Specifically, it checks for 400 Bad Request
// with "invalid_request_error" in the message, indicating the request itself is
// malformed and switching to a different auth will not help.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest {
		return false
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return false
	}
	if strings.Contains(message, "invalid_request_error") {
		return true
	}
	lowerMessage := strings.ToLower(message)
	if strings.Contains(lowerMessage, "model is not supported") || strings.Contains(lowerMessage, "model not supported") {
		return true
	}
	if strings.Contains(lowerMessage, "not supported when using codex with a chatgpt account") {
		return true
	}

	var payload map[string]any
	if !json.Valid([]byte(message)) {
		return false
	}
	if errParse := json.Unmarshal([]byte(message), &payload); errParse != nil {
		return false
	}
	lowerDetail := strings.ToLower(firstNonEmptyString(
		nestedString(payload, "error", "type"),
		nestedString(payload, "error", "code"),
		nestedString(payload, "error", "message"),
		stringValue(payload["detail"]),
	))
	if strings.Contains(lowerDetail, "invalid_request_error") {
		return true
	}
	if strings.Contains(lowerDetail, "model is not supported") || strings.Contains(lowerDetail, "model not supported") {
		return true
	}
	if strings.Contains(lowerDetail, "not supported when using codex with a chatgpt account") {
		return true
	}
	return false
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
