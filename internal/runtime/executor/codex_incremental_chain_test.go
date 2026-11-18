package executor

import (
	"testing"
	"time"
)

type incPoolClock struct{ now time.Time }

func (c *incPoolClock) Now() time.Time { return c.now }

func newTestIncrementalPool() (*codexIncrementalPool, *incPoolClock) {
	clock := &incPoolClock{now: time.Unix(1_000_000, 0)}
	pool := newCodexIncrementalPool()
	pool.now = clock.Now
	return pool, clock
}

var incTestLimits = codexIncrementalLimits{idleTimeout: 5 * time.Minute, maxConns: 3, maxConnsAuth: 2}

// completeTurn releases chain with a base made of req and the reply below.
func completeTurn(t *testing.T, pool *codexIncrementalPool, chain *codexIncrementalChain, req *codexIncrementalRequest) {
	t.Helper()
	projection, unusable := projectCodexOutputItems([]string{incOutMessage})
	pool.release(chain, codexIncrementalRelease{base: &codexIncrementalBase{items: req.itemDigests(), props: req.props, responseID: "resp", output: projection, unusable: unusable}})
}

func TestCodexIncrementalPoolFollowsOneConversation(t *testing.T) {
	pool, clock := newTestIncrementalPool()
	turn1 := mustParseIncremental(t, incBody(incDeveloper, incUser1))
	turn2 := mustParseIncremental(t, incBody(incDeveloper, incUser1, incChatMessage, incUser2))

	chain, got, _ := pool.acquire("conv", "", "auth-a", turn1, incTestLimits)
	if got != codexChainNew || chain == nil {
		t.Fatalf("first turn: %v", got)
	}
	completeTurn(t, pool, chain, turn1)

	again, got, _ := pool.acquire("conv", "", "auth-a", turn2, incTestLimits)
	if got != codexChainFound || again != chain {
		t.Fatalf("second turn: %v, same chain %t", got, again == chain)
	}
	// While the turn is out, a request that extends the same base waits for
	// nothing: it goes out on its own.
	if _, got, _ := pool.acquire("conv", "", "auth-a", turn2, incTestLimits); got != codexChainBusy {
		t.Fatalf("concurrent turn: %v, want busy", got)
	}
	completeTurn(t, pool, again, turn2)

	// The conversation moved to another account: the old chain goes.
	turn3 := mustParseIncremental(t, incBody(incDeveloper, incUser1, incChatMessage, incUser2, incChatMessage, incUser2))
	moved, got, line := pool.acquire("conv", "", "auth-b", turn3, incTestLimits)
	if got != codexChainNew || moved == chain || line.reason != "account_changed" {
		t.Fatalf("account switch: %v %+v, reused old chain %t", got, line, moved == chain)
	}
	completeTurn(t, pool, moved, turn3)
	if size := pool.size(); size.chains != 1 {
		t.Fatalf("chains = %d, want the old account's chain gone", size.chains)
	}

	// Idle past the timeout: the next turn starts over.
	clock.now = clock.now.Add(6 * time.Minute)
	if _, got, _ := pool.acquire("conv", "", "auth-b", turn3, incTestLimits); got != codexChainNew {
		t.Fatalf("after idle timeout: %v, want a new chain", got)
	}
}

func TestCodexIncrementalPoolKeepsSideLinesApart(t *testing.T) {
	pool, _ := newTestIncrementalPool()
	main1 := mustParseIncremental(t, incBody(incDeveloper, incUser1))
	side1 := mustParseIncremental(t, incBody(incDeveloper, incUser2))

	mainChain, _, _ := pool.acquire("conv", "", "auth-a", main1, incTestLimits)
	completeTurn(t, pool, mainChain, main1)
	sideChain, got, line := pool.acquire("conv", "", "auth-a", side1, incTestLimits)
	if got != codexChainNew || sideChain == mainChain {
		t.Fatalf("side request took the main chain")
	}
	if line.reason != "input_prefix_mismatch" || line.prevItems != 2 || line.mismatchAt != 1 {
		t.Fatalf("new line = %+v, want a prefix mismatch at item 1 of 2", line)
	}
	completeTurn(t, pool, sideChain, side1)

	main2 := mustParseIncremental(t, incBody(incDeveloper, incUser1, incChatMessage, incUser2))
	if chain, got, _ := pool.acquire("conv", "", "auth-a", main2, incTestLimits); got != codexChainFound || chain != mainChain {
		t.Fatalf("main line lost its base to the side request: %v", got)
	}
}

func TestCodexIncrementalPoolReusesTheOldestLine(t *testing.T) {
	pool, clock := newTestIncrementalPool()
	var first *codexIncrementalChain
	for i := 0; i < codexIncrementalChainsPerConversation; i++ {
		req := mustParseIncremental(t, incBody(incDeveloper, `{"type":"message","role":"user","content":"line `+string(rune('a'+i))+`"}`))
		chain, _, _ := pool.acquire("conv", "", "auth-a", req, incTestLimits)
		if first == nil {
			first = chain
		}
		completeTurn(t, pool, chain, req)
		clock.now = clock.now.Add(time.Second)
	}
	extra := mustParseIncremental(t, incBody(incDeveloper, `{"type":"message","role":"user","content":"line z"}`))
	chain, got, _ := pool.acquire("conv", "", "auth-a", extra, incTestLimits)
	if got != codexChainNew || chain != first || chain.base != nil {
		t.Fatalf("extra line: %v, reused oldest %t", got, chain == first)
	}
	if size := pool.size(); size.chains != codexIncrementalChainsPerConversation {
		t.Fatalf("chains = %d", size.chains)
	}
}

func TestCodexIncrementalPoolBoundsConnections(t *testing.T) {
	pool, clock := newTestIncrementalPool()
	conns := map[string]*codexIncrementalConn{}
	open := func(key, authID string) *codexIncrementalChain {
		t.Helper()
		req := mustParseIncremental(t, incBody(`{"type":"message","role":"user","content":"`+key+`"}`))
		chain, _, _ := pool.acquire(key, "", authID, req, incTestLimits)
		if !pool.reserveConn(chain, incTestLimits) {
			t.Fatalf("reserveConn(%s) refused", key)
		}
		conn := &codexIncrementalConn{serial: codexIncrementalConnSerial.Add(1), identity: codexIncrementalConnIdentity{authID: authID}, createdAt: clock.now, done: make(chan struct{})}
		pool.attachConn(chain, conn)
		conns[key] = conn
		pool.release(chain, codexIncrementalRelease{})
		clock.now = clock.now.Add(time.Second)
		return chain
	}
	open("a1", "auth-a")
	open("a2", "auth-a")
	// The account's third connection closes its least recently used one.
	open("a3", "auth-a")
	if !conns["a1"].isClosed() || conns["a2"].isClosed() {
		t.Fatalf("per-account bound: a1 closed=%t a2 closed=%t", conns["a1"].isClosed(), conns["a2"].isClosed())
	}
	open("b1", "auth-b")
	// The global bound (3) closes the oldest connection of any account.
	open("b2", "auth-b")
	if !conns["a2"].isClosed() {
		t.Fatal("global bound left the oldest connection open")
	}
	if size := pool.size(); size.conns != 3 {
		t.Fatalf("conns = %d, want 3", size.conns)
	}
}

func TestCodexIncrementalPoolRefusesWhenEveryConnectionIsBusy(t *testing.T) {
	pool, _ := newTestIncrementalPool()
	limits := codexIncrementalLimits{idleTimeout: time.Minute, maxConns: 1, maxConnsAuth: 1}
	reqA := mustParseIncremental(t, incBody(`{"type":"message","role":"user","content":"a"}`))
	chainA, _, _ := pool.acquire("a", "", "auth-a", reqA, limits)
	if !pool.reserveConn(chainA, limits) {
		t.Fatal("first reservation refused")
	}
	pool.attachConn(chainA, &codexIncrementalConn{serial: codexIncrementalConnSerial.Add(1), identity: codexIncrementalConnIdentity{authID: "auth-a"}, done: make(chan struct{})})

	reqB := mustParseIncremental(t, incBody(`{"type":"message","role":"user","content":"b"}`))
	chainB, _, _ := pool.acquire("b", "", "auth-a", reqB, limits)
	if pool.reserveConn(chainB, limits) {
		t.Fatal("reservation granted although the only connection is serving a turn")
	}
	pool.release(chainA, codexIncrementalRelease{})
	if !pool.reserveConn(chainB, limits) {
		t.Fatal("reservation refused after the busy connection went idle")
	}
}

func TestCodexIncrementalPoolClosesSessionChains(t *testing.T) {
	pool, _ := newTestIncrementalPool()
	req := mustParseIncremental(t, incBody(incDeveloper, incUser1))
	chain, _, _ := pool.acquire("conv", "ws-session", "auth-a", req, incTestLimits)
	conn := &codexIncrementalConn{serial: codexIncrementalConnSerial.Add(1), identity: codexIncrementalConnIdentity{authID: "auth-a"}, done: make(chan struct{})}
	if !pool.reserveConn(chain, incTestLimits) {
		t.Fatal("reserveConn refused")
	}
	pool.attachConn(chain, conn)

	// The client closes its socket while the turn is still streaming.
	pool.closeSession("ws-session")
	if conn.isClosed() {
		t.Fatal("closing the session cut the turn in progress")
	}
	completeTurn(t, pool, chain, req)
	if !conn.isClosed() {
		t.Fatal("connection of a closed session stayed open after its turn")
	}
	if size := pool.size(); size.chains != 0 || size.conns != 0 {
		t.Fatalf("pool after session close: %+v", size)
	}
}
