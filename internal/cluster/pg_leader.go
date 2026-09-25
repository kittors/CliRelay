package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
)

// Leadership is the session advisory lock LockLeader, held on one dedicated
// connection from the shared pool. PostgreSQL releases it when that session
// ends, which covers a crashed process; tuneSession makes it end quickly when
// the whole host disappears too.
//
// A switchover to a new primary needs no special handling: the new primary
// holds no advisory locks, the old sessions fail their next check, and every
// node campaigns again against whichever server the DSN now reaches.

var errReadOnlyServer = errors.New("session is on a read-only server")

func leaderAppName(nodeID string) string { return "clirelay-leader:" + nodeID }

func (p *pgNode) runLeader(ctx context.Context) {
	defer close(p.leaderDone)
	defer p.resign()
	timer := time.NewTimer(p.t.campaign)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		wait := p.t.campaign
		if p.leaderConn == nil {
			p.campaign(ctx)
		} else if err := p.checkLeadership(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warnf("cluster: node %s gave up leadership: %v", p.nodeID, err)
			p.dropLeadership()
			// Sit out one round so a healthy peer takes over rather than this
			// node, whose session just failed, grabbing the lock straight back.
			wait = 2 * p.t.campaign
		}
		timer.Reset(wait)
	}
}

// campaign tries to take the leadership lock once.
func (p *pgNode) campaign(ctx context.Context) {
	stepCtx, cancel := context.WithTimeout(ctx, p.t.step)
	defer cancel()
	conn, err := p.db.Conn(stepCtx)
	if err != nil {
		log.Debugf("cluster: leadership campaign of node %s: %v", p.nodeID, err)
		return
	}
	var locked bool
	if err := conn.QueryRowContext(stepCtx, `SELECT pg_try_advisory_lock($1)`, LockKey(LockLeader)).Scan(&locked); err != nil {
		// The lock may have been granted before the reply was lost; only
		// ending the session is sure to release it.
		discardConn(conn)
		log.Debugf("cluster: leadership campaign of node %s: %v", p.nodeID, err)
		return
	}
	if !locked {
		// Untouched session, safe to hand back to the pool.
		_ = conn.Close()
		return
	}
	if err := tuneSession(stepCtx, sqlConnExec(conn), leaderAppName(p.nodeID)); err != nil {
		log.Warnf("cluster: tune leadership session of node %s (a dead host may keep leadership longer): %v", p.nodeID, err)
	}
	if err := checkWritableConn(stepCtx, conn); err != nil {
		log.Warnf("cluster: node %s won the leadership lock on an unusable session, releasing it: %v", p.nodeID, err)
		releaseLeaderConn(conn, p.t.step)
		return
	}
	p.leaderConn = conn
	p.c.setLeader(true)
	log.Infof("cluster: node %s is now the leader", p.nodeID)
	p.kickHeartbeat()
}

// checkLeadership verifies the lock session is alive and still on a
// writable primary. The lock is only as good as the session: once the
// session is gone the server has released it, and a peer may hold it next.
func (p *pgNode) checkLeadership(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, p.t.leaderPing)
	defer cancel()
	return checkWritableConn(checkCtx, p.leaderConn)
}

// dropLeadership stops acting as leader first, then ends the session so the
// server releases the lock no matter what state the connection is in.
func (p *pgNode) dropLeadership() {
	p.c.setLeader(false)
	discardConn(p.leaderConn)
	p.leaderConn = nil
	p.kickHeartbeat()
}

// resign releases leadership on shutdown: stop acting as leader, unlock,
// then close the session.
func (p *pgNode) resign() {
	if p.leaderConn == nil {
		return
	}
	p.c.setLeader(false)
	releaseLeaderConn(p.leaderConn, p.t.step)
	p.leaderConn = nil
	log.Infof("cluster: node %s released leadership", p.nodeID)
}

func releaseLeaderConn(conn *sql.Conn, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, LockKey(LockLeader))
	discardConn(conn)
}

// discardConn closes the physical connection behind conn instead of
// returning it to the pool. Lock sessions carry a session lock and tuned
// settings, and neither may leak into a connection other code reuses.
func discardConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func checkWritableConn(ctx context.Context, conn *sql.Conn) error {
	var readOnly string
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('transaction_read_only')`).Scan(&readOnly); err != nil {
		return err
	}
	if readOnly != "off" {
		return errReadOnlyServer
	}
	return nil
}

func sqlConnExec(conn *sql.Conn) execFunc {
	return func(ctx context.Context, query string, args ...any) error {
		_, err := conn.ExecContext(ctx, query, args...)
		return err
	}
}
