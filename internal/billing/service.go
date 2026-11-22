package billing

import (
	"context"
	"strings"
	"sync"
	"time"
)

// TargetMeta describes an end-user or API key looked up from the runtime database.
type TargetMeta struct {
	TargetKind       TargetKind
	TargetID         string
	EndUserID        string
	Label            string
	Subtitle         string
	KeyMasked        string
	LifetimeLimitUSD float64
	LifetimeUsedUSD  float64
	Active           bool
}

// UsageLookupFunc returns the current lifetime USD spent for an end-user or API key.
type UsageLookupFunc func(tenantID string, kind TargetKind, targetID string) float64

// Service coordinates billing settings, plans, target bindings, XunhuPay checkout,
// webhook/polling confirmation, and quota/expiry fulfillment.
type Service struct {
	store          *Store
	xunhu          *XunhuClient
	dailyReset     DailyResetFunc
	usageLookup    UsageLookupFunc
	onQuotaChanged func() error

	syncMu       sync.Mutex
	lastSyncByNo map[string]time.Time
}

type ServiceOptions struct {
	Store          *Store
	XunhuClient    *XunhuClient
	DailyReset     DailyResetFunc
	UsageLookup    UsageLookupFunc
	OnQuotaChanged func() error
}

func NewService(opts ServiceOptions) *Service {
	client := opts.XunhuClient
	if client == nil {
		client = NewXunhuClient(nil)
	}
	return &Service{
		store:          opts.Store,
		xunhu:          client,
		dailyReset:     opts.DailyReset,
		usageLookup:    opts.UsageLookup,
		onQuotaChanged: opts.OnQuotaChanged,
		lastSyncByNo:   make(map[string]time.Time),
	}
}

func (s *Service) Store() *Store { return s.store }

func (s *Service) GetSettingsView(ctx context.Context, tenantID, fallbackBaseURL string) (SettingsView, error) {
	st, saved, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return SettingsView{}, err
	}
	return st.ToView(saved, s.store.usable(), fallbackBaseURL), nil
}

func (s *Service) UpdateSettings(ctx context.Context, tenantID, updatedBy, fallbackBaseURL string, patch SettingsPatch) (SettingsView, error) {
	if !s.store.usable() {
		return SettingsView{}, ErrUnavailable
	}
	current, _, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return SettingsView{}, err
	}
	if patch.Enabled != nil {
		current.Enabled = *patch.Enabled
	}
	if patch.Provider != nil {
		p := strings.ToLower(strings.TrimSpace(*patch.Provider))
		if p != "" && p != ProviderXunhuPay {
			return SettingsView{}, fieldErr("provider", "only xunhupay is supported")
		}
		current.Provider = ProviderXunhuPay
	}
	if patch.PublicBaseURL != nil {
		raw := strings.TrimSpace(*patch.PublicBaseURL)
		if raw != "" {
			if err := validateHTTPURL("public_base_url", raw); err != nil {
				return SettingsView{}, err
			}
		}
		current.PublicBaseURL = strings.TrimRight(raw, "/")
	}
	if patch.XunhuGatewayURL != nil {
		raw := strings.TrimSpace(*patch.XunhuGatewayURL)
		if raw == "" {
			raw = DefaultXunhuGatewayURL
		}
		if err := validateHTTPURL("xunhu_gateway_url", raw); err != nil {
			return SettingsView{}, err
		}
		current.XunhuGatewayURL = raw
	}
	if patch.XunhuQueryURL != nil {
		raw := strings.TrimSpace(*patch.XunhuQueryURL)
		if raw == "" {
			raw = DefaultXunhuQueryURL
		}
		if err := validateHTTPURL("xunhu_query_url", raw); err != nil {
			return SettingsView{}, err
		}
		current.XunhuQueryURL = raw
	}
	if patch.XunhuWeChatEnabled != nil {
		current.XunhuWeChatEnabled = *patch.XunhuWeChatEnabled
	}
	if patch.XunhuWeChatAppID != nil {
		current.XunhuWeChatAppID = strings.TrimSpace(*patch.XunhuWeChatAppID)
	}
	if patch.XunhuWeChatSecret != nil {
		v := strings.TrimSpace(*patch.XunhuWeChatSecret)
		if !isMaskedSecret(v) {
			current.XunhuWeChatSecret = v
		}
	}
	if patch.XunhuAlipayEnabled != nil {
		current.XunhuAlipayEnabled = *patch.XunhuAlipayEnabled
	}
	if patch.XunhuAlipayAppID != nil {
		current.XunhuAlipayAppID = strings.TrimSpace(*patch.XunhuAlipayAppID)
	}
	if patch.XunhuAlipaySecret != nil {
		v := strings.TrimSpace(*patch.XunhuAlipaySecret)
		if !isMaskedSecret(v) {
			current.XunhuAlipaySecret = v
		}
	}
	if patch.OrderExpireMinutes != nil {
		if *patch.OrderExpireMinutes < 1 || *patch.OrderExpireMinutes > 1440 {
			return SettingsView{}, fieldErr("order_expire_minutes", "must be between 1 and 1440")
		}
		current.OrderExpireMinutes = *patch.OrderExpireMinutes
	}
	if current.XunhuWeChatEnabled && (current.XunhuWeChatAppID == "" || current.XunhuWeChatSecret == "") {
		return SettingsView{}, fieldErr("xunhu_wechat_appid", "WeChat Pay requires both AppID and AppSecret when enabled")
	}
	if current.XunhuAlipayEnabled && (current.XunhuAlipayAppID == "" || current.XunhuAlipaySecret == "") {
		return SettingsView{}, fieldErr("xunhu_alipay_appid", "Alipay requires both AppID and AppSecret when enabled")
	}
	current.UpdatedBy = strings.TrimSpace(updatedBy)
	saved, err := s.store.UpsertSettings(ctx, current)
	if err != nil {
		return SettingsView{}, err
	}
	return saved.ToView(true, true, fallbackBaseURL), nil
}

func (s *Service) TestChannel(ctx context.Context, tenantID string, req TestChannelRequest) (TestChannelResult, error) {
	ch := strings.ToLower(strings.TrimSpace(req.Channel))
	if ch != ChannelWeChat && ch != ChannelAlipay {
		return TestChannelResult{}, fieldErr("channel", "channel must be wechat or alipay")
	}
	saved, _, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return TestChannelResult{}, err
	}
	queryURL := strings.TrimSpace(req.QueryURL)
	if queryURL == "" {
		queryURL = saved.XunhuQueryURL
	}
	appID := strings.TrimSpace(req.AppID)
	appSecret := strings.TrimSpace(req.AppSecret)
	if ch == ChannelWeChat {
		if appID == "" {
			appID = saved.XunhuWeChatAppID
		}
		if appSecret == "" || isMaskedSecret(appSecret) {
			appSecret = saved.XunhuWeChatSecret
		}
	} else {
		if appID == "" {
			appID = saved.XunhuAlipayAppID
		}
		if appSecret == "" || isMaskedSecret(appSecret) {
			appSecret = saved.XunhuAlipaySecret
		}
	}
	if appID == "" || appSecret == "" {
		return TestChannelResult{Channel: ch, OK: false, Message: "AppID 和 AppSecret 不能为空"}, nil
	}
	started := time.Now()
	probeErr := s.xunhu.TestChannel(ctx, queryURL, appID, appSecret)
	elapsed := time.Since(started).Milliseconds()
	if probeErr != nil {
		return TestChannelResult{Channel: ch, OK: false, Message: probeErr.Error(), DurationMs: elapsed}, nil
	}
	return TestChannelResult{Channel: ch, OK: true, Message: "虎皮椒网关连接及签名校验通过", DurationMs: elapsed}, nil
}

func (s *Service) ListPlans(ctx context.Context, tenantID string) ([]Plan, error) {
	return s.store.ListPlans(ctx, tenantID, false)
}

func (s *Service) CreatePlan(ctx context.Context, tenantID string, input PlanInput) (Plan, error) {
	return s.store.CreatePlan(ctx, tenantID, input)
}

func (s *Service) UpdatePlan(ctx context.Context, tenantID, planID string, input PlanInput) (Plan, error) {
	return s.store.UpdatePlan(ctx, tenantID, planID, input)
}

func (s *Service) DeletePlan(ctx context.Context, tenantID, planID string) error {
	return s.store.DeletePlan(ctx, tenantID, planID)
}

func (s *Service) ListBindingViews(ctx context.Context, tenantID, fallbackBaseURL string) ([]BindingView, error) {
	bindings, err := s.store.ListBindings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	st, _, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSpace(st.PublicBaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(fallbackBaseURL)
	}
	acctExp, keyExp, _ := s.store.ListEntitlementsForTenant(ctx, tenantID)
	views := make([]BindingView, 0, len(bindings))
	for _, b := range bindings {
		meta := s.lookupTargetMeta(ctx, tenantID, b.TargetKind, b.TargetID)
		v := BindingView{
			Binding:          b,
			TargetLabel:      meta.Label,
			TargetSubtitle:   meta.Subtitle,
			KeyMasked:        meta.KeyMasked,
			LifetimeLimitUSD: meta.LifetimeLimitUSD,
		}
		if b.AllowPublicPay && b.PublicCode != "" {
			v.PublicPayURL = BuildPublicPayURL(baseURL, b.PublicCode)
		}
		if b.TargetKind == TargetKindEndUser {
			if ts, ok := acctExp[b.TargetID]; ok {
				exp := ts
				v.ExpiresAt = &exp
			}
		} else if ts, ok := keyExp[b.TargetID]; ok {
			exp := ts
			v.ExpiresAt = &exp
		}
		views = append(views, v)
	}
	return views, nil
}

func (s *Service) UpsertBinding(ctx context.Context, tenantID, fallbackBaseURL string, input BindingInput) (BindingView, error) {
	if input.TargetKind != TargetKindEndUser && input.TargetKind != TargetKindAPIKey {
		return BindingView{}, fieldErr("target_kind", "target_kind must be end_user or api_key")
	}
	input.TargetID = strings.TrimSpace(input.TargetID)
	if input.TargetID == "" {
		return BindingView{}, fieldErr("target_id", "target_id is required")
	}
	meta := s.lookupTargetMeta(ctx, tenantID, input.TargetKind, input.TargetID)
	if !meta.Active {
		return BindingView{}, ErrNotFound
	}
	// Verify every referenced plan belongs to this tenant.
	plans, err := s.store.ListPlans(ctx, tenantID, false)
	if err != nil {
		return BindingView{}, err
	}
	validPlans := make(map[string]struct{}, len(plans))
	for _, p := range plans {
		validPlans[p.ID] = struct{}{}
	}
	cleanPlanIDs := make([]string, 0, len(input.PlanIDs))
	for _, pid := range dedupeTrimmed(input.PlanIDs) {
		if _, ok := validPlans[pid]; !ok {
			return BindingView{}, fieldErr("plan_ids", "unknown plan id "+pid)
		}
		cleanPlanIDs = append(cleanPlanIDs, pid)
	}
	input.PlanIDs = cleanPlanIDs

	saved, err := s.store.UpsertBinding(ctx, tenantID, input.TargetKind, input.TargetID, meta.EndUserID, input)
	if err != nil {
		return BindingView{}, err
	}
	if input.SetExpiresAt {
		if err := s.store.SetEntitlementExpiry(ctx, tenantID, input.TargetKind, input.TargetID, input.ExpiresAt); err != nil {
			return BindingView{}, err
		}
		if s.onQuotaChanged != nil {
			_ = s.onQuotaChanged()
		}
	}
	views, err := s.ListBindingViews(ctx, tenantID, fallbackBaseURL)
	if err != nil {
		return BindingView{}, err
	}
	for _, v := range views {
		if v.ID == saved.ID {
			return v, nil
		}
	}
	return BindingView{Binding: saved}, nil
}

func (s *Service) RotateBindingPublicCode(ctx context.Context, tenantID, bindingID, fallbackBaseURL string) (BindingView, error) {
	saved, err := s.store.RotateBindingPublicCode(ctx, tenantID, bindingID)
	if err != nil {
		return BindingView{}, err
	}
	views, err := s.ListBindingViews(ctx, tenantID, fallbackBaseURL)
	if err != nil {
		return BindingView{}, err
	}
	for _, v := range views {
		if v.ID == saved.ID {
			return v, nil
		}
	}
	return BindingView{Binding: saved}, nil
}

func (s *Service) DeleteBinding(ctx context.Context, tenantID, bindingID string) error {
	return s.store.DeleteBinding(ctx, tenantID, bindingID)
}

// AdjustEntitlement lets an operator directly set, clear, extend (+N days), or shorten (-N days)
// an end-user or API key's subscription expiration, propagating the change across cluster nodes.
func (s *Service) AdjustEntitlement(ctx context.Context, tenantID string, input AdjustEntitlementInput) (EntitlementView, error) {
	if input.TargetKind != TargetKindEndUser && input.TargetKind != TargetKindAPIKey {
		return EntitlementView{}, fieldErr("target_kind", "target_kind must be end_user or api_key")
	}
	input.TargetID = strings.TrimSpace(input.TargetID)
	if input.TargetID == "" {
		return EntitlementView{}, fieldErr("target_id", "target_id is required")
	}
	meta := s.lookupTargetMeta(ctx, tenantID, input.TargetKind, input.TargetID)
	if !meta.Active {
		return EntitlementView{}, ErrNotFound
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode == "" {
		if input.DeltaDays != 0 {
			mode = EntitlementModeAddDays
		} else {
			mode = EntitlementModeSet
		}
	}
	var resultExp *time.Time
	switch mode {
	case EntitlementModeSet:
		if err := s.store.SetEntitlementExpiry(ctx, tenantID, input.TargetKind, input.TargetID, input.ExpiresAt); err != nil {
			return EntitlementView{}, err
		}
		if input.ExpiresAt != nil {
			t := input.ExpiresAt.UTC()
			resultExp = &t
		}
	case EntitlementModeAddDays:
		if input.DeltaDays == 0 || input.DeltaDays < -3650 || input.DeltaDays > 3650 {
			return EntitlementView{}, fieldErr("delta_days", "delta_days must be non-zero and within [-3650, 3650]")
		}
		next, err := s.store.AdjustEntitlementDays(ctx, tenantID, input.TargetKind, input.TargetID, input.DeltaDays, time.Now().UTC())
		if err != nil {
			return EntitlementView{}, err
		}
		resultExp = &next
	default:
		return EntitlementView{}, fieldErr("mode", "mode must be set or add_days")
	}
	if s.onQuotaChanged != nil {
		_ = s.onQuotaChanged()
	}
	return EntitlementView{
		TargetKind: input.TargetKind,
		TargetID:   input.TargetID,
		ExpiresAt:  resultExp,
	}, nil
}

// HasPortalBilling reports whether the signed-in portal user has at least one active
// binding with at least one enabled plan while tenant billing is enabled.
func (s *Service) HasPortalBilling(ctx context.Context, tenantID, endUserID string) bool {
	overview, err := s.GetPortalOverview(ctx, tenantID, endUserID)
	if err != nil {
		return false
	}
	return overview.Enabled && len(overview.Targets) > 0
}

func (s *Service) GetPortalOverview(ctx context.Context, tenantID, endUserID string) (PortalOverview, error) {
	st, _, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return PortalOverview{}, err
	}
	channels := st.EnabledChannels()
	if !st.Enabled || len(channels) == 0 {
		return PortalOverview{Enabled: false, Currency: st.Currency, Channels: []string{}, Targets: []PortalTargetView{}}, nil
	}
	bindings, err := s.store.ListBindingsForEndUser(ctx, tenantID, endUserID)
	if err != nil {
		return PortalOverview{}, err
	}
	plans, err := s.store.ListPlans(ctx, tenantID, true)
	if err != nil {
		return PortalOverview{}, err
	}
	planByID := make(map[string]Plan, len(plans))
	for _, p := range plans {
		planByID[p.ID] = p
	}
	acctExp, keyExp, _ := s.store.ListEntitlementsForTenant(ctx, tenantID)
	targets := make([]PortalTargetView, 0, len(bindings))
	for _, b := range bindings {
		meta := s.lookupTargetMeta(ctx, tenantID, b.TargetKind, b.TargetID)
		if !meta.Active {
			continue
		}
		targetPlans := resolveBindingPlans(b, planByID)
		if len(targetPlans) == 0 {
			continue
		}
		var exp *time.Time
		if b.TargetKind == TargetKindEndUser {
			if ts, ok := acctExp[b.TargetID]; ok {
				t := ts
				exp = &t
			}
		} else if ts, ok := keyExp[b.TargetID]; ok {
			t := ts
			exp = &t
		}
		targets = append(targets, PortalTargetView{
			BindingID:        b.ID,
			TargetKind:       b.TargetKind,
			TargetID:         b.TargetID,
			Label:            meta.Label,
			KeyMasked:        meta.KeyMasked,
			ExpiresAt:        exp,
			LifetimeLimitUSD: meta.LifetimeLimitUSD,
			LifetimeUsedUSD:  meta.LifetimeUsedUSD,
			Note:             b.Note,
			Plans:            targetPlans,
		})
	}
	return PortalOverview{
		Enabled:  len(targets) > 0,
		Currency: st.Currency,
		Channels: channels,
		Targets:  targets,
	}, nil
}

func (s *Service) GetPublicPayInfo(ctx context.Context, publicCode string) (PublicPayInfo, error) {
	b, err := s.store.GetBindingByPublicCode(ctx, publicCode)
	if err != nil {
		return PublicPayInfo{}, err
	}
	if !b.Enabled || !b.AllowPublicPay {
		return PublicPayInfo{}, ErrNotFound
	}
	st, _, err := s.store.GetSettings(ctx, b.TenantID)
	if err != nil {
		return PublicPayInfo{}, err
	}
	channels := st.EnabledChannels()
	if !st.Enabled || len(channels) == 0 {
		return PublicPayInfo{}, ErrDisabled
	}
	plans, err := s.store.ListPlans(ctx, b.TenantID, true)
	if err != nil {
		return PublicPayInfo{}, err
	}
	planByID := make(map[string]Plan, len(plans))
	for _, p := range plans {
		planByID[p.ID] = p
	}
	targetPlans := resolveBindingPlans(b, planByID)
	if len(targetPlans) == 0 {
		return PublicPayInfo{}, ErrDisabled
	}
	meta := s.lookupTargetMeta(ctx, b.TenantID, b.TargetKind, b.TargetID)
	if !meta.Active {
		return PublicPayInfo{}, ErrNotFound
	}
	exp, _ := s.store.GetEntitlement(ctx, b.TenantID, b.TargetKind, b.TargetID)
	return PublicPayInfo{
		Code:     b.PublicCode,
		Currency: st.Currency,
		Channels: channels,
		Target: PortalTargetView{
			BindingID:        b.ID,
			TargetKind:       b.TargetKind,
			TargetID:         b.TargetID,
			Label:            meta.Label,
			KeyMasked:        meta.KeyMasked,
			ExpiresAt:        exp,
			LifetimeLimitUSD: meta.LifetimeLimitUSD,
			LifetimeUsedUSD:  meta.LifetimeUsedUSD,
			Note:             b.Note,
			Plans:            targetPlans,
		},
	}, nil
}

func resolveBindingPlans(b Binding, planByID map[string]Plan) []PortalPlanView {
	out := make([]PortalPlanView, 0, len(b.PlanIDs))
	for _, pid := range b.PlanIDs {
		p, ok := planByID[pid]
		if !ok || !p.Enabled {
			continue
		}
		price := p.PriceCents
		if override, has := b.PriceOverrides[p.ID]; has && override > 0 && p.Kind != PlanKindCustomQuota {
			price = override
		}
		out = append(out, PortalPlanView{
			ID:                  p.ID,
			Name:                p.Name,
			Description:         p.Description,
			Kind:                p.Kind,
			PriceCents:          price,
			DurationDays:        p.DurationDays,
			QuotaAmountUSD:      p.QuotaAmountUSD,
			QuotaRequests:       p.QuotaRequests,
			QuotaTokens:         p.QuotaTokens,
			RateUSDPerCNY:       p.RateUSDPerCNY,
			MinAmountCents:      p.MinAmountCents,
			MaxAmountCents:      p.MaxAmountCents,
			PermissionProfileID: p.PermissionProfileID,
			Badge:               p.Badge,
		})
	}
	return out
}
