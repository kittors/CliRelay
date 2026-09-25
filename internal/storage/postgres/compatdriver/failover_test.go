package compatdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeConn stands in for a pgx connection. Each Exec pops the next scripted
// error; a transaction that errors moves to status 'E' like PostgreSQL does.
type fakeConn struct {
	mu     sync.Mutex
	id     int
	errs   []error
	status byte
	execs  int
	closed bool
}

func (f *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeConn) setErrs(errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = errs
}

func (f *fakeConn) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs
}

func (f *fakeConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeConn) Begin() (driver.Tx, error) {
	return f.BeginTx(context.Background(), driver.TxOptions{})
}

func (f *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = 'T'
	return &fakeTx{conn: f}, nil
}

func (f *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			if f.status == 'T' {
				f.status = 'E'
			}
			return nil, err
		}
	}
	return driver.RowsAffected(1), nil
}

func (f *fakeConn) TxStatus() byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == 0 {
		return 'I'
	}
	return f.status
}

type fakeTx struct{ conn *fakeConn }

func (t *fakeTx) Commit() error   { return t.end() }
func (t *fakeTx) Rollback() error { return t.end() }

func (t *fakeTx) end() error {
	t.conn.mu.Lock()
	defer t.conn.mu.Unlock()
	t.conn.status = 'I'
	return nil
}

// fakeConnector hands out fakeConns; script(n) returns the errors the n-th
// connection (1-based) fails its statements with.
type fakeConnector struct {
	mu     sync.Mutex
	conns  []*fakeConn
	script func(n int) []error
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn := &fakeConn{id: len(c.conns) + 1}
	if c.script != nil {
		conn.errs = c.script(conn.id)
	}
	c.conns = append(c.conns, conn)
	return conn, nil
}

func (c *fakeConnector) Driver() driver.Driver { return driverWrapper{} }

func (c *fakeConnector) conn(n int) *fakeConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns[n-1]
}

func (c *fakeConnector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.conns)
}

func openFakeDB(t *testing.T, script func(n int) []error) (*sql.DB, *fakeConnector) {
	t.Helper()
	inner := &fakeConnector{script: script}
	db := sql.OpenDB(&connector{driver: driverWrapper{}, inner: inner, state: &poolState{}})
	t.Cleanup(func() { _ = db.Close() })
	return db, inner
}

func pgError(code string) error {
	return &pgconn.PgError{Severity: "ERROR", Code: code, Message: "sqlstate " + code}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		err               error
		discard, readOnly bool
	}{
		{nil, false, false},
		{errors.New("plain"), false, false},
		{pgError("23505"), false, false},
		{pgError("25006"), true, true},
		{pgError("57P01"), true, false},
		{pgError("57P02"), true, false},
		{pgError("57P03"), true, false},
		{pgError("08006"), true, false},
		{fmt.Errorf("wrapped: %w", pgError("08003")), true, false},
	}
	for _, tc := range cases {
		discard, readOnly := classifyError(tc.err)
		if discard != tc.discard || readOnly != tc.readOnly {
			t.Errorf("classifyError(%v) = %v,%v want %v,%v", tc.err, discard, readOnly, tc.discard, tc.readOnly)
		}
	}
}

func TestReadOnlyAutocommitIsRetriedOnFreshConnection(t *testing.T) {
	db, inner := openFakeDB(t, func(n int) []error {
		if n == 1 {
			return []error{pgError(sqlstateReadOnlyTransaction)}
		}
		return nil
	})
	if _, err := db.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("exec after failover: %v", err)
	}
	if inner.count() != 2 {
		t.Fatalf("connections opened = %d, want 2 (one retry)", inner.count())
	}
	if !inner.conn(1).isClosed() {
		t.Fatal("connection that reached the read-only server must be closed, not pooled")
	}
	if inner.conn(1).execCount() != 1 || inner.conn(2).execCount() != 1 {
		t.Fatalf("statement ran %d/%d times, want once per connection", inner.conn(1).execCount(), inner.conn(2).execCount())
	}
}

func TestReadOnlyErrorStillVisibleAfterRetriesRunOut(t *testing.T) {
	db, _ := openFakeDB(t, func(int) []error { return []error{pgError(sqlstateReadOnlyTransaction)} })
	_, err := db.Exec("INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("expected an error when every server refuses writes")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateReadOnlyTransaction {
		t.Fatalf("error must still carry the SQLSTATE, got %v", err)
	}
}

func TestReadOnlyInsideTransactionIsNotRetried(t *testing.T) {
	db, inner := openFakeDB(t, func(n int) []error {
		if n == 1 {
			return []error{pgError(sqlstateReadOnlyTransaction)}
		}
		return nil
	})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec("INSERT INTO t VALUES (1)")
	if err == nil || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("in-transaction failure must surface unchanged, got %v", err)
	}
	_ = tx.Rollback()
	if inner.conn(1).execCount() != 1 {
		t.Fatalf("statement inside a transaction ran %d times, want 1", inner.conn(1).execCount())
	}
	if !inner.conn(1).isClosed() {
		t.Fatal("connection must be discarded once the transaction ends")
	}
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if inner.count() != 2 {
		t.Fatalf("next statement must use a new connection, opened %d", inner.count())
	}
}

func TestConnectionErrorsDiscardWithoutRetry(t *testing.T) {
	db, inner := openFakeDB(t, func(n int) []error {
		if n == 1 {
			return []error{pgError(sqlstateAdminShutdown)}
		}
		return nil
	})
	_, err := db.Exec("UPDATE t SET v = 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateAdminShutdown || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("a terminated statement may have run; it must not be retried, got %v", err)
	}
	if !inner.conn(1).isClosed() {
		t.Fatal("terminated connection must not return to the pool")
	}
}

func TestReadOnlyServerRetiresOlderConnections(t *testing.T) {
	db, inner := openFakeDB(t, nil)
	ctx := context.Background()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inner.conn(1).setErrs(pgError(sqlstateReadOnlyTransaction))
	_ = c2.Close() // conn 2 goes back to the pool while it still looks fine
	if _, err := c1.ExecContext(ctx, "INSERT INTO t VALUES (1)"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("autocommit write on a read-only server must be marked retryable, got %v", err)
	}
	_ = c1.Close()
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if !inner.conn(2).isClosed() {
		t.Fatal("a pooled connection opened before the failover must be retired")
	}
	if inner.count() != 3 {
		t.Fatalf("connections opened = %d, want 3", inner.count())
	}
}

func TestHealthyErrorsKeepConnection(t *testing.T) {
	db, inner := openFakeDB(t, func(n int) []error { return []error{pgError("23505")} })
	if _, err := db.Exec("INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("expected the unique violation")
	}
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if inner.count() != 1 || inner.conn(1).isClosed() {
		t.Fatalf("an ordinary SQL error must keep the connection pooled (opened %d)", inner.count())
	}
}
