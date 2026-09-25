package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// busChannel is the LISTEN/NOTIFY channel every node shares.
const busChannel = "clirelay_cluster"

const notifySQL = `SELECT pg_notify('` + busChannel + `', $1)`

// maxNotifyBytes is PostgreSQL's NOTIFY limit: the payload must be shorter
// than 8000 bytes or pg_notify fails.
const maxNotifyBytes = 7999

// pgTimings holds the coordinator's intervals. Tests shorten some of them;
// production always uses defaultPGTimings.
type pgTimings struct {
	// heartbeat is the membership upsert interval. A row older than the
	// active window (20s, in SQL) no longer counts as a live node.
	heartbeat time.Duration
	// campaign is how often a follower tries to take the leadership lock and
	// how often the leader checks its lock session.
	campaign time.Duration
	// leaderPing bounds the leader's session check. A slower answer counts
	// as a lost session: leadership is dropped rather than held blindly.
	leaderPing time.Duration
	// listenIdle is how long the listener waits for a notification before it
	// checks that its session is alive and still on the primary.
	listenIdle time.Duration
	// reconnectMin and reconnectMax bound the listener's reconnect backoff.
	reconnectMin time.Duration
	reconnectMax time.Duration
	// publishTimeout bounds Publish on the shared pool.
	publishTimeout time.Duration
	// step bounds each individual database call made while starting,
	// campaigning or shutting down.
	step time.Duration
}

var defaultPGTimings = pgTimings{
	heartbeat:      5 * time.Second,
	campaign:       2 * time.Second,
	leaderPing:     2 * time.Second,
	listenIdle:     30 * time.Second,
	reconnectMin:   time.Second,
	reconnectMax:   15 * time.Second,
	publishTimeout: 3 * time.Second,
	step:           2 * time.Second,
}

// pgNode is the PostgreSQL backend of one enabled coordinator: membership
// heartbeats, the leadership lock and the bus listener.
type pgNode struct {
	c         *Coordinator
	nodeID    string
	version   string
	advertise string
	dsn       string
	db        *sql.DB
	t         pgTimings

	// startedAt is the database time this process registered its row. It
	// tells this process's row apart from one written by another process
	// that was (mis)configured with the same node ID.
	startedAt time.Time

	kick chan struct{}

	heartbeatStop context.CancelFunc
	heartbeatDone chan struct{}
	leaderStop    context.CancelFunc
	leaderDone    chan struct{}
	listenerStop  context.CancelFunc
	listenerDone  chan struct{}

	// leaderConn is the session holding the leadership lock. Only the leader
	// goroutine touches it once that goroutine runs.
	leaderConn *sql.Conn

	lastHeartbeatWarn time.Time
	lastConflictWarn  time.Time
	shutdownOnce      sync.Once
}

// backendSlot is the coordinator's transport and membership source. A
// coordinator from Prepare is shared before its backend exists, so the
// backend is attached atomically once Start has connected it.
type backendSlot struct {
	node   atomic.Pointer[pgNode]
	mu     sync.Mutex
	closed bool
	nodeID string
}

func newBackendSlot(c *Coordinator) *backendSlot {
	slot := &backendSlot{nodeID: c.nodeID}
	c.transport = slot
	c.nodesProvider = slot.nodes
	return slot
}

func (s *backendSlot) attach(p *pgNode) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.node.Store(p)
	return true
}

func (s *backendSlot) publish(ctx context.Context, ev Event) error {
	if p := s.node.Load(); p != nil {
		return p.publish(ctx, ev)
	}
	return nil
}

func (s *backendSlot) publishTx(ctx context.Context, tx *sql.Tx, ev Event) error {
	if p := s.node.Load(); p != nil {
		return p.publishTx(ctx, tx, ev)
	}
	return nil
}

func (s *backendSlot) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if p := s.node.Load(); p != nil {
		p.shutdown()
	}
}

func (s *backendSlot) nodes(ctx context.Context) ([]NodeStatus, error) {
	if p := s.node.Load(); p != nil {
		return p.nodes(ctx)
	}
	// Not connected yet: report only this node, as a follower.
	now := time.Now()
	return []NodeStatus{{NodeID: s.nodeID, StartedAt: now, LastSeen: now, Active: true, Self: true}}, nil
}

// startPostgres builds the PostgreSQL-backed coordinator: the LISTEN
// connection, membership heartbeats and the leadership lock.
func startPostgres(ctx context.Context, nodeID string, opts Options) (*Coordinator, error) {
	return startPostgresWith(ctx, nodeID, opts, defaultPGTimings)
}

func startPostgresWith(ctx context.Context, nodeID string, opts Options, timings pgTimings) (*Coordinator, error) {
	if opts.DB == nil {
		return nil, errors.New("runtime database is not initialised")
	}
	dsn := strings.TrimSpace(opts.DSN)
	if dsn == "" {
		return nil, errors.New("postgres dsn is required for the bus listener")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c := adoptPrepared(nodeID)
	var slot *backendSlot
	if c != nil {
		slot, _ = c.transport.(*backendSlot)
	}
	if slot == nil {
		c = newCoordinator(true, nodeID)
		slot = newBackendSlot(c)
	}
	p := &pgNode{
		c:         c,
		nodeID:    nodeID,
		version:   strings.TrimSpace(opts.Version),
		advertise: strings.TrimSpace(opts.Advertise),
		dsn:       dsn,
		db:        opts.DB,
		t:         timings,
		kick:      make(chan struct{}, 1),
	}
	if err := p.start(ctx); err != nil {
		p.shutdown()
		return nil, err
	}
	if !slot.attach(p) {
		p.shutdown()
		return nil, errors.New("coordinator was closed while starting")
	}
	p.announceJoin(ctx)
	return c, nil
}

// start registers the node, connects the listener and begins the heartbeat
// and leadership loops. Registration and the listener must work for the node
// to join; leadership is best effort and keeps retrying in the background.
func (p *pgNode) start(ctx context.Context) error {
	stepCtx, cancel := context.WithTimeout(ctx, 3*p.t.step)
	err := p.register(stepCtx)
	cancel()
	if err != nil {
		return err
	}
	p.refreshActiveCount(ctx)

	listenCtx, listenCancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := p.connectListener(listenCtx)
	listenCancel()
	if err != nil {
		return fmt.Errorf("connect bus listener: %w", err)
	}
	// Events published before this point were missed; subscribers that
	// registered during startup reload now.
	p.c.deliver(Event{Resync: true, Origin: p.nodeID})

	loopCtx, stop := context.WithCancel(context.Background())
	p.listenerStop, p.listenerDone = stop, make(chan struct{})
	go p.runListener(loopCtx, conn)

	// One synchronous attempt, so a node that starts alone is leader by the
	// time startup continues.
	p.campaign(ctx)
	loopCtx, stop = context.WithCancel(context.Background())
	p.leaderStop, p.leaderDone = stop, make(chan struct{})
	go p.runLeader(loopCtx)

	loopCtx, stop = context.WithCancel(context.Background())
	p.heartbeatStop, p.heartbeatDone = stop, make(chan struct{})
	go p.runHeartbeat(loopCtx)
	return nil
}

// shutdown leaves the cluster promptly: heartbeats stop, leadership is
// released so a peer can take over within one campaign interval, the row is
// marked stopped and the listener disconnects. It never waits for requests to
// drain; the caller runs it as soon as the process starts shutting down.
func (p *pgNode) shutdown() {
	p.shutdownOnce.Do(func() {
		stopLoop(p.heartbeatStop, p.heartbeatDone)
		stopLoop(p.leaderStop, p.leaderDone)
		// A leader goroutine that never started still owns the session from
		// the synchronous campaign.
		p.resign()
		if !p.startedAt.IsZero() {
			ctx, cancel := context.WithTimeout(context.Background(), p.t.step)
			p.markStopped(ctx)
			cancel()
		}
		stopLoop(p.listenerStop, p.listenerDone)
	})
}

func stopLoop(stop context.CancelFunc, done chan struct{}) {
	if stop == nil {
		return
	}
	stop()
	<-done
}

func (p *pgNode) kickHeartbeat() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *pgNode) publish(ctx context.Context, ev Event) error {
	payload, err := encodeEnvelope(ev)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, p.t.publishTimeout)
	defer cancel()
	if _, err := p.db.ExecContext(ctx, notifySQL, payload); err != nil {
		return fmt.Errorf("cluster: publish %s: %w", ev.Topic, err)
	}
	return nil
}

func (p *pgNode) publishTx(ctx context.Context, tx *sql.Tx, ev Event) error {
	payload, err := encodeEnvelope(ev)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// NOTIFY inside a transaction is queued until COMMIT and dropped on
	// ROLLBACK, which is exactly the delivery guarantee PublishTx promises.
	if _, err := tx.ExecContext(ctx, notifySQL, payload); err != nil {
		return fmt.Errorf("cluster: publish %s in transaction: %w", ev.Topic, err)
	}
	return nil
}

// announceJoin tells peers this node is up. See MembershipJoined.
func (p *pgNode) announceJoin(ctx context.Context) {
	if err := p.c.Publish(ctx, TopicMembership, MembershipEvent{Kind: MembershipJoined, Version: p.version}); err != nil {
		log.Warnf("cluster: announce node %s: %v", p.nodeID, err)
	}
}

func encodeEnvelope(ev Event) (string, error) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return "", fmt.Errorf("cluster: encode %s event: %w", ev.Topic, err)
	}
	if len(raw) > maxNotifyBytes {
		return "", fmt.Errorf("%w: %s envelope is %d bytes", ErrPayloadTooLarge, ev.Topic, len(raw))
	}
	return string(raw), nil
}

func decodeEnvelope(payload string) (Event, error) {
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return Event{}, err
	}
	if ev.Topic == "" {
		return Event{}, errors.New("event without topic")
	}
	return ev, nil
}
