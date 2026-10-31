package inflight

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

var (
	keyA       = KeyName("k1")
	accountA   = AccountName("u1")
	ownedKey   = Owner{Key: keyA, Account: accountA}
	unownedKey = Owner{Key: keyA}
)

// estimate returns what a request of owner would reserve, without keeping the
// reservation.
func estimate(l *Ledger, owner Owner) Amounts {
	r := l.Reserve("", owner)
	defer r.Cancel()
	return r.Amounts()
}

// bill runs one request of owner to completion with a committed usage record
// of cost and tokens, so its budgets have a history to estimate from.
func bill(t *testing.T, l *Ledger, owner Owner, cost float64, tokens int64) {
	t.Helper()
	r := l.Reserve("bill", owner)
	l.Published("bill")
	l.Settle("bill", true, cost, tokens)
	r.Finish()
	if held := l.Held(owner.Key); !held.IsZero() {
		t.Fatalf("billed request still held: %+v", held)
	}
}

func TestReservationsCountOnlyTheRequestsAdmittedBefore(t *testing.T) {
	l := NewLedger(time.Minute)
	bill(t, l, ownedKey, 0.5, 1000)

	first := l.Reserve("r1", ownedKey)
	second := l.Reserve("r2", ownedKey)
	sibling := l.Reserve("r3", Owner{Key: KeyName("k2"), Account: accountA})

	if got := first.Before(ScopeKey); !got.IsZero() {
		t.Fatalf("first request sees %+v before it; a request running alone must see nothing", got)
	}
	if got := second.Before(ScopeKey); got != (Amounts{Requests: 1, Tokens: 1000, Cost: 0.5}) {
		t.Fatalf("second request sees %+v, want the first one's reservation", got)
	}
	if got := sibling.Before(ScopeKey); !got.IsZero() {
		t.Fatalf("another key of the account sees %+v in its own key scope", got)
	}
	if got := sibling.Before(ScopeAccount); got.Requests != 2 || got.Cost != 1 {
		t.Fatalf("account scope before the sibling key's request = %+v, want both earlier requests", got)
	}
	if got := l.Reserve("r4", unownedKey).Before(ScopeAccount); !got.IsZero() {
		t.Fatalf("a key without an account sees %+v in the account scope", got)
	}
}

func TestReservationEstimateFollowsTheKeysBilledRequests(t *testing.T) {
	l := NewLedger(time.Minute)
	if got := estimate(l, ownedKey); got != (Amounts{Requests: 1}) {
		t.Fatalf("with nothing billed yet the estimate is %+v, want one request and no cost", got)
	}

	bill(t, l, Owner{Key: KeyName("other")}, 2, 4000)
	if got := estimate(l, ownedKey); got.Cost != 2 || got.Tokens != 4000 {
		t.Fatalf("a key without history should borrow the process average, got %+v", got)
	}

	bill(t, l, Owner{Key: KeyName("k2"), Account: accountA}, 3, 3000)
	if got := estimate(l, ownedKey); got.Cost != 3 {
		t.Fatalf("a key without history should borrow its account's average before the process's, got %+v", got)
	}

	bill(t, l, ownedKey, 1, 1000)
	bill(t, l, ownedKey, 2, 2000)
	got := estimate(l, ownedKey)
	// 1 + 0.2 × (2 − 1)
	if got.Cost < 1.1999 || got.Cost > 1.2001 {
		t.Fatalf("running average cost = %v, want 1.2", got.Cost)
	}
	if got.Tokens < 1199.99 || got.Tokens > 1200.01 {
		t.Fatalf("running average tokens = %v, want 1200", got.Tokens)
	}
}

func TestARequestThatPublishedNothingIsReleasedWhenItEnds(t *testing.T) {
	l := NewLedger(time.Hour)
	r := l.Reserve("r1", ownedKey)
	r.Finish()
	if held := l.Held(keyA).add(l.Held(accountA)); !held.IsZero() {
		t.Fatalf("a request refused before reaching an upstream is still held: %+v", held)
	}
	if names := l.names(); len(names) != 0 {
		t.Fatalf("empty names left behind: %v", names)
	}
}

func TestARequestIsHeldUntilItsLastRecordIsCommitted(t *testing.T) {
	l := NewLedger(time.Hour)
	r := l.Reserve("r1", ownedKey)
	// The first attempt fails and is retried on another account.
	l.Published("r1")
	l.Settle("r1", false, 0, 0)
	if held := l.Held(keyA); held.Requests != 1 {
		t.Fatalf("a failed attempt released the running request: held %+v", held)
	}
	l.Published("r1")
	r.Finish()
	if held := l.Held(keyA); held.Requests != 1 {
		t.Fatalf("released before the retry's usage was committed: held %+v", held)
	}
	l.Settle("r1", true, 0.3, 300)
	if held := l.Held(keyA); !held.IsZero() {
		t.Fatalf("still held after every record was committed: %+v", held)
	}
	if got := estimate(l, ownedKey); got.Cost != 0.3 {
		t.Fatalf("estimate = %+v, want only the billed attempt (0.3) in the average", got)
	}
}

func TestUsageCommittedWhileTheRequestRunsReleasesItWhenItEnds(t *testing.T) {
	l := NewLedger(time.Hour)
	r := l.Reserve("r1", ownedKey)
	l.Published("r1")
	l.Settle("r1", true, 0.3, 300)
	if held := l.Held(keyA); held.Requests != 1 {
		t.Fatalf("usage released a request that is still streaming: held %+v", held)
	}
	r.Finish()
	if held := l.Held(keyA); !held.IsZero() {
		t.Fatalf("finished request with committed usage still held: %+v", held)
	}
}

func TestARecordThatIsNeverCommittedHoldsItsRequestForTheSettleBoundOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewLedger(10 * time.Second)
		r := l.Reserve("r1", ownedKey)
		l.Published("r1")
		r.Finish()

		time.Sleep(9 * time.Second)
		synctest.Wait()
		if held := l.Held(keyA); held.Requests != 1 {
			t.Fatalf("released before the settle bound: %+v", held)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if held := l.Held(keyA); !held.IsZero() {
			t.Fatalf("still held after the settle bound: %+v", held)
		}
		if names := l.names(); len(names) != 0 {
			t.Fatalf("empty names left behind: %v", names)
		}
		// The record arriving late finds nothing to release.
		l.Settle("r1", true, 1, 1)
	})
}

func TestUsageCommittedAfterTheRequestEndsStopsTheSettleTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewLedger(10 * time.Second)
		r := l.Reserve("r1", ownedKey)
		l.Published("r1")
		r.Finish()
		l.Settle("r1", true, 0.2, 200)
		if held := l.Held(keyA); !held.IsZero() {
			t.Fatalf("still held after its usage was committed: %+v", held)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
	})
}

func TestCancelReleasesARefusedRequest(t *testing.T) {
	l := NewLedger(time.Hour)
	r := l.Reserve("r1", ownedKey)
	r.Cancel()
	r.Finish()
	if held := l.Held(keyA).add(l.Held(accountA)); !held.IsZero() {
		t.Fatalf("refused request still held: %+v", held)
	}
	// Nothing holds the id any more: its usage changes nothing.
	l.Published("r1")
	l.Settle("r1", true, 5, 5)
	if got := estimate(l, ownedKey); got.Cost != 0 {
		t.Fatalf("usage of a refused request taught the average: %+v", got)
	}
}

func TestTurnsSharingARequestIDAreSettledInOrder(t *testing.T) {
	l := NewLedger(time.Hour)
	first := l.Reserve("conn", ownedKey)
	l.Published("conn")
	first.Finish()
	second := l.Reserve("conn", ownedKey)
	l.Published("conn")

	// The first turn's record is committed first, while the second streams.
	l.Settle("conn", true, 0.1, 10)
	if held := l.Held(keyA); held.Requests != 1 {
		t.Fatalf("held = %+v, want only the second turn", held)
	}
	if got := second.Before(ScopeKey); !got.IsZero() {
		t.Fatalf("second turn still sees %+v before it", got)
	}
	l.Settle("conn", true, 0.1, 10)
	if held := l.Held(keyA); held.Requests != 1 {
		t.Fatalf("a running turn was released by its own usage: held %+v", held)
	}
	second.Finish()
	if held := l.Held(keyA); !held.IsZero() {
		t.Fatalf("both turns billed, still held %+v", held)
	}
}

func TestARecordPublishedAfterItsRequestEndedIsWaitedFor(t *testing.T) {
	l := NewLedger(time.Hour)
	first := l.Reserve("conn", ownedKey)
	l.Published("conn")
	first.Finish()
	// The stream winds down after the turn ended and reports once more.
	l.Published("conn")
	l.Settle("conn", true, 0.1, 10)
	if held := l.Held(keyA); held.Requests != 1 {
		t.Fatalf("released with a record still on its way: held %+v", held)
	}
	l.Settle("conn", false, 0, 0)
	if held := l.Held(keyA); !held.IsZero() {
		t.Fatalf("still held after both records: %+v", held)
	}
}

func TestModelRulesCountOnlyRequestsOfTheirModels(t *testing.T) {
	l := NewLedger(time.Hour)
	bill(t, l, ownedKey, 1, 10)
	opus := l.Reserve("r1", ownedKey)
	opus.SetModel("Claude-Opus-4-7(high)")
	l.Reserve("r2", ownedKey) // model not known yet
	haiku := l.Reserve("r3", ownedKey)
	haiku.SetModel("claude-haiku-4-5")
	probe := l.Reserve("r4", ownedKey)

	if got := probe.BeforeMatching(ScopeKey, []string{"claude-opus-*"}); got.Requests != 1 {
		t.Fatalf("opus rule sees %+v, want the one opus request", got)
	}
	if got := probe.BeforeMatching(ScopeAccount, []string{"claude-*"}); got.Requests != 2 {
		t.Fatalf("claude rule sees %+v, want the two routed requests", got)
	}
}

func TestAmountsSpreadOverEveryWindow(t *testing.T) {
	m := Amounts{Requests: 2, Tokens: 30, Cost: 0.5}.Matrix()
	for _, window := range quota.OrderedWindows {
		if m.Value(quota.MetricRequests, window) != 2 || m.Value(quota.MetricTokens, window) != 30 || m.Value(quota.MetricCost, window) != 0.5 {
			t.Fatalf("window %s = %v/%v/%v", window, m.Value(quota.MetricRequests, window), m.Value(quota.MetricTokens, window), m.Value(quota.MetricCost, window))
		}
	}
}

func TestUsageShowsNothingWhenNothingIsHeld(t *testing.T) {
	if got := (Amounts{}).Usage(); got != nil {
		t.Fatalf("usage of nothing = %+v, want nil so the view omits it", got)
	}
	got := Amounts{Requests: 2, Tokens: 1999.6, Cost: 0.25}.Usage()
	if got == nil || *got != (quota.InFlightUsage{Requests: 2, Tokens: 2000, Cost: 0.25}) {
		t.Fatalf("usage = %+v", got)
	}
}

func TestNilReservationSeesNothing(t *testing.T) {
	var r *Reservation
	if !r.Before(ScopeKey).IsZero() || !r.BeforeMatching(ScopeAccount, []string{"*"}).IsZero() || !r.Amounts().IsZero() {
		t.Fatal("nil reservation must see nothing")
	}
	r.SetModel("x")
	r.Cancel()
	r.Finish()
	var l *Ledger
	if l.Reserve("r", ownedKey) != nil || !l.Held(keyA).IsZero() {
		t.Fatal("nil ledger must hold nothing")
	}
	l.Published("r")
	l.Settle("r", true, 1, 1)
}
