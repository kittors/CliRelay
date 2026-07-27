// Package claude provides request translation functionality for Claude Code API compatibility.
// It handles parsing and transforming Claude Code API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude Code API format and the internal client's expected format.
package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToCodex parses and transforms a Claude Code API request into the internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
// The function performs the following transformations:
// 1. Sets up a template with the model name and empty instructions field
// 2. Processes system messages and converts them to developer input content
// 3. Transforms message contents (text, image, tool_use, tool_result) to appropriate formats
// 4. Converts tools declarations to the expected format
// 5. Adds additional configuration parameters for the Codex API
// 6. Maps Claude thinking configuration to Codex reasoning settings
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in internal client format
func ConvertClaudeRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	rootResult := gjson.ParseBytes(rawJSON)

	// Build input/tools once. Never sjson.SetRaw(template, "input.-1") in a loop —
	// that full-document-copies multi-MB histories on every message (production RSS spike).
	var inputBuf bytes.Buffer
	inputBuf.WriteByte('[')
	inputFirst := true
	appendInput := func(raw string) {
		if raw == "" {
			return
		}
		if !inputFirst {
			inputBuf.WriteByte(',')
		}
		inputFirst = false
		inputBuf.WriteString(raw)
	}

	// Convert Claude's top-level system prompt to Responses API instructions.
	// Do not emit system/developer messages inside input because the Codex
	// ChatGPT OAuth upstream rejects them ("System messages are not allowed").
	var instructionParts []string
	systemsResult := rootResult.Get("system")
	switch {
	case systemsResult.IsArray():
		for _, systemResult := range systemsResult.Array() {
			if systemResult.Get("type").String() != "text" {
				continue
			}
			if text := systemResult.Get("text").String(); text != "" {
				instructionParts = append(instructionParts, text)
			}
		}
	case systemsResult.Type == gjson.String:
		if text := systemsResult.String(); text != "" {
			instructionParts = append(instructionParts, text)
		}
	}
	// Note: instructions is assembled after the message loop below, because
	// in-conversation system/developer messages are also folded into it.

	// Build tool short-name map once for tool_use renames (avoid rescanning tools per call).
	toolNameMap := buildReverseMapFromClaudeOriginalToShort(rawJSON)

	// Process messages and transform their contents to appropriate formats.
	messagesResult := rootResult.Get("messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()

		for i := 0; i < len(messageResults); i++ {
			messageResult := messageResults[i]
			messageRole := messageResult.Get("role").String()

			// Some clients (e.g. Claude Code Agent Teams) inject system/developer
			// messages inside the messages array. The Codex ChatGPT OAuth upstream
			// rejects those roles in input ("System messages are not allowed"), so
			// fold their text into the top-level instructions instead of emitting
			// them as input messages.
			if messageRole == "system" || messageRole == "developer" {
				roleContent := messageResult.Get("content")
				if roleContent.IsArray() {
					for _, part := range roleContent.Array() {
						if part.Get("type").String() != "text" {
							continue
						}
						if text := part.Get("text").String(); text != "" {
							instructionParts = append(instructionParts, text)
						}
					}
				} else if roleContent.Type == gjson.String {
					if text := roleContent.String(); text != "" {
						instructionParts = append(instructionParts, text)
					}
				}
				continue
			}

			var contentBuf bytes.Buffer
			contentFirst := true
			hasContent := false
			appendContentPart := func(part string) {
				if part == "" {
					return
				}
				if !contentFirst {
					contentBuf.WriteByte(',')
				}
				contentFirst = false
				contentBuf.WriteString(part)
				hasContent = true
			}

			flushMessage := func() {
				if !hasContent {
					return
				}
				// Assemble message without sjson on large content text.
				var msgBuf bytes.Buffer
				msgBuf.WriteString(`{"type":"message","role":`)
				msgBuf.Write(mustJSONString(messageRole))
				msgBuf.WriteString(`,"content":[`)
				msgBuf.Write(contentBuf.Bytes())
				msgBuf.WriteString(`]}`)
				appendInput(msgBuf.String())
				contentBuf.Reset()
				contentFirst = true
				hasContent = false
			}

			appendTextContentRaw := func(textRaw string) {
				// textRaw is gjson .Raw (quoted JSON string) when available — avoids re-escape copy.
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				var part bytes.Buffer
				part.WriteString(`{"type":"`)
				part.WriteString(partType)
				part.WriteString(`","text":`)
				if strings.HasPrefix(textRaw, `"`) {
					part.WriteString(textRaw)
				} else {
					part.Write(mustJSONString(textRaw))
				}
				part.WriteByte('}')
				appendContentPart(part.String())
			}

			appendImageContent := func(dataURL string) {
				var part bytes.Buffer
				part.WriteString(`{"type":"input_image","image_url":`)
				part.Write(mustJSONString(dataURL))
				part.WriteByte('}')
				appendContentPart(part.String())
			}

			messageContentsResult := messageResult.Get("content")
			if messageContentsResult.IsArray() {
				messageContentResults := messageContentsResult.Array()
				for j := 0; j < len(messageContentResults); j++ {
					messageContentResult := messageContentResults[j]
					contentType := messageContentResult.Get("type").String()

					switch contentType {
					case "text":
						appendTextContentRaw(messageContentResult.Get("text").Raw)
					case "image":
						sourceResult := messageContentResult.Get("source")
						if sourceResult.Exists() {
							data := sourceResult.Get("data").String()
							if data == "" {
								data = sourceResult.Get("base64").String()
							}
							if data != "" {
								mediaType := sourceResult.Get("media_type").String()
								if mediaType == "" {
									mediaType = sourceResult.Get("mime_type").String()
								}
								if mediaType == "" {
									mediaType = "application/octet-stream"
								}
								dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, data)
								appendImageContent(dataURL)
							}
						}
					case "tool_use":
						flushMessage()
						functionCallMessage := `{"type":"function_call"}`
						functionCallMessage, _ = sjson.Set(functionCallMessage, "call_id", messageContentResult.Get("id").String())
						{
							name := messageContentResult.Get("name").String()
							if short, ok := toolNameMap[name]; ok {
								name = short
							} else {
								name = shortenNameIfNeeded(name)
							}
							functionCallMessage, _ = sjson.Set(functionCallMessage, "name", name)
						}
						functionCallMessage, _ = sjson.Set(functionCallMessage, "arguments", messageContentResult.Get("input").Raw)
						appendInput(functionCallMessage)
					case "tool_result":
						flushMessage()
						functionCallOutputMessage := `{"type":"function_call_output"}`
						functionCallOutputMessage, _ = sjson.Set(functionCallOutputMessage, "call_id", messageContentResult.Get("tool_use_id").String())
						functionCallOutputMessage, _ = sjson.Set(functionCallOutputMessage, "output", messageContentResult.Get("content").String())
						appendInput(functionCallOutputMessage)
					}
				}
				flushMessage()
			} else if messageContentsResult.Type == gjson.String {
				appendTextContentRaw(messageContentsResult.Raw)
				flushMessage()
			}
		}
	}
	inputBuf.WriteByte(']')

	// Assemble instructions from top-level system plus any in-conversation
	// system/developer messages collected during the loop above.
	instructions := strings.Join(instructionParts, "\n\n")

	// Convert tools declarations once.
	// Codex rejects defer_loading without tools.tool_search; strip it on each tool
	// fragment only — never N× sjson.DeleteBytes on the full multi-MB request.
	toolsResult := rootResult.Get("tools")

	var toolsBuf bytes.Buffer
	toolsFirst := true
	hasTools := false
	appendTool := func(raw string) {
		if raw == "" {
			return
		}
		if !toolsFirst {
			toolsBuf.WriteByte(',')
		}
		toolsFirst = false
		toolsBuf.WriteString(raw)
		hasTools = true
	}
	if toolsResult.IsArray() && len(toolsResult.Array()) > 0 {
		toolsBuf.WriteByte('[')
		toolResults := toolsResult.Array()
		var names []string
		for i := 0; i < len(toolResults); i++ {
			n := toolResults[i].Get("name").String()
			if n != "" {
				names = append(names, n)
			}
		}
		shortMap := buildShortNameMap(names)
		for i := 0; i < len(toolResults); i++ {
			toolResult := toolResults[i]
			// Special handling: map Claude web search tool to Codex web_search
			if toolResult.Get("type").String() == "web_search_20250305" {
				appendTool(`{"type":"web_search"}`)
				continue
			}
			tool := toolResult.Raw
			tool, _ = sjson.Set(tool, "type", "function")
			if v := toolResult.Get("name"); v.Exists() {
				name := v.String()
				if short, ok := shortMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				tool, _ = sjson.Set(tool, "name", name)
			}
			tool, _ = sjson.SetRaw(tool, "parameters", normalizeToolParameters(toolResult.Get("input_schema").Raw))
			tool, _ = sjson.Delete(tool, "input_schema")
			tool, _ = sjson.Delete(tool, "parameters.$schema")
			// Drop Claude deferred-tool flag from the small tool fragment only.
			if toolResult.Get("defer_loading").Exists() {
				tool, _ = sjson.Delete(tool, "defer_loading")
			}
			tool, _ = sjson.Set(tool, "strict", false)
			appendTool(tool)
		}
		toolsBuf.WriteByte(']')
	}

	// Convert thinking.budget_tokens to reasoning.effort.
	reasoningEffort := "medium"
	if thinkingConfig := rootResult.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
					reasoningEffort = effort
				}
			}
		case "adaptive":
			// Claude adaptive means "enable with max capacity"; keep it as highest level
			// and let ApplyThinking normalize per target model capability.
			reasoningEffort = string(thinking.LevelXHigh)
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	}

	// Pure buffer envelope: never sjson.SetRaw the multi-MB input into a growing template.
	var out bytes.Buffer
	out.Grow(inputBuf.Len() + toolsBuf.Len() + 512)
	out.WriteString(`{"model":`)
	out.Write(mustJSONString(modelName))
	out.WriteString(`,"instructions":`)
	out.Write(mustJSONString(instructions))
	out.WriteString(`,"input":`)
	out.Write(inputBuf.Bytes())
	if hasTools {
		out.WriteString(`,"tools":`)
		out.Write(toolsBuf.Bytes())
		out.WriteString(`,"tool_choice":"auto"`)
	}
	out.WriteString(`,"parallel_tool_calls":true,"reasoning":{"effort":`)
	out.Write(mustJSONString(reasoningEffort))
	out.WriteString(`,"summary":"auto"},"stream":true,"store":false,"include":["reasoning.encrypted_content"]}`)
	return out.Bytes()
}

// shortenNameIfNeeded applies a simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// buildReverseMapFromClaudeOriginalToShort builds original->short map, used to map tool_use names to short.
func buildReverseMapFromClaudeOriginalToShort(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	m := map[string]string{}
	if !tools.IsArray() {
		return m
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
		m = buildShortNameMap(names)
	}
	return m
}

func mustJSONString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return b
}

// normalizeToolParameters ensures object schemas contain at least an empty properties map.
func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	schema := raw
	result := gjson.Parse(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.Set(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRaw(schema, "properties", `{}`)
	}
	return schema
}
