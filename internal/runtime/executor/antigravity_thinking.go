package executor

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// antigravityThoughtSentinel is the placeholder signature the Gemini family
// accepts in place of a real one. The other families reject it — see
// antigravityThinkingReplay below.
const antigravityThoughtSentinel = "skip_thought_signature_validator"

// antigravityThinkingReplay describes how much of a previous turn's thinking a
// model family will accept when it is replayed in a follow-up request.
type antigravityThinkingReplay int

const (
	// replaySentinelOK: an unsigned thought is fine as long as it carries the
	// skip sentinel. Only the Gemini family behaves this way.
	replaySentinelOK antigravityThinkingReplay = iota
	// replaySignedOnly: only a genuine upstream signature is accepted. Anthropic
	// answers an empty signature with "thinking.signature: Field required" and a
	// fabricated one with "Invalid `signature` in `thinking` block".
	replaySignedOnly
	// replayNever: the family rejects a replayed thought whatever its signature.
	// CloudCode renders GPT turns as an OpenAI `messages` array and has no shape
	// for a thought part, so it fails the whole request with
	// "Expected a(n) 'messages' array element to be an object."
	replayNever
)

// antigravityThinkingReplayFor maps a model onto its family's replay rule.
// It keys off the shared model group rather than a model list so newly
// published models inherit the right behaviour without a code change.
func antigravityThinkingReplayFor(modelName string) antigravityThinkingReplay {
	switch cache.GetModelGroup(modelName) {
	case "gemini":
		return replaySentinelOK
	case "gpt":
		return replayNever
	case "claude":
		return replaySignedOnly
	default:
		// Unknown families get the conservative rule: replaying an unsigned
		// thought is what breaks requests, dropping one never does.
		return replaySignedOnly
	}
}

// sanitizeAntigravityThoughts drops thought parts the upstream would reject and
// tops up the sentinel for the family that wants one.
//
// Every entrypoint format (Gemini, Claude, OpenAI chat completions, OpenAI
// responses) has its own translator, and each one used to decide this for
// itself — the Claude translator dropped unsigned thoughts correctly while the
// Responses translator emitted an empty signature. Doing it here instead covers
// all four at once, because this is the last point they share.
func sanitizeAntigravityThoughts(payload []byte, modelName string) []byte {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	policy := antigravityThinkingReplayFor(modelName)

	type drop struct {
		contentIdx int
		partIdx    int
	}
	var partDrops []drop
	var contentDrops []int

	contentResults := contents.Array()
	for contentIdx, content := range contentResults {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		partResults := parts.Array()
		kept := 0
		for partIdx, part := range partResults {
			if !part.Get("thought").Bool() {
				kept++
				continue
			}
			signature := part.Get("thoughtSignature").String()
			switch policy {
			case replayNever:
				partDrops = append(partDrops, drop{contentIdx, partIdx})
			case replaySentinelOK:
				if !cache.HasValidSignature(modelName, signature) {
					payload, _ = sjson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", contentIdx, partIdx), antigravityThoughtSentinel)
				}
				kept++
			default: // replaySignedOnly
				if cache.HasValidSignature(modelName, signature) && signature != antigravityThoughtSentinel {
					kept++
					continue
				}
				partDrops = append(partDrops, drop{contentIdx, partIdx})
			}
		}
		// A content left with no parts is itself invalid upstream ("required
		// oneof field 'data' must have one initialized field"), so it goes too.
		if kept == 0 && len(partResults) > 0 {
			contentDrops = append(contentDrops, contentIdx)
		}
	}

	// Delete back to front so the earlier indices stay addressable.
	for i := len(partDrops) - 1; i >= 0; i-- {
		d := partDrops[i]
		payload, _ = sjson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d", d.contentIdx, d.partIdx))
	}
	for i := len(contentDrops) - 1; i >= 0; i-- {
		payload, _ = sjson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d", contentDrops[i]))
	}
	return payload
}

// stripAntigravityUnsupportedThinkingConfig removes a thinkingConfig the model
// cannot accept.
//
// The GPT family rejects both `includeThoughts` and `thinkingLevel` with a bare
// 400 that names no field, which is easy to misread as a broken credential. Its
// reasoning effort is already carried by the model id itself (gpt-oss-120b-low
// / -medium / -high), so nothing is lost by dropping the block.
func stripAntigravityUnsupportedThinkingConfig(payload []byte, modelName string) []byte {
	if cache.GetModelGroup(modelName) != "gpt" {
		return payload
	}
	if !gjson.GetBytes(payload, "request.generationConfig.thinkingConfig").Exists() {
		return payload
	}
	payload, _ = sjson.DeleteBytes(payload, "request.generationConfig.thinkingConfig")
	// An empty generationConfig is harmless but noisy; drop it when it was only
	// ever holding the thinkingConfig.
	if gen := gjson.GetBytes(payload, "request.generationConfig"); gen.IsObject() && len(gen.Map()) == 0 {
		payload, _ = sjson.DeleteBytes(payload, "request.generationConfig")
	}
	return payload
}
