package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIChatToolCallMessagesMergesConsecutiveAssistantToolCalls(t *testing.T) {
	input := []byte(`{
		"model":"deepseek-v4-flash",
		"messages":[
			{"role":"user","content":"run checks"},
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},
			{"role":"assistant","tool_calls":[{"id":"call_b","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_a","content":"ok-a"},
			{"role":"tool","tool_call_id":"call_b","content":"ok-b"},
			{"role":"user","content":"continue"}
		]
	}`)

	out := normalizeOpenAIChatToolCallMessages(input)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 5 {
		t.Fatalf("messages len = %d, want 5: %s", len(messages), out)
	}
	assistant := messages[1]
	calls := assistant.Get("tool_calls").Array()
	if len(calls) != 2 {
		t.Fatalf("tool_calls len = %d, want 2: %s", len(calls), out)
	}
	if got := messages[2].Get("role").String(); got != "tool" {
		t.Fatalf("messages[2].role = %q, want tool: %s", got, out)
	}
	if got := messages[3].Get("tool_call_id").String(); got != "call_b" {
		t.Fatalf("messages[3].tool_call_id = %q, want call_b: %s", got, out)
	}
}

func TestNormalizeOpenAIChatToolCallMessagesPreservesValidToolBlock(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":"","tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}},
				{"id":"call_b","type":"function","function":{"name":"Read","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_a","content":"a"},
			{"role":"tool","tool_call_id":"call_b","content":"b"},
			{"role":"user","content":"next"}
		]
	}`)

	out := normalizeOpenAIChatToolCallMessages(input)
	if string(out) != string(input) {
		t.Fatalf("valid payload should be unchanged:\n%s", out)
	}
}

func TestNormalizeOpenAIChatToolCallMessagesStripsInvalidToolBlocks(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"user","content":"before"},
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}}]},
			{"role":"user","content":"interrupts before tool output"},
			{"role":"tool","tool_call_id":"call_a","content":"late orphan"},
			{"role":"assistant","content":"after"}
		]
	}`)

	out := normalizeOpenAIChatToolCallMessages(input)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3: %s", len(messages), out)
	}
	for _, msg := range messages {
		if msg.Get("tool_calls").Exists() {
			t.Fatalf("invalid tool_calls should be stripped: %s", out)
		}
		if msg.Get("role").String() == "tool" {
			t.Fatalf("orphan tool message should be stripped: %s", out)
		}
	}
}

func TestNormalizeOpenAIChatToolCallMessagesKeepsAssistantContentWhenToolOutputMissing(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":"I will call a tool","tool_calls":[{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{}"}}]},
			{"role":"user","content":"next"}
		]
	}`)

	out := normalizeOpenAIChatToolCallMessages(input)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2: %s", len(messages), out)
	}
	if got := messages[0].Get("content").String(); got != "I will call a tool" {
		t.Fatalf("assistant content = %q, want preserved: %s", got, out)
	}
	if messages[0].Get("tool_calls").Exists() {
		t.Fatalf("missing-output tool_calls should be removed: %s", out)
	}
}

func TestNormalizeOpenAIChatToolCallMessagesRejectsDuplicateToolCallIDs(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_dup","type":"function","function":{"name":"Read","arguments":"{}"}},
				{"id":"call_dup","type":"function","function":{"name":"Write","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_dup","content":"result"},
			{"role":"user","content":"next"}
		]
	}`)

	out := normalizeOpenAIChatToolCallMessages(input)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1: %s", len(messages), out)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("remaining role = %q, want user: %s", got, out)
	}
}
