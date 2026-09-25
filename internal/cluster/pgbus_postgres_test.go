package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// testPGTimings keeps the production election cadence, so takeover tests
// measure the real bound, and speeds up heartbeats and idle checks.
var testPGTimings = func() pgTimings {
	t := defaultPGTimings
	t.heartbeat = 250 * time.Millisecond
	t.listenIdle = 300 * time.Millisecond
	return t
}()

type pgCluster struct {
	t      *testing.T
	db     *sql.DB
	dsn    string
	prefix string
}

func newPGCluster(t *testing.T) *pgCluster {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	db, err := postgresstore.OpenRuntimeDB(context.Background(), config.PostgresConfig{DSN: dsn, MaxOpenConns: 16, MaxIdleConns: 4})
	if err != nil {
		t.Fatalf("open runtime db: %v", err)
	}
	// Short: application_name, which carries the node id, is cut at 63 bytes.
	name := strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(strings.TrimPrefix(t.Name(), "TestPostgres"), ""))
	if len(name) > 16 {
		name = name[:16]
	}
	prefix := fmt.Sprintf("ct-%s-%04x-", name, time.Now().UnixNano()&0xffff)
	cleanup := func() {
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE node_id LIKE $1`, prefix+"%")
	}
	cleanup()
	t.Cleanup(func() {
		cleanup()
		_ = db.Close()
	})
	return &pgCluster{t: t, db: db, dsn: dsn, prefix: prefix}
}

func (pc *pgCluster) start(name string) *Coordinator {
	pc.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := startPostgresWith(ctx, pc.prefix+name, Options{Enabled: true, DSN: pc.dsn, DB: pc.db, Version: "test"}, testPGTimings)
	if err != nil {
		pc.t.Fatalf("start %s: %v", name, err)
	}
	pc.t.Cleanup(c.Close)
	return c
}

func (pc *pgCluster) terminate(appName string) {
	pc.t.Helper()
	var terminated int
	err := pc.db.QueryRow(`
		SELECT count(*) FILTER (WHERE pg_terminate_backend(pid))
		  FROM pg_stat_activity WHERE application_name = $1`, appName).Scan(&terminated)
	if err != nil || terminated == 0 {
		pc.t.Fatalf("terminate %s: terminated=%d err=%v", appName, terminated, err)
	}
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) record(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) count(match func(Event) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ev := range l.events {
		if match(ev) {
			n++
		}
	}
	return n
}

func authEvent(id string) func(Event) bool {
	return func(ev Event) bool {
		var payload AuthEvent
		return !ev.Resync && ev.Topic == TopicAuth && ev.Decode(&payload) == nil && payload.ID == id
	}
}

func isResync(ev Event) bool { return ev.Resync }

func leaders(nodes ...*Coordinator) []*Coordinator {
	var out []*Coordinator
	for _, c := range nodes {
		if c.IsLeader() {
			out = append(out, c)
		}
	}
	return out
}

func leaderAndFollower(t *testing.T, a, b *Coordinator) (*Coordinator, *Coordinator) {
	t.Helper()
	waitFor(t, 5*time.Second, "a single leader", func() bool { return len(leaders(a, b)) == 1 })
	if a.IsLeader() {
		return a, b
	}
	return b, a
}

func TestPostgresExactlyOneLeader(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	leader, follower := leaderAndFollower(t, a, b)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if n := len(leaders(a, b)); n != 1 {
			t.Fatalf("%d leaders at once", n)
		}
	}
	waitFor(t, 3*time.Second, "the membership view to agree", func() bool {
		nodes, err := follower.Nodes(context.Background())
		if err != nil || len(nodes) != 2 {
			return false
		}
		for _, node := range nodes {
			if !node.Active || node.Leader != (node.NodeID == leader.NodeID()) || node.Self != (node.NodeID == follower.NodeID()) {
				return false
			}
		}
		return true
	})
}

func TestPostgresLeaderCloseHandsOver(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	leader, follower := leaderAndFollower(t, a, b)
	closedAt := time.Now()
	leader.Close()
	if leader.IsLeader() {
		t.Fatal("a closed node must not stay leader")
	}
	waitFor(t, 5*time.Second, "the follower to take over", follower.IsLeader)
	t.Logf("takeover after close took %s", time.Since(closedAt).Round(time.Millisecond))

	var stopped bool
	var leaderFlag bool
	if err := pc.db.QueryRow(`SELECT stopped_at IS NOT NULL, leader FROM cluster_nodes WHERE node_id = $1`, leader.NodeID()).Scan(&stopped, &leaderFlag); err != nil {
		t.Fatal(err)
	}
	if !stopped || leaderFlag {
		t.Fatalf("closed node row: stopped=%v leader=%v", stopped, leaderFlag)
	}
}

func TestPostgresTerminatedLeaderSessionHandsOver(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	leader, follower := leaderAndFollower(t, a, b)
	terminatedAt := time.Now()
	pc.terminate(leaderAppName(leader.NodeID()))
	waitFor(t, 5*time.Second, "the follower to take over", follower.IsLeader)
	waitFor(t, 5*time.Second, "the old leader to step down", func() bool { return !leader.IsLeader() })
	t.Logf("takeover after terminate took %s", time.Since(terminatedAt).Round(time.Millisecond))
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if leader.IsLeader() {
			t.Fatal("the node whose session failed must not grab leadership back from a healthy peer")
		}
	}
}

func TestPostgresPublishReachesPeersOnly(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	var gotA, gotB eventLog
	a.Subscribe(TopicAuth, gotA.record)
	b.Subscribe(TopicAuth, gotB.record)

	// Outlast a few idle checks so delivery after them is covered too.
	time.Sleep(3 * testPGTimings.listenIdle)
	if err := a.Publish(context.Background(), TopicAuth, AuthEvent{ID: "direct", Version: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the peer to receive the event", func() bool { return gotB.count(authEvent("direct")) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := gotA.count(authEvent("direct")); n != 0 {
		t.Fatalf("publisher received its own event %d times", n)
	}
}

func TestPostgresPublishTxDeliversOnlyOnCommit(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	var got eventLog
	b.Subscribe(TopicAuth, got.record)
	ctx := context.Background()

	tx, err := pc.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PublishTx(ctx, tx, TopicAuth, AuthEvent{ID: "rolled-back"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	tx, err = pc.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PublishTx(ctx, tx, TopicAuth, AuthEvent{ID: "committed"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got.count(authEvent("committed")) != 0 {
		t.Fatal("event must not be delivered before commit")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the committed event", func() bool { return got.count(authEvent("committed")) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := got.count(authEvent("rolled-back")); n != 0 {
		t.Fatalf("rolled back event delivered %d times", n)
	}
}

func TestPostgresListenerReconnectsAndResyncs(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	var got eventLog
	b.Subscribe(TopicConfig, got.record)
	b.Subscribe(TopicAuth, got.record)
	time.Sleep(200 * time.Millisecond)
	before := got.count(isResync)

	pc.terminate(listenerAppName(b.NodeID()))
	waitFor(t, 5*time.Second, "a resync after the listener reconnects", func() bool {
		// One Resync per subscription.
		return got.count(isResync) >= before+2
	})
	if err := a.Publish(context.Background(), TopicAuth, AuthEvent{ID: "after-reconnect"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "delivery on the new listener", func() bool { return got.count(authEvent("after-reconnect")) == 1 })
}

func TestPostgresJoiningNodeTriggersResync(t *testing.T) {
	pc := newPGCluster(t)
	a := pc.start("a")
	var got eventLog
	a.Subscribe(TopicConfig, got.record)
	pc.start("b")
	waitFor(t, 3*time.Second, "a resync on the running node", func() bool {
		return got.count(func(ev Event) bool { return ev.Resync && ev.Origin == pc.prefix+"b" }) == 1
	})
}

func TestPostgresActiveNodeCount(t *testing.T) {
	pc := newPGCluster(t)
	a, b := pc.start("a"), pc.start("b")
	waitFor(t, 3*time.Second, "both nodes to count two", func() bool {
		return a.ActiveNodeCount() == 2 && b.ActiveNodeCount() == 2
	})
	b.Close()
	waitFor(t, 3*time.Second, "the stopped node to drop out", func() bool { return a.ActiveNodeCount() == 1 })
	nodes, err := a.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.NodeID == b.NodeID() && (node.Active || node.Leader) {
			t.Fatalf("stopped node still reported active: %+v", node)
		}
	}
}

func TestPostgresSessionLocksSerialise(t *testing.T) {
	pc := newPGCluster(t)
	name := pc.prefix + "serial"
	type span struct{ start, end time.Time }
	var mu sync.Mutex
	var spans []span
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WithSessionLock(context.Background(), pc.db, name, 10*time.Second, func(context.Context) error {
				s := span{start: time.Now()}
				time.Sleep(400 * time.Millisecond)
				s.end = time.Now()
				mu.Lock()
				spans = append(spans, s)
				mu.Unlock()
				return nil
			})
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if err := WithSessionLock(context.Background(), pc.db, name, 0, func(context.Context) error { return nil }); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a single attempt while the lock is held must time out, got %v", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(spans) != 2 {
		t.Fatalf("ran %d times, want 2", len(spans))
	}
	first, second := spans[0], spans[1]
	if second.start.Before(first.end) {
		t.Fatalf("critical sections overlapped: %v-%v and %v-%v", first.start, first.end, second.start, second.end)
	}
	// The lock session is closed, not pooled, so nothing keeps the lock.
	var held int
	if err := pc.db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND ((classid::bigint << 32) | objid::bigint) = $1`, LockKey(name)).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("advisory lock still held by %d session(s)", held)
	}
}

func TestPostgresStartAdoptsPreparedCoordinator(t *testing.T) {
	pc := newPGCluster(t)
	t.Cleanup(func() { SetDefault(nil) })
	opts := Options{Enabled: true, NodeID: pc.prefix + "prepared", DSN: pc.dsn, DB: pc.db, Version: "test"}
	prepared := Prepare(opts)
	var got eventLog
	prepared.Subscribe(TopicConfig, got.record)
	c, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if c != prepared || Default() != c {
		t.Fatal("Start must reuse the coordinator Prepare installed")
	}
	if got.count(isResync) != 1 {
		t.Fatalf("subscriber from before Start must get the initial resync, got %d", got.count(isResync))
	}
	waitFor(t, 3*time.Second, "leadership of the only node", c.IsLeader)
}
