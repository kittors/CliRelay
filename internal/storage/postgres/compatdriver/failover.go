package compatdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Failover awareness.
//
// After a primary switchover the old primary can stay up read-only, and the
// connections database/sql pooled against it keep failing every write until
// they age out. pgx only discards a connection when the socket dies, so the
// wrapper does it: a connection that sees one of the errors below is reported
// invalid and database/sql closes it instead of reusing it. The replacement
// dials through the DSN again, and a multi-host DSN with
// target_session_attrs=read-write lands on the new primary.

const (
	sqlstateReadOnlyTransaction = "25006"
	sqlstateAdminShutdown       = "57P01"
	sqlstateCrashShutdown       = "57P02"
	sqlstateCannotConnectNow    = "57P03"
	sqlstateConnectionClass     = "08"
)

// poolState is shared by every connection a connector opened.
type poolState struct {
	// epoch advances when a connection learns that its server went read-only.
	// Connections opened before that point reached the same server, so they
	// are retired too rather than each failing one write first.
	epoch atomic.Uint64
}

// classifyError reports whether err means the connection must not be reused,
// and whether it came from a server that refuses writes.
func classifyError(err error) (discard, readOnly bool) {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return false, false
	}
	switch code := pgErr.Code; {
	case code == sqlstateReadOnlyTransaction:
		return true, true
	case code == sqlstateAdminShutdown, code == sqlstateCrashShutdown, code == sqlstateCannotConnectNow:
		return true, false
	case strings.HasPrefix(code, sqlstateConnectionClass):
		return true, false
	}
	return false, false
}

// retryableError marks a statement the server rejected before running it, so
// database/sql may retry it on another connection. It matches
// driver.ErrBadConn for that decision and still unwraps to the PgError for
// callers that inspect the SQLSTATE.
type retryableError struct{ err error }

func (e *retryableError) Error() string        { return "driver: bad connection: " + e.err.Error() }
func (e *retryableError) Unwrap() error        { return e.err }
func (e *retryableError) Is(target error) bool { return target == driver.ErrBadConn }

// noteError records what err says about the connection. statement is true for
// a statement run outside Commit/Rollback; only such a statement is eligible
// for the transparent retry.
func (c *wrappedConn) noteError(err error, statement bool) error {
	discard, readOnly := classifyError(err)
	if !discard {
		return err
	}
	c.broken.Store(true)
	if !readOnly {
		// Connection failures may have hit a statement mid-flight; the caller
		// has to see them, database/sql must not replay the statement.
		return err
	}
	status := c.txStatus()
	if c.pool != nil && (status == 'I' || !c.readOnlyTx) {
		// A write refused outside a transaction the application declared
		// READ ONLY means the server itself is read-only.
		c.pool.epoch.Add(1)
	}
	if statement && status == 'I' {
		// Idle afterwards means the statement ran in its own implicit
		// transaction, which the server rejected before executing anything:
		// replaying it on a fresh connection cannot apply it twice.
		return &retryableError{err: err}
	}
	return err
}

// unusable reports whether database/sql must drop this connection.
func (c *wrappedConn) unusable() bool {
	if c.broken.Load() {
		return true
	}
	return c.pool != nil && c.epoch < c.pool.epoch.Load()
}

func (c *wrappedConn) pgxConn() *pgx.Conn {
	if inner, ok := c.Conn.(interface{ Conn() *pgx.Conn }); ok {
		return inner.Conn()
	}
	return nil
}

// txStatus returns the backend transaction status byte ('I' idle, 'T' in a
// transaction, 'E' failed transaction), or 0 when it is unknown.
func (c *wrappedConn) txStatus() byte {
	if conn := c.pgxConn(); conn != nil && conn.PgConn() != nil {
		return conn.PgConn().TxStatus()
	}
	if inner, ok := c.Conn.(interface{ TxStatus() byte }); ok {
		return inner.TxStatus()
	}
	return 0
}

func (c *wrappedConn) innerClosed() bool {
	if conn := c.pgxConn(); conn != nil {
		return conn.IsClosed()
	}
	return false
}

// wrappedStmt observes errors from explicitly prepared statements, which pgx
// executes without going through the connection's Exec/Query methods.
type wrappedStmt struct {
	driver.Stmt
	conn *wrappedConn
}

func (s *wrappedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	inner, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := inner.ExecContext(ctx, args)
	return result, s.conn.noteError(err, true)
}

func (s *wrappedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	inner, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := inner.QueryContext(ctx, args)
	return rows, s.conn.noteError(err, true)
}

// wrappedTx observes Commit and Rollback failures, and clears the READ ONLY
// marker once the transaction ends.
type wrappedTx struct {
	driver.Tx
	conn *wrappedConn
}

func (t *wrappedTx) Commit() error {
	err := t.Tx.Commit()
	t.conn.readOnlyTx = false
	return t.conn.noteError(err, false)
}

func (t *wrappedTx) Rollback() error {
	err := t.Tx.Rollback()
	t.conn.readOnlyTx = false
	return t.conn.noteError(err, false)
}

var _ driver.StmtExecContext = (*wrappedStmt)(nil)
var _ driver.StmtQueryContext = (*wrappedStmt)(nil)
var _ driver.Tx = (*wrappedTx)(nil)
