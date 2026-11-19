package claude

import (
	"bytes"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Code below is the translator as it was before the 2026-11-16 rewrite
// (origin/dev 25c4642), kept verbatim except for the function names. The
// equivalence tests compare the current translator against it byte for byte;
// do not change it to match new behaviour.

func convertClaudeRequestToCLIReference(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	rawJSON = bytes.Replace(rawJSON, []byte(`"url":{"type":"string","format":"uri",`), []byte(`"url":{"type":"string",`), -1)

	// Build output Gemini CLI request JSON
	out := `{"model":"","request":{"contents":[]}}`
	out, _ = sjson.Set(out, "model", modelName)

	// system instruction
	if systemResult := gjson.GetBytes(rawJSON, "system"); systemResult.IsArray() {
		systemInstruction := `{"role":"user","parts":[]}`
		hasSystemParts := false
		systemResult.ForEach(func(_, systemPromptResult gjson.Result) bool {
			if systemPromptResult.Get("type").String() == "text" {
				textResult := systemPromptResult.Get("text")
				if textResult.Type == gjson.String {
					part := `{"text":""}`
					part, _ = sjson.Set(part, "text", textResult.String())
					systemInstruction, _ = sjson.SetRaw(systemInstruction, "parts.-1", part)
					hasSystemParts = true
				}
			}
			return true
		})
		if hasSystemParts {
			out, _ = sjson.SetRaw(out, "request.systemInstruction", systemInstruction)
		}
	} else if systemResult.Type == gjson.String {
		out, _ = sjson.Set(out, "request.systemInstruction.parts.-1.text", systemResult.String())
	}

	// contents
	if messagesResult := gjson.GetBytes(rawJSON, "messages"); messagesResult.IsArray() {
		messagesResult.ForEach(func(_, messageResult gjson.Result) bool {
			roleResult := messageResult.Get("role")
			if roleResult.Type != gjson.String {
				return true
			}
			role := roleResult.String()
			if role == "assistant" {
				role = "model"
			}

			contentJSON := `{"role":"","parts":[]}`
			contentJSON, _ = sjson.Set(contentJSON, "role", role)

			contentsResult := messageResult.Get("content")
			if contentsResult.IsArray() {
				contentsResult.ForEach(func(_, contentResult gjson.Result) bool {
					switch contentResult.Get("type").String() {
					case "text":
						part := `{"text":""}`
						part, _ = sjson.Set(part, "text", contentResult.Get("text").String())
						contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)

					case "tool_use":
						functionName := contentResult.Get("name").String()
						functionArgs := contentResult.Get("input").String()
						argsResult := gjson.Parse(functionArgs)
						if argsResult.IsObject() && gjson.Valid(functionArgs) {
							part := `{"thoughtSignature":"","functionCall":{"name":"","args":{}}}`
							part, _ = sjson.Set(part, "thoughtSignature", geminiCLIClaudeThoughtSignature)
							part, _ = sjson.Set(part, "functionCall.name", functionName)
							part, _ = sjson.SetRaw(part, "functionCall.args", functionArgs)
							contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)
						}

					case "tool_result":
						toolCallID := contentResult.Get("tool_use_id").String()
						if toolCallID == "" {
							return true
						}
						funcName := toolCallID
						toolCallIDs := strings.Split(toolCallID, "-")
						if len(toolCallIDs) > 1 {
							funcName = strings.Join(toolCallIDs[0:len(toolCallIDs)-1], "-")
						}
						part := `{"functionResponse":{"name":"","response":{"result":""}}}`
						part, _ = sjson.Set(part, "functionResponse.name", funcName)
						if parsed := translatorcommon.ParseToolResult(contentResult.Get("content")); parsed.HasImage() {
							fr := translatorcommon.SetGeminiFunctionResponse(gjson.Get(part, "functionResponse").Raw, parsed)
							part, _ = sjson.SetRaw(part, "functionResponse", fr)
						} else {
							part, _ = sjson.Set(part, "functionResponse.response.result", contentResult.Get("content").Raw)
						}
						contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)

					case "image":
						source := contentResult.Get("source")
						if source.Get("type").String() == "base64" {
							mimeType := source.Get("media_type").String()
							data := source.Get("data").String()
							if mimeType != "" && data != "" {
								part := `{"inlineData":{"mime_type":"","data":""}}`
								part, _ = sjson.Set(part, "inlineData.mime_type", mimeType)
								part, _ = sjson.Set(part, "inlineData.data", data)
								contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)
							}
						}
					}
					return true
				})
				out, _ = sjson.SetRaw(out, "request.contents.-1", contentJSON)
			} else if contentsResult.Type == gjson.String {
				part := `{"text":""}`
				part, _ = sjson.Set(part, "text", contentsResult.String())
				contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)
				out, _ = sjson.SetRaw(out, "request.contents.-1", contentJSON)
			}
			return true
		})
	}

	// tools
	if toolsResult := gjson.GetBytes(rawJSON, "tools"); toolsResult.IsArray() {
		hasTools := false
		toolsResult.ForEach(func(_, toolResult gjson.Result) bool {
			inputSchemaResult := toolResult.Get("input_schema")
			if inputSchemaResult.Exists() && inputSchemaResult.IsObject() {
				inputSchema := inputSchemaResult.Raw
				tool, _ := sjson.Delete(toolResult.Raw, "input_schema")
				tool, _ = sjson.SetRaw(tool, "parametersJsonSchema", inputSchema)
				tool, _ = sjson.Delete(tool, "strict")
				tool, _ = sjson.Delete(tool, "input_examples")
				tool, _ = sjson.Delete(tool, "type")
				tool, _ = sjson.Delete(tool, "cache_control")
				if gjson.Valid(tool) && gjson.Parse(tool).IsObject() {
					if !hasTools {
						out, _ = sjson.SetRaw(out, "request.tools", `[{"functionDeclarations":[]}]`)
						hasTools = true
					}
					out, _ = sjson.SetRaw(out, "request.tools.0.functionDeclarations.-1", tool)
				}
			}
			return true
		})
		if !hasTools {
			out, _ = sjson.Delete(out, "request.tools")
		}
	}

	// Map Anthropic thinking -> Gemini thinkingBudget/include_thoughts when type==enabled
	if t := gjson.GetBytes(rawJSON, "thinking"); t.Exists() && t.IsObject() {
		switch t.Get("type").String() {
		case "enabled":
			if b := t.Get("budget_tokens"); b.Exists() && b.Type == gjson.Number {
				budget := int(b.Int())
				out, _ = sjson.Set(out, "request.generationConfig.thinkingConfig.thinkingBudget", budget)
				out, _ = sjson.Set(out, "request.generationConfig.thinkingConfig.includeThoughts", true)
			}
		case "adaptive":
			// Keep adaptive as a high level sentinel; ApplyThinking resolves it
			// to model-specific max capability.
			out, _ = sjson.Set(out, "request.generationConfig.thinkingConfig.thinkingLevel", "high")
			out, _ = sjson.Set(out, "request.generationConfig.thinkingConfig.includeThoughts", true)
		}
	}
	if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.temperature", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.topP", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, "request.generationConfig.topK", v.Num)
	}

	outBytes := []byte(out)
	outBytes = translatorcommon.OutputFormatFromClaude(gjson.ParseBytes(rawJSON)).WriteGemini(outBytes, "request.", modelName)
	outBytes = common.AttachDefaultSafetySettings(outBytes, "request.safetySettings")

	return outBytes
}
