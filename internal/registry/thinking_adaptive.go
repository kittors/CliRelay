package registry

// Values of AdaptiveThinking.Off.
const (
	// ClaudeThinkingAlwaysOn marks a model that cannot turn thinking off
	// (Opus 5.5, Fable, Mythos): thinking.type "disabled" returns a 400 at every
	// effort level, so the request omits thinking and lowers the effort instead.
	ClaudeThinkingAlwaysOn = ""
	// ClaudeThinkingOffDisabled turns thinking off with thinking.type "disabled".
	ClaudeThinkingOffDisabled = "disabled"
	// ClaudeThinkingOffBetweenTools turns thinking off with thinking.type
	// "between_tools", the lowest setting on Claude Sonnet 5.5, which rejects
	// "disabled". The object takes no other field.
	ClaudeThinkingOffBetweenTools = "between_tools"
)

// AdaptiveThinking describes a Claude model that only takes thinking.type
// "adaptive" with output_config.effort. These models answer the budget form,
// thinking.type "enabled" with budget_tokens, with a 400, and they disagree on
// how thinking is turned off, so each one records its rule here.
type AdaptiveThinking struct {
	// Off is the thinking.type that turns thinking off, one of the
	// ClaudeThinking* constants.
	Off string `json:"off,omitempty"`
	// OffMaxLevel is the highest effort Off is accepted at; the API rejects
	// it above that (Opus 5 and Sonnet 5.5 accept it only at "high" or
	// below). Empty means every level.
	OffMaxLevel string `json:"off_max_level,omitempty"`
}

// claudeEffortLevels is the output_config.effort vocabulary of Claude Opus 4.7
// and later, lowest first.
func claudeEffortLevels() []string {
	return []string{"low", "medium", "high", "xhigh", "max"}
}

// adaptiveClaudeThinking describes a Claude model that only takes adaptive
// thinking. off and offMaxLevel fill AdaptiveThinking.
//
// Min and Max stay zero on purpose: that makes the model level-only, so a
// budget written by a translator (they map reasoning effort to budget_tokens)
// is converted back to the nearest effort level instead of being sent.
func adaptiveClaudeThinking(off, offMaxLevel string) *ThinkingSupport {
	return &ThinkingSupport{
		// "none" is only offered for models that can turn thinking off.
		ZeroAllowed: off != ClaudeThinkingAlwaysOn,
		// Adaptive thinking is the model deciding how much to think, which is
		// what a dynamic (-1) budget asks for.
		DynamicAllowed: true,
		Levels:         claudeEffortLevels(),
		Adaptive:       &AdaptiveThinking{Off: off, OffMaxLevel: offMaxLevel},
	}
}
