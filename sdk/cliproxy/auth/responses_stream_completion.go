package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

const (
	responsesStreamCompletedType = "response.completed"
	// responsesStreamIncompleteType ends a reply the upstream cut short
	// (max_output_tokens, content_filter). It is a result to pass on, as the Codex
	// executor and the translators treat it (codexResponseFinished), not a broken
	// stream: counting it as one marked the auth failed and sent the client an
	// error after the reply it had already received.
	responsesStreamIncompleteType = "response.incomplete"
	// responsesStreamDoneType is the name older Responses WebSocket peers give the
	// completed event.
	responsesStreamDoneType   = "response.done"
	responsesStreamFailedType = "response.failed"
	responsesStreamErrorType  = "error"
)

// responsesStreamFinished reports whether eventType ends a Responses stream with
// a reply.
func responsesStreamFinished(eventType string) bool {
	switch eventType {
	case responsesStreamCompletedType, responsesStreamIncompleteType, responsesStreamDoneType:
		return true
	}
	return false
}

type responsesStreamCompletionTracker struct {
	buffer      []byte
	completed   bool
	terminalErr error
}

func newResponsesStreamCompletionTracker(sourceFormat sdktranslator.Format) *responsesStreamCompletionTracker {
	if sourceFormat != sdktranslator.FormatOpenAIResponse {
		return nil
	}
	return &responsesStreamCompletionTracker{}
}

func (t *responsesStreamCompletionTracker) Observe(chunk []byte) {
	if t == nil || t.completed || t.terminalErr != nil || len(chunk) == 0 {
		return
	}
	if completed, err := responsesSSEBlockTerminalState(chunk); completed || err != nil {
		t.completed = completed
		t.terminalErr = err
		return
	}
	t.buffer = append(t.buffer, chunk...)
	for {
		block, rest, ok := takeSSEBlock(t.buffer)
		if !ok {
			return
		}
		t.buffer = rest
		completed, err := responsesSSEBlockTerminalState(block)
		if completed || err != nil {
			t.completed = completed
			t.terminalErr = err
			return
		}
	}
}

func (t *responsesStreamCompletionTracker) ErrIfIncomplete() error {
	if t == nil || t.completed {
		return nil
	}
	if t.terminalErr != nil {
		return t.terminalErr
	}
	if len(bytes.TrimSpace(t.buffer)) > 0 {
		completed, err := responsesSSEBlockTerminalState(t.buffer)
		if completed {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return newResponsesStreamIncompleteError()
}

func newResponsesStreamIncompleteError() *Error {
	return &Error{
		Code:       "response_stream_incomplete",
		Message:    "upstream responses stream closed before response.completed",
		Retryable:  true,
		HTTPStatus: http.StatusBadGateway,
	}
}

func responsesSSEBlockTerminalState(block []byte) (bool, error) {
	for _, event := range parseResponsesSSEEvents(block) {
		completed, err := responsesSSEEventTerminalState(event.eventType, event.data)
		if completed || err != nil {
			return completed, err
		}
	}
	return false, nil
}

func responsesSSEEventTerminalState(eventType string, data string) (bool, error) {
	if responsesStreamFinished(eventType) {
		return true, nil
	}
	if eventType == responsesStreamFailedType || eventType == responsesStreamErrorType {
		if strings.TrimSpace(data) == "" {
			return false, nil
		}
		return false, responsesStreamTerminalError(eventType, data)
	}
	if data == "" || data == "[DONE]" {
		return false, nil
	}
	var payload responsesStreamTerminalPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return false, nil
	}
	payloadType := strings.TrimSpace(payload.Type)
	if responsesStreamFinished(payloadType) {
		return true, nil
	}
	switch payloadType {
	case responsesStreamFailedType, responsesStreamErrorType:
		return false, responsesStreamTerminalError(payloadType, data)
	default:
		return false, nil
	}
}

type responsesSSEEvent struct {
	eventType string
	data      string
}

func takeSSEBlock(buffer []byte) ([]byte, []byte, bool) {
	end, delimLen := firstSSEBlockEnd(buffer)
	if end < 0 {
		return nil, buffer, false
	}
	block := buffer[:end]
	rest := buffer[end+delimLen:]
	return block, rest, true
}

func firstSSEBlockEnd(buffer []byte) (int, int) {
	type delimiter struct {
		value []byte
		len   int
	}
	delimiters := []delimiter{
		{value: []byte("\n\n"), len: 2},
		{value: []byte("\r\n\r\n"), len: 4},
		{value: []byte("\r\r"), len: 2},
	}
	best := -1
	bestLen := 0
	for _, delimiter := range delimiters {
		idx := bytes.Index(buffer, delimiter.value)
		if idx < 0 {
			continue
		}
		if best < 0 || idx < best {
			best = idx
			bestLen = delimiter.len
		}
	}
	return best, bestLen
}

func parseResponsesSSEEvents(block []byte) []responsesSSEEvent {
	events := make([]responsesSSEEvent, 0, 1)
	var eventType string
	var dataLines []string

	flush := func() {
		if eventType == "" && len(dataLines) == 0 {
			return
		}
		events = append(events, responsesSSEEvent{
			eventType: strings.TrimSpace(eventType),
			data:      strings.TrimSpace(strings.Join(dataLines, "\n")),
		})
		eventType = ""
		dataLines = nil
	}

	for _, rawLine := range bytes.Split(block, []byte("\n")) {
		line := strings.TrimSpace(strings.TrimSuffix(string(rawLine), "\r"))
		if line == "" || strings.HasPrefix(line, ":") {
			if line == "" {
				flush()
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			if eventType != "" || len(dataLines) > 0 {
				flush()
			}
			eventType = strings.TrimSpace(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data := strings.TrimSpace(value)
			if eventType == "" && len(dataLines) > 0 && responsesSSEDataLooksStandaloneEvent(strings.Join(dataLines, "\n")) && responsesSSEDataLooksStandaloneEvent(data) {
				flush()
			}
			dataLines = append(dataLines, data)
		}
	}
	flush()
	return events
}

func responsesSSEDataLooksStandaloneEvent(data string) bool {
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		return true
	}
	if !json.Valid([]byte(data)) {
		return false
	}
	var payload responsesStreamTerminalPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload.Type) != "" || strings.TrimSpace(payload.Response.Error.Message) != "" || strings.TrimSpace(payload.Error.Message) != ""
}

type responsesStreamTerminalPayload struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

func responsesStreamTerminalError(eventType string, data string) *Error {
	payload := responsesStreamTerminalPayload{Type: eventType}
	if strings.TrimSpace(data) != "" && json.Valid([]byte(data)) {
		_ = json.Unmarshal([]byte(data), &payload)
	}
	code := firstNonEmpty(payload.Response.Error.Code, payload.Error.Code, payload.Code)
	errType := firstNonEmpty(payload.Response.Error.Type, payload.Error.Type, payload.Type, eventType)
	message := firstNonEmpty(payload.Response.Error.Message, payload.Error.Message, payload.Message)
	if message == "" {
		message = fmt.Sprintf("upstream responses stream ended with %s", firstNonEmpty(payload.Type, eventType, responsesStreamErrorType))
	}
	return &Error{
		Code:       firstNonEmpty(code, errType, "response_stream_terminal_error"),
		Message:    message,
		Retryable:  responsesStreamTerminalRetryable(code, errType),
		HTTPStatus: responsesStreamTerminalHTTPStatus(code, errType),
	}
}

func responsesStreamTerminalHTTPStatus(code string, errType string) int {
	lower := strings.ToLower(code + " " + errType)
	switch {
	case strings.Contains(lower, "rate_limit") || strings.Contains(lower, "quota"):
		return http.StatusTooManyRequests
	case strings.Contains(lower, "invalid_request"):
		return http.StatusBadRequest
	case strings.Contains(lower, "invalid_api_key") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "forbidden") || strings.Contains(lower, "permission"):
		return http.StatusForbidden
	case strings.Contains(lower, "timeout"):
		return http.StatusRequestTimeout
	default:
		return http.StatusBadGateway
	}
}

func responsesStreamTerminalRetryable(code string, errType string) bool {
	status := responsesStreamTerminalHTTPStatus(code, errType)
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= http.StatusInternalServerError
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
