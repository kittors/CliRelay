package enduser

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

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
	DailySpendingUsed    float64                `json:"daily-spending-used"`
	LifetimeSpendingUsed float64                `json:"lifetime-spending-used"`
	PeriodSpending       []quota.PeriodSpending `json:"period-spending"`
	CappedKeys           []quota.CappedKey      `json:"capped-keys,omitempty"`
	// DailySpendingResetCount is the number of persisted manual reset events.
	DailySpendingResetCount int `json:"daily-spending-reset-count"`

	// Account-level quota/permissions: shared by every API key under this user.
	PermissionProfileID  string                     `json:"permission-profile-id,omitempty"`
	DailyLimit           int                        `json:"daily-limit,omitempty"`
	TotalQuota           int                        `json:"total-quota,omitempty"`
	SpendingLimit        float64                    `json:"spending-limit,omitempty"`
	DailySpendingLimit   float64                    `json:"daily-spending-limit,omitempty"`
	PeriodSpendingLimits quota.PeriodSpendingLimits `json:"period-spending-limits"`
	ConcurrencyLimit     int                        `json:"concurrency-limit,omitempty"`
	RPMLimit             int                        `json:"rpm-limit,omitempty"`
	TPMLimit             int                        `json:"tpm-limit,omitempty"`
	AllowedModels        []string                   `json:"allowed-models,omitempty"`
	AllowedChannels      []string                   `json:"allowed-channels,omitempty"`
	AllowedChannelGroups []string                   `json:"allowed-channel-groups,omitempty"`
	SystemPrompt         string                     `json:"system-prompt,omitempty"`

	// QuotaLimits 是完整的总额度矩阵（花费/请求/Token × 5h/日/周/月/累计），
	// ModelRules / ModelRoutes 是按模型规则与模型路由。列表与详情返回模板覆盖后的
	// 生效值：绑定模板时就是模板的值，未绑定时就是账号自己的值。
	QuotaLimits quota.Matrix       `json:"quota-limits"`
	ModelRules  []quota.ModelRule  `json:"model-rules"`
	ModelRoutes []quota.ModelRoute `json:"model-routes"`
	// QuotaUsage 是总额度各格与每条规则当前的用量，只在列表等需要展示进度的接口填充。
	QuotaUsage *quota.UsageView `json:"quota-usage,omitempty"`
	// CappedCells 是本次修改连带压低的 Key 额度格子（CappedKeys 只含其中美元周期部分）。
	CappedCells []quota.CappedCell `json:"capped-cells,omitempty"`
}

// QuotaPatch updates account-level limits. Nil field = leave unchanged.
// Pointer to empty profile id or zero limit clears that field.
type QuotaPatch struct {
	PermissionProfileID  *string                          `json:"permission-profile-id,omitempty"`
	DailyLimit           *int                             `json:"daily-limit,omitempty"`
	TotalQuota           *int                             `json:"total-quota,omitempty"`
	SpendingLimit        *float64                         `json:"spending-limit,omitempty"`
	DailySpendingLimit   *float64                         `json:"daily-spending-limit,omitempty"`
	PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits,omitempty"`
	ConcurrencyLimit     *int                             `json:"concurrency-limit,omitempty"`
	RPMLimit             *int                             `json:"rpm-limit,omitempty"`
	TPMLimit             *int                             `json:"tpm-limit,omitempty"`
	AllowedModels        *[]string                        `json:"allowed-models,omitempty"`
	AllowedChannels      *[]string                        `json:"allowed-channels,omitempty"`
	AllowedChannelGroups *[]string                        `json:"allowed-channel-groups,omitempty"`
	SystemPrompt         *string                          `json:"system-prompt,omitempty"`
	// QuotaLimits 是矩阵的局部修改；与上面的旧字段（daily-limit 等）指向同一格时
	// 二者必须一致，否则返回 ErrQuotaLegacyConflict。
	QuotaLimits *quota.MatrixPatch `json:"quota-limits,omitempty"`
	// ModelRules / ModelRoutes 非 nil 时整体替换。
	ModelRules  *[]quota.ModelRule  `json:"model-rules,omitempty"`
	ModelRoutes *[]quota.ModelRoute `json:"model-routes,omitempty"`
}

// KeyPatch 修改账号下的一把 Key。nil 字段表示不修改。
type KeyPatch struct {
	Name *string
	// Disabled 暂停或恢复 Key。暂停保留密钥，恢复后原密钥继续可用；账号至少要留一把
	// 启用的 Key（与删除的规则一致）。
	Disabled *bool
	// Period 是旧版的美元四周期子额度字段；与 QuotaLimits 指向同一格时二者必须一致。
	Period      *quota.PeriodSpendingLimitsPatch
	QuotaLimits *quota.MatrixPatch
	// ModelRules 非 nil 时整体替换 Key 的按模型规则。Key 不配置模型路由：路由决定
	// 请求实际用哪个模型，属于账号/模板层的权限，不随某一把 Key 变化。
	ModelRules *[]quota.ModelRule
}

type APIKey struct {
	ID                      string                     `json:"id"`
	TenantID                string                     `json:"tenant_id"`
	EndUserID               string                     `json:"end_user_id"`
	Key                     string                     `json:"key,omitempty"`
	KeyMasked               string                     `json:"key_masked,omitempty"`
	Name                    string                     `json:"name"`
	Disabled                bool                       `json:"disabled"`
	IsDefault               bool                       `json:"is_default"`
	CreatedAt               string                     `json:"created_at,omitempty"`
	UpdatedAt               string                     `json:"updated_at,omitempty"`
	DailySpendingLimit      float64                    `json:"daily-spending-limit"`
	PeriodSpendingLimits    quota.PeriodSpendingLimits `json:"period-spending-limits"`
	PeriodSpending          []quota.PeriodSpending     `json:"period-spending"`
	DailySpendingUsed       float64                    `json:"daily-spending-used"`
	LifetimeSpendingUsed    float64                    `json:"lifetime-spending-used"`
	DailySpendingResetCount int                        `json:"daily-spending-reset-count"`
	// QuotaLimits 是 Key 的子额度矩阵（每格不超过账号同一格），ModelRules 是 Key
	// 自己的按模型规则；QuotaUsage 是二者当前的用量。
	QuotaLimits quota.Matrix      `json:"quota-limits"`
	ModelRules  []quota.ModelRule `json:"model-rules"`
	QuotaUsage  *quota.UsageView  `json:"quota-usage,omitempty"`
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
