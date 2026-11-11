package alerting

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// brokenProxy is a pool entry whose breaker opened at since; probes failed
// after it.
func brokenProxy(id string, since time.Time, probes int) EgressSample {
	return EgressSample{
		Kind: EgressPoolEntry, ID: id, Name: "SG " + id, Endpoint: "socks5://203.0.113.7:443", Fingerprint: "fp-" + id,
		Accounts: 7, Open: true, Fault: EgressFaultAuthRejected, Since: since, NextProbe: since.Add(4 * time.Minute),
		Refused: 120, FailedProbes: probes,
	}
}

// workingProxy is the same entry with the breaker closed, a request having got
// through it at lastPass (zero: none has in this process).
func workingProxy(id string, lastPass time.Time) EgressSample {
	s := brokenProxy(id, time.Time{}, 0)
	s.Open, s.Fault, s.NextProbe, s.Refused = false, "", time.Time{}, 0
	s.LastPass = lastPass
	return s
}

func TestEgressRuleWaitsForAFailedProbeAndTheMinutes(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	opened := h.clock.Now()

	// Just opened: two rejections a moment ago.
	h.sources.setEgress(tenantA, brokenProxy("sg-1", opened, 0))
	if got := h.round(); len(got) != 0 {
		t.Fatalf("a breaker that just opened sent %d message(s)", len(got))
	}
	// Five minutes on, but no request came to probe it: not confirmed.
	h.clock.Advance(5 * time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("an outage no probe confirmed sent %d message(s)", len(got))
	}
	// A probe failed, but the outage is younger than the threshold.
	h.sources.setEgress(tenantA, brokenProxy("sg-1", h.clock.Now().Add(-2*time.Minute), 2))
	if got := h.round(); len(got) != 0 {
		t.Fatalf("an outage of 2 minutes sent %d message(s), want none before 3", len(got))
	}

	confirmed := brokenProxy("sg-1", opened, 3)
	confirmed.NextProbe = h.clock.Now().Add(90 * time.Second)
	h.sources.setEgress(tenantA, confirmed)
	got := h.round()
	if len(got) != 1 {
		t.Fatalf("a confirmed outage of 5 minutes: %d message(s), want 1", len(got))
	}
	msg := got[0]
	if msg.Rule != RuleEgressBreaker || msg.Severity != SeverityCritical || msg.Title != "Egress proxy is not working" {
		t.Fatalf("message = %+v", msg)
	}
	if msg.Subject != (Subject{Kind: SubjectProxy, ID: "sg-1", Name: "SG sg-1"}) {
		t.Fatalf("subject = %+v", msg.Subject)
	}
	for _, want := range []string{`Proxy "SG sg-1" (socks5://203.0.113.7:443)`, "rejects its credentials", "refused for 5m", "7 of this tenant's accounts", "120 attempts refused", "3 probes failed", "next probe at 2026-11-02 12:06 UTC"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("text %q lacks %q", msg.Text, want)
		}
	}
	facts := msg.Items[0].Facts
	if facts["fault"] != EgressFaultAuthRejected || facts["proxy_id"] != "sg-1" || facts["open_minutes"] != 5 || facts["accounts"] != 7 || facts["failed_probes"] != 3 {
		t.Fatalf("facts = %+v", facts)
	}
	if _, leaked := facts["fingerprint"]; leaked {
		t.Fatal("the fingerprint of the address reached the payload")
	}

	// Still down an hour later: inside the cooldown.
	h.clock.Advance(time.Hour)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("inside the cooldown: %d message(s)", len(got))
	}
	// Past the cooldown it is said again.
	h.clock.Advance(time.Duration(DefaultCooldownMinutes) * time.Minute)
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressBreaker {
		t.Fatalf("past the cooldown: %+v", got)
	}
}

func TestEgressRuleIgnoresABlip(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	opened := h.clock.Now()
	h.sources.setEgress(tenantA, brokenProxy("sg-1", opened, 1))
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("minute 1: %d message(s)", len(got))
	}
	// The probe 45 seconds in got through.
	h.sources.setEgress(tenantA, workingProxy("sg-1", opened.Add(45*time.Second)))
	for range 5 {
		h.clock.Advance(time.Minute)
		if got := h.round(); len(got) != 0 {
			t.Fatalf("a blip that was over in under a minute sent %+v", got)
		}
	}
}

// reportOutage gets an outage of sg-1 reported and returns when it began.
func reportOutage(t *testing.T, h *harness) time.Time {
	t.Helper()
	opened := h.clock.Now()
	h.sources.setEgress(tenantA, brokenProxy("sg-1", opened, 2))
	h.clock.Advance(4 * time.Minute)
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressBreaker {
		t.Fatalf("the outage was not reported: %+v", got)
	}
	return opened
}

func TestEgressRecoveryNeedsARequestToGetThrough(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	opened := reportOutage(t, h)

	// A process that started after the outage (a deploy) shows the breaker
	// closed without having seen a request through: not a recovery.
	h.sources.setEgress(tenantA, workingProxy("sg-1", time.Time{}))
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("a closed breaker with no request through sent %+v", got)
	}
	// A request through it before the outage proves nothing either.
	h.sources.setEgress(tenantA, workingProxy("sg-1", opened.Add(-time.Minute)))
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("a pass from before the outage sent %+v", got)
	}

	recovered := opened.Add(27 * time.Minute)
	h.clock.Advance(recovered.Sub(h.clock.Now()) + 30*time.Second)
	h.sources.setEgress(tenantA, workingProxy("sg-1", recovered))
	got := h.round()
	if len(got) != 1 {
		t.Fatalf("the end of the outage: %d message(s), want 1", len(got))
	}
	msg := got[0]
	if msg.Rule != RuleEgressRecovered || msg.Severity != SeverityInfo || msg.Title != "Egress proxy works again" {
		t.Fatalf("message = %+v", msg)
	}
	if want := `Proxy "SG sg-1" (socks5://203.0.113.7:443) works again after about 27m`; msg.Text != want {
		t.Fatalf("text = %q, want %q", msg.Text, want)
	}
	if facts := msg.Items[0].Facts; facts["outage_minutes"] != 27 || facts["address_changed"] != false {
		t.Fatalf("facts = %+v", facts)
	}

	// Said once.
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("the end was reported again: %+v", got)
	}
}

// During a deploy the old slot keeps evaluating with the breaker as it was
// when its requests stopped. Its view of the outage must not raise it again
// right after the new slot reported the end.
func TestEgressStaleViewAfterTheEndIsNotReportedAgain(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	opened := reportOutage(t, h)

	h.clock.Advance(time.Minute)
	h.sources.setEgress(tenantA, workingProxy("sg-1", h.clock.Now()))
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressRecovered {
		t.Fatalf("the end was not reported: %+v", got)
	}

	stale := brokenProxy("sg-1", opened, 2)
	for range 10 {
		h.clock.Advance(time.Minute)
		h.sources.setEgress(tenantA, stale)
		if got := h.round(); len(got) != 0 {
			t.Fatalf("the old slot's frozen view raised the outage again: %+v", got)
		}
	}

	// A real new outage is reported once the quiet period is over.
	h.clock.Advance(resolvedQuiet)
	h.sources.setEgress(tenantA, brokenProxy("sg-1", h.clock.Now().Add(-5*time.Minute), 2))
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressBreaker {
		t.Fatalf("a new outage after the quiet period: %+v", got)
	}
}

func TestEgressEntryWhoseAddressWasChanged(t *testing.T) {
	t.Run("to one that works", func(t *testing.T) {
		h := newHarness(t)
		h.enable(tenantA, withEgress)
		reportOutage(t, h)

		moved := workingProxy("sg-1", h.clock.Now().Add(time.Minute))
		moved.Fingerprint, moved.Endpoint = "fp-new", "socks5://198.51.100.9:7000"
		h.clock.Advance(2 * time.Minute)
		h.sources.setEgress(tenantA, moved)
		got := h.round()
		if len(got) != 1 || got[0].Rule != RuleEgressRecovered || !strings.Contains(got[0].Text, "socks5://198.51.100.9:7000") ||
			!strings.HasSuffix(got[0].Text, "(its address was changed)") || got[0].Items[0].Facts["address_changed"] != true {
			t.Fatalf("the end after an address change: %+v", got)
		}
	})

	t.Run("to one that fails too", func(t *testing.T) {
		h := newHarness(t)
		h.enable(tenantA, withEgress)
		reportOutage(t, h)

		moved := brokenProxy("sg-1", h.clock.Now(), 1)
		moved.Fingerprint = "fp-new"
		h.sources.setEgress(tenantA, moved)
		if got := h.round(); len(got) != 0 {
			t.Fatalf("the new address is not down for long yet, but got %+v", got)
		}
		// Past the spacing of the rule's messages (the first one went out 4
		// minutes ago).
		h.clock.Advance(minMessageGap)
		got := h.round()
		if len(got) != 1 || got[0].Rule != RuleEgressBreaker {
			t.Fatalf("the new address failing for 10 minutes: %+v", got)
		}
		sent, _, _ := h.store.LoadSent(tenantA)
		if _, kept := sent.Records["egress_breaker|entry:sg-1|fp-sg-1"]; kept {
			t.Fatal("the outage of the old address is still on record")
		}
	})
}

func TestEgressRemovedEntryIsForgottenWithoutAWord(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	reportOutage(t, h)

	h.sources.setEgress(tenantA)
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("a removed entry sent %+v", got)
	}
	sent, _, _ := h.store.LoadSent(tenantA)
	for key := range sent.Records {
		if strings.HasPrefix(key, egressKeyPrefix) {
			t.Fatalf("the outage of a removed entry is still on record: %s", key)
		}
	}
}

func TestEgressUnreadableRoundKeepsTheRecords(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	reportOutage(t, h)

	h.sources.setEgressErr(context.DeadlineExceeded)
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("an unreadable round sent %+v", got)
	}
	h.sources.setEgressErr(nil)
	h.sources.setEgress(tenantA, workingProxy("sg-1", h.clock.Now()))
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressRecovered {
		t.Fatalf("the end after an unreadable round: %+v", got)
	}
}

func TestEgressEndIsRetriedWhenNoChannelTakesIt(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress)
	reportOutage(t, h)

	h.sender.fail = true
	h.sources.setEgress(tenantA, workingProxy("sg-1", h.clock.Now()))
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressRecovered {
		t.Fatalf("first try of the end: %+v", got)
	}
	h.sender.fail = false
	h.clock.Advance(time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("retried before the wait: %+v", got)
	}
	h.clock.Advance(failedRetryAfter)
	if got := h.round(); len(got) != 1 || got[0].Rule != RuleEgressRecovered {
		t.Fatalf("the retry of the end: %+v", got)
	}
	h.clock.Advance(failedRetryAfter + time.Minute)
	if got := h.round(); len(got) != 0 {
		t.Fatalf("the end was sent again after it went out: %+v", got)
	}
}

func TestEgressManyProxiesDownMakeOneMessage(t *testing.T) {
	h := newHarness(t)
	h.enable(tenantA, withEgress, func(s *Settings) { s.Language = LanguageZHCN })
	opened := h.clock.Now()
	var samples []EgressSample
	for i := range 10 {
		samples = append(samples, brokenProxy(fmt.Sprintf("sg-%02d", i), opened, 2))
	}
	h.sources.setEgress(tenantA, samples...)
	h.clock.Advance(4 * time.Minute)
	got := h.round()
	if len(got) != 1 || len(got[0].Items) != 10 {
		t.Fatalf("ten proxies down: %+v", got)
	}
	msg := got[0]
	if msg.Title != "出口代理不可用 (10)" || msg.Subject.Name != "10 个代理" {
		t.Fatalf("summary = %q / %+v", msg.Title, msg.Subject)
	}
	if want := "代理「SG sg-00」（socks5://203.0.113.7:443）：代理拒绝了账号密码，已熔断 4 分钟（本租户 7 个账号经它出网；拦下 120 次请求，2 次试探均失败），下一个请求到来时试探"; msg.Items[0].Text != want {
		t.Fatalf("item = %q\nwant   %q", msg.Items[0].Text, want)
	}
}

func TestEgressProxiesThatAreNotPoolEntries(t *testing.T) {
	now := timeNow()
	shared := brokenProxy("hk-shared", now, 1)
	shared.Kind, shared.Fault, shared.Accounts = EgressSharedEntry, EgressFaultUnreachable, 0
	fallback := brokenProxy("", now, 1)
	fallback.Kind, fallback.Name, fallback.Endpoint = EgressDefaultProxy, "", "http://192.0.2.1:3128"
	own := brokenProxy("", now, 1)
	own.Kind, own.Name, own.Endpoint, own.Accounts = EgressAccountProxy, "", "socks5://192.0.2.2:1080", 1

	cases := []struct {
		sample  EgressSample
		subject Subject
		en, zh  string
	}{
		{shared, Subject{Kind: SubjectProxy, ID: "hk-shared", Name: "SG hk-shared"},
			`Shared proxy "SG hk-shared" (socks5://203.0.113.7:443): the proxy cannot be reached; requests through it have been refused for 10m (no account of this tenant uses it;`,
			"平台共享代理「SG hk-shared」（socks5://203.0.113.7:443）：连不上代理，已熔断 10 分钟（本租户没有账号经它出网；"},
		{fallback, Subject{Kind: SubjectProxy, Name: "proxy-url"},
			"The default proxy-url http://192.0.2.1:3128: the proxy rejects its credentials;",
			"默认代理 proxy-url http://192.0.2.1:3128：代理拒绝了账号密码，"},
		{own, Subject{Kind: SubjectProxy, Name: "socks5://192.0.2.2:1080"},
			"The accounts' own proxy socks5://192.0.2.2:1080: the proxy rejects its credentials; requests through it have been refused for 10m (1 of this tenant's accounts use it;",
			"账号自带的代理 socks5://192.0.2.2:1080：代理拒绝了账号密码，已熔断 10 分钟（本租户 1 个账号经它出网；"},
	}
	for _, tc := range cases {
		got := egressCandidates(EgressBreakerRule{Enabled: true, ForMinutes: 3}, []EgressSample{tc.sample}, now.Add(10*time.Minute))
		if len(got) != 1 {
			t.Fatalf("%s: %d candidates", tc.sample.Kind, len(got))
		}
		if got[0].Subject != tc.subject {
			t.Errorf("%s: subject = %+v, want %+v", tc.sample.Kind, got[0].Subject, tc.subject)
		}
		if text := got[0].Describe(LanguageEN); !strings.HasPrefix(text, tc.en) {
			t.Errorf("%s: en = %q", tc.sample.Kind, text)
		}
		if text := got[0].Describe(LanguageZHCN); !strings.HasPrefix(text, tc.zh) {
			t.Errorf("%s: zh = %q", tc.sample.Kind, text)
		}
	}
	// Proxies outside the pool are told apart by their address alone.
	if a, b := egressKey(fallback), egressKey(own); a == b || !strings.HasPrefix(a, "egress_breaker|default:|") {
		t.Fatalf("keys %q and %q", a, b)
	}
}

func TestEgressKeysRoundTrip(t *testing.T) {
	key := egressKey(brokenProxy("sg-1", timeNow(), 1))
	if key != "egress_breaker|entry:sg-1|fp-sg-1" {
		t.Fatalf("key = %q", key)
	}
	if ref, fingerprint, ok := splitEgressKey(key); !ok || ref != "entry:sg-1" || fingerprint != "fp-sg-1" {
		t.Fatalf("split = %q %q %v", ref, fingerprint, ok)
	}
	for _, other := range []string{egressEndKey(key), "account_failure|x", "egress_breaker|entry:sg-1", "gap|egress_breaker"} {
		if _, _, ok := splitEgressKey(other); ok {
			t.Fatalf("%q was taken for an outage key", other)
		}
	}
}

func TestEgressRuleSettings(t *testing.T) {
	// A row saved before the rule existed gets it switched on.
	old := DefaultSettings()
	old.Rules.EgressBreaker = EgressBreakerRule{}
	if got := old.Normalized().Rules.EgressBreaker; got != (EgressBreakerRule{Enabled: true, ForMinutes: DefaultEgressMinutes}) {
		t.Fatalf("rule of an old row = %+v", got)
	}
	// One switched off stays off.
	off := DefaultSettings()
	off.Rules.EgressBreaker.Enabled = false
	if got := off.Normalized().Rules.EgressBreaker; got.Enabled || got.ForMinutes != DefaultEgressMinutes {
		t.Fatalf("rule switched off = %+v", got)
	}
	if !off.Normalized().RulesEnabled() {
		t.Fatal("the other default rules are on")
	}

	enabled, minutes := false, 15
	next, err := DefaultSettings().ApplyPatch(Patch{Rules: &RulesPatch{EgressBreaker: &EgressBreakerPatch{Enabled: &enabled, ForMinutes: &minutes}}}, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	if next.Rules.EgressBreaker != (EgressBreakerRule{Enabled: false, ForMinutes: 15}) {
		t.Fatalf("patched rule = %+v", next.Rules.EgressBreaker)
	}
	for _, bad := range []int{0, MaxEgressMinutes + 1} {
		s := DefaultSettings()
		s.Rules.EgressBreaker.ForMinutes = bad
		if fieldErr := s.Validate(false); fieldErr == nil || fieldErr.Field != "rules.egress_breaker.for_minutes" {
			t.Fatalf("for_minutes %d: %v", bad, fieldErr)
		}
	}
}
