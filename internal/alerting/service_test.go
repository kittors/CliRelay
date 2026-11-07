package alerting

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newService(t *testing.T) (*Service, *MemoryStore, *fakeReceiver, *clock) {
	t.Helper()
	store := NewMemoryStore()
	rec := &fakeReceiver{body: `{"code":0,"ok":true}`}
	clk := newClock()
	svc := NewService(store, newDispatcherWith(rec))
	svc.now = clk.Now
	return svc, store, rec, clk
}

func TestServiceViewOfATenantThatNeverSavedIsTheDefaults(t *testing.T) {
	svc, _, _, _ := newService(t)
	view, err := svc.View(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if view.Enabled || len(view.Channels) != 0 || view.Rules != DefaultRules() || view.CooldownMinutes != DefaultCooldownMinutes || !view.Persistent {
		t.Fatalf("view = %+v", view)
	}
	if view.UpdatedAt != nil {
		t.Errorf("a tenant that never saved has no update time: %v", view.UpdatedAt)
	}
}

func TestServiceUpdateIsAPartialWrite(t *testing.T) {
	svc, store, _, clk := newService(t)
	hook := "https://hooks.example.com/services/abc/def/ghijklmnop"
	view, err := svc.Update(tenantA, Patch{
		Enabled:  boolp(true),
		Channels: &[]ChannelPatch{{Kind: KindWebhook, Name: strp("ops"), URL: strp(hook), Secret: strp("signing-key")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || len(view.Channels) != 1 || view.Channels[0].ID == "" || view.Channels[0].URL == hook {
		t.Fatalf("view = %+v", view)
	}
	if view.UpdatedAt == nil || !view.UpdatedAt.Equal(clk.Now()) {
		t.Errorf("updated_at = %v", view.UpdatedAt)
	}

	// Changing one threshold leaves the channel, its secrets and the rest alone.
	clk.Advance(time.Hour)
	if _, err := svc.Update(tenantA, Patch{Rules: &RulesPatch{QuotaWindow: &QuotaWindowPatch{ThresholdPercent: intp(80)}}}); err != nil {
		t.Fatal(err)
	}
	stored, _, _, _ := store.LoadSettings(tenantA)
	if stored.Channels[0].URL != hook || stored.Channels[0].Secret != "signing-key" || stored.Rules.QuotaWindow.ThresholdPercent != 80 || !stored.Enabled {
		t.Fatalf("stored = %+v", stored)
	}

	// An empty write changes nothing.
	before, _, _, _ := store.LoadSettings(tenantA)
	if _, err := svc.Update(tenantA, Patch{}); err != nil {
		t.Fatal(err)
	}
	after, _, _, _ := store.LoadSettings(tenantA)
	after.UpdatedAt = before.UpdatedAt
	if len(after.Channels) != 1 || after.Channels[0] != before.Channels[0] || after.Rules != before.Rules {
		t.Fatalf("an empty write changed the settings: %+v -> %+v", before, after)
	}
}

func TestServiceUpdateRefusesWhatIsInvalidAndStoresNothing(t *testing.T) {
	svc, store, _, _ := newService(t)
	_, err := svc.Update(tenantA, Patch{
		Enabled:  boolp(true),
		Channels: &[]ChannelPatch{{Kind: KindWebhook, Name: strp("x"), URL: strp("https://10.0.0.1/x")}},
	})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "channels[0].url" {
		t.Fatalf("err = %v", err)
	}
	if _, _, found, _ := store.LoadSettings(tenantA); found {
		t.Fatal("a refused write stored something")
	}
}

func TestServiceOperatorMayUseThePrivateNetworkButATenantMayNot(t *testing.T) {
	svc, _, _, _ := newService(t)
	patch := Patch{Channels: &[]ChannelPatch{{Kind: KindWebhook, Name: strp("relay"), URL: strp("http://127.0.0.1:9000/hook")}}}
	if _, err := svc.Update(systemTenantID, patch); err != nil {
		t.Fatalf("the operator: %v", err)
	}
	if _, err := svc.Update(tenantA, patch); err == nil {
		t.Fatal("a tenant was allowed to aim a webhook at the local network")
	}
}

func TestServiceUpdateWithoutAStoreIsRefused(t *testing.T) {
	store := NewMemoryStore()
	store.Persist = false
	svc := NewService(store, newDispatcherWith(&fakeReceiver{}))
	if _, err := svc.Update(tenantA, Patch{Enabled: boolp(true)}); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("err = %v", err)
	}
	view, err := svc.View(tenantA)
	if err != nil || view.Persistent {
		t.Fatalf("view = %+v, err = %v", view, err)
	}
}

type conflictingSettings struct {
	*MemoryStore
	conflicts atomic.Int32
	// intervene runs before the first conflict, as another administrator's write.
	intervene func()
}

func (c *conflictingSettings) SaveSettings(tenantID string, s Settings, expected int64) error {
	if c.conflicts.Add(-1) >= 0 {
		if c.intervene != nil {
			c.intervene()
			c.intervene = nil
		}
		return ErrConflict
	}
	return c.MemoryStore.SaveSettings(tenantID, s, expected)
}

func TestServiceUpdateReappliesItsPatchAfterLosingARace(t *testing.T) {
	mem := NewMemoryStore()
	store := &conflictingSettings{MemoryStore: mem}
	store.conflicts.Store(1)
	svc := NewService(store, newDispatcherWith(&fakeReceiver{}))
	svc.now = newClock().Now
	// Another administrator changes the cooldown while this write is in flight.
	store.intervene = func() {
		other := DefaultSettings()
		other.CooldownMinutes = 720
		_ = mem.SaveSettings(tenantA, other, AnyVersion)
	}
	if _, err := svc.Update(tenantA, Patch{Rules: &RulesPatch{Spending: &SpendingPatch{ThresholdPercent: intp(70)}}}); err != nil {
		t.Fatal(err)
	}
	got, _, _, _ := mem.LoadSettings(tenantA)
	if got.Rules.Spending.ThresholdPercent != 70 || got.CooldownMinutes != 720 {
		t.Fatalf("one of the two edits was lost: %+v", got)
	}

	store.conflicts.Store(100)
	if _, err := svc.Update(tenantA, Patch{Enabled: boolp(true)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write that keeps losing must say so: %v", err)
	}
}

func saveChannels(t *testing.T, store *MemoryStore, tenantID string, channels ...Channel) {
	t.Helper()
	s := DefaultSettings()
	s.Enabled = true
	s.Channels = channels
	if err := store.SaveSettings(tenantID, s, AnyVersion); err != nil {
		t.Fatal(err)
	}
}

func TestServiceTestSendsToEveryEnabledChannel(t *testing.T) {
	svc, store, rec, _ := newService(t)
	saveChannels(t, store, tenantA,
		Channel{ID: "w", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL},
		Channel{ID: "f", Kind: KindFeishu, Name: "team", Enabled: true, URL: testFeishuURL},
		Channel{ID: "off", Kind: KindWebhook, Name: "disabled", Enabled: false, URL: testWebhookURL},
	)
	got, err := svc.Test(context.Background(), tenantA, TestRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || !got.Results[0].OK || !got.Results[1].OK || got.Results[0].Status != DeliverySent {
		t.Fatalf("results = %+v", got.Results)
	}
	if len(rec.all()) != 2 {
		t.Fatalf("receivers called %d times", len(rec.all()))
	}
	events, _ := store.LoadEvents(tenantA)
	if len(events) != 1 || events[0].Rule != RuleTest || len(events[0].Deliveries) != 2 {
		t.Fatalf("a test belongs in the history: %+v", events)
	}
	// Channels are delivered to at the same time, so the order they arrive in is not fixed.
	var sawWebhookPayload bool
	for _, req := range rec.all() {
		sawWebhookPayload = sawWebhookPayload || strings.Contains(string(req.Body), `"rule":"test"`)
	}
	if !sawWebhookPayload {
		t.Errorf("no webhook payload carried the test rule: %v", rec.all())
	}
}

func TestServiceTestReportsAFailureWithoutLeakingTheAddress(t *testing.T) {
	svc, store, rec, _ := newService(t)
	rec.err = errors.New(`Post "` + testWebhookURL + `": dial tcp: lookup hooks.example.com: no such host`)
	saveChannels(t, store, tenantA, Channel{ID: "w", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL, Secret: "k"})
	got, err := svc.Test(context.Background(), tenantA, TestRequest{ChannelID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	r := got.Results[0]
	if r.OK || r.Status != DeliveryFailed || r.Error == "" || strings.Contains(r.Error, "hooks.example.com") || strings.Contains(r.Error, "XXXX") {
		t.Fatalf("result = %+v", r)
	}
}

func TestServiceTestOfASavedChannelByID(t *testing.T) {
	svc, store, rec, _ := newService(t)
	saveChannels(t, store, tenantA,
		Channel{ID: "w", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL},
		Channel{ID: "f", Kind: KindFeishu, Name: "team", Enabled: true, URL: testFeishuURL})
	got, err := svc.Test(context.Background(), tenantA, TestRequest{ChannelID: "f"})
	if err != nil || len(got.Results) != 1 || got.Results[0].ChannelID != "f" || len(rec.all()) != 1 {
		t.Fatalf("results = %+v, err = %v", got, err)
	}
	var fieldErr *FieldError
	if _, err := svc.Test(context.Background(), tenantA, TestRequest{ChannelID: "nope"}); !errors.As(err, &fieldErr) || fieldErr.Field != "channel_id" {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceTestOfADraftResolvesEchoedMasks(t *testing.T) {
	svc, store, rec, _ := newService(t)
	saveChannels(t, store, tenantA, Channel{ID: "f", Kind: KindFeishu, Name: "team", Enabled: true, URL: testFeishuURL, Secret: testSigningKey})
	view, _ := svc.View(tenantA)
	ch := view.Channels[0]
	got, err := svc.Test(context.Background(), tenantA, TestRequest{Channel: &ChannelPatch{
		ID: ch.ID, Kind: ch.Kind, Name: strp("renamed"), URL: strp(ch.URL), Secret: strp(ch.Secret),
	}})
	if err != nil || len(got.Results) != 1 || !got.Results[0].OK || got.Results[0].ChannelName != "renamed" {
		t.Fatalf("results = %+v, err = %v", got, err)
	}
	if req := rec.all()[0]; req.URL != testFeishuURL || !strings.Contains(string(req.Body), `"sign"`) {
		t.Fatalf("the draft was sent without the saved address or secret: %s %s", req.URL, req.Body)
	}

	// A draft with no id cannot refer to a mask.
	var fieldErr *FieldError
	_, err = svc.Test(context.Background(), tenantA, TestRequest{Channel: &ChannelPatch{Kind: KindFeishu, URL: strp(ch.URL)}})
	if !errors.As(err, &fieldErr) {
		t.Fatalf("err = %v", err)
	}
	// A new draft is validated like a saved one.
	_, err = svc.Test(context.Background(), tenantA, TestRequest{Channel: &ChannelPatch{Kind: KindWebhook, URL: strp("https://10.0.0.1/x")}})
	if !errors.As(err, &fieldErr) || fieldErr.Field != "channel.url" {
		t.Fatalf("err = %v", err)
	}
	draft, err := svc.Test(context.Background(), tenantA, TestRequest{Channel: &ChannelPatch{Kind: KindWebhook, URL: strp("https://hooks.example.com/new")}})
	if err != nil || len(draft.Results) != 1 || draft.Results[0].ChannelID == "" {
		t.Fatalf("draft = %+v, err = %v", draft, err)
	}
}

func TestServiceTestWithNothingToSendToIsAnError(t *testing.T) {
	svc, store, _, _ := newService(t)
	var fieldErr *FieldError
	if _, err := svc.Test(context.Background(), tenantA, TestRequest{}); !errors.As(err, &fieldErr) || fieldErr.Field != "channels" {
		t.Fatalf("err = %v", err)
	}
	saveChannels(t, store, tenantA, Channel{ID: "off", Kind: KindWebhook, Name: "x", Enabled: false, URL: testWebhookURL})
	if _, err := svc.Test(context.Background(), tenantA, TestRequest{}); !errors.As(err, &fieldErr) {
		t.Fatalf("a disabled channel is not something to send to: %v", err)
	}
}

func TestServiceTestIsRateLimited(t *testing.T) {
	svc, store, _, clk := newService(t)
	saveChannels(t, store, tenantA, Channel{ID: "w", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL})
	for i := 0; i < testMax; i++ {
		if _, err := svc.Test(context.Background(), tenantA, TestRequest{}); err != nil {
			t.Fatalf("test %d: %v", i, err)
		}
	}
	if _, err := svc.Test(context.Background(), tenantA, TestRequest{}); !errors.Is(err, ErrTooManyTests) {
		t.Fatalf("err = %v, want ErrTooManyTests", err)
	}
	// Another tenant has its own allowance, and time restores this one's.
	saveChannels(t, store, tenantB, Channel{ID: "w", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL})
	if _, err := svc.Test(context.Background(), tenantB, TestRequest{}); err != nil {
		t.Fatalf("another tenant: %v", err)
	}
	clk.Advance(testWindow + time.Second)
	if _, err := svc.Test(context.Background(), tenantA, TestRequest{}); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestServiceTestMessageIsInTheTenantsLanguage(t *testing.T) {
	svc, store, rec, _ := newService(t)
	s := DefaultSettings()
	s.Enabled, s.Language = true, LanguageZHCN
	s.Channels = []Channel{{ID: "w", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL}}
	_ = store.SaveSettings(tenantA, s, AnyVersion)
	if _, err := svc.Test(context.Background(), tenantA, TestRequest{}); err != nil {
		t.Fatal(err)
	}
	if body := string(rec.all()[0].Body); !strings.Contains(body, "测试消息") {
		t.Errorf("payload = %s", body)
	}
}

func TestServiceEvents(t *testing.T) {
	svc, store, _, _ := newService(t)
	resp, err := svc.Events(tenantA, 0)
	if err != nil || resp.Items == nil || len(resp.Items) != 0 || resp.Total != 0 {
		t.Fatalf("empty history: %+v, err = %v", resp, err)
	}
	var events []Event
	for i := 0; i < 70; i++ {
		events = append(events, Event{ID: strings.Repeat("e", i+1), At: timeNow().Add(time.Duration(i) * time.Minute)})
	}
	_ = store.AppendEvents(tenantA, events)
	resp, _ = svc.Events(tenantA, 0)
	if len(resp.Items) != 50 || resp.Total != 70 || resp.Items[0].ID != strings.Repeat("e", 70) {
		t.Fatalf("default page: %d items of %d, first %q", len(resp.Items), resp.Total, resp.Items[0].ID)
	}
	resp, _ = svc.Events(tenantA, 5)
	if len(resp.Items) != 5 {
		t.Fatalf("limit 5: %d items", len(resp.Items))
	}
	resp, _ = svc.Events(tenantA, 100000)
	if len(resp.Items) != 70 {
		t.Fatalf("a huge limit is capped to what exists: %d", len(resp.Items))
	}
	// Tenants never see each other's history.
	if other, _ := svc.Events(tenantB, 0); len(other.Items) != 0 {
		t.Fatal("another tenant's history leaked")
	}
}

var _ http.RoundTripper = (*fakeReceiver)(nil)
