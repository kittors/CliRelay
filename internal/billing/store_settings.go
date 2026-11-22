package billing

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
)

// Store backs billing settings, plans, bindings, entitlements and orders in
// the runtime SQL database (PostgreSQL in production, SQLite in unit tests).
type Store struct {
	db *sql.DB
}

var (
	defaultStoreMu sync.RWMutex
	defaultStore   *Store
)

// NewStore wraps db; nil db returns ErrUnavailable on writes and empty reads where appropriate.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetDefaultStore registers the process-wide billing store (used by configaccess
// to read active subscription expirations when rebuilding the key snapshot).
func SetDefaultStore(s *Store) {
	defaultStoreMu.Lock()
	defaultStore = s
	defaultStoreMu.Unlock()
}

// DefaultStore returns the process-wide billing store, or nil if not attached.
func DefaultStore() *Store {
	defaultStoreMu.RLock()
	defer defaultStoreMu.RUnlock()
	return defaultStore
}

// ListAllExpiries returns all active non-nil subscription expiration timestamps
// from the process-wide billing store. Returns empty maps when no store is attached.
func ListAllExpiries() (map[string]time.Time, map[string]time.Time) {
	s := DefaultStore()
	if s == nil || s.db == nil {
		return nil, nil
	}
	accounts, keys, err := s.ListAllExpiries(context.Background())
	if err != nil {
		return nil, nil
	}
	return accounts, keys
}

func (s *Store) usable() bool {
	return s != nil && s.db != nil
}

// DB exposes the underlying database handle for transactional fulfillment.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func defaultSettings(tenantID string) Settings {
	return Settings{
		TenantID:           tenantID,
		Enabled:            false,
		Provider:           ProviderXunhuPay,
		XunhuGatewayURL:    DefaultXunhuGatewayURL,
		XunhuQueryURL:      DefaultXunhuQueryURL,
		OrderExpireMinutes: DefaultOrderExpireMins,
		Currency:           DefaultCurrency,
	}
}

// MaskSecret obscures a stored secret while leaving the last 4 characters visible
// for operator recognition. Echoing a value containing "****" preserves the stored secret.
func MaskSecret(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

func isMaskedSecret(val string) bool {
	return strings.Contains(val, "****")
}

// GetSettings loads the tenant's billing settings, returning defaults if never saved.
func (s *Store) GetSettings(ctx context.Context, tenantID string) (Settings, bool, error) {
	if !s.usable() {
		return defaultSettings(tenantID), false, nil
	}
	var st Settings
	var rawUpdated any
	err := s.db.QueryRowContext(ctx, `
		SELECT tenant_id, enabled, provider, public_base_url,
		       xunhu_gateway_url, xunhu_query_url,
		       xunhu_wechat_enabled, xunhu_wechat_appid, xunhu_wechat_appsecret,
		       xunhu_alipay_enabled, xunhu_alipay_appid, xunhu_alipay_appsecret,
		       order_expire_minutes, currency, updated_at, updated_by
		  FROM billing_settings
		 WHERE tenant_id = ?
	`, tenantID).Scan(
		&st.TenantID, &st.Enabled, &st.Provider, &st.PublicBaseURL,
		&st.XunhuGatewayURL, &st.XunhuQueryURL,
		&st.XunhuWeChatEnabled, &st.XunhuWeChatAppID, &st.XunhuWeChatSecret,
		&st.XunhuAlipayEnabled, &st.XunhuAlipayAppID, &st.XunhuAlipaySecret,
		&st.OrderExpireMinutes, &st.Currency, &rawUpdated, &st.UpdatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultSettings(tenantID), false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	if ts, ok := parseAnyTime(rawUpdated); ok {
		st.UpdatedAt = ts
	}
	if strings.TrimSpace(st.Provider) == "" {
		st.Provider = ProviderXunhuPay
	}
	if strings.TrimSpace(st.XunhuGatewayURL) == "" {
		st.XunhuGatewayURL = DefaultXunhuGatewayURL
	}
	if strings.TrimSpace(st.XunhuQueryURL) == "" {
		st.XunhuQueryURL = DefaultXunhuQueryURL
	}
	if st.OrderExpireMinutes <= 0 {
		st.OrderExpireMinutes = DefaultOrderExpireMins
	}
	if strings.TrimSpace(st.Currency) == "" {
		st.Currency = DefaultCurrency
	}
	return st, true, nil
}

// UpsertSettings persists the tenant's billing settings.
func (s *Store) UpsertSettings(ctx context.Context, st Settings) (Settings, error) {
	if !s.usable() {
		return Settings{}, ErrUnavailable
	}
	now := time.Now().UTC()
	st.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_settings (
			tenant_id, enabled, provider, public_base_url,
			xunhu_gateway_url, xunhu_query_url,
			xunhu_wechat_enabled, xunhu_wechat_appid, xunhu_wechat_appsecret,
			xunhu_alipay_enabled, xunhu_alipay_appid, xunhu_alipay_appsecret,
			order_expire_minutes, currency, updated_at, updated_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			provider = EXCLUDED.provider,
			public_base_url = EXCLUDED.public_base_url,
			xunhu_gateway_url = EXCLUDED.xunhu_gateway_url,
			xunhu_query_url = EXCLUDED.xunhu_query_url,
			xunhu_wechat_enabled = EXCLUDED.xunhu_wechat_enabled,
			xunhu_wechat_appid = EXCLUDED.xunhu_wechat_appid,
			xunhu_wechat_appsecret = EXCLUDED.xunhu_wechat_appsecret,
			xunhu_alipay_enabled = EXCLUDED.xunhu_alipay_enabled,
			xunhu_alipay_appid = EXCLUDED.xunhu_alipay_appid,
			xunhu_alipay_appsecret = EXCLUDED.xunhu_alipay_appsecret,
			order_expire_minutes = EXCLUDED.order_expire_minutes,
			currency = EXCLUDED.currency,
			updated_at = EXCLUDED.updated_at,
			updated_by = EXCLUDED.updated_by
	`,
		st.TenantID, st.Enabled, st.Provider, st.PublicBaseURL,
		st.XunhuGatewayURL, st.XunhuQueryURL,
		st.XunhuWeChatEnabled, st.XunhuWeChatAppID, st.XunhuWeChatSecret,
		st.XunhuAlipayEnabled, st.XunhuAlipayAppID, st.XunhuAlipaySecret,
		st.OrderExpireMinutes, st.Currency, now, st.UpdatedBy,
	); err != nil {
		return Settings{}, err
	}
	return st, nil
}

// EnabledChannels returns the list of configured and enabled payment channels.
func (st Settings) EnabledChannels() []string {
	if !st.Enabled {
		return nil
	}
	channels := make([]string, 0, 2)
	if st.XunhuWeChatEnabled && strings.TrimSpace(st.XunhuWeChatAppID) != "" && strings.TrimSpace(st.XunhuWeChatSecret) != "" {
		channels = append(channels, ChannelWeChat)
	}
	if st.XunhuAlipayEnabled && strings.TrimSpace(st.XunhuAlipayAppID) != "" && strings.TrimSpace(st.XunhuAlipaySecret) != "" {
		channels = append(channels, ChannelAlipay)
	}
	return channels
}

// ChannelCredentials returns appid/appsecret for the named channel if enabled.
func (st Settings) ChannelCredentials(channel string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case ChannelWeChat:
		if st.XunhuWeChatEnabled && strings.TrimSpace(st.XunhuWeChatAppID) != "" && strings.TrimSpace(st.XunhuWeChatSecret) != "" {
			return strings.TrimSpace(st.XunhuWeChatAppID), strings.TrimSpace(st.XunhuWeChatSecret), true
		}
	case ChannelAlipay:
		if st.XunhuAlipayEnabled && strings.TrimSpace(st.XunhuAlipayAppID) != "" && strings.TrimSpace(st.XunhuAlipaySecret) != "" {
			return strings.TrimSpace(st.XunhuAlipayAppID), strings.TrimSpace(st.XunhuAlipaySecret), true
		}
	}
	return "", "", false
}

// BuildNotifyURL returns the XunhuPay async webhook callback URL for a tenant + channel.
func BuildNotifyURL(baseURL, tenantID, channel string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	path := "/v0/management/public/billing/callback/xunhupay/" + strings.TrimSpace(tenantID) + "/" + strings.TrimSpace(channel)
	if base == "" {
		return path
	}
	return base + path
}

// BuildPublicPayURL returns the shareable standalone payment page URL for a binding code.
func BuildPublicPayURL(baseURL, publicCode string) string {
	code := strings.TrimSpace(publicCode)
	if code == "" {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "/#/pay/" + code
	}
	return base + "/#/pay/" + code
}

// ToView converts stored settings to the masked operator view.
func (st Settings) ToView(saved bool, persistent bool, fallbackBaseURL string) SettingsView {
	base := strings.TrimSpace(st.PublicBaseURL)
	if base == "" {
		base = strings.TrimSpace(fallbackBaseURL)
	}
	view := SettingsView{
		Enabled:            st.Enabled,
		Provider:           st.Provider,
		PublicBaseURL:      st.PublicBaseURL,
		XunhuGatewayURL:    st.XunhuGatewayURL,
		XunhuQueryURL:      st.XunhuQueryURL,
		XunhuWeChatEnabled: st.XunhuWeChatEnabled,
		XunhuWeChatAppID:   st.XunhuWeChatAppID,
		XunhuWeChatSecret:  MaskSecret(st.XunhuWeChatSecret),
		XunhuWeChatHasKey:  strings.TrimSpace(st.XunhuWeChatSecret) != "",
		XunhuAlipayEnabled: st.XunhuAlipayEnabled,
		XunhuAlipayAppID:   st.XunhuAlipayAppID,
		XunhuAlipaySecret:  MaskSecret(st.XunhuAlipaySecret),
		XunhuAlipayHasKey:  strings.TrimSpace(st.XunhuAlipaySecret) != "",
		OrderExpireMinutes: st.OrderExpireMinutes,
		Currency:           st.Currency,
		WeChatNotifyURL:    BuildNotifyURL(base, st.TenantID, ChannelWeChat),
		AlipayNotifyURL:    BuildNotifyURL(base, st.TenantID, ChannelAlipay),
		Persistent:         persistent,
		UpdatedBy:          st.UpdatedBy,
	}
	if saved && !st.UpdatedAt.IsZero() {
		ts := st.UpdatedAt
		view.UpdatedAt = &ts
	}
	return view
}
