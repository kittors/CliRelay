package clusterauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// maxWriteBackoff caps the retry delay of a buffered write the database
// keeps refusing (it is down, or failing over).
const maxWriteBackoff = time.Minute

// runtimePatch buffers the runtime observations of one credential until the
// next flush. Later values replace earlier ones.
type runtimePatch struct {
	set       map[string][]byte
	removed   map[string]struct{}
	failures  int
	notBefore time.Time
}

func (p *runtimePatch) merge(set map[string][]byte, removed []string) {
	if p.set == nil {
		p.set = make(map[string][]byte)
	}
	if p.removed == nil {
		p.removed = make(map[string]struct{})
	}
	for key, raw := range set {
		p.set[key] = raw
		delete(p.removed, key)
	}
	for _, key := range removed {
		delete(p.set, key)
		p.removed[key] = struct{}{}
	}
}

// overlay applies the patch to a runtime view.
func (p *runtimePatch) overlay(runtime map[string][]byte) {
	for key, raw := range p.set {
		runtime[key] = raw
	}
	for key := range p.removed {
		delete(runtime, key)
	}
}

func (p *runtimePatch) removedKeys() []string {
	keys := make([]string, 0, len(p.removed))
	for key := range p.removed {
		keys = append(keys, key)
	}
	return keys
}

// stageRuntimeLocked buffers the difference between the runtime keys of a
// save and this node's view, and updates the view. Caller holds s.mu.
func (s *Store) stageRuntimeLocked(id string, entry *knownEntry, incoming map[string][]byte) {
	var set map[string][]byte
	var removed []string
	for key, raw := range incoming {
		if !bytes.Equal(entry.runtime[key], raw) {
			if set == nil {
				set = make(map[string][]byte)
			}
			set[key] = raw
		}
	}
	for key := range entry.runtime {
		if _, ok := incoming[key]; !ok {
			removed = append(removed, key)
		}
	}
	if len(set) == 0 && len(removed) == 0 {
		return
	}
	if entry.runtime == nil {
		entry.runtime = make(map[string][]byte)
	}
	for key, raw := range set {
		entry.runtime[key] = raw
	}
	for _, key := range removed {
		delete(entry.runtime, key)
	}
	patch := s.patches[id]
	if patch == nil {
		patch = &runtimePatch{}
		s.patches[id] = patch
	}
	patch.merge(set, removed)
}

// deferredWrite is a credential change a request-result save could not
// write inline.
type deferredWrite struct {
	write     credentialWrite
	failures  int
	notBefore time.Time
}

func (s *Store) deferWrite(w credentialWrite) {
	s.mu.Lock()
	s.writes[w.id] = &deferredWrite{write: w}
	s.mu.Unlock()
}

// dropPending discards buffered state of a credential that was deleted or
// replaced by an explicit create.
func (s *Store) dropPending(id string) {
	s.mu.Lock()
	delete(s.patches, id)
	delete(s.writes, id)
	s.mu.Unlock()
}

func (s *Store) writeLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.flush(ctx)
			cancel()
		}
	}
}

// flush writes the buffered runtime observations, merging them into the row
// without a version bump or a notification, and the deferred credential
// writes, compare-and-set like any other.
func (s *Store) flush(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	patches := make(map[string]*runtimePatch)
	for id, patch := range s.patches {
		if !patch.notBefore.After(now) {
			patches[id] = patch
			delete(s.patches, id)
		}
	}
	writes := make(map[string]*deferredWrite)
	for id, write := range s.writes {
		if !write.notBefore.After(now) {
			writes[id] = write
			delete(s.writes, id)
		}
	}
	s.mu.Unlock()
	if len(patches) == 0 && len(writes) == 0 {
		return
	}
	b, err := s.backendFor(ctx)
	if err != nil {
		s.requeue(patches, writes)
		return
	}
	failedPatches := make(map[string]*runtimePatch)
	for id, patch := range patches {
		set := make(map[string]json.RawMessage, len(patch.set))
		for key, raw := range patch.set {
			set[key] = raw
		}
		if errPatch := b.patchRuntime(ctx, id, s.nodeID(), set, patch.removedKeys()); errPatch != nil {
			// Warn once per streak; the retry backs off while the database is away.
			if patch.failures == 2 {
				log.WithError(errPatch).Warnf("cluster auth: runtime state of %s keeps failing to write, retrying", id)
			}
			failedPatches[id] = patch
		}
	}
	failedWrites := make(map[string]*deferredWrite)
	for id, write := range writes {
		errWrite := s.updateCredential(ctx, write.write)
		switch {
		case errWrite == nil:
		case errors.Is(errWrite, coreauth.ErrCredentialConflict), errors.Is(errWrite, coreauth.ErrCredentialGone):
			// The credential moved on; this node converged to the newer copy.
			log.Debugf("cluster auth: dropped deferred write of %s: %v", id, errWrite)
		default:
			log.WithError(errWrite).Warnf("cluster auth: deferred write of %s failed, will retry", id)
			failedWrites[id] = write
		}
	}
	s.requeue(failedPatches, failedWrites)
}

// requeue puts failed buffered writes back with a growing delay. Anything
// buffered since takes precedence over the failed values.
func (s *Store) requeue(patches map[string]*runtimePatch, writes map[string]*deferredWrite) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, failed := range patches {
		failed.failures++
		failed.notBefore = now.Add(writeBackoff(s.opts.FlushInterval, failed.failures))
		if newer := s.patches[id]; newer != nil {
			failed.merge(newer.set, newer.removedKeys())
		}
		s.patches[id] = failed
	}
	for id, failed := range writes {
		if _, newer := s.writes[id]; newer {
			continue
		}
		failed.failures++
		failed.notBefore = now.Add(writeBackoff(s.opts.FlushInterval, failed.failures))
		s.writes[id] = failed
	}
}

func writeBackoff(base time.Duration, failures int) time.Duration {
	delay := base
	for i := 1; i < failures && delay < maxWriteBackoff; i++ {
		delay *= 2
	}
	if delay > maxWriteBackoff {
		delay = maxWriteBackoff
	}
	return delay
}
