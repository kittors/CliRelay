package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type openAIResponsesStreamErrorChunk struct {
	Type           string `json:"type"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	SequenceNumber int    `json:"sequence_number"`
}

// openAIResponsesStreamFailedEvent is the terminal event a Responses client
// needs to stop waiting for a reply. An `error` event alone is not terminal for
// every client: Codex reports "stream closed before response.completed" and
// discards the real reason when the stream ends on an `error` frame.
type openAIResponsesStreamFailedEvent struct {
	Type     string `json:"type"`
	Response struct {
		ID        string                           `json:"id,omitempty"`
		Object    string                           `json:"object"`
		CreatedAt int64                            `json:"created_at,omitempty"`
		Status    string                           `json:"status"`
		Model     string                           `json:"model,omitempty"`
		Error     openAIResponsesStreamFailedError `json:"error"`
	} `json:"response"`
}

type openAIResponsesStreamFailedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func openAIResponsesStreamErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusRequestTimeout:
		return "request_timeout"
	default:
		if status >= http.StatusInternalServerError {
			return "internal_server_error"
		}
		if status >= http.StatusBadRequest {
			return "invalid_request_error"
		}
		return "unknown_error"
	}
}

// BuildOpenAIResponsesStreamErrorChunk builds an OpenAI Responses streaming error chunk.
//
// Important: OpenAI's HTTP error bodies are shaped like {"error":{...}}; those are valid for
// non-streaming responses, but streaming clients validate SSE `data:` payloads against a union
// of chunks that requires a top-level `type` field.
func BuildOpenAIResponsesStreamErrorChunk(status int, errText string, sequenceNumber int) []byte {
	code, message, parsedSequence := openAIResponsesStreamErrorDetails(status, errText)
	if sequenceNumber <= 0 {
		sequenceNumber = parsedSequence
	}

	data, err := json.Marshal(openAIResponsesStreamErrorChunk{
		Type:           "error",
		Code:           code,
		Message:        message,
		SequenceNumber: sequenceNumber,
	})
	if err == nil {
		return data
	}

	// Extremely defensive fallback.
	data, _ = json.Marshal(openAIResponsesStreamErrorChunk{
		Type:           "error",
		Code:           "internal_server_error",
		Message:        message,
		SequenceNumber: sequenceNumber,
	})
	if len(data) > 0 {
		return data
	}
	return []byte(`{"type":"error","code":"internal_server_error","message":"internal error","sequence_number":0}`)
}

// openAIResponsesStreamErrorDetails derives the client-facing code and message
// of a streaming failure from either an HTTP status or an upstream error body.
func openAIResponsesStreamErrorDetails(status int, errText string) (string, string, int) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	sequenceNumber := 0

	message := strings.TrimSpace(errText)
	if message == "" {
		message = http.StatusText(status)
	}

	code := openAIResponsesStreamErrorCode(status)

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		var payload map[string]any
		if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
			if t, ok := payload["type"].(string); ok && strings.TrimSpace(t) == "error" {
				if m, ok := payload["message"].(string); ok && strings.TrimSpace(m) != "" {
					message = strings.TrimSpace(m)
				}
				if v, ok := payload["code"]; ok && v != nil {
					if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
						code = strings.TrimSpace(c)
					} else {
						code = strings.TrimSpace(fmt.Sprint(v))
					}
				}
				if v, ok := payload["sequence_number"].(float64); ok {
					sequenceNumber = int(v)
				}
			}
			if e, ok := payload["error"].(map[string]any); ok {
				if m, ok := e["message"].(string); ok && strings.TrimSpace(m) != "" {
					message = strings.TrimSpace(m)
				}
				if v, ok := e["code"]; ok && v != nil {
					if c, ok := v.(string); ok && strings.TrimSpace(c) != "" {
						code = strings.TrimSpace(c)
					} else {
						code = strings.TrimSpace(fmt.Sprint(v))
					}
				}
			}
		}
	}

	if strings.TrimSpace(code) == "" {
		code = "unknown_error"
	}
	// Codex and other Responses clients recognise an over-long prompt by this
	// code, whatever words the upstream used.
	if isContextLengthExceeded(status, parseUpstreamError(errText)) {
		code = "context_length_exceeded"
	}
	return code, message, sequenceNumber
}

// BuildOpenAIResponsesStreamFailedChunk builds a terminal `response.failed`
// event so a Responses client ends the turn with the real upstream reason
// instead of its generic "stream closed before response.completed" message.
// responseID/model identify the reply created earlier in the stream so clients
// can match the failure to it; empty values are omitted.
func BuildOpenAIResponsesStreamFailedChunk(status int, errText, responseID, model string, createdAt int64, sequenceNumber int) []byte {
	code, message, parsedSequence := openAIResponsesStreamErrorDetails(status, errText)
	if sequenceNumber <= 0 {
		sequenceNumber = parsedSequence
	}

	event := openAIResponsesStreamFailedEvent{Type: "response.failed"}
	event.Response.ID = strings.TrimSpace(responseID)
	event.Response.Object = "response"
	event.Response.CreatedAt = createdAt
	event.Response.Status = "failed"
	event.Response.Model = strings.TrimSpace(model)
	event.Response.Error = openAIResponsesStreamFailedError{Code: code, Message: message}

	data, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	return data
}
