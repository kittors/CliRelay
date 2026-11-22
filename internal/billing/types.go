package billing

import (
	"errors"
	"fmt"
	"time"
)

const (
	ProviderXunhuPay = "xunhupay"
	ProviderManual   = "manual"

	ChannelWeChat = "wechat"
	ChannelAlipay = "alipay"
	ChannelManual = "manual"

	DefaultXunhuGatewayURL = "https://api.xunhupay.com/payment/do.html"
	DefaultXunhuQueryURL   = "https://api.xunhupay.com/payment/query.html"
	DefaultOrderExpireMins = 15
	DefaultCurrency        = "CNY"
)

type PlanKind string

const (
	PlanKindSubscription PlanKind = "subscription"
	PlanKindQuotaPack    PlanKind = "quota_pack"
	PlanKindCustomQuota  PlanKind = "custom_quota"
	PlanKindDailyReset   PlanKind = "daily_reset"
)

type TargetKind string

const (
	TargetKindEndUser TargetKind = "end_user"
	TargetKindAPIKey  TargetKind = "api_key"
)

type OrderStatus string

const (
	OrderStatusPending   OrderStatus = "pending"
	OrderStatusPaid      OrderStatus = "paid"
	OrderStatusFulfilled OrderStatus = "fulfilled"
	OrderStatusFailed    OrderStatus = "failed"
	OrderStatusExpired   OrderStatus = "expired"
	OrderStatusClosed    OrderStatus = "closed"
)

var (
	ErrUnavailable       = errors.New("billing: runtime database unavailable")
	ErrNotFound          = errors.New("billing: not found")
	ErrDisabled          = errors.New("billing: payment is not enabled")
	ErrChannelDisabled   = errors.New("billing: selected payment channel is not enabled")
	ErrInvalidSignature  = errors.New("billing: invalid provider signature")
	ErrAmountMismatch    = errors.New("billing: payment amount mismatch")
	ErrInvalidTransition = errors.New("billing: invalid order status transition")
	ErrProviderFailed    = errors.New("billing: payment provider request failed")
)

// FieldError names the single field rejected by input validation.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func fieldErr(field, message string) error {
	return &FieldError{Field: field, Message: message}
}

// Settings is the stored per-tenant payment configuration (contains plaintext secrets).
type Settings struct {
	TenantID           string    `json:"tenant_id"`
	Enabled            bool      `json:"enabled"`
	Provider           string    `json:"provider"`
	PublicBaseURL      string    `json:"public_base_url"`
	XunhuGatewayURL    string    `json:"xunhu_gateway_url"`
	XunhuQueryURL      string    `json:"xunhu_query_url"`
	XunhuWeChatEnabled bool      `json:"xunhu_wechat_enabled"`
	XunhuWeChatAppID   string    `json:"xunhu_wechat_appid"`
	XunhuWeChatSecret  string    `json:"xunhu_wechat_appsecret"`
	XunhuAlipayEnabled bool      `json:"xunhu_alipay_enabled"`
	XunhuAlipayAppID   string    `json:"xunhu_alipay_appid"`
	XunhuAlipaySecret  string    `json:"xunhu_alipay_appsecret"`
	OrderExpireMinutes int       `json:"order_expire_minutes"`
	Currency           string    `json:"currency"`
	UpdatedAt          time.Time `json:"updated_at"`
	UpdatedBy          string    `json:"updated_by,omitempty"`
}

// SettingsView is the operator-facing settings view with secrets masked and callback URLs derived.
type SettingsView struct {
	Enabled            bool       `json:"enabled"`
	Provider           string     `json:"provider"`
	PublicBaseURL      string     `json:"public_base_url"`
	XunhuGatewayURL    string     `json:"xunhu_gateway_url"`
	XunhuQueryURL      string     `json:"xunhu_query_url"`
	XunhuWeChatEnabled bool       `json:"xunhu_wechat_enabled"`
	XunhuWeChatAppID   string     `json:"xunhu_wechat_appid"`
	XunhuWeChatSecret  string     `json:"xunhu_wechat_appsecret"`
	XunhuWeChatHasKey  bool       `json:"xunhu_wechat_has_secret"`
	XunhuAlipayEnabled bool       `json:"xunhu_alipay_enabled"`
	XunhuAlipayAppID   string     `json:"xunhu_alipay_appid"`
	XunhuAlipaySecret  string     `json:"xunhu_alipay_appsecret"`
	XunhuAlipayHasKey  bool       `json:"xunhu_alipay_has_secret"`
	OrderExpireMinutes int        `json:"order_expire_minutes"`
	Currency           string     `json:"currency"`
	WeChatNotifyURL    string     `json:"wechat_notify_url"`
	AlipayNotifyURL    string     `json:"alipay_notify_url"`
	Persistent         bool       `json:"persistent"`
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`
	UpdatedBy          string     `json:"updated_by,omitempty"`
}

// SettingsPatch carries a partial update; nil fields keep stored values and
// secrets containing "****" keep their stored value.
type SettingsPatch struct {
	Enabled            *bool   `json:"enabled"`
	Provider           *string `json:"provider"`
	PublicBaseURL      *string `json:"public_base_url"`
	XunhuGatewayURL    *string `json:"xunhu_gateway_url"`
	XunhuQueryURL      *string `json:"xunhu_query_url"`
	XunhuWeChatEnabled *bool   `json:"xunhu_wechat_enabled"`
	XunhuWeChatAppID   *string `json:"xunhu_wechat_appid"`
	XunhuWeChatSecret  *string `json:"xunhu_wechat_appsecret"`
	XunhuAlipayEnabled *bool   `json:"xunhu_alipay_enabled"`
	XunhuAlipayAppID   *string `json:"xunhu_alipay_appid"`
	XunhuAlipaySecret  *string `json:"xunhu_alipay_appsecret"`
	OrderExpireMinutes *int    `json:"order_expire_minutes"`
}

// TestChannelRequest tests either saved settings or a draft channel config.
type TestChannelRequest struct {
	Channel    string `json:"channel"`
	GatewayURL string `json:"gateway_url,omitempty"`
	QueryURL   string `json:"query_url,omitempty"`
	AppID      string `json:"appid,omitempty"`
	AppSecret  string `json:"appsecret,omitempty"`
}

type TestChannelResult struct {
	Channel    string `json:"channel"`
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	DurationMs int64  `json:"duration_ms"`
}

// Plan defines a reusable billing product.
type Plan struct {
	ID                  string    `json:"id"`
	TenantID            string    `json:"tenant_id"`
	Name                string    `json:"name"`
	Description         string    `json:"description"`
	Kind                PlanKind  `json:"kind"`
	PriceCents          int       `json:"price_cents"`
	DurationDays        int       `json:"duration_days"`
	QuotaAmountUSD      float64   `json:"quota_amount_usd"`
	QuotaRequests       int64     `json:"quota_requests"`
	QuotaTokens         int64     `json:"quota_tokens"`
	RateUSDPerCNY       float64   `json:"rate_usd_per_cny"`
	MinAmountCents      int       `json:"min_amount_cents"`
	MaxAmountCents      int       `json:"max_amount_cents"`
	PermissionProfileID string    `json:"permission_profile_id"`
	Badge               string    `json:"badge"`
	SortOrder           int       `json:"sort_order"`
	Enabled             bool      `json:"enabled"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type PlanInput struct {
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Kind                PlanKind `json:"kind"`
	PriceCents          int      `json:"price_cents"`
	DurationDays        int      `json:"duration_days"`
	QuotaAmountUSD      float64  `json:"quota_amount_usd"`
	QuotaRequests       int64    `json:"quota_requests"`
	QuotaTokens         int64    `json:"quota_tokens"`
	RateUSDPerCNY       float64  `json:"rate_usd_per_cny"`
	MinAmountCents      int      `json:"min_amount_cents"`
	MaxAmountCents      int      `json:"max_amount_cents"`
	PermissionProfileID string   `json:"permission_profile_id"`
	Badge               string   `json:"badge"`
	SortOrder           int      `json:"sort_order"`
	Enabled             *bool    `json:"enabled"`
}

// Binding attaches plans and an optional shareable direct-pay QR code to an
// end-user account or a specific API key.
type Binding struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"tenant_id"`
	TargetKind     TargetKind     `json:"target_kind"`
	TargetID       string         `json:"target_id"`
	EndUserID      string         `json:"end_user_id"`
	Enabled        bool           `json:"enabled"`
	PlanIDs        []string       `json:"plan_ids"`
	PriceOverrides map[string]int `json:"price_overrides"`
	PublicCode     string         `json:"public_code"`
	AllowPublicPay bool           `json:"allow_public_pay"`
	Note           string         `json:"note"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type BindingInput struct {
	TargetKind     TargetKind     `json:"target_kind"`
	TargetID       string         `json:"target_id"`
	Enabled        bool           `json:"enabled"`
	PlanIDs        []string       `json:"plan_ids"`
	PriceOverrides map[string]int `json:"price_overrides"`
	AllowPublicPay bool           `json:"allow_public_pay"`
	Note           string         `json:"note"`
	// ExpiresAt optionally lets an operator directly set or clear the target's expiry.
	SetExpiresAt bool       `json:"set_expires_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// BindingView enriches a binding with display metadata and live quota/expiry state.
type BindingView struct {
	Binding
	TargetLabel      string     `json:"target_label"`
	TargetSubtitle   string     `json:"target_subtitle,omitempty"`
	KeyMasked        string     `json:"key_masked,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	LifetimeLimitUSD float64    `json:"lifetime_limit_usd"`
	PublicPayURL     string     `json:"public_pay_url,omitempty"`
}

// Order represents a payment checkout record and its fulfillment result.
type Order struct {
	ID                 string      `json:"id"`
	OrderNo            string      `json:"order_no"`
	TenantID           string      `json:"tenant_id"`
	BindingID          string      `json:"binding_id"`
	TargetKind         TargetKind  `json:"target_kind"`
	TargetID           string      `json:"target_id"`
	TargetLabel        string      `json:"target_label"`
	EndUserID          string      `json:"end_user_id"`
	PlanID             string      `json:"plan_id"`
	PlanName           string      `json:"plan_name"`
	PlanKind           PlanKind    `json:"plan_kind"`
	PlanSnapshot       Plan        `json:"plan_snapshot"`
	Provider           string      `json:"provider"`
	PayChannel         string      `json:"pay_channel"`
	AmountCents        int         `json:"amount_cents"`
	Currency           string      `json:"currency"`
	Status             OrderStatus `json:"status"`
	ProviderOrderID    string      `json:"provider_order_id,omitempty"`
	ProviderTradeNo    string      `json:"provider_trade_no,omitempty"`
	QRCodeURL          string      `json:"qrcode_url,omitempty"`
	PayURL             string      `json:"pay_url,omitempty"`
	FulfillmentSummary string      `json:"fulfillment_summary,omitempty"`
	FulfillmentError   string      `json:"fulfillment_error,omitempty"`
	Source             string      `json:"source"`
	CreatedBy          string      `json:"created_by,omitempty"`
	ExpiresAt          time.Time   `json:"expires_at"`
	PaidAt             *time.Time  `json:"paid_at,omitempty"`
	FulfilledAt        *time.Time  `json:"fulfilled_at,omitempty"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
}

type OrderFilter struct {
	Status     string
	Search     string
	TargetKind TargetKind
	TargetID   string
	EndUserID  string
	Page       int
	Size       int
}

type OrderSummary struct {
	TodayAmountCents int64 `json:"today_amount_cents"`
	MonthAmountCents int64 `json:"month_amount_cents"`
	TotalAmountCents int64 `json:"total_amount_cents"`
	PaidCount        int64 `json:"paid_count"`
	PendingCount     int64 `json:"pending_count"`
	FailedCount      int64 `json:"failed_count"`
}

type OrderListResult struct {
	Items   []Order      `json:"items"`
	Total   int64        `json:"total"`
	Page    int          `json:"page"`
	Size    int          `json:"size"`
	Summary OrderSummary `json:"summary"`
}

type CreateOrderInput struct {
	BindingID         string `json:"binding_id"`
	PlanID            string `json:"plan_id"`
	PayChannel        string `json:"pay_channel"`
	CustomAmountCents int    `json:"custom_amount_cents,omitempty"`
}

type ManualOrderInput struct {
	TargetKind        TargetKind `json:"target_kind"`
	TargetID          string     `json:"target_id"`
	PlanID            string     `json:"plan_id"`
	AmountCents       *int       `json:"amount_cents,omitempty"`
	CustomAmountCents int        `json:"custom_amount_cents,omitempty"`
	Note              string     `json:"note,omitempty"`
	// PaidAt optionally backdates an offline payment (e.g. a historical transfer before billing launched).
	PaidAt *time.Time `json:"paid_at,omitempty"`
	// DurationDays optionally overrides a subscription plan's duration for this manual order (e.g. 2 months = 61 days).
	DurationDays int `json:"duration_days,omitempty"`
	// ExtendMode controls how a manual order affects expiry:
	//   "" or "extend_current": extend from max(current expiry, now)
	//   "from_paid_at":         set expiry to max(current expiry, paid_at + duration_days)
	//   "record_only":          record revenue ledger entry only without mutating expiry or quota
	ExtendMode string `json:"extend_mode,omitempty"`
}

const (
	ManualExtendCurrent    = "extend_current"
	ManualExtendFromPaidAt = "from_paid_at"
	ManualExtendRecordOnly = "record_only"

	EntitlementModeSet     = "set"
	EntitlementModeAddDays = "add_days"
)

// AdjustEntitlementInput lets an operator freely extend, shorten, set, or clear
// a target account or API key's subscription expiration.
type AdjustEntitlementInput struct {
	TargetKind TargetKind `json:"target_kind"`
	TargetID   string     `json:"target_id"`
	Mode       string     `json:"mode"` // "set" | "add_days"
	DeltaDays  int        `json:"delta_days,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// EntitlementView returns the effective expiration after an operator adjustment.
type EntitlementView struct {
	TargetKind TargetKind `json:"target_kind"`
	TargetID   string     `json:"target_id"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// PortalPlanView is one plan offered to a specific target (with any per-target price override applied).
type PortalPlanView struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Kind                PlanKind `json:"kind"`
	PriceCents          int      `json:"price_cents"`
	DurationDays        int      `json:"duration_days"`
	QuotaAmountUSD      float64  `json:"quota_amount_usd"`
	QuotaRequests       int64    `json:"quota_requests"`
	QuotaTokens         int64    `json:"quota_tokens"`
	RateUSDPerCNY       float64  `json:"rate_usd_per_cny"`
	MinAmountCents      int      `json:"min_amount_cents"`
	MaxAmountCents      int      `json:"max_amount_cents"`
	PermissionProfileID string   `json:"permission_profile_id,omitempty"`
	Badge               string   `json:"badge,omitempty"`
}

type PortalTargetView struct {
	BindingID        string           `json:"binding_id"`
	TargetKind       TargetKind       `json:"target_kind"`
	TargetID         string           `json:"target_id"`
	Label            string           `json:"label"`
	KeyMasked        string           `json:"key_masked,omitempty"`
	ExpiresAt        *time.Time       `json:"expires_at,omitempty"`
	LifetimeLimitUSD float64          `json:"lifetime_limit_usd"`
	LifetimeUsedUSD  float64          `json:"lifetime_used_usd"`
	Note             string           `json:"note,omitempty"`
	Plans            []PortalPlanView `json:"plans"`
}

type PortalOverview struct {
	Enabled  bool               `json:"enabled"`
	Currency string             `json:"currency"`
	Channels []string           `json:"channels"`
	Targets  []PortalTargetView `json:"targets"`
}

// PublicPayInfo is the unauthenticated view rendered for a shareable Key QR pay link.
type PublicPayInfo struct {
	Code     string           `json:"code"`
	Currency string           `json:"currency"`
	Channels []string         `json:"channels"`
	Target   PortalTargetView `json:"target"`
}

// APIKeyBillingOverview is the key-scoped billing view served on /#/apikey-usage
// when a caller proves ownership by presenting the plaintext API key in a POST body.
type APIKeyBillingOverview struct {
	Enabled        bool             `json:"enabled"`
	HasBillingInfo bool             `json:"has_billing_info"`
	Currency       string           `json:"currency"`
	Channels       []string         `json:"channels"`
	Target         PortalTargetView `json:"target"`
	OrdersCount    int64            `json:"orders_count"`
}
