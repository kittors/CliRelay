package nodemetrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/storage/postgres/compatdriver"
)

func counters(at time.Time, commits, hit, read uint64) databaseCounters {
	return databaseCounters{at: at, commits: commits, rollbacks: commits / 100, hit: hit, read: read}
}

// The rates cover about the last five minutes, and a counter reset (stats
// reset, or a crash, which zeroes them) starts a new window instead of
// producing a huge negative step.
func TestDatabaseWindowSpansFiveMinutesAndRestartsOnReset(t *testing.T) {
	probe := NewDatabaseProbe(DatabaseProbeOptions{})
	start := testStart
	if w := probe.window(counters(start, 1000, 90_000, 10_000)); w != nil {
		t.Fatalf("window from one sample = %+v", w)
	}
	at := start
	for i := 1; i <= 12; i++ {
		at = at.Add(30 * time.Second)
		probe.window(counters(at, 1000+uint64(i)*300, 90_000+uint64(i)*9_900, 10_000+uint64(i)*100))
	}
	at = at.Add(30 * time.Second)
	w := probe.window(counters(at, 1000+13*300, 90_000+13*9_900, 10_000+13*100))
	if w == nil || w.Seconds != 300 || w.Commits != 10*300 || w.BlocksHit != 10*9_900 || w.BlocksRead != 10*100 {
		t.Fatalf("window = %+v, want 300 s of 300 commits and 9900/100 blocks per step", w)
	}

	// Counters went backwards: a new window starts.
	at = at.Add(30 * time.Second)
	if w := probe.window(counters(at, 5, 10, 1)); w != nil {
		t.Fatalf("window across a reset = %+v", w)
	}
	at = at.Add(30 * time.Second)
	if w := probe.window(counters(at, 65, 1010, 11)); w == nil || w.Seconds != 30 || w.Commits != 60 {
		t.Fatalf("window after the reset = %+v", w)
	}
	// stats_reset moved: also a new window, even with growing counters.
	at = at.Add(30 * time.Second)
	moved := counters(at, 100, 2000, 20)
	moved.reset = at
	if w := probe.window(moved); w != nil {
		t.Fatalf("window across a stats reset = %+v", w)
	}
}

// fakeDB returns a non-nil *sql.DB whose connections always fail, for the
// probe's failure paths.
func fakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(compatdriver.DriverName, "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A failed round keeps showing the last figures that were read, marked with
// the error and when they were read.
func TestDatabaseProbeFailureKeepsTheLastGoodFigures(t *testing.T) {
	clock := &testClock{now: testStart}
	probe := NewDatabaseProbe(DatabaseProbeOptions{Now: clock.Now})
	db := fakeDB(t)
	probe.SetSources(func() *sql.DB { return db }, nil)

	good := &Database{CollectedAt: testStart.Add(-30 * time.Second), Engine: "postgres", Version: "15.19", SizeBytes: 559693159,
		Connections: &DatabaseConnections{Max: 200, Client: 26}}
	probe.lastOK = good

	probe.round(context.Background())
	got := probe.Latest()
	if got == nil || got.Error == "" {
		t.Fatalf("latest = %+v, want an error", got)
	}
	if got.Version != "15.19" || got.SizeBytes != 559693159 || got.Connections == nil || got.Connections.Client != 26 {
		t.Fatalf("latest = %+v, want the last good figures", got)
	}
	if got.LastOKAt == nil || !got.LastOKAt.Equal(good.CollectedAt) || !got.CollectedAt.Equal(testStart) {
		t.Fatalf("times = collected %s, last ok %v", got.CollectedAt, got.LastOKAt)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), "nobody") {
		t.Fatalf("the sample names the server: %s", raw)
	}
}

// Only the leader's snapshot carries the database; a node that loses the
// lead drops it at once and starts a fresh window if it leads again.
func TestDatabaseProbeReportsOnlyWhileLeading(t *testing.T) {
	probe := NewDatabaseProbe(DatabaseProbeOptions{})
	var leading atomic.Bool
	leading.Store(true)
	db := fakeDB(t)
	probe.SetSources(func() *sql.DB { return db }, leading.Load)
	probe.latest.Store(&Database{Engine: "postgres", Version: "15.19"})
	probe.history = []databaseCounters{counters(testStart, 1, 1, 1)}

	if probe.Latest() == nil {
		t.Fatal("the leader reports nothing")
	}
	leading.Store(false)
	if got := probe.Latest(); got != nil {
		t.Fatalf("a follower reports %+v", got)
	}
	probe.round(context.Background())
	if probe.history != nil || probe.lastOK != nil {
		t.Fatal("a follower keeps the window it would later span the gap with")
	}

	// Not PostgreSQL: nothing either.
	leading.Store(true)
	probe.SetSources(func() *sql.DB { return nil }, leading.Load)
	probe.round(context.Background())
	if got := probe.Latest(); got != nil {
		t.Fatalf("latest without a database = %+v", got)
	}
}

func TestServerVersionDropsThePackagingSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"15.19":                            "15.19",
		"15.19 (Debian 15.19-1.pgdg120+1)": "15.19",
		" 16.4 ":                           "16.4",
	} {
		if got := serverVersion(in); got != want {
			t.Errorf("serverVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectorCarriesTheDatabaseSample(t *testing.T) {
	collector, _ := newTestCollector(t, t.TempDir())
	sample := &Database{Engine: "postgres", Version: "15.19"}
	collector.SetSources(Sources{Database: func() *Database { return sample }})
	collector.sample(context.Background())
	raw, _ := collector.LatestJSON()
	if !strings.Contains(string(raw), `"database":{`) || !strings.Contains(string(raw), `"version":"15.19"`) {
		t.Fatalf("snapshot = %s", raw)
	}
	collector.SetSources(Sources{Database: func() *Database { return nil }})
	collector.sample(context.Background())
	raw, _ = collector.LatestJSON()
	if strings.Contains(string(raw), `"database"`) {
		t.Fatalf("snapshot of a follower = %s", raw)
	}
}

// Against a real server (CLIRELAY_POSTGRES_TEST_DSN): every part is read, the
// probe does not count itself as activity, and an idle transaction held open
// by another session shows up.
func TestPostgresDatabaseProbeReadsTheServer(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Another session idles in a transaction.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	clock := &testClock{now: time.Now()}
	probe := NewDatabaseProbe(DatabaseProbeOptions{Now: clock.Now})
	probe.SetSources(func() *sql.DB { return db }, nil)
	probe.round(ctx)
	first := probe.Latest()
	if first == nil || first.Error != "" {
		t.Fatalf("first round = %+v", first)
	}
	if first.Engine != "postgres" || first.Version == "" || first.StartedAt == nil || first.SizeBytes <= 0 {
		t.Fatalf("server = %+v", first)
	}
	c := first.Connections
	if c == nil || c.Max <= 0 || c.Client < 2 || c.IdleInTransaction < 1 {
		t.Fatalf("connections = %+v, want this probe and the idle transaction", c)
	}
	a := first.Activity
	if a == nil || a.LongestIdleInTransactionSeconds < 1 || a.LongestTransactionSeconds < 1 {
		t.Fatalf("activity = %+v, want the idle transaction of over a second", a)
	}
	if first.WAL == nil || first.WAL.MaxWALSizeBytes == nil || *first.WAL.MaxWALSizeBytes <= 0 {
		t.Fatalf("wal = %+v", first.WAL)
	}
	if first.Window != nil {
		t.Fatalf("window after one round = %+v", first.Window)
	}

	// The counters themselves may lag: backends flush their statistics up
	// to ten seconds late. Only the window is asserted.
	clock.Advance(30 * time.Second)
	probe.round(ctx)
	second := probe.Latest()
	if second == nil || second.Window == nil || second.Window.Seconds != 30 {
		t.Fatalf("second round = %+v, want a 30 s window", second)
	}
	raw, _ := json.Marshal(second)
	for _, field := range []string{`"engine":"postgres"`, `"connections":{`, `"activity":{`, `"wal":{`, `"window":{`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("sample JSON %s lacks %s", raw, field)
		}
	}
}

// A context that ends mid-round leaves the previous sample in place instead
// of publishing a timeout the probe caused itself.
func TestDatabaseProbeStoppedMidRoundPublishesNothing(t *testing.T) {
	probe := NewDatabaseProbe(DatabaseProbeOptions{})
	db := fakeDB(t)
	probe.SetSources(func() *sql.DB { return db }, nil)
	previous := &Database{Engine: "postgres", Version: "15.19"}
	probe.latest.Store(previous)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe.round(ctx)
	if got := probe.Latest(); got != previous {
		t.Fatalf("latest = %+v, want the previous sample", got)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("context not cancelled")
	}
}
