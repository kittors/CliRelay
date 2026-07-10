package identityfingerprint

import "time"

type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
	ProviderGemini Provider = "gemini"
	ProviderXAI    Provider = "xai"
)

type FieldSource string

const (
	FieldSourceLearned        FieldSource = "learned"
	FieldSourcePreset         FieldSource = "preset"
	FieldSourceBuiltinDefault FieldSource = "builtin_default"

	// Deprecated aliases kept for older callers; new responses emit preset or builtin_default.
	FieldSourceCustom  = FieldSourcePreset
	FieldSourceDefault = FieldSourceBuiltinDefault
)

type FieldValue struct {
	Value  string      `json:"value"`
	Source FieldSource `json:"source"`
}

type LearnedRecord struct {
	Provider        Provider          `json:"provider"`
	AccountKey      string            `json:"account_key"`
	AuthSubjectID   string            `json:"auth_subject_id,omitempty"`
	ClientProduct   string            `json:"client_product,omitempty"`
	ClientVariant   string            `json:"client_variant,omitempty"`
	Version         string            `json:"version,omitempty"`
	Fields          map[string]string `json:"fields"`
	ObservedHeaders map[string]string `json:"observed_headers,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	LastSeenAt      time.Time         `json:"last_seen_at"`
}

type EffectiveFingerprint struct {
	Provider      Provider              `json:"provider"`
	AccountKey    string                `json:"account_key,omitempty"`
	AuthSubjectID string                `json:"auth_subject_id,omitempty"`
	Enabled       bool                  `json:"enabled"`
	ClientProduct string                `json:"client_product,omitempty"`
	Version       string                `json:"version,omitempty"`
	Fields        map[string]FieldValue `json:"fields"`
	Learned       *LearnedRecord        `json:"learned,omitempty"`
}

type Observation struct {
	Provider        Provider
	AccountKey      string
	AuthSubjectID   string
	ClientProduct   string
	ClientVariant   string
	Version         string
	Fields          map[string]string
	ObservedHeaders map[string]string
	ObservedAt      time.Time
}

type LearnInput struct {
	Provider      Provider
	AccountKey    string
	AuthSubjectID string
	Headers       map[string][]string
	ObservedAt    time.Time
}

type MergeResult struct {
	Record  *LearnedRecord
	Changed bool
	Reason  string
}
