package responses

// Claude thinking <-> Responses reasoning items.
//
// Anthropic signs every thinking block and rejects a replayed one whose
// signature is missing or not its own. Responses carries that signature in the
// reasoning item's encrypted_content, which Codex CLI and pi persist from
// response.output_item.done and send back unchanged next turn. These helpers
// build the item on the way out and rebuild the block on the way in.

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeResponsesRedactedThinkingPrefix marks a Responses reasoning item whose
// encrypted_content carries an Anthropic redacted_thinking payload rather than
// a thinking signature. Responses has no redacted reasoning type and Anthropic
// requires redacted_thinking blocks to be replayed verbatim, so the payload
// rides in encrypted_content behind this marker and is restored on the way
// back. The marker is not a valid signature anywhere, so no other upstream can
// mistake it for one.
const ClaudeResponsesRedactedThinkingPrefix = "claude-redacted-thinking:"

// claudeReasoningCarrier returns the encrypted_content value for a Claude
// thinking or redacted_thinking content block.
func claudeReasoningCarrier(contentBlock gjson.Result) string {
	if contentBlock.Get("type").String() == "redacted_thinking" {
		if data := contentBlock.Get("data").String(); data != "" {
			return ClaudeResponsesRedactedThinkingPrefix + data
		}
		return ""
	}
	return contentBlock.Get("signature").String()
}

// claudeReasoningItem builds the Responses reasoning item that mirrors one
// Claude thinking block. encrypted_content is only set when there is something
// to carry, so a client never replays an empty signature it would have to drop.
func claudeReasoningItem(id, text, encryptedContent string) string {
	item := `{"id":"","type":"reasoning","status":"completed","summary":[]}`
	item, _ = sjson.Set(item, "id", id)
	if text != "" {
		item, _ = sjson.SetRaw(item, "summary.-1", `{"type":"summary_text","text":""}`)
		item, _ = sjson.Set(item, "summary.0.text", text)
	}
	if encryptedContent != "" {
		item, _ = sjson.Set(item, "encrypted_content", encryptedContent)
	}
	return item
}

// convertResponsesReasoningToClaudeThinking rebuilds the Claude thinking block
// behind a replayed Responses reasoning item. It returns "" when the item
// carries nothing Anthropic would accept: Anthropic rejects a thinking block
// whose signature is missing, empty or not its own, so such an item is dropped
// rather than replayed. Anthropic does not check the text against the
// signature, which is what makes restoring the summary text safe.
func convertResponsesReasoningToClaudeThinking(item gjson.Result) string {
	encrypted := strings.TrimSpace(item.Get("encrypted_content").String())
	if strings.HasPrefix(encrypted, ClaudeResponsesRedactedThinkingPrefix) {
		data := strings.TrimSpace(strings.TrimPrefix(encrypted, ClaudeResponsesRedactedThinkingPrefix))
		if data == "" {
			return ""
		}
		block := `{"type":"redacted_thinking","data":""}`
		block, _ = sjson.Set(block, "data", data)
		return block
	}
	signature, ok := claudeThinkingSignature(encrypted)
	if !ok {
		return ""
	}
	block := `{"type":"thinking","thinking":"","signature":""}`
	block, _ = sjson.Set(block, "thinking", responsesReasoningText(item))
	block, _ = sjson.Set(block, "signature", signature)
	return block
}

// claudeThinkingSignature reports whether encrypted_content holds an Anthropic
// thinking signature and returns it. Sending another family's value (a GPT
// "gAAAA" blob, the Gemini skip sentinel) as a Claude signature fails the whole
// request, so only values shaped like an Anthropic signature are kept. A group
// prefix as the Claude Code path writes it ("claude#...") is accepted and
// stripped; any other group is rejected.
func claudeThinkingSignature(encrypted string) (string, bool) {
	sig := strings.TrimSpace(encrypted)
	if group, rest, found := strings.Cut(sig, "#"); found {
		if group != "claude" {
			return "", false
		}
		sig = rest
	}
	if len(sig) < claudeMinSignatureLen {
		return "", false
	}
	if isClaudeSignatureEnvelope(sig) {
		return sig, true
	}
	return "", false
}

// claudeMinSignatureLen matches cache.MinValidSignatureLen: real Anthropic
// signatures are hundreds of characters long.
const claudeMinSignatureLen = 50

// isClaudeSignatureEnvelope checks the envelope an Anthropic signature decodes
// to: a protobuf record starting with 0x12 (classic "E..." signatures) or 0x08
// (current "CAIS..." signatures), either directly or under a second base64
// layer. It is a shallow check: it rejects GPT reasoning blobs (Fernet tokens,
// 0x80), the Gemini skip sentinel and non-base64 values, but some Gemini 3
// thought signatures share the 0x12 marker. That is acceptable here because
// Codex and pi only replay reasoning to the model that produced it.
func isClaudeSignatureEnvelope(sig string) bool {
	decoded, ok := decodeSignatureBase64(sig)
	if !ok || len(decoded) == 0 {
		return false
	}
	if decoded[0] == 0x12 || decoded[0] == 0x08 {
		return true
	}
	inner, ok := decodeSignatureBase64(string(decoded))
	return ok && len(inner) > 0 && (inner[0] == 0x12 || inner[0] == 0x08)
}

func decodeSignatureBase64(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(s); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

// responsesReasoningText collects a reasoning item's text. Claude output only
// fills summary[], but clients echo the item through whichever array their SDK
// models, so content[] (reasoning_text) is read when summary[] is empty.
func responsesReasoningText(item gjson.Result) string {
	collect := func(parts gjson.Result) string {
		var b strings.Builder
		parts.ForEach(func(_, part gjson.Result) bool {
			if text := part.Get("text").String(); text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(text)
			}
			return true
		})
		return b.String()
	}
	if text := collect(item.Get("summary")); text != "" {
		return text
	}
	return collect(item.Get("content"))
}

// stripTrailingClaudeThinking removes thinking blocks at the end of the final
// assistant message. Anthropic rejects a request whose last assistant content
// block is a thinking block, which is what a client sends when a turn was cut
// off right after the model finished thinking. The message is removed when
// nothing else is left in it.
func stripTrailingClaudeThinking(out string) string {
	messages := gjson.Get(out, "messages")
	count := int(messages.Get("#").Int())
	if count == 0 {
		return out
	}
	last := messages.Get(fmt.Sprintf("%d", count-1))
	if last.Get("role").String() != "assistant" || !last.Get("content").IsArray() {
		return out
	}
	parts := last.Get("content").Array()
	end := len(parts)
	for end > 0 {
		if t := parts[end-1].Get("type").String(); t == "thinking" || t == "redacted_thinking" {
			end--
			continue
		}
		break
	}
	if end == len(parts) {
		return out
	}
	path := fmt.Sprintf("messages.%d", count-1)
	if end == 0 {
		out, _ = sjson.Delete(out, path)
		return out
	}
	kept := "[]"
	for _, part := range parts[:end] {
		kept, _ = sjson.SetRaw(kept, "-1", part.Raw)
	}
	out, _ = sjson.SetRaw(out, path+".content", kept)
	return out
}
