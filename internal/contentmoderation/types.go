package contentmoderation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ModeOff      = "off"
	ModePreBlock = "pre_block"

	KeywordModeAPIOnly       = "api_only"
	KeywordModeKeywordOnly   = "keyword_only"
	KeywordModeKeywordAndAPI = "keyword_and_api"

	// Moderation upstreams. "backend" rather than "provider": provider already
	// names an AI upstream channel in this codebase (ChannelTypeProvider,
	// provider_config_id), and reusing it here would collide in logs and DTOs.
	BackendOpenAIModerations = "openai_moderations"
	BackendQwen3Guard        = "qwen3guard"

	// How a Qwen3Guard "Controversial" verdict maps onto our two-state
	// allow/block model.
	ControversialActionAllow        = "allow"
	ControversialActionBlock        = "block"
	ControversialActionElevatedOnly = "elevated_only"

	ChannelTypeAuthFile    = "auth_file"
	ChannelTypeProviderKey = "provider_key"
	ChannelTypeProvider    = "provider"

	DefaultBaseURL      = "https://api.openai.com"
	DefaultModel        = "omni-moderation-latest"
	DefaultTimeoutMS    = 3000
	DefaultBlockStatus  = 403
	DefaultBlockMessage = "Your request was blocked by the content moderation policy."

	DefaultInputLimit = 4000
	DefaultMaxChunks  = 4
	MinInputLimit     = 128
	MaxInputLimit     = 100000
	MaxChunksLimit    = 32
)

var (
	ErrUnavailable     = errors.New("content moderation store unavailable")
	ErrNotFound        = errors.New("content moderation profile not found")
	ErrNameConflict    = errors.New("content moderation profile name already exists")
	ErrVersionConflict = errors.New("content moderation profile version conflict")
	ErrProfileBound    = errors.New("content moderation profile has channel bindings")
	ErrBindingConflict = errors.New("content moderation channel is already bound")
	ErrInvalidChannel  = errors.New("invalid content moderation channel")
)

type Profile struct {
	TenantID     string `json:"-"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Mode         string `json:"mode"`
	Backend      string `json:"backend"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKeySecret string `json:"-"`
	// TimeoutMS is the budget for the whole evaluation, not for one HTTP call:
	// the Qwen3Guard backend may issue several chunk requests under it.
	TimeoutMS       int                `json:"timeout_ms"`
	KeywordMode     string             `json:"keyword_mode"`
	BlockedKeywords []string           `json:"blocked_keywords"`
	Thresholds      map[string]float64 `json:"thresholds"`
	// Scanners lists the Qwen3Guard categories this profile acts on. Empty
	// means every category in the catalog.
	Scanners            []string  `json:"scanners"`
	ControversialAction string    `json:"controversial_action"`
	ElevatedCategories  []string  `json:"elevated_categories"`
	InputLimit          int       `json:"input_limit"`
	MaxChunks           int       `json:"max_chunks"`
	BlockHTTPStatus     int       `json:"block_http_status"`
	BlockMessage        string    `json:"block_message"`
	Version             int64     `json:"version"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type CreateProfileInput struct {
	Name                string             `json:"name"`
	Mode                string             `json:"mode"`
	Backend             string             `json:"backend"`
	BaseURL             string             `json:"base_url"`
	Model               string             `json:"model"`
	APIKey              string             `json:"api_key"`
	TimeoutMS           int                `json:"timeout_ms"`
	KeywordMode         string             `json:"keyword_mode"`
	BlockedKeywords     []string           `json:"blocked_keywords"`
	Thresholds          map[string]float64 `json:"thresholds"`
	Scanners            []string           `json:"scanners"`
	ControversialAction string             `json:"controversial_action"`
	ElevatedCategories  []string           `json:"elevated_categories"`
	InputLimit          int                `json:"input_limit"`
	MaxChunks           int                `json:"max_chunks"`
	BlockHTTPStatus     int                `json:"block_http_status"`
	BlockMessage        string             `json:"block_message"`
}

type PatchProfileInput struct {
	Name                *string             `json:"name"`
	Mode                *string             `json:"mode"`
	Backend             *string             `json:"backend"`
	BaseURL             *string             `json:"base_url"`
	Model               *string             `json:"model"`
	APIKey              *string             `json:"api_key"`
	ClearAPIKey         bool                `json:"clear_api_key"`
	TimeoutMS           *int                `json:"timeout_ms"`
	KeywordMode         *string             `json:"keyword_mode"`
	BlockedKeywords     *[]string           `json:"blocked_keywords"`
	Thresholds          *map[string]float64 `json:"thresholds"`
	Scanners            *[]string           `json:"scanners"`
	ControversialAction *string             `json:"controversial_action"`
	ElevatedCategories  *[]string           `json:"elevated_categories"`
	InputLimit          *int                `json:"input_limit"`
	MaxChunks           *int                `json:"max_chunks"`
	BlockHTTPStatus     *int                `json:"block_http_status"`
	BlockMessage        *string             `json:"block_message"`
	Version             int64               `json:"version"`
}

type Binding struct {
	TenantID    string    `json:"-"`
	ChannelType string    `json:"channel_type"`
	ChannelID   string    `json:"channel_id"`
	ProfileID   string    `json:"profile_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type BindingOperation struct {
	ChannelType string  `json:"channel_type"`
	ChannelID   string  `json:"channel_id"`
	ProfileID   *string `json:"profile_id"`
}

type BindingConflictError struct {
	ChannelType       string
	ChannelID         string
	ExistingProfileID string
}

func (e *BindingConflictError) Error() string {
	return fmt.Sprintf("%s %s is already bound to profile %s", e.ChannelType, e.ChannelID, e.ExistingProfileID)
}

func (e *BindingConflictError) Unwrap() error { return ErrBindingConflict }

type ProfileBoundError struct {
	Count int
}

func (e *ProfileBoundError) Error() string {
	return fmt.Sprintf("profile has %d channel binding(s)", e.Count)
}

func (e *ProfileBoundError) Unwrap() error { return ErrProfileBound }

func DefaultThresholds() map[string]float64 {
	return map[string]float64{
		"harassment":             0.98,
		"harassment/threatening": 0.90,
		"hate":                   0.65,
		"hate/threatening":       0.65,
		"illicit":                0.95,
		"illicit/violent":        0.95,
		"self-harm":              0.65,
		"self-harm/intent":       0.85,
		"self-harm/instructions": 0.65,
		"sexual":                 0.65,
		"sexual/minors":          0.65,
		"violence":               0.95,
		"violence/graphic":       0.95,
	}
}

func NewProfile(tenantID, id string, input CreateProfileInput, now time.Time) (Profile, error) {
	elevated := input.ElevatedCategories
	// A missing field means "use the recommended set"; an explicitly empty list
	// means "never escalate on Controversial" and must survive.
	if elevated == nil {
		elevated = DefaultElevatedCategories()
	}
	profile := Profile{
		TenantID:            strings.TrimSpace(tenantID),
		ID:                  strings.TrimSpace(id),
		Name:                input.Name,
		Mode:                input.Mode,
		Backend:             input.Backend,
		BaseURL:             input.BaseURL,
		Model:               input.Model,
		APIKeySecret:        input.APIKey,
		TimeoutMS:           input.TimeoutMS,
		KeywordMode:         input.KeywordMode,
		BlockedKeywords:     input.BlockedKeywords,
		Thresholds:          input.Thresholds,
		Scanners:            input.Scanners,
		ControversialAction: input.ControversialAction,
		ElevatedCategories:  elevated,
		InputLimit:          input.InputLimit,
		MaxChunks:           input.MaxChunks,
		BlockHTTPStatus:     input.BlockHTTPStatus,
		BlockMessage:        input.BlockMessage,
		Version:             1,
		CreatedAt:           now.UTC(),
		UpdatedAt:           now.UTC(),
	}
	applyProfileDefaults(&profile)
	return profile, ValidateProfile(profile)
}

func ApplyProfilePatch(profile Profile, patch PatchProfileInput, now time.Time) (Profile, error) {
	if patch.Version <= 0 || patch.Version != profile.Version {
		return Profile{}, ErrVersionConflict
	}
	if patch.Name != nil {
		profile.Name = *patch.Name
	}
	if patch.Mode != nil {
		profile.Mode = *patch.Mode
	}
	if patch.Backend != nil {
		profile.Backend = *patch.Backend
	}
	if patch.BaseURL != nil {
		profile.BaseURL = *patch.BaseURL
	}
	if patch.Model != nil {
		profile.Model = *patch.Model
	}
	if patch.APIKey != nil {
		if key := strings.TrimSpace(*patch.APIKey); key != "" {
			profile.APIKeySecret = key
		}
	}
	if patch.ClearAPIKey {
		profile.APIKeySecret = ""
	}
	if patch.TimeoutMS != nil {
		profile.TimeoutMS = *patch.TimeoutMS
	}
	if patch.KeywordMode != nil {
		profile.KeywordMode = *patch.KeywordMode
	}
	if patch.BlockedKeywords != nil {
		profile.BlockedKeywords = *patch.BlockedKeywords
	}
	if patch.Thresholds != nil {
		profile.Thresholds = *patch.Thresholds
	}
	if patch.Scanners != nil {
		profile.Scanners = *patch.Scanners
	}
	if patch.ControversialAction != nil {
		profile.ControversialAction = *patch.ControversialAction
	}
	if patch.ElevatedCategories != nil {
		profile.ElevatedCategories = *patch.ElevatedCategories
	}
	if patch.InputLimit != nil {
		profile.InputLimit = *patch.InputLimit
	}
	if patch.MaxChunks != nil {
		profile.MaxChunks = *patch.MaxChunks
	}
	if patch.BlockHTTPStatus != nil {
		profile.BlockHTTPStatus = *patch.BlockHTTPStatus
	}
	if patch.BlockMessage != nil {
		profile.BlockMessage = *patch.BlockMessage
	}
	applyProfileDefaults(&profile)
	profile.Version++
	profile.UpdatedAt = now.UTC()
	return profile, ValidateProfile(profile)
}

func applyProfileDefaults(profile *Profile) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Mode = strings.ToLower(strings.TrimSpace(profile.Mode))
	if profile.Mode == "" {
		profile.Mode = ModeOff
	}
	profile.Backend = strings.ToLower(strings.TrimSpace(profile.Backend))
	if profile.Backend == "" {
		profile.Backend = BackendOpenAIModerations
	}
	profile.BaseURL = strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/")
	profile.Model = strings.TrimSpace(profile.Model)
	// The OpenAI defaults are meaningless for a guard model, so they are only
	// applied to the OpenAI backend. A qwen3guard profile with no endpoint is
	// rejected by ValidateProfile once it is switched to pre_block.
	if profile.Backend == BackendOpenAIModerations {
		if profile.BaseURL == "" {
			profile.BaseURL = DefaultBaseURL
		}
		if profile.Model == "" {
			profile.Model = DefaultModel
		}
	}
	profile.APIKeySecret = strings.TrimSpace(profile.APIKeySecret)
	if profile.TimeoutMS == 0 {
		profile.TimeoutMS = DefaultTimeoutMS
	}
	profile.KeywordMode = strings.ToLower(strings.TrimSpace(profile.KeywordMode))
	if profile.KeywordMode == "" {
		profile.KeywordMode = KeywordModeAPIOnly
	}
	profile.BlockedKeywords = normalizeKeywords(profile.BlockedKeywords)
	profile.Thresholds = mergeThresholds(profile.Thresholds)
	profile.Scanners = normalizeScannerList(profile.Scanners)
	profile.ControversialAction = strings.ToLower(strings.TrimSpace(profile.ControversialAction))
	if profile.ControversialAction == "" {
		profile.ControversialAction = ControversialActionElevatedOnly
	}
	profile.ElevatedCategories = normalizeScannerList(profile.ElevatedCategories)
	if profile.InputLimit == 0 {
		profile.InputLimit = DefaultInputLimit
	}
	if profile.MaxChunks == 0 {
		profile.MaxChunks = DefaultMaxChunks
	}
	if profile.BlockHTTPStatus == 0 {
		profile.BlockHTTPStatus = DefaultBlockStatus
	}
	profile.BlockMessage = strings.TrimSpace(profile.BlockMessage)
	if profile.BlockMessage == "" {
		profile.BlockMessage = DefaultBlockMessage
	}
}

func ValidateProfile(profile Profile) error {
	if profile.TenantID == "" || profile.ID == "" {
		return errors.New("tenant and profile id are required")
	}
	if profile.Name == "" {
		return errors.New("name is required")
	}
	if profile.Mode != ModeOff && profile.Mode != ModePreBlock {
		return errors.New("mode must be off or pre_block")
	}
	switch profile.Backend {
	case BackendOpenAIModerations, BackendQwen3Guard:
	default:
		return errors.New("backend must be openai_moderations or qwen3guard")
	}
	switch profile.KeywordMode {
	case KeywordModeAPIOnly, KeywordModeKeywordOnly, KeywordModeKeywordAndAPI:
	default:
		return errors.New("keyword_mode must be api_only, keyword_only, or keyword_and_api")
	}
	switch profile.ControversialAction {
	case ControversialActionAllow, ControversialActionBlock, ControversialActionElevatedOnly:
	default:
		return errors.New("controversial_action must be allow, block, or elevated_only")
	}
	if profile.TimeoutMS <= 0 || profile.TimeoutMS > 30000 {
		return errors.New("timeout_ms must be between 1 and 30000")
	}
	if profile.InputLimit < MinInputLimit || profile.InputLimit > MaxInputLimit {
		return fmt.Errorf("input_limit must be between %d and %d", MinInputLimit, MaxInputLimit)
	}
	if profile.MaxChunks < 1 || profile.MaxChunks > MaxChunksLimit {
		return fmt.Errorf("max_chunks must be between 1 and %d", MaxChunksLimit)
	}
	if profile.BlockHTTPStatus < 400 || profile.BlockHTTPStatus > 599 {
		return errors.New("block_http_status must be between 400 and 599")
	}
	// Only the API path needs an upstream; keyword_only never leaves the process.
	if profile.Mode == ModePreBlock && profile.KeywordMode != KeywordModeKeywordOnly {
		switch profile.Backend {
		case BackendOpenAIModerations:
			if profile.APIKeySecret == "" {
				return errors.New("api_key is required for API moderation in pre_block mode")
			}
		case BackendQwen3Guard:
			// Self-hosted guard endpoints (vLLM, SGLang, Ollama) commonly run
			// without auth, so api_key stays optional here; the endpoint
			// address and model do not have usable defaults and are required.
			if profile.BaseURL == "" {
				return errors.New("base_url is required for qwen3guard in pre_block mode")
			}
			if profile.Model == "" {
				return errors.New("model is required for qwen3guard in pre_block mode")
			}
		}
	}
	if profile.BaseURL != "" {
		if _, err := normalizeModerationBaseURL(profile.BaseURL); err != nil {
			return err
		}
	}
	for category, threshold := range profile.Thresholds {
		if strings.TrimSpace(category) == "" || threshold < 0 || threshold > 1 {
			return fmt.Errorf("invalid threshold for category %q", category)
		}
	}
	for _, category := range profile.Scanners {
		if !IsScannerID(category) {
			return fmt.Errorf("unknown scanner category %q", category)
		}
	}
	for _, category := range profile.ElevatedCategories {
		if !IsScannerID(category) {
			return fmt.Errorf("unknown elevated category %q", category)
		}
	}
	return nil
}

func normalizeKeywords(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func mergeThresholds(overrides map[string]float64) map[string]float64 {
	out := DefaultThresholds()
	for category, threshold := range overrides {
		category = strings.TrimSpace(category)
		if category != "" {
			out[category] = threshold
		}
	}
	return out
}

func IsChannelType(value string) bool {
	switch strings.TrimSpace(value) {
	case ChannelTypeAuthFile, ChannelTypeProviderKey, ChannelTypeProvider:
		return true
	default:
		return false
	}
}

func MaskSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return "****"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}
