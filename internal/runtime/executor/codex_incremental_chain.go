package executor

import (
	"sync"
	"time"
)

const (
	// codexIncrementalChainsPerConversation bounds the chains one conversation key
	// keeps. Sub-agents and side requests (titles, summaries) often share their
	// parent's session identity while sending a different history; with a single
	// chain they would overwrite each other's base every turn.
	codexIncrementalChainsPerConversation = 3
	// codexIncrementalMaxShadowChains bounds chains without a connection. Each
	// holds 16 bytes per input item of its last turn.
	codexIncrementalMaxShadowChains = 2048
	// codexIncrementalConnMaxAge retires a connection before the backend's own
	// 60-minute limit ("Responses websocket connection limit reached"), so the
	// limit lands between turns rather than in the middle of one.
	codexIncrementalConnMaxAge = 55 * time.Minute
)

// codexIncrementalChain is one conversation line: the base its next turn is
// compared with and, in "on" mode, the upstream connection holding that base.
type codexIncrementalChain struct {
	key    string
	authID string
	// startedAt is when the chain last went out in full; in shadow mode it
	// stands in for the age of the connection that would carry it.
	startedAt time.Time
	lastUsed  time.Time
	busy      bool
	// turns counts the increments sent since the last full request.
	turns int
	base  *codexIncrementalBase
	conn  *codexIncrementalConn
}

// codexIncrementalLimits are the bounds a pool enforces, read from the config on
// every request so a hot reload applies at once.
type codexIncrementalLimits struct {
	idleTimeout  time.Duration
	maxConns     int
	maxConnsAuth int
}

// codexIncrementalPool keeps the chains of every conversation that takes part.
type codexIncrementalPool struct {
	mu       sync.Mutex
	chains   map[string][]*codexIncrementalChain
	total    int
	conns    int
	connAuth map[string]int
	// sessions maps a client websocket session to its conversation keys, so
	// the chains go when the client closes the socket.
	sessions map[string]map[string]struct{}
	// limits are the bounds of the latest request, used by the timed sweep.
	limits codexIncrementalLimits
	now    func() time.Time
	stats  *codexIncrementalStats
}

func (p *codexIncrementalPool) lastLimits() codexIncrementalLimits {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limits
}

func newCodexIncrementalPool() *codexIncrementalPool {
	return &codexIncrementalPool{
		chains:   make(map[string][]*codexIncrementalChain),
		connAuth: make(map[string]int),
		sessions: make(map[string]map[string]struct{}),
		now:      time.Now,
		stats:    newCodexIncrementalStats(),
	}
}

// closeSession drops the chains of a client websocket session that ended.
func (p *codexIncrementalPool) closeSession(session string) {
	if p == nil || session == "" {
		return
	}
	p.mu.Lock()
	keys := p.sessions[session]
	delete(p.sessions, session)
	p.mu.Unlock()
	for key := range keys {
		p.closeKey(key)
	}
}

// codexIncrementalAcquire tells the caller what it got.
type codexIncrementalAcquire uint8

const (
	// codexChainFound is a chain whose base the request extends.
	codexChainFound codexIncrementalAcquire = iota
	// codexChainNew is a chain starting with this request.
	codexChainNew
	// codexChainBusy means the conversation's matching chain is serving another
	// request; this one goes out on its own.
	codexChainBusy
	// codexChainFull means every chain slot is taken by a turn in progress.
	codexChainFull
)

// codexIncrementalNewLine says why a request starts a new line: the diagnosis
// shadow mode is there to collect.
type codexIncrementalNewLine struct {
	// reason is no_previous_turn (the conversation has no line yet),
	// input_prefix_mismatch (it has lines, none of which the request extends) or
	// account_changed.
	reason string
	// prevItems and mismatchAt describe the closest line for a prefix
	// mismatch: how long its input was and where the request first differs.
	prevItems, mismatchAt int
}

// acquire returns the chain the request belongs to, marked busy. It prefers the
// chain whose base the request extends furthest; with none, it starts a chain,
// reusing an idle one of the same account (and its connection) before opening
// another.
func (p *codexIncrementalPool) acquire(key, session, authID string, cur *codexIncrementalRequest, limits codexIncrementalLimits) (*codexIncrementalChain, codexIncrementalAcquire, codexIncrementalNewLine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits = limits
	if session != "" {
		keys := p.sessions[session]
		if keys == nil {
			keys = make(map[string]struct{}, 1)
			p.sessions[session] = keys
		}
		keys[key] = struct{}{}
	}
	now := p.now()
	chains := p.pruneLocked(key, now, limits)

	line := codexIncrementalNewLine{reason: "no_previous_turn"}
	var best *codexIncrementalChain
	busyMatch := false
	for _, chain := range chains {
		if chain.base == nil {
			continue
		}
		if at := codexIncrementalMismatchAt(chain.base, cur); at >= 0 {
			// Keep the line the request follows longest, the one it most
			// plausibly continues, to say where the two part.
			if line.reason != "input_prefix_mismatch" || at > line.mismatchAt {
				line = codexIncrementalNewLine{reason: "input_prefix_mismatch", prevItems: len(chain.base.items), mismatchAt: at}
			}
			continue
		}
		if chain.busy {
			busyMatch = true
			continue
		}
		if best == nil || len(chain.base.items) > len(best.base.items) {
			best = chain
		}
	}
	if best != nil && best.authID != authID {
		// The conversation moved to another account; its base and connection
		// belong to the old one.
		p.removeLocked(best)
		best = nil
		chains = p.chains[key]
		line = codexIncrementalNewLine{reason: "account_changed"}
	}
	if best != nil {
		best.busy = true
		best.lastUsed = now
		return best, codexChainFound, codexIncrementalNewLine{}
	}
	if busyMatch {
		return nil, codexChainBusy, line
	}

	// A new line for this conversation. A chain of the same account left without
	// a base (its last turn failed and was retried) is taken first, so retries
	// do not pile up lines; past the conversation's share the oldest idle line
	// of the account is reused, keeping its connection.
	for _, chain := range chains {
		if !chain.busy && chain.base == nil && chain.authID == authID {
			chain.restart(now)
			return chain, codexChainNew, line
		}
	}
	if len(chains) >= codexIncrementalChainsPerConversation {
		reuse := oldestIdleCodexChain(chains, authID)
		if reuse == nil {
			reuse = oldestIdleCodexChain(chains, "")
			if reuse != nil {
				p.removeLocked(reuse)
				reuse = nil
			}
		}
		if reuse != nil {
			reuse.restart(now)
			return reuse, codexChainNew, line
		}
		if len(p.chains[key]) >= codexIncrementalChainsPerConversation {
			return nil, codexChainBusy, line
		}
	}
	if p.total >= codexIncrementalMaxShadowChains && !p.evictOldestIdleLocked(func(*codexIncrementalChain) bool { return true }) {
		return nil, codexChainFull, line
	}
	chain := &codexIncrementalChain{key: key, authID: authID, startedAt: now, lastUsed: now, busy: true}
	p.chains[key] = append(p.chains[key], chain)
	p.total++
	return chain, codexChainNew, line
}

// restart reuses an idle chain for a new line of the conversation. Its
// connection, and so the connection's age, carries over.
func (c *codexIncrementalChain) restart(now time.Time) {
	c.busy = true
	c.base = nil
	c.turns = 0
	c.lastUsed = now
}

func oldestIdleCodexChain(chains []*codexIncrementalChain, authID string) *codexIncrementalChain {
	var oldest *codexIncrementalChain
	for _, chain := range chains {
		if chain.busy || (authID != "" && chain.authID != authID) {
			continue
		}
		if oldest == nil || chain.lastUsed.Before(oldest.lastUsed) {
			oldest = chain
		}
	}
	return oldest
}

// pruneLocked drops the idle chains of key that timed out and the connections
// that closed, and returns what is left.
func (p *codexIncrementalPool) pruneLocked(key string, now time.Time, limits codexIncrementalLimits) []*codexIncrementalChain {
	for _, chain := range append([]*codexIncrementalChain(nil), p.chains[key]...) {
		if chain.busy {
			continue
		}
		if limits.idleTimeout > 0 && now.Sub(chain.lastUsed) > limits.idleTimeout {
			p.removeLocked(chain)
			continue
		}
		p.dropDeadConnLocked(chain)
	}
	return p.chains[key]
}

// dropDeadConnLocked forgets a connection that closed between turns. The
// backend's copy of the base went with it; the chain's record of the base stays
// so the next turn is told apart from a conversation's first.
func (p *codexIncrementalPool) dropDeadConnLocked(chain *codexIncrementalChain) {
	if chain.conn == nil || !chain.conn.isClosed() {
		return
	}
	p.forgetConnLocked(chain)
}

func (p *codexIncrementalPool) forgetConnLocked(chain *codexIncrementalChain) {
	conn := chain.conn
	if conn == nil {
		return
	}
	chain.conn = nil
	p.conns--
	if n := p.connAuth[chain.authID] - 1; n > 0 {
		p.connAuth[chain.authID] = n
	} else {
		delete(p.connAuth, chain.authID)
	}
	// The base stays: the next turn still matches it, and is then reported as
	// upstream_connection_lost rather than as the start of a conversation.
	conn.close("chain_dropped")
}

func (p *codexIncrementalPool) removeLocked(chain *codexIncrementalChain) {
	p.forgetConnLocked(chain)
	chains := p.chains[chain.key]
	for i, candidate := range chains {
		if candidate != chain {
			continue
		}
		chains = append(chains[:i], chains[i+1:]...)
		p.total--
		break
	}
	if len(chains) == 0 {
		delete(p.chains, chain.key)
	} else {
		p.chains[chain.key] = chains
	}
}

// evictOldestIdleLocked removes the least recently used idle chain that match
// accepts, reporting whether one was found.
func (p *codexIncrementalPool) evictOldestIdleLocked(match func(*codexIncrementalChain) bool) bool {
	var oldest *codexIncrementalChain
	for _, chains := range p.chains {
		for _, chain := range chains {
			if chain.busy || !match(chain) {
				continue
			}
			if oldest == nil || chain.lastUsed.Before(oldest.lastUsed) {
				oldest = chain
			}
		}
	}
	if oldest == nil {
		return false
	}
	p.removeLocked(oldest)
	return true
}

// reserveConn makes room for a new upstream connection on chain's account,
// closing the least recently used idle connection when a bound is reached. It
// reports false when every connection under the bound is in use.
func (p *codexIncrementalPool) reserveConn(chain *codexIncrementalChain, limits codexIncrementalLimits) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if chain.conn != nil {
		p.forgetConnLocked(chain)
	}
	for limits.maxConnsAuth > 0 && p.connAuth[chain.authID] >= limits.maxConnsAuth {
		if !p.evictOldestIdleConnLocked(chain.authID) {
			return false
		}
	}
	for limits.maxConns > 0 && p.conns >= limits.maxConns {
		if !p.evictOldestIdleConnLocked("") {
			return false
		}
	}
	// Counted now so concurrent dials cannot overshoot; attachConn or
	// releaseReservation settles it.
	p.conns++
	p.connAuth[chain.authID]++
	return true
}

func (p *codexIncrementalPool) evictOldestIdleConnLocked(authID string) bool {
	var oldest *codexIncrementalChain
	for _, chains := range p.chains {
		for _, chain := range chains {
			if chain.busy || chain.conn == nil || (authID != "" && chain.authID != authID) {
				continue
			}
			if oldest == nil || chain.lastUsed.Before(oldest.lastUsed) {
				oldest = chain
			}
		}
	}
	if oldest == nil {
		return false
	}
	p.forgetConnLocked(oldest)
	return true
}

// attachConn hands a dialled connection to the chain holding its reservation.
func (p *codexIncrementalPool) attachConn(chain *codexIncrementalChain, conn *codexIncrementalConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	chain.conn = conn
}

// releaseReservation gives back a reservation whose dial failed.
func (p *codexIncrementalPool) releaseReservation(chain *codexIncrementalChain) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns--
	if n := p.connAuth[chain.authID] - 1; n > 0 {
		p.connAuth[chain.authID] = n
	} else {
		delete(p.connAuth, chain.authID)
	}
}

// codexIncrementalRelease is how a turn leaves its chain.
type codexIncrementalRelease struct {
	// base is the turn that completed, or nil when it left nothing to continue
	// from.
	base *codexIncrementalBase
	// dropConn closes the connection, which a turn that did not finish leaves
	// with events still to come.
	dropConn bool
	// turns is the chain's increment count after this turn.
	turns int
	// resetAge restarts the shadow stand-in for the connection's age, after a
	// turn that would have opened a new connection.
	resetAge bool
}

// release ends a turn on chain.
func (p *codexIncrementalPool) release(chain *codexIncrementalChain, outcome codexIncrementalRelease) {
	if chain == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	chain.busy = false
	chain.lastUsed = now
	chain.base = outcome.base
	chain.turns = outcome.turns
	if outcome.resetAge {
		chain.startedAt = now
	}
	if outcome.dropConn {
		p.forgetConnLocked(chain)
	} else {
		p.dropDeadConnLocked(chain)
	}
	if !p.containsLocked(chain) {
		// Removed while busy (closeKey); nothing else refers to it.
		p.forgetConnLocked(chain)
	}
}

func (p *codexIncrementalPool) containsLocked(chain *codexIncrementalChain) bool {
	for _, candidate := range p.chains[chain.key] {
		if candidate == chain {
			return true
		}
	}
	return false
}

// closeKey drops every chain of a conversation; a client websocket that closed
// will not send its next turn.
func (p *codexIncrementalPool) closeKey(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, chain := range append([]*codexIncrementalChain(nil), p.chains[key]...) {
		if chain.busy {
			// Its turn is still streaming; release sees it is gone and closes
			// the connection then.
			chains := p.chains[key]
			for i, candidate := range chains {
				if candidate == chain {
					p.chains[key] = append(chains[:i], chains[i+1:]...)
					p.total--
					break
				}
			}
			continue
		}
		p.removeLocked(chain)
	}
	if len(p.chains[key]) == 0 {
		delete(p.chains, key)
	}
}

// sweep drops idle chains past the timeout and connections that closed. It runs
// on a timer so idle upstream connections do not wait for their conversation's
// next request to be freed.
func (p *codexIncrementalPool) sweep(limits codexIncrementalLimits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for key := range p.chains {
		p.pruneLocked(key, now, limits)
	}
	// A session whose socket closed without telling the executor (a crash on
	// the client side) must not pin its keys forever.
	for session, keys := range p.sessions {
		for key := range keys {
			if _, live := p.chains[key]; !live {
				delete(keys, key)
			}
		}
		if len(keys) == 0 {
			delete(p.sessions, session)
		}
	}
}

// closeAll drops every idle chain and its connection; busy chains go when their
// turn ends.
func (p *codexIncrementalPool) closeAll() {
	p.mu.Lock()
	keys := make([]string, 0, len(p.chains))
	for key := range p.chains {
		keys = append(keys, key)
	}
	p.sessions = make(map[string]map[string]struct{})
	p.mu.Unlock()
	for _, key := range keys {
		p.closeKey(key)
	}
}

// codexIncrementalPoolSize is a point-in-time view for the summary log.
type codexIncrementalPoolSize struct {
	chains, conns int
}

func (p *codexIncrementalPool) size() codexIncrementalPoolSize {
	p.mu.Lock()
	defer p.mu.Unlock()
	return codexIncrementalPoolSize{chains: p.total, conns: p.conns}
}
