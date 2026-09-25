package compatdriver

import (
	"context"
	"database/sql/driver"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// DefaultConnectTimeout bounds each host attempt when the DSN does not set
// connect_timeout. Without it a dial into a partitioned network waits for the
// kernel's SYN retries, and every pool checkout queues behind that dial.
const DefaultConnectTimeout = 5 * time.Second

var dialKeepAlive atomic.Pointer[net.KeepAliveConfig]

// SetDialKeepAlive sets the TCP keepalive used by connections opened after the
// call. Cluster mode tightens it so a dead database host or a silent network
// partition is noticed in seconds rather than after the platform default. A
// config with Enable false restores pgx's default dialer.
func SetDialKeepAlive(cfg net.KeepAliveConfig) {
	if !cfg.Enable {
		dialKeepAlive.Store(nil)
		return
	}
	dialKeepAlive.Store(&cfg)
}

// ParseConnConfig parses dsn the way the pgxq driver connects: pgx parsing,
// which keeps multi-host DSNs and target_session_attrs intact, plus the
// default connect timeout and the process keepalive policy.
func ParseConnConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if !connectTimeoutConfigured(dsn) {
		// pgx applies ConnectTimeout per distinct host, so a multi-host DSN
		// still reaches the next host after one unreachable address.
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	if dialer := keepAliveDialer(cfg.ConnectTimeout); dialer != nil {
		cfg.DialFunc = dialer.DialContext
	}
	return cfg, nil
}

// keepAliveDialer returns the dialer for the configured keepalive policy, or
// nil to keep pgx's default dialer.
func keepAliveDialer(timeout time.Duration) *net.Dialer {
	keepAlive := dialKeepAlive.Load()
	if keepAlive == nil {
		return nil
	}
	return &net.Dialer{Timeout: timeout, KeepAliveConfig: *keepAlive}
}

// connectTimeoutConfigured reports whether the operator chose a connect
// timeout, in the DSN or through libpq's environment variable. An explicit
// value, including 0, is left alone.
func connectTimeoutConfigured(dsn string) bool {
	if _, ok := os.LookupEnv("PGCONNECT_TIMEOUT"); ok {
		return true
	}
	return strings.Contains(dsn, "connect_timeout")
}

// OpenConnector makes database/sql dial through a connector, so a caller's
// context bounds connection setup. The plain driver.Open path ignores it and
// waits up to pgx's fixed 60 seconds.
func (d driverWrapper) OpenConnector(name string) (driver.Connector, error) {
	cfg, err := ParseConnConfig(name)
	if err != nil {
		return nil, err
	}
	return &connector{
		driver: d,
		inner:  stdlib.GetConnector(*cfg),
		state:  &poolState{},
	}, nil
}

type connector struct {
	driver driverWrapper
	inner  driver.Connector
	state  *poolState
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return newWrappedConn(conn, c.state), nil
}

// Driver returns the registered wrapper, so fmt.Sprintf("%T", db.Driver())
// keeps naming this package; SQLite detection elsewhere relies on that.
func (c *connector) Driver() driver.Driver { return c.driver }

var _ driver.DriverContext = driverWrapper{}
var _ driver.Connector = (*connector)(nil)
