package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaude_AssistantReasoningContentToThinking(t *testing.T) {
	input := []byte(`{
		"model": "test-model",
		"messages": [
			{"role": "user", "content": "hi"},
			{
				"role": "assistant",
				"reasoning_content": "thoughts from previous turn",
				"content": "visible answer"
			},
			{"role": "user", "content": "continue"}
		]
	}`)

	got := ConvertOpenAIRequestToClaude("test-model", input, false)
	assistant := gjson.GetBytes(got, "messages.1")

	if assistant.Get("role").String() != "assistant" {
		t.Fatalf("expected messages.1 to be assistant, got body=%s", got)
	}
	if gotType := assistant.Get("content.0.type").String(); gotType != "thinking" {
		t.Fatalf("expected assistant content[0].type to be thinking, got %q; body=%s", gotType, got)
	}
	if gotThinking := assistant.Get("content.0.thinking").String(); gotThinking != "thoughts from previous turn" {
		t.Fatalf("expected assistant content[0].thinking to be preserved, got %q; body=%s", gotThinking, got)
	}
	if gotType := assistant.Get("content.1.type").String(); gotType != "text" {
		t.Fatalf("expected assistant content[1].type to be text, got %q; body=%s", gotType, got)
	}
	if gotText := assistant.Get("content.1.text").String(); gotText != "visible answer" {
		t.Fatalf("expected assistant visible text to be preserved, got %q; body=%s", gotText, got)
	}
}

func TestConvertOpenAIRequestToClaude_AssistantReasoningContentWithToolCalls(t *testing.T) {
	input := []byte(`{
		"model": "test-model",
		"messages": [
			{
				"role": "assistant",
				"reasoning_content": "tool reasoning",
				"content": [{"type": "text", "text": "checking"}],
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}
				}]
			}
		]
	}`)

	got := ConvertOpenAIRequestToClaude("test-model", input, false)
	assistant := gjson.GetBytes(got, "messages.0")

	if gotType := assistant.Get("content.0.type").String(); gotType != "thinking" {
		t.Fatalf("expected assistant content[0].type to be thinking, got %q; body=%s", gotType, got)
	}
	if gotThinking := assistant.Get("content.0.thinking").String(); gotThinking != "tool reasoning" {
		t.Fatalf("expected reasoning content to be preserved as thinking, got %q; body=%s", gotThinking, got)
	}
	if gotType := assistant.Get("content.1.type").String(); gotType != "text" {
		t.Fatalf("expected assistant content[1].type to be text, got %q; body=%s", gotType, got)
	}
	if gotType := assistant.Get("content.2.type").String(); gotType != "tool_use" {
		t.Fatalf("expected assistant content[2].type to be tool_use, got %q; body=%s", gotType, got)
	}
}
