package registry

import "strings"

// ClaudeRequestRules lists request fields a Claude model answers with a 400.
// Clients written for an older model, and the Chat and Gemini translators, still
// send them, so the executors drop or rewrite them before the request goes out.
// These are rules of the request surface, separate from how the model thinks
// (ThinkingSupport.Adaptive).
type ClaudeRequestRules struct {
	// NoSampling: temperature, top_p and top_k must be absent. Opus 4.7 and
	// later, Fable, Mythos and Sonnet 5 reject them at any value; Sonnet 5.5
	// and Haiku 5.5 reject non-default values. Leaving them out is accepted on
	// every model and means the default. The 400 reads
	// "`temperature` is deprecated for this model."
	NoSampling bool
	// NoForcedToolChoice: tool_choice "any" and "tool" are rejected on the
	// Messages API, Batches and count_tokens (Opus 5.5, Sonnet 5.5, Fable 5.1,
	// Mythos 5.1). The 400 reads
	// `tool_choice: type "tool" and "any" are not supported for this model.`
	NoForcedToolChoice bool
}

// LookupClaudeModelInfo returns the Claude catalog entry for modelID, the name
// sent upstream, or nil when the catalog does not know it. Unlike
// LookupModelInfo it ignores the dynamic registry: entries registered from
// configuration or discovery are copies that do not carry every field, while a
// rule of the upstream model belongs to its upstream name.
func LookupClaudeModelInfo(modelID string) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	for _, model := range GetClaudeModels() {
		if model != nil && strings.EqualFold(model.ID, modelID) {
			return model
		}
	}
	return nil
}
