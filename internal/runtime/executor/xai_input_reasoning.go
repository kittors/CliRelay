package executor

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reasoning replay and Grok's compaction blobs.
//
// A Responses client replays earlier reasoning as input items carrying
// `encrypted_content`. xAI reads that blob back when the turn continues, and
// answers with a 400
//
//	Could not decode the compaction blob. Ensure it is unmodified from the
//	compact response.
//
// when the item it receives is not the one it issued.
//
// Two things break that. xAI omits `content` on reasoning items, while a
// replayed item often carries an explicit `"content": null`, and the null member
// alone is enough to make the blob fail to decode. And a thread that started on
// another provider arrives with that provider's ciphertext — OpenAI's `gAAAA`
// Fernet token, or a padded value — which xAI can never decode.
//
// Null members are simply dropped, never replaced with a value: the blob must
// travel exactly as issued. Foreign ciphertext is dropped from the item, which
// keeps the replayed turn valid without pretending the blob is Grok's.

// xaiOmittedReasoningNullFields is every reasoning-item member xAI itself omits
// when it means "no value".
var xaiOmittedReasoningNullFields = []string{"content", "encrypted_content"}

// sanitizeXAIReasoningInputItems makes replayed reasoning items decodable by
// xAI: it removes null members and ciphertext that is not Grok's own transport
// shape. Everything else in the item is left byte for byte.
func sanitizeXAIReasoningInputItems(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if len(body) == 0 || !input.IsArray() {
		return body
	}
	for index, item := range input.Array() {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		for _, field := range xaiOmittedReasoningNullFields {
			value := item.Get(field)
			if value.Exists() && value.Type == gjson.Null {
				body = deleteXAIInputField(body, index, field)
			}
		}
		ciphertext := item.Get("encrypted_content")
		if ciphertext.Type == gjson.String && !isNativeGrokCiphertext(ciphertext.String()) {
			body = deleteXAIInputField(body, index, "encrypted_content")
		}
	}
	return body
}

func deleteXAIInputField(body []byte, index int, field string) []byte {
	next, err := sjson.DeleteBytes(body, fmt.Sprintf("input.%d.%s", index, field))
	if err != nil {
		return body
	}
	return next
}

// isNativeGrokCiphertext reports whether s has the transport shape of a Grok
// encrypted_content: unpadded standard base64 that is not another provider's
// self-describing envelope.
//
// This proves nothing about decryptability; it only rejects the values xAI can
// never read back.
func isNativeGrokCiphertext(s string) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	// OpenAI/Codex reasoning signatures are Fernet tokens and always start here.
	if strings.HasPrefix(s, "gAAAA") {
		return false
	}
	// Grok emits unpadded base64; a padded value came from another envelope.
	if strings.Contains(s, "=") {
		return false
	}
	if len(s)%4 == 1 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
		case c == '.':
			// A router may append a dot-delimited tag after the native payload.
			return isMetaRouterTag(s[i+1:])
		default:
			return false
		}
	}
	return true
}

func isMetaRouterTag(tag string) bool {
	if tag == "" || len(tag) > 16 {
		return false
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
