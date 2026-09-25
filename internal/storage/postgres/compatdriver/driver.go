package compatdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const DriverName = "pgxq"

func init() {
	sql.Register(DriverName, driverWrapper{})
}

// driverWrapper wraps pgx's database/sql driver: it rewrites the SQLite
// dialect still used by shared queries, hardens connection setup and drops
// connections that point at a server which can no longer take writes.
type driverWrapper struct{}

// Open serves callers that use the driver directly; database/sql goes through
// OpenConnector. The deadline matches the one pgx's own Open applies.
func (d driverWrapper) Open(name string) (driver.Conn, error) {
	connector, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return connector.Connect(ctx)
}

type wrappedConn struct {
	driver.Conn

	// pool and epoch tie the connection to its connector, see poolState.
	pool  *poolState
	epoch uint64
	// broken is set once an error showed the connection must not be reused.
	broken atomic.Bool
	// readOnlyTx is true while a transaction begun with ReadOnly is open.
	// database/sql never uses one connection concurrently, so it needs no lock.
	readOnlyTx bool
}

func newWrappedConn(conn driver.Conn, pool *poolState) *wrappedConn {
	c := &wrappedConn{Conn: conn, pool: pool}
	if pool != nil {
		c.epoch = pool.epoch.Load()
	}
	return c
}

func (c *wrappedConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(rewriteSQL(query))
	if err != nil {
		return nil, c.noteError(err, false)
	}
	return &wrappedStmt{Stmt: stmt, conn: c}, nil
}

func (c *wrappedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	inner, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	stmt, err := inner.PrepareContext(ctx, rewriteSQL(query))
	if err != nil {
		return nil, c.noteError(err, false)
	}
	return &wrappedStmt{Stmt: stmt, conn: c}, nil
}

func (c *wrappedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	inner, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := inner.ExecContext(ctx, rewriteSQL(query), args)
	return result, c.noteError(err, true)
}

func (c *wrappedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	inner, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := inner.QueryContext(ctx, rewriteSQL(query), args)
	return rows, c.noteError(err, true)
}

func (c *wrappedConn) Ping(ctx context.Context) error {
	if inner, ok := c.Conn.(driver.Pinger); ok {
		return inner.Ping(ctx)
	}
	return nil
}

func (c *wrappedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	inner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, driver.ErrSkip
	}
	tx, err := inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, c.noteError(err, false)
	}
	c.readOnlyTx = opts.ReadOnly
	return &wrappedTx{Tx: tx, conn: c}, nil
}

// ResetSession runs before database/sql hands a pooled connection out again.
// Returning driver.ErrBadConn makes it close the connection and pick another.
func (c *wrappedConn) ResetSession(ctx context.Context) error {
	if c.unusable() {
		return driver.ErrBadConn
	}
	if inner, ok := c.Conn.(driver.SessionResetter); ok {
		return inner.ResetSession(ctx)
	}
	return nil
}

// IsValid runs when a connection is returned to the pool; false closes it.
// pgx's connection does not implement it, so a closed socket used to linger
// in the pool until the next checkout noticed.
func (c *wrappedConn) IsValid() bool {
	if c.unusable() || c.innerClosed() {
		return false
	}
	if inner, ok := c.Conn.(driver.Validator); ok {
		return inner.IsValid()
	}
	return true
}

var sqliteDDLRewrites = []struct {
	old string
	new string
}{
	{"INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY"},
	{"INTEGER PRIMARY KEY", "BIGINT PRIMARY KEY"},
	{"DATETIME", "TIMESTAMPTZ"},
	{"BLOB", "BYTEA"},
	{"REAL", "DOUBLE PRECISION"},
	{"X''", "decode('', 'hex')"},
	// SQLite 'localtime' modifiers are deliberately not translated. They follow
	// the process TZ and PostgreSQL has no equivalent; rewriting them to UTC
	// silently shifted every chart on non-UTC deployments. Callers bucket
	// against edges computed from the usage timezone instead.
}

var pragmaRE = regexp.MustCompile(`(?is)^\s*PRAGMA\b.*$`)
var alterAddColumnRE = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+\S+\s+ADD\s+COLUMN\s+`)

func rewriteSQL(query string) string {
	if pragmaRE.MatchString(strings.TrimSpace(query)) {
		return "SELECT 1"
	}
	for _, replacement := range sqliteDDLRewrites {
		query = strings.ReplaceAll(query, replacement.old, replacement.new)
	}
	query = rewriteAlterAddColumn(query)
	query = rewriteInsertOrIgnore(query)
	return rewritePlaceholders(query)
}

func rewriteAlterAddColumn(query string) string {
	matches := alterAddColumnRE.FindAllStringIndex(query, -1)
	if len(matches) == 0 {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + len(matches)*14)
	last := 0
	for _, match := range matches {
		b.WriteString(query[last:match[1]])
		rest := strings.TrimLeft(query[match[1]:], " \t\r\n")
		if !strings.HasPrefix(strings.ToUpper(rest), "IF NOT EXISTS") {
			b.WriteString("IF NOT EXISTS ")
		}
		last = match[1]
	}
	b.WriteString(query[last:])
	return b.String()
}

func rewriteInsertOrIgnore(query string) string {
	if !strings.Contains(strings.ToUpper(query), "INSERT OR IGNORE INTO") {
		return query
	}
	rewritten := strings.Replace(query, "INSERT OR IGNORE INTO", "INSERT INTO", 1)
	if strings.Contains(strings.ToUpper(rewritten), " ON CONFLICT") {
		return rewritten
	}
	return rewritten + " ON CONFLICT DO NOTHING"
}

func rewritePlaceholders(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	placeholder := 1
	inSingle := false
	inDouble := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			b.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			b.WriteByte(ch)
			continue
		}
		if ch == '?' && !inSingle && !inDouble {
			b.WriteByte('$')
			b.WriteString(intString(placeholder))
			placeholder++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func intString(n int) string {
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

var _ driver.Driver = driverWrapper{}
var _ driver.Conn = (*wrappedConn)(nil)
var _ driver.ConnPrepareContext = (*wrappedConn)(nil)
var _ driver.ExecerContext = (*wrappedConn)(nil)
var _ driver.QueryerContext = (*wrappedConn)(nil)
var _ driver.Pinger = (*wrappedConn)(nil)
var _ driver.ConnBeginTx = (*wrappedConn)(nil)
var _ driver.SessionResetter = (*wrappedConn)(nil)
var _ driver.Validator = (*wrappedConn)(nil)
var _ io.Closer = (*wrappedConn)(nil)
