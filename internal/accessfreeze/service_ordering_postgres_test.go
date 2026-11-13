package accessfreeze

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// pausedQueryStore uses the real Postgres Store, but holds one completed row
// read/update just before database/sql returns it to the service. The database
// has already committed: a concurrent release can run, reproducing the original
// database-result/cache-publication gap without sleep-based query scheduling.
func pausedQueryStore(t *testing.T, db *sql.DB, queryFragment string) (*Store, <-chan struct{}, func()) {
	t.Helper()
	var schema string
	if err := db.QueryRow("SHOW search_path").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", schema)
	dsn.RawQuery = q.Encode()
	dc, ok := db.Driver().(driver.DriverContext)
	if !ok {
		t.Fatal("test driver lacks connector")
	}
	inner, err := dc.OpenConnector(dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	pause := &queryPause{fragment: queryFragment, entered: make(chan struct{}), resume: make(chan struct{})}
	wrapped := sql.OpenDB(&pausedConnector{Connector: inner, pause: pause})
	t.Cleanup(func() { pause.release(); _ = wrapped.Close() })
	return NewStore(wrapped), pause.entered, pause.release
}

type queryPause struct {
	fragment          string
	entered, resume   chan struct{}
	once, releaseOnce sync.Once
}

func (p *queryPause) release() { p.releaseOnce.Do(func() { close(p.resume) }) }

type pausedConnector struct {
	driver.Connector
	pause *queryPause
}

func (c *pausedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pausedConn{Conn: conn, pause: c.pause}, nil
}

type pausedConn struct {
	driver.Conn
	pause *queryPause
}

func (c *pausedConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}
func (c *pausedConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, c.pause.fragment) {
		return &pausedRows{Rows: rows, pause: c.pause}, nil
	}
	return rows, nil
}

type pausedRows struct {
	driver.Rows
	pause *queryPause
}

func (r *pausedRows) Close() error {
	err := r.Rows.Close()
	r.pause.once.Do(func() { close(r.pause.entered); <-r.pause.resume })
	return err
}

func waitOrdering(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("ordering barrier timed out")
	}
}
func refusalFor(subject Subject) refusalJob {
	return refusalJob{subject: subject, at: time.Now(), event: coreauth.UpstreamRefusalEvent{Provider: "codex", Model: "model", Refusal: coreauth.UpstreamContentRefusal{Code: "cyber_policy", Message: "fixture"}}}
}
func seedOrderingFreeze(t *testing.T, store *Store, subject Subject) Freeze {
	t.Helper()
	f, created, err := store.Freeze(context.Background(), testStrike(subject, "codex", time.Now()), 1, time.Now())
	if err != nil || !created {
		t.Fatalf("seed freeze: %#v %v %v", f, created, err)
	}
	return f
}

func TestServicePostgresReleaseWaitsForStandingPublication(t *testing.T) {
	db := openFreezePostgres(t)
	base := NewStore(db)
	subject := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "standing"}
	initial := seedOrderingFreeze(t, base, subject)
	store, entered, resume := pausedQueryStore(t, db, "UPDATE access_freezes SET strike_count")
	service := NewService(store, testZone)
	service.registry.put(initial)
	handled := make(chan struct{})
	go func() { service.handle(refusalFor(subject)); close(handled) }()
	waitOrdering(t, entered)
	// Request reads are never held by the transition gate.
	read := make(chan struct{})
	go func() { _ = service.FrozenProviders(testTenant, subject.ID, "key"); close(read) }()
	waitOrdering(t, read)
	// The old implementation commits this release while the active result is
	// paused. The fixed service must time out waiting, without changing the DB.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := service.Release(ctx, testTenant, initial.ID, "admin", "")
	if err != context.DeadlineExceeded {
		resume()
		waitOrdering(t, handled)
		t.Fatalf("release while result pending = %v, want context deadline", err)
	}
	active, err := base.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("canceled waiter changed DB: %v %v", active, err)
	}
	resume()
	waitOrdering(t, handled)
	if _, err = service.Release(context.Background(), testTenant, initial.ID, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if got := service.FrozenProviders(testTenant, subject.ID, "key"); got != "" {
		t.Fatalf("released freeze returned: %s", got)
	}
}

func TestServicePostgresReleaseWaitsForNewFreezePublication(t *testing.T) {
	db := openFreezePostgres(t)
	base := NewStore(db)
	subject := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "new"}
	store, entered, resume := pausedQueryStore(t, db, "INSERT INTO access_freezes")
	service := NewService(store, testZone)
	handled := make(chan struct{})
	go func() { service.handle(refusalFor(subject)); close(handled) }()
	waitOrdering(t, entered)
	active, err := base.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("created freeze not committed: %v %v", active, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = service.Release(ctx, testTenant, active[0].ID, "admin", ""); err != context.DeadlineExceeded {
		resume()
		waitOrdering(t, handled)
		t.Fatalf("release = %v", err)
	}
	resume()
	waitOrdering(t, handled)
	if _, err = service.Release(context.Background(), testTenant, active[0].ID, "admin", ""); err != nil {
		t.Fatal(err)
	}
	// A subsequent genuine refusal starts a new freeze; a delayed duplicate
	// release of the old id cannot lift or replace it.
	service.handle(refusalFor(subject))
	standing := service.Standing(subject)
	if len(standing) != 1 || standing[0].ID == active[0].ID {
		t.Fatalf("new freeze = %#v", standing)
	}
	if _, err = service.Release(context.Background(), testTenant, active[0].ID, "admin", ""); err != ErrAlreadyReleased {
		t.Fatalf("old release = %v", err)
	}
	if got := service.Standing(subject); len(got) != 1 || got[0].ID != standing[0].ID {
		t.Fatalf("old release changed new freeze: %#v", got)
	}
}

func TestServicePostgresRefreshOrdersWithReleaseAndFreeze(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_then_freeze", true: "active_then_release"}[existing], func(t *testing.T) {
			db := openFreezePostgres(t)
			base := NewStore(db)
			subject := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "refresh"}
			var initial Freeze
			if existing {
				initial = seedOrderingFreeze(t, base, subject)
			}
			store, entered, resume := pausedQueryStore(t, db, "FROM access_freezes WHERE released_at IS NULL ORDER BY id")
			service := NewService(store, testZone)
			refreshed := make(chan struct{})
			go func() { service.refresh(context.Background()); close(refreshed) }()
			waitOrdering(t, entered)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if existing {
				if _, err := service.Release(ctx, testTenant, initial.ID, "admin", ""); err != context.DeadlineExceeded {
					resume()
					waitOrdering(t, refreshed)
					t.Fatalf("release = %v", err)
				}
			} else {
				// Even a release of an absent id waits behind the authoritative
				// empty snapshot. Cancellation must leave the transition reusable.
				if _, err := service.Release(ctx, testTenant, 999, "admin", ""); err != context.DeadlineExceeded {
					resume()
					waitOrdering(t, refreshed)
					t.Fatalf("release during empty refresh = %v", err)
				}
			}
			var handled chan struct{}
			if !existing {
				handled = make(chan struct{})
				started := make(chan struct{})
				go func() { close(started); service.handle(refusalFor(subject)); close(handled) }()
				waitOrdering(t, started)
				select {
				case <-handled:
					resume()
					waitOrdering(t, refreshed)
					t.Fatal("new freeze completed while the older empty refresh was unpublished")
				case <-time.After(30 * time.Millisecond):
				}
			}
			resume()
			waitOrdering(t, refreshed)
			if existing {
				if _, err := service.Release(context.Background(), testTenant, initial.ID, "admin", ""); err != nil {
					t.Fatal(err)
				}
				if len(service.Standing(subject)) != 0 {
					t.Fatal("refresh revived release")
				}
			} else {
				waitOrdering(t, handled)
				if len(service.Standing(subject)) != 1 {
					t.Fatal("empty refresh lost new freeze")
				}
			}
			// A peer learns the final state using the real DB and its own cache.
			peer := NewService(base, testZone)
			peer.refresh(context.Background())
			if len(peer.Standing(subject)) != len(service.Standing(subject)) {
				t.Fatal("peer differs after refresh")
			}
			other := subject
			other.TenantID = "00000000-0000-0000-0000-000000000002"
			if len(service.Standing(other)) != 0 || len(peer.Standing(other)) != 0 {
				t.Fatal("same subject id leaked across tenants")
			}
		})
	}
}

func TestServicePostgresRefreshCannotOvertakePendingPublication(t *testing.T) {
	db := openFreezePostgres(t)
	base := NewStore(db)
	subject := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "peer-release"}
	initial := seedOrderingFreeze(t, base, subject)
	store, entered, resume := pausedQueryStore(t, db, "UPDATE access_freezes SET strike_count")
	service := NewService(store, testZone)
	service.registry.put(initial)
	handled := make(chan struct{})
	go func() { service.handle(refusalFor(subject)); close(handled) }()
	waitOrdering(t, entered)
	// Another process can commit a release independently; it does not share
	// this service's gate. The pending local snapshot still precedes that write.
	if _, err := base.Release(context.Background(), testTenant, initial.ID, "peer", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	service.refresh(ctx)
	if ctx.Err() != context.DeadlineExceeded {
		resume()
		waitOrdering(t, handled)
		t.Fatal("refresh overtook an unpublished local result")
	}
	resume()
	waitOrdering(t, handled)
	// Remote changes remain eventually consistent, not instantly linearizable.
	// Once the prior result is published, a fresh authoritative load wins.
	service.refresh(context.Background())
	if got := service.Standing(subject); len(got) != 0 {
		t.Fatalf("peer release not learned: %#v", got)
	}
}

func TestServicePostgresNewFreezeWaitsForReleasePublication(t *testing.T) {
	db := openFreezePostgres(t)
	base := NewStore(db)
	subject := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "release-first"}
	initial := seedOrderingFreeze(t, base, subject)
	store, entered, resume := pausedQueryStore(t, db, "UPDATE access_freezes SET released_at")
	service := NewService(store, testZone)
	service.registry.put(initial)
	released := make(chan struct{})
	var releaseErr error
	go func() {
		_, releaseErr = service.Release(context.Background(), testTenant, initial.ID, "admin", "")
		close(released)
	}()
	waitOrdering(t, entered)
	// The release is committed but not yet reflected in memory. A genuine new
	// refusal must be handled after publication, not be erased by that release.
	handled := make(chan struct{})
	started := make(chan struct{})
	go func() { close(started); service.handle(refusalFor(subject)); close(handled) }()
	waitOrdering(t, started)
	select {
	case <-handled:
		resume()
		waitOrdering(t, released)
		t.Fatal("refusal overtook release publication")
	case <-time.After(30 * time.Millisecond):
	}
	resume()
	waitOrdering(t, released)
	waitOrdering(t, handled)
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	got := service.Standing(subject)
	if len(got) != 1 || got[0].ID == initial.ID {
		t.Fatalf("new freeze = %#v", got)
	}
	rows, err := base.ListActive(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != got[0].ID {
		t.Fatalf("cache/database differ: %#v %v", rows, err)
	}
}
