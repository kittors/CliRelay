package contentmoderation

import (
	"container/list"
	"sync"
)

// Building the contexts of a request means copying the rules of every layer and
// compiling them: for a full list of 10,000 keywords that takes milliseconds and
// a few megabytes. A request therefore does not build its contexts, it looks
// for the ones already built for the policies it was given. Stored policies
// change only when saved, and a save changes the policy's stamp, so contexts
// stay good for as long as the stamps do.
//
// What the cache holds is bounded in three ways, because the lists it keeps can
// be large and operators edit them:
//
//   - Of any stored policy only one saved state is kept. When an entry built
//     from a newer state of a policy is stored, every entry built from an older
//     state goes, since no request is given that state again. Editing a list
//     fifty times leaves one compiled list in memory, not fifty.
//   - The entries are weighed (see wordMatcher.size) and the cache holds at most
//     maxCachedWordBytes of them. Past that the least recently used go one by
//     one, so a full cache loses what nobody asked for lately and not
//     everything at once (a reset would send every request to compile its list
//     in the same instant).
//   - An entry is built once however many requests need it at the same time.
//     A burst of requests that finds a policy not yet compiled, which is what
//     every request is after a save, waits for one compile instead of running
//     one each.

// maxCachedWordBytes bounds the compiled lists the cache keeps, by estimate. A
// list of 10,000 keywords, the most a policy may hold, takes about 3.5 MB, so
// this holds a few dozen of those and any number of ordinary ones.
const maxCachedWordBytes = 128 << 20

// wordCacheEntryBytes is charged to every entry besides its matcher, for the
// entry itself, the maps and the lists that point at it. It keeps a great many
// small policies from being free.
const wordCacheEntryBytes = 1024

// wordPolicyID names a stored policy: the tenant it belongs to, its scope and,
// for a key policy, the key.
type wordPolicyID struct{ tenantID, scopeType, scopeID string }

// wordPolicyStamp names one saved state of a stored policy. Every save raises
// the version and sets the time of the last save, and a policy that is deleted
// and created again starts over at version 1 with a new time, so the two
// together tell states apart (the policy cache in the store relies on the same
// pair). The zero stamp stands for a policy that does not take part.
type wordPolicyStamp struct {
	id      wordPolicyID
	version int64
	savedAt int64 // Unix nanoseconds
}

// stampWordPolicy returns the stamp of a policy read from the store. A policy
// without a version or a save time (a draft, a policy built by hand) cannot be
// told apart from a changed copy of itself, so it has no stamp and is never
// cached.
func stampWordPolicy(p *WordPolicy) (wordPolicyStamp, bool) {
	if p == nil {
		return wordPolicyStamp{}, true
	}
	if p.Version <= 0 || p.UpdatedAt.IsZero() {
		return wordPolicyStamp{}, false
	}
	return wordPolicyStamp{wordPolicyID{p.TenantID, p.ScopeType, p.ScopeID}, p.Version, p.UpdatedAt.UnixNano()}, true
}

// wordCacheKey names a context by the saved states of the policies it is built
// from.
type wordCacheKey struct {
	platform bool
	// stamps are the platform policy, for the platform's context, and the tenant
	// and key policies for the other. A policy that is not enabled does not take
	// part in the context it would have joined, so its stamp is left out: saving
	// a disabled policy must not make anything compile again.
	stamps [2]wordPolicyStamp
}

type wordCacheEntry struct {
	key wordCacheKey
	ctx wordContext
	// has is false when the policies make no context (no enabled rule). That is
	// remembered too.
	has   bool
	bytes int64
	elem  *list.Element
}

// wordPolicyRefs lists the entries built from one stored policy. They are all
// built from the same saved state of it, stamp.
type wordPolicyRefs struct {
	stamp   wordPolicyStamp
	entries map[*wordCacheEntry]struct{}
}

// wordFlight is a build in progress. The requests that need the same entry wait
// on done instead of building it again.
type wordFlight struct {
	done chan struct{}
	ctx  wordContext
	has  bool
	// built is false when the build did not finish (it panicked), and whoever
	// waited then builds for itself.
	built bool
}

// wordContextCache remembers the contexts built from stored policies; see the
// notes above. The zero value is ready to use.
type wordContextCache struct {
	mu      sync.Mutex
	entries map[wordCacheKey]*wordCacheEntry
	refs    map[wordPolicyID]*wordPolicyRefs
	flights map[wordCacheKey]*wordFlight
	// lru has the entries from the most recently used to the least.
	lru   list.List
	bytes int64
	// limit is the bound on bytes. Zero means maxCachedWordBytes; tests set it.
	limit int64
}

// contextsFor is buildWordContexts with the result remembered while the
// policies keep the stamps it was built for. The result is shared between
// requests and must not be modified.
func (c *wordContextCache) contextsFor(platform, tenant, key *WordPolicy) []wordContext {
	if platform == nil && tenant == nil && key == nil {
		return nil
	}
	contexts := make([]wordContext, 0, 2)
	if platform != nil && platform.Enabled {
		var context wordContext
		var ok bool
		if stamp, stamped := stampWordPolicy(platform); stamped {
			cacheKey := wordCacheKey{platform: true, stamps: [2]wordPolicyStamp{stamp}}
			context, ok = c.context(cacheKey, func() (wordContext, bool) { return buildPlatformContext(platform) })
		} else {
			context, ok = buildPlatformContext(platform)
		}
		if ok {
			contexts = append(contexts, context)
		}
	}
	tenantActive := tenant != nil && tenant.Enabled
	keyActive := key != nil && key.Enabled
	if tenantActive || keyActive {
		var cacheKey wordCacheKey
		stamped := true
		if tenantActive {
			var ok bool
			cacheKey.stamps[0], ok = stampWordPolicy(tenant)
			stamped = stamped && ok
		}
		if keyActive {
			var ok bool
			cacheKey.stamps[1], ok = stampWordPolicy(key)
			stamped = stamped && ok
		}
		var context wordContext
		var ok bool
		if stamped {
			context, ok = c.context(cacheKey, func() (wordContext, bool) { return buildTenantContext(tenant, key) })
		} else {
			context, ok = buildTenantContext(tenant, key)
		}
		if ok {
			contexts = append(contexts, context)
		}
	}
	if len(contexts) == 0 {
		return nil
	}
	return contexts
}

// context returns the context for key, which build makes if the cache has none.
// Requests that ask for the same key while it is being built wait for that build.
func (c *wordContextCache) context(key wordCacheKey, build func() (wordContext, bool)) (wordContext, bool) {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.lru.MoveToFront(entry.elem)
		context, has := entry.ctx, entry.has
		c.mu.Unlock()
		return context, has
	}
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		<-flight.done
		if flight.built {
			return flight.ctx, flight.has
		}
		return build()
	}
	flight := &wordFlight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = make(map[wordCacheKey]*wordFlight)
	}
	c.flights[key] = flight
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.flights, key)
		if flight.built {
			c.store(key, flight.ctx, flight.has)
		}
		c.mu.Unlock()
		close(flight.done)
	}()
	flight.ctx, flight.has = build()
	flight.built = true
	return flight.ctx, flight.has
}

// store keeps an entry, first removing what it makes out of date and afterwards
// what the bound on size asks for. The caller holds the lock.
func (c *wordContextCache) store(key wordCacheKey, ctx wordContext, has bool) {
	if old := c.entries[key]; old != nil {
		c.drop(old)
	}
	// Entries built from another saved state of a policy this one is built from
	// are out of date for good. Which state is the later one is not decided by
	// comparing stamps: with the clocks of several nodes, or a policy created
	// again, the order of the numbers is not the order of the saves. The state
	// seen last wins, and a request that was still holding an earlier one costs
	// at most one more build.
	for _, stamp := range key.stamps {
		if stamp == (wordPolicyStamp{}) {
			continue
		}
		if refs := c.refs[stamp.id]; refs != nil && refs.stamp != stamp {
			for entry := range refs.entries {
				c.drop(entry)
			}
		}
	}
	entry := &wordCacheEntry{key: key, ctx: ctx, has: has, bytes: wordCacheEntryBytes}
	if has {
		entry.bytes += ctx.matcher.size()
	}
	entry.elem = c.lru.PushFront(entry)
	if c.entries == nil {
		c.entries = make(map[wordCacheKey]*wordCacheEntry)
	}
	c.entries[key] = entry
	c.bytes += entry.bytes
	for _, stamp := range key.stamps {
		if stamp == (wordPolicyStamp{}) {
			continue
		}
		if c.refs == nil {
			c.refs = make(map[wordPolicyID]*wordPolicyRefs)
		}
		refs := c.refs[stamp.id]
		if refs == nil {
			refs = &wordPolicyRefs{stamp: stamp, entries: make(map[*wordCacheEntry]struct{})}
			c.refs[stamp.id] = refs
		}
		refs.entries[entry] = struct{}{}
	}
	limit := c.limit
	if limit <= 0 {
		limit = maxCachedWordBytes
	}
	for c.bytes > limit {
		last := c.lru.Back()
		// The entry just made stays, even when it alone is over the bound: the
		// request that built it and the ones behind it need it, and dropping it
		// would have them all compile it again.
		if last == nil || last == entry.elem {
			break
		}
		c.drop(last.Value.(*wordCacheEntry))
	}
}

// drop removes an entry. The caller holds the lock.
func (c *wordContextCache) drop(entry *wordCacheEntry) {
	c.lru.Remove(entry.elem)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
	for _, stamp := range entry.key.stamps {
		if stamp == (wordPolicyStamp{}) {
			continue
		}
		if refs := c.refs[stamp.id]; refs != nil {
			delete(refs.entries, entry)
			if len(refs.entries) == 0 {
				delete(c.refs, stamp.id)
			}
		}
	}
}
