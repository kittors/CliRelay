// Package dberr classifies database errors by what a caller can do about them,
// whichever store or driver produced them.
package dberr

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// transientMarkers catch connection failures that reach us as plain strings,
// for example after a driver wrapped them with fmt.Errorf("%v").
var transientMarkers = []string{
	"sql: database is closed",
	"driver: bad connection",
	"conn closed",
	"connection refused",
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"server closed the connection",
	"failed to connect",
	"i/o timeout",
	"no such host",
	"the database system is shutting down",
	"the database system is starting up",
	"the database system is in recovery mode",
	"cannot execute insert in a read-only transaction",
	"committed locally, but might not have been replicated",
}

// IsTransient reports whether err means the database could not be reached or
// could not accept the statement at that moment, as opposed to rejecting the
// statement or its data. A transient failure says nothing about what was sent:
// the same request can succeed once the database answers again, so callers
// retry or spool it and must not report it as a rejection.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range []error{
		driver.ErrBadConn, sql.ErrConnDone, sql.ErrTxDone,
		io.EOF, io.ErrUnexpectedEOF, context.DeadlineExceeded, net.ErrClosed,
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) {
		return isTransientSQLState(sqlState.SQLState())
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) || pgconn.Timeout(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func isTransientSQLState(code string) bool {
	switch {
	case strings.HasPrefix(code, "08"): // connection_exception class
		return true
	case code == "57P01", code == "57P02", code == "57P03": // admin/crash shutdown, cannot connect now
		return true
	case code == "25006": // read_only_sql_transaction: the pool still points at a demoted primary
		return true
	case code == "53300": // too_many_connections: every node reconnecting at once after a failover
		return true
	}
	return false
}
