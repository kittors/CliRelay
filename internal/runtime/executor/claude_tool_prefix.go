package executor

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func checkSystemInstructions(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	claudeCodeInstructions := `[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]`
	if system.IsArray() {
		if gjson.GetBytes(payload, "system.0.text").String() != "You are Claude Code, Anthropic's official CLI for Claude." {
			system.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					claudeCodeInstructions, _ = sjson.SetRaw(claudeCodeInstructions, "-1", part.Raw)
				}
				return true
			})
			payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
		}
	} else {
		payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
	}
	return payload
}

// rebuildMidSystemMessagesToTopLevel 将 Anthropic 不兼容的 system-role 消息移到顶层 system。
func rebuildMidSystemMessagesToTopLevel(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var movedSystemParts []string
	removedSystemMessage := false
	keptMessages := make([]string, 0, int(messages.Get("#").Int()))
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			removedSystemMessage = true
			movedSystemParts = append(movedSystemParts, claudeSystemTextParts(message.Get("content"))...)
			return true
		}
		keptMessages = append(keptMessages, message.Raw)
		return true
	})
	if !removedSystemMessage {
		return payload
	}

	systemParts := claudeSystemTextParts(gjson.GetBytes(payload, "system"))
	systemParts = append(systemParts, movedSystemParts...)
	if len(systemParts) > 0 {
		if updated, err := sjson.SetRawBytes(payload, "system", rawJSONArray(systemParts)); err == nil {
			payload = updated
		}
	}
	if updated, err := sjson.SetRawBytes(payload, "messages", rawJSONArray(keptMessages)); err == nil {
		payload = updated
	}
	return payload
}

// claudeSystemTextParts 从支持的 system content 结构中提取有效的 Anthropic text block。
func claudeSystemTextParts(content gjson.Result) []string {
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return []string{string(block)}
	}
	if !content.IsArray() {
		return nil
	}

	parts := make([]string, 0, int(content.Get("#").Int()))
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			text := item.String()
			if strings.TrimSpace(text) != "" {
				block := []byte(`{"type":"text","text":""}`)
				block, _ = sjson.SetBytes(block, "text", text)
				parts = append(parts, string(block))
			}
			return true
		}
		if item.IsObject() &&
			item.Get("type").String() == "text" &&
			strings.TrimSpace(item.Get("text").String()) != "" {
			parts = append(parts, item.Raw)
		}
		return true
	})
	return parts
}

// rawJSONArray 用已编码的 JSON item 字符串构造 JSON array。
func rawJSONArray(items []string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return []byte(builder.String())
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	builtinTools := map[string]bool{}
	for _, name := range []string{"web_search", "code_execution", "text_editor", "computer"} {
		builtinTools[name] = true
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if !strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if !strings.HasPrefix(toolName, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.tool_name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
		case "tool_result":
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if strings.HasPrefix(nestedToolName, prefix) {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
							body, _ = sjson.SetBytes(body, nestedPath, strings.TrimPrefix(nestedToolName, prefix))
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}
