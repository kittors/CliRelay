package management

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	_ "modernc.org/sqlite"
)

func setupBillingHandlerTest(t *testing.T) (*Handler, *sql.DB, string, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
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
			must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_login_at TEXT,
			failed_login_count INTEGER NOT NULL DEFAULT 0,
			lock_stage INTEGER NOT NULL DEFAULT 0,
			locked_until TEXT,
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
	if err := billing.InitSQLiteTables(db); err != nil {
		t.Fatalf("init billing tables: %v", err)
	}

	tenantID := identity.SystemTenantID
	userID := uuid.NewString()
	keyID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, status, spending_limit)
		VALUES (?, ?, 'bob', 'bob', 'Bob', 'active', 10)
	`, userID, tenantID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO api_keys (key, tenant_id, id, name, disabled, end_user_id, is_default, access_policy)
		VALUES ('sk-XXXXhandlerkey00001', ?, ?, 'Bob Key', 0, ?, 1, '{}')
	`, tenantID, keyID, userID); err != nil {
		t.Fatalf("insert key: %v", err)
	}

	mockGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errcode":    0,
			"openid":     "XH-H-1",
			"url_qrcode": "https://qr.example.test/h1.png",
			"url":        "https://pay.example.test/h1",
		})
	}))
	t.Cleanup(mockGW.Close)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	store := billing.NewStore(db)
	billing.SetDefaultStore(store)
	t.Cleanup(func() { billing.SetDefaultStore(nil) })
	h.SetBillingService(billing.NewService(billing.ServiceOptions{
		Store:       store,
		XunhuClient: billing.NewXunhuClient(mockGW.Client()),
	}))
	euSvc := enduser.NewService(db)
	enduser.SetDefault(euSvc)
	t.Cleanup(func() { enduser.SetDefault(nil) })
	return h, db, mockGW.URL, userID, keyID
}

func TestBillingHandlersAdminPublicAndWebhook(t *testing.T) {
	h, _, gwURL, _, keyID := setupBillingHandlerTest(t)

	engine := gin.New()
	admin := engine.Group("/v0/management")
	admin.Use(func(c *gin.Context) {
		h.setServicePrincipal(c)
		c.Next()
	})
	admin.GET("/billing/settings", h.GetBillingSettings)
	admin.PUT("/billing/settings", h.PutBillingSettings)
	admin.POST("/billing/plans", h.PostBillingPlan)
	admin.GET("/billing/plans", h.GetBillingPlans)
	admin.PUT("/billing/bindings", h.PutBillingBinding)
	admin.GET("/billing/bindings", h.GetBillingBindings)
	admin.PATCH("/billing/entitlements", h.PatchBillingEntitlement)
	admin.GET("/billing/orders", h.GetBillingOrders)
	admin.POST("/billing/orders/manual", h.PostBillingManualOrder)

	pub := engine.Group("/v0/management/public")
	pub.GET("/billing/pay/:code", h.GetPublicBillingPay)
	pub.POST("/billing/pay/:code/orders", h.PostPublicBillingOrder)
	pub.GET("/billing/pay/:code/orders/:order_no", h.GetPublicBillingOrder)
	pub.POST("/billing/callback/xunhupay/:tenant_id/:channel", h.PostPublicXunhuPayCallback)

	doJSON := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Host = "relay.example.test"
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	// 1. Save gateway settings.
	rec := doJSON(http.MethodPut, "/v0/management/billing/settings", map[string]any{
		"enabled":                true,
		"public_base_url":        "https://relay.example.test",
		"xunhu_gateway_url":      gwURL,
		"xunhu_wechat_enabled":   true,
		"xunhu_wechat_appid":     "wx-id-1",
		"xunhu_wechat_appsecret": "wx-secret-1234",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 2. Create plan.
	rec = doJSON(http.MethodPost, "/v0/management/billing/plans", map[string]any{
		"name":             "月卡",
		"kind":             "subscription",
		"price_cents":      3000,
		"duration_days":    30,
		"quota_amount_usd": 10,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	var plan billing.Plan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)

	// 3. Bind plan to specific key with public QR pay enabled.
	rec = doJSON(http.MethodPut, "/v0/management/billing/bindings", map[string]any{
		"target_kind":      "api_key",
		"target_id":        keyID,
		"enabled":          true,
		"plan_ids":         []string{plan.ID},
		"allow_public_pay": true,
		"note":             "扫码即到账",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT binding status=%d body=%s", rec.Code, rec.Body.String())
	}
	var bindingView billing.BindingView
	_ = json.Unmarshal(rec.Body.Bytes(), &bindingView)
	if bindingView.PublicCode == "" {
		t.Fatalf("expected non-empty public_code: %+v", bindingView)
	}

	// 4. Public pay info & checkout order.
	rec = doJSON(http.MethodGet, "/v0/management/public/billing/pay/"+bindingView.PublicCode, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET public pay status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(http.MethodPost, "/v0/management/public/billing/pay/"+bindingView.PublicCode+"/orders", map[string]any{
		"plan_id":     plan.ID,
		"pay_channel": "wechat",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST public order status=%d body=%s", rec.Code, rec.Body.String())
	}
	var order billing.Order
	_ = json.Unmarshal(rec.Body.Bytes(), &order)

	// 5. Simulate XunhuPay async webhook callback.
	cbParams := map[string]string{
		"appid":          "wx-id-1",
		"trade_order_id": order.OrderNo,
		"total_fee":      "30.00",
		"status":         "OD",
		"open_order_id":  "XH-H-1",
		"transaction_id": "WX-999",
	}
	cbParams["hash"] = billing.SignXunhu(cbParams, "wx-secret-1234")
	form := url.Values{}
	for k, v := range cbParams {
		form.Set(k, v)
	}
	cbReq := httptest.NewRequest(http.MethodPost, "/v0/management/public/billing/callback/xunhupay/"+identity.SystemTenantID+"/wechat", strings.NewReader(form.Encode()))
	cbReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cbRec := httptest.NewRecorder()
	engine.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusOK || cbRec.Body.String() != "success" {
		t.Fatalf("callback status=%d body=%q, want 200 success", cbRec.Code, cbRec.Body.String())
	}

	// 6. Verify public order status is now fulfilled & admin order list reflects revenue.
	rec = doJSON(http.MethodGet, "/v0/management/public/billing/pay/"+bindingView.PublicCode+"/orders/"+order.OrderNo, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET public order status=%d body=%s", rec.Code, rec.Body.String())
	}
	var fetched billing.Order
	_ = json.Unmarshal(rec.Body.Bytes(), &fetched)
	if fetched.Status != billing.OrderStatusFulfilled {
		t.Fatalf("expected fulfilled status, got %s", fetched.Status)
	}

	rec = doJSON(http.MethodGet, "/v0/management/billing/orders", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET orders status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list billing.OrderListResult
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Total != 1 || list.Summary.TotalAmountCents != 3000 || list.Summary.PaidCount != 1 {
		t.Fatalf("unexpected order list summary: %+v", list)
	}

	// 7. Operator freely adjusts key duration via PATCH /billing/entitlements.
	rec = doJSON(http.MethodPatch, "/v0/management/billing/entitlements", map[string]any{
		"target_kind": "api_key",
		"target_id":   keyID,
		"mode":        "add_days",
		"delta_days":  60,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH entitlements status=%d body=%s", rec.Code, rec.Body.String())
	}
	var entView billing.EntitlementView
	_ = json.Unmarshal(rec.Body.Bytes(), &entView)
	if entView.ExpiresAt == nil || time.Until(*entView.ExpiresAt).Hours()/24 < 89 {
		t.Fatalf("expected expiry extended to ~90 days (30+60), got %+v", entView)
	}
}
