package chat_completions

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func messageArrayFixtureForTest(calls int, textBytes int) []byte {
	messages := []any{map[string]any{"role": "system", "content": "Synthetic system instruction."}}
	for i := 0; i < calls; i++ {
		id := fmt.Sprint("call", i)
		messages = append(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "inspect", "arguments": "{}"}}}})
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat("x", textBytes)})
	}
	body, _ := json.Marshal(map[string]any{"messages": messages, "max_completion_tokens": 16384, "reasoning_effort": "medium"})
	return body
}

func TestClaudeMessageArrayBoundedCopies(t *testing.T) {
	body := messageArrayFixtureForTest(64, 4096)
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = ConvertOpenAIRequestToClaude("claude-opus-5-5", body, true)
		}
	})
	if n := result.AllocedBytesPerOp(); n > int64(len(body))*8 {
		t.Fatalf("message translation allocates %d bytes for %d-byte input; growing full-request copies exceed eight input sizes", n, len(body))
	}
}

func TestClaudeMessageArrayLegacyBytes(t *testing.T) {
	fixtures := [][]byte{
		messageArrayFixtureForTest(100, 256),
		[]byte(`{"messages":[{"role":"user","content":"before"},{"role":"system","content":"first"},{"role":"assistant","content":"between","reasoning_content":"retained thought"},{"role":"system","content":[{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"ignored"}},{"type":"text","text":""}]},{"role":"tool","tool_call_id":"call1","content":[{"type":"text","text":"opaque"}]},{"role":"system","content":"third"}],"temperature":0,"top_p":0.7,"stop":["stop"],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object"}}}],"tool_choice":"required"}`),
		[]byte(`{"messages":[{"role":"system","content":null},{"role":"system","content":""},{"role":"developer","content":"ignored"},{"role":"assistant","tool_calls":[{"id":"id1","type":"function","function":{"name":"inspect","arguments":"not-json"}}]},{"role":"user","content":[{"type":"text","text":"quote\"<é"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}],"max_tokens":42,"stream":false}`),
		[]byte(`{"messages":[{"role":"unknown","content":"ignored"}]}`),
		[]byte(`{"messages":[]}`), []byte(`{}`), []byte(`{"messages":null}`),
		[]byte(`{"messages":[{"role":"user","content":"valid"}],"messages":[{"role":"system","content":"ignored duplicate"}]}`),
		[]byte(`{"messages":[{"role":"system","content":"partial"},`),
	}
	for i, body := range fixtures {
		for _, stream := range []bool{false, true} {
			before := bytes.Clone(body)
			want := legacyMessageArrayTranslatorForTest("claude-opus-5-5", body, stream)
			got := ConvertOpenAIRequestToClaude("claude-opus-5-5", body, stream)
			if !bytes.Equal(want, got) {
				t.Fatalf("fixture %d stream %t differs from legacy", i, stream)
			}
			if !bytes.Equal(before, body) {
				t.Fatalf("fixture %d mutated input", i)
			}
		}
	}
}

func TestClaudeMessageArrayGeneratedLegacyParity(t *testing.T) {
	rng := mathrand.New(mathrand.NewSource(35))
	roles := []string{"system", "user", "assistant", "tool", "developer", "unknown"}
	for trial := 0; trial < 200; trial++ {
		var messages []any
		for i := 0; i < rng.Intn(64); i++ {
			role := roles[rng.Intn(len(roles))]
			msg := map[string]any{"role": role}
			switch rng.Intn(4) {
			case 0:
				msg["content"] = "quote\"<é " + strings.Repeat("x", rng.Intn(128))
			case 1:
				msg["content"] = []any{map[string]any{"type": "text", "text": "part1"}, map[string]any{"type": "unknown", "opaque": "keep"}, map[string]any{"type": "text", "text": "part2"}}
			case 2:
				msg["content"] = nil
			}
			if role == "assistant" {
				msg["reasoning_content"] = "synthetic thought"
				msg["tool_calls"] = []any{map[string]any{"type": "function", "id": fmt.Sprint("trial", trial, "call", i), "function": map[string]any{"name": "inspect", "arguments": "{\"x\":1}"}}}
			}
			if role == "tool" {
				msg["tool_call_id"] = fmt.Sprint("tool", i)
			}
			messages = append(messages, msg)
		}
		body, _ := json.Marshal(map[string]any{"messages": messages, "reasoning_effort": "medium", "max_tokens": 16384, "unknown": "opaque"})
		before := bytes.Clone(body)
		for _, stream := range []bool{false, true} {
			want := legacyMessageArrayTranslatorForTest("custom-model", body, stream)
			got := ConvertOpenAIRequestToClaude("custom-model", body, stream)
			if !bytes.Equal(want, got) {
				t.Fatalf("trial %d stream %t differs", trial, stream)
			}
		}
		if !bytes.Equal(before, body) {
			t.Fatalf("trial %d mutated input", trial)
		}
	}
}

func BenchmarkClaudeMessageArrayHistory(b *testing.B) {
	for _, n := range []int{1, 32, 128, 512} {
		body := messageArrayFixtureForTest(n, 4096)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				_ = ConvertOpenAIRequestToClaude("claude-opus-5-5", body, true)
			}
		})
	}
}

func BenchmarkClaudeMessageArrayLegacyHistory(b *testing.B) {
	for _, n := range []int{1, 32, 128, 512} {
		body := messageArrayFixtureForTest(n, 4096)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				_ = legacyMessageArrayTranslatorForTest("claude-opus-5-5", body, true)
			}
		})
	}
}

func legacyMessageArrayTranslatorForTest(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON

	if account == "" {
		u, _ := uuid.NewRandom()
		account = u.String()
	}
	if session == "" {
		u, _ := uuid.NewRandom()
		session = u.String()
	}
	if user == "" {
		sum := sha256.Sum256([]byte(account + session))
		user = hex.EncodeToString(sum[:])
	}
	userID := fmt.Sprintf("user_%s_account_%s_session_%s", user, account, session)

	// Base Claude Code API template with default max_tokens value
	out := fmt.Sprintf(`{"model":"","max_tokens":32000,"messages":[],"metadata":{"user_id":"%s"}}`, userID)

	root := gjson.ParseBytes(rawJSON)

	// Convert OpenAI reasoning_effort to Claude thinking config.
	if v := root.Get("reasoning_effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			budget, ok := thinking.ConvertLevelToBudget(effort)
			if ok {
				switch budget {
				case 0:
					out, _ = sjson.Set(out, "thinking.type", "disabled")
				case -1:
					out, _ = sjson.Set(out, "thinking.type", "enabled")
				default:
					if budget > 0 {
						out, _ = sjson.Set(out, "thinking.type", "enabled")
						out, _ = sjson.Set(out, "thinking.budget_tokens", budget)
					}
				}
			}
		}
	}

	// Helper for generating tool call IDs in the form: toolu_<alphanum>
	// This ensures unique identifiers for tool calls in the Claude Code format
	genToolCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		// 24 chars random suffix for uniqueness
		for i := 0; i < 24; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "toolu_" + b.String()
	}

	// Model mapping to specify which Claude Code model to use
	out, _ = sjson.Set(out, "model", modelName)

	// Max tokens configuration with fallback to the legacy max_tokens field.
	if maxTokens := root.Get("max_completion_tokens"); maxTokens.Exists() {
		out, _ = sjson.Set(out, "max_tokens", maxTokens.Int())
	} else if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.Set(out, "max_tokens", maxTokens.Int())
	}

	// Temperature setting for controlling response randomness
	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.Set(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() {
		// Top P setting for nucleus sampling (filtered out if temperature is set)
		out, _ = sjson.Set(out, "top_p", topP.Float())
	}

	// Stop sequences configuration for custom termination conditions
	if stop := root.Get("stop"); stop.Exists() {
		if stop.IsArray() {
			var stopSequences []string
			stop.ForEach(func(_, value gjson.Result) bool {
				stopSequences = append(stopSequences, value.String())
				return true
			})
			if len(stopSequences) > 0 {
				out, _ = sjson.Set(out, "stop_sequences", stopSequences)
			}
		} else {
			out, _ = sjson.Set(out, "stop_sequences", []string{stop.String()})
		}
	}

	// Stream configuration to enable or disable streaming responses
	out, _ = sjson.Set(out, "stream", stream)

	// Process messages and transform them to Claude Code format
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		messageIndex := 0
		systemMessageIndex := -1
		messages.ForEach(func(_, message gjson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")

			switch role {
			case "system":
				if systemMessageIndex == -1 {
					systemMsg := `{"role":"user","content":[]}`
					out, _ = sjson.SetRaw(out, "messages.-1", systemMsg)
					systemMessageIndex = messageIndex
					messageIndex++
				}
				if contentResult.Exists() && contentResult.Type == gjson.String && contentResult.String() != "" {
					textPart := `{"type":"text","text":""}`
					textPart, _ = sjson.Set(textPart, "text", contentResult.String())
					out, _ = sjson.SetRaw(out, fmt.Sprintf("messages.%d.content.-1", systemMessageIndex), textPart)
				} else if contentResult.Exists() && contentResult.IsArray() {
					contentResult.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "text" {
							textPart := `{"type":"text","text":""}`
							textPart, _ = sjson.Set(textPart, "text", part.Get("text").String())
							out, _ = sjson.SetRaw(out, fmt.Sprintf("messages.%d.content.-1", systemMessageIndex), textPart)
						}
						return true
					})
				}
			case "user", "assistant":
				msg := `{"role":"","content":[]}`
				msg, _ = sjson.Set(msg, "role", role)

				if role == "assistant" {
					if reasoning := message.Get("reasoning_content"); reasoning.Exists() {
						reasoningText := reasoning.String()
						if strings.TrimSpace(reasoningText) != "" {
							thinkingPart := `{"type":"thinking","thinking":""}`
							thinkingPart, _ = sjson.Set(thinkingPart, "thinking", reasoningText)
							msg, _ = sjson.SetRaw(msg, "content.-1", thinkingPart)
						}
					}
				}

				// Handle content based on its type (string or array)
				if contentResult.Exists() && contentResult.Type == gjson.String && contentResult.String() != "" {
					part := `{"type":"text","text":""}`
					part, _ = sjson.Set(part, "text", contentResult.String())
					msg, _ = sjson.SetRaw(msg, "content.-1", part)
				} else if contentResult.Exists() && contentResult.IsArray() {
					contentResult.ForEach(func(_, part gjson.Result) bool {
						partType := part.Get("type").String()

						switch partType {
						case "text":
							textPart := `{"type":"text","text":""}`
							textPart, _ = sjson.Set(textPart, "text", part.Get("text").String())
							msg, _ = sjson.SetRaw(msg, "content.-1", textPart)

						case "image_url":
							// Convert OpenAI image format to Claude Code format
							imageURL := part.Get("image_url.url").String()
							if strings.HasPrefix(imageURL, "data:") {
								// Extract base64 data and media type from data URL
								parts := strings.Split(imageURL, ",")
								if len(parts) == 2 {
									mediaTypePart := strings.Split(parts[0], ";")[0]
									mediaType := strings.TrimPrefix(mediaTypePart, "data:")
									data := parts[1]

									imagePart := `{"type":"image","source":{"type":"base64","media_type":"","data":""}}`
									imagePart, _ = sjson.Set(imagePart, "source.media_type", mediaType)
									imagePart, _ = sjson.Set(imagePart, "source.data", data)
									msg, _ = sjson.SetRaw(msg, "content.-1", imagePart)
								}
							}

						case "file":
							fileData := part.Get("file.file_data").String()
							if strings.HasPrefix(fileData, "data:") {
								semicolonIdx := strings.Index(fileData, ";")
								commaIdx := strings.Index(fileData, ",")
								if semicolonIdx != -1 && commaIdx != -1 && commaIdx > semicolonIdx {
									mediaType := strings.TrimPrefix(fileData[:semicolonIdx], "data:")
									data := fileData[commaIdx+1:]
									docPart := `{"type":"document","source":{"type":"base64","media_type":"","data":""}}`
									docPart, _ = sjson.Set(docPart, "source.media_type", mediaType)
									docPart, _ = sjson.Set(docPart, "source.data", data)
									msg, _ = sjson.SetRaw(msg, "content.-1", docPart)
								}
							}
						}
						return true
					})
				}

				// Handle tool calls (for assistant messages)
				if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() && role == "assistant" {
					toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
						if toolCall.Get("type").String() == "function" {
							toolCallID := toolCall.Get("id").String()
							if toolCallID == "" {
								toolCallID = genToolCallID()
							}

							function := toolCall.Get("function")
							toolUse := `{"type":"tool_use","id":"","name":"","input":{}}`
							toolUse, _ = sjson.Set(toolUse, "id", toolCallID)
							toolUse, _ = sjson.Set(toolUse, "name", function.Get("name").String())

							// Parse arguments for the tool call
							if args := function.Get("arguments"); args.Exists() {
								argsStr := args.String()
								if argsStr != "" && gjson.Valid(argsStr) {
									argsJSON := gjson.Parse(argsStr)
									if argsJSON.IsObject() {
										toolUse, _ = sjson.SetRaw(toolUse, "input", argsJSON.Raw)
									} else {
										toolUse, _ = sjson.SetRaw(toolUse, "input", "{}")
									}
								} else {
									toolUse, _ = sjson.SetRaw(toolUse, "input", "{}")
								}
							} else {
								toolUse, _ = sjson.SetRaw(toolUse, "input", "{}")
							}

							msg, _ = sjson.SetRaw(msg, "content.-1", toolUse)
						}
						return true
					})
				}

				out, _ = sjson.SetRaw(out, "messages.-1", msg)
				messageIndex++

			case "tool":
				// Handle tool result messages conversion
				toolCallID := message.Get("tool_call_id").String()

				msg := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"","content":""}]}`
				msg, _ = sjson.Set(msg, "content.0.tool_use_id", toolCallID)
				msg, _ = sjson.SetRaw(msg, "content.0.content", common.ToClaudeContent(common.ParseToolResult(message.Get("content"))))
				out, _ = sjson.SetRaw(out, "messages.-1", msg)
				messageIndex++
			}
			return true
		})
	}

	// Tools mapping: OpenAI tools -> Claude Code tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		hasAnthropicTools := false
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "function" {
				function := tool.Get("function")
				anthropicTool := `{"name":"","description":""}`
				anthropicTool, _ = sjson.Set(anthropicTool, "name", function.Get("name").String())
				anthropicTool, _ = sjson.Set(anthropicTool, "description", function.Get("description").String())

				// Convert parameters schema for the tool
				if parameters := function.Get("parameters"); parameters.Exists() {
					anthropicTool, _ = sjson.SetRaw(anthropicTool, "input_schema", parameters.Raw)
				} else if parameters := function.Get("parametersJsonSchema"); parameters.Exists() {
					anthropicTool, _ = sjson.SetRaw(anthropicTool, "input_schema", parameters.Raw)
				}

				out, _ = sjson.SetRaw(out, "tools.-1", anthropicTool)
				hasAnthropicTools = true
			}
			return true
		})

		if !hasAnthropicTools {
			out, _ = sjson.Delete(out, "tools")
		}
	}

	// Tool choice mapping from OpenAI format to Claude Code format
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		switch toolChoice.Type {
		case gjson.String:
			choice := toolChoice.String()
			switch choice {
			case "none":
				// Leaving tool_choice unset would not stop tool use: Anthropic defaults to
				// auto whenever tools are present. Omitted when no tools are sent: Anthropic
				// rejects a tool_choice without tools, and "none" is already its default then.
				if gjson.Get(out, "tools.#").Int() > 0 {
					out, _ = sjson.SetRaw(out, "tool_choice", `{"type":"none"}`)
				}
			case "auto":
				out, _ = sjson.SetRaw(out, "tool_choice", `{"type":"auto"}`)
			case "required":
				out, _ = sjson.SetRaw(out, "tool_choice", `{"type":"any"}`)
			}
		case gjson.JSON:
			// Specific tool choice mapping
			if toolChoice.Get("type").String() == "function" {
				functionName := toolChoice.Get("function.name").String()
				toolChoiceJSON := `{"type":"tool","name":""}`
				toolChoiceJSON, _ = sjson.Set(toolChoiceJSON, "name", functionName)
				out, _ = sjson.SetRaw(out, "tool_choice", toolChoiceJSON)
			}
		default:
		}
	}

	return []byte(out)
}
