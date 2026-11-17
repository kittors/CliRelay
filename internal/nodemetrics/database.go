package nodemetrics

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// The database part of the snapshot describes the PostgreSQL server the
// nodes share: connections, size, WAL, replication slots and connections,
// long transactions, lock waits, transaction and cache rates, transaction ID
// age. Only the leader samples it (a single node is its own leader), so the
// server is queried once per interval however many nodes there are and
// however many consoles are open; the leader's snapshot carries the result.
//
// Sampling runs on its own timer, not in the snapshot loop: a slow or stuck
// database must not hold back the node's own figures. The queries read
// statistics views only. Some need more than ordinary rights: pg_ls_waldir()
// needs superuser or pg_monitor, and pg_stat_activity hides the state of
// other roles' sessions from a role without pg_read_all_stats. The parts a
// role cannot read are left out; the rest is still reported.
//
// The host's free disk space is not here: SQL cannot see it. The database
// host reports its machine through cmd/clirelay-hostagent instead.

const (
	// DefaultDatabaseInterval is how often the leader samples the server.
	// Its statistics views are cheap, but there is no need to look more
	// often than an operator would notice a change.
	DefaultDatabaseInterval = 30 * time.Second
	// databaseQueryTimeout bounds one round. A server that cannot answer
	// statistics queries in this time is reported as timing out.
	databaseQueryTimeout = 5 * time.Second
	// databaseWindowSpan is the window of the rates (commits, cache hits,
	// deadlocks): long enough that a cache hit ratio is not one burst.
	databaseWindowSpan = 5 * time.Minute
	// longTransactionAge is the age from which a transaction counts as long:
	// it holds back vacuum, so dead rows pile up behind it.
	longTransactionAge = 5 * time.Minute
)

// Database is one sample of the PostgreSQL server.
type Database struct {
	CollectedAt time.Time `json:"collected_at"`
	// Error is "timeout" or "error" when the latest round failed. The
	// figures below then come from the last round that worked, taken at
	// LastOKAt; with no such round they are empty. The cause stays out, as
	// for the round trips: driver errors name hosts and roles.
	Error    string     `json:"error,omitempty"`
	LastOKAt *time.Time `json:"last_ok_at,omitempty"`

	// Engine is "postgres".
	Engine string `json:"engine"`
	// Version is server_version without the packaging suffix, e.g. "15.19".
	Version   string     `json:"version,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	// InRecovery is true on a standby, which cannot take writes.
	InRecovery bool `json:"in_recovery"`
	// SizeBytes is pg_database_size of the runtime database.
	SizeBytes int64 `json:"size_bytes"`
	// XIDAge is the oldest unfrozen transaction ID age of any database,
	// max(age(datfrozenxid)). Autovacuum forces a freeze from
	// autovacuum_freeze_max_age (200 million by default); near 2 billion the
	// server stops taking writes to avoid wraparound.
	XIDAge int64 `json:"xid_age"`

	Connections *DatabaseConnections `json:"connections,omitempty"`
	Activity    *DatabaseActivity    `json:"activity,omitempty"`
	WAL         *DatabaseWAL         `json:"wal,omitempty"`
	Slots       []DatabaseSlot       `json:"slots,omitempty"`
	Replication []DatabaseReplica    `json:"replication,omitempty"`
	// Window holds the counters' change over about the last five minutes,
	// left out until the leader has two rounds.
	Window *DatabaseWindow `json:"window,omitempty"`
}

// DatabaseConnections counts the server's sessions.
type DatabaseConnections struct {
	// Max is max_connections; Reserved is superuser_reserved_connections,
	// the part of Max only superusers may use.
	Max      int `json:"max"`
	Reserved int `json:"reserved"`
	// Client counts client backends: the nodes' pools, tools such as psql,
	// and this probe's own connection.
	Client            int `json:"client"`
	Active            int `json:"active"`
	Idle              int `json:"idle"`
	IdleInTransaction int `json:"idle_in_transaction"`
	// WALSenders counts replication connections (pg_receivewal, standbys).
	// Since PostgreSQL 12 they have their own slots (max_wal_senders) and
	// do not take from Max.
	WALSenders int `json:"wal_senders"`
}

// DatabaseActivity is what the sessions are stuck on.
type DatabaseActivity struct {
	// LongestTransactionSeconds is the age of the oldest open transaction of
	// a client backend, 0 with none open.
	LongestTransactionSeconds float64 `json:"longest_transaction_seconds"`
	// LongTransactions counts client transactions open longer than five
	// minutes.
	LongTransactions int `json:"long_transactions"`
	// LongestIdleInTransactionSeconds is how long the oldest "idle in
	// transaction" session has said nothing: a client that began a
	// transaction and stopped, holding its locks and the vacuum horizon.
	LongestIdleInTransactionSeconds float64 `json:"longest_idle_in_transaction_seconds"`
	// LockWaits counts sessions waiting for a lock now.
	// LongestLockWaitSeconds is how long the longest-running of their
	// statements has run: PostgreSQL does not record when the wait began.
	LockWaits              int     `json:"lock_waits"`
	LongestLockWaitSeconds float64 `json:"longest_lock_wait_seconds"`
}

// DatabaseWAL is the write-ahead log directory and its limits.
type DatabaseWAL struct {
	// Bytes and Files are the size and number of files of pg_wal, from
	// pg_ls_waldir(); left out for a role that may not call it.
	Bytes *int64 `json:"bytes,omitempty"`
	Files *int   `json:"files,omitempty"`
	// MaxWALSizeBytes is max_wal_size: between checkpoints pg_wal stays
	// around it unless a replication slot holds more.
	MaxWALSizeBytes *int64 `json:"max_wal_size_bytes,omitempty"`
	// MaxSlotKeepBytes is max_slot_wal_keep_size (PostgreSQL 13+), the most
	// WAL a slot may hold; -1 means no limit, so a stalled slot can fill the
	// disk.
	MaxSlotKeepBytes *int64 `json:"max_slot_keep_bytes,omitempty"`
}

// DatabaseSlot is one replication slot.
type DatabaseSlot struct {
	Name string `json:"name"`
	// Type is "physical" or "logical".
	Type   string `json:"type"`
	Active bool   `json:"active"`
	// WALStatus (PostgreSQL 13+) is "reserved" or "extended" while the WAL
	// the slot needs is kept, "unreserved" when it is about to be removed,
	// "lost" when it was and the slot is useless.
	WALStatus string `json:"wal_status,omitempty"`
	// RetainedBytes is the WAL kept for the slot, from its restart_lsn to the
	// current write position; left out on a standby and for a slot not used
	// yet.
	RetainedBytes *int64 `json:"retained_bytes,omitempty"`
	// SafeWALSizeBytes (13+) is how much more WAL can be written before the
	// slot is lost; left out when max_slot_wal_keep_size is -1.
	SafeWALSizeBytes *int64 `json:"safe_wal_size_bytes,omitempty"`
}

// DatabaseReplica is one replication connection (pg_stat_replication).
type DatabaseReplica struct {
	// Name is the client's application_name, e.g. "pg_receivewal". Its
	// address stays out of the snapshot.
	Name string `json:"name"`
	// State is "streaming" in normal operation; "startup", "catchup",
	// "backup" and "stopping" are transitions.
	State     string `json:"state"`
	SyncState string `json:"sync_state,omitempty"`
	// SentLagBytes is WAL written but not yet sent to the client;
	// FlushLagBytes WAL it has not yet confirmed on disk. pg_receivewal
	// confirms at segment ends unless run with --synchronous, so its flush
	// lag reaches a segment (16 MB) in normal operation.
	SentLagBytes  *int64 `json:"sent_lag_bytes,omitempty"`
	FlushLagBytes *int64 `json:"flush_lag_bytes,omitempty"`
	// ReplyAgeSeconds (PostgreSQL 12+) is how long ago the client last
	// reported its position: a stuck client keeps its connection and stops
	// replying.
	ReplyAgeSeconds *float64 `json:"reply_age_seconds,omitempty"`
}

// DatabaseWindow is the change of the runtime database's counters
// (pg_stat_database) over the window; the console turns it into rates and a
// cache hit ratio.
type DatabaseWindow struct {
	Seconds    float64 `json:"seconds"`
	Commits    uint64  `json:"commits"`
	Rollbacks  uint64  `json:"rollbacks"`
	BlocksHit  uint64  `json:"blocks_hit"`
	BlocksRead uint64  `json:"blocks_read"`
	Deadlocks  uint64  `json:"deadlocks"`
	TempBytes  uint64  `json:"temp_bytes"`
}

// databaseCounters is one reading of pg_stat_database. reset is stats_reset:
// counters from different resets cannot be subtracted.
type databaseCounters struct {
	at                                      time.Time
	reset                                   time.Time
	commits, rollbacks, hit, read, deadlock uint64
	tempBytes                               uint64
}

// DatabaseProbe samples the PostgreSQL server while this node leads.
type DatabaseProbe struct {
	interval time.Duration
	now      func() time.Time

	sourcesMu sync.RWMutex
	db        func() *sql.DB
	isLeader  func() bool

	// roundMu guards the state below; the loop holds it for a round, as do
	// tests calling round directly.
	roundMu sync.Mutex
	history []databaseCounters
	// lastOK is the last round that worked, shown with the error of a
	// failed one.
	lastOK *Database

	latest atomic.Pointer[Database]

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// DatabaseProbeOptions configures a probe. Zero values take the defaults.
type DatabaseProbeOptions struct {
	Interval time.Duration
	Now      func() time.Time
}

var defaultDatabaseProbe = NewDatabaseProbe(DatabaseProbeOptions{})

// DefaultDatabaseProbe is the process-wide probe the API server runs and the
// snapshot reads.
func DefaultDatabaseProbe() *DatabaseProbe { return defaultDatabaseProbe }

// NewDatabaseProbe builds a probe; it samples nothing until Start, or a
// direct call in tests, and nothing at all until SetSources gives it a
// database.
func NewDatabaseProbe(opts DatabaseProbeOptions) *DatabaseProbe {
	if opts.Interval <= 0 {
		opts.Interval = DefaultDatabaseInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &DatabaseProbe{interval: opts.Interval, now: opts.Now}
}

// SetSources tells the probe where the database is and whether this node
// leads. db returns nil while the runtime database is not PostgreSQL; a nil
// isLeader counts as leading.
func (p *DatabaseProbe) SetSources(db func() *sql.DB, isLeader func() bool) {
	p.sourcesMu.Lock()
	p.db, p.isLeader = db, isLeader
	p.sourcesMu.Unlock()
}

func (p *DatabaseProbe) sources() (func() *sql.DB, func() bool) {
	p.sourcesMu.RLock()
	defer p.sourcesMu.RUnlock()
	return p.db, p.isLeader
}

func (p *DatabaseProbe) leading() bool {
	_, isLeader := p.sources()
	return isLeader == nil || isLeader()
}

// Latest returns the newest sample while this node leads, nil otherwise:
// after losing the lead the node's snapshot drops the database part, and the
// new leader's takes over.
func (p *DatabaseProbe) Latest() *Database {
	if !p.leading() {
		return nil
	}
	return p.latest.Load()
}

// Start samples at once and then every interval until Stop. Calling it again
// while it runs does nothing.
func (p *DatabaseProbe) Start() {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			p.round(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop ends sampling and waits for a round in progress, which its timeout
// bounds.
func (p *DatabaseProbe) Stop() {
	p.runMu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.runMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// round samples the server once if this node leads, and publishes the
// result.
func (p *DatabaseProbe) round(ctx context.Context) {
	p.roundMu.Lock()
	defer p.roundMu.Unlock()

	dbFunc, _ := p.sources()
	var db *sql.DB
	if dbFunc != nil {
		db = dbFunc()
	}
	if db == nil || !p.leading() {
		// Not PostgreSQL, or not leading: forget the state, so a later lead
		// starts a fresh window instead of spanning the time it did not look.
		p.history, p.lastOK = nil, nil
		p.latest.Store(nil)
		return
	}

	now := p.now()
	queryCtx, cancel := context.WithTimeout(ctx, databaseQueryTimeout)
	defer cancel()
	sample, counters, err := queryDatabase(queryCtx, db)
	if ctx.Err() != nil {
		// Stopped mid-round: the cut queries would read as a timeout.
		return
	}
	if err != nil {
		p.latest.Store(p.failed(now, err, queryCtx))
		return
	}
	sample.CollectedAt = now.UTC()
	sample.Engine = "postgres"
	if counters != nil {
		counters.at = now
		sample.Window = p.window(*counters)
	}
	p.lastOK = sample
	p.latest.Store(sample)
}

// failed is the sample of a failed round: the last good figures, if any,
// marked with the error.
func (p *DatabaseProbe) failed(now time.Time, err error, queryCtx context.Context) *Database {
	out := &Database{Engine: "postgres"}
	if p.lastOK != nil {
		copied := *p.lastOK
		out = &copied
		lastOK := p.lastOK.CollectedAt
		out.LastOKAt = &lastOK
	}
	out.CollectedAt = now.UTC()
	out.Error = "error"
	if errors.Is(err, context.DeadlineExceeded) || queryCtx.Err() != nil {
		out.Error = "timeout"
	}
	return out
}

// window adds cur to the counter history and returns the change since the
// start of the window. Counters that went backwards or were reset start a
// new history.
func (p *DatabaseProbe) window(cur databaseCounters) *DatabaseWindow {
	if n := len(p.history); n > 0 {
		last := p.history[n-1]
		if !cur.reset.Equal(last.reset) || cur.commits < last.commits || cur.rollbacks < last.rollbacks ||
			cur.hit < last.hit || cur.read < last.read || cur.deadlock < last.deadlock || cur.tempBytes < last.tempBytes {
			p.history = nil
		}
	}
	var start databaseCounters
	var ok bool
	p.history, start, ok = slide(p.history, cur, func(s databaseCounters) time.Time { return s.at }, databaseWindowSpan)
	if !ok {
		return nil
	}
	elapsed := cur.at.Sub(start.at)
	if elapsed <= 0 {
		return nil
	}
	return &DatabaseWindow{
		Seconds:    round3(elapsed.Seconds()),
		Commits:    cur.commits - start.commits,
		Rollbacks:  cur.rollbacks - start.rollbacks,
		BlocksHit:  cur.hit - start.hit,
		BlocksRead: cur.read - start.read,
		Deadlocks:  cur.deadlock - start.deadlock,
		TempBytes:  cur.tempBytes - start.tempBytes,
	}
}
