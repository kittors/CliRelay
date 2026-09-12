package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// realSignature stands in for a genuine upstream signature: what matters to the
// code under test is only that it clears cache.MinValidSignatureLen.
var realSignature = strings.Repeat("s", 64)

func thoughtPayload(signature string) []byte {
	return []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"count to two"}]},` +
		`{"role":"model","parts":[{"text":"thinking out loud","thought":true,"thoughtSignature":"` + signature + `"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"calc","args":{}}}]}` +
		`]}}`)
}

func thoughtParts(t *testing.T, payload []byte) []gjson.Result {
	t.Helper()
	var out []gjson.Result
	gjson.GetBytes(payload, "request.contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("thought").Bool() {
				out = append(out, part)
			}
			return true
		})
		return true
	})
	return out
}

// The GPT family has no shape for a replayed thought: CloudCode renders those
// turns as an OpenAI messages array and fails the whole request with
// "Expected a(n) 'messages' array element to be an object." Verified against
// the live upstream — the same payload with the thought removed answers 200.
func TestSanitizeAntigravityThoughtsDropsThoughtsForGPT(t *testing.T) {
	for _, signature := range []string{"", antigravityThoughtSentinel, realSignature} {
		got := sanitizeAntigravityThoughts(thoughtPayload(signature), "gpt-oss-120b-medium")
		if parts := thoughtParts(t, got); len(parts) != 0 {
			t.Errorf("signature %q: thought parts survived: %v", signature, parts)
		}
		if n := gjson.GetBytes(got, "request.contents.#").Int(); n != 2 {
			t.Errorf("signature %q: contents = %d, want 2 (the emptied one is dropped too)", signature, n)
		}
		if !gjson.GetBytes(got, "request.contents.1.parts.0.functionCall").Exists() {
			t.Errorf("signature %q: the functionCall turn must survive", signature)
		}
	}
}

// Anthropic validates the signature for real: an empty one comes back as
// "thinking.signature: Field required" and a fabricated one as
// "Invalid `signature` in `thinking` block". Only a genuine signature may be
// replayed; anything else has to go.
func TestSanitizeAntigravityThoughtsDropsUnsignedThoughtsForClaude(t *testing.T) {
	for _, signature := range []string{"", antigravityThoughtSentinel} {
		got := sanitizeAntigravityThoughts(thoughtPayload(signature), "claude-opus-4-6-thinking")
		if parts := thoughtParts(t, got); len(parts) != 0 {
			t.Errorf("signature %q: unsigned thought survived: %v", signature, parts)
		}
	}

	got := sanitizeAntigravityThoughts(thoughtPayload(realSignature), "claude-opus-4-6-thinking")
	parts := thoughtParts(t, got)
	if len(parts) != 1 {
		t.Fatalf("signed thought parts = %d, want 1", len(parts))
	}
	if sig := parts[0].Get("thoughtSignature").String(); sig != realSignature {
		t.Errorf("signature = %q, want the original to be preserved", sig)
	}
}

// Gemini is the one family that accepts the sentinel, so an unsigned thought is
// topped up rather than dropped and the turn keeps its context.
func TestSanitizeAntigravityThoughtsFillsSentinelForGemini(t *testing.T) {
	got := sanitizeAntigravityThoughts(thoughtPayload(""), "gemini-3-pro-high")
	parts := thoughtParts(t, got)
	if len(parts) != 1 {
		t.Fatalf("thought parts = %d, want 1", len(parts))
	}
	if sig := parts[0].Get("thoughtSignature").String(); sig != antigravityThoughtSentinel {
		t.Errorf("thoughtSignature = %q, want %q", sig, antigravityThoughtSentinel)
	}
	if n := gjson.GetBytes(got, "request.contents.#").Int(); n != 3 {
		t.Errorf("contents = %d, want all 3 kept", n)
	}
}

// A thought sharing a part list with real content must not take that content
// with it: only the thought part itself is removed.
func TestSanitizeAntigravityThoughtsKeepsSiblingParts(t *testing.T) {
	payload := []byte(`{"request":{"contents":[` +
		`{"role":"model","parts":[` +
		`{"text":"thinking","thought":true,"thoughtSignature":""},` +
		`{"text":"the answer is two"}` +
		`]}]}}`)

	got := sanitizeAntigravityThoughts(payload, "claude-opus-4-6-thinking")
	if n := gjson.GetBytes(got, "request.contents.#").Int(); n != 1 {
		t.Fatalf("contents = %d, want 1", n)
	}
	parts := gjson.GetBytes(got, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	if text := parts[0].Get("text").String(); text != "the answer is two" {
		t.Errorf("surviving part text = %q", text)
	}
}

// Both `includeThoughts` and `thinkingLevel` draw a bare 400 from the GPT
// family. Its reasoning effort already rides on the model id, so the block is
// dropped rather than translated.
func TestStripAntigravityUnsupportedThinkingConfig(t *testing.T) {
	payload := []byte(`{"request":{"generationConfig":{"maxOutputTokens":64,"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}}}`)

	got := stripAntigravityUnsupportedThinkingConfig(payload, "gpt-oss-120b-medium")
	if gjson.GetBytes(got, "request.generationConfig.thinkingConfig").Exists() {
		t.Error("thinkingConfig must be removed for the GPT family")
	}
	if gjson.GetBytes(got, "request.generationConfig.maxOutputTokens").Int() != 64 {
		t.Error("the rest of generationConfig must survive")
	}

	for _, model := range []string{"claude-opus-4-6-thinking", "gemini-3-pro-high"} {
		got := stripAntigravityUnsupportedThinkingConfig(payload, model)
		if !gjson.GetBytes(got, "request.generationConfig.thinkingConfig").Exists() {
			t.Errorf("%s: thinkingConfig must be preserved", model)
		}
	}
}

// A generationConfig that held nothing but an unsupported thinkingConfig is
// removed outright rather than left behind as an empty object.
func TestStripAntigravityUnsupportedThinkingConfigDropsEmptyGenerationConfig(t *testing.T) {
	payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}}`)
	got := stripAntigravityUnsupportedThinkingConfig(payload, "gpt-oss-120b-medium")
	if gjson.GetBytes(got, "request.generationConfig").Exists() {
		t.Error("an emptied generationConfig must be dropped")
	}
}
