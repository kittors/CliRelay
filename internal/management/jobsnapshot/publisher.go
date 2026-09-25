package jobsnapshot

import (
	"context"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// syncStoreTimeout bounds the writes made on the caller's goroutine: the
	// first snapshot is written inside the request that created the job, and
	// a database outage must not hold that request for long.
	syncStoreTimeout = 3 * time.Second
	storeTimeout     = 10 * time.Second
	// retryInterval and maxRetries bound how long a failed write is retried.
	// A final snapshot that never lands would leave other nodes reporting the
	// job as running until its heartbeat ages out.
	retryInterval = 5 * time.Second
	maxRetries    = 12
)

// Publisher writes the snapshots of the jobs one service runs on this node
// and reads snapshots of jobs run elsewhere.
type Publisher struct {
	store Store
	kind  string
	ttl   time.Duration
	owner func() string

	mu        sync.Mutex
	flushGap  time.Duration
	beatGap   time.Duration
	jobs      map[string]*published
	beating   bool
	closed    bool
	stopBeats chan struct{}
}

type published struct {
	pending     *Snapshot
	written     int64
	lastWrite   time.Time
	lastPublish time.Time
	timer       *time.Timer
	failures    int
	// writing counts saves in progress. Background flushes wait for them
	// instead of piling up while the database is slow.
	writing int
}

// NewPublisher returns a publisher for jobs of kind kept for ttl after their
// last update. owner names this node.
func NewPublisher(store Store, kind string, ttl time.Duration, owner func() string) *Publisher {
	if owner == nil {
		owner = func() string { return "" }
	}
	return &Publisher{
		store:     store,
		kind:      strings.TrimSpace(kind),
		ttl:       ttl,
		owner:     owner,
		flushGap:  FlushInterval,
		beatGap:   HeartbeatInterval,
		jobs:      make(map[string]*published),
		stopBeats: make(chan struct{}),
	}
}

// SetIntervals changes the progress flush and heartbeat intervals.
func (p *Publisher) SetIntervals(flush, heartbeat time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if flush > 0 {
		p.flushGap = flush
	}
	if heartbeat > 0 {
		p.beatGap = heartbeat
	}
}

// Kind returns the job kind this publisher writes.
func (p *Publisher) Kind() string { return p.kind }

// Publish records snap as the latest state of its job. The first snapshot of
// a job and a terminal one are written before Publish returns, so a job is
// visible to every node by the time its id is handed to a client, and its
// outcome is written by the time the job goroutine ends. Progress in between
// is written in the background, at most once per flush interval, so a slow
// database never stalls the job itself.
func (p *Publisher) Publish(snap Snapshot) {
	if p == nil || strings.TrimSpace(snap.ID) == "" {
		return
	}
	snap.Kind = p.kind
	snap.OwnerNode = p.owner()
	now := time.Now()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	st := p.jobs[snap.ID]
	first := st == nil
	if first {
		st = &published{}
		p.jobs[snap.ID] = st
	}
	if snap.Version <= st.written || (st.pending != nil && snap.Version <= st.pending.Version) {
		p.mu.Unlock()
		return
	}
	st.pending = &snap
	st.lastPublish = now
	immediate := first || snap.Terminal
	if !immediate {
		p.scheduleLocked(snap.ID, st, p.flushGap-now.Sub(st.lastWrite))
	}
	p.startBeatsLocked()
	p.mu.Unlock()

	if immediate {
		p.write(snap.ID, syncStoreTimeout, true)
	}
}

// scheduleLocked arranges a background write of the pending snapshot after
// delay, unless one is already scheduled or a write is still in progress;
// that write reschedules when it ends.
func (p *Publisher) scheduleLocked(id string, st *published, delay time.Duration) {
	if st.timer != nil || st.writing > 0 || p.closed {
		return
	}
	if delay < 0 {
		delay = 0
	}
	st.timer = time.AfterFunc(delay, func() { p.flush(id) })
}

func (p *Publisher) flush(id string) { p.write(id, storeTimeout, false) }

// write saves the pending snapshot of job id. A forced write (the first and
// the final snapshot) goes out even while a background write is in progress;
// the version guard in the store orders them.
func (p *Publisher) write(id string, timeout time.Duration, force bool) {
	p.mu.Lock()
	st := p.jobs[id]
	if st == nil || st.pending == nil || (!force && st.writing > 0) {
		p.mu.Unlock()
		return
	}
	snap := *st.pending
	st.pending = nil
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.lastWrite = time.Now()
	st.writing++
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := p.store.Save(ctx, snap, p.ttl)
	cancel()

	p.mu.Lock()
	defer p.mu.Unlock()
	st.writing--
	if p.jobs[id] != st {
		return
	}
	if err != nil {
		st.failures++
		if st.failures > maxRetries {
			log.WithError(err).WithField("job_id", id).Warnf("jobsnapshot: giving up sharing %s job state", p.kind)
			delete(p.jobs, id)
			return
		}
		log.WithError(err).WithField("job_id", id).Debugf("jobsnapshot: sharing %s job state failed; retrying", p.kind)
		if st.pending == nil || st.pending.Version < snap.Version {
			st.pending = &snap
		}
		p.scheduleLocked(id, st, retryInterval)
		return
	}
	st.failures = 0
	if snap.Version > st.written {
		st.written = snap.Version
	}
	if st.pending != nil {
		// Progress published while this write was in flight.
		p.scheduleLocked(id, st, p.flushGap-time.Since(st.lastWrite))
		return
	}
	if snap.Terminal {
		delete(p.jobs, id)
	}
}

// Lookup reads the shared snapshot of job id.
func (p *Publisher) Lookup(ctx context.Context, id string) (Snapshot, bool, error) {
	if p == nil {
		return Snapshot{}, false, nil
	}
	return p.store.Get(ctx, p.kind, strings.TrimSpace(id))
}

// startBeatsLocked runs the heartbeat loop while there are unfinished jobs.
func (p *Publisher) startBeatsLocked() {
	if p.beating || p.closed {
		return
	}
	p.beating = true
	go p.beatLoop()
}

func (p *Publisher) beatLoop() {
	p.mu.Lock()
	ticker := time.NewTicker(p.beatGap)
	p.mu.Unlock()
	defer ticker.Stop()
	for {
		select {
		case <-p.stopBeats:
			return
		case <-ticker.C:
		}
		ids := p.runningIDs()
		if len(ids) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		if err := p.store.Touch(ctx, p.owner(), ids); err != nil {
			log.WithError(err).Debugf("jobsnapshot: %s job heartbeat failed", p.kind)
		}
		cancel()
	}
}

// runningIDs lists the jobs to heartbeat and ends the loop when there are
// none. Entries whose job has not reported for a whole retention window are
// dropped: their rows are gone and the job is not coming back.
func (p *Publisher) runningIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-p.ttl)
	ids := make([]string, 0, len(p.jobs))
	for id, st := range p.jobs {
		if p.ttl > 0 && st.lastPublish.Before(cutoff) {
			if st.timer != nil {
				st.timer.Stop()
			}
			delete(p.jobs, id)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		p.beating = false
	}
	return ids
}

// Close stops heartbeats and pending writes. Unfinished jobs then read as
// lost on the other nodes once their heartbeat ages out.
func (p *Publisher) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	for _, st := range p.jobs {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
	}
	close(p.stopBeats)
}
