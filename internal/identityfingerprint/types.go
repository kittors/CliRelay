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
	ProfileKey      string            `json:"profile_key"`
	ProfileFamily   string            `json:"profile_family,omitempty"`
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
	ProfileKey    string                `json:"profile_key,omitempty"`
	ProfileFamily string                `json:"profile_family,omitempty"`
	ClientVariant string                `json:"client_variant,omitempty"`
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
	ProfileKey      string
	ProfileFamily   string
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

type AccountStrategy string

const (
	AccountStrategyCLIPreferred  AccountStrategy = "cli_preferred"
	AccountStrategyActiveProfile AccountStrategy = "active_profile"
)

type AccountPolicy struct {
	Provider         Provider        `json:"provider"`
	AccountKey       string          `json:"account_key"`
	Strategy         AccountStrategy `json:"strategy"`
	ActiveProfileKey string          `json:"active_profile_key,omitempty"`
	Revision         int64           `json:"revision"`
	UpdatedAt        time.Time       `json:"updated_at,omitempty"`
}

type ProfileSelection struct {
	Profile *LearnedRecord `json:"profile,omitempty"`
	Policy  AccountPolicy  `json:"policy"`
	Reason  string         `json:"reason"`
}
