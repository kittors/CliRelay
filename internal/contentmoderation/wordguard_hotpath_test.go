package contentmoderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// The guard stands in front of every request, so the policy store must not be
// able to hold requests up: a request waits a short while for its policies and
// then goes on with the ones remembered from earlier requests.

// eventually polls until check passes or the time is up.
func eventually(t *testing.T, within time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("still waiting for %s after %v", what, within)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestWordPolicyReadsShareOneReadPerKey(t *testing.T) {
	var reads wordPolicyReads
	var started atomic.Int32
	release := make(chan struct{})
	run := func(context.Context) (WordPolicies, error) {
		started.Add(1)
		<-release
		return WordPolicies{Tenant: &WordPolicy{TenantID: "a"}}, nil
	}

	const callers = 50
	got := make([]*wordRead, callers)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = reads.join(wordReadKey{"a", "k"}, run)
		}()
	}
	wg.Wait()
	other := reads.join(wordReadKey{"b", "k"}, run)
	close(release)
	<-other.done
	for _, read := range got {
		if read != got[0] {
			t.Fatal("callers asking for the same tenant and key must share one read")
		}
		<-read.done
	}
	if n := started.Load(); n != 2 {
		t.Fatalf("%d reads ran for 51 callers over 2 keys, want 2", n)
	}
	if got[0].err != nil || got[0].policies.Tenant == nil {
		t.Fatalf("read = %+v", got[0])
	}
	// Once a read is over the next caller starts a new one, which sees the data
	// as it is now.
	again := reads.join(wordReadKey{"a", "k"}, func(context.Context) (WordPolicies, error) { return WordPolicies{}, nil })
	<-again.done
	if again == got[0] {
		t.Fatal("a finished read was handed out again")
	}
}

func TestWordPolicyReadsSurviveAPanic(t *testing.T) {
	var reads wordPolicyReads
	read := reads.join(wordReadKey{"a", ""}, func(context.Context) (WordPolicies, error) { panic("boom") })
	select {
	case <-read.done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters of a read that panicked were left waiting")
	}
	if read.err == nil {
		t.Fatal("a read that panicked must report an error")
	}
	next := reads.join(wordReadKey{"a", ""}, func(context.Context) (WordPolicies, error) { return WordPolicies{}, nil })
	<-next.done
	if next == read || next.err != nil {
		t.Fatalf("the key stayed taken by the read that panicked: %+v", next)
	}
}

func TestWordPolicyReadsSlowModeEndsWhenAReadSucceeds(t *testing.T) {
	reads := wordPolicyReads{slowFor: time.Hour}
	now := time.Now()
	if reads.slow(now) {
		t.Fatal("slow before anything went wrong")
	}
	reads.markSlow(now)
	if !reads.slow(now.Add(time.Minute)) || reads.slow(now.Add(2*time.Hour)) {
		t.Fatal("slow mode must last for slowFor")
	}
	failed := reads.join(wordReadKey{"a", ""}, func(context.Context) (WordPolicies, error) { return WordPolicies{}, errors.New("down") })
	<-failed.done
	if !reads.slow(now.Add(time.Minute)) {
		t.Fatal("a failed read must not end slow mode")
	}
	ok := reads.join(wordReadKey{"a", ""}, func(context.Context) (WordPolicies, error) { return WordPolicies{}, nil })
	<-ok.done
	if reads.slow(now.Add(time.Minute)) {
		t.Fatal("the store answered; requests should wait for it again")
	}
}

func TestWordPolicyReadsHaveATimeOfTheirOwn(t *testing.T) {
	reads := wordPolicyReads{readTimeout: 30 * time.Millisecond}
	read := reads.join(wordReadKey{"a", ""}, func(ctx context.Context) (WordPolicies, error) {
		<-ctx.Done()
		return WordPolicies{}, ctx.Err()
	})
	select {
	case <-read.done:
	case <-time.After(2 * time.Second):
		t.Fatal("a read must not outlive its own timeout")
	}
	if !errors.Is(read.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", read.err)
	}
}

// busyStore is a store whose only database connection is taken until release is
// called, as when every connection of the pool is busy with a slow statement.
func busyStore(t *testing.T) (store *Store, hold func(), release func()) {
	t.Helper()
	store = newTestStore(t)
	var conn *sql.Conn
	hold = func() {
		var err error
		if conn, err = store.db.Conn(context.Background()); err != nil {
			t.Fatalf("take the connection: %v", err)
		}
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = conn.Close() }) }
	t.Cleanup(release)
	return store, hold, release
}

func TestLoadWordPoliciesDoesNotWaitForAStalledStore(t *testing.T) {
	store, hold, release := busyStore(t)
	remembered := savedWordPolicy(t, store, "tenant-h", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("remembered")}}, nil)
	if got, err := store.LoadWordPolicies(context.Background(), "tenant-h", ""); err != nil || got.Tenant == nil {
		t.Fatalf("first load: %+v, %v", got, err)
	}
	hold()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := store.LoadWordPolicies(ctx, "tenant-h", "")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("a request waited %v for a store that was not answering", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if got.Tenant == nil || got.Tenant.Version != remembered.Version {
		t.Fatalf("the policy it last saw must still be returned: %+v", got)
	}

	// Having just lost patience, the next requests do not even try to wait.
	for i := 0; i < 20; i++ {
		start = time.Now()
		got, err = store.LoadWordPolicies(context.Background(), "tenant-h", "")
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("request %d waited %v while the store is known to be slow", i, elapsed)
		}
		if !errors.Is(err, errWordPolicySlow) || got.Tenant == nil {
			t.Fatalf("request %d: %+v, %v", i, got, err)
		}
	}

	// The read the first request started was not cancelled: it finishes once the
	// connection is free, and the store answers normally again.
	release()
	eventually(t, 2*time.Second, "the store to answer again", func() bool {
		_, err := store.LoadWordPolicies(context.Background(), "tenant-h", "")
		return err == nil
	})
}

// A policy too big to read in the time a request can spare is still read: by the
// request after the one that gave up.
func TestLoadWordPoliciesFinishesAReadTheRequestGaveUpOn(t *testing.T) {
	store, hold, release := busyStore(t)
	savedWordPolicy(t, store, "tenant-cold", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Action: WordActionBlock, Rules: []WordRule{kw("bad")}}, nil)
	hold()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, err := store.LoadWordPolicies(ctx, "tenant-cold", "")
	if err == nil || got.Tenant != nil {
		t.Fatalf("nothing is known yet, so the request goes unguarded: %+v, %v", got, err)
	}
	release()
	eventually(t, 2*time.Second, "the background read to fill the cache", func() bool {
		return store.rememberedWordPolicies("tenant-cold", "").Tenant != nil
	})
	if got, err = store.LoadWordPolicies(context.Background(), "tenant-cold", ""); err != nil || got.Tenant == nil {
		t.Fatalf("the next request must find the policy: %+v, %v", got, err)
	}
}

func TestGuardRequestStopsWaitingForTheStoreAndAppliesWhatItKnows(t *testing.T) {
	store, hold, _ := busyStore(t)
	savedWordPolicy(t, store, "tenant-g", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Action: WordActionBlock, Rules: []WordRule{kw("bad")}}, nil)
	// The guard learns the policy the way it does in service: from the store.
	if got, err := store.LoadWordPolicies(context.Background(), "tenant-g", ""); err != nil || got.Tenant == nil {
		t.Fatalf("first load: %+v, %v", got, err)
	}
	moderator := NewRequestModerator(store, nil)
	moderator.wordLoadTimeout = 25 * time.Millisecond
	req, opts := guardOptions("tenant-g", chatRequest("hello bad world"))
	if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked {
		t.Fatalf("the policy must apply while the store answers: %+v", result)
	}
	hold()

	ctx, ginCtx := snapshotContext(t)
	start := time.Now()
	result := moderator.GuardRequest(ctx, req, opts)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("the guard held the request up for %v", elapsed)
	}
	if !result.Blocked {
		t.Fatalf("the policy it last saw must still apply: %+v", result)
	}
	snapshot, _ := WordGuardSnapshotFromGin(ginCtx)
	encoded, _ := json.Marshal(snapshot)
	if gjson.GetBytes(encoded, "policy_error").String() != "policy_store_timeout" {
		t.Fatalf("snapshot = %s", encoded)
	}

	// A tenant it has never seen goes through, and fast.
	other, otherOpts := guardOptions("tenant-never-seen", chatRequest("hello bad world"))
	start = time.Now()
	if result = moderator.GuardRequest(context.Background(), other, otherOpts); result.Blocked || result.Payload != nil {
		t.Fatalf("result = %+v", result)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("the guard waited %v while the store is known to be slow", elapsed)
	}
}

// The same with the pool of a PostgreSQL connection all busy with slow
// statements, which is how a database under load looks to the guard.
func TestGuardRequestWhenThePostgresPoolIsBusy(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db := scopedPostgres(t, dsn, "cm_words_busy")
	db.SetMaxOpenConns(3)
	if err := InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	store := NewStore(db)
	savedWordPolicy(t, store, "tenant-pg", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Action: WordActionBlock, Rules: []WordRule{kw("bad")}}, nil)
	if got, err := store.LoadWordPolicies(context.Background(), "tenant-pg", ""); err != nil || got.Tenant == nil {
		t.Fatalf("first load: %+v, %v", got, err)
	}
	moderator := NewRequestModerator(store, nil)
	req, opts := guardOptions("tenant-pg", chatRequest("hello bad world"))
	if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked {
		t.Fatalf("the policy must apply while the database answers: %+v", result)
	}

	var holders sync.WaitGroup
	for i := 0; i < 3; i++ {
		holders.Add(1)
		go func() {
			defer holders.Done()
			_, _ = db.ExecContext(context.Background(), `SELECT pg_sleep(1.5)`)
		}()
	}
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	result := moderator.GuardRequest(context.Background(), req, opts)
	elapsed := time.Since(start)
	if !result.Blocked {
		t.Fatalf("the policy it last saw must apply while the pool is busy: %+v", result)
	}
	// Without the bound the call waits out the 1.5 s the statements take.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the guard held the request up for %v while the pool was busy", elapsed)
	}
	holders.Wait()
	eventually(t, 3*time.Second, "the guard to read the database again", func() bool {
		_, err := store.LoadWordPolicies(context.Background(), "tenant-pg", "")
		return err == nil
	})
}

// A fault in the guard needs a way past it that does not wait for a build.
func TestWordGuardSwitch(t *testing.T) {
	req, opts := guardOptions("tenant-a", chatRequest("hello bad world"))
	policies := WordPolicies{Tenant: tenantOf(t, WordActionBlock, kw("bad"))}

	tests := []struct {
		value string
		off   bool
	}{
		{"", false}, {"on", false}, {"1", false}, {"true", false}, {"ON", false},
		{"off", true}, {"OFF", true}, {" off ", true}, {"0", true}, {"false", true}, {"no", true}, {"disabled", true},
		// A value that means nothing leaves the guard on: a typo must not switch off a safety check.
		{"of", false}, {"maybe", false},
	}
	for _, tt := range tests {
		t.Run("CLIRELAY_WORD_GUARD="+tt.value, func(t *testing.T) {
			t.Setenv(WordGuardEnvVar, tt.value)
			resolver := &wordResolverStub{policies: policies}
			moderator := guardModerator(resolver)
			// guardModerator builds the moderator before it has a resolver, so the
			// switch is read in the constructor, as it is at start.
			result := moderator.GuardRequest(context.Background(), req, opts)
			if tt.off {
				if result.Blocked || result.Payload != nil || result.Reply != nil || len(resolver.tenants) != 0 || moderator.WordGuardEnabled() {
					t.Fatalf("a guard that is switched off must not look at the request or the store: %+v, asked %v", result, resolver.tenants)
				}
				return
			}
			if !result.Blocked || !moderator.WordGuardEnabled() {
				t.Fatalf("the guard must stay on: %+v", result)
			}
		})
	}

	t.Run("switched at run time", func(t *testing.T) {
		moderator := guardModerator(&wordResolverStub{policies: policies})
		moderator.SetWordGuardEnabled(false)
		if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked {
			t.Fatalf("result = %+v", result)
		}
		moderator.SetWordGuardEnabled(true)
		if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("a moderator without a store is not enabled", func(t *testing.T) {
		if NewRequestModerator(nil, nil).WordGuardEnabled() {
			t.Fatal("no policy store, no word guard")
		}
		var none *RuntimeModerator
		none.SetWordGuardEnabled(true)
		if none.WordGuardEnabled() {
			t.Fatal("a nil moderator cannot be enabled")
		}
	})
}
