package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// SessionAffinityStore shares session-sticky bindings between processes.
//
// The binding key is deterministic, but the key→account binding used to live
// only in the process that made it. When a conversation's next turn lands on
// another process it was bound afresh, usually to another account, and lost
// that account's upstream prompt cache. With a store installed every process
// reads and writes the same bindings; the in-memory table is kept as a mirror
// and is what selection falls back to when the store cannot be reached.
//
// Bindings name accounts by AffinityAccountRef, never by auth ID: the store
// lives outside this process (in production a Redis on another machine), and
// auth IDs are credential file names that often carry the account's e-mail.
type SessionAffinityStore interface {
	// Lookup returns the binding for key, counts this request against it and
	// extends its TTL.
	Lookup(ctx context.Context, key string, ttl time.Duration) (SessionAffinityBinding, error)
	// Bind binds key to accountRef unless a binding exists; then the existing
	// binding wins, counts this request and is returned.
	Bind(ctx context.Context, key, accountRef string, ttl time.Duration) (SessionAffinityBinding, error)
	// Release deletes the binding for key if it still names accountRef.
	Release(ctx context.Context, key, accountRef string) error
}

// SessionAffinityBinding is one shared binding.
type SessionAffinityBinding struct {
	// AccountRef is the AffinityAccountRef of the bound auth.
	AccountRef string
	// Served counts the requests the binding served before this one; the
	// sticky-max-requests limit compares against it.
	Served int
	// Found is false when no binding existed.
	Found bool
}

// AffinityAccountRef is the stable, non-reversible name a shared binding
// uses for an auth.
func AffinityAccountRef(authID string) string {
	sum := sha256.Sum256([]byte("clirelay-affinity\x00" + authID))
	return hex.EncodeToString(sum[:16])
}

type sessionAffinityRef struct{ store SessionAffinityStore }

// SetSessionAffinityStore shares sticky bindings through store. nil restores
// per-process bindings. Hosts set it before serving traffic.
func (m *Manager) SetSessionAffinityStore(store SessionAffinityStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.scheduler == nil {
		m.scheduler = &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: m.concurrencyLimiter}
	}
	deps := m.scheduler
	m.mu.Unlock()
	if store == nil {
		deps.affinity.Store(nil)
		return
	}
	deps.affinity.Store(&sessionAffinityRef{store: store})
}

func (d *schedulerDeps) sessionAffinity() SessionAffinityStore {
	if d == nil {
		return nil
	}
	if ref := d.affinity.Load(); ref != nil {
		return ref.store
	}
	return nil
}

// honour returns the account bound to key when that binding still stands,
// consulting the shared store when there is one.
func (s *SessionStickySelector) honour(ctx context.Context, key string, available []*Auth, limits stickyLimits, now time.Time) *Auth {
	store := s.deps.sessionAffinity()
	if store == nil {
		return s.honourBinding(key, available, limits, now)
	}
	binding, err := store.Lookup(ctx, key, sessionStickyTTL)
	if err != nil {
		return s.honourBinding(key, available, limits, now)
	}
	if !binding.Found {
		// A binding made here while the store was unreachable still stands
		// locally; carry it into the store so every process follows it.
		bound := s.honourBinding(key, available, limits, now)
		if bound == nil {
			return nil
		}
		return s.bindShared(ctx, store, key, bound, available, now)
	}

	// The same release rules as the local table: request budget spent, the
	// account no longer eligible, or the account too loaded.
	bound := findAuthByRef(available, binding.AccountRef)
	if bound == nil ||
		(limits.maxRequests > 0 && binding.Served >= limits.maxRequests) ||
		(limits.releaseAtLoad > 0 && s.deps.loadRatio(bound) >= limits.releaseAtLoad) {
		_ = store.Release(ctx, key, binding.AccountRef)
		s.deleteBinding(key)
		return nil
	}
	s.deps.observeSelection(bound, now)
	s.mirrorBinding(key, bound.ID, binding.Served+1, now)
	return bound
}

// bindSelected records a fresh selection for key and returns the account the
// request should use.
func (s *SessionStickySelector) bindSelected(ctx context.Context, key string, selected *Auth, available []*Auth, now time.Time) *Auth {
	store := s.deps.sessionAffinity()
	if store == nil {
		s.bind(key, selected.ID, now)
		return selected
	}
	return s.bindShared(ctx, store, key, selected, available, now)
}

// bindShared stores key→picked. If another process bound the session first,
// its binding wins and the request joins that account when it may use it.
func (s *SessionStickySelector) bindShared(ctx context.Context, store SessionAffinityStore, key string, picked *Auth, available []*Auth, now time.Time) *Auth {
	ref := AffinityAccountRef(picked.ID)
	winner, err := store.Bind(ctx, key, ref, sessionStickyTTL)
	if err != nil || winner.AccountRef == "" || winner.AccountRef == ref {
		s.bind(key, picked.ID, now)
		return picked
	}
	if other := findAuthByRef(available, winner.AccountRef); other != nil {
		s.deps.observeSelection(other, now)
		s.mirrorBinding(key, other.ID, winner.Served+1, now)
		return other
	}
	// The winning account is not eligible for this request (another group,
	// cooling down here). Serve it with this pick and leave the binding to the
	// process that made it.
	s.deleteBinding(key)
	return picked
}

// mirrorBinding keeps the in-memory table in step with the shared binding, so
// a fallback to it keeps the conversation where it is.
func (s *SessionStickySelector) mirrorBinding(key, authID string, requests int, now time.Time) {
	s.bind(key, authID, now)
	s.mu.Lock()
	if binding := s.bindings[key]; binding != nil {
		binding.requests = requests
	}
	s.mu.Unlock()
}

func findAuthByRef(auths []*Auth, ref string) *Auth {
	if ref == "" {
		return nil
	}
	for _, auth := range auths {
		if auth != nil && AffinityAccountRef(auth.ID) == ref {
			return auth
		}
	}
	return nil
}
