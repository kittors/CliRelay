package executor

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
)

// Synthetic identifiers across a cluster.
//
// The prompt cache keys, Session_id/Conversation_id headers and cloaked
// user_ids this package invents are random values cached per conversation.
// In a cluster each node would invent its own, and one conversation served by
// two nodes would show the upstream two sessions, each missing the other's
// prompt cache. With a SharedIDMinter installed the first node to need an
// identifier mints it for the whole cluster and every other node adopts it.
//
// The identifier is still generated here, in exactly the format it always had
// (UUIDv7 where it was v7, v4 where it was v4, the Claude Code user_id shape):
// the shape of these identifiers is part of the client fingerprint and must
// not change because of clustering. Redis only decides which candidate wins.
// The per-process caches stay in front of it, and when Redis cannot be
// reached identifiers are minted locally exactly as before.

// SharedIDMinter mints synthetic identifiers once per cluster.
type SharedIDMinter interface {
	Available() bool
	MintID(ctx context.Context, namespace, key, candidate string, ttl time.Duration, sliding bool) (sharedstate.MintedID, error)
}

type sharedIDMinterRef struct{ minter SharedIDMinter }

var sharedIDMinter atomic.Pointer[sharedIDMinterRef]

// SetSharedIDMinter installs the cluster-wide minter. nil restores
// per-process minting.
func SetSharedIDMinter(m SharedIDMinter) {
	if m == nil {
		sharedIDMinter.Store(nil)
		return
	}
	sharedIDMinter.Store(&sharedIDMinterRef{minter: m})
}

const (
	sharedIDNamespaceCodex      = "codex-session"
	sharedIDNamespaceClaudeUser = "claude-user"

	// codexClaudePromptCacheTTL is how long a Claude-to-Codex conversation
	// keeps its derived prompt cache key.
	codexClaudePromptCacheTTL = time.Hour

	// userIDSharedTouchInterval is how often a node that keeps reusing a
	// cached user_id refreshes the shared copy. The user_id TTL slides on
	// use, and a node answering from its own cache never reaches Redis, so
	// without the refresh the shared copy would expire under a conversation
	// that is still active and the next node to miss would mint a new one.
	userIDSharedTouchInterval = 5 * time.Minute
)

// mintSharedID asks the cluster for the identifier of (namespace, key),
// offering candidate. ok is false outside a cluster and whenever the shared
// store cannot answer; the caller then keeps its local behaviour.
func mintSharedID(namespace, key, candidate string, ttl time.Duration, sliding bool) (sharedstate.MintedID, bool) {
	ref := sharedIDMinter.Load()
	if ref == nil || !ref.minter.Available() || ttl <= 0 {
		return sharedstate.MintedID{}, false
	}
	minted, err := ref.minter.MintID(context.Background(), namespace, key, candidate, ttl, sliding)
	if err != nil || minted.ID == "" {
		return sharedstate.MintedID{}, false
	}
	return minted, true
}

func sharedIDsInstalled() bool {
	ref := sharedIDMinter.Load()
	return ref != nil && ref.minter.Available()
}

// mintSharedCodexID resolves a codex cache miss through the cluster and caches
// the winner for as long as the cluster keeps it.
func mintSharedCodexID(key string, ttl time.Duration, newID func() string) (string, bool) {
	if !sharedIDsInstalled() {
		return "", false
	}
	minted, ok := mintSharedID(sharedIDNamespaceCodex, key, newID(), ttl, false)
	if !ok {
		return "", false
	}
	codexCacheMu.Lock()
	codexCacheMap[key] = codexCache{ID: minted.ID, Expire: time.Now().Add(minted.TTL), Shared: true}
	codexCacheMu.Unlock()
	return minted.ID, true
}

// adoptSharedCodexID reconciles an identifier this node minted on its own,
// while the shared store was unreachable. Offering it with its remaining
// lifetime makes it the cluster's identifier when no other node minted one
// meanwhile; otherwise this node switches to the other node's identifier,
// which costs that conversation one prompt-cache miss instead of the nodes
// disagreeing until the entry expires.
func adoptSharedCodexID(key string, cache codexCache) string {
	if !sharedIDsInstalled() {
		return cache.ID
	}
	remaining := time.Until(cache.Expire)
	minted, ok := mintSharedID(sharedIDNamespaceCodex, key, cache.ID, remaining, false)
	if !ok {
		return cache.ID
	}
	codexCacheMu.Lock()
	if current, exists := codexCacheMap[key]; exists && current.ID == cache.ID {
		codexCacheMap[key] = codexCache{ID: minted.ID, Expire: time.Now().Add(minted.TTL), Shared: true}
	}
	codexCacheMu.Unlock()
	return minted.ID
}

// mintSharedUserID resolves a user_id cache miss through the cluster.
func mintSharedUserID(key, candidate string) (string, bool) {
	minted, ok := mintSharedID(sharedIDNamespaceClaudeUser, key, candidate, userIDTTL, true)
	if !ok || !isValidUserID(minted.ID) {
		return "", false
	}
	return minted.ID, true
}

// userIDNeedsSharedRefresh reports whether a cached user_id should be pushed
// to the cluster again, and marks it as refreshed so concurrent hits start at
// most one refresh. Callers must hold userIDCacheMu.
func userIDNeedsSharedRefresh(entry *userIDCacheEntry, now time.Time) bool {
	if !sharedIDsInstalled() || now.Sub(entry.sharedAt) < userIDSharedTouchInterval {
		return false
	}
	entry.sharedAt = now
	return true
}

// refreshSharedUserID renews the shared copy of a cached user_id off the
// request path and adopts the cluster's value if another node minted a
// different one (possible after an outage).
func refreshSharedUserID(key, value string) {
	shared, ok := mintSharedUserID(key, value)
	if !ok || shared == value {
		return
	}
	userIDCacheMu.Lock()
	if entry, exists := userIDCache[key]; exists && entry.value == value {
		entry.value = shared
		userIDCache[key] = entry
	}
	userIDCacheMu.Unlock()
}
