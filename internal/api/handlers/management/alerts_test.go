package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

const (
	alertTenantA = "00000000-0000-0000-0000-0000000000a1"
	alertTenantB = "00000000-0000-0000-0000-0000000000b2"
)

// recordingSender answers for the delivery channels without a network.
type recordingSender struct {
	mu     sync.Mutex
	calls  [][]alerting.Channel
	status string
	reason string
}

func (s *recordingSender) Send(_ context.Context, channels []alerting.Channel, _ alerting.Message, _ bool) []alerting.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]alerting.Channel(nil), channels...))
	status := s.status
	if status == "" {
		status = alerting.DeliverySent
	}
	out := make([]alerting.Delivery, 0, len(channels))
	for _, c := range channels {
		out = append(out, alerting.Delivery{ChannelID: c.ID, ChannelName: c.Name, Kind: c.Kind, Status: status, Error: s.reason, Duration: 12 * time.Millisecond})
	}
	return out
}

// alertAPI serves the four /alerts routes for one tenant, the way the real
// router does after authentication.
type alertAPI struct {
	t      *testing.T
	router *gin.Engine
	store  *alerting.MemoryStore
	sender *recordingSender
}

func newAlertAPI(t *testing.T, tenant string) *alertAPI {
	t.Helper()
	store := alerting.NewMemoryStore()
	sender := &recordingSender{}
	return newAlertAPIWith(t, tenant, store, sender)
}

func newAlertAPIWith(t *testing.T, tenant string, store alerting.Store, sender alerting.Sender) *alertAPI {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &Handler{alertService: alerting.NewService(store, sender)}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenant}})
	})
	router.GET("/alerts/settings", h.GetAlertSettings)
	router.PUT("/alerts/settings", h.PutAlertSettings)
	router.POST("/alerts/test", h.PostAlertTest)
	router.GET("/alerts/events", h.GetAlertEvents)
	api := &alertAPI{t: t, router: router}
	api.store, _ = store.(*alerting.MemoryStore)
	api.sender, _ = sender.(*recordingSender)
	return api
}

func (a *alertAPI) do(method, path, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

func (a *alertAPI) expect(method, path, body string, status int) *httptest.ResponseRecorder {
	a.t.Helper()
	rec := a.do(method, path, body)
	if rec.Code != status {
		a.t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
	}
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"error"`
}

const (
	webhookURL    = "https://hooks.example.com/services/T000/B000/abcdefghijklmnop"
	webhookSecret = "whsec_super_secret_signing_key"
)

func TestAlertSettingsStartAsDefaultsAndOff(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	rec := api.expect(http.MethodGet, "/alerts/settings", "", http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: the view holds the tail of webhook addresses", got)
	}
	view := decode[alerting.SettingsView](t, rec)
	if view.Enabled {
		t.Fatal("alerting must be off until someone switches it on")
	}
	if len(view.Channels) != 0 || !view.Persistent || view.Limits.MaxChannels != alerting.MaxChannels {
		t.Fatalf("view = %+v", view)
	}
	if !view.Rules.QuotaWindow.Enabled || view.Rules.QuotaWindow.ThresholdPercent != 90 || view.Rules.UseItOrLoseIt.Enabled {
		t.Fatalf("rules = %+v, want the documented defaults (quota 90%%, use-it-or-lose-it off)", view.Rules)
	}
	// The raw body must carry empty arrays, not null: the panel maps over them.
	if !strings.Contains(rec.Body.String(), `"channels":[]`) {
		t.Fatalf("body = %s, want \"channels\":[]", rec.Body.String())
	}
}

func TestAlertSettingsRoundTripMasksEverySecret(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	body := `{"enabled":true,"channels":[{"kind":"webhook","name":"Ops webhook","url":"` + webhookURL + `","secret":"` + webhookSecret + `"}]}`
	rec := api.expect(http.MethodPut, "/alerts/settings", body, http.StatusOK)
	for _, leaked := range []string{webhookSecret, "abcdefghijklmnop", "T000/B000", "/services/"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("the response to a write carries %q: %s", leaked, rec.Body.String())
		}
	}
	view := decode[alerting.SettingsView](t, rec)
	if !view.Enabled || len(view.Channels) != 1 {
		t.Fatalf("view = %+v", view)
	}
	ch := view.Channels[0]
	if ch.ID == "" || ch.Kind != "webhook" || ch.Name != "Ops webhook" || !ch.Enabled {
		t.Fatalf("channel = %+v", ch)
	}
	if ch.URL != "https://hooks.example.com/****mnop" || ch.Secret != "****" {
		t.Fatalf("masks = url %q secret %q", ch.URL, ch.Secret)
	}

	// Reading back is masked the same way.
	get := api.expect(http.MethodGet, "/alerts/settings", "", http.StatusOK)
	for _, leaked := range []string{webhookSecret, "abcdefghijklmnop"} {
		if strings.Contains(get.Body.String(), leaked) {
			t.Fatalf("reading the settings back reveals %q: %s", leaked, get.Body.String())
		}
	}

	// ...but the store holds the real values: the masks are a view, not a rewrite.
	stored, _, _, _ := api.store.LoadSettings(alertTenantA)
	if stored.Channels[0].URL != webhookURL || stored.Channels[0].Secret != webhookSecret {
		t.Fatalf("stored channel = %+v, want the real address and secret", stored.Channels[0])
	}

	// Saving the form again with the masks the page was shown keeps the secrets.
	echo := `{"channels":[{"id":"` + ch.ID + `","kind":"webhook","name":"Renamed","url":"` + ch.URL + `","secret":"` + ch.Secret + `"}]}`
	api.expect(http.MethodPut, "/alerts/settings", echo, http.StatusOK)
	stored, _, _, _ = api.store.LoadSettings(alertTenantA)
	if stored.Channels[0].URL != webhookURL || stored.Channels[0].Secret != webhookSecret || stored.Channels[0].Name != "Renamed" {
		t.Fatalf("after echoing the masks the channel = %+v, want the secrets kept and the new name", stored.Channels[0])
	}

	// An empty string clears; a field that is left out keeps.
	api.expect(http.MethodPut, "/alerts/settings", `{"channels":[{"id":"`+ch.ID+`","kind":"webhook","secret":""}]}`, http.StatusOK)
	stored, _, _, _ = api.store.LoadSettings(alertTenantA)
	if stored.Channels[0].Secret != "" || stored.Channels[0].URL != webhookURL {
		t.Fatalf("after clearing the secret the channel = %+v", stored.Channels[0])
	}
}

func TestAlertSettingsPartialWritesKeepTheRest(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true,"cooldown_minutes":120,"channels":[{"kind":"webhook","name":"w","url":"`+webhookURL+`"}]}`, http.StatusOK)
	rec := api.expect(http.MethodPut, "/alerts/settings", `{"rules":{"quota_window":{"threshold_percent":75}}}`, http.StatusOK)
	view := decode[alerting.SettingsView](t, rec)
	if !view.Enabled || view.CooldownMinutes != 120 || len(view.Channels) != 1 {
		t.Fatalf("a write of one threshold changed more than that: %+v", view)
	}
	if view.Rules.QuotaWindow.ThresholdPercent != 75 || !view.Rules.QuotaWindow.Enabled {
		t.Fatalf("quota rule = %+v", view.Rules.QuotaWindow)
	}
	empty := api.expect(http.MethodPut, "/alerts/settings", `{}`, http.StatusOK)
	if view2 := decode[alerting.SettingsView](t, empty); view2.Rules.QuotaWindow.ThresholdPercent != 75 || len(view2.Channels) != 1 {
		t.Fatalf("an empty write changed the settings: %+v", view2)
	}
}

func TestAlertSettingsRejectInvalidWritesWithTheFieldNamed(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"plain http for a tenant", `{"channels":[{"kind":"webhook","name":"w","url":"http://hooks.example.com/x"}]}`, "channels[0].url"},
		{"a private address for a tenant", `{"channels":[{"kind":"webhook","name":"w","url":"https://10.1.2.3/hook"}]}`, "channels[0].url"},
		{"an internal host name for a tenant", `{"channels":[{"kind":"webhook","name":"w","url":"https://metadata.internal/hook"}]}`, "channels[0].url"},
		{"a feishu address that is not feishu", `{"channels":[{"kind":"feishu","name":"f","url":"https://evil.example.com/open-apis/bot/v2/hook/abcdefghijkl"}]}`, "channels[0].url"},
		{"a malformed telegram token", `{"channels":[{"kind":"telegram","name":"t","bot_token":"not-a-token","chat_id":"-100123"}]}`, "channels[0].bot_token"},
		{"an unknown kind", `{"channels":[{"kind":"carrier-pigeon","name":"p"}]}`, "channels[0].kind"},
		{"a threshold above 100", `{"rules":{"quota_window":{"threshold_percent":120}}}`, "rules.quota_window.threshold_percent"},
		{"a cooldown below the minimum", `{"cooldown_minutes":1}`, "cooldown_minutes"},
		{"a stale mask that matches nothing", `{"channels":[{"kind":"webhook","name":"w","url":"https://hooks.example.com/****zzzz"}]}`, "channels[0].url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAlertAPI(t, alertTenantA)
			rec := api.expect(http.MethodPut, "/alerts/settings", c.body, http.StatusBadRequest)
			got := decode[apiError](t, rec)
			if got.Error.Code != "invalid_alert_settings" || got.Error.Field != c.field || got.Error.Message == "" {
				t.Fatalf("error = %+v, want invalid_alert_settings on %s", got.Error, c.field)
			}
			if _, _, found, _ := api.store.LoadSettings(alertTenantA); found {
				t.Fatal("a rejected write must change nothing")
			}
		})
	}
}

func TestAlertRejectionNeverEchoesTheSubmittedSecret(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	rec := api.expect(http.MethodPut, "/alerts/settings",
		`{"channels":[{"kind":"webhook","name":"w","url":"http://10.0.0.5/hook?token=SECRETTOKEN123","secret":"HMACKEY456"}]}`, http.StatusBadRequest)
	for _, leaked := range []string{"SECRETTOKEN123", "HMACKEY456", "10.0.0.5"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("the error response echoes %q: %s", leaked, rec.Body.String())
		}
	}
}

func TestOnlyThePlatformOperatorMayAimAWebhookInward(t *testing.T) {
	system := newAlertAPI(t, identity.SystemTenantID)
	system.expect(http.MethodPut, "/alerts/settings", `{"channels":[{"kind":"webhook","name":"local relay","url":"http://127.0.0.1:9000/alerts"}]}`, http.StatusOK)

	tenant := newAlertAPI(t, alertTenantA)
	rec := tenant.expect(http.MethodPut, "/alerts/settings", `{"channels":[{"kind":"webhook","name":"local relay","url":"http://127.0.0.1:9000/alerts"}]}`, http.StatusBadRequest)
	if got := decode[apiError](t, rec); got.Error.Field != "channels[0].url" {
		t.Fatalf("error = %+v", got.Error)
	}
}

func TestAlertRequestsWithABrokenBodyAreRefused(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/alerts/settings", `{"enabled":`},
		{http.MethodPut, "/alerts/settings", `[1,2,3]`},
		{http.MethodPost, "/alerts/test", `{"channel_id":`},
	} {
		rec := api.expect(c.method, c.path, c.body, http.StatusBadRequest)
		if got := decode[apiError](t, rec); got.Error.Code != "invalid_request" {
			t.Fatalf("%s %s error = %+v, want invalid_request", c.method, c.path, got.Error)
		}
	}
}

func TestAlertSettingsCannotBeSavedWithoutAStore(t *testing.T) {
	store := alerting.NewMemoryStore()
	store.Persist = false
	api := newAlertAPIWith(t, alertTenantA, store, &recordingSender{})

	view := decode[alerting.SettingsView](t, api.expect(http.MethodGet, "/alerts/settings", "", http.StatusOK))
	if view.Persistent {
		t.Fatal("the view must say that nothing would be saved")
	}
	rec := api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true}`, http.StatusServiceUnavailable)
	if got := decode[apiError](t, rec); got.Error.Code != "alerts_unavailable" {
		t.Fatalf("error = %+v", got.Error)
	}
}

// failingStore wraps a store and fails writes the way a database does.
type failingStore struct {
	*alerting.MemoryStore
	err error
}

func (s failingStore) SaveSettings(string, alerting.Settings, int64) error { return s.err }

func TestAlertWriteErrorsAreAnsweredWithoutLeakingInternals(t *testing.T) {
	t.Run("a lost race reports a conflict", func(t *testing.T) {
		api := newAlertAPIWith(t, alertTenantA, failingStore{MemoryStore: alerting.NewMemoryStore(), err: alerting.ErrConflict}, &recordingSender{})
		rec := api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true}`, http.StatusConflict)
		if got := decode[apiError](t, rec); got.Error.Code != "alerts_conflict" {
			t.Fatalf("error = %+v", got.Error)
		}
	})
	t.Run("a database failure is a plain 500", func(t *testing.T) {
		api := newAlertAPIWith(t, alertTenantA,
			failingStore{MemoryStore: alerting.NewMemoryStore(), err: errors.New(`pq: password authentication failed for user "cliproxy" host=10.9.8.7`)}, &recordingSender{})
		rec := api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true}`, http.StatusInternalServerError)
		if got := decode[apiError](t, rec); got.Error.Code != "alerts_failed" {
			t.Fatalf("error = %+v", got.Error)
		}
		for _, leaked := range []string{"password", "cliproxy", "10.9.8.7"} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Fatalf("the 500 body leaks %q: %s", leaked, rec.Body.String())
			}
		}
	})
}

func TestAlertTestSendsAndReportsEachChannel(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true,"channels":[{"kind":"webhook","name":"w","url":"`+webhookURL+`"}]}`, http.StatusOK)

	rec := api.expect(http.MethodPost, "/alerts/test", ``, http.StatusOK)
	resp := decode[alerting.TestResponse](t, rec)
	if len(resp.Results) != 1 || !resp.Results[0].OK || resp.Results[0].Status != alerting.DeliverySent || resp.Results[0].Kind != "webhook" {
		t.Fatalf("results = %+v", resp.Results)
	}
	if len(api.sender.calls) != 1 || api.sender.calls[0][0].URL != webhookURL {
		t.Fatalf("the sender saw %+v, want one call with the real address", api.sender.calls)
	}
	for _, leaked := range []string{"abcdefghijklmnop", "T000/B000"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("the test response carries %q: %s", leaked, rec.Body.String())
		}
	}

	// A test is recorded in the history like any other message.
	events := decode[alerting.EventsResponse](t, api.expect(http.MethodGet, "/alerts/events", "", http.StatusOK))
	if events.Total != 1 || events.Items[0].Rule != alerting.RuleTest || len(events.Items[0].Deliveries) != 1 {
		t.Fatalf("history = %+v", events)
	}
}

func TestAlertTestDraftChannelIsNotSaved(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	body := `{"channel":{"kind":"telegram","name":"draft","bot_token":"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw","chat_id":"-1001234567890"}}`
	resp := decode[alerting.TestResponse](t, api.expect(http.MethodPost, "/alerts/test", body, http.StatusOK))
	if len(resp.Results) != 1 || resp.Results[0].ChannelName != "draft" {
		t.Fatalf("results = %+v", resp.Results)
	}
	if _, _, found, _ := api.store.LoadSettings(alertTenantA); found {
		t.Fatal("trying a draft must not save anything")
	}
}

func TestAlertTestReportsAFailureWithoutFailingTheRequest(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	api.sender.status, api.sender.reason = alerting.DeliveryFailed, "the receiver answered 500"
	api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true,"channels":[{"kind":"webhook","name":"w","url":"`+webhookURL+`"}]}`, http.StatusOK)
	resp := decode[alerting.TestResponse](t, api.expect(http.MethodPost, "/alerts/test", `{}`, http.StatusOK))
	if len(resp.Results) != 1 || resp.Results[0].OK || resp.Results[0].Error != "the receiver answered 500" {
		t.Fatalf("results = %+v, want the failure and its reason", resp.Results)
	}
}

func TestAlertTestRefusalsAreStructured(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	rec := api.expect(http.MethodPost, "/alerts/test", `{}`, http.StatusBadRequest)
	if got := decode[apiError](t, rec); got.Error.Code != "invalid_alert_settings" || got.Error.Field != "channels" {
		t.Fatalf("error = %+v, want a refusal to send when there is no channel", got.Error)
	}
	rec = api.expect(http.MethodPost, "/alerts/test", `{"channel_id":"nope"}`, http.StatusBadRequest)
	if got := decode[apiError](t, rec); got.Error.Field != "channel_id" {
		t.Fatalf("error = %+v, want channel_id", got.Error)
	}
	if len(api.sender.calls) != 0 {
		t.Fatal("nothing may be sent for a refused test")
	}
}

func TestAlertTestIsRateLimited(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	api.expect(http.MethodPut, "/alerts/settings", `{"enabled":true,"channels":[{"kind":"webhook","name":"w","url":"`+webhookURL+`"}]}`, http.StatusOK)
	for i := 0; i < 10; i++ {
		api.expect(http.MethodPost, "/alerts/test", `{}`, http.StatusOK)
	}
	rec := api.expect(http.MethodPost, "/alerts/test", `{}`, http.StatusTooManyRequests)
	if got := decode[apiError](t, rec); got.Error.Code != "too_many_tests" {
		t.Fatalf("error = %+v", got.Error)
	}
	if len(api.sender.calls) != 10 {
		t.Fatalf("the sender was called %d times, want 10: the refused test must not send", len(api.sender.calls))
	}
}

func TestAlertEventsListNewestFirstAndValidateTheLimit(t *testing.T) {
	api := newAlertAPI(t, alertTenantA)
	base := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	var events []alerting.Event
	for i := 0; i < 5; i++ {
		events = append(events, alerting.Event{ID: string(rune('a' + i)), At: base.Add(time.Duration(i) * time.Minute), Rule: alerting.RuleQuotaWindow, Deliveries: []alerting.EventDelivery{}})
	}
	if err := api.store.AppendEvents(alertTenantA, events); err != nil {
		t.Fatal(err)
	}

	all := decode[alerting.EventsResponse](t, api.expect(http.MethodGet, "/alerts/events", "", http.StatusOK))
	if all.Total != 5 || len(all.Items) != 5 || all.Items[0].ID != "e" || all.Items[4].ID != "a" {
		t.Fatalf("events = %+v, want the newest first", all)
	}
	two := decode[alerting.EventsResponse](t, api.expect(http.MethodGet, "/alerts/events?limit=2", "", http.StatusOK))
	if two.Total != 5 || len(two.Items) != 2 || two.Items[0].ID != "e" {
		t.Fatalf("limit=2 gave %+v", two)
	}
	for _, bad := range []string{"limit=0", "limit=-3", "limit=abc"} {
		rec := api.expect(http.MethodGet, "/alerts/events?"+bad, "", http.StatusBadRequest)
		if got := decode[apiError](t, rec); got.Error.Code != "invalid_request" {
			t.Fatalf("%s: error = %+v", bad, got.Error)
		}
	}
	// An empty history is an empty list, never null.
	empty := newAlertAPI(t, alertTenantB)
	rec := empty.expect(http.MethodGet, "/alerts/events", "", http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("body = %s, want \"items\":[]", rec.Body.String())
	}
}

func TestAlertTenantsDoNotSeeEachOthersSettingsOrHistory(t *testing.T) {
	store := alerting.NewMemoryStore()
	sender := &recordingSender{}
	a := newAlertAPIWith(t, alertTenantA, store, sender)
	b := newAlertAPIWith(t, alertTenantB, store, sender)

	a.expect(http.MethodPut, "/alerts/settings", `{"enabled":true,"channels":[{"kind":"webhook","name":"tenant a only","url":"`+webhookURL+`"}]}`, http.StatusOK)
	a.expect(http.MethodPost, "/alerts/test", `{}`, http.StatusOK)

	view := decode[alerting.SettingsView](t, b.expect(http.MethodGet, "/alerts/settings", "", http.StatusOK))
	if view.Enabled || len(view.Channels) != 0 {
		t.Fatalf("tenant B sees tenant A's settings: %+v", view)
	}
	events := decode[alerting.EventsResponse](t, b.expect(http.MethodGet, "/alerts/events", "", http.StatusOK))
	if events.Total != 0 {
		t.Fatalf("tenant B sees tenant A's history: %+v", events)
	}
	// Tenant B has nothing to test with, and cannot reach A's channel by its id.
	idOfA := decode[alerting.SettingsView](t, a.expect(http.MethodGet, "/alerts/settings", "", http.StatusOK)).Channels[0].ID
	b.expect(http.MethodPost, "/alerts/test", `{"channel_id":"`+idOfA+`"}`, http.StatusBadRequest)
}

func TestAlertCapabilitiesAreDeclared(t *testing.T) {
	resp := newConfigResponse(nil, nil)
	if !resp.Capabilities.Alerts || !resp.Capabilities.PoolCapacity {
		t.Fatalf("capabilities = %+v, want alerts and pool-capacity declared so the panel offers them", resp.Capabilities)
	}
	raw, err := json.Marshal(resp.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"alerts":true`, `"pool-capacity":true`, `"payload-rules-dry-run":true`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("capabilities JSON = %s, want %s", raw, key)
		}
	}
}

func TestAlertRoutesStayTenantScopedAndMapToTheirPermissions(t *testing.T) {
	tenantOperator := identity.Principal{EffectiveTenant: identity.Tenant{ID: "tenant-potato"}, HomeTenant: identity.Tenant{ID: "tenant-potato"}}
	for _, path := range []string{
		"/v0/management/alerts/settings", "/v0/management/alerts/test", "/v0/management/alerts/events",
		"/v0/management/ai-accounts/pool-capacity",
	} {
		if !isTenantScopedManagementPath(path) || deniesTenantResourceScope(tenantOperator, path) {
			t.Errorf("a business tenant must reach %s: alerts and pool capacity are tenant-level", path)
		}
	}
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/v0/management/alerts/settings", "tenant_settings.read"},
		{http.MethodPut, "/v0/management/alerts/settings", "tenant_settings.write"},
		{http.MethodPost, "/v0/management/alerts/test", "tenant_settings.write"},
		{http.MethodGet, "/v0/management/alerts/events", "tenant_settings.read"},
		{http.MethodGet, "/v0/management/ai-accounts/pool-capacity", "auth_files.read"},
	} {
		if got := permissionForManagementRequest(c.method, c.path); got != c.want {
			t.Errorf("%s %s needs %q, want %q", c.method, c.path, got, c.want)
		}
	}
}
