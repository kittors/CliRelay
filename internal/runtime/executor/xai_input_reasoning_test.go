package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const xaiTestGrokBlob = "Ytt1ZaAt1jB3eqJ3fR93iSdgCUka8/gEBOqXBnyAH8pNrppkcpUNdkyHB4OVTrUSjUqo1w7V7Ygwf2Ef"

// A replayed reasoning item xAI issued without `content`, rounded through a
// Responses client, carries an explicit `"content": null`. Forwarding that made
// xAI answer 400 "Could not decode the compaction blob. Ensure it is unmodified
// from the compact response." on every turn that continued a Grok reply.
func TestSanitizeXAIReasoningInputItemsDropsNullContent(t *testing.T) {
	body := []byte(`{"model":"grok-4.7","store":false,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"content":null,"encrypted_content":"` + xaiTestGrokBlob + `"}]}`)

	got := sanitizeXAIReasoningInputItems(body)
	if strings.Contains(string(got), `"content":null`) {
		t.Fatalf("null reasoning content survived: %s", got)
	}
	item := gjson.GetBytes(got, "input.1")
	if item.Get("content").Exists() {
		t.Fatalf("content member should be absent: %s", item.Raw)
	}
	if item.Get("encrypted_content").String() != xaiTestGrokBlob {
		t.Fatalf("blob was rewritten: %s", item.Raw)
	}
	if item.Get("summary.0.text").String() != "thinking" || item.Get("id").String() != "rs_1" {
		t.Fatalf("reasoning item lost members: %s", item.Raw)
	}
	if gjson.GetBytes(got, "input.0.content.0.text").String() != "hi" {
		t.Fatalf("unrelated item was rewritten: %s", got)
	}
}

func TestSanitizeXAIReasoningInputItemsDropsNullEncryptedContent(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","summary":[],"encrypted_content":null}]}`)
	got := string(sanitizeXAIReasoningInputItems(body))
	if strings.Contains(got, "encrypted_content") {
		t.Fatalf("null encrypted_content survived: %s", got)
	}
}

// A thread that started on another provider replays that provider's ciphertext.
// xAI can never decode it, so only the blob goes; the item stays.
func TestSanitizeXAIReasoningInputItemsDropsForeignCiphertext(t *testing.T) {
	for name, ciphertext := range map[string]string{
		"openai-fernet": "gAAAAABm3n9Xk1J8fS0mQ0Z2r6c9tY3w8p1v5u7x2y4z6A0b1C2d3E4f5G6h7I8j9K",
		"padded-base64": "Ytt1ZaAt1jB3eqJ3fR93iSdgCUka8/gEBOqXBnyAH8pNrppkcpUNdkyHB4OVTrUSjUqo1w7V7Ygwf2Ef==",
		"claude-prefix": "claude-redacted-thinking:AAAA",
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}]}`)
			got := sanitizeXAIReasoningInputItems(body)
			item := gjson.GetBytes(got, "input.0")
			if item.Get("encrypted_content").Exists() {
				t.Fatalf("foreign ciphertext survived: %s", item.Raw)
			}
			if item.Get("type").String() != "reasoning" {
				t.Fatalf("reasoning item was dropped: %s", got)
			}
		})
	}
}

func TestSanitizeXAIReasoningInputItemsKeepsNativeBlobs(t *testing.T) {
	for name, ciphertext := range map[string]string{
		"native":        xaiTestGrokBlob,
		"router-suffix": xaiTestGrokBlob + ".openrouter",
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}]}`)
			got := sanitizeXAIReasoningInputItems(body)
			if string(got) != string(body) {
				t.Fatalf("native blob was rewritten: got=%s want=%s", got, body)
			}
		})
	}
}

func TestSanitizeXAIReasoningInputItemsLeavesOrdinaryItemsAlone(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":null}]}`)
	got := sanitizeXAIReasoningInputItems(body)
	if string(got) != string(body) {
		t.Fatalf("body changed without a reasoning item: got=%s want=%s", got, body)
	}
}
