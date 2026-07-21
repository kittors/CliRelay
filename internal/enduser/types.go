package enduser

import "time"

type User struct {
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"display_name"`
	Status             string     `json:"status"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	FailedLoginCount   int        `json:"failed_login_count"`
	LockStage          int        `json:"lock_stage"`
	LockedUntil        *time.Time `json:"locked_until,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	Version            int64      `json:"version"`
	// APIKeyCount is filled on list; 0 means unknown/none.
	APIKeyCount int `json:"api_key_count,omitempty"`
	// DailySpendingUsed is the account-level effective spend for the current project day.
	DailySpendingUsed float64 `json:"daily-spending-used"`
	// DailySpendingResetCount is the number of persisted manual reset events.
	DailySpendingResetCount int `json:"daily-spending-reset-count"`

	// Account-level quota/permissions: shared by every API key under this user.
	PermissionProfileID  string   `json:"permission-profile-id,omitempty"`
	DailyLimit           int      `json:"daily-limit,omitempty"`
	TotalQuota           int      `json:"total-quota,omitempty"`
	SpendingLimit        float64  `json:"spending-limit,omitempty"`
	DailySpendingLimit   float64  `json:"daily-spending-limit,omitempty"`
	ConcurrencyLimit     int      `json:"concurrency-limit,omitempty"`
	RPMLimit             int      `json:"rpm-limit,omitempty"`
	TPMLimit             int      `json:"tpm-limit,omitempty"`
	AllowedModels        []string `json:"allowed-models,omitempty"`
	AllowedChannels      []string `json:"allowed-channels,omitempty"`
	AllowedChannelGroups []string `json:"allowed-channel-groups,omitempty"`
	SystemPrompt         string   `json:"system-prompt,omitempty"`
}

// QuotaPatch updates account-level limits. Nil field = leave unchanged.
// Pointer to empty profile id or zero limit clears that field.
type QuotaPatch struct {
	PermissionProfileID  *string   `json:"permission-profile-id,omitempty"`
	DailyLimit           *int      `json:"daily-limit,omitempty"`
	TotalQuota           *int      `json:"total-quota,omitempty"`
	SpendingLimit        *float64  `json:"spending-limit,omitempty"`
	DailySpendingLimit   *float64  `json:"daily-spending-limit,omitempty"`
	ConcurrencyLimit     *int      `json:"concurrency-limit,omitempty"`
	RPMLimit             *int      `json:"rpm-limit,omitempty"`
	TPMLimit             *int      `json:"tpm-limit,omitempty"`
	AllowedModels        *[]string `json:"allowed-models,omitempty"`
	AllowedChannels      *[]string `json:"allowed-channels,omitempty"`
	AllowedChannelGroups *[]string `json:"allowed-channel-groups,omitempty"`
	SystemPrompt         *string   `json:"system-prompt,omitempty"`
}

type APIKey struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	EndUserID string `json:"end_user_id"`
	Key       string `json:"key,omitempty"`
	KeyMasked string `json:"key_masked,omitempty"`
	Name      string `json:"name"`
	Disabled  bool   `json:"disabled"`
	IsDefault bool   `json:"is_default"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type LoginResult struct {
	AccessToken        string    `json:"access_token"`
	RefreshToken       string    `json:"refresh_token"`
	TokenType          string    `json:"token_type"`
	ExpiresAt          time.Time `json:"expires_at"`
	RefreshExpiresAt   time.Time `json:"refresh_expires_at"`
	User               User      `json:"user"`
	MustChangePassword bool      `json:"must_change_password"`
}

type CreateUserResult struct {
	User              User    `json:"user"`
	GeneratedPassword string  `json:"generated_password,omitempty"`
	DefaultAPIKey     *APIKey `json:"default_api_key,omitempty"`
}

type CreateKeyResult struct {
	APIKey       APIKey `json:"api_key"`
	PlaintextKey string `json:"plaintext_key,omitempty"`
}

type TenantTokenTTL struct {
	AccessSeconds  int
	RefreshSeconds int
}
