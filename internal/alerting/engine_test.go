package alerting

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

type harness struct {
	t       *testing.T
	store   *MemoryStore
	sources *fakeSources
	sender  *fakeSender
	clock   *clock
	engine  *Engine
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: NewMemoryStore(), sources: &fakeSources{}, sender: &fakeSender{}, clock: newClock()}
	h.engine = NewEngine(EngineOptions{Store: h.store, Sources: h.sources, Sender: h.sender, Now: h.clock.Now})
	return h
}

// enable saves settings that switch a tenant's alerting on with one channel.
func (h *harness) enable(tenantID string, mutate ...func(*Settings)) {
	h.t.Helper()
	s := DefaultSettings()
	s.Enabled = true
	s.Channels = []Channel{{ID: "ch1", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL}}
	// Only the rule a test is about is on, so rounds stay legible.
	s.Rules = Rules{
		QuotaWindow:    QuotaWindowRule{ThresholdPercent: DefaultUsageThresholdPercent},
		AccountFailure: AccountFailureRule{ForMinutes: DefaultFailureMinutes},
		Spending:       SpendingRule{ThresholdPercent: DefaultUsageThresholdPercent},
		UseItOrLoseIt:  UseItOrLoseItRule{MinRemainingPercent: DefaultMinRemainingPercent, HoursBeforeReset: DefaultHoursBeforeReset},
	}
	for _, m := range mutate {
		m(&s)
	}
	if err := h.store.SaveSettings(tenantID, s, AnyVersion); err != nil {
		h.t.Fatal(err)
	}
}

func withQuota(s *Settings)   { s.Rules.QuotaWindow.Enabled = true }
func withFailure(s *Settings) { s.Rules.AccountFailure.Enabled = true }
func withSpend(s *Settings)   { s.Rules.Spending.Enabled = true }
func withUseIt(s *Settings)   { s.Rules.UseItOrLoseIt.Enabled = true }

func (h *harness) round() []Message {
	h.t.Helper()
	before := len(h.sender.messages())
	h.engine.RunOnce(context.Background())
	return h.sender.messages()[before:]
}

func weekly(resetIn time.Duration, used float64, now time.Time) WindowSample {
	return WindowSample{Key: "code_week", Label: "Weekly", Span: 7 * day, UsedPercent: used, ResetAt: now.Add(resetIn), ObservedAt: now}
}

func TestEngineSkipsTenantsThatAreNotActive(t *testing.T) {
	h := newHarness(t)
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 99, h.clock.Now())))

	// Master switch off.
	h.enable(tenantA, withQuota, func(s *Settings) { s.Enabled = false })
	// Switched on but nowhere to send.
	h.enable(tenantB, withQuota, func(s *Settings) { s.Channels = nil })
	// Everything on, but every rule off.
	h.enable("33333333-3333-3333-3333-333333333333")

	if got := h.round(); len(got) != 0 {
		t.Fatalf("messages = %d, want 0", len(got))
	}
	if accounts, spenders := h.sources.calls(); accounts != 0 || spenders != 0 {
		t.Fatalf("an inactive tenant cost %d account reads and %d spending reads", accounts, spenders)
	}
}

func TestEngineReportsAQuotaWindowOncePerCycle(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, now)))

	got := h.round()
	if len(got) != 1 {
		t.Fatalf("first round: %d messages, want 1", len(got))
	}
	msg := got[0]
	if msg.Rule != RuleQuotaWindow || msg.Severity != SeverityWarning || msg.TenantID != tenantA || msg.Language != LanguageEN {
		t.Errorf("message = %+v", msg)
	}
	if !strings.Contains(msg.Text, "95% used") || len(msg.Items) != 1 || msg.Items[0].Facts["used_percent"] != 95.0 {
		t.Errorf("message = %+v", msg)
	}

	// The alert is recorded with where it went.
	events, _ := h.store.LoadEvents(tenantA)
	if len(events) != 1 || events[0].ID != msg.ID || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].Status != DeliverySent {
		t.Fatalf("events = %+v", events)
	}

	// Rounds and cooldowns pass; the window has not renewed, so nothing more is said,
	// even though the reading's reset time wobbles from probe to probe.
	for i := 0; i < 5; i++ {
		h.clock.Advance(8 * time.Hour)
		now = h.clock.Now()
		h.sources.setAccounts(tenantA, account("a", WindowSample{
			Key: "code_week", Span: 7 * day, UsedPercent: 96, ResetAt: now.Add(2*day - 8*time.Hour*time.Duration(i+1) + time.Duration(i)*time.Second), ObservedAt: now,
		}))
		if i < 4 {
			if got := h.round(); len(got) != 0 {
				t.Fatalf("round %d inside the cycle sent %d message(s)", i+2, len(got))
			}
		}
	}

	// The window renews and fills up again: a new cycle, a new alert.
	h.clock.Advance(3 * day)
	now = h.clock.Now()
	h.sources.setAccounts(tenantA, account("a", weekly(5*day, 92, now)))
	if got := h.round(); len(got) != 1 {
		t.Fatalf("a new cycle sent %d message(s), want 1", len(got))
	}
}

func TestEngineWindowWithoutAResetWaitsOutTheCooldown(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota, func(s *Settings) { s.CooldownMinutes = 360 })
	bucket := WindowSample{Key: "model:x", UsedPercent: 97, ObservedAt: h.clock.Now()}
	h.sources.setAccounts(tenantA, account("a", bucket))

	if got := h.round(); len(got) != 1 {
		t.Fatalf("first round: %d messages", len(got))
	}
	h.clock.Advance(359 * time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("inside the cooldown: %d messages", len(got))
	}
	h.clock.Advance(2 * time.Minute)
	if got := h.round(); len(got) != 1 {
		t.Fatalf("after the cooldown: %d messages, want the reminder", len(got))
	}
}

func TestEngineGroupsManyAlertsIntoOneSummary(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	var accounts []AccountSample
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		accounts = append(accounts, account(id, weekly(2*day, 95, now)))
	}
	h.sources.setAccounts(tenantA, accounts...)

	got := h.round()
	if len(got) != 1 {
		t.Fatalf("messages = %d, want one summary", len(got))
	}
	msg := got[0]
	if len(msg.Items) != 5 || !strings.Contains(msg.Title, "(5)") || msg.Subject.Name != "5 accounts" || msg.Subject.ID != "" {
		t.Errorf("summary = %+v", msg)
	}
	if lines := strings.Split(msg.Text, "\n"); len(lines) != 5 || !strings.HasPrefix(lines[0], "• ") {
		t.Errorf("text = %q", msg.Text)
	}
	// Every one of them is accounted for: nothing is repeated next round.
	h.clock.Advance(time.Hour)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("the next round repeated %d message(s)", len(got))
	}
}

func TestEngineSummaryListsTenAndCountsTheRest(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	var accounts []AccountSample
	for i := 0; i < 14; i++ {
		accounts = append(accounts, account(string(rune('a'+i)), weekly(2*day, 95, now)))
	}
	h.sources.setAccounts(tenantA, accounts...)
	msg := h.round()[0]
	lines := strings.Split(msg.Text, "\n")
	if len(msg.Items) != 14 || len(lines) != summaryLines+1 || !strings.Contains(lines[len(lines)-1], "and 4 more") {
		t.Fatalf("items = %d, text = %q", len(msg.Items), msg.Text)
	}
}

func TestEngineSendsAFewAlertsSeparately(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	h.sources.setAccounts(tenantA,
		account("a", weekly(2*day, 95, now)), account("b", weekly(2*day, 95, now)), account("c", weekly(2*day, 100, now)))
	got := h.round()
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 3", len(got))
	}
	if got[0].Severity != SeverityCritical {
		t.Errorf("the worst goes first: %+v", got[0])
	}
}

func TestEngineSpacesTheMessagesOfOneRule(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, now)))
	if got := h.round(); len(got) != 1 {
		t.Fatalf("first round: %d messages", len(got))
	}

	// Another account crosses a minute later: it waits for the gap instead of
	// buzzing the channel again.
	h.clock.Advance(time.Minute)
	now = h.clock.Now()
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, now)), account("b", weekly(2*day, 95, now)))
	if got := h.round(); len(got) != 0 {
		t.Fatalf("inside the gap: %d messages", len(got))
	}
	h.clock.Advance(minMessageGap)
	got := h.round()
	if len(got) != 1 || got[0].Items[0].Subject.ID != "subject-b" {
		t.Fatalf("after the gap: %+v", got)
	}
}

func TestEngineFailureLifecycle(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withFailure)

	// An account that starts failing with a hiccup-class error is not reported yet.
	h.sources.setAccounts(tenantA, failing("flaky", FailureNeedsAction, "unauthorized"))
	if got := h.round(); len(got) != 0 {
		t.Fatalf("first sight: %d messages", len(got))
	}
	h.clock.Advance(9 * time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("after 9 minutes: %d messages", len(got))
	}
	h.clock.Advance(time.Minute)
	got := h.round()
	if len(got) != 1 || got[0].Rule != RuleAccountFailure || !strings.Contains(got[0].Text, "HTTP 401") || !strings.Contains(got[0].Text, "failing for 10m") {
		t.Fatalf("after 10 minutes: %+v", got)
	}

	// Still failing an hour later: inside the cooldown, so quiet.
	h.clock.Advance(time.Hour)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("inside the cooldown: %d messages", len(got))
	}

	// It recovers, then fails with a revoked credential: reported at once, not after
	// waiting out the cooldown of the last episode.
	h.sources.setAccounts(tenantA, account("flaky"))
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("a healthy account: %d messages", len(got))
	}
	h.sources.setAccounts(tenantA, failing("flaky", FailureRevoked, "reauth_required"))
	h.clock.Advance(time.Minute)
	got = h.round()
	if len(got) != 1 || got[0].Severity != SeverityCritical {
		t.Fatalf("a revoked credential after recovery: %+v", got)
	}
}

func TestEngineDoesNotForgetFailuresWhenTheAccountsCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withFailure, func(s *Settings) { s.Rules.AccountFailure.ForMinutes = 0 })
	h.sources.setAccounts(tenantA, failing("a", FailureRevoked, "reauth_required"))
	if got := h.round(); len(got) != 1 {
		t.Fatalf("first round: %d", len(got))
	}
	// A round that fails to read must not be taken for "everything recovered".
	h.sources.accountErr = context.DeadlineExceeded
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("an unreadable round sent %d message(s)", len(got))
	}
	h.sources.accountErr = nil
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("the record of a failure that never recovered was lost: %d message(s)", len(got))
	}
}

func TestEngineRetriesAFailedDeliveryLater(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))
	h.sender.fail = true

	if got := h.round(); len(got) != 1 {
		t.Fatalf("first round: %d messages", len(got))
	}
	events, _ := h.store.LoadEvents(tenantA)
	if len(events) != 1 || events[0].Deliveries[0].Status != DeliveryFailed || events[0].Deliveries[0].Error == "" {
		t.Fatalf("the failure must be visible in the history: %+v", events)
	}
	state, _, _ := h.store.LoadSent(tenantA)
	if rec := state.Records["quota_window|subject-a|code_week"]; !rec.Failed || rec.Pending {
		t.Fatalf("record = %+v", rec)
	}

	// Not retried at once (a dead channel must not be hammered)...
	h.clock.Advance(failedRetryAfter - time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("retried too early: %d messages", len(got))
	}
	// ...but retried after the backoff, and then recorded as sent.
	h.clock.Advance(2 * time.Minute)
	h.sender.fail = false
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))
	if got := h.round(); len(got) != 1 {
		t.Fatalf("retry: %d messages", len(got))
	}
	h.clock.Advance(time.Hour)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("after a successful retry: %d messages", len(got))
	}
}

func TestEnginePartialDeliveryCountsAsDelivered(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota, func(s *Settings) {
		s.Channels = append(s.Channels, Channel{ID: "ch2", Kind: KindFeishu, Name: "team", Enabled: true, URL: testFeishuURL})
	})
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))
	h.engine.sender = senderFunc(func(_ context.Context, channels []Channel, _ Message, _ bool) []Delivery {
		return []Delivery{
			{ChannelID: channels[0].ID, Kind: channels[0].Kind, Status: DeliverySent},
			{ChannelID: channels[1].ID, Kind: channels[1].Kind, Status: DeliveryFailed, Error: "the request failed"},
		}
	})
	h.engine.RunOnce(context.Background())
	state, _, _ := h.store.LoadSent(tenantA)
	if rec := state.Records["quota_window|subject-a|code_week"]; rec.Failed || rec.Pending {
		t.Fatalf("one channel accepted it, so it is delivered: %+v", rec)
	}
}

type senderFunc func(context.Context, []Channel, Message, bool) []Delivery

func (f senderFunc) Send(ctx context.Context, channels []Channel, msg Message, trusted bool) []Delivery {
	return f(ctx, channels, msg, trusted)
}

// While a message is being delivered, its problem is already reserved, so a
// second process evaluating at the same moment (the other backend slot during a
// deploy) stays silent.
func TestEngineReservesBeforeDelivering(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))

	other := NewEngine(EngineOptions{Store: h.store, Sources: h.sources, Sender: h.sender, Now: h.clock.Now})
	var duringDelivery int
	var reserved SentRecord
	h.sender.onSend = func(Message) {
		state, _, _ := h.store.LoadSent(tenantA)
		reserved = state.Records["quota_window|subject-a|code_week"]
		before := len(h.sender.messages())
		other.RunOnce(context.Background())
		duringDelivery = len(h.sender.messages()) - before
	}
	if got := h.round(); len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	if !reserved.Pending || !reserved.Until.Equal(h.clock.Now().Add(claimLease)) {
		t.Fatalf("the alert was not reserved before it was delivered: %+v", reserved)
	}
	if duringDelivery != 0 {
		t.Fatalf("a second process sent %d duplicate message(s) while the first was delivering", duringDelivery)
	}
	// Once delivered, the reservation is replaced by the real quiet period.
	state, _, _ := h.store.LoadSent(tenantA)
	if rec := state.Records["quota_window|subject-a|code_week"]; rec.Pending || rec.Failed || !rec.Until.After(h.clock.Now().Add(claimLease)) {
		t.Fatalf("record after delivery = %+v", rec)
	}
}

func TestEngineReservationExpiresIfTheProcessDies(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, now)))
	// A process claimed the alert and died before delivering.
	state := SentState{}
	state.set("quota_window|subject-a|code_week", SentRecord{At: now, Until: now.Add(claimLease), Pending: true})
	if err := h.store.SaveSent(tenantA, state, AnyVersion); err != nil {
		t.Fatal(err)
	}
	if got := h.round(); len(got) != 0 {
		t.Fatalf("inside the lease: %d messages", len(got))
	}
	h.clock.Advance(claimLease + time.Minute)
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))
	if got := h.round(); len(got) != 1 {
		t.Fatalf("after the lease: %d messages, want the alert the dead process never sent", len(got))
	}
}

type conflictOnce struct {
	*MemoryStore
	remaining atomic.Int32
}

func (c *conflictOnce) SaveSent(tenantID string, s SentState, expected int64) error {
	if c.remaining.Add(-1) >= 0 {
		return ErrConflict
	}
	return c.MemoryStore.SaveSent(tenantID, s, expected)
}

func TestEngineYieldsWhenAnotherProcessReservedFirst(t *testing.T) {
	h := newHarness(t)
	store := &conflictOnce{MemoryStore: h.store}
	store.remaining.Store(1)
	h.engine = NewEngine(EngineOptions{Store: store, Sources: h.sources, Sender: h.sender, Now: h.clock.Now})
	h.enable(tenantA, withQuota)
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))

	if got := h.round(); len(got) != 0 {
		t.Fatalf("a lost reservation still sent %d message(s)", len(got))
	}
	if got := h.round(); len(got) != 1 {
		t.Fatalf("the next round sent %d message(s), want 1", len(got))
	}
}

func TestEngineOneTenantsPanicDoesNotStopTheOthers(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	h.enable(tenantB, withQuota)
	h.sources.panicFor = tenantA
	h.sources.setAccounts(tenantB, account("b", weekly(2*day, 95, h.clock.Now())))
	got := h.round()
	if len(got) != 1 || got[0].TenantID != tenantB {
		t.Fatalf("messages = %+v", got)
	}
}

func TestEngineReadsSpendingEveryFiveMinutes(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withSpend)
	h.sources.spenders = map[string][]SpendSample{tenantA: {{Kind: SubjectKey, ID: "k", Name: "k", Cells: []SpendCell{
		{Metric: "cost", Window: "day", Used: 95, Limit: 100, ResetAt: h.clock.Now().Add(4 * hour)},
	}}}}
	if got := h.round(); len(got) != 1 {
		t.Fatalf("first round: %d messages", len(got))
	}
	for i := 0; i < 4; i++ {
		h.clock.Advance(time.Minute)
		h.round()
	}
	if _, spenders := h.sources.calls(); spenders != 1 {
		t.Fatalf("spending was read %d times in 4 minutes, want 1", spenders)
	}
	h.clock.Advance(time.Minute)
	h.round()
	if _, spenders := h.sources.calls(); spenders != 2 {
		t.Fatalf("spending was read %d times after 5 minutes, want 2", spenders)
	}
	if accounts, _ := h.sources.calls(); accounts != 0 {
		t.Errorf("a tenant with only the spending rule on cost %d account reads", accounts)
	}
}

func TestEngineWritesInTheTenantsLanguage(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota, func(s *Settings) { s.Language = LanguageZHCN })
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))
	msg := h.round()[0]
	if msg.Language != LanguageZHCN || msg.Title != "账号额度即将用完" || !strings.Contains(msg.Text, "已用 95%") {
		t.Fatalf("message = %+v", msg)
	}
}

func TestEngineTellsTheSenderWhoseChannelsThey_Are(t *testing.T) {
	h := newHarness(t)
	h.enable(systemTenantID, withQuota)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	h.sources.setAccounts(systemTenantID, account("s", weekly(2*day, 95, now)))
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, now)))
	h.round()
	trusted := map[string]bool{}
	for i, msg := range h.sender.messages() {
		trusted[msg.TenantID] = h.sender.trusted[i]
	}
	if !trusted[systemTenantID] || trusted[tenantA] {
		t.Fatalf("trusted = %v: only the platform operator's tenant may use the local network", trusted)
	}
}

func TestEngineNeverSendsMoreThanTheItemCap(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	now := h.clock.Now()
	var accounts []AccountSample
	for i := 0; i < maxItems+10; i++ {
		accounts = append(accounts, account(strings.Repeat("x", i+1), weekly(2*day, 95, now)))
	}
	h.sources.setAccounts(tenantA, accounts...)
	got := h.round()
	if len(got) != 1 || len(got[0].Items) != maxItems {
		t.Fatalf("messages = %d, items = %d", len(got), len(got[0].Items))
	}
	// The ten that did not fit are not marked as sent: they go in the next message.
	h.clock.Advance(minMessageGap + time.Minute)
	now = h.clock.Now()
	for i := range accounts {
		accounts[i].Windows = []WindowSample{weekly(2*day, 95, now)}
	}
	h.sources.setAccounts(tenantA, accounts...)
	got = h.round()
	if len(got) != 1 || len(got[0].Items) != 10 {
		t.Fatalf("the remainder: %d messages, %d items", len(got), len(got[0].Items))
	}
}

func TestEngineLoopRunsOnlyOnTheLeader(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withQuota)
	h.sources.setAccounts(tenantA, account("a", weekly(2*day, 95, h.clock.Now())))
	var leader atomic.Bool
	e := NewEngine(EngineOptions{
		Store: h.store, Sources: h.sources, Sender: h.sender, Now: h.clock.Now,
		IsLeader: leader.Load, Interval: 10 * time.Millisecond,
	})
	e.Start()
	defer e.Stop()
	time.Sleep(80 * time.Millisecond)
	if accounts, _ := h.sources.calls(); accounts != 0 {
		t.Fatalf("a follower evaluated %d time(s)", accounts)
	}
	leader.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for len(h.sender.messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.sender.messages()) != 1 {
		t.Fatalf("messages = %d, want 1 once this node led", len(h.sender.messages()))
	}
	e.Stop()
	time.Sleep(30 * time.Millisecond)
	calls, _ := h.sources.calls()
	time.Sleep(60 * time.Millisecond)
	if after, _ := h.sources.calls(); after != calls {
		t.Fatalf("the loop kept running after Stop (%d -> %d)", calls, after)
	}
}

func TestEngineStartIgnoresAnIncompleteSetup(t *testing.T) {
	NewEngine(EngineOptions{Interval: time.Millisecond}).Start()
	var e *Engine
	e.Start()
	e.Stop()
	e.RunOnce(context.Background())
}

func TestEngineHistoryIsBounded(t *testing.T) {
	store := NewMemoryStore()
	var events []Event
	for i := 0; i < MaxEvents+25; i++ {
		events = append(events, Event{ID: strings.Repeat("e", i+1), At: timeNow().Add(time.Duration(i) * time.Second)})
	}
	if err := store.AppendEvents(tenantA, events); err != nil {
		t.Fatal(err)
	}
	// A retried write must not double an entry.
	if err := store.AppendEvents(tenantA, events[:5]); err != nil {
		t.Fatal(err)
	}
	got, _ := store.LoadEvents(tenantA)
	if len(got) != MaxEvents {
		t.Fatalf("history = %d, want %d", len(got), MaxEvents)
	}
	if got[0].At.Before(got[len(got)-1].At) {
		t.Error("the history is newest first")
	}
}

func TestSentStatePruneBoundsTheTable(t *testing.T) {
	now := timeNow()
	state := SentState{}
	for i := 0; i < maxSentKeys+50; i++ {
		state.set(strings.Repeat("k", i+1), SentRecord{At: now.Add(time.Duration(i) * time.Second), Until: now.Add(day)})
	}
	state.set("expired", SentRecord{At: now.Add(-day), Until: now.Add(-time.Hour)})
	state.prune(now)
	if len(state.Records) != maxSentKeys {
		t.Fatalf("records = %d, want %d", len(state.Records), maxSentKeys)
	}
	if _, ok := state.Records["expired"]; ok {
		t.Error("an expired record was kept")
	}
	// The oldest are the ones forgotten.
	if _, ok := state.Records[strings.Repeat("k", 1)]; ok {
		t.Error("the oldest record survived while newer ones were dropped")
	}
}
