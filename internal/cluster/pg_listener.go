package cluster

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
	log "github.com/sirupsen/logrus"
)

// The bus listener owns one connection opened straight from the DSN. It must
// not come from the shared pool: a pooled connection gets recycled, and the
// LISTEN registration silently goes with it. NOTIFY is not replicated to
// standbys either, so the listener has to sit on the primary; a multi-host
// DSN with target_session_attrs=read-write keeps it there across failovers.

func listenerAppName(nodeID string) string { return "clirelay-listener:" + nodeID }

func (p *pgNode) connectListener(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := compatdriver.ParseConnConfig(p.dsn)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["application_name"] = listenerAppName(p.nodeID)
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAliveConfig: DialKeepAlive()}
	cfg.DialFunc = dialer.DialContext
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := tuneSession(ctx, pgxConnExec(conn), ""); err != nil {
		log.Warnf("cluster: tune bus listener session of node %s: %v", p.nodeID, err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+busChannel); err != nil {
		closeListener(conn)
		return nil, fmt.Errorf("listen on %s: %w", busChannel, err)
	}
	return conn, nil
}

// runListener delivers notifications until ctx ends, reconnecting with
// backoff. Every successful reconnect is followed by a Resync, because
// whatever was published while the listener was away is gone for good.
func (p *pgNode) runListener(ctx context.Context, conn *pgx.Conn) {
	defer close(p.listenerDone)
	backoff := p.t.reconnectMin
	for {
		if conn != nil {
			err := p.listen(ctx, conn)
			closeListener(conn)
			conn = nil
			if ctx.Err() != nil {
				return
			}
			log.Warnf("cluster: bus listener of node %s disconnected: %v", p.nodeID, err)
		}
		if !sleepContext(ctx, backoff) {
			return
		}
		connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		next, err := p.connectListener(connectCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			backoff = nextBackoff(backoff, p.t.reconnectMax)
			log.Warnf("cluster: reconnect bus listener of node %s: %v (next attempt in %s)", p.nodeID, err, backoff)
			continue
		}
		backoff = p.t.reconnectMin
		conn = next
		log.Infof("cluster: bus listener of node %s reconnected; resyncing", p.nodeID)
		p.c.deliver(Event{Resync: true, Origin: p.nodeID})
	}
}

// listen waits for notifications on conn. When nothing arrives for a while it
// checks the session instead of trusting it: after a silent partition, or a
// switchover that left this session on a read-only server, the connection
// would otherwise wait forever while every event goes elsewhere.
func (p *pgNode) listen(ctx context.Context, conn *pgx.Conn) error {
	for {
		waitCtx, cancel := context.WithTimeout(ctx, p.t.listenIdle)
		notification, err := conn.WaitForNotification(waitCtx)
		cancel()
		if err == nil {
			p.dispatch(notification)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A timed out wait leaves the connection usable; anything else ends it.
		if !pgconn.Timeout(err) || conn.IsClosed() {
			return err
		}
		if err := p.checkListener(ctx, conn); err != nil {
			return err
		}
	}
}

func (p *pgNode) checkListener(ctx context.Context, conn *pgx.Conn) error {
	checkCtx, cancel := context.WithTimeout(ctx, p.t.step)
	defer cancel()
	var readOnly string
	if err := conn.QueryRow(checkCtx, `SELECT current_setting('transaction_read_only')`).Scan(&readOnly); err != nil {
		return fmt.Errorf("check listener session: %w", err)
	}
	if readOnly != "off" {
		return errReadOnlyServer
	}
	return nil
}

func (p *pgNode) dispatch(notification *pgconn.Notification) {
	if notification == nil || notification.Channel != busChannel {
		return
	}
	ev, err := decodeEnvelope(notification.Payload)
	if err != nil {
		log.Debugf("cluster: ignore malformed bus message: %v", err)
		return
	}
	if ev.Topic == TopicMembership && ev.Origin != p.nodeID {
		var membership MembershipEvent
		if ev.Decode(&membership) == nil && membership.Kind == MembershipJoined {
			p.c.deliver(Event{Resync: true, Origin: ev.Origin})
		}
	}
	p.c.deliver(ev)
}

func closeListener(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

func nextBackoff(current, limit time.Duration) time.Duration {
	next := current * 2
	if next > limit {
		return limit
	}
	return next
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func pgxConnExec(conn *pgx.Conn) execFunc {
	return func(ctx context.Context, query string, args ...any) error {
		_, err := conn.Exec(ctx, query, args...)
		return err
	}
}
