package responses

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Code below is the translator as it was before the 2026-11-16 rewrite
// (origin/dev 25c4642), kept verbatim except for the function names. The
// equivalence tests compare the current translator against it byte for byte;
// do not change it to match new behaviour.

func convertOpenAIResponsesRequestToOpenAIChatCompletionsReference(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI chat completions template with default values
	out := `{"model":"","messages":[],"stream":false}`

	root := gjson.ParseBytes(rawJSON)

	// Set model name
	out, _ = sjson.Set(out, "model", modelName)

	// Set stream configuration
	out, _ = sjson.Set(out, "stream", stream)

	// Map generation parameters from responses format to chat completions format
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		out, _ = sjson.Set(out, "max_tokens", maxTokens.Int())
	}

	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
		out, _ = sjson.Set(out, "parallel_tool_calls", parallelToolCalls.Bool())
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := `{"role":"system","content":""}`
		systemMessage, _ = sjson.Set(systemMessage, "content", instructions.String())
		out, _ = sjson.SetRaw(out, "messages.-1", systemMessage)
	}

	// Convert input array to messages
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		// Responses carries reasoning separately from the assistant message or
		// function call it precedes. Keep readable summaries on that assistant
		// turn; encrypted_content is opaque and must not become plaintext.
		var reasoningParts []string
		withReasoning := func(message string) string {
			if len(reasoningParts) > 0 {
				message, _ = sjson.Set(message, "reasoning_content", strings.Join(reasoningParts, "\n"))
				reasoningParts = nil
			}
			return message
		}
		flushReasoning := func() {
			if len(reasoningParts) > 0 {
				message := withReasoning(`{"role":"assistant","content":""}`)
				out, _ = sjson.SetRaw(out, "messages.-1", message)
			}
		}
		input.ForEach(func(_, item gjson.Result) bool {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}

			if itemType != "reasoning" && itemType != "function_call" &&
				!((itemType == "message" || itemType == "") && item.Get("role").String() == "assistant") {
				flushReasoning()
			}
			switch itemType {
			case "reasoning":
				item.Get("summary").ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text"); part.Get("type").String() == "summary_text" && text.Type == gjson.String && text.String() != "" {
						reasoningParts = append(reasoningParts, text.String())
					}
					return true
				})
			case "message", "":
				// Handle regular message conversion
				role := item.Get("role").String()
				if role == "developer" {
					role = "user"
				}
				message := `{"role":"","content":[]}`
				message, _ = sjson.Set(message, "role", role)

				if content := item.Get("content"); content.Exists() && content.IsArray() {
					var messageContent string
					var toolCalls []interface{}

					content.ForEach(func(_, contentItem gjson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						switch contentType {
						case "input_text", "output_text":
							text := contentItem.Get("text").String()
							contentPart := `{"type":"text","text":""}`
							contentPart, _ = sjson.Set(contentPart, "text", text)
							message, _ = sjson.SetRaw(message, "content.-1", contentPart)
						case "input_image":
							imageURL := contentItem.Get("image_url").String()
							contentPart := `{"type":"image_url","image_url":{"url":""}}`
							contentPart, _ = sjson.Set(contentPart, "image_url.url", imageURL)
							message, _ = sjson.SetRaw(message, "content.-1", contentPart)
						}
						return true
					})

					if messageContent != "" {
						message, _ = sjson.Set(message, "content", messageContent)
					}

					if len(toolCalls) > 0 {
						message, _ = sjson.Set(message, "tool_calls", toolCalls)
					}
				} else if content.Type == gjson.String {
					message, _ = sjson.Set(message, "content", content.String())
				}

				message = withReasoning(message)
				out, _ = sjson.SetRaw(out, "messages.-1", message)

			case "function_call":
				// Handle function call conversion to assistant message with tool_calls
				assistantMessage := `{"role":"assistant","tool_calls":[]}`

				toolCall := `{"id":"","type":"function","function":{"name":"","arguments":""}}`

				if callId := item.Get("call_id"); callId.Exists() {
					toolCall, _ = sjson.Set(toolCall, "id", callId.String())
				}

				if name := item.Get("name"); name.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.name", name.String())
				}

				if arguments := item.Get("arguments"); arguments.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.arguments", arguments.String())
				}

				assistantMessage, _ = sjson.SetRaw(assistantMessage, "tool_calls.0", toolCall)
				assistantMessage = withReasoning(assistantMessage)
				out, _ = sjson.SetRaw(out, "messages.-1", assistantMessage)

			case "function_call_output":
				// Handle function call output conversion to tool message
				toolMessage := `{"role":"tool","tool_call_id":"","content":""}`

				if callId := item.Get("call_id"); callId.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "tool_call_id", callId.String())
				}

				if output := item.Get("output"); output.Exists() {
					toolMessage, _ = sjson.SetRaw(toolMessage, "content", common.ToChatContent(common.ParseToolResult(output)))
				}

				out, _ = sjson.SetRaw(out, "messages.-1", toolMessage)
			}

			return true
		})
		flushReasoning()
	} else if input.Type == gjson.String {
		msg := "{}"
		msg, _ = sjson.Set(msg, "role", "user")
		msg, _ = sjson.Set(msg, "content", input.String())
		out, _ = sjson.SetRaw(out, "messages.-1", msg)
	}

	// Convert tools from responses format to chat completions format
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var chatCompletionsTools []interface{}

		tools.ForEach(func(_, tool gjson.Result) bool {
			// Built-in tools (e.g. {"type":"web_search"}) are already compatible with the Chat Completions schema.
			// Only function tools need structural conversion because Chat Completions nests details under "function".
			toolType := tool.Get("type").String()
			if toolType != "" && toolType != "function" && tool.IsObject() {
				// Almost all providers lack built-in tools, so we just ignore them.
				// chatCompletionsTools = append(chatCompletionsTools, tool.Value())
				return true
			}

			chatTool := `{"type":"function","function":{}}`

			// Convert tool structure from responses format to chat completions format
			function := `{"name":"","description":"","parameters":{}}`

			if name := tool.Get("name"); name.Exists() {
				function, _ = sjson.Set(function, "name", name.String())
			}

			if description := tool.Get("description"); description.Exists() {
				function, _ = sjson.Set(function, "description", description.String())
			}

			if parameters := tool.Get("parameters"); parameters.Exists() {
				function, _ = sjson.SetRaw(function, "parameters", parameters.Raw)
			}

			chatTool, _ = sjson.SetRaw(chatTool, "function", function)
			chatCompletionsTools = append(chatCompletionsTools, gjson.Parse(chatTool).Value())

			return true
		})

		if len(chatCompletionsTools) > 0 {
			out, _ = sjson.Set(out, "tools", chatCompletionsTools)
		}
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = sjson.Set(out, "reasoning_effort", effort)
		}
	}

	// Convert tool_choice if present
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		out, _ = sjson.Set(out, "tool_choice", toolChoice.String())
	}

	return common.OutputFormatFromResponses(root).WriteChat([]byte(out))
}
