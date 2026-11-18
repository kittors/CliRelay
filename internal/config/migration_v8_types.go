package config

// RequestScopedErrorRule configures custom classification and handling for upstream errors.
type RequestScopedErrorRule struct {
	Status      int      `yaml:"status,omitempty" json:"status,omitempty"`
	Match       []string `yaml:"match,omitempty" json:"match,omitempty"`
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	Action      string   `yaml:"action,omitempty" json:"action,omitempty"`
}

// XAIKey uses the Codex API key structure for native xAI execution.
type XAIKey = CodexKey
type XAIModel = CodexModel

// MetaKey uses the Codex API key structure for native Meta execution.
type MetaKey = CodexKey
