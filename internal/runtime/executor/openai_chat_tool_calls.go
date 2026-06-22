package executor

import "encoding/json"

func normalizeOpenAIChatToolCallMessages(payload []byte) []byte {
	if len(payload) == 0 || !json.Valid(payload) {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}

	merged, changedMerge := mergeConsecutiveAssistantToolCalls(messages)
	normalized, changedInvalid := stripInvalidToolCallBlocks(merged)
	if !changedMerge && !changedInvalid {
		return payload
	}

	root["messages"] = normalized
	out, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return out
}

func mergeConsecutiveAssistantToolCalls(messages []any) ([]any, bool) {
	out := make([]any, 0, len(messages))
	changed := false

	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok || messageRole(msg) != "assistant" || len(messageToolCalls(msg)) == 0 {
			out = append(out, messages[i])
			continue
		}

		calls := append([]any{}, messageToolCalls(msg)...)
		for i+1 < len(messages) {
			next, ok := messages[i+1].(map[string]any)
			if !ok || messageRole(next) != "assistant" || len(messageToolCalls(next)) == 0 || !messageContentEmpty(next) {
				break
			}
			calls = append(calls, messageToolCalls(next)...)
			i++
			changed = true
		}
		msg["tool_calls"] = calls
		out = append(out, msg)
	}

	return out, changed
}

func stripInvalidToolCallBlocks(messages []any) ([]any, bool) {
	out := make([]any, 0, len(messages))
	changed := false

	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			out = append(out, messages[i])
			continue
		}

		switch messageRole(msg) {
		case "assistant":
			calls := messageToolCalls(msg)
			if len(calls) == 0 {
				out = append(out, messages[i])
				continue
			}

			expected, validIDs := toolCallIDSet(calls)
			j := i + 1
			toolBlock := make([]any, 0, len(expected))
			seen := make(map[string]bool, len(expected))
			for j < len(messages) {
				toolMsg, ok := messages[j].(map[string]any)
				if !ok || messageRole(toolMsg) != "tool" {
					break
				}
				id, _ := toolMsg["tool_call_id"].(string)
				if expected[id] && !seen[id] {
					toolBlock = append(toolBlock, toolMsg)
					seen[id] = true
				} else {
					changed = true
				}
				j++
			}

			if validIDs && len(seen) == len(expected) {
				out = append(out, msg)
				out = append(out, toolBlock...)
				i = j - 1
				continue
			}

			changed = true
			if !messageContentEmpty(msg) {
				delete(msg, "tool_calls")
				out = append(out, msg)
			}
		case "tool":
			changed = true
		default:
			out = append(out, messages[i])
		}
	}

	return out, changed
}

func messageRole(msg map[string]any) string {
	role, _ := msg["role"].(string)
	return role
}

func messageToolCalls(msg map[string]any) []any {
	calls, _ := msg["tool_calls"].([]any)
	return calls
}

func messageContentEmpty(msg map[string]any) bool {
	content, exists := msg["content"]
	if !exists || content == nil {
		return true
	}
	switch v := content.(type) {
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	default:
		return false
	}
}

func toolCallIDSet(calls []any) (map[string]bool, bool) {
	ids := make(map[string]bool, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			return ids, false
		}
		id, _ := call["id"].(string)
		if id == "" {
			return ids, false
		}
		if ids[id] {
			return ids, false
		}
		ids[id] = true
	}
	return ids, len(ids) > 0
}
