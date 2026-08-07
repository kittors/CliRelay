package aiaccountstatus

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type RefreshState string

const (
	RefreshIdle    RefreshState = "idle"
	RefreshQueued  RefreshState = "queued"
	RefreshRunning RefreshState = "running"
	RefreshSuccess RefreshState = "success"
	RefreshError   RefreshState = "error"
)

// RefreshRequest is the only client-controlled input for batch refresh.
// Provider/tenant/URL are never accepted from the client.
type RefreshRequest struct {
	AuthIndexes    []string `json:"auth_indexes"`
	AuthSubjectIDs []string `json:"auth_subject_ids"`
	Force          bool     `json:"force"`
}

type RefreshAccepted struct {
	JobID        string   `json:"job_id"`
	Accepted     int      `json:"accepted"`
	Deduplicated int      `json:"deduplicated"`
	Skipped      []string `json:"skipped,omitempty"`
}

type AccountRefreshResult struct {
	AuthIndex     string             `json:"auth_index"`
	AuthSubjectID string             `json:"auth_subject_id"`
	State         RefreshState       `json:"state"`
	ErrorCode     string             `json:"error_code,omitempty"`
	ErrorMessage  string             `json:"error_message,omitempty"`
	UpdatedAt     time.Time          `json:"updated_at,omitempty"`
	Result        *AccountStatusView `json:"result,omitempty"`
}

type JobSnapshot struct {
	JobID     string                 `json:"job_id"`
	TenantID  string                 `json:"tenant_id"`
	State     string                 `json:"state"` // running|completed
	Total     int                    `json:"total"`
	Completed int                    `json:"completed"`
	Failed    int                    `json:"failed"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
	Results   []AccountRefreshResult `json:"results"`
}

type AccountStatusView struct {
	AuthSubjectID             string                        `json:"auth_subject_id"`
	AuthIndex                 string                        `json:"auth_index"`
	Provider                  string                        `json:"provider"`
	StatusScope               string                        `json:"status_scope"`
	SubjectScope              string                        `json:"subject_scope"`
	ShareEligible             bool                          `json:"share_eligible"`
	SubjectSeedKind           string                        `json:"subject_seed_kind"`
	CurrentTenantBindingCount int                           `json:"current_tenant_binding_count"`
	RefreshState              string                        `json:"refresh_state"`
	HealthStatus              string                        `json:"health_status"`
	PlanType                  string                        `json:"plan_type,omitempty"`
	RestrictionSummary        string                        `json:"restriction_summary,omitempty"`
	ErrorSummary              string                        `json:"error_summary,omitempty"`
	ErrorCode                 string                        `json:"error_code,omitempty"`
	ErrorMessage              string                        `json:"error_message,omitempty"`
	Quotas                    []usage.QuotaWindowDTO        `json:"quotas"`
	ResetCreditCount          *int64                        `json:"reset_credit_count,omitempty"`
	ResetCreditExpirations    []string                      `json:"reset_credit_expirations,omitempty"`
	Usage                     usage.AuthSubjectUsageSummary `json:"usage"`
	SubscriptionStartedAt     *time.Time                    `json:"subscription_started_at,omitempty"`
	SubscriptionExpiresAt     *time.Time                    `json:"subscription_expires_at,omitempty"`
	SubscriptionSource        string                        `json:"subscription_source,omitempty"`
	UpstreamCheckedAt         *time.Time                    `json:"upstream_checked_at,omitempty"`
	// QuotaObservedAt dates the quota payload itself. It lags UpstreamCheckedAt
	// whenever probes are failing or returning partial window sets, which is what
	// lets the panel mark quota values as stale instead of implying they are live.
	QuotaObservedAt *time.Time `json:"quota_observed_at,omitempty"`
	UsageUpdatedAt  *time.Time `json:"usage_updated_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	Version         int64      `json:"version"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

type StatusListResponse struct {
	Items []AccountStatusView `json:"items"`
}
