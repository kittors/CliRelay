package dberr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// The shape pgx produced on n143 when the link to the database dropped.
		{"dial timeout", fmt.Errorf("failed to connect to `user=cliproxy database=cliproxy`: 10.0.0.1:25432 (10.0.0.1): dial error: timeout: %w", context.DeadlineExceeded), true},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"closed pool", errors.New("sql: database is closed"), true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"no rows", sql.ErrNoRows, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("%s: IsTransient(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
