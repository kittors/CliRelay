package usage

import (
	"testing"
	"time"
)

func subscriptionTestSubject(t *testing.T, accountID string) string {
	t.Helper()
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "sub-"+accountID, accountID, accountID+"@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		t.Fatal("test auth has no identity")
	}
	if err := UpsertAIAccountSubject(identity); err != nil {
		t.Fatalf("upsert subject: %v", err)
	}
	return identity.ID
}

func upsertSubscriptionForTest(t *testing.T, subjectID string, at time.Time, subscription AIAccountSubscription) {
	t.Helper()
	if err := UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
		AuthSubjectID: subjectID, Provider: "codex", LastProbeState: "success", HealthStatus: "healthy",
		Subscription: subscription, UpstreamCheckedAt: &at, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("upsert status: %v", err)
	}
}

func storedSubscriptionForTest(t *testing.T, subjectID string) AIAccountSubscription {
	t.Helper()
	rows, err := ListAIAccountSubjectStatus([]string{subjectID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list status: rows=%d err=%v", len(rows), err)
	}
	return rows[0].Subscription
}

func sameTimePointer(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func sameSubscription(a, b AIAccountSubscription) bool {
	return sameTimePointer(a.StartedAt, b.StartedAt) && sameTimePointer(a.ExpiresAt, b.ExpiresAt) &&
		a.Source == b.Source && sameTimePointer(a.RenewsAt, b.RenewsAt) && sameTimePointer(a.CancelsAt, b.CancelsAt) &&
		a.Period == b.Period && a.State == b.State && sameTimePointer(a.CheckedAt, b.CheckedAt)
}

// A live read stores the whole cycle. A later token snapshot or a round that
// read nothing leaves it alone (otherwise every 15-minute quota round would
// undo the daily read), and only the next live read replaces it, all of it.
func TestSubscriptionUpsertKeepsLiveReadAgainstOlderSources(t *testing.T) {
	subjectID := subscriptionTestSubject(t, "acct-cycle")
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	renews := time.Date(2026, 12, 10, 5, 3, 33, 0, time.UTC)
	expires := renews.Add(6 * time.Hour)
	live := AIAccountSubscription{
		ExpiresAt: &expires, Source: SubscriptionSourceProbe, RenewsAt: &renews,
		Period: "monthly", State: SubscriptionStateActive, CheckedAt: &at,
	}
	upsertSubscriptionForTest(t, subjectID, at, live)
	if got := storedSubscriptionForTest(t, subjectID); !sameSubscription(got, live) {
		t.Fatalf("live read round trip:\n got  %+v\n want %+v", got, live)
	}

	claimStart := time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)
	claimEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	upsertSubscriptionForTest(t, subjectID, at.Add(time.Minute), AIAccountSubscription{
		StartedAt: &claimStart, ExpiresAt: &claimEnd, Source: SubscriptionSourceSignedClaims,
	})
	if got := storedSubscriptionForTest(t, subjectID); !sameSubscription(got, live) {
		t.Fatalf("a token snapshot replaced the live read: %+v", got)
	}
	upsertSubscriptionForTest(t, subjectID, at.Add(2*time.Minute), AIAccountSubscription{})
	if got := storedSubscriptionForTest(t, subjectID); !sameSubscription(got, live) {
		t.Fatalf("a round without a read changed the cycle: %+v", got)
	}

	// Renewal turned off: the next read drops the renewal date with it.
	nextCheck := at.Add(24 * time.Hour)
	next := AIAccountSubscription{
		ExpiresAt: &expires, Source: SubscriptionSourceProbe, CancelsAt: &renews,
		Period: "monthly", State: SubscriptionStateCanceling, CheckedAt: &nextCheck,
	}
	upsertSubscriptionForTest(t, subjectID, nextCheck, next)
	if got := storedSubscriptionForTest(t, subjectID); !sameSubscription(got, next) {
		t.Fatalf("next live read:\n got  %+v\n want %+v", got, next)
	}
}

// Before any live read, token snapshots fill the cycle and replace each other.
func TestSubscriptionUpsertTokenSnapshotWithoutLiveRead(t *testing.T) {
	subjectID := subscriptionTestSubject(t, "acct-claims")
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	first := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	upsertSubscriptionForTest(t, subjectID, at, AIAccountSubscription{ExpiresAt: &first, Source: SubscriptionSourceSignedClaims})
	second := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	want := AIAccountSubscription{ExpiresAt: &second, Source: SubscriptionSourceSignedClaims}
	upsertSubscriptionForTest(t, subjectID, at.Add(time.Minute), want)
	if got := storedSubscriptionForTest(t, subjectID); !sameSubscription(got, want) {
		t.Fatalf("token snapshot:\n got  %+v\n want %+v", got, want)
	}
}

func TestPresentableDropsTokenSnapshotsPastTheirEnd(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	for _, tc := range []struct {
		name string
		in   AIAccountSubscription
		keep bool
	}{
		{"token snapshot ahead of its end", AIAccountSubscription{ExpiresAt: &future, Source: SubscriptionSourceSignedClaims}, true},
		{"token snapshot past its end", AIAccountSubscription{ExpiresAt: &past, Source: SubscriptionSourceSignedClaims}, false},
		{"token snapshot without an end", AIAccountSubscription{StartedAt: &past, Source: SubscriptionSourceSignedClaims}, false},
		{"migrated value past its end", AIAccountSubscription{ExpiresAt: &past, Source: SubscriptionSourceMigration}, false},
		// What the provider last said, even once past: the panel shows it with
		// the time it was checked.
		{"live read past its end", AIAccountSubscription{ExpiresAt: &past, Source: SubscriptionSourceProbe, CheckedAt: &past}, true},
		{"nothing known", AIAccountSubscription{}, true},
	} {
		got := tc.in.Presentable(now)
		if tc.keep && !sameSubscription(got, tc.in) {
			t.Errorf("%s: dropped, got %+v", tc.name, got)
		}
		if !tc.keep && got != (AIAccountSubscription{}) {
			t.Errorf("%s: kept %+v", tc.name, got)
		}
	}
}
