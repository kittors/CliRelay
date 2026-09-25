package clusterauth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

const (
	// syncRetryDelay spaces re-reads of a row whose announced version is not
	// visible yet (a transport that delivers before the commit).
	syncRetryDelay = 250 * time.Millisecond
	// syncMaxAttempts bounds those re-reads; the reconcile loop catches up
	// with anything that gives up.
	syncMaxAttempts = 20
)

type syncRequest struct {
	version  int64
	attempts int
}

// onEvent receives credential changes from other nodes. It runs on the bus
// goroutine, so it only queues work for the sync loop.
func (s *Store) onEvent(ev cluster.Event) {
	if ev.Resync {
		s.syncMu.Lock()
		s.resync = true
		s.syncMu.Unlock()
		signal(s.syncSignal)
		return
	}
	var payload cluster.AuthEvent
	if err := ev.Decode(&payload); err != nil || strings.TrimSpace(payload.ID) == "" {
		return
	}
	id, err := s.normalizeID(payload.ID)
	if err != nil {
		return
	}
	s.queueSync(id, syncRequest{version: payload.Version})
}

func (s *Store) queueSync(id string, req syncRequest) {
	s.syncMu.Lock()
	if current, ok := s.syncPending[id]; !ok || req.version > current.version {
		s.syncPending[id] = req
	}
	s.syncMu.Unlock()
	signal(s.syncSignal)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Store) syncLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.opts.ReconcileInterval)
	defer ticker.Stop()
	scan := time.NewTicker(s.opts.LocalScanInterval)
	defer scan.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.reconcile()
		case <-scan.C:
			s.reconcileLocalFiles(strayDirPrefix(), s.opts.StrayGrace)
		case <-s.syncSignal:
			s.processSync()
		}
	}
}

// processSync re-reads the rows other nodes announced and mirrors them.
func (s *Store) processSync() {
	s.syncMu.Lock()
	pending := s.syncPending
	s.syncPending = make(map[string]syncRequest)
	resync := s.resync
	s.resync = false
	s.syncMu.Unlock()
	if resync {
		// A (re)connected bus may have missed anything; compare everything.
		s.reconcile()
		return
	}
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := s.backendFor(ctx)
	if err != nil {
		return
	}
	retry := make(map[string]syncRequest)
	for id, req := range pending {
		r, errGet := b.get(ctx, id)
		switch {
		case errGet != nil:
			log.WithError(errGet).Debugf("cluster auth: re-read %s", id)
			req.attempts++
			retry[id] = req
		case r == nil && req.version > 0:
			// Announced but not visible yet: an in-process bus can deliver a
			// PublishTx event before the writer's commit is visible to other
			// connections, and a brand-new row reads as missing until then.
			// Forgetting it here would leave the mirror empty until the next
			// reconcile, so re-read it like any row that is behind.
			req.attempts++
			retry[id] = req
		case r == nil:
			s.forget(id)
		case r.Version < req.version:
			req.attempts++
			retry[id] = req
		default:
			s.applyRow(*r)
		}
	}
	for id, req := range retry {
		if req.attempts >= syncMaxAttempts {
			delete(retry, id)
		}
	}
	if len(retry) > 0 {
		time.AfterFunc(syncRetryDelay, func() {
			for id, req := range retry {
				s.queueSync(id, req)
			}
		})
	}
}

// reconcile compares every row version with this node's view and re-reads
// only the rows that changed, rewriting only their mirrors. It runs
// periodically and on every Resync (bus reconnects, nodes joining), because
// notifications are not durable, so it must stay cheap: one id/version scan.
func (s *Store) reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := s.backendFor(ctx)
	if err != nil {
		return
	}
	versions, err := b.listVersions(ctx)
	if err != nil {
		log.WithError(err).Warn("cluster auth: reconcile credentials")
		return
	}
	seen := make(map[string]struct{}, len(versions))
	var stale, behind []string
	s.mu.Lock()
	for _, v := range versions {
		seen[v.ID] = struct{}{}
		entry := s.known[v.ID]
		switch {
		case entry == nil || entry.version < v.Version:
			stale = append(stale, v.ID)
		case entry.version > v.Version:
			behind = append(behind, v.ID)
		}
	}
	var vanished []string
	for id, entry := range s.known {
		if _, ok := seen[id]; !ok && !entry.deleted {
			vanished = append(vanished, id)
		}
	}
	s.mu.Unlock()
	if len(stale) > 0 {
		rows, errRows := b.getMany(ctx, stale)
		if errRows != nil {
			log.WithError(errRows).Warn("cluster auth: reconcile credentials")
			return
		}
		for _, r := range rows {
			s.applyRow(r)
		}
	}
	for _, id := range behind {
		s.repairBehind(ctx, b, id)
	}
	for _, id := range vanished {
		s.forget(id)
	}
}

// repairBehind handles a row the database holds at an older version than
// this node saw committed, which only a failover that lost writes (or a
// restore) produces. The newer state this node holds is written back rather
// than dropped, so rotated tokens and deletions survive.
func (s *Store) repairBehind(ctx context.Context, b backend, id string) {
	current, err := b.get(ctx, id)
	if err != nil || current == nil {
		return
	}
	s.mu.Lock()
	entry := s.known[id]
	var (
		version int64
		deleted bool
		content []byte
	)
	if entry != nil {
		version, deleted, content = entry.version, entry.deleted, entry.content
	}
	s.mu.Unlock()
	if entry == nil || current.Version >= version {
		return
	}
	log.Warnf("cluster auth: %s went back from version %d to %d in the database; restoring the newer state", id, version, current.Version)
	// Adopt the surviving row without touching the mirror yet, so the node
	// does not flap to the older state while the newer one is written back.
	s.mu.Lock()
	s.recordRowLocked(*current, true)
	s.mu.Unlock()
	defer s.syncMirror(id)
	switch {
	case deleted && !current.Deleted:
		if newVersion, ok, errDelete := b.tombstone(ctx, id, s.nodeID(), s.publisher(ctx, id, true)); errDelete == nil && ok {
			s.applyRow(row{ID: id, Version: newVersion, Deleted: true})
		}
	case !deleted && !current.Deleted:
		_ = s.compareAndSwap(ctx, credentialWrite{id: id, content: content, version: current.Version, provider: providerOf(content)}, false)
	case !deleted && current.Deleted:
		// The revive was lost: write it again as the explicit create it was.
		_ = s.createCredential(ctx, credentialWrite{id: id, content: content, provider: providerOf(content)})
	}
}

func providerOf(content []byte) string {
	doc, err := decodeObject(content)
	if err != nil {
		return ""
	}
	return documentProvider(doc)
}
