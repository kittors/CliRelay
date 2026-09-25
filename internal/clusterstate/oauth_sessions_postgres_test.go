package clusterstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
)

func TestOAuthSessionsRepoSemantics(t *testing.T) {
	db := openTestDB(t)
	repo := NewOAuthSessions(db)
	ctx := context.Background()

	if err := repo.Create(ctx, oauthsession.Record{State: "s1", Provider: "codex", TenantID: "t1", OwnerNode: "node-a"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := repo.Get(ctx, "s1")
	if err != nil || !ok || rec.Status != oauthsession.RecordPending || rec.Expired || rec.OwnerLost || rec.OwnerNode != "node-a" {
		t.Fatalf("created row = %+v ok=%v err=%v", rec, ok, err)
	}
	if delivered, err := repo.Deliver(ctx, "s1", "gemini", map[string]string{"code": "x"}); err != nil || delivered {
		t.Fatalf("delivery for another provider = %v, %v", delivered, err)
	}
	if delivered, err := repo.Deliver(ctx, "s1", "codex", map[string]string{"code": "c1", "state": "s1"}); err != nil || !delivered {
		t.Fatalf("delivery = %v, %v", delivered, err)
	}
	payload, taken, err := repo.TakeCallback(ctx, "s1")
	if err != nil || !taken || payload["code"] != "c1" {
		t.Fatalf("take = %v %v %v", payload, taken, err)
	}
	if _, taken, _ := repo.TakeCallback(ctx, "s1"); taken {
		t.Fatal("a callback must be taken only once")
	}
	if alive, err := repo.Heartbeat(ctx, "s1", "node-b"); err != nil || alive {
		t.Fatalf("only the owner may heartbeat: %v %v", alive, err)
	}
	if alive, err := repo.Heartbeat(ctx, "s1", "node-a"); err != nil || !alive {
		t.Fatalf("owner heartbeat = %v %v", alive, err)
	}
	if done, err := repo.Finish(ctx, "s1", oauthsession.RecordSuccess, "", time.Minute); err != nil || !done {
		t.Fatalf("finish = %v %v", done, err)
	}
	if done, _ := repo.Finish(ctx, "s1", oauthsession.RecordError, "late", time.Minute); done {
		t.Fatal("a finished session must not be finished again")
	}
	if alive, _ := repo.Heartbeat(ctx, "s1", "node-a"); alive {
		t.Fatal("a finished session must stop heartbeating")
	}

	// Superseding: only live pending sessions of the provider and tenant.
	for _, rec := range []oauthsession.Record{
		{State: "p1", Provider: "codex", TenantID: "t1", OwnerNode: "node-a"},
		{State: "p2", Provider: "codex", TenantID: "t2", OwnerNode: "node-a"},
		{State: "p3", Provider: "xai", TenantID: "t1", OwnerNode: "node-a"},
	} {
		if err := repo.Create(ctx, rec, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	states, err := repo.CancelPending(ctx, "codex", "t1", oauthsession.MessageSuperseded, time.Minute)
	if err != nil || len(states) != 1 || states[0] != "p1" {
		t.Fatalf("cancelled = %v err=%v", states, err)
	}
	if rec, _, _ := repo.Get(ctx, "p1"); rec.Status != oauthsession.RecordCancelled || rec.Error != oauthsession.MessageSuperseded {
		t.Fatalf("cancelled row = %+v", rec)
	}

	// Judgements use the database clock.
	mustExec(t, db, `UPDATE oauth_sessions SET heartbeat_at = now() - interval '2 minutes' WHERE state = 'p2'`)
	if rec, _, _ := repo.Get(ctx, "p2"); !rec.OwnerLost {
		t.Fatalf("stale heartbeat must read as a lost owner: %+v", rec)
	}
	if delivered, _ := repo.Deliver(ctx, "p2", "codex", map[string]string{"code": "x"}); delivered {
		t.Fatal("a session whose owner is gone must refuse callbacks")
	}
	mustExec(t, db, `UPDATE oauth_sessions SET expires_at = now() - interval '1 second' WHERE state = 'p3'`)
	if rec, _, _ := repo.Get(ctx, "p3"); !rec.Expired {
		t.Fatalf("past expiry must read as expired: %+v", rec)
	}
	if delivered, _ := repo.Deliver(ctx, "p3", "xai", map[string]string{"code": "x"}); delivered {
		t.Fatal("an expired session must refuse callbacks")
	}
}

func TestOAuthSessionsDeliveryExtendsExpiry(t *testing.T) {
	db := openTestDB(t)
	repo := NewOAuthSessions(db)
	ctx := context.Background()
	if err := repo.Create(ctx, oauthsession.Record{State: "late", Provider: "codex", OwnerNode: "node-a"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if delivered, err := repo.Deliver(ctx, "late", "codex", map[string]string{"code": "c"}); err != nil || !delivered {
		t.Fatalf("deliver = %v %v", delivered, err)
	}
	var remaining float64
	if err := db.QueryRow(`SELECT EXTRACT(EPOCH FROM expires_at - now()) FROM oauth_sessions WHERE state = 'late'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining < oauthsession.CallbackGrace.Seconds()-5 {
		t.Fatalf("a delivered callback must leave time for the exchange, %.0fs left", remaining)
	}
}

// Two nodes sharing PostgreSQL: node A starts the login and owns the PKCE
// verifier, node B receives the callback, A finishes, B reports the outcome.
func TestOAuthLoginAcrossNodesOnPostgres(t *testing.T) {
	db := openTestDB(t)
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := hub.Join("node-a"), hub.Join("node-b")
	storeA := oauthsession.NewClusterStore(NewOAuthSessions(db), func() *cluster.Coordinator { return nodeA }, time.Minute)
	storeB := oauthsession.NewClusterStore(NewOAuthSessions(db), func() *cluster.Coordinator { return nodeB }, time.Minute)
	storeA.SetPollInterval(time.Hour)
	t.Cleanup(storeA.Close)
	t.Cleanup(storeB.Close)

	storeA.RegisterTenant("pg-state", "anthropic", "tenant-1")
	type result struct {
		payload map[string]string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		payload, err := storeA.WaitCallback("", "anthropic", "pg-state", 10*time.Second, 0)
		done <- result{payload, err}
	}()
	// Deliver only once node A is waiting, so the wake-up has to come from
	// the bus rather than from the first read.
	time.Sleep(50 * time.Millisecond)
	if err := storeB.DeliverCallback("", "claude", "pg-state", "code-9", ""); err != nil {
		t.Fatalf("deliver on node B: %v", err)
	}
	select {
	case res := <-done:
		if res.err != nil || res.payload["code"] != "code-9" {
			t.Fatalf("owner wait = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner was not woken by the callback on node B")
	}
	storeA.Complete("pg-state")
	lookup, err := storeB.Lookup("pg-state")
	if err != nil || lookup.Outcome != oauthsession.LookupFound || lookup.Session.Status != oauthsession.StatusCompleted {
		t.Fatalf("node B lookup = %+v err=%v", lookup, err)
	}
	if err := storeB.DeliverCallback("", "anthropic", "pg-state", "again", ""); !errors.Is(err, oauthsession.ErrNotPending) {
		t.Fatalf("callback after completion = %v", err)
	}
	if lookup, err := storeB.Lookup("unknown-state"); err != nil || lookup.Outcome != oauthsession.LookupMissing {
		t.Fatalf("unknown lookup = %+v err=%v", lookup, err)
	}
}

func TestSweepClosesAbandonedOAuthSessions(t *testing.T) {
	db := openTestDB(t)
	repo := NewOAuthSessions(db)
	ctx := context.Background()
	for _, state := range []string{"expired", "orphaned", "live", "old"} {
		if err := repo.Create(ctx, oauthsession.Record{State: state, Provider: "codex", OwnerNode: "node-a"}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, db, `UPDATE oauth_sessions SET expires_at = now() - interval '1 second' WHERE state = 'expired'`)
	mustExec(t, db, `UPDATE oauth_sessions SET heartbeat_at = now() - interval '5 minutes' WHERE state = 'orphaned'`)
	mustExec(t, db, `UPDATE oauth_sessions SET status = 'success', expires_at = now() - interval '2 hours' WHERE state = 'old'`)

	if err := Sweep(ctx, db); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"expired": oauthsession.RecordExpired, "orphaned": oauthsession.RecordError, "live": oauthsession.RecordPending}
	for state, status := range want {
		rec, ok, err := repo.Get(ctx, state)
		if err != nil || !ok || rec.Status != status {
			t.Fatalf("%s after sweep = %+v ok=%v err=%v, want %s", state, rec, ok, err, status)
		}
	}
	if rec, _, _ := repo.Get(ctx, "orphaned"); rec.Error != oauthsession.MessageOwnerLost {
		t.Fatalf("orphaned session message = %q", rec.Error)
	}
	if _, ok, _ := repo.Get(ctx, "old"); ok {
		t.Fatal("rows past retention must be deleted")
	}
}
