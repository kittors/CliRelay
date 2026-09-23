package registry

// Latest-generation Claude model definitions.
//
// Kept out of model_definitions_static_data.go because that file is frozen at
// its current size by the structure ratchet in
// scripts/check-backend-structure.py and may only shrink. Follows the same
// convention as model_definitions_commandcode.go.
//
// These ids are served by Anthropic's Claude Code OAuth surface but are absent
// from the upstream static snapshot, so they are declared here to carry the
// context window, completion cap and reasoning levels the pi client needs.

// latestClaudeModels returns Claude models newer than the upstream static
// snapshot. Unexported: callers use GetClaudeModels, which includes these.
func latestClaudeModels() []*ModelInfo {
	thinking := func() *ThinkingSupport {
		return &ThinkingSupport{
			Min:            1024,
			Max:            128000,
			ZeroAllowed:    true,
			DynamicAllowed: false,
			Levels:         []string{"low", "medium", "high", "xhigh", "max"},
		}
	}

	return []*ModelInfo{
		{
			ID:                  "claude-opus-5-5",
			Object:              "model",
			Created:             1790035200, // 2026-09-22
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Opus 5.5",
			Description:         "Latest generation Claude Opus frontier model",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            thinking(),
		},
		{
			ID:                  "claude-opus-5",
			Object:              "model",
			Created:             1783641600, // 2026-07-13
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Opus 5",
			Description:         "Previous generation Claude Opus frontier model",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            thinking(),
		},
		{
			ID:                  "claude-sonnet-5",
			Object:              "model",
			Created:             1783641600, // 2026-07-13
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Sonnet 5",
			Description:         "Latest generation Claude Sonnet balanced model",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            thinking(),
		},
		{
			ID:                  "claude-fable-5-1",
			Object:              "model",
			Created:             1789521272, // 2026-09-15
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Fable 5.1",
			Description:         "Claude Fable long-horizon agentic model",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            thinking(),
		},
		{
			ID:                  "claude-fable-5",
			Object:              "model",
			Created:             1781740800, // 2026-06-21
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Fable 5",
			Description:         "Claude Fable long-horizon agentic model",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            thinking(),
		},
	}
}

// GetClaudeModels returns every Claude model definition, latest generation first
// so newer ids win on de-duplication.
//
// The package's only Claude accessor. It lives here rather than beside the
// snapshot because the structure ratchet freezes that file's size.
func GetClaudeModels() []*ModelInfo {
	latest := latestClaudeModels()
	base := claudeStaticSnapshot()
	out := make([]*ModelInfo, 0, len(latest)+len(base))
	out = append(out, latest...)
	out = append(out, base...)
	return out
}
