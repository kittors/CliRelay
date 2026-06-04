package vision

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// GlobalRegistry manages all session stores with resource limits and GC.
type GlobalRegistry struct {
	mu       sync.RWMutex
	sessions map[SessionKey]*SessionStore
	config   GlobalConfig
	done     chan struct{}
	sfGroup  singleflight.Group
}

var global *GlobalRegistry
var globalOnce sync.Once

// GetGlobal returns the singleton global registry.
func GetGlobal() *GlobalRegistry {
	globalOnce.Do(func() {
		global = newGlobalRegistry(DefaultGlobalConfig())
	})
	return global
}

func newGlobalRegistry(cfg GlobalConfig) *GlobalRegistry {
	r := &GlobalRegistry{
		sessions: make(map[SessionKey]*SessionStore),
		config:   cfg,
		done:     make(chan struct{}),
	}
	go r.gcLoop()
	return r
}

// GetOrCreateSession returns the session store for the given key, creating one if needed.
func (r *GlobalRegistry) GetOrCreateSession(key SessionKey) *SessionStore {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.sessions[key]; ok {
		return s
	}

	// Check global limit
	if len(r.sessions) >= r.config.MaxSessions {
		r.evictOldestSession()
	}

	s := &SessionStore{
		entries: make(map[ImageHash]*ImageEntry),
	}
	r.sessions[key] = s
	return s
}

// GetSession returns an existing session store, or nil if not found.
func (r *GlobalRegistry) GetSession(key SessionKey) *SessionStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[key]
}

// evictOldestSession removes the least recently used session.
func (r *GlobalRegistry) evictOldestSession() {
	var oldestKey SessionKey
	var oldestTime time.Time
	first := true
	for key, s := range r.sessions {
		s.mu.RLock()
		t := s.updatedAt
		s.mu.RUnlock()
		if first || t.Before(oldestTime) {
			oldestKey = key
			oldestTime = t
			first = false
		}
	}
	if oldestKey != "" {
		delete(r.sessions, oldestKey)
	}
}

func (r *GlobalRegistry) gcLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.gc()
		case <-r.done:
			return
		}
	}
}

func (r *GlobalRegistry) gc() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	ttl := r.config.SessionTTL
	for key, s := range r.sessions {
		s.mu.RLock()
		idle := now.Sub(s.updatedAt)
		s.mu.RUnlock()
		if idle > ttl {
			delete(r.sessions, key)
		}
	}
}

// Stop terminates the GC goroutine.
func (r *GlobalRegistry) Stop() {
	close(r.done)
}

// Stats returns basic metrics about the registry.
func (r *GlobalRegistry) Stats() (sessions, entries int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sessions = len(r.sessions)
	for _, s := range r.sessions {
		s.mu.RLock()
		entries += len(s.entries)
		s.mu.RUnlock()
	}
	return
}

// --- SessionStore methods ---

// ComputeHash returns the SHA256 hex of base64 image data.
func ComputeHash(data string) ImageHash {
	h := sha256.Sum256([]byte(data))
	return ImageHash(fmt.Sprintf("%x", h))
}

// NextOrdinal returns the next available ordinal and increments the counter.
func (s *SessionStore) NextOrdinal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextOrdinal++
	return s.nextOrdinal
}

// ResetReachability marks all entries as not reachable for the current request.
// Callers should then mark the entries actually present in the request as reachable.
func (s *SessionStore) ResetReachability() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		e.CurrentPayloadReachable = false
		e.Availability = ImageUnavailableNow
	}
	s.updatedAt = time.Now()
}

// GetOrCreateEntry finds an existing entry by hash or creates a new one.
func (s *SessionStore) GetOrCreateEntry(hash ImageHash, ordinal int, maxEntries int) *ImageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.entries[hash]; ok {
		e.LastAccessAt = time.Now()
		s.promote(hash)
		return e
	}

	// Check per-session limit
	if maxEntries > 0 && len(s.entries) >= maxEntries {
		s.evictOldestEntry()
	}

	e := &ImageEntry{
		Hash:          hash,
		StableOrdinal: ordinal,
		CreatedAt:     time.Now(),
		LastAccessAt:  time.Now(),
	}
	s.entries[hash] = e
	s.order = append(s.order, hash)
	s.updatedAt = time.Now()
	return e
}

// GetEntry retrieves an entry by hash.
func (s *SessionStore) GetEntry(hash ImageHash) *ImageEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[hash]
	if ok {
		e.LastAccessAt = time.Now()
	}
	return e
}

// UpdateEntry applies a mutator function to an existing entry.
func (s *SessionStore) UpdateEntry(hash ImageHash, fn func(e *ImageEntry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[hash]; ok {
		fn(e)
		e.LastAccessAt = time.Now()
		e.Revision++
		s.updatedAt = time.Now()
	}
}

// AllEntries returns a copy of all entries sorted by ordinal.
func (s *SessionStore) AllEntries() []*ImageEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ImageEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	// Sort by ordinal (stable across turns)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].StableOrdinal < out[i].StableOrdinal {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (s *SessionStore) promote(hash ImageHash) {
	// Move hash to end of order list (most recently used)
	idx := -1
	for i, h := range s.order {
		if h == hash {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s.order = append(s.order[:idx], s.order[idx+1:]...)
		s.order = append(s.order, hash)
	}
}

func (s *SessionStore) evictOldestEntry() {
	if len(s.order) == 0 {
		return
	}
	oldest := s.order[0]
	s.order = s.order[1:]
	delete(s.entries, oldest)
}
