package executor

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reasoning ciphertext and client `include` for the Codex upstream.
//
// A Responses request replays earlier reasoning as items carrying an opaque
// `encrypted_content`. Only the model family that wrote it can read it back, so
// a conversation that began on Claude or Gemini and moves to a Codex model
// arrives with ciphertext the ChatGPT backend cannot decrypt, and the whole
// request is refused with `invalid_encrypted_content`. The Responses translators
// carry other vendors' signatures in that same field, and nothing between the
// client and the upstream looked at it.
//
// Two layers handle it. Before sending, ciphertext that is plainly not OpenAI's
// is removed (below). After the upstream refuses a request anyway, the sender
// strips every reasoning ciphertext and retries once (codex_responses_send.go),
// which is the only thing that can catch OpenAI ciphertext minted for a
// different account or organization. The websocket transport has no such retry
// (the refusal arrives as an event on a connection that may carry other turns),
// so it gets the first layer only.

// codexOpenAICiphertextPrefix is how OpenAI's reasoning ciphertext begins: it is
// a Fernet token, whose base64 form always opens with the version byte and the
// zero high bytes of the timestamp. Claude signatures, Gemini thought signatures
// and the `claude-redacted-thinking:` prefix the translators add all start
// differently.
const codexOpenAICiphertextPrefix = "gAAAA"

// codexEncryptedContentKey is matched with its quotes so the include entry
// "reasoning.encrypted_content", which every translated request carries, does
// not make an otherwise ciphertext-free body look like it has work to do.
var codexEncryptedContentKey = []byte(`"encrypted_content"`)

func isForeignCodexCiphertext(ciphertext string) bool {
	return !strings.HasPrefix(ciphertext, codexOpenAICiphertextPrefix)
}

func anyCodexCiphertext(string) bool { return true }

// dropCodexReasoningItems removes reasoning input items whose ciphertext drop
// selects, and returns how many it removed.
//
// The whole item goes, not just its ciphertext field: a reasoning item without
// ciphertext carries nothing the model can use (summaries are never fed back),
// and a half-empty one invites the upstream to look it up by id.
func dropCodexReasoningItems(body []byte, drop func(ciphertext string) bool) ([]byte, int) {
	if len(body) == 0 || !bytes.Contains(body, codexEncryptedContentKey) {
		return body, 0
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, 0
	}
	kept := make([][]byte, 0, 16)
	dropped := 0
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" {
			ciphertext := item.Get("encrypted_content")
			if ciphertext.Type == gjson.String && ciphertext.String() != "" && drop(ciphertext.String()) {
				dropped++
				return true
			}
		}
		kept = append(kept, []byte(item.Raw))
		return true
	})
	if dropped == 0 {
		return body, 0
	}
	rebuilt := make([]byte, 0, len(input.Raw))
	rebuilt = append(rebuilt, '[')
	for i, raw := range kept {
		if i > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
	}
	rebuilt = append(rebuilt, ']')
	updated, err := sjson.SetRawBytes(body, "input", rebuilt)
	if err != nil {
		return body, 0
	}
	return updated, dropped
}

// mergeCodexClientInclude adds the `include` entries the client asked for to the
// ones the translator wrote.
//
// The Responses->Codex translator overwrites `include` with the one value this
// proxy needs (reasoning.encrypted_content, so reasoning can be replayed), which
// silently dropped everything else a client asked for, such as
// web_search_call.action.sources or message.output_text.logprobs. Entries are
// unioned rather than replaced so the translator's value is never lost; the body
// is only rewritten when the client asked for something the translator did not.
// Other source formats are left alone: their payloads have no `include` of this
// meaning (Chat Completions and Claude have no such field).
func mergeCodexClientInclude(body, clientPayload []byte, source sdktranslator.Format) []byte {
	if source != sdktranslator.FormatOpenAIResponse {
		return body
	}
	client := gjson.GetBytes(clientPayload, "include")
	if !client.IsArray() {
		return body
	}
	merged := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(_, entry gjson.Result) bool {
		if entry.Type != gjson.String {
			return true
		}
		value := strings.TrimSpace(entry.String())
		if value == "" {
			return true
		}
		if _, dup := seen[value]; !dup {
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
		return true
	}
	translated := gjson.GetBytes(body, "include")
	if translated.IsArray() {
		translated.ForEach(add)
	}
	before := len(merged)
	client.ForEach(add)
	if len(merged) == before {
		return body
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return body
	}
	return util.MutateTopLevelObject(body, map[string][]byte{"include": raw}, nil)
}

// prepareCodexResponsesBody applies the Codex-specific input rules that must run
// after translation. chatGPTBackend selects the ciphertext check, which relies on
// the ChatGPT backend's token format and so must not touch API-key upstreams.
func prepareCodexResponsesBody(body, clientPayload []byte, source sdktranslator.Format, chatGPTBackend bool) []byte {
	body = mergeCodexClientInclude(body, clientPayload, source)
	if chatGPTBackend {
		var dropped int
		body, dropped = dropCodexReasoningItems(body, isForeignCodexCiphertext)
		if dropped > 0 {
			log.Debugf("codex executor: dropped %d reasoning item(s) carrying non-OpenAI encrypted_content", dropped)
		}
	}
	return body
}
