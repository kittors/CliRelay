package responses

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponsesRequestToOpenAIChatCompletions converts OpenAI responses format to OpenAI chat completions format.
// It transforms the OpenAI responses API format (with instructions and input array) into the standard
// OpenAI chat completions format (with messages array and system content).
//
// The conversion handles:
// 1. Model name and streaming configuration
// 2. Instructions to system message conversion
// 3. Input array to messages array transformation
// 4. Tool definitions and tool choice conversion
// 5. Function calls and function results handling
// 6. Generation parameters mapping (max_tokens, reasoning, etc.)
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data in OpenAI responses format
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI chat completions format
func ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) []byte {
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
		input.ForEach(func(_, item gjson.Result) bool {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}

			switch itemType {
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

				out, _ = sjson.SetRaw(out, "messages.-1", message)

			case "function_call":
				// Handle function call conversion to assistant message with tool_calls
				assistantMessage := `{"role":"assistant","tool_calls":[]}`

				toolCall := `{"id":"","type":"function","function":{"name":"","arguments":""}}`

				if callId := item.Get("call_id"); callId.Exists() {
					toolCall, _ = sjson.Set(toolCall, "id", callId.String())
				}

				if name := item.Get("name"); name.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.name", responsesLiteChatToolName(item.Get("namespace").String(), name.String()))
				}

				if arguments := item.Get("arguments"); arguments.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.arguments", arguments.String())
				}

				assistantMessage, _ = sjson.SetRaw(assistantMessage, "tool_calls.0", toolCall)
				out, _ = sjson.SetRaw(out, "messages.-1", assistantMessage)

			case "custom_tool_call":
				// Chat Completions has no freeform custom tool call type. Represent it as
				// the function wrapper used for Responses Lite custom tool definitions.
				assistantMessage := `{"role":"assistant","tool_calls":[]}`
				toolCall := `{"id":"","type":"function","function":{"name":"","arguments":""}}`
				toolCall, _ = sjson.Set(toolCall, "id", item.Get("call_id").String())
				toolCall, _ = sjson.Set(toolCall, "function.name", responsesLiteChatToolName(item.Get("namespace").String(), item.Get("name").String()))
				arguments := `{"input":""}`
				arguments, _ = sjson.Set(arguments, "input", item.Get("input").String())
				toolCall, _ = sjson.Set(toolCall, "function.arguments", arguments)
				assistantMessage, _ = sjson.SetRaw(assistantMessage, "tool_calls.0", toolCall)
				out, _ = sjson.SetRaw(out, "messages.-1", assistantMessage)

			case "function_call_output":
				// Handle function call output conversion to tool message
				toolMessage := `{"role":"tool","tool_call_id":"","content":""}`

				if callId := item.Get("call_id"); callId.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "tool_call_id", callId.String())
				}

				if output := item.Get("output"); output.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "content", output.String())
				}

				out, _ = sjson.SetRaw(out, "messages.-1", toolMessage)

			case "custom_tool_call_output":
				toolMessage := `{"role":"tool","tool_call_id":"","content":""}`
				toolMessage, _ = sjson.Set(toolMessage, "tool_call_id", item.Get("call_id").String())
				if output := item.Get("output"); output.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "content", output.String())
				}
				out, _ = sjson.SetRaw(out, "messages.-1", toolMessage)
			}

			return true
		})
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

	// Responses Lite sends model-visible tools in input additional_tools items
	// instead of the top-level tools array. Flatten namespace tools into regular
	// Chat Completions functions so OpenAI-compatible providers can call them.
	out = appendResponsesLiteCompatTools(out, root)

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

	return []byte(out)
}

const responsesLiteDefaultNamespace = "functions"

type responsesLiteCompatTool struct {
	Namespace string
	Name      string
	Kind      string
}

func appendResponsesLiteCompatTools(out string, root gjson.Result) string {
	input := root.Get("input")
	if !input.IsArray() {
		return out
	}

	seen := make(map[string]struct{})
	for _, tool := range gjson.Get(out, "tools").Array() {
		if name := tool.Get("function.name").String(); name != "" {
			seen[name] = struct{}{}
		}
	}

	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "additional_tools" {
			return true
		}
		item.Get("tools").ForEach(func(_, declaredTool gjson.Result) bool {
			namespace := responsesLiteDefaultNamespace
			tools := []gjson.Result{declaredTool}
			if declaredTool.Get("type").String() == "namespace" {
				namespace = declaredTool.Get("name").String()
				tools = declaredTool.Get("tools").Array()
			}
			for _, tool := range tools {
				chatTool, chatName, ok := responsesLiteToolToChat(namespace, tool)
				if !ok {
					continue
				}
				if _, exists := seen[chatName]; exists {
					continue
				}
				out, _ = sjson.SetRaw(out, "tools.-1", chatTool)
				seen[chatName] = struct{}{}
			}
			return true
		})
		return true
	})

	return out
}

func responsesLiteToolToChat(namespace string, tool gjson.Result) (chatTool string, chatName string, ok bool) {
	kind := tool.Get("type").String()
	if kind != "function" && kind != "custom" {
		return "", "", false
	}
	name := tool.Get("name").String()
	if name == "" {
		return "", "", false
	}
	chatName = responsesLiteChatToolName(namespace, name)
	chatTool = `{"type":"function","function":{"name":"","description":"","parameters":{}}}`
	chatTool, _ = sjson.Set(chatTool, "function.name", chatName)
	description := tool.Get("description").String()
	if kind == "custom" {
		if description != "" {
			description += "\n\n"
		}
		description += "Pass the tool's freeform input in the `input` string."
		chatTool, _ = sjson.SetRaw(chatTool, "function.parameters", `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
		chatTool, _ = sjson.Set(chatTool, "function.strict", true)
	} else if parameters := tool.Get("parameters"); parameters.Exists() {
		chatTool, _ = sjson.SetRaw(chatTool, "function.parameters", parameters.Raw)
		if strict := tool.Get("strict"); strict.Exists() {
			chatTool, _ = sjson.Set(chatTool, "function.strict", strict.Bool())
		}
	}
	chatTool, _ = sjson.Set(chatTool, "function.description", description)
	return chatTool, chatName, true
}

func responsesLiteChatToolName(namespace, name string) string {
	if namespace == "" || namespace == responsesLiteDefaultNamespace {
		return name
	}
	if strings.HasSuffix(namespace, "_") || strings.HasPrefix(name, "_") {
		return namespace + name
	}
	return namespace + "__" + name
}

func lookupResponsesLiteCompatTool(rawJSON []byte, chatName string) (responsesLiteCompatTool, bool) {
	root := gjson.ParseBytes(rawJSON)
	for _, item := range root.Get("input").Array() {
		if item.Get("type").String() != "additional_tools" {
			continue
		}
		for _, declaredTool := range item.Get("tools").Array() {
			namespace := responsesLiteDefaultNamespace
			tools := []gjson.Result{declaredTool}
			if declaredTool.Get("type").String() == "namespace" {
				namespace = declaredTool.Get("name").String()
				tools = declaredTool.Get("tools").Array()
			}
			for _, tool := range tools {
				name := tool.Get("name").String()
				kind := tool.Get("type").String()
				if (kind == "function" || kind == "custom") && responsesLiteChatToolName(namespace, name) == chatName {
					return responsesLiteCompatTool{Namespace: namespace, Name: name, Kind: kind}, true
				}
			}
		}
	}
	return responsesLiteCompatTool{}, false
}

func lookupResponsesLiteCompatToolPair(originalRequestRawJSON, requestRawJSON []byte, chatName string) (responsesLiteCompatTool, bool) {
	if tool, ok := lookupResponsesLiteCompatTool(originalRequestRawJSON, chatName); ok {
		return tool, true
	}
	if tool, ok := lookupResponsesLiteCompatTool(requestRawJSON, chatName); ok {
		return tool, true
	}
	return responsesLiteCompatTool{}, false
}

func responsesLiteCustomInput(arguments string) string {
	input := gjson.Get(arguments, "input")
	if input.Exists() && input.Type == gjson.String {
		return input.String()
	}
	return arguments
}
