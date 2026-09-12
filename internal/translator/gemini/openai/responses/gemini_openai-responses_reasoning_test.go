package responses

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/tidwall/gjson"
)

// reasoningReplayRequest is the shape a Responses client sends on the turn after
// a tool call: the reasoning item it received back, then the call and its
// output. `encrypted_content` is absent, which is the usual case — clients only
// return it when the caller asked for it and stored it.
func reasoningReplayRequest(model, encryptedContent string) []byte {
	encrypted := ""
	if encryptedContent != "" {
		encrypted = `,"encrypted_content":"` + encryptedContent + `"`
	}
	return []byte(`{"model":"` + model + `","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"use calc on 1*8"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"Call calc."}],"content":[{"type":"reasoning_text","text":"Call calc."}]` + encrypted + `},` +
		`{"type":"function_call","call_id":"call_1","name":"calc","arguments":"{\"expr\":\"1*8\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"8"}` +
		`],"stream":true}`)
}

func thoughtPartCount(payload []byte) int {
	count := 0
	gjson.GetBytes(payload, "contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("thought").Bool() {
				count++
			}
			return true
		})
		return true
	})
	return count
}

// Writing an empty signature out is what broke these turns: the upstream
// answered "thinking.signature: Field required" for Claude and
// "Expected a(n) 'messages' array element to be an object." for GPT. With
// nothing valid to send, the reasoning item is dropped instead.
func TestConvertOpenAIResponsesRequestToGeminiDropsUnsignedReasoning(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6-thinking", "gpt-oss-120b-medium"} {
		got := ConvertOpenAIResponsesRequestToGemini(model, reasoningReplayRequest(model, ""), true)
		if n := thoughtPartCount(got); n != 0 {
			t.Errorf("%s: thought parts = %d, want 0", model, n)
		}
		if strings.Contains(string(got), `"thoughtSignature":""`) {
			t.Errorf("%s: an empty thoughtSignature must never be emitted: %s", model, got)
		}
		// Dropping the reasoning must not disturb the turns around it.
		if !gjson.GetBytes(got, "contents.1.parts.0.functionCall").Exists() {
			t.Errorf("%s: the function call turn must survive: %s", model, got)
		}
		if !gjson.GetBytes(got, "contents.2.parts.0.functionResponse").Exists() {
			t.Errorf("%s: the function response turn must survive: %s", model, got)
		}
	}
}

// The Gemini family accepts the skip sentinel, which is what the signature
// cache hands back for it, so its reasoning is still replayed.
func TestConvertOpenAIResponsesRequestToGeminiKeepsGeminiReasoning(t *testing.T) {
	got := ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-high", reasoningReplayRequest("gemini-3-pro-high", ""), true)
	if n := thoughtPartCount(got); n != 1 {
		t.Fatalf("thought parts = %d, want 1: %s", n, got)
	}
	if sig := gjson.GetBytes(got, "contents.1.parts.0.thoughtSignature").String(); sig != "skip_thought_signature_validator" {
		t.Errorf("thoughtSignature = %q, want the skip sentinel", sig)
	}
}

// A client that does return `encrypted_content` gets it replayed verbatim —
// that is the only signature Anthropic actually accepts.
func TestConvertOpenAIResponsesRequestToGeminiKeepsSignedReasoning(t *testing.T) {
	signature := strings.Repeat("s", cache.MinValidSignatureLen+8)
	got := ConvertOpenAIResponsesRequestToGemini("claude-opus-4-6-thinking", reasoningReplayRequest("claude-opus-4-6-thinking", signature), true)
	if n := thoughtPartCount(got); n != 1 {
		t.Fatalf("thought parts = %d, want 1: %s", n, got)
	}
	if sig := gjson.GetBytes(got, "contents.1.parts.0.thoughtSignature").String(); sig != signature {
		t.Errorf("thoughtSignature = %q, want the client signature preserved", sig)
	}
}
