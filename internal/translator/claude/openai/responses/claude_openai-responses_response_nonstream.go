package responses

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeResponseToOpenAIResponsesNonStream aggregates Claude SSE into a single OpenAI Responses JSON.
func ConvertClaudeResponseToOpenAIResponsesNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) string {
	// Aggregate Claude SSE lines into a single OpenAI Responses JSON (non-stream)
	// We follow the same aggregation logic as the streaming variant but produce
	// one final object matching docs/out.json structure.

	// Collect SSE data: lines start with "data: "; ignore others
	var chunks [][]byte
	{
		// Use a simple scanner to iterate through raw bytes
		// Note: extremely large responses may require increasing the buffer
		scanner := bufio.NewScanner(bytes.NewReader(rawJSON))
		buf := make([]byte, 52_428_800) // 50MB
		scanner.Buffer(buf, 52_428_800)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, dataTag) {
				continue
			}
			chunks = append(chunks, line[len(dataTag):])
		}
	}

	// Base OpenAI Responses (non-stream) object
	out := `{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"incomplete_details":null,"output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{},"total_tokens":0}}`

	// Aggregation state
	var (
		responseID      string
		createdAt       int64
		currentMsgID    string
		currentFCID     string
		textBuf         strings.Builder
		textSeen        bool
		textOutputIndex int
		reasoningBuf    strings.Builder
		reasoningActive bool
		reasoningItemID string
		reasoningSig    string
		reasoningChars  int
		inputTokens     int64
		outputTokens    int64
	)
	textOutputIndex = -1
	// outputItems is deliberately populated as Claude closes content blocks.
	// Responses output is an ordered sequence; rebuilding it by type (reasoning,
	// message, then tools) changes the meaning of interleaved thinking and calls.
	var outputItems []string

	// Per-index tool call aggregation
	type toolState struct {
		id   string
		name string
		args strings.Builder
	}
	toolCalls := make(map[int]*toolState)

	// Walk through SSE chunks to fill state
	for _, ch := range chunks {
		root := gjson.ParseBytes(ch)
		ev := root.Get("type").String()

		switch ev {
		case "message_start":
			if msg := root.Get("message"); msg.Exists() {
				responseID = msg.Get("id").String()
				createdAt = time.Now().Unix()
				if usage := msg.Get("usage"); usage.Exists() {
					inputTokens = usage.Get("input_tokens").Int()
				}
			}

		case "content_block_start":
			cb := root.Get("content_block")
			if !cb.Exists() {
				continue
			}
			idx := int(root.Get("index").Int())
			typ := cb.Get("type").String()
			switch typ {
			case "text":
				currentMsgID = "msg_" + responseID + "_0"
				if !textSeen {
					textSeen = true
					// Reserve the message's position at the first text block. Text
					// blocks are retained as one assistant message for compatibility,
					// but that message stays at its source position relative to tools
					// and reasoning items.
					textOutputIndex = len(outputItems)
					outputItems = append(outputItems, "")
				}
			case "tool_use":
				currentFCID = cb.Get("id").String()
				name := cb.Get("name").String()
				if toolCalls[idx] == nil {
					toolCalls[idx] = &toolState{id: currentFCID, name: name}
				} else {
					toolCalls[idx].id = currentFCID
					toolCalls[idx].name = name
				}
			case "thinking":
				reasoningActive = true
				reasoningItemID = fmt.Sprintf("rs_%s_%d", responseID, idx)
				reasoningBuf.Reset()
				reasoningSig = cb.Get("signature").String()
			case "redacted_thinking":
				item := claudeReasoningItem(fmt.Sprintf("rs_%s_%d", responseID, idx), "", claudeReasoningCarrier(cb))
				outputItems = append(outputItems, item)
			}

		case "content_block_delta":
			d := root.Get("delta")
			if !d.Exists() {
				continue
			}
			dt := d.Get("type").String()
			switch dt {
			case "text_delta":
				if t := d.Get("text"); t.Exists() {
					textBuf.WriteString(t.String())
				}
			case "input_json_delta":
				if pj := d.Get("partial_json"); pj.Exists() {
					idx := int(root.Get("index").Int())
					if toolCalls[idx] == nil {
						toolCalls[idx] = &toolState{}
					}
					toolCalls[idx].args.WriteString(pj.String())
				}
			case "thinking_delta":
				if reasoningActive {
					if t := d.Get("thinking"); t.Exists() {
						reasoningBuf.WriteString(t.String())
					}
				}
			case "signature_delta":
				if reasoningActive {
					if sig := d.Get("signature"); sig.Exists() && sig.String() != "" {
						reasoningSig = sig.String()
					}
				}
			}

		case "content_block_stop":
			if reasoningActive {
				outputItems = append(outputItems, claudeReasoningItem(reasoningItemID, reasoningBuf.String(), reasoningSig))
				reasoningChars += reasoningBuf.Len()
				reasoningActive = false
				reasoningBuf.Reset()
				reasoningSig = ""
			} else if idxState := toolCalls[int(root.Get("index").Int())]; idxState != nil && idxState.id != "" {
				// A tool item is finalized at its content_block_stop, preserving
				// its source position in outputItems.
				args := idxState.args.String()
				if args == "" {
					args = "{}"
				}
				item := `{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`
				item, _ = sjson.Set(item, "id", fmt.Sprintf("fc_%s", idxState.id))
				item, _ = sjson.Set(item, "arguments", args)
				item, _ = sjson.Set(item, "call_id", idxState.id)
				item, _ = sjson.Set(item, "name", idxState.name)
				outputItems = append(outputItems, item)
				// Mark the state as closed so malformed duplicate stop events do
				// not duplicate the output item.
				idxState.id = ""
			}

		case "message_delta":
			if usage := root.Get("usage"); usage.Exists() {
				outputTokens = usage.Get("output_tokens").Int()
			}
		}
	}

	// Populate base fields
	out, _ = sjson.Set(out, "id", responseID)
	out, _ = sjson.Set(out, "created_at", createdAt)

	// Inject request echo fields as top-level (similar to streaming variant)
	reqBytes := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	if len(reqBytes) > 0 {
		req := gjson.ParseBytes(reqBytes)
		if v := req.Get("instructions"); v.Exists() {
			out, _ = sjson.Set(out, "instructions", v.String())
		}
		if v := req.Get("max_output_tokens"); v.Exists() {
			out, _ = sjson.Set(out, "max_output_tokens", v.Int())
		}
		if v := req.Get("max_tool_calls"); v.Exists() {
			out, _ = sjson.Set(out, "max_tool_calls", v.Int())
		}
		if v := req.Get("model"); v.Exists() {
			out, _ = sjson.Set(out, "model", v.String())
		}
		if v := req.Get("parallel_tool_calls"); v.Exists() {
			out, _ = sjson.Set(out, "parallel_tool_calls", v.Bool())
		}
		if v := req.Get("previous_response_id"); v.Exists() {
			out, _ = sjson.Set(out, "previous_response_id", v.String())
		}
		if v := req.Get("prompt_cache_key"); v.Exists() {
			out, _ = sjson.Set(out, "prompt_cache_key", v.String())
		}
		if v := req.Get("reasoning"); v.Exists() {
			out, _ = sjson.Set(out, "reasoning", v.Value())
		}
		if v := req.Get("safety_identifier"); v.Exists() {
			out, _ = sjson.Set(out, "safety_identifier", v.String())
		}
		if v := req.Get("service_tier"); v.Exists() {
			out, _ = sjson.Set(out, "service_tier", v.String())
		}
		if v := req.Get("store"); v.Exists() {
			out, _ = sjson.Set(out, "store", v.Bool())
		}
		if v := req.Get("temperature"); v.Exists() {
			out, _ = sjson.Set(out, "temperature", v.Float())
		}
		if v := req.Get("text"); v.Exists() {
			out, _ = sjson.Set(out, "text", v.Value())
		}
		if v := req.Get("tool_choice"); v.Exists() {
			out, _ = sjson.Set(out, "tool_choice", v.Value())
		}
		if v := req.Get("tools"); v.Exists() {
			out, _ = sjson.Set(out, "tools", v.Value())
		}
		if v := req.Get("top_logprobs"); v.Exists() {
			out, _ = sjson.Set(out, "top_logprobs", v.Int())
		}
		if v := req.Get("top_p"); v.Exists() {
			out, _ = sjson.Set(out, "top_p", v.Float())
		}
		if v := req.Get("truncation"); v.Exists() {
			out, _ = sjson.Set(out, "truncation", v.String())
		}
		if v := req.Get("user"); v.Exists() {
			out, _ = sjson.Set(out, "user", v.Value())
		}
		if v := req.Get("metadata"); v.Exists() {
			out, _ = sjson.Set(out, "metadata", v.Value())
		}
	}

	// Build output array in Claude source order. The text placeholder reserved
	// at its first block is filled after all deltas have been consumed.
	outputsWrapper := `{"arr":[]}`
	if reasoningActive && reasoningBuf.Len() > 0 {
		item := claudeReasoningItem(reasoningItemID, reasoningBuf.String(), reasoningSig)
		outputItems = append(outputItems, item)
		reasoningChars += reasoningBuf.Len()
	}
	if textSeen {
		item := `{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`
		item, _ = sjson.Set(item, "id", currentMsgID)
		item, _ = sjson.Set(item, "content.0.text", textBuf.String())
		outputItems[textOutputIndex] = item
	}
	for _, item := range outputItems {
		if item != "" {
			outputsWrapper, _ = sjson.SetRaw(outputsWrapper, "arr.-1", item)
		}
	}
	if gjson.Get(outputsWrapper, "arr.#").Int() > 0 {
		out, _ = sjson.SetRaw(out, "output", gjson.Get(outputsWrapper, "arr").Raw)
	}

	// Usage
	total := inputTokens + outputTokens
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)
	out, _ = sjson.Set(out, "usage.total_tokens", total)
	if reasoningChars > 0 {
		// Rough estimate similar to chat completions
		reasoningTokens := int64(reasoningChars / 4)
		if reasoningTokens > 0 {
			out, _ = sjson.Set(out, "usage.output_tokens_details.reasoning_tokens", reasoningTokens)
		}
	}

	return out
}
