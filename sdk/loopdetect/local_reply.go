package loopdetect

import (
	"encoding/json"
	"fmt"
)

// LocalReply is a finished assistant message the gateway writes itself, in the
// API the client speaks: a text and nothing to call, which ends an agent's turn.
// Nothing is sent upstream for it, so it reports no tokens.
type LocalReply struct {
	Protocol Protocol
	// Model is the model the client asked for, echoed back.
	Model string
	Text  string
	// ID tells this reply from others in the client's logs. The caller makes it
	// unique.
	ID string
	// Created is the time of the reply, in Unix seconds.
	Created int64
}

// JSON is the reply as the body of a non-streaming response. It is nil for a
// protocol the package does not write.
func (r LocalReply) JSON() []byte {
	switch r.Protocol {
	case ProtocolChat:
		return mustJSON(map[string]any{
			"id": "chatcmpl-" + r.ID, "object": "chat.completion", "created": r.Created, "model": r.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": r.Text},
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
	case ProtocolClaude:
		return mustJSON(map[string]any{
			"id": "msg_" + r.ID, "type": "message", "role": "assistant", "model": r.Model,
			"content":     []any{map[string]any{"type": "text", "text": r.Text}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		})
	case ProtocolResponses:
		return mustJSON(r.responsesObject("completed"))
	case ProtocolGemini:
		return mustJSON(r.geminiObject())
	}
	return nil
}

// Stream is the reply as the chunks of a streaming response, in the form the
// executors hand them to the entry handlers: a Claude or Responses event whole
// (`event:` and `data:` lines), a Chat or Gemini object bare. The handlers add
// the framing and the closing marker of their own API. It is nil for a protocol
// the package does not write.
func (r LocalReply) Stream() [][]byte {
	switch r.Protocol {
	case ProtocolChat:
		chunk := func(delta map[string]any, finish any) []byte {
			return mustJSON(map[string]any{
				"id": "chatcmpl-" + r.ID, "object": "chat.completion.chunk", "created": r.Created, "model": r.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			})
		}
		return [][]byte{
			chunk(map[string]any{"role": "assistant", "content": ""}, nil),
			chunk(map[string]any{"content": r.Text}, nil),
			chunk(map[string]any{}, "stop"),
		}
	case ProtocolClaude:
		sse := func(event string, data map[string]any) []byte {
			data["type"] = event
			return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, mustJSON(data)))
		}
		return [][]byte{
			sse("message_start", map[string]any{"message": map[string]any{
				"id": "msg_" + r.ID, "type": "message", "role": "assistant", "model": r.Model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			}}),
			sse("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
			sse("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": r.Text}}),
			sse("content_block_stop", map[string]any{"index": 0}),
			sse("message_delta", map[string]any{
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 0},
			}),
			sse("message_stop", map[string]any{}),
		}
	case ProtocolResponses:
		return r.responsesStream()
	case ProtocolGemini:
		return [][]byte{mustJSON(r.geminiObject())}
	}
	return nil
}

func (r LocalReply) geminiObject() map[string]any {
	return map[string]any{
		"candidates": []any{map[string]any{
			"index": 0, "finishReason": "STOP",
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": r.Text}}},
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 0, "candidatesTokenCount": 0, "totalTokenCount": 0},
		"modelVersion":  r.Model,
		"responseId":    r.ID,
	}
}

func (r LocalReply) responsesItem(status string) map[string]any {
	item := map[string]any{
		"id": "msg_" + r.ID, "type": "message", "status": status, "role": "assistant",
		"content": []any{},
	}
	if status == "completed" {
		item["content"] = []any{r.responsesPart(r.Text)}
	}
	return item
}

func (r LocalReply) responsesPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text}
}

func (r LocalReply) responsesObject(status string) map[string]any {
	obj := map[string]any{
		"id": "resp_" + r.ID, "object": "response", "created_at": r.Created, "status": status,
		"background": false, "error": nil, "model": r.Model, "output": []any{},
	}
	if status == "completed" {
		obj["output"] = []any{r.responsesItem("completed")}
		obj["usage"] = map[string]any{
			"input_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 0, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens": 0,
		}
	}
	return obj
}

// responsesStream is the event sequence of a Responses stream with one message
// in it. The last event, response.completed, carries the whole response: Codex
// reads the turn from it, and the WebSocket entry chains the next turn onto
// its output.
func (r LocalReply) responsesStream() [][]byte {
	var out [][]byte
	seq := 0
	emit := func(event string, data map[string]any) {
		data["type"] = event
		data["sequence_number"] = seq
		seq++
		out = append(out, []byte(fmt.Sprintf("event: %s\ndata: %s", event, mustJSON(data))))
	}
	itemID := "msg_" + r.ID
	emit("response.created", map[string]any{"response": r.responsesObject("in_progress")})
	emit("response.in_progress", map[string]any{"response": r.responsesObject("in_progress")})
	emit("response.output_item.added", map[string]any{"output_index": 0, "item": r.responsesItem("in_progress")})
	emit("response.content_part.added", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "part": r.responsesPart("")})
	emit("response.output_text.delta", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "delta": r.Text, "logprobs": []any{}})
	emit("response.output_text.done", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "text": r.Text, "logprobs": []any{}})
	emit("response.content_part.done", map[string]any{"item_id": itemID, "output_index": 0, "content_index": 0, "part": r.responsesPart(r.Text)})
	emit("response.output_item.done", map[string]any{"output_index": 0, "item": r.responsesItem("completed")})
	emit("response.completed", map[string]any{"response": r.responsesObject("completed")})
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only strings, numbers and nested maps go in; a failure would be a
		// programming error, and an empty body says so to the client.
		return []byte("{}")
	}
	return b
}
