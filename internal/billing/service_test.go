package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	_ "modernc.org/sqlite"
)

func openBillingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqlapikey.InitTable(db)
	sqlapikey.InitPermissionProfilesTable(db)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL DEFAULT 'x',
			status TEXT NOT NULL DEFAULT 'active',
			permission_profile_id TEXT NOT NULL DEFAULT '',
			daily_limit INTEGER NOT NULL DEFAULT 0,
			total_quota INTEGER NOT NULL DEFAULT 0,
			spending_limit REAL NOT NULL DEFAULT 0,
			daily_spending_limit REAL NOT NULL DEFAULT 0,
			five_hour_spending_limit REAL NOT NULL DEFAULT 0,
			weekly_spending_limit REAL NOT NULL DEFAULT 0,
			monthly_spending_limit REAL NOT NULL DEFAULT 0,
			concurrency_limit INTEGER NOT NULL DEFAULT 0,
			rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_channels TEXT NOT NULL DEFAULT '[]',
			allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
			system_prompt TEXT NOT NULL DEFAULT '',
			access_policy TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			version INTEGER NOT NULL DEFAULT 1
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := InitSQLiteTables(db); err != nil {
		t.Fatalf("init billing tables: %v", err)
	}
	return db
}

func seedUserAndKey(t *testing.T, db *sql.DB, tenantID, username, displayName string, acctSpend float64, keyName, keySecret string) (string, string) {
	t.Helper()
	userID := uuid.NewString()
	keyID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, status, spending_limit)
		VALUES (?, ?, ?, ?, ?, 'active', ?)
	`, userID, tenantID, username, username, displayName, acctSpend); err != nil {
		t.Fatalf("insert end_user: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO api_keys (key, tenant_id, id, name, disabled, end_user_id, is_default, access_policy)
		VALUES (?, ?, ?, ?, 0, ?, 1, '{}')
	`, keySecret, tenantID, keyID, keyName, userID); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	return userID, keyID
}

func TestBillingEndToEndSubscriptionQuotaAndIdempotency(t *testing.T) {
	t.Parallel()
	db := openBillingTestDB(t)
	tenantID := "00000000-0000-0000-0000-000000000001"
	otherTenantID := "00000000-0000-0000-0000-000000000002"
	userID, keyID := seedUserAndKey(t, db, tenantID, "alice", "Alice", 20, "Main Key", "sk-XXXXtestkey00000001")

	secret := "xunhu-wechat-secret-9999"
	appID := "wx-appid-1"
	var queryPaid atomic.Bool
	mockGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/do":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errcode":    0,
				"openid":     "XH-" + r.PostForm.Get("trade_order_id"),
				"url_qrcode": "https://qr.example.test/" + r.PostForm.Get("trade_order_id") + ".png",
				"url":        "https://pay.example.test/" + r.PostForm.Get("trade_order_id"),
			})
		case "/query":
			st := "WP"
			if queryPaid.Load() {
				st = "OD"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errcode": 0,
				"data": map[string]any{
					"status":        st,
					"open_order_id": "XH-SYNC-1",
				},
			})
		}
	}))
	defer mockGW.Close()

	var resetCount atomic.Int32
	var cacheRefreshes atomic.Int32
	svc := NewService(ServiceOptions{
		Store:       NewStore(db),
		XunhuClient: NewXunhuClient(mockGW.Client()),
		DailyReset: func(_ context.Context, _ string, _ TargetKind, _, _ string) error {
			resetCount.Add(1)
			return nil
		},
		OnQuotaChanged: func() error {
			cacheRefreshes.Add(1)
			return nil
		},
	})

	ctx := context.Background()
	enabled := true
	wxAppID := appID
	wxSecret := secret
	gwURL := mockGW.URL + "/do"
	qURL := mockGW.URL + "/query"
	baseURL := "https://relay.example.test"

	// 1. Configure settings & verify secret masking preservation.
	view, err := svc.UpdateSettings(ctx, tenantID, "admin", "", SettingsPatch{
		Enabled:            &enabled,
		PublicBaseURL:      &baseURL,
		XunhuGatewayURL:    &gwURL,
		XunhuQueryURL:      &qURL,
		XunhuWeChatEnabled: &enabled,
		XunhuWeChatAppID:   &wxAppID,
		XunhuWeChatSecret:  &wxSecret,
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if view.XunhuWeChatSecret != "****9999" || !view.XunhuWeChatHasKey {
		t.Fatalf("unexpected masked secret: %+v", view)
	}
	// Echoing masked secret must preserve the stored plaintext secret.
	maskedEcho := view.XunhuWeChatSecret
	if _, err := svc.UpdateSettings(ctx, tenantID, "admin", "", SettingsPatch{XunhuWeChatSecret: &maskedEcho}); err != nil {
		t.Fatalf("UpdateSettings preserve secret: %v", err)
	}
	stored, _, err := svc.Store().GetSettings(ctx, tenantID)
	if err != nil || stored.XunhuWeChatSecret != secret {
		t.Fatalf("stored secret lost: got %q want %q", stored.XunhuWeChatSecret, secret)
	}

	// 2. Create plans (subscription, quota pack, custom quota, daily reset).
	subPlan, err := svc.CreatePlan(ctx, tenantID, PlanInput{
		Name:           "包月标准版",
		Kind:           PlanKindSubscription,
		PriceCents:     3000,
		DurationDays:   30,
		QuotaAmountUSD: 5,
		Badge:          "推荐",
	})
	if err != nil {
		t.Fatalf("CreatePlan sub: %v", err)
	}
	packPlan, err := svc.CreatePlan(ctx, tenantID, PlanInput{
		Name:           "额度加油包",
		Kind:           PlanKindQuotaPack,
		PriceCents:     2000,
		QuotaAmountUSD: 10,
	})
	if err != nil {
		t.Fatalf("CreatePlan pack: %v", err)
	}
	customPlan, err := svc.CreatePlan(ctx, tenantID, PlanInput{
		Name:           "自定义充值",
		Kind:           PlanKindCustomQuota,
		RateUSDPerCNY:  0.5,
		MinAmountCents: 500,
		MaxAmountCents: 50000,
	})
	if err != nil {
		t.Fatalf("CreatePlan custom: %v", err)
	}
	resetPlan, err := svc.CreatePlan(ctx, tenantID, PlanInput{
		Name:       "今日额度重置",
		Kind:       PlanKindDailyReset,
		PriceCents: 500,
	})
	if err != nil {
		t.Fatalf("CreatePlan reset: %v", err)
	}

	// 3. Bind plans to the specific API key with a per-key price override & shareable QR code.
	bindingView, err := svc.UpsertBinding(ctx, tenantID, baseURL, BindingInput{
		TargetKind:     TargetKindAPIKey,
		TargetID:       keyID,
		Enabled:        true,
		PlanIDs:        []string{subPlan.ID, packPlan.ID, customPlan.ID, resetPlan.ID},
		PriceOverrides: map[string]int{subPlan.ID: 2500},
		AllowPublicPay: true,
		Note:           "主力 Key 专属续费",
	})
	if err != nil {
		t.Fatalf("UpsertBinding: %v", err)
	}
	if bindingView.PublicCode == "" || bindingView.PublicPayURL == "" {
		t.Fatalf("expected public pay code and URL, got %+v", bindingView)
	}

	// 4. Verify portal overview exposes the key binding with overridden price.
	overview, err := svc.GetPortalOverview(ctx, tenantID, userID)
	if err != nil || !overview.Enabled || len(overview.Targets) != 1 {
		t.Fatalf("unexpected overview: %+v err=%v", overview, err)
	}
	if overview.Targets[0].Plans[0].PriceCents != 2500 {
		t.Fatalf("expected overridden price 2500, got %d", overview.Targets[0].Plans[0].PriceCents)
	}

	// 5. Create portal subscription order & complete via XunhuPay callback (with idempotent replay!).
	order, err := svc.CreatePortalOrder(ctx, tenantID, userID, "alice", baseURL, CreateOrderInput{
		BindingID:  bindingView.ID,
		PlanID:     subPlan.ID,
		PayChannel: ChannelWeChat,
	})
	if err != nil {
		t.Fatalf("CreatePortalOrder: %v", err)
	}
	if order.AmountCents != 2500 || order.QRCodeURL == "" {
		t.Fatalf("unexpected created order: %+v", order)
	}

	// Tampered amount must fail.
	badParams := map[string]string{
		"appid":          appID,
		"trade_order_id": order.OrderNo,
		"total_fee":      "0.01",
		"status":         "OD",
		"open_order_id":  "XH-1",
	}
	badParams["hash"] = SignXunhu(badParams, secret)
	if err := svc.HandleXunhuCallback(ctx, tenantID, ChannelWeChat, badParams); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("expected ErrAmountMismatch, got %v", err)
	}

	// Valid callback fulfills once; replay must NOT double-extend or double-add quota!
	cbParams := map[string]string{
		"appid":          appID,
		"trade_order_id": order.OrderNo,
		"total_fee":      "25.00",
		"status":         "OD",
		"open_order_id":  "XH-1",
		"transaction_id": "WX-TX-1",
	}
	cbParams["hash"] = SignXunhu(cbParams, secret)
	if err := svc.HandleXunhuCallback(ctx, tenantID, ChannelWeChat, cbParams); err != nil {
		t.Fatalf("HandleXunhuCallback first: %v", err)
	}
	if err := svc.HandleXunhuCallback(ctx, tenantID, ChannelWeChat, cbParams); err != nil {
		t.Fatalf("HandleXunhuCallback replay: %v", err)
	}

	// Verify key expiry (~30d), key lifetime USD (+$5), AND owning account ceiling auto-raised ($20 -> $25).
	exp, err := svc.Store().GetEntitlement(ctx, tenantID, TargetKindAPIKey, keyID)
	if err != nil || exp == nil {
		t.Fatalf("expected key expiry set, got %v err=%v", exp, err)
	}
	daysLeft := time.Until(*exp).Hours() / 24
	if daysLeft < 29.5 || daysLeft > 30.5 {
		t.Fatalf("expected ~30 days expiry (no double extend), got %.2f days", daysLeft)
	}
	var keyPolicyJSON string
	var acctSpend float64
	if err := db.QueryRow(`SELECT access_policy FROM api_keys WHERE id = ?`, keyID).Scan(&keyPolicyJSON); err != nil {
		t.Fatalf("query key policy: %v", err)
	}
	if err := db.QueryRow(`SELECT spending_limit FROM end_users WHERE id = ?`, userID).Scan(&acctSpend); err != nil {
		t.Fatalf("query account spend: %v", err)
	}
	keyPolicy, _ := quota.DecodeAccessPolicy(keyPolicyJSON)
	if keyPolicy.Limits.Cost.Lifetime != 5 {
		t.Fatalf("expected key lifetime cost $5 (once), got %.2f", keyPolicy.Limits.Cost.Lifetime)
	}
	if acctSpend != 25 {
		t.Fatalf("expected account ceiling auto-raised from $20 to $25, got %.2f", acctSpend)
	}

	// 6. Public direct-pay custom quota order confirmed via active SyncOrderStatus polling.
	pubInfo, err := svc.GetPublicPayInfo(ctx, bindingView.PublicCode)
	if err != nil || pubInfo.Target.KeyMasked == "" {
		t.Fatalf("GetPublicPayInfo: %+v err=%v", pubInfo, err)
	}
	pubOrder, err := svc.CreatePublicOrder(ctx, bindingView.PublicCode, baseURL, CreateOrderInput{
		PlanID:            customPlan.ID,
		PayChannel:        ChannelWeChat,
		CustomAmountCents: 2000, // ¥20 * 0.5 = +$10
	})
	if err != nil {
		t.Fatalf("CreatePublicOrder: %v", err)
	}
	queryPaid.Store(true)
	synced, err := svc.GetOrderForPublicCode(ctx, bindingView.PublicCode, pubOrder.OrderNo, true)
	if err != nil || synced.Status != OrderStatusFulfilled {
		t.Fatalf("expected synced public order fulfilled, got %+v err=%v", synced, err)
	}
	if err := db.QueryRow(`SELECT access_policy FROM api_keys WHERE id = ?`, keyID).Scan(&keyPolicyJSON); err != nil {
		t.Fatalf("query key policy 2: %v", err)
	}
	keyPolicy, _ = quota.DecodeAccessPolicy(keyPolicyJSON)
	if keyPolicy.Limits.Cost.Lifetime != 15 {
		t.Fatalf("expected key lifetime cost $15 ($5+$10), got %.2f", keyPolicy.Limits.Cost.Lifetime)
	}

	// 7. Manual offline daily reset order executes daily reset hook immediately.
	manualReset, err := svc.CreateManualOrder(ctx, tenantID, "admin", ManualOrderInput{
		TargetKind: TargetKindAPIKey,
		TargetID:   keyID,
		PlanID:     resetPlan.ID,
		Note:       "offline transfer",
	})
	if err != nil || manualReset.Status != OrderStatusFulfilled {
		t.Fatalf("CreateManualOrder reset: %+v err=%v", manualReset, err)
	}
	if resetCount.Load() != 1 {
		t.Fatalf("expected daily reset invoked once, got %d", resetCount.Load())
	}

	// 8. Tenant isolation: another tenant cannot read this tenant's orders or plans.
	otherOrders, err := svc.ListOrders(ctx, otherTenantID, OrderFilter{})
	if err != nil || otherOrders.Total != 0 {
		t.Fatalf("expected 0 orders for other tenant, got %+v err=%v", otherOrders, err)
	}
	if _, err := svc.GetOrderForPublicCode(ctx, "wrong-code", pubOrder.OrderNo, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected wrong public code rejected with ErrNotFound, got %v", err)
	}
	if cacheRefreshes.Load() < 2 {
		t.Fatalf("expected cache refresh callback invoked on fulfillment, got %d", cacheRefreshes.Load())
	}

	// 9. Historical offline order (e.g. Oct 26 paid ¥400 for 2 months = 61 days) + operator free expiry adjustments.
	paidOct26 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	amt400 := 40000
	histKeyID := "key-hist-1"
	if _, err := db.Exec(`INSERT INTO api_keys (id, tenant_id, key, name, end_user_id, disabled, access_policy) VALUES (?, ?, ?, ?, ?, 0, '{}')`,
		histKeyID, tenantID, "sk-TEST-XXXX-HIST-0001", "Historical Key", userID); err != nil {
		t.Fatalf("seed historical key: %v", err)
	}
	histOrder, err := svc.CreateManualOrder(ctx, tenantID, "admin", ManualOrderInput{
		TargetKind:   TargetKindAPIKey,
		TargetID:     histKeyID,
		PlanID:       subPlan.ID,
		AmountCents:  &amt400,
		PaidAt:       &paidOct26,
		DurationDays: 61,
		ExtendMode:   ManualExtendFromPaidAt,
		Note:         "10月26日线下付款400元两个月",
	})
	if err != nil || histOrder.Status != OrderStatusFulfilled {
		t.Fatalf("CreateManualOrder historical: %+v err=%v", histOrder, err)
	}
	if histOrder.AmountCents != 40000 || histOrder.PaidAt == nil || !histOrder.PaidAt.Equal(paidOct26) {
		t.Fatalf("expected backdated paid_at %v and amount 40000, got %+v", paidOct26, histOrder)
	}
	histExp, err := svc.Store().GetEntitlement(ctx, tenantID, TargetKindAPIKey, histKeyID)
	if err != nil || histExp == nil {
		t.Fatalf("expected historical expiry set, got %v err=%v", histExp, err)
	}
	wantDec26 := paidOct26.Add(61 * 24 * time.Hour)
	if !histExp.Equal(wantDec26) {
		t.Fatalf("expected expiry %v (Oct 26 + 61d), got %v", wantDec26, *histExp)
	}

	// Record-only manual order preserves expiry untouched and allows 0-cent comp.
	zeroAmt := 0
	recOnly, err := svc.CreateManualOrder(ctx, tenantID, "admin", ManualOrderInput{
		TargetKind:  TargetKindAPIKey,
		TargetID:    histKeyID,
		PlanID:      subPlan.ID,
		AmountCents: &zeroAmt,
		ExtendMode:  ManualExtendRecordOnly,
		Note:        "audit backfill",
	})
	if err != nil || recOnly.AmountCents != 0 || recOnly.Status != OrderStatusFulfilled {
		t.Fatalf("CreateManualOrder record_only: %+v err=%v", recOnly, err)
	}
	histExpAfter, _ := svc.Store().GetEntitlement(ctx, tenantID, TargetKindAPIKey, histKeyID)
	if histExpAfter == nil || !histExpAfter.Equal(wantDec26) {
		t.Fatalf("record_only must not alter expiry: got %v want %v", histExpAfter, wantDec26)
	}

	// AdjustEntitlement: freely extend +30d, shorten -10d, set arbitrary future date, and clear.
	refreshesBefore := cacheRefreshes.Load()
	extView, err := svc.AdjustEntitlement(ctx, tenantID, AdjustEntitlementInput{
		TargetKind: TargetKindAPIKey,
		TargetID:   histKeyID,
		Mode:       EntitlementModeAddDays,
		DeltaDays:  30,
	})
	if err != nil || extView.ExpiresAt == nil {
		t.Fatalf("AdjustEntitlement +30d: %+v err=%v", extView, err)
	}
	if !extView.ExpiresAt.Equal(wantDec26.Add(30 * 24 * time.Hour)) {
		t.Fatalf("expected expiry extended +30d from Dec 26 to %v, got %v", wantDec26.Add(30*24*time.Hour), *extView.ExpiresAt)
	}
	shortView, err := svc.AdjustEntitlement(ctx, tenantID, AdjustEntitlementInput{
		TargetKind: TargetKindAPIKey,
		TargetID:   histKeyID,
		Mode:       EntitlementModeAddDays,
		DeltaDays:  -10,
	})
	if err != nil || shortView.ExpiresAt == nil || !shortView.ExpiresAt.Equal(wantDec26.Add(20*24*time.Hour)) {
		t.Fatalf("AdjustEntitlement -10d: %+v err=%v", shortView, err)
	}
	customDate := time.Date(2027, 6, 1, 15, 59, 59, 0, time.UTC)
	setView, err := svc.AdjustEntitlement(ctx, tenantID, AdjustEntitlementInput{
		TargetKind: TargetKindAPIKey,
		TargetID:   histKeyID,
		Mode:       EntitlementModeSet,
		ExpiresAt:  &customDate,
	})
	if err != nil || setView.ExpiresAt == nil || !setView.ExpiresAt.Equal(customDate) {
		t.Fatalf("AdjustEntitlement set: %+v err=%v", setView, err)
	}
	clearView, err := svc.AdjustEntitlement(ctx, tenantID, AdjustEntitlementInput{
		TargetKind: TargetKindAPIKey,
		TargetID:   histKeyID,
		Mode:       EntitlementModeSet,
		ExpiresAt:  nil,
	})
	if err != nil || clearView.ExpiresAt != nil {
		t.Fatalf("AdjustEntitlement clear: %+v err=%v", clearView, err)
	}
	if cacheRefreshes.Load() < refreshesBefore+4 {
		t.Fatalf("expected cache refresh on every AdjustEntitlement call")
	}

	// 10. Key-scoped billing overview, checkout, order history & cross-key isolation (/#/apikey-usage).
	keyOv, err := svc.GetAPIKeyOverview(ctx, tenantID, keyID)
	if err != nil || !keyOv.Enabled || !keyOv.HasBillingInfo || len(keyOv.Target.Plans) != 4 {
		t.Fatalf("GetAPIKeyOverview: %+v err=%v", keyOv, err)
	}
	if keyOv.Target.ExpiresAt == nil || keyOv.OrdersCount < 2 {
		t.Fatalf("expected key overview expiry and order count >= 2, got %+v", keyOv)
	}
	keyOrder, err := svc.CreateAPIKeyOrder(ctx, tenantID, keyID, baseURL, CreateOrderInput{
		PlanID:     subPlan.ID,
		PayChannel: ChannelWeChat,
	})
	if err != nil || keyOrder.Source != "apikey_usage" || keyOrder.Status != OrderStatusPending {
		t.Fatalf("CreateAPIKeyOrder: %+v err=%v", keyOrder, err)
	}
	keyOrders, err := svc.ListAPIKeyOrders(ctx, tenantID, keyID, 1, 20)
	if err != nil || keyOrders.Total < 3 {
		t.Fatalf("ListAPIKeyOrders: %+v err=%v", keyOrders, err)
	}
	// Sibling key must NOT see keyID's orders.
	if _, err := svc.GetOrderForAPIKey(ctx, tenantID, histKeyID, keyOrder.OrderNo, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected sibling key denied with ErrNotFound, got %v", err)
	}
	// histKeyID has no binding but has historical orders -> HasBillingInfo=true, Enabled=false.
	histOv, err := svc.GetAPIKeyOverview(ctx, tenantID, histKeyID)
	if err != nil || histOv.Enabled || !histOv.HasBillingInfo || histOv.OrdersCount != 2 {
		t.Fatalf("GetAPIKeyOverview histKeyID: %+v err=%v", histOv, err)
	}
}
