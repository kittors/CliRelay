// Package claude provides response translation functionality for Codex to Claude Code API compatibility.
// This package handles the conversion of Codex API responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertCodexResponseToClaudeParams holds parameters for response conversion.
type ConvertCodexResponseToClaudeParams struct {
	HasToolCall               bool
	BlockIndex                int
	HasReceivedArgumentsDelta bool
	OpenBlockType             string
	OpenBlockIndex            int
	ActiveFunctionCallKeys    map[string]struct{}
	MessageCompleted          bool
}

const (
	codexClaudeThinkingBlock = "thinking"
	codexClaudeTextBlock     = "text"
	codexClaudeToolBlock     = "tool_use"
)

// ConvertCodexResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates Codex API responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - []string: A slice of strings, each containing a Claude Code-compatible JSON response
func ConvertCodexResponseToClaude(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &ConvertCodexResponseToClaudeParams{
			HasToolCall: false,
			BlockIndex:  0,
		}
	}
	params := (*param).(*ConvertCodexResponseToClaudeParams)
	if params.MessageCompleted {
		return []string{}
	}

	// log.Debugf("rawJSON: %s", string(rawJSON))
	if !bytes.HasPrefix(rawJSON, dataTag) {
		return []string{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	output := ""
	rootResult := gjson.ParseBytes(rawJSON)
	typeResult := rootResult.Get("type")
	typeStr := typeResult.String()
	template := ""
	if typeStr == "response.created" {
		template = `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"claude-opus-4-1-20250805","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`
		template, _ = sjson.Set(template, "message.model", rootResult.Get("response.model").String())
		template, _ = sjson.Set(template, "message.id", rootResult.Get("response.id").String())

		output = "event: message_start\n"
		output += fmt.Sprintf("data: %s\n\n", template)
	} else if typeStr == "response.reasoning_summary_part.added" {
		if params.OpenBlockType == codexClaudeThinkingBlock {
			return []string{output}
		}
		output += closeCodexClaudeContentBlock(params)
		template = `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		openCodexClaudeContentBlock(params, codexClaudeThinkingBlock)

		output += "event: content_block_start\n"
		output += fmt.Sprintf("data: %s\n\n", template)
	} else if typeStr == "response.reasoning_summary_text.delta" {
		if params.OpenBlockType == "" {
			template = `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			openCodexClaudeContentBlock(params, codexClaudeThinkingBlock)
			output += "event: content_block_start\n"
			output += fmt.Sprintf("data: %s\n\n", template)
		}
		if params.OpenBlockType != codexClaudeThinkingBlock {
			return []string{output}
		}
		template = `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
		template, _ = sjson.Set(template, "index", params.OpenBlockIndex)
		template, _ = sjson.Set(template, "delta.thinking", rootResult.Get("delta").String())

		output += "event: content_block_delta\n"
		output += fmt.Sprintf("data: %s\n\n", template)
	} else if typeStr == "response.reasoning_summary_part.done" {
		if params.OpenBlockType == codexClaudeThinkingBlock {
			output += closeCodexClaudeContentBlock(params)
		}
	} else if typeStr == "response.content_part.added" {
		if params.OpenBlockType == codexClaudeTextBlock {
			return []string{output}
		}
		output += closeCodexClaudeContentBlock(params)
		template = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		openCodexClaudeContentBlock(params, codexClaudeTextBlock)

		output += "event: content_block_start\n"
		output += fmt.Sprintf("data: %s\n\n", template)
	} else if typeStr == "response.output_text.delta" {
		if params.OpenBlockType == codexClaudeThinkingBlock {
			output += closeCodexClaudeContentBlock(params)
		}
		if params.OpenBlockType == "" {
			template = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			openCodexClaudeContentBlock(params, codexClaudeTextBlock)
			output += "event: content_block_start\n"
			output += fmt.Sprintf("data: %s\n\n", template)
		}
		if params.OpenBlockType != codexClaudeTextBlock {
			return []string{output}
		}
		template = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
		template, _ = sjson.Set(template, "index", params.OpenBlockIndex)
		template, _ = sjson.Set(template, "delta.text", rootResult.Get("delta").String())

		output += "event: content_block_delta\n"
		output += fmt.Sprintf("data: %s\n\n", template)
	} else if typeStr == "response.content_part.done" {
		if params.OpenBlockType == codexClaudeTextBlock {
			output += closeCodexClaudeContentBlock(params)
		}
	} else if typeStr == "response.completed" || typeStr == "response.incomplete" {
		output += closeCodexClaudeContentBlock(params)
		template = `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
		template, _ = sjson.Set(template, "delta.stop_reason", codexClaudeStopReason(rootResult.Get("response"), params.HasToolCall))
		inputTokens, outputTokens, cachedTokens := extractResponsesUsage(rootResult.Get("response.usage"))
		template, _ = sjson.Set(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.Set(template, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			template, _ = sjson.Set(template, "usage.cache_read_input_tokens", cachedTokens)
		}

		output += "event: message_delta\n"
		output += fmt.Sprintf("data: %s\n\n", template)
		output += "event: message_stop\n"
		output += `data: {"type":"message_stop"}`
		output += "\n\n"
		params.MessageCompleted = true
	} else if typeStr == "response.output_item.added" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "function_call" {
			name := strings.TrimSpace(itemResult.Get("name").String())
			rev := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)
			if orig, ok := rev[name]; ok {
				name = strings.TrimSpace(orig)
			}
			callID := strings.TrimSpace(itemResult.Get("call_id").String())
			if callID == "" {
				callID = strings.TrimSpace(itemResult.Get("id").String())
			}
			if name == "" || callID == "" {
				return []string{output}
			}

			keys := codexClaudeFunctionCallKeys(rootResult, itemResult)
			if params.OpenBlockType == codexClaudeToolBlock && codexClaudeFunctionCallMatches(params, keys) {
				return []string{output}
			}
			output += closeCodexClaudeContentBlock(params)
			params.HasToolCall = true
			params.HasReceivedArgumentsDelta = false
			template = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			template, _ = sjson.Set(template, "content_block.id", callID)
			template, _ = sjson.Set(template, "content_block.name", name)
			openCodexClaudeContentBlock(params, codexClaudeToolBlock)
			params.ActiveFunctionCallKeys = keys

			output += "event: content_block_start\n"
			output += fmt.Sprintf("data: %s\n\n", template)

			template = `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
			template, _ = sjson.Set(template, "index", params.OpenBlockIndex)

			output += "event: content_block_delta\n"
			output += fmt.Sprintf("data: %s\n\n", template)
		}
	} else if typeStr == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		if itemType == "function_call" {
			if params.OpenBlockType != codexClaudeToolBlock || !codexClaudeFunctionCallMatches(params, codexClaudeFunctionCallKeys(rootResult, itemResult)) {
				return []string{output}
			}
			if !params.HasReceivedArgumentsDelta {
				if args := itemResult.Get("arguments").String(); args != "" {
					output += codexClaudeFunctionArgumentsDelta(params.OpenBlockIndex, args)
					params.HasReceivedArgumentsDelta = true
				}
			}
			output += closeCodexClaudeContentBlock(params)
		}
	} else if typeStr == "response.function_call_arguments.delta" {
		if params.OpenBlockType == codexClaudeToolBlock && codexClaudeFunctionCallMatches(params, codexClaudeFunctionCallKeys(rootResult, gjson.Result{})) {
			params.HasReceivedArgumentsDelta = true
			output += codexClaudeFunctionArgumentsDelta(params.OpenBlockIndex, rootResult.Get("delta").String())
		}
	} else if typeStr == "response.function_call_arguments.done" {
		// Some models (e.g. gpt-5.3-codex-spark) send function call arguments
		// in a single "done" event without preceding "delta" events.
		// Emit the full arguments as a single input_json_delta so the
		// downstream Claude client receives the complete tool input.
		// When delta events were already received, skip to avoid duplicating arguments.
		if params.OpenBlockType == codexClaudeToolBlock && codexClaudeFunctionCallMatches(params, codexClaudeFunctionCallKeys(rootResult, gjson.Result{})) && !params.HasReceivedArgumentsDelta {
			if args := rootResult.Get("arguments").String(); args != "" {
				output += codexClaudeFunctionArgumentsDelta(params.OpenBlockIndex, args)
				params.HasReceivedArgumentsDelta = true
			}
		}
	}

	return []string{output}
}

func openCodexClaudeContentBlock(params *ConvertCodexResponseToClaudeParams, blockType string) {
	params.OpenBlockType = blockType
	params.OpenBlockIndex = params.BlockIndex
}

func closeCodexClaudeContentBlock(params *ConvertCodexResponseToClaudeParams) string {
	if params.OpenBlockType == "" {
		return ""
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", params.OpenBlockIndex)
	if params.BlockIndex <= params.OpenBlockIndex {
		params.BlockIndex = params.OpenBlockIndex + 1
	}
	params.OpenBlockType = ""
	params.OpenBlockIndex = 0
	params.ActiveFunctionCallKeys = nil
	params.HasReceivedArgumentsDelta = false
	return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
}

func codexClaudeFunctionArgumentsDelta(blockIndex int, arguments string) string {
	template := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
	template, _ = sjson.Set(template, "index", blockIndex)
	template, _ = sjson.Set(template, "delta.partial_json", arguments)
	return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)
}

func codexClaudeFunctionCallKeys(rootResult, itemResult gjson.Result) map[string]struct{} {
	keys := make(map[string]struct{}, 4)
	if outputIndex := strings.TrimSpace(rootResult.Get("output_index").String()); outputIndex != "" {
		keys["output:"+outputIndex] = struct{}{}
	}
	if itemID := strings.TrimSpace(rootResult.Get("item_id").String()); itemID != "" {
		keys["item:"+itemID] = struct{}{}
	}
	if itemID := strings.TrimSpace(itemResult.Get("id").String()); itemID != "" {
		keys["item:"+itemID] = struct{}{}
	}
	if callID := strings.TrimSpace(rootResult.Get("call_id").String()); callID != "" {
		keys["call:"+callID] = struct{}{}
	}
	if callID := strings.TrimSpace(itemResult.Get("call_id").String()); callID != "" {
		keys["call:"+callID] = struct{}{}
	}
	return keys
}

func codexClaudeFunctionCallMatches(params *ConvertCodexResponseToClaudeParams, eventKeys map[string]struct{}) bool {
	if len(params.ActiveFunctionCallKeys) == 0 || len(eventKeys) == 0 {
		return true
	}
	for _, prefix := range []string{"output:", "item:", "call:"} {
		activeValue, activeOK := codexClaudeFunctionCallKeyValue(params.ActiveFunctionCallKeys, prefix)
		eventValue, eventOK := codexClaudeFunctionCallKeyValue(eventKeys, prefix)
		if activeOK && eventOK && activeValue != eventValue {
			return false
		}
	}
	return true
}

func codexClaudeFunctionCallKeyValue(keys map[string]struct{}, prefix string) (string, bool) {
	for key := range keys {
		if strings.HasPrefix(key, prefix) {
			return key, true
		}
	}
	return "", false
}

// ConvertCodexResponseToClaudeNonStream converts a non-streaming Codex response to a non-streaming Claude Code response.
// This function processes the complete Codex response and transforms it into a single Claude Code-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Claude Code API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - string: A Claude Code-compatible JSON response containing all message content and metadata
func ConvertCodexResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, _ *any) string {
	revNames := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)

	rootResult := gjson.ParseBytes(rawJSON)
	if typ := rootResult.Get("type").String(); typ != "response.completed" && typ != "response.incomplete" {
		return ""
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return ""
	}

	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", responseData.Get("id").String())
	out, _ = sjson.Set(out, "model", responseData.Get("model").String())
	inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.Set(out, "usage.cache_read_input_tokens", cachedTokens)
	}

	hasToolCall := false

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				thinkingBuilder := strings.Builder{}
				if summary := item.Get("summary"); summary.Exists() {
					if summary.IsArray() {
						summary.ForEach(func(_, part gjson.Result) bool {
							if txt := part.Get("text"); txt.Exists() {
								thinkingBuilder.WriteString(txt.String())
							} else {
								thinkingBuilder.WriteString(part.String())
							}
							return true
						})
					} else {
						thinkingBuilder.WriteString(summary.String())
					}
				}
				if thinkingBuilder.Len() == 0 {
					if content := item.Get("content"); content.Exists() {
						if content.IsArray() {
							content.ForEach(func(_, part gjson.Result) bool {
								if txt := part.Get("text"); txt.Exists() {
									thinkingBuilder.WriteString(txt.String())
								} else {
									thinkingBuilder.WriteString(part.String())
								}
								return true
							})
						} else {
							thinkingBuilder.WriteString(content.String())
						}
					}
				}
				if thinkingBuilder.Len() > 0 {
					block := `{"type":"thinking","thinking":""}`
					block, _ = sjson.Set(block, "thinking", thinkingBuilder.String())
					out, _ = sjson.SetRaw(out, "content.-1", block)
				}
			case "message":
				if content := item.Get("content"); content.Exists() {
					if content.IsArray() {
						content.ForEach(func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								text := part.Get("text").String()
								if text != "" {
									block := `{"type":"text","text":""}`
									block, _ = sjson.Set(block, "text", text)
									out, _ = sjson.SetRaw(out, "content.-1", block)
								}
							}
							return true
						})
					} else {
						text := content.String()
						if text != "" {
							block := `{"type":"text","text":""}`
							block, _ = sjson.Set(block, "text", text)
							out, _ = sjson.SetRaw(out, "content.-1", block)
						}
					}
				}
			case "function_call":
				name := strings.TrimSpace(item.Get("name").String())
				if original, ok := revNames[name]; ok {
					name = strings.TrimSpace(original)
				}
				callID := strings.TrimSpace(item.Get("call_id").String())
				if callID == "" {
					callID = strings.TrimSpace(item.Get("id").String())
				}
				if name == "" || callID == "" {
					return true
				}
				hasToolCall = true

				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", callID)
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
			}
			return true
		})
	}

	out, _ = sjson.Set(out, "stop_reason", codexClaudeStopReason(responseData, hasToolCall))

	if stopSequence := responseData.Get("stop_sequence"); stopSequence.Exists() && stopSequence.String() != "" {
		out, _ = sjson.SetRaw(out, "stop_sequence", stopSequence.Raw)
	}

	return out
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0
	}

	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}

	return inputTokens, outputTokens, cachedTokens
}

// buildReverseMapFromClaudeOriginalShortToOriginal builds a map[short]original from original Claude request tools.
func buildReverseMapFromClaudeOriginalShortToOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func ClaudeTokenCount(ctx context.Context, count int64) string {
	return fmt.Sprintf(`{"input_tokens":%d}`, count)
}

// codexClaudeStopReason is the Anthropic stop_reason for a finished Responses
// response. An incomplete response says why it stopped early, which wins: a
// reply cut off in the middle of a tool call stops for max_tokens, as
// Anthropic's own would. Otherwise a reply that called tools stops for tool use.
// Some Responses-compatible upstreams add a stop_reason field of their own:
// one of Anthropic's names goes on as it is (stop_sequence among them), and
// OpenAI's are mapped; anything else ends the turn.
func codexClaudeStopReason(response gjson.Result, hasToolCall bool) string {
	reason := translatorcommon.StopReasonFromResponses(response.Get("status").String(), response.Get("incomplete_details.reason").String())
	if reason != translatorcommon.StopEndTurn {
		return reason.Claude()
	}
	if hasToolCall {
		return translatorcommon.StopToolUse.Claude()
	}
	native := response.Get("stop_reason").String()
	switch reason = translatorcommon.StopReasonFromClaude(native); {
	case reason == translatorcommon.StopToolUse:
		// A stop_reason naming tool use with no tool call in the reply.
		return translatorcommon.StopEndTurn.Claude()
	case translatorcommon.IsClaudeStopReason(native):
		return native
	}
	return reason.Claude()
}
