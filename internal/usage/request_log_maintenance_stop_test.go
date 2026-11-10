package usage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// TestStopRequestLogMaintenanceCancelsBodyPurgeWalk covers the retiring slots
// that systemd killed on 2026-11-10. With body storage off, every maintenance
// pass then walked all of request_log_content to strip bodies from details; in
// production that walk ran for 16 to 22 minutes. It now walks only the rows not
// yet stripped, but the first pass after an upgrade, or after storage was on,
// can still cover the whole table. CloseDB stops the maintenance worker and
// waits for it, so a walk that ignored the cancel held shutdown until
// TimeoutStopSec ran out. The driver below parks the walk's query until the
// query's context ends, as a long statement on PostgreSQL does.
func TestStopRequestLogMaintenanceCancelsBodyPurgeWalk(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	CloseDB()
	if err := InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	previous := RequestLogBodyStorageEnabled()
	SetRequestLogBodyStorageEnabled(false)
	t.Cleanup(func() { SetRequestLogBodyStorageEnabled(previous) })

	gate := &statementGate{
		match:   "WHERE bodies_stripped = 0 AND log_id > ?",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	db := openGatedSQLite(t, dbPath, gate)
	t.Cleanup(gate.open)

	startRequestLogMaintenance(db, "sqlite")
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		gate.open()
		stopRequestLogMaintenance()
		t.Fatal("maintenance pass never reached the request detail walk")
	}

	stopped := make(chan struct{})
	go func() {
		stopRequestLogMaintenance()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		gate.open()
		<-stopped
		t.Fatal("stopRequestLogMaintenance waited for the body purge walk instead of cancelling it")
	}
	if !gate.cancelled.Load() {
		t.Fatal("the walk's statement was not cancelled; it ran without the maintenance context")
	}
}

// statementGate parks queries whose text contains match until the query's
// context ends or the test opens the gate.
type statementGate struct {
	match     string
	entered   chan struct{}
	enterOnce sync.Once
	release   chan struct{}
	openOnce  sync.Once
	cancelled atomic.Bool
}

func (g *statementGate) open() { g.openOnce.Do(func() { close(g.release) }) }

func openGatedSQLite(t *testing.T, dbPath string, gate *statementGate) *sql.DB {
	t.Helper()
	probe, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite) error = %v", err)
	}
	inner := probe.Driver()
	_ = probe.Close()

	db := sql.OpenDB(&gatedConnector{dsn: dbPath, inner: inner, gate: gate})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type gatedConnector struct {
	dsn   string
	inner driver.Driver
	gate  *statementGate
}

func (c *gatedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	// The usage database stays open beside this handle; wait out its writes
	// instead of failing with SQLITE_BUSY.
	if _, err := conn.(driver.ExecerContext).ExecContext(ctx, "PRAGMA busy_timeout = 5000", nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &gatedConn{Conn: conn, gate: c.gate}, nil
}

func (c *gatedConnector) Driver() driver.Driver { return c.inner }

type gatedConn struct {
	driver.Conn
	gate *statementGate
}

func (c *gatedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, c.gate.match) {
		c.gate.enterOnce.Do(func() { close(c.gate.entered) })
		select {
		case <-ctx.Done():
			c.gate.cancelled.Store(true)
			return nil, ctx.Err()
		case <-c.gate.release:
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *gatedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *gatedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
