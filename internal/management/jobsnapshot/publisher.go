package jobsnapshot

import (
	"context"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	storeTimeout = 10 * time.Second
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
// is coalesced to at most one write per flush interval.
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
	immediate := first || snap.Terminal || now.Sub(st.lastWrite) >= p.flushGap
	if !immediate && st.timer == nil {
		st.timer = time.AfterFunc(p.flushGap-now.Sub(st.lastWrite), func() { p.flush(snap.ID) })
	}
	p.startBeatsLocked()
	p.mu.Unlock()

	if immediate {
		p.flush(snap.ID)
	}
}

func (p *Publisher) flush(id string) {
	p.mu.Lock()
	st := p.jobs[id]
	if st == nil || st.pending == nil {
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
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	err := p.store.Save(ctx, snap, p.ttl)
	cancel()

	p.mu.Lock()
	defer p.mu.Unlock()
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
		if st.timer == nil && !p.closed {
			st.timer = time.AfterFunc(retryInterval, func() { p.flush(id) })
		}
		return
	}
	st.failures = 0
	if snap.Version > st.written {
		st.written = snap.Version
	}
	if snap.Terminal && st.pending == nil {
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
