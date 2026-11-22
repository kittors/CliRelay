package billing

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// CreatePortalOrder creates a live XunhuPay order for an authenticated portal user.
func (s *Service) CreatePortalOrder(ctx context.Context, tenantID, endUserID, actorName, fallbackBaseURL string, input CreateOrderInput) (Order, error) {
	b, err := s.store.GetBinding(ctx, tenantID, input.BindingID)
	if err != nil {
		return Order{}, err
	}
	if !b.Enabled || b.EndUserID != endUserID {
		return Order{}, ErrNotFound
	}
	return s.createCheckoutOrder(ctx, b, "portal", actorName, fallbackBaseURL, input)
}

// CreatePublicOrder creates a live XunhuPay order from a shareable public pay code.
func (s *Service) CreatePublicOrder(ctx context.Context, publicCode, fallbackBaseURL string, input CreateOrderInput) (Order, error) {
	b, err := s.store.GetBindingByPublicCode(ctx, publicCode)
	if err != nil {
		return Order{}, err
	}
	if !b.Enabled || !b.AllowPublicPay {
		return Order{}, ErrNotFound
	}
	return s.createCheckoutOrder(ctx, b, "public_link", "public:"+b.PublicCode, fallbackBaseURL, input)
}

// CreateAPIKeyOrder creates a live XunhuPay order directly for a caller presenting a specific API key.
func (s *Service) CreateAPIKeyOrder(ctx context.Context, tenantID, keyID, fallbackBaseURL string, input CreateOrderInput) (Order, error) {
	b, err := s.store.GetBindingByTarget(ctx, tenantID, TargetKindAPIKey, keyID)
	if err != nil {
		return Order{}, err
	}
	if !b.Enabled {
		return Order{}, ErrNotFound
	}
	meta := s.lookupTargetMeta(ctx, tenantID, TargetKindAPIKey, keyID)
	if !meta.Active {
		return Order{}, ErrNotFound
	}
	return s.createCheckoutOrder(ctx, b, "apikey_usage", "key:"+meta.KeyMasked, fallbackBaseURL, input)
}

func (s *Service) createCheckoutOrder(ctx context.Context, b Binding, source, createdBy, fallbackBaseURL string, input CreateOrderInput) (Order, error) {
	st, _, err := s.store.GetSettings(ctx, b.TenantID)
	if err != nil {
		return Order{}, err
	}
	if !st.Enabled {
		return Order{}, ErrDisabled
	}
	channel := strings.ToLower(strings.TrimSpace(input.PayChannel))
	appID, appSecret, ok := st.ChannelCredentials(channel)
	if !ok {
		return Order{}, ErrChannelDisabled
	}
	allowedPlan := false
	for _, pid := range b.PlanIDs {
		if pid == input.PlanID {
			allowedPlan = true
			break
		}
	}
	if !allowedPlan {
		return Order{}, fieldErr("plan_id", "plan is not enabled for this key or account")
	}
	plan, err := s.store.GetPlan(ctx, b.TenantID, input.PlanID)
	if err != nil {
		return Order{}, err
	}
	if !plan.Enabled {
		return Order{}, fieldErr("plan_id", "plan is disabled")
	}

	amountCents := plan.PriceCents
	if plan.Kind == PlanKindCustomQuota {
		amountCents = input.CustomAmountCents
		if amountCents < plan.MinAmountCents || amountCents > plan.MaxAmountCents {
			return Order{}, fieldErr("custom_amount_cents",
				fmt.Sprintf("amount must be between ¥%s and ¥%s", FormatCNYYuan(plan.MinAmountCents), FormatCNYYuan(plan.MaxAmountCents)))
		}
	} else if override, has := b.PriceOverrides[plan.ID]; has && override > 0 {
		amountCents = override
	}
	if amountCents <= 0 {
		return Order{}, fieldErr("amount_cents", "order amount must be positive")
	}

	meta := s.lookupTargetMeta(ctx, b.TenantID, b.TargetKind, b.TargetID)
	if !meta.Active {
		return Order{}, ErrNotFound
	}

	now := time.Now().UTC()
	orderNo, err := GenerateOrderNo(now)
	if err != nil {
		return Order{}, err
	}
	baseURL := strings.TrimSpace(st.PublicBaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(fallbackBaseURL)
	}
	notifyURL := BuildNotifyURL(baseURL, b.TenantID, channel)
	returnURL := strings.TrimRight(baseURL, "/") + "/#/apikey-lookup"
	switch {
	case source == "public_link" && b.PublicCode != "":
		returnURL = BuildPublicPayURL(baseURL, b.PublicCode)
	case source == "apikey_usage":
		returnURL = strings.TrimRight(baseURL, "/") + "/#/apikey-usage"
	}

	targetLabel := meta.Label
	if meta.KeyMasked != "" {
		targetLabel = fmt.Sprintf("%s (%s)", meta.Label, meta.KeyMasked)
	}

	order := Order{
		ID:           uuid.NewString(),
		OrderNo:      orderNo,
		TenantID:     b.TenantID,
		BindingID:    b.ID,
		TargetKind:   b.TargetKind,
		TargetID:     b.TargetID,
		TargetLabel:  targetLabel,
		EndUserID:    b.EndUserID,
		PlanID:       plan.ID,
		PlanName:     plan.Name,
		PlanKind:     plan.Kind,
		PlanSnapshot: plan,
		Provider:     ProviderXunhuPay,
		PayChannel:   channel,
		AmountCents:  amountCents,
		Currency:     st.Currency,
		Status:       OrderStatusPending,
		Source:       source,
		CreatedBy:    createdBy,
		ExpiresAt:    now.Add(time.Duration(st.OrderExpireMinutes) * time.Minute),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	resp, err := s.xunhu.CreatePayment(ctx, XunhuCreateRequest{
		GatewayURL:   st.XunhuGatewayURL,
		AppID:        appID,
		AppSecret:    appSecret,
		Channel:      channel,
		TradeOrderID: order.OrderNo,
		AmountCents:  order.AmountCents,
		Title:        fmt.Sprintf("%s - %s", plan.Name, meta.Label),
		NotifyURL:    notifyURL,
		ReturnURL:    returnURL,
		WapURL:       baseURL,
		WapName:      "CliRelay",
	})
	if err != nil {
		return Order{}, err
	}
	order.ProviderOrderID = resp.OpenOrderID
	order.QRCodeURL = resp.QRCodeURL
	order.PayURL = resp.PayURL

	return s.store.CreateOrder(ctx, order)
}

// HandleXunhuCallback verifies a XunhuPay async webhook and fulfills the order idempotently.
func (s *Service) HandleXunhuCallback(ctx context.Context, tenantID, channel string, params map[string]string) error {
	st, _, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return err
	}
	appID, appSecret, ok := st.ChannelCredentials(channel)
	if !ok {
		return ErrChannelDisabled
	}
	if gotAppID := strings.TrimSpace(params["appid"]); gotAppID != "" && gotAppID != appID {
		return ErrInvalidSignature
	}
	if !VerifyXunhuSignature(params, appSecret) {
		return ErrInvalidSignature
	}
	if strings.ToUpper(strings.TrimSpace(params["status"])) != "OD" {
		// Non-paid status notification; acknowledge without fulfilling.
		return nil
	}
	orderNo := strings.TrimSpace(params["trade_order_id"])
	if orderNo == "" {
		return ErrNotFound
	}
	existing, err := s.store.GetOrderByNo(ctx, tenantID, orderNo)
	if err != nil {
		return err
	}
	if existing.PayChannel != strings.ToLower(strings.TrimSpace(channel)) {
		return ErrInvalidSignature
	}
	if feeRaw := strings.TrimSpace(params["total_fee"]); feeRaw != "" {
		paidCents, parseErr := ParseCNYCents(feeRaw)
		if parseErr != nil || paidCents != existing.AmountCents {
			return ErrAmountMismatch
		}
	}
	_, err = s.confirmAndFulfill(ctx, tenantID, orderNo,
		strings.TrimSpace(params["open_order_id"]),
		strings.TrimSpace(params["transaction_id"]),
		time.Now().UTC(),
		false,
	)
	return err
}

// SyncOrderStatus actively queries XunhuPay for a pending/expired order and fulfills if paid.
func (s *Service) SyncOrderStatus(ctx context.Context, tenantID, orderNo string, respectCooldown bool) (Order, error) {
	o, err := s.store.GetOrderByNo(ctx, tenantID, orderNo)
	if err != nil {
		return Order{}, err
	}
	if o.Status == OrderStatusFulfilled || o.Status == OrderStatusClosed || o.Provider != ProviderXunhuPay {
		return o, nil
	}
	if o.Status == OrderStatusFailed {
		return s.confirmAndFulfill(ctx, tenantID, o.OrderNo, o.ProviderOrderID, o.ProviderTradeNo, time.Now().UTC(), true)
	}
	if o.Status == OrderStatusPaid {
		return o, nil
	}
	if respectCooldown {
		s.syncMu.Lock()
		last := s.lastSyncByNo[o.OrderNo]
		if time.Since(last) < 3*time.Second {
			s.syncMu.Unlock()
			return o, nil
		}
		s.lastSyncByNo[o.OrderNo] = time.Now()
		s.syncMu.Unlock()
	}
	st, _, err := s.store.GetSettings(ctx, tenantID)
	if err != nil {
		return o, err
	}
	appID, appSecret, ok := st.ChannelCredentials(o.PayChannel)
	if !ok {
		return o, nil
	}
	qResp, err := s.xunhu.QueryOrder(ctx, XunhuQueryRequest{
		QueryURL:     st.XunhuQueryURL,
		AppID:        appID,
		AppSecret:    appSecret,
		TradeOrderID: o.OrderNo,
	})
	if err != nil {
		return o, nil
	}
	if qResp.Status == "OD" {
		if qResp.AmountCents > 0 && qResp.AmountCents != o.AmountCents {
			return o, ErrAmountMismatch
		}
		return s.confirmAndFulfill(ctx, tenantID, o.OrderNo, qResp.OpenOrderID, qResp.TransactionID, time.Now().UTC(), false)
	}
	return s.store.GetOrderByNo(ctx, tenantID, o.OrderNo)
}

// ManualFulfill lets an operator manually confirm & fulfill an order (e.g. callback lost or retry failed).
func (s *Service) ManualFulfill(ctx context.Context, tenantID, orderNo string) (Order, error) {
	return s.confirmAndFulfill(ctx, tenantID, orderNo, "", "", time.Now().UTC(), true)
}

// CreateManualOrder records an offline payment (e.g. direct WeChat transfer or historical backfill)
// and fulfills it according to extend_mode (extend_current | from_paid_at | record_only).
func (s *Service) CreateManualOrder(ctx context.Context, tenantID, actorName string, input ManualOrderInput) (Order, error) {
	if input.TargetKind != TargetKindEndUser && input.TargetKind != TargetKindAPIKey {
		return Order{}, fieldErr("target_kind", "target_kind must be end_user or api_key")
	}
	input.TargetID = strings.TrimSpace(input.TargetID)
	if input.TargetID == "" {
		return Order{}, fieldErr("target_id", "target_id is required")
	}
	plan, err := s.store.GetPlan(ctx, tenantID, input.PlanID)
	if err != nil {
		return Order{}, err
	}
	meta := s.lookupTargetMeta(ctx, tenantID, input.TargetKind, input.TargetID)
	if !meta.Active {
		return Order{}, ErrNotFound
	}
	extendMode := strings.ToLower(strings.TrimSpace(input.ExtendMode))
	if extendMode == "" {
		extendMode = ManualExtendCurrent
	}
	if extendMode != ManualExtendCurrent && extendMode != ManualExtendFromPaidAt && extendMode != ManualExtendRecordOnly {
		return Order{}, fieldErr("extend_mode", "extend_mode must be extend_current, from_paid_at, or record_only")
	}
	if input.DurationDays < 0 || input.DurationDays > 3650 {
		return Order{}, fieldErr("duration_days", "duration_days must be between 0 and 3650")
	}
	if input.DurationDays > 0 && plan.Kind == PlanKindSubscription {
		plan.DurationDays = input.DurationDays
	}

	var amountCents int
	if input.AmountCents != nil {
		if *input.AmountCents < 0 {
			return Order{}, fieldErr("amount_cents", "amount_cents cannot be negative")
		}
		amountCents = *input.AmountCents
	} else if plan.Kind == PlanKindCustomQuota {
		amountCents = input.CustomAmountCents
	} else {
		amountCents = plan.PriceCents
	}
	if plan.Kind == PlanKindCustomQuota && extendMode != ManualExtendRecordOnly && amountCents <= 0 {
		return Order{}, fieldErr("amount_cents", "amount must be positive")
	}

	now := time.Now().UTC()
	paidAt := now
	if input.PaidAt != nil && !input.PaidAt.IsZero() {
		paidAt = input.PaidAt.UTC()
	}
	orderNo, err := GenerateOrderNo(paidAt)
	if err != nil {
		return Order{}, err
	}
	targetLabel := meta.Label
	if meta.KeyMasked != "" {
		targetLabel = fmt.Sprintf("%s (%s)", meta.Label, meta.KeyMasked)
	}
	source := "admin_manual"
	if extendMode == ManualExtendFromPaidAt {
		source = "admin_manual_from_paid_at"
	}
	order := Order{
		ID:           uuid.NewString(),
		OrderNo:      orderNo,
		TenantID:     tenantID,
		TargetKind:   input.TargetKind,
		TargetID:     input.TargetID,
		TargetLabel:  targetLabel,
		EndUserID:    meta.EndUserID,
		PlanID:       plan.ID,
		PlanName:     plan.Name,
		PlanKind:     plan.Kind,
		PlanSnapshot: plan,
		Provider:     ProviderManual,
		PayChannel:   ChannelManual,
		AmountCents:  amountCents,
		Currency:     DefaultCurrency,
		Status:       OrderStatusPending,
		Source:       source,
		CreatedBy:    strings.TrimSpace(actorName),
		ExpiresAt:    paidAt,
		CreatedAt:    paidAt,
		UpdatedAt:    now,
	}
	if _, err := s.store.CreateOrder(ctx, order); err != nil {
		return Order{}, err
	}
	if extendMode == ManualExtendRecordOnly {
		if _, _, err := s.store.ClaimOrderForFulfillment(ctx, tenantID, order.OrderNo, "manual", strings.TrimSpace(input.Note), paidAt, true); err != nil {
			return Order{}, err
		}
		summary := "线下收款补录（仅记账，未变更当前到期时间/额度）"
		if note := strings.TrimSpace(input.Note); note != "" {
			summary = summary + " · " + note
		}
		return s.store.MarkOrderFulfilled(ctx, tenantID, order.OrderNo, summary, paidAt)
	}
	return s.confirmAndFulfill(ctx, tenantID, order.OrderNo, "manual", strings.TrimSpace(input.Note), paidAt, true)
}

func (s *Service) confirmAndFulfill(ctx context.Context, tenantID, orderNo, providerOrderID, providerTradeNo string, paidAt time.Time, forceRetry bool) (Order, error) {
	claimed, alreadyHandled, err := s.store.ClaimOrderForFulfillment(ctx, tenantID, orderNo, providerOrderID, providerTradeNo, paidAt, forceRetry)
	if err != nil {
		return Order{}, err
	}
	if alreadyHandled {
		return claimed, nil
	}
	summary, err := s.fulfillOrder(ctx, claimed)
	if err != nil {
		s.store.MarkOrderFulfillmentFailed(ctx, tenantID, orderNo, err.Error())
		return Order{}, err
	}
	return s.store.MarkOrderFulfilled(ctx, tenantID, orderNo, summary, time.Now().UTC())
}

func (s *Service) ListOrders(ctx context.Context, tenantID string, filter OrderFilter) (OrderListResult, error) {
	return s.store.ListOrders(ctx, tenantID, filter)
}

func (s *Service) GetOrderForEndUser(ctx context.Context, tenantID, endUserID, orderNo string, sync bool) (Order, error) {
	o, err := s.store.GetOrderByNo(ctx, tenantID, orderNo)
	if err != nil {
		return Order{}, err
	}
	if o.EndUserID != endUserID {
		return Order{}, ErrNotFound
	}
	if sync && (o.Status == OrderStatusPending || o.Status == OrderStatusExpired) {
		return s.SyncOrderStatus(ctx, tenantID, orderNo, true)
	}
	return o, nil
}

func (s *Service) GetOrderForPublicCode(ctx context.Context, publicCode, orderNo string, sync bool) (Order, error) {
	b, err := s.store.GetBindingByPublicCode(ctx, publicCode)
	if err != nil {
		return Order{}, err
	}
	o, err := s.store.GetOrderByNo(ctx, b.TenantID, orderNo)
	if err != nil {
		return Order{}, err
	}
	if o.BindingID != b.ID {
		return Order{}, ErrNotFound
	}
	if sync && (o.Status == OrderStatusPending || o.Status == OrderStatusExpired) {
		return s.SyncOrderStatus(ctx, b.TenantID, orderNo, true)
	}
	return o, nil
}

// ListAPIKeyOrders returns orders belonging strictly to a single API key.
func (s *Service) ListAPIKeyOrders(ctx context.Context, tenantID, keyID string, page, size int) (OrderListResult, error) {
	return s.store.ListOrders(ctx, tenantID, OrderFilter{
		TargetKind: TargetKindAPIKey,
		TargetID:   strings.TrimSpace(keyID),
		Page:       page,
		Size:       size,
	})
}

// GetOrderForAPIKey fetches one order strictly owned by the presented API key, optionally syncing XunhuPay.
func (s *Service) GetOrderForAPIKey(ctx context.Context, tenantID, keyID, orderNo string, sync bool) (Order, error) {
	o, err := s.store.GetOrderByNo(ctx, tenantID, orderNo)
	if err != nil {
		return Order{}, err
	}
	if o.TargetKind != TargetKindAPIKey || o.TargetID != strings.TrimSpace(keyID) {
		return Order{}, ErrNotFound
	}
	if sync && (o.Status == OrderStatusPending || o.Status == OrderStatusExpired) {
		return s.SyncOrderStatus(ctx, tenantID, orderNo, true)
	}
	return o, nil
}

func (s *Service) lookupTargetMeta(ctx context.Context, tenantID string, kind TargetKind, targetID string) TargetMeta {
	meta := TargetMeta{TargetKind: kind, TargetID: targetID}
	db := s.store.DB()
	if db == nil {
		return meta
	}
	switch kind {
	case TargetKindEndUser:
		var username, displayName, status string
		var spend float64
		if err := db.QueryRowContext(ctx, `
			SELECT username, display_name, status, COALESCE(spending_limit, 0)
			  FROM end_users
			 WHERE tenant_id = ? AND id = ?
		`, tenantID, targetID).Scan(&username, &displayName, &status, &spend); err == nil {
			meta.EndUserID = targetID
			meta.Label = strings.TrimSpace(displayName)
			if meta.Label == "" {
				meta.Label = strings.TrimSpace(username)
			}
			meta.Subtitle = strings.TrimSpace(username)
			meta.LifetimeLimitUSD = spend
			meta.Active = strings.TrimSpace(status) == "active"
		}
	case TargetKindAPIKey:
		var keySecret, name string
		var disabled int
		var endUserID sql.NullString
		var spend float64
		var policyJSON string
		if err := db.QueryRowContext(ctx, `
			SELECT key, name, disabled, end_user_id, COALESCE(spending_limit, 0), COALESCE(access_policy, '{}')
			  FROM api_keys
			 WHERE tenant_id = ? AND id = ?
		`, tenantID, targetID).Scan(&keySecret, &name, &disabled, &endUserID, &spend, &policyJSON); err == nil {
			meta.Label = strings.TrimSpace(name)
			if meta.Label == "" {
				meta.Label = maskKeyForDisplay(keySecret)
			}
			meta.KeyMasked = maskKeyForDisplay(keySecret)
			if endUserID.Valid {
				meta.EndUserID = strings.TrimSpace(endUserID.String)
			}
			if meta.EndUserID != "" {
				policy, _ := quota.DecodeAccessPolicy(policyJSON)
				meta.LifetimeLimitUSD = policy.Limits.Cost.Lifetime
				if meta.LifetimeLimitUSD == 0 {
					var acctSpend float64
					if err := db.QueryRowContext(ctx, `SELECT COALESCE(spending_limit, 0) FROM end_users WHERE tenant_id = ? AND id = ?`, tenantID, meta.EndUserID).Scan(&acctSpend); err == nil {
						meta.LifetimeLimitUSD = acctSpend
					}
				}
				var ownerName string
				if err := db.QueryRowContext(ctx, `SELECT display_name FROM end_users WHERE tenant_id = ? AND id = ?`, tenantID, meta.EndUserID).Scan(&ownerName); err == nil {
					meta.Subtitle = strings.TrimSpace(ownerName)
				}
			} else {
				meta.LifetimeLimitUSD = spend
			}
			meta.Active = disabled == 0 && !strings.HasPrefix(keySecret, "sk-deleted-")
		}
	}
	if meta.Active && s.usageLookup != nil {
		meta.LifetimeUsedUSD = s.usageLookup(tenantID, kind, targetID)
	}
	return meta
}

func maskKeyForDisplay(secret string) string {
	s := strings.TrimSpace(secret)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
