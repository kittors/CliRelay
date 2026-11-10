package management

import (
	"testing"
	"time"
)

// useIsolatedOAuthSessions gives the test its own in-process session store
// and puts the previous one back when the test ends. A login started by an
// earlier test may still have its callback waiter running. Whichever store
// that waiter reads, its state is not pending there, so it returns within
// one poll interval and records nothing in this test's store.
func useIsolatedOAuthSessions(t *testing.T, ttl time.Duration) {
	t.Helper()
	previous := oauthSessions.Swap(newOAuthSessionStore(ttl))
	t.Cleanup(func() { oauthSessions.Store(previous) })
}

func TestCompleteOAuthSessionRetainsCompletedState(t *testing.T) {
	useIsolatedOAuthSessions(t, time.Minute)

	const (
		completedState = "completed-state"
		staleState     = "stale-state"
	)

	RegisterOAuthSession(completedState, "codex")
	RegisterOAuthSession(staleState, "codex")

	CompleteOAuthSession(completedState)
	superseded := CompleteOAuthSessionsByProvider("codex")
	if superseded != 1 {
		t.Fatalf("superseded = %d, want 1", superseded)
	}

	provider, status, ok := GetOAuthSession(completedState)
	if !ok {
		t.Fatal("expected completed session to remain queryable")
	}
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if status != oauthSessionStatusCompleted {
		t.Fatalf("status = %q, want %q", status, oauthSessionStatusCompleted)
	}

	// The replaced login stays as a non-pending tombstone, so a status poll can
	// tell it apart from an expired one.
	_, staleStatus, ok := GetOAuthSession(staleState)
	if !ok || staleStatus != oauthSessionStatusSuperseded {
		t.Fatalf("stale session = (%q, %v), want a %q tombstone", staleStatus, ok, oauthSessionStatusSuperseded)
	}
	if IsOAuthSessionPending(staleState, "codex") {
		t.Fatal("a superseded session must not accept callbacks")
	}
}
