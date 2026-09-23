package compatdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
)

const DriverName = "pgxq"

func init() {
	sql.Register(DriverName, driverWrapper{inner: stdlib.GetDefaultDriver()})
}

type driverWrapper struct {
	inner driver.Driver
}

func (d driverWrapper) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return wrappedConn{Conn: conn}, nil
}

type wrappedConn struct {
	driver.Conn
}

func (c wrappedConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(rewriteSQL(query))
}

func (c wrappedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if inner, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return inner.PrepareContext(ctx, rewriteSQL(query))
	}
	return nil, driver.ErrSkip
}

func (c wrappedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if inner, ok := c.Conn.(driver.ExecerContext); ok {
		return inner.ExecContext(ctx, rewriteSQL(query), args)
	}
	return nil, driver.ErrSkip
}

func (c wrappedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if inner, ok := c.Conn.(driver.QueryerContext); ok {
		return inner.QueryContext(ctx, rewriteSQL(query), args)
	}
	return nil, driver.ErrSkip
}

func (c wrappedConn) Ping(ctx context.Context) error {
	if inner, ok := c.Conn.(driver.Pinger); ok {
		return inner.Ping(ctx)
	}
	return nil
}

func (c wrappedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if inner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return inner.BeginTx(ctx, opts)
	}
	return nil, driver.ErrSkip
}

func (c wrappedConn) ResetSession(ctx context.Context) error {
	if inner, ok := c.Conn.(driver.SessionResetter); ok {
		return inner.ResetSession(ctx)
	}
	return nil
}

func (c wrappedConn) IsValid() bool {
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
var _ driver.Conn = wrappedConn{}
var _ driver.ConnPrepareContext = wrappedConn{}
var _ driver.ExecerContext = wrappedConn{}
var _ driver.QueryerContext = wrappedConn{}
var _ driver.Pinger = wrappedConn{}
var _ driver.ConnBeginTx = wrappedConn{}
var _ driver.SessionResetter = wrappedConn{}
var _ driver.Validator = wrappedConn{}
var _ io.Closer = wrappedConn{}
