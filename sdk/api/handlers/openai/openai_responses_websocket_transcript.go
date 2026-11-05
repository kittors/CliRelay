package openai

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// shouldReplaceWebsocketTranscript reports whether an incoming request carries
// the whole conversation rather than the delta that follows the previous turn.
//
// The signalling is the presence of items only a model produces (function_call,
// custom_tool_call, assistant messages): a delta never repeats those, because
// the previous turn's own output is added server-side. Without this check such
// a request is read as an append, and the previous turn's output lands in the
// middle of it.
//
// Developer messages are not a signal: clients such as Codex add them to a
// turn's delta (permission or environment updates), and reading that delta as
// a full transcript would drop the conversation.
func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	// A parent reference means the client is continuing an upstream-side
	// transcript, so its input is a delta by definition.
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) != "" {
		return false
	}
	if !nextInput.Exists() || !nextInput.IsArray() {
		return false
	}

	for _, item := range nextInput.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call":
			return true
		case "message":
			role := strings.TrimSpace(item.Get("role").String())
			if role == "assistant" {
				return true
			}
		}
	}

	return false
}

// normalizeResponseTranscriptReplacement prepares a self-contained request: the
// input is kept as sent, and the fields it omits are inherited from the previous
// request on this socket.
func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		if modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String()); modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		if instructions := gjson.GetBytes(lastRequest, "instructions"); instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return bytes.Clone(normalized)
}
