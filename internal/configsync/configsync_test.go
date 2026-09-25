package configsync

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "configsync.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTables(db)
	return db
}

type recordedEvents struct {
	mu     sync.Mutex
	events []cluster.ConfigEvent
}

func (r *recordedEvents) publisher(_ context.Context, tx *sql.Tx, ev cluster.ConfigEvent) error {
	if tx == nil {
		return errors.New("event published outside a transaction")
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

func (r *recordedEvents) snapshot() []cluster.ConfigEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cluster.ConfigEvent(nil), r.events...)
}

func TestBumpTxComparesAndSwaps(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if got := CollectionVersion(ctx, db, DomainProxyPool, ""); got != 0 {
		t.Fatalf("fresh collection version = %d, want 0", got)
	}
	// Expecting "never written" succeeds exactly once.
	if v, err := WriteCollection(ctx, db, DomainProxyPool, "", 0, nil); err != nil || v != 1 {
		t.Fatalf("first write = (%d, %v), want (1, nil)", v, err)
	}
	if _, err := WriteCollection(ctx, db, DomainProxyPool, "", 0, nil); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("second create-only write err = %v, want version conflict", err)
	}
	if v, err := WriteCollection(ctx, db, DomainProxyPool, "", 1, nil); err != nil || v != 2 {
		t.Fatalf("write at version 1 = (%d, %v), want (2, nil)", v, err)
	}
	_, err := WriteCollection(ctx, db, DomainProxyPool, "", 1, nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current != 2 || conflict.Expected != 1 {
		t.Fatalf("stale write err = %#v, want conflict expected=1 current=2", err)
	}
	if v, err := WriteCollection(ctx, db, DomainProxyPool, "", AnyVersion, nil); err != nil || v != 3 {
		t.Fatalf("unchecked write = (%d, %v), want (3, nil)", v, err)
	}
	// Tenants are independent collections.
	if v, err := WriteCollection(ctx, db, DomainProxyPool, "tenant-b", AnyVersion, nil); err != nil || v != 1 {
		t.Fatalf("other tenant write = (%d, %v), want (1, nil)", v, err)
	}
}

func TestWriteCollectionRollsBackOnConflictAndOnError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	rec := &recordedEvents{}
	restore := SetPublisherForTest(rec.publisher)
	defer restore()

	if _, err := db.Exec(`CREATE TABLE items (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	insert := func(id string) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO items (id) VALUES (?)`, id)
			return err
		}
	}
	if _, err := WriteCollection(ctx, db, DomainCcSwitch, "", AnyVersion, insert("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCollection(ctx, db, DomainCcSwitch, "", 7, insert("b")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale write err = %v, want conflict", err)
	}
	failing := func(tx *sql.Tx) error {
		if err := insert("c")(tx); err != nil {
			return err
		}
		return errors.New("boom")
	}
	if _, err := WriteCollection(ctx, db, DomainCcSwitch, "", AnyVersion, failing); err == nil {
		t.Fatal("failing write returned nil error")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("items = %d (%v), want only the committed row", count, err)
	}
	if got := CollectionVersion(ctx, db, DomainCcSwitch, ""); got != 1 {
		t.Fatalf("version after rolled back writes = %d, want 1", got)
	}
	events := rec.snapshot()
	if len(events) != 1 || events[0].Domain != DomainCcSwitch || events[0].Version != 1 || events[0].TenantID != systemTenantID {
		t.Fatalf("events = %#v, want exactly the committed write", events)
	}
}

func TestExecSkipsTransactionWhenNothingIsAnnounced(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE items (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if Active() {
		t.Fatal("single-node process must not announce writes")
	}
	if _, err := Exec(context.Background(), db, []cluster.ConfigEvent{Event(DomainPricing, "")}, `INSERT INTO items (id) VALUES (?)`, "x"); err != nil {
		t.Fatal(err)
	}
	rec := &recordedEvents{}
	restore := SetPublisherForTest(rec.publisher)
	defer restore()
	if _, err := Exec(context.Background(), db, []cluster.ConfigEvent{Event(DomainPricing, "t1")}, `INSERT INTO items (id) VALUES (?)`, "y"); err != nil {
		t.Fatal(err)
	}
	if events := rec.snapshot(); len(events) != 1 || events[0].TenantID != "t1" {
		t.Fatalf("events = %#v", events)
	}
}

func TestRuntimeSettingDomainRouting(t *testing.T) {
	RouteRuntimeSettingKey("test-policy-key", DomainIPAccessPolicy)
	if got := RuntimeSettingDomain("test-policy-key"); got != DomainIPAccessPolicy {
		t.Fatalf("routed domain = %q", got)
	}
	if got := RuntimeSettingDomain("debug"); got != DomainRuntimeSettings {
		t.Fatalf("default domain = %q", got)
	}
}

type dispatchLog struct {
	mu    sync.Mutex
	calls []string
	batch map[string][]cluster.ConfigEvent
}

func (l *dispatchLog) handler(domain string) Handler {
	return func(_ context.Context, events []cluster.ConfigEvent) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.calls = append(l.calls, domain)
		if l.batch == nil {
			l.batch = make(map[string][]cluster.ConfigEvent)
		}
		l.batch[domain] = append(l.batch[domain], events...)
		return nil
	}
}

func (l *dispatchLog) snapshot() ([]string, map[string][]cluster.ConfigEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	batch := make(map[string][]cluster.ConfigEvent, len(l.batch))
	for k, v := range l.batch {
		batch[k] = append([]cluster.ConfigEvent(nil), v...)
	}
	return append([]string(nil), l.calls...), batch
}

func waitIdle(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherCoalescesBurstsPerDomain(t *testing.T) {
	hub := cluster.NewMemoryHub()
	a, b := hub.Join("a"), hub.Join("b")
	logs := &dispatchLog{}
	fulls := 0
	var fullMu sync.Mutex
	d := NewDispatcher(Options{
		Window: 30 * time.Millisecond,
		Handlers: map[string]Handler{
			DomainAPIKeys:         logs.handler(DomainAPIKeys),
			DomainRuntimeSettings: logs.handler(DomainRuntimeSettings),
		},
		FullReload: func(context.Context) error {
			fullMu.Lock()
			fulls++
			fullMu.Unlock()
			return nil
		},
	})
	d.Start(b)
	defer d.Stop()

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = a.Publish(ctx, cluster.TopicConfig, Event(DomainAPIKeys, ""))
	}
	_ = a.Publish(ctx, cluster.TopicConfig, KeyEvent(DomainRuntimeSettings, "", "debug", 3))
	_ = a.Publish(ctx, cluster.TopicConfig, KeyEvent(DomainRuntimeSettings, "", "debug", 4))
	_ = a.Publish(ctx, cluster.TopicConfig, KeyEvent(DomainRuntimeSettings, "", "debug", 2))
	waitIdle(t, d)

	calls, batch := logs.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want one reload per domain", calls)
	}
	if got := batch[DomainAPIKeys]; len(got) != 1 {
		t.Fatalf("api key events = %#v, want the burst coalesced", got)
	}
	if got := batch[DomainRuntimeSettings]; len(got) != 1 || got[0].Version != 4 {
		t.Fatalf("runtime events = %#v, want one event carrying the highest version", got)
	}
	fullMu.Lock()
	defer fullMu.Unlock()
	if fulls != 0 {
		t.Fatalf("full reloads = %d, want 0", fulls)
	}
}

func TestDispatcherFullReloadOnResyncAndUnknownDomain(t *testing.T) {
	hub := cluster.NewMemoryHub()
	a, b := hub.Join("a"), hub.Join("b")
	logs := &dispatchLog{}
	var fullMu sync.Mutex
	fulls := 0
	d := NewDispatcher(Options{
		Window:   20 * time.Millisecond,
		Handlers: map[string]Handler{DomainRouting: logs.handler(DomainRouting)},
		FullReload: func(context.Context) error {
			fullMu.Lock()
			fulls++
			fullMu.Unlock()
			return nil
		},
	})
	d.Start(b)
	defer d.Stop()

	hub.Disconnect("b")
	_ = a.Publish(context.Background(), cluster.TopicConfig, Event(DomainRouting, ""))
	hub.Reconnect("b")
	waitIdle(t, d)
	fullMu.Lock()
	if fulls != 1 {
		fullMu.Unlock()
		t.Fatalf("full reloads after resync = %d, want 1", fulls)
	}
	fullMu.Unlock()

	_ = a.Publish(context.Background(), cluster.TopicConfig, Event("some_future_domain", ""))
	_ = a.Publish(context.Background(), cluster.TopicConfig, Event(DomainRouting, ""))
	waitIdle(t, d)
	fullMu.Lock()
	defer fullMu.Unlock()
	if fulls != 2 {
		t.Fatalf("full reloads after unknown domain = %d, want 2", fulls)
	}
	if calls, _ := logs.snapshot(); len(calls) != 0 {
		t.Fatalf("domain handlers ran = %v, want the full reload to cover them", calls)
	}
}

func TestDispatcherSurvivesPanickingHandler(t *testing.T) {
	logs := &dispatchLog{}
	d := NewDispatcher(Options{
		Window: 10 * time.Millisecond,
		Handlers: map[string]Handler{
			DomainPricing: func(context.Context, []cluster.ConfigEvent) error { panic("boom") },
			DomainTenants: logs.handler(DomainTenants),
		},
	})
	d.Start(nil)
	defer d.Stop()
	payload := func(domain string) cluster.Event {
		return cluster.Event{Topic: cluster.TopicConfig, Origin: "peer", Payload: []byte(`{"domain":"` + domain + `"}`)}
	}
	d.Enqueue(payload(DomainPricing))
	waitIdle(t, d)
	d.Enqueue(payload(DomainTenants))
	waitIdle(t, d)
	if calls, _ := logs.snapshot(); len(calls) != 1 || calls[0] != DomainTenants {
		t.Fatalf("calls = %v, want the worker alive after a panic", calls)
	}
}
