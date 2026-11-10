package inflight

import (
	"runtime"
	"testing"
	"time"
)

// Wait for registration, not a scheduling delay: users includes blocked callers.
// Mutex waits are not durable blocking points for testing/synctest.
func waitModelGateUsers(t *testing.T, l *Ledger, name string, want int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		gate := l.modelGates[name]
		got := 0
		if gate != nil {
			got = gate.users
		}
		l.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("gate %q users = %d, want %d", name, got, want)
		default:
			runtime.Gosched()
		}
	}
}

func awaitModelAdmission(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("model admission operation did not finish")
	}
}

func assertModelGatesEmpty(t *testing.T, l *Ledger) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.modelGates) != 0 {
		t.Fatalf("unused model gates remain: %v", l.modelGates)
	}
}

func TestModelAdmissionCapturesLaterRoutedHoldsOnly(t *testing.T) {
	l := NewLedger(time.Minute)
	bill(t, l, ownedKey, 0.5, 100)
	self := l.Reserve("self", ownedKey)
	defer self.Cancel()
	self.SetModel("claude-opus-4")
	later := l.Reserve("later", ownedKey)
	defer later.Cancel()
	later.SetModel("claude-opus-4")
	unknown := l.Reserve("unknown", ownedKey)
	defer unknown.Cancel()
	sibling := l.Reserve("sibling", Owner{Key: KeyName("sibling"), Account: accountA})
	defer sibling.Cancel()
	sibling.SetModel("claude-sonnet-4")
	unrelated := l.Reserve("unrelated", Owner{Key: KeyName("other"), Account: AccountName("other")})
	defer unrelated.Cancel()
	unrelated.SetModel("claude-opus-4")

	usage, release := self.BeginModelAdmission()
	defer release()
	want := Amounts{Requests: 1, Cost: 0.5, Tokens: 100}
	if got := usage.Matching(ScopeKey, []string{"claude-*", "claude-opus-*", "*"}); got != want {
		t.Fatalf("overlapping key patterns = %+v, want %+v", got, want)
	}
	if got := usage.Matching(ScopeAccount, []string{"claude-*"}); got != (Amounts{Requests: 2, Cost: 1, Tokens: 200}) {
		t.Fatalf("account holds = %+v, want later and sibling only", got)
	}
	if got := usage.Matching(ScopeKey, []string{"gpt-*"}); !got.IsZero() {
		t.Fatalf("nonmatching model = %+v", got)
	}
	if got := usage.Matching(ScopeKey, nil); !got.IsZero() {
		t.Fatalf("empty patterns = %+v", got)
	}
}

func TestModelAdmissionSnapshotSurvivesSettlementAndCancellation(t *testing.T) {
	l := NewLedger(time.Minute)
	bill(t, l, ownedKey, 1, 100)
	settling := l.Reserve("settling", ownedKey)
	settling.SetModel("claude-opus-4")
	l.Published("settling")
	settling.Finish()
	canceling := l.Reserve("canceling", ownedKey)
	canceling.SetModel("claude-sonnet-4")
	self := l.Reserve("self", ownedKey)
	defer self.Cancel()
	usage, release := self.BeginModelAdmission()
	defer release()

	// Settlement and cancellation must finish even while the decision gate is held.
	done := make(chan struct{})
	go func() {
		l.Settle("settling", true, 1, 100)
		canceling.Cancel()
		close(done)
	}()
	awaitModelAdmission(t, done)
	if got := l.Held(keyA).Requests; got != 1 {
		t.Fatalf("live holds = %v, want self only", got)
	}
	if got := usage.Matching(ScopeAccount, []string{"claude-*"}); got != (Amounts{Requests: 2, Cost: 2, Tokens: 200}) {
		t.Fatalf("frozen holds changed after settlement: %+v", got)
	}
	release()
	fresh, finish := self.BeginModelAdmission()
	defer finish()
	if got := fresh.Matching(ScopeAccount, []string{"*"}); !got.IsZero() {
		t.Fatalf("fresh capture retained settled holds: %+v", got)
	}
}

func TestModelAdmissionSerializesSharedOwnersAndCleansWaiters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		other  Owner
		shared string
	}{
		{"same account different keys", Owner{Key: KeyName("sibling"), Account: accountA}, accountA},
		{"same key changed account", Owner{Key: keyA, Account: AccountName("new-account")}, keyA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLedger(time.Minute)
			first := l.Reserve("first", ownedKey)
			defer first.Cancel()
			second := l.Reserve("second", tc.other)
			defer second.Cancel()
			_, release := first.BeginModelAdmission()
			defer release()
			l.mu.Lock()
			original := l.modelGates[tc.shared]
			l.mu.Unlock()
			acquired := make(chan struct{})
			seen := make(chan Amounts, 1)
			leave := make(chan struct{})
			defer close(leave)
			done := make(chan struct{})
			go func() {
				usage, finish := second.BeginModelAdmission()
				scope := ScopeKey
				if tc.shared == accountA {
					scope = ScopeAccount
				}
				seen <- usage.Matching(scope, []string{"claude-*"})
				close(acquired)
				<-leave
				finish()
				close(done)
			}()
			waitModelGateUsers(t, l, tc.shared, 2)
			select {
			case <-acquired:
				t.Fatal("shared-owner admission passed a held gate")
			default:
			}
			first.SetModel("claude-opus-4")
			release()
			awaitModelAdmission(t, acquired)
			if got := <-seen; got.Requests != 1 {
				t.Fatalf("waiter did not see the accepted model: %+v", got)
			}
			l.mu.Lock()
			current := l.modelGates[tc.shared]
			users := current.users
			l.mu.Unlock()
			if current != original || users != 1 {
				t.Fatalf("waiter's gate replaced or released: same=%v users=%d", current == original, users)
			}
			leave <- struct{}{}
			awaitModelAdmission(t, done)
			assertModelGatesEmpty(t, l)
		})
	}
}

func TestModelAdmissionUnrelatedOwnersProceedIndependently(t *testing.T) {
	l := NewLedger(time.Minute)
	first := l.Reserve("first", ownedKey)
	defer first.Cancel()
	other := l.Reserve("other", Owner{Key: KeyName("other"), Account: AccountName("other")})
	defer other.Cancel()
	_, release := first.BeginModelAdmission()
	defer release()
	done := make(chan struct{})
	go func() {
		_, finish := other.BeginModelAdmission()
		other.SetModel("gpt-5")
		finish()
		close(done)
	}()
	awaitModelAdmission(t, done)
	release()
	assertModelGatesEmpty(t, l)
}

func TestModelAdmissionOrdersOwnerLocksDeterministically(t *testing.T) {
	l := NewLedger(time.Minute)
	// Deliberately reverse the fields. Owner names are opaque to the ledger;
	// ordering must depend on sorted names, not on Key versus Account.
	first := l.Reserve("first", Owner{Key: "z", Account: "a"})
	defer first.Cancel()
	second := l.Reserve("second", Owner{Key: "a", Account: "z"})
	defer second.Cancel()
	// Hold the first sorted gate. Both admissions must block on it without
	// holding z, even though their owner field orders differ.
	blocker := &modelGate{users: 1}
	blocker.mu.Lock()
	defer func() { assertModelGatesEmpty(t, l) }()
	l.modelGates = map[string]*modelGate{"a": blocker}
	done := make(chan struct{}, 2)
	for _, r := range []*Reservation{first, second} {
		go func() {
			_, finish := r.BeginModelAdmission()
			finish()
			done <- struct{}{}
		}()
	}
	waitModelGateUsers(t, l, "a", 3)
	l.mu.Lock()
	last := l.modelGates["z"]
	l.mu.Unlock()
	free := last.mu.TryLock()
	if free {
		last.mu.Unlock()
	}
	blocker.mu.Unlock()
	l.mu.Lock()
	blocker.users--
	if blocker.users == 0 {
		delete(l.modelGates, "a")
	}
	l.mu.Unlock()
	awaitModelAdmission(t, done)
	awaitModelAdmission(t, done)
	if !free {
		t.Fatal("a caller acquired z before the held a gate")
	}
}

func TestModelAdmissionNilAndRepeatedRelease(t *testing.T) {
	for _, r := range []*Reservation{nil, {}} {
		usage, release := r.BeginModelAdmission()
		if got := usage.Matching(ScopeAccount, []string{"*"}); !got.IsZero() {
			t.Fatalf("empty reservation holds = %+v", got)
		}
		release()
		release()
	}
	l := NewLedger(time.Minute)
	// Duplicate owner names must be locked only once.
	r := l.Reserve("same", Owner{Key: "same", Account: "same"})
	defer r.Cancel()
	_, release := r.BeginModelAdmission()
	release()
	release()
	assertModelGatesEmpty(t, l)
}
