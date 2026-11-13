package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOllamaNativeMessagesPreservesReasoningInToolHistory(t *testing.T) {
	request := []byte(`{"model":"test-model","messages":[{"role":"assistant","content":"","reasoning_content":"retained reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)

	native := ollamaNativeChatRequest(request, false)
	if got := gjson.GetBytes(native, "messages.0.thinking").String(); got != "retained reasoning" {
		t.Fatalf("messages.0.thinking = %q, want retained reasoning", got)
	}
	if got := gjson.GetBytes(native, "messages.1.tool_name").String(); got != "lookup" {
		t.Fatalf("messages.1.tool_name = %q, want lookup", got)
	}
}

func TestOllamaNativeChatResponsePreservesReasoning(t *testing.T) {
	native := []byte(`{"message":{"role":"assistant","content":"answer","thinking":"reply reasoning"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":2}`)

	response := ollamaNativeChatToOpenAI(native, "test-model", &ollamaPromptCacheEstimator{})
	if got := gjson.GetBytes(response, "choices.0.message.reasoning_content").String(); got != "reply reasoning" {
		t.Fatalf("reasoning_content = %q, want reply reasoning", got)
	}
}

func TestOllamaNativeStreamPreservesReasoning(t *testing.T) {
	native := []byte(`{"message":{"role":"assistant","thinking":"stream reasoning"},"done":false}`)
	roleSent := false

	lines, _, done := ollamaNativeStreamChunkToOpenAI(native, "test-model", "chatcmpl_test", &ollamaPromptCacheEstimator{}, &roleSent)
	if done {
		t.Fatal("partial stream chunk reported done")
	}
	joined := string(strings.Join(func() []string {
		out := make([]string, len(lines))
		for i := range lines {
			out[i] = string(lines[i])
		}
		return out
	}(), ""))
	if !strings.Contains(joined, `"reasoning_content":"stream reasoning"`) {
		t.Fatalf("stream reasoning missing from %q", joined)
	}
}
