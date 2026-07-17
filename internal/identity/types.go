package identity

import "time"

const (
	SystemTenantID = "00000000-0000-0000-0000-000000000001"
	SystemRoleID   = "00000000-0000-0000-0000-000000000002"
	SystemUserID   = "00000000-0000-0000-0000-000000000003"
)

type Tenant struct {
	ID                     string     `json:"id"`
	Slug                   string     `json:"slug"`
	Name                   string     `json:"name"`
	Type                   string     `json:"type"`
	Status                 string     `json:"status"`
	EffectiveStatus        string     `json:"effective_status"`
	ExpiresAt              *time.Time `json:"expires_at"`
	Description            string     `json:"description"`
	AccessTokenTTLSeconds  int        `json:"access_token_ttl_seconds"`
	RefreshTokenTTLSeconds int        `json:"refresh_token_ttl_seconds"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	Version                int64      `json:"version"`
}

type User struct {
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"display_name"`
	Status             string     `json:"status"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	RoleIDs            []string   `json:"role_ids"`
	RoleCodes          []string   `json:"role_codes"`
	Version            int64      `json:"version"`
}

type Role struct {
	ID              string   `json:"id"`
	TenantID        string   `json:"tenant_id"`
	Code            string   `json:"code"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Scope           string   `json:"scope"`
	SystemProtected bool     `json:"system_protected"`
	Permissions     []string `json:"permissions"`
	Version         int64    `json:"version"`
}

type Principal struct {
	Kind             string          `json:"kind"`
	User             User            `json:"user"`
	HomeTenant       Tenant          `json:"home_tenant"`
	EffectiveTenant  Tenant          `json:"effective_tenant"`
	Roles            []Role          `json:"roles"`
	Menus            []Menu          `json:"menus"`
	Permissions      map[string]bool `json:"-"`
	PermissionList   []string        `json:"permissions"`
	PlatformAdmin    bool            `json:"platform_admin"`
	SessionID        string          `json:"session_id,omitempty"`
	SessionExpiresAt *time.Time      `json:"session_expires_at,omitempty"`
}

func (p Principal) Has(permission string) bool {
	return p.PlatformAdmin || p.Permissions[permission]
}

type LoginResult struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	TokenType        string    `json:"token_type"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	Principal        Principal `json:"principal"`
}

type AuditEvent struct {
	TenantID       string
	ActorKind      string
	ActorUserID    string
	ActorSessionID string
	Action         string
	ResourceType   string
	ResourceID     string
	Result         string
	RequestID      string
	// Changes stores structured call-chain / mutation metadata (JSON object).
	Changes map[string]any
}
