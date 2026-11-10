// Package openai provides utilities to translate OpenAI Chat Completions
// request JSON into OpenAI Responses API request JSON using gjson/sjson.
// It supports tools, multimodal text/image inputs, and Structured Outputs.
// The package handles the conversion of OpenAI API requests into the format
// expected by the OpenAI Responses API, including proper mapping of messages,
// tools, and generation parameters.
package chat_completions

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIRequestToCodex converts an OpenAI Chat Completions request JSON
// into an OpenAI Responses API request JSON. The transformation follows the
// examples defined in docs/2.md exactly, including tools, multi-turn dialog,
// multimodal text/image handling, and Structured Outputs mapping.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI Chat Completions API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI Responses API format
func ConvertOpenAIRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	// One string copy of the request backs every lookup below. GetBytes would
	// copy each result out of the slice instead, and messages is nearly the
	// whole request.
	rawJSON := string(inputRawJSON)
	fields := readChatRequestFields(rawJSON)

	// Build tool name shortening map from original tools (if any)
	originalToolNameMap := map[string]string{}
	{
		tools := fields.tools
		if tools.IsArray() && len(tools.Array()) > 0 {
			var names []string
			arr := tools.Array()
			for i := 0; i < len(arr); i++ {
				t := arr[i]
				if t.Get("type").String() == "function" {
					fn := t.Get("function")
					if fn.Exists() {
						if v := fn.Get("name"); v.Exists() {
							names = append(names, v.String())
						}
					}
				}
			}
			if len(names) > 0 {
				originalToolNameMap = buildShortNameMap(names)
			}
		}
	}

	// The input array is written once, straight into this buffer, message
	// contents included. Every sjson call re-parses and copies the whole string
	// it edits: appending each content part to its message with sjson copied
	// everything already in the message once per part, so a screenshot early
	// in a turn was copied again for every later part. On 2026-11-10 that took
	// 13–15% of the relay's CPU. The parts themselves are written by hand too
	// (codex_openai_request_json.go): building one with sjson copied its
	// screenshot twice more before it reached the buffer.
	var inputBuf bytes.Buffer
	inputBuf.Grow(len(rawJSON))
	inputBuf.WriteByte('[')
	inputFirst := true
	appendInput := func(raw string) {
		if raw == "" {
			return
		}
		appendArrayElement(&inputBuf, &inputFirst, raw)
	}

	messages := fields.messages
	if messages.IsArray() {
		// ForEach rather than Array, here and for content below: Array finds
		// where each element ends a byte at a time, ForEach eight bytes at a
		// time, and the elements carry the screenshots.
		messages.ForEach(func(_, m gjson.Result) bool {
			role := m.Get("role").String()

			switch role {
			case "tool":
				toolCallID := m.Get("tool_call_id").String()
				funcOutput := `{}`
				funcOutput, _ = sjson.Set(funcOutput, "type", "function_call_output")
				funcOutput, _ = sjson.Set(funcOutput, "call_id", toolCallID)
				funcOutput, _ = sjson.SetRaw(funcOutput, "output", common.ToResponsesOutput(common.ParseToolResult(m.Get("content"))))
				appendInput(funcOutput)

			default:
				head := `{}`
				head, _ = sjson.Set(head, "type", "message")
				if role == "system" {
					head, _ = sjson.Set(head, "role", "developer")
				} else {
					head, _ = sjson.Set(head, "role", role)
				}
				// The content array follows the head minus its closing brace,
				// exactly where sjson would have added the key.
				appendArrayElement(&inputBuf, &inputFirst, head[:len(head)-1])
				inputBuf.WriteString(`,"content":[`)
				partsFirst := true

				c := m.Get("content")
				if c.Exists() && c.Type == gjson.String && c.String() != "" {
					writeTextPart(&inputBuf, &partsFirst, role, c.String())
				} else if c.Exists() && c.IsArray() {
					c.ForEach(func(_, it gjson.Result) bool {
						t := it.Get("type").String()
						switch t {
						case "text":
							writeTextPart(&inputBuf, &partsFirst, role, it.Get("text").String())
						case "image_url":
							if role == "user" {
								startPart(&inputBuf, &partsFirst, "input_image")
								if u := it.Get("image_url.url"); u.Exists() {
									writeStringMember(&inputBuf, "image_url", u.String())
								}
								inputBuf.WriteByte('}')
							}
						case "file":
							if role == "user" {
								fileData := it.Get("file.file_data").String()
								filename := it.Get("file.filename").String()
								if fileData != "" {
									startPart(&inputBuf, &partsFirst, "input_file")
									writeStringMember(&inputBuf, "file_data", fileData)
									if filename != "" {
										writeStringMember(&inputBuf, "filename", filename)
									}
									inputBuf.WriteByte('}')
								}
							}
						}
						return true
					})
				}

				inputBuf.WriteString(`]}`)

				if role == "assistant" {
					toolCalls := m.Get("tool_calls")
					if toolCalls.Exists() && toolCalls.IsArray() {
						toolCallsArr := toolCalls.Array()
						for j := 0; j < len(toolCallsArr); j++ {
							tc := toolCallsArr[j]
							if tc.Get("type").String() == "function" {
								funcCall := `{}`
								funcCall, _ = sjson.Set(funcCall, "type", "function_call")
								funcCall, _ = sjson.Set(funcCall, "call_id", tc.Get("id").String())
								{
									name := tc.Get("function.name").String()
									if short, ok := originalToolNameMap[name]; ok {
										name = short
									} else {
										name = shortenNameIfNeeded(name)
									}
									funcCall, _ = sjson.Set(funcCall, "name", name)
								}
								funcCall, _ = sjson.Set(funcCall, "arguments", tc.Get("function.arguments").String())
								appendInput(funcCall)
							}
						}
					}
				}
			}
			return true
		})
	}
	inputBuf.WriteByte(']')

	// The envelope is built around an empty input array and the history goes in
	// last, in the placeholder's place: every sjson call below copies the whole
	// document it edits, which would otherwise include the history.
	out := `{"instructions":""}`
	out, _ = sjson.Set(out, "stream", stream)
	if v := fields.reasoningEffort; v.Exists() {
		out, _ = sjson.Set(out, "reasoning.effort", v.Value())
	} else {
		out, _ = sjson.Set(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.Set(out, "parallel_tool_calls", true)
	out, _ = sjson.Set(out, "reasoning.summary", "auto")
	out, _ = sjson.Set(out, "include", []string{"reasoning.encrypted_content"})
	out, _ = sjson.Set(out, "model", modelName)
	out, _ = sjson.Set(out, "store", false)
	out, _ = sjson.SetRaw(out, "input", `[]`)

	// Map response_format to the Responses API text.format, then the verbosity
	// a client may have set for a Responses upstream.
	format := common.OutputFormatFromChatResponseFormat(fields.responseFormat)
	if spec, ok := format.ResponsesFormat(); ok {
		out, _ = sjson.SetRaw(out, "text.format", spec)
	}
	if v := fields.verbosity; v.Exists() {
		out, _ = sjson.Set(out, "text.verbosity", v.Value())
	}

	// Map tools (flatten function fields) once.
	tools := fields.tools
	if tools.IsArray() && len(tools.Array()) > 0 {
		var toolsBuf bytes.Buffer
		toolsBuf.WriteByte('[')
		toolsFirst := true
		appendTool := func(raw string) {
			if raw == "" {
				return
			}
			if !toolsFirst {
				toolsBuf.WriteByte(',')
			}
			toolsFirst = false
			toolsBuf.WriteString(raw)
		}
		arr := tools.Array()
		for i := 0; i < len(arr); i++ {
			t := arr[i]
			toolType := t.Get("type").String()
			if toolType != "" && toolType != "function" && t.IsObject() {
				appendTool(t.Raw)
				continue
			}
			if toolType == "function" {
				item := `{}`
				item, _ = sjson.Set(item, "type", "function")
				fn := t.Get("function")
				if fn.Exists() {
					if v := fn.Get("name"); v.Exists() {
						name := v.String()
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						item, _ = sjson.Set(item, "name", name)
					}
					if v := fn.Get("description"); v.Exists() {
						item, _ = sjson.Set(item, "description", v.Value())
					}
					if v := fn.Get("parameters"); v.Exists() {
						item, _ = sjson.SetRaw(item, "parameters", v.Raw)
					}
					if v := fn.Get("strict"); v.Exists() {
						item, _ = sjson.Set(item, "strict", v.Value())
					}
				}
				appendTool(item)
			}
		}
		toolsBuf.WriteByte(']')
		out, _ = sjson.SetRaw(out, "tools", toolsBuf.String())
	}

	// Map tool_choice when present.
	if tc := fields.toolChoice; tc.Exists() {
		switch {
		case tc.Type == gjson.String:
			out, _ = sjson.Set(out, "tool_choice", tc.String())
		case tc.IsObject():
			tcType := tc.Get("type").String()
			if tcType == "function" {
				name := tc.Get("function.name").String()
				if name != "" {
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
				}
				choice := `{}`
				choice, _ = sjson.Set(choice, "type", "function")
				if name != "" {
					choice, _ = sjson.Set(choice, "name", name)
				}
				out, _ = sjson.SetRaw(out, "tool_choice", choice)
			} else if tcType != "" {
				out, _ = sjson.SetRaw(out, "tool_choice", tc.Raw)
			}
		}
	}

	body := spliceInput(out, inputBuf.Bytes())
	if format.Kind == common.OutputFormatJSONObject {
		// JSON mode needs the word "json" somewhere in the conversation. The
		// check reads the history, so it runs once the history is in.
		body = common.EnsureResponsesMentionsJSON(body)
	}
	return body
}

// appendArrayElement writes raw as the next element of the JSON array being
// written to buf; first is true until the array has an element.
func appendArrayElement(buf *bytes.Buffer, first *bool, raw string) {
	if !*first {
		buf.WriteByte(',')
	}
	*first = false
	buf.WriteString(raw)
}

// spliceInput writes input in place of the empty input array the envelope was
// built with, copying the history once.
func spliceInput(envelope string, input []byte) []byte {
	placeholder := gjson.Get(envelope, "input")
	if placeholder.Raw != "[]" || placeholder.Index <= 0 {
		// Unreachable while the envelope is built as above; sjson produces the
		// same bytes, only with more copying.
		body, _ := sjson.SetRawBytes([]byte(envelope), "input", input)
		return body
	}
	end := placeholder.Index + len(placeholder.Raw)
	body := make([]byte, 0, len(envelope)-len(placeholder.Raw)+len(input))
	body = append(body, envelope[:placeholder.Index]...)
	body = append(body, input...)
	return append(body, envelope[end:]...)
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
// If the name length exceeds 64, it will try to preserve the "mcp__" prefix and last segment.
// Otherwise it truncates to 64 characters.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

// buildShortNameMap generates unique short names (<=64) for the given list of names.
// It preserves the "mcp__" prefix with the last segment when possible and ensures uniqueness
// by appending suffixes like "~1", "~2" if needed.
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
