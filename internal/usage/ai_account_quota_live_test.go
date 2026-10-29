package usage

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCodexQuotaWindowKey(t *testing.T) {
	cases := []struct {
		prefix    string
		seconds   int64
		wantKey   string
		wantLabel string
	}{
		{"code", 18000, "code_5h", "m_quota.code_5h"},
		{"code", 604800, "code_week", "m_quota.code_weekly"},
		{"code", 3600, "code_subscription_3600", "m_quota.code_subscription"},
		{"code", 86400, "code_subscription_86400", "m_quota.code_subscription"},
		{"review", 18000, "review_5h", "m_quota.review_5h"},
		{"review", 604800, "review_week", "m_quota.review_weekly"},
		{"review", 2592000, "review_subscription_2592000", "m_quota.review_subscription"},
	}
	for _, tc := range cases {
		key, label := CodexQuotaWindowKey(tc.prefix, tc.seconds)
		if key != tc.wantKey || label != tc.wantLabel {
			t.Errorf("CodexQuotaWindowKey(%q, %d) = (%q, %q), want (%q, %q)", tc.prefix, tc.seconds, key, label, tc.wantKey, tc.wantLabel)
		}
	}
}

func TestLiveQuotaWindows(t *testing.T) {
	reset := time.Date(2026, 10, 30, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	observation := cliproxyexecutor.QuotaObservation{Windows: []cliproxyexecutor.QuotaWindowObservation{
		{Label: "5h", UsedPercent: 42, WindowMinutes: 300, ResetAt: reset},
		{Label: "week", UsedPercent: 100, WindowMinutes: 10080},
		{Label: "1h", UsedPercent: 0, WindowMinutes: 60, ResetAt: reset},
		{UsedPercent: 77},                              // no length: cannot be placed on the card
		{UsedPercent: 130, WindowMinutes: 300},         // past 100 is clamped, not negative remaining
		{UsedPercent: -4, WindowMinutes: 10080},        // never negative usage, never more than 100 remaining
		{UsedPercent: 10, WindowMinutes: -300},         // a negative length cannot be placed either
		{UsedPercent: 10, WindowMinutes: 0, Label: ""}, // unknown length
	}}

	got := liveQuotaWindows("codex", observation)
	type want struct {
		key       string
		label     string
		remaining float64
		seconds   int64
		reset     *time.Time
	}
	utcReset := reset.UTC()
	wants := []want{
		{"code_5h", "m_quota.code_5h", 58, 18000, &utcReset},
		{"code_week", "m_quota.code_weekly", 0, 604800, nil},
		{"code_subscription_3600", "m_quota.code_subscription", 100, 3600, &utcReset},
		{"code_5h", "m_quota.code_5h", 0, 18000, nil},
		{"code_week", "m_quota.code_weekly", 100, 604800, nil},
	}
	if len(got) != len(wants) {
		t.Fatalf("windows = %+v, want %d", got, len(wants))
	}
	for i, w := range wants {
		g := got[i]
		if g.QuotaKey != w.key || g.QuotaLabel != w.label || g.WindowSeconds != w.seconds || g.Percent == nil || *g.Percent != w.remaining {
			t.Errorf("window %d = %+v (percent %v), want %+v", i, g, g.Percent, w)
		}
		switch {
		case w.reset == nil && g.ResetAt != nil:
			t.Errorf("window %d invented a reset: %v", i, g.ResetAt)
		case w.reset != nil && (g.ResetAt == nil || !g.ResetAt.Equal(*w.reset) || g.ResetAt.Location() != time.UTC):
			t.Errorf("window %d reset = %v, want %v in UTC", i, g.ResetAt, w.reset)
		}
		if g.Value != "" || g.Meta != "" || g.ObservedAt != nil {
			t.Errorf("window %d carries fields the merge owns or the probe fills: %+v", i, g)
		}
	}

	// Only Codex windows can be named; another provider's numbers must not become
	// Codex card entries.
	for _, provider := range []string{"claude", "antigravity", "", "gemini-cli"} {
		if windows := liveQuotaWindows(provider, observation); windows != nil {
			t.Errorf("provider %q produced %+v", provider, windows)
		}
	}
	if windows := liveQuotaWindows(" Codex ", observation); len(windows) == 0 {
		t.Error("the provider match must ignore case and spacing")
	}
}

// --- persistence ---------------------------------------------------------

type liveFixture struct {
	auth     *coreauth.Auth
	identity *AuthSubjectIdentity
}

func newLiveFixture(t *testing.T) liveFixture {
	t.Helper()
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "live-auth", "acct-live", "live@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		t.Fatal("test auth has no identity")
	}
	if err := UpsertAIAccountSubject(identity); err != nil {
		t.Fatalf("upsert subject: %v", err)
	}
	return liveFixture{auth: auth, identity: identity}
}

func (f liveFixture) probe(t *testing.T, at time.Time, windows ...QuotaWindowDTO) {
	t.Helper()
	started := at.Add(-30 * 24 * time.Hour)
	expires := at.Add(10 * 24 * time.Hour)
	credits := int64(2)
	if err := UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
		AuthSubjectID: f.identity.ID, Provider: "codex", LastProbeState: "success", HealthStatus: "healthy",
		PlanType: "plus", SubscriptionStartedAt: &started, SubscriptionExpiresAt: &expires, SubscriptionSource: "probe",
		RestrictionSummary: "none", Quotas: windows, ResetCreditCount: &credits, ResetCreditExpirations: []string{"2026-12-01T00:00:00Z"},
		UpstreamCheckedAt: &at, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("seed probe: %v", err)
	}
}

func (f liveFixture) status(t *testing.T) AIAccountSubjectStatusRecord {
	t.Helper()
	rows, err := ListAIAccountSubjectStatus([]string{f.identity.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("status rows = %d err=%v", len(rows), err)
	}
	return rows[0]
}

func probeWindow(key string, remaining float64, reset time.Time) QuotaWindowDTO {
	r := remaining
	w := QuotaWindowDTO{QuotaKey: key, QuotaLabel: "m_quota." + key, Percent: &r}
	if !reset.IsZero() {
		w.ResetAt = &reset
	}
	return w
}

func liveObservation(at time.Time, windows ...cliproxyexecutor.QuotaWindowObservation) cliproxyexecutor.QuotaObservation {
	return cliproxyexecutor.QuotaObservation{ObservedAt: at, Windows: windows}
}

func TestMergeObservedQuotaMovesOnlyTheWindowsItReported(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	oldReset := probeAt.Add(2 * time.Hour)
	f.probe(t, probeAt,
		probeWindow("code_5h", 80, oldReset),
		probeWindow("code_week", 90, probeAt.Add(72*time.Hour)),
		probeWindow("additional:codex_bengalfox:week", 96, probeAt.Add(48*time.Hour)),
	)
	before := f.status(t)

	liveAt := probeAt.Add(20 * time.Minute)
	newReset := probeAt.Add(4 * time.Hour)
	err := MergeObservedAIAccountQuota(f.auth, liveObservation(liveAt,
		cliproxyexecutor.QuotaWindowObservation{Label: "5h", UsedPercent: 42, WindowMinutes: 300, ResetAt: newReset}))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	after := f.status(t)
	five := findWindow(after.Quotas, "code_5h")
	if five == nil || five.Percent == nil || *five.Percent != 58 || five.ResetAt == nil || !five.ResetAt.Equal(newReset) {
		t.Fatalf("code_5h = %+v, want 58%% remaining resetting at %v", five, newReset)
	}
	if five.ObservedAt == nil || !five.ObservedAt.Equal(liveAt) {
		t.Fatalf("code_5h observed at %v, want the time of the live answer %v", five.ObservedAt, liveAt)
	}
	if five.QuotaLabel != "m_quota.code_5h" || five.WindowSeconds != 18000 {
		t.Fatalf("code_5h label/seconds = %q/%d", five.QuotaLabel, five.WindowSeconds)
	}
	// Windows the answer did not mention keep their value and, above all, their age.
	for _, key := range []string{"code_week", "additional:codex_bengalfox:week"} {
		was, now := findWindow(before.Quotas, key), findWindow(after.Quotas, key)
		if now == nil || *now.Percent != *was.Percent || now.ObservedAt == nil || !now.ObservedAt.Equal(*was.ObservedAt) {
			t.Errorf("%s = %+v, want %+v untouched", key, now, was)
		}
	}
	if len(after.Quotas) != 3 || after.Quotas[0].QuotaKey != "code_5h" {
		t.Fatalf("windows = %+v: the card order must not churn", after.Quotas)
	}

	// A live reading is not a probe: nothing about the account's plan, health or
	// probe history moves, and the version still advances for readers that poll it.
	if after.PlanType != before.PlanType || after.HealthStatus != before.HealthStatus || after.LastProbeState != before.LastProbeState ||
		after.RestrictionSummary != before.RestrictionSummary || after.SubscriptionSource != before.SubscriptionSource ||
		after.ErrorCode != "" || after.ErrorSummary != "" {
		t.Fatalf("probe-owned fields changed:\n before %+v\n after  %+v", before, after)
	}
	if after.SubscriptionStartedAt == nil || !after.SubscriptionStartedAt.Equal(*before.SubscriptionStartedAt) ||
		after.SubscriptionExpiresAt == nil || !after.SubscriptionExpiresAt.Equal(*before.SubscriptionExpiresAt) {
		t.Fatalf("subscription dates changed: %+v -> %+v", before, after)
	}
	if after.ResetCreditCount == nil || *after.ResetCreditCount != 2 || len(after.ResetCreditExpirations) != 1 {
		t.Fatalf("reset credits changed: %+v", after)
	}
	if after.UpstreamCheckedAt == nil || !after.UpstreamCheckedAt.Equal(*before.UpstreamCheckedAt) {
		t.Fatalf("upstream_checked_at moved to %v: the probe throttle reads it and a live answer must not count as a probe", after.UpstreamCheckedAt)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("version = %d, want %d", after.Version, before.Version+1)
	}
	if after.QuotaObservedAt == nil || !after.QuotaObservedAt.Equal(liveAt) {
		t.Fatalf("quota_observed_at = %v, want %v", after.QuotaObservedAt, liveAt)
	}
}

func TestMergeObservedQuotaAddsAWindowTheProbeNeverSaw(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_week", 90, probeAt.Add(72*time.Hour)))

	err := MergeObservedAIAccountQuota(f.auth, liveObservation(probeAt.Add(time.Minute),
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 100, WindowMinutes: 300, ResetAt: probeAt.Add(3 * time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	after := f.status(t)
	five := findWindow(after.Quotas, "code_5h")
	if five == nil || *five.Percent != 0 {
		t.Fatalf("code_5h = %+v, want a spent window", five)
	}
	if len(after.Quotas) != 2 || after.Quotas[0].QuotaKey != "code_week" {
		t.Fatalf("existing windows must keep their place and the new one follow: %+v", after.Quotas)
	}
}

// A live reading waits in the scheduler's write queue, so a probe can store a
// newer answer for the same window first. The older reading must lose.
func TestMergeObservedQuotaNeverReplacesANewerProbe(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	f.probe(t, probeAt,
		probeWindow("code_5h", 70, probeAt.Add(3*time.Hour)),
		probeWindow("code_week", 90, probeAt.Add(72*time.Hour)))
	before := f.status(t)

	stale := probeAt.Add(-10 * time.Second)
	if err := MergeObservedAIAccountQuota(f.auth, liveObservation(stale,
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 5, WindowMinutes: 300, ResetAt: probeAt.Add(time.Hour)})); err != nil {
		t.Fatal(err)
	}
	after := f.status(t)
	if got := findWindow(after.Quotas, "code_5h"); *got.Percent != 70 {
		t.Fatalf("code_5h = %v%%, the older reading replaced the newer probe", *got.Percent)
	}
	if after.Version != before.Version {
		t.Fatalf("version moved to %d although nothing was written", after.Version)
	}

	// Mixed: the window the probe holds newer data for is skipped, the other is applied.
	mixedAt := probeAt.Add(30 * time.Second)
	f.probe(t, mixedAt, probeWindow("code_5h", 60, mixedAt.Add(3*time.Hour)))
	if err := MergeObservedAIAccountQuota(f.auth, liveObservation(mixedAt.Add(-5*time.Second),
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 1, WindowMinutes: 300},
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 20, WindowMinutes: 10080, ResetAt: mixedAt.Add(96 * time.Hour)})); err != nil {
		t.Fatal(err)
	}
	mixed := f.status(t)
	if got := *findWindow(mixed.Quotas, "code_5h").Percent; got != 60 {
		t.Fatalf("code_5h = %v%%, want the probe's 60%%", got)
	}
	if got := *findWindow(mixed.Quotas, "code_week").Percent; got != 80 {
		t.Fatalf("code_week = %v%%, want the live 80%% (the probe's copy of it is older)", got)
	}
}

func TestMergeObservedQuotaKeepsTheNewerRowTimestamp(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_week", 90, probeAt.Add(72*time.Hour)))

	older := probeAt.Add(-time.Hour)
	if err := MergeObservedAIAccountQuota(f.auth, liveObservation(older,
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 5, WindowMinutes: 300})); err != nil {
		t.Fatal(err)
	}
	// The API summarises the per-window times, which would hide the column; read it.
	var stored sql.NullString
	if err := getDB().QueryRow(`SELECT quota_observed_at FROM ai_account_subject_status WHERE auth_subject_id = ?`, f.identity.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if got := parseNullableTime(stored); got == nil || !got.Equal(probeAt) {
		t.Fatalf("quota_observed_at = %v: a reading older than the row's marker must not pull it back from %v", got, probeAt)
	}
}

func TestMergeObservedQuotaCreatesNothingForUnknownAccounts(t *testing.T) {
	f := newLiveFixture(t)
	// The account has a subject but no status row yet: a probe creates it, with
	// the plan and health a live reading cannot supply.
	err := MergeObservedAIAccountQuota(f.auth, liveObservation(time.Now(),
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 100, WindowMinutes: 300, ResetAt: time.Now().Add(time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ListAIAccountSubjectStatus([]string{f.identity.ID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("a status row appeared without a probe: %+v err=%v", rows, err)
	}
}

func TestMergeObservedQuotaIgnoresWhatItCannotPlace(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_5h", 80, probeAt.Add(time.Hour)))
	before := f.status(t)
	window := cliproxyexecutor.QuotaWindowObservation{UsedPercent: 100, WindowMinutes: 300, ResetAt: probeAt.Add(2 * time.Hour)}

	claude := sharedSubjectTestAuth(sharedSubjectTenantA, "live-claude", "acct-live", "live@example.com")
	claude.Provider = "claude"
	noIdentity := &coreauth.Auth{ID: "anon", Provider: "codex"}
	for name, auth := range map[string]*coreauth.Auth{
		"another provider": claude, "an auth without identity": noIdentity, "no auth": nil,
	} {
		if err := MergeObservedAIAccountQuota(auth, liveObservation(time.Now(), window)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A window with no length cannot be placed on the card.
	if err := MergeObservedAIAccountQuota(f.auth, liveObservation(time.Now(), cliproxyexecutor.QuotaWindowObservation{UsedPercent: 100})); err != nil {
		t.Fatal(err)
	}

	after := f.status(t)
	if after.Version != before.Version || *findWindow(after.Quotas, "code_5h").Percent != 80 {
		t.Fatalf("the row changed: %+v", after)
	}
}

// The live path is the card's only. Quota history points, daily snapshots and the
// legacy tenant mirror are probe-owned, and the scheduling load provider reads the
// mirror: writing there would turn usage below 100% into a routing weight.
func TestMergeObservedQuotaTouchesOnlyTheSharedStatusRow(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_5h", 80, probeAt.Add(time.Hour)))

	snapshot := func() map[string]int {
		counts := map[string]int{}
		for _, table := range []string{
			"ai_account_subject_quota_points", "ai_account_status", "ai_account_subject_quota_cycles",
			"ai_account_subject_quota_cycle_anchors", "quota_snapshot_points", "quota_snapshot_daily",
		} {
			var n int
			if err := getDB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				continue // a table this schema does not have cannot be written to either
			}
			counts[table] = n
		}
		return counts
	}
	before := snapshot()
	for _, table := range []string{"ai_account_subject_quota_points", "ai_account_status"} {
		if _, ok := before[table]; !ok {
			t.Fatalf("%s does not exist in this schema; the test would guard nothing", table)
		}
	}

	for i := 0; i < 3; i++ {
		if err := MergeObservedAIAccountQuota(f.auth, liveObservation(probeAt.Add(time.Duration(i+1)*time.Minute),
			cliproxyexecutor.QuotaWindowObservation{UsedPercent: float64(10 * (i + 1)), WindowMinutes: 300, ResetAt: probeAt.Add(2 * time.Hour)})); err != nil {
			t.Fatal(err)
		}
	}
	after := snapshot()
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows before, %d after a live reading", table, n, after[table])
		}
	}
}

// Both writers of quota_json read, merge and write back. Without one lock a
// probe that loaded the row before a live merge would write it back without the
// merge, and the card would show the older number.
func TestQuotaReadModifyWritersShareOneLock(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_5h", 80, probeAt.Add(time.Hour)))

	run := func(name string, write func() error) {
		subjectQuotaMu.Lock()
		done := make(chan error, 1)
		go func() { done <- write() }()
		select {
		case err := <-done:
			subjectQuotaMu.Unlock()
			t.Fatalf("%s finished (%v) while another writer held the quota lock", name, err)
		case <-time.After(150 * time.Millisecond):
		}
		subjectQuotaMu.Unlock()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never finished after the lock was released", name)
		}
	}
	run("probe upsert", func() error {
		at := probeAt.Add(time.Minute)
		return UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
			AuthSubjectID: f.identity.ID, Provider: "codex", LastProbeState: "success", HealthStatus: "healthy",
			Quotas: []QuotaWindowDTO{probeWindow("code_5h", 79, at.Add(time.Hour))}, UpstreamCheckedAt: &at, UpdatedAt: at,
		})
	})
	run("live merge", func() error {
		return MergeObservedAIAccountQuota(f.auth, liveObservation(probeAt.Add(2*time.Minute),
			cliproxyexecutor.QuotaWindowObservation{UsedPercent: 30, WindowMinutes: 300, ResetAt: probeAt.Add(2 * time.Hour)}))
	})
}

func TestProbesAndLiveMergesInterleaveWithoutLosingEitherKindOfWindow(t *testing.T) {
	f := newLiveFixture(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, base, probeWindow("code_week", 90, base.Add(72*time.Hour)))

	const rounds = 40
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			at := base.Add(time.Duration(2*i) * time.Second)
			err := UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
				AuthSubjectID: f.identity.ID, Provider: "codex", LastProbeState: "success", HealthStatus: "healthy", PlanType: "plus",
				Quotas:            []QuotaWindowDTO{probeWindow("code_week", float64(90-i), at.Add(72*time.Hour))},
				UpstreamCheckedAt: &at, UpdatedAt: at,
			})
			if err != nil {
				t.Errorf("probe %d: %v", i, err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			at := base.Add(time.Duration(2*i+1) * time.Second)
			err := MergeObservedAIAccountQuota(f.auth, liveObservation(at,
				cliproxyexecutor.QuotaWindowObservation{UsedPercent: float64(i), WindowMinutes: 300, ResetAt: at.Add(time.Hour)}))
			if err != nil {
				t.Errorf("live %d: %v", i, err)
			}
		}
	}()
	wg.Wait()

	final := f.status(t)
	week, five := findWindow(final.Quotas, "code_week"), findWindow(final.Quotas, "code_5h")
	if week == nil || five == nil || len(final.Quotas) != 2 {
		t.Fatalf("windows = %+v, want exactly one of each", final.Quotas)
	}
	// Each writer's last write is the newest word on its own window, whichever of
	// them ran last.
	if *week.Percent != float64(90-rounds) {
		t.Errorf("code_week = %v%%, want %v%% from the last probe", *week.Percent, 90-rounds)
	}
	if *five.Percent != float64(100-rounds) {
		t.Errorf("code_5h = %v%%, want %v%% from the last live reading", *five.Percent, 100-rounds)
	}
}

// --- through the hook ----------------------------------------------------

func TestBindingHookWritesObservedQuotaToTheCard(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_5h", 80, probeAt.Add(time.Hour)))

	var hook coreauth.QuotaObservationHook = AIAccountBindingHook{}
	hook.OnQuotaObserved(context.Background(), f.auth, liveObservation(probeAt.Add(time.Minute),
		cliproxyexecutor.QuotaWindowObservation{UsedPercent: 64, WindowMinutes: 300, ResetAt: probeAt.Add(90 * time.Minute)}))
	if got := *findWindow(f.status(t).Quotas, "code_5h").Percent; got != 36 {
		t.Fatalf("code_5h = %v%%, want 36%%", got)
	}

	// A hook has nobody to report to: a failed write is logged, never raised.
	observation := liveObservation(time.Now(), cliproxyexecutor.QuotaWindowObservation{UsedPercent: 1, WindowMinutes: 300})
	if err := getDB().Close(); err != nil {
		t.Fatal(err)
	}
	if err := MergeObservedAIAccountQuota(f.auth, observation); err == nil {
		t.Fatal("a closed database must surface as an error from the merge")
	}
	hook.OnQuotaObserved(context.Background(), f.auth, observation)
	hook.OnQuotaObserved(context.Background(), nil, cliproxyexecutor.QuotaObservation{})
}

// quotaReportingExecutor is what the Codex executor does on a live answer.
type quotaReportingExecutor struct {
	observation cliproxyexecutor.QuotaObservation
}

func (e *quotaReportingExecutor) Identifier() string { return "codex" }
func (e *quotaReportingExecutor) Execute(ctx context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	cliproxyexecutor.ObserveQuota(ctx, e.observation)
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}
func (e *quotaReportingExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "not_implemented", HTTPStatus: http.StatusNotImplemented}
}
func (e *quotaReportingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (e *quotaReportingExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &coreauth.Error{Code: "not_implemented", HTTPStatus: http.StatusNotImplemented}
}
func (e *quotaReportingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", HTTPStatus: http.StatusNotImplemented}
}

// The whole chain, as production wires it: an executor reads the windows off a
// live answer, the scheduler hands them to the binding hook, and the card the
// console reads shows them.
func TestLiveAnswerReachesTheQuotaCardThroughTheScheduler(t *testing.T) {
	f := newLiveFixture(t)
	probeAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	f.probe(t, probeAt, probeWindow("code_5h", 80, probeAt.Add(time.Hour)), probeWindow("code_week", 90, probeAt.Add(72*time.Hour)))

	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, NewAIAccountBindingHook())
	manager.RegisterExecutorForTenant(sharedSubjectTenantA, &quotaReportingExecutor{observation: liveObservation(time.Now(),
		cliproxyexecutor.QuotaWindowObservation{Label: "5h", UsedPercent: 100, WindowMinutes: 300, ResetAt: time.Now().Add(2 * time.Hour)},
		cliproxyexecutor.QuotaWindowObservation{Label: "week", UsedPercent: 35, WindowMinutes: 10080, ResetAt: time.Now().Add(96 * time.Hour)},
	)})
	registered := *f.auth
	registered.Status = coreauth.StatusActive
	if _, err := manager.Register(context.Background(), &registered); err != nil {
		t.Fatal(err)
	}

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: sharedSubjectTenantA}}
	if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.5"}, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		five := findWindow(f.status(t).Quotas, "code_5h")
		if five != nil && *five.Percent == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card never showed the live reading: %+v", f.status(t).Quotas)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := *findWindow(f.status(t).Quotas, "code_week").Percent; got != 65 {
		t.Fatalf("code_week = %v%%, want 65%%", got)
	}

	// The same answer parked the account for the model it was routed as.
	auth, _ := manager.GetByID(registered.ID)
	if !auth.Quota.Exceeded {
		t.Fatalf("the exhausted window did not gate the account: %+v", auth.Quota)
	}
}

func TestLoadSubjectQuotaRowSeparatesMissingFromEmpty(t *testing.T) {
	f := newLiveFixture(t)
	db := getDB()

	windows, observed, exists, err := loadSubjectQuotaRow(db, f.identity.ID)
	if err != nil || exists || len(windows) != 0 || observed != nil {
		t.Fatalf("no row: windows=%v observed=%v exists=%v err=%v", windows, observed, exists, err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
		AuthSubjectID: f.identity.ID, Provider: "codex", LastProbeState: "success", UpstreamCheckedAt: &at, UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	windows, _, exists, err = loadSubjectQuotaRow(db, f.identity.ID)
	if err != nil || !exists || len(windows) != 0 {
		t.Fatalf("empty row: windows=%v exists=%v err=%v", windows, exists, err)
	}

	if _, _, exists, err := loadSubjectQuotaRow(nil, f.identity.ID); err != nil || exists {
		t.Fatalf("nil db: exists=%v err=%v", exists, err)
	}
	// The wrapper the probe path uses keeps its old contract: nothing, not an error.
	if w, o := loadSubjectQuotaState(db, "no-such-subject"); w != nil || o != nil {
		t.Fatalf("loadSubjectQuotaState = %v %v for a missing row", w, o)
	}
}
