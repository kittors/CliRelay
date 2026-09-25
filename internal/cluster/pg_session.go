package cluster

import (
	"context"
	"fmt"
	"net"
	"time"
)

// DialKeepAlive is the client-side TCP keepalive for cluster connections: a
// database host that vanishes, or a partition that drops packets silently,
// surfaces within about 30 seconds instead of the platform default.
func DialKeepAlive() net.KeepAliveConfig {
	return net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}
}

// sessionKeepalives make the server notice a vanished client quickly. Locks
// held by a session are released only when PostgreSQL ends that session, and
// with the Linux defaults a host that loses power keeps its session, and the
// leadership with it, for about two hours. tcp_user_timeout also covers the
// case where the server still has unacknowledged data in flight.
var sessionKeepalives = [][2]string{
	{"tcp_keepalives_idle", "10"},
	{"tcp_keepalives_interval", "5"},
	{"tcp_keepalives_count", "3"},
	{"tcp_user_timeout", "20000"},
}

type execFunc func(ctx context.Context, query string, args ...any) error

// tuneSession applies sessionKeepalives and, when appName is set, labels the
// session in pg_stat_activity. Every setting is attempted; the first failure
// is returned (tcp_user_timeout does not exist before PostgreSQL 12).
func tuneSession(ctx context.Context, exec execFunc, appName string) error {
	settings := sessionKeepalives
	if appName != "" {
		settings = append(append([][2]string{}, settings...), [2]string{"application_name", appName})
	}
	var firstErr error
	for _, setting := range settings {
		if err := exec(ctx, `SELECT set_config($1, $2, false)`, setting[0], setting[1]); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("set %s: %w", setting[0], err)
		}
	}
	return firstErr
}
