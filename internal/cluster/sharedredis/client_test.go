package sharedredis

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

var incrScript = redis.NewScript(`return redis.call('INCR', KEYS[1])`)

func fastOptions() Options {
	return Options{
		NodeID:           "test-node",
		OpTimeout:        200 * time.Millisecond,
		HealthInterval:   50 * time.Millisecond,
		MinBackoff:       20 * time.Millisecond,
		MaxBackoff:       100 * time.Millisecond,
		StableAfter:      time.Millisecond,
		FailureWindow:    time.Second,
		FailureThreshold: 3,
		FailureRatio:     0.5,
		// Shorter than two op timeouts, so that the third of three commands
		// timing out back to back trips the streak.
		OutageAfter: 300 * time.Millisecond,
	}
}

// newHookedClient builds a client that is not started, so the test drives
// every probe itself, with a faultHook installed before any command runs.
func newHookedClient(t *testing.T, addr string, opts Options) (*Client, *faultHook) {
	t.Helper()
	c, err := New(config.ClusterRedisConfig{Addr: addr}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &faultHook{}
	c.Redis().AddHook(h)
	t.Cleanup(c.Close)
	return c, h
}

// faultHook fails chosen commands without sending them, to script what a
// lossy link or a sick server does to them.
type faultHook struct {
	mu   sync.Mutex
	fail func(cmd redis.Cmder) error
}

func (h *faultHook) set(fail func(cmd redis.Cmder) error) {
	h.mu.Lock()
	h.fail = fail
	h.mu.Unlock()
}

func (h *faultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *faultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		fail := h.fail
		h.mu.Unlock()
		if fail != nil {
			if err := fail(cmd); err != nil {
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (h *faultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// failHealthWrites fails the health probe's writes for which fail returns
// true, given how many health writes there have been including this one.
func failHealthWrites(counter *atomic.Int32, err error, fail func(n int32) bool) func(redis.Cmder) error {
	return func(cmd redis.Cmder) error {
		if cmd.Name() != "set" {
			return nil
		}
		if n := counter.Add(1); fail(n) {
			return err
		}
		return nil
	}
}

func failScripts(err error) func(redis.Cmder) error {
	return func(cmd redis.Cmder) error {
		if cmd.Name() == "evalsha" || cmd.Name() == "eval" {
			return err
		}
		return nil
	}
}

func (c *Client) backoffForTest() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backoff
}

func (c *Client) windowForTest() (ok, failed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures.totals(time.Now(), c.opts)
}

func newStartedClient(t *testing.T, addr string, opts Options) *Client {
	t.Helper()
	c, err := New(config.ClusterRedisConfig{Addr: addr}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start()
	t.Cleanup(c.Close)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientAvailableAfterStart(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newStartedClient(t, mr.Addr(), fastOptions())
	if !c.Available() {
		t.Fatalf("client must be available right after a successful start probe: %+v", c.Status())
	}
	res, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	if err != nil || res.(int64) != 1 {
		t.Fatalf("Eval = %v, %v", res, err)
	}
	if got, _ := mr.Get(KeyPrefix + "health:test-node"); got == "" {
		t.Fatalf("probe must write the node health key")
	}
}

func TestClientUnreachableAtStartStaysUnavailableWithoutBlocking(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there any more

	c := newStartedClient(t, addr, fastOptions())
	if c.Available() {
		t.Fatalf("unreachable server must start unavailable")
	}
	start := time.Now()
	_, err = c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Eval while down = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("Eval while down must not touch the network, took %s", elapsed)
	}
}

func TestClientRecoversAfterRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newStartedClient(t, mr.Addr(), fastOptions())
	if !c.Available() {
		t.Fatal("expected available")
	}

	// The pool drops the connections the server closed when it next hands one
	// out, so the command redials and the refusal marks the client down at
	// once. A connection whose close the pool has not seen yet fails that one
	// command only, hence the second chance.
	mr.Close()
	for i := 0; i < 2 && c.Available(); i++ {
		if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err == nil {
			t.Fatal("Eval against a closed server must fail")
		}
	}
	if c.Available() {
		t.Fatal("a refused dial must mark the client unavailable at once")
	}
	if st := c.Status(); !strings.Contains(st.LastError, "refused") {
		t.Fatalf("the refused dial must be what marked the client down, got %q", st.LastError)
	}
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("later calls must short-circuit, got %v", err)
	}

	if err := mr.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	waitFor(t, "recovery", c.Available)
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err != nil {
		t.Fatalf("Eval after recovery: %v", err)
	}
}

func TestClientScriptErrorDoesNotMarkDown(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newStartedClient(t, mr.Addr(), fastOptions())
	bad := redis.NewScript(`return redis.call('HGET', KEYS[1])`)
	if _, err := c.Eval(context.Background(), bad, []string{KeyPrefix + "t"}); err == nil {
		t.Fatal("malformed command must fail")
	}
	if !c.Available() {
		t.Fatal("a command the server rejected must not mark the server unavailable")
	}
}

// TestClientSingleTimeoutDoesNotMarkDown is one command lost on the link: it
// fails on its own and falls back, while the client stays available and the
// next command uses the server again.
func TestClientSingleTimeoutDoesNotMarkDown(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newBlackholeProxy(t, mr.Addr())
	c := newStartedClient(t, proxy.addr(), fastOptions())
	if !c.Available() {
		t.Fatal("expected available through the proxy")
	}
	since := c.Status().Since

	proxy.blackhole(true)
	start := time.Now()
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err == nil {
		t.Fatal("Eval through a stalled link must fail")
	}
	if elapsed := time.Since(start); elapsed > c.OpTimeout()+150*time.Millisecond {
		t.Fatalf("stalled Eval took %s, want about the op timeout %s", elapsed, c.OpTimeout())
	}
	if !c.Available() {
		t.Fatalf("one timed-out command must not mark the client down: %+v", c.Status())
	}

	// The link heals by dropping its connections. A pooled one whose close
	// the pool has not seen yet costs one more lost command, still below the
	// threshold.
	proxy.blackhole(false)
	var err error
	for i := 0; i < 2; i++ {
		if _, err = c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("the next command must use the server again, got %v", err)
	}
	if st := c.Status(); !st.Available || !st.Since.Equal(since) {
		t.Fatalf("the client must not have gone down and come back: %+v, available since %s", st, since)
	}
}

// TestClientSustainedTimeoutsMarkDown pins the requirement that a slow
// cluster Redis can delay a command by at most the op timeout, and requests
// only until the client has noticed: once the timeouts add up, requests skip
// Redis entirely.
func TestClientSustainedTimeoutsMarkDown(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newBlackholeProxy(t, mr.Addr())
	opts := fastOptions()
	c := newStartedClient(t, proxy.addr(), opts)
	if !c.Available() {
		t.Fatal("expected available through the proxy")
	}

	proxy.blackhole(true)
	for i := 1; i <= opts.FailureThreshold; i++ {
		start := time.Now()
		_, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("Eval through a stalled link must fail")
		}
		if elapsed > c.OpTimeout()+150*time.Millisecond {
			t.Fatalf("stalled Eval %d took %s, want about the op timeout %s", i, elapsed, c.OpTimeout())
		}
		if i < opts.FailureThreshold && !c.Available() {
			t.Fatalf("timeout %d of %d must not mark the client down yet", i, opts.FailureThreshold)
		}
	}
	if c.Available() {
		t.Fatal("timeouts adding up to the threshold with nothing succeeding must mark the client down")
	}
	start := time.Now()
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Eval once marked down = %v, want ErrUnavailable", err)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("Eval once marked down must not wait")
	}

	proxy.blackhole(false)
	waitFor(t, "recovery after the link heals", c.Available)
}

func TestIsConnectivityError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{redis.Nil, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, true},
		{io.EOF, true},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{&dialError{err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, true},
		{redis.ErrPoolTimeout, true},
		{replyError("WRONGTYPE Operation against a key holding the wrong kind of value"), false},
		{replyError("NOAUTH Authentication required."), true},
	}
	for _, tc := range cases {
		if got := IsConnectivityError(tc.err); got != tc.want {
			t.Errorf("IsConnectivityError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsConnectivityErrorServerReplies(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	_ = rdb.Set(ctx, "s", "v", 0).Err()
	_, wrongType := rdb.HGet(ctx, "s", "y").Result()
	if wrongType == nil || IsConnectivityError(wrongType) || classifyError(wrongType) != failureCommand {
		t.Fatalf("WRONGTYPE is a per-command error, got %v", wrongType)
	}

	mr.SetError("LOADING Redis is loading the dataset in memory")
	_, loading := rdb.Get(ctx, "s").Result()
	if loading == nil || !IsConnectivityError(loading) || classifyError(loading) != failureFatal {
		t.Fatalf("LOADING means the server is unusable, got %v", loading)
	}
}

// TestClientFatalReplyMarksDownAtOnce: a reply saying the server cannot serve
// anyone needs no window to be believed.
func TestClientFatalReplyMarksDownAtOnce(t *testing.T) {
	for _, reply := range []string{
		"NOAUTH Authentication required.",
		"WRONGPASS invalid username-password pair or user is disabled.",
		"LOADING Redis is loading the dataset in memory",
	} {
		mr := miniredis.RunT(t)
		c := newStartedClient(t, mr.Addr(), fastOptions())
		if !c.Available() {
			t.Fatal("expected available")
		}
		mr.SetError(reply)
		if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err == nil {
			t.Fatalf("%s: Eval must fail", reply)
		}
		if c.Available() {
			t.Fatalf("%s must mark the client down at once", reply)
		}
		mr.SetError("")
		waitFor(t, "recovery after "+reply, c.Available)
	}
}

// TestCallerDeadlineTimeoutDoesNotCount: a command cut short by the caller's
// own deadline, earlier than the op timeout, ran out of the caller's budget,
// not the server's.
func TestCallerDeadlineTimeoutDoesNotCount(t *testing.T) {
	mr := miniredis.RunT(t)
	c, h := newHookedClient(t, mr.Addr(), fastOptions())
	c.probe()
	if !c.Available() {
		t.Fatal("expected available")
	}
	h.set(failScripts(errLostReply))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	for i := 0; i < 5; i++ {
		if _, err := c.Eval(ctx, incrScript, []string{KeyPrefix + "t"}); err == nil {
			t.Fatal("Eval must fail")
		}
	}
	if !c.Available() {
		t.Fatal("timeouts under the caller's deadline must not mark the client down")
	}
	if _, failed := c.windowForTest(); failed != 0 {
		t.Fatalf("timeouts under the caller's deadline counted %d failures", failed)
	}

	// The same timeouts under our own deadline do count.
	_, _ = c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	if _, failed := c.windowForTest(); failed != 1 {
		t.Fatalf("a timeout under the op timeout counted %d failures, want 1", failed)
	}
}

// TestProbeRetriesALostAttempt is the recovery path on a lossy link: the
// warm write of the first attempt is lost, the second attempt gets through,
// and the server comes back on this probe rather than after another backoff.
func TestProbeRetriesALostAttempt(t *testing.T) {
	mr := miniredis.RunT(t)
	c, h := newHookedClient(t, mr.Addr(), fastOptions())
	var writes atomic.Int32
	h.set(failHealthWrites(&writes, errLostReply, func(n int32) bool { return n == 2 }))
	c.probe()
	if !c.Available() {
		t.Fatalf("a probe whose retry succeeds must bring the client up: %+v", c.Status())
	}
	if got := writes.Load(); got != 4 {
		t.Fatalf("health writes = %d, want 4 (two attempts of two)", got)
	}
}

func TestProbeLosingBothAttemptsKeepsTheClientDown(t *testing.T) {
	mr := miniredis.RunT(t)
	opts := fastOptions()
	c, h := newHookedClient(t, mr.Addr(), opts)
	var writes atomic.Int32
	h.set(failHealthWrites(&writes, errLostReply, func(int32) bool { return true }))
	c.probe()
	if c.Available() {
		t.Fatal("a probe losing both attempts must not bring the client up")
	}
	if got := writes.Load(); got != 2 {
		t.Fatalf("health writes = %d, want 2 (the first write of each attempt)", got)
	}
	if got := c.backoffForTest(); got != 2*opts.MinBackoff {
		t.Fatalf("backoff after a failed probe = %s, want %s", got, 2*opts.MinBackoff)
	}

	h.set(nil)
	c.probe()
	if !c.Available() {
		t.Fatal("the next probe that gets through must bring the client up")
	}
}

func TestProbeDoesNotRetryAFatalError(t *testing.T) {
	mr := miniredis.RunT(t)
	c, h := newHookedClient(t, mr.Addr(), fastOptions())
	var writes atomic.Int32
	h.set(failHealthWrites(&writes, replyError("NOAUTH Authentication required."), func(int32) bool { return true }))
	c.probe()
	if c.Available() {
		t.Fatal("a refused health write must not bring the client up")
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("health writes = %d, want 1: a fatal reply is not retried", got)
	}
}

// TestAvailableClientCountsALostProbe: while available, a probe that loses
// both attempts counts as one lost command, not as an outage.
func TestAvailableClientCountsALostProbe(t *testing.T) {
	mr := miniredis.RunT(t)
	c, h := newHookedClient(t, mr.Addr(), fastOptions())
	c.probe()
	if !c.Available() {
		t.Fatal("expected available")
	}
	var writes atomic.Int32
	h.set(failHealthWrites(&writes, errLostReply, func(int32) bool { return true }))
	c.probe()
	if !c.Available() {
		t.Fatal("one lost probe must not mark the client down")
	}
	if ok, failed := c.windowForTest(); ok != 1 || failed != 1 {
		t.Fatalf("window = %d ok, %d failed; want the first probe and the lost one", ok, failed)
	}

	// A probe the server refuses is fatal even while available.
	h.set(failHealthWrites(&writes, replyError("READONLY You can't write against a read only replica."), func(int32) bool { return true }))
	c.probe()
	if c.Available() {
		t.Fatal("a refused health write must mark the client down")
	}
}

// TestClientFlappingBacksOff: failing again soon after coming back is
// flapping. The backoff doubles at each such outage and the client does not
// probe at once, instead of flipping back on and off.
func TestClientFlappingBacksOff(t *testing.T) {
	mr := miniredis.RunT(t)
	opts := fastOptions()
	opts.StableAfter = time.Minute
	c, _ := newHookedClient(t, mr.Addr(), opts)
	c.probe()
	if !c.Available() {
		t.Fatal("expected available")
	}
	want := opts.MinBackoff
	for round := 1; round <= 2; round++ {
		mr.SetError("LOADING Redis is loading the dataset in memory")
		_, _ = c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
		if c.Available() {
			t.Fatalf("round %d: expected the client down", round)
		}
		want *= 2
		if got := c.backoffForTest(); got != want {
			t.Fatalf("round %d: backoff = %s, want %s", round, got, want)
		}
		select {
		case <-c.probeNow:
			t.Fatalf("round %d: a flapping client must wait out its backoff, not probe at once", round)
		default:
		}
		mr.SetError("")
		c.probe()
		if !c.Available() {
			t.Fatalf("round %d: expected recovery", round)
		}
	}

	// Once the server has stayed up for StableAfter, a new outage is fresh:
	// the backoff starts over and the client probes at once.
	c.mu.Lock()
	c.healthySince = time.Now().Add(-opts.StableAfter)
	c.mu.Unlock()
	mr.SetError("LOADING Redis is loading the dataset in memory")
	_, _ = c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	if got := c.backoffForTest(); got != opts.MinBackoff {
		t.Fatalf("fresh outage: backoff = %s, want %s", got, opts.MinBackoff)
	}
	select {
	case <-c.probeNow:
	default:
		t.Fatal("a fresh outage must probe at once")
	}
}

func TestHashPartIsStableAndOpaque(t *testing.T) {
	a := HashPart("sk-XXXXsecret")
	if a != HashPart("sk-XXXXsecret") || len(a) != 32 {
		t.Fatalf("HashPart must be stable and 32 hex chars, got %q", a)
	}
	if HashPart("a", "b") == HashPart("ab") {
		t.Fatal("HashPart must separate parts")
	}
}

func TestNilClientIsUnavailable(t *testing.T) {
	var c *Client
	if c.Available() {
		t.Fatal("nil client must be unavailable")
	}
	if _, err := c.Eval(context.Background(), incrScript, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil client Eval = %v", err)
	}
	c.Start()
	c.Close()
	c.Observe(errors.New("x"))
}

// blackholeProxy forwards TCP to a backend until blackhole(true), after which
// it keeps connections open but stops moving bytes in either direction: the
// behaviour of a congested or partitioned public link.
type blackholeProxy struct {
	ln      net.Listener
	backend string
	stalled atomic.Bool
	closed  atomic.Bool
	mu      sync.Mutex
	conns   []net.Conn
}

func newBlackholeProxy(t *testing.T, backend string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{ln: ln, backend: backend}
	go p.serve()
	t.Cleanup(func() {
		p.closed.Store(true)
		_ = ln.Close()
		p.mu.Lock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
	})
	return p
}

func (p *blackholeProxy) addr() string { return p.ln.Addr().String() }

func (p *blackholeProxy) blackhole(on bool) {
	p.stalled.Store(on)
	if on {
		return
	}
	// Healing drops the stalled connections so the client redials cleanly.
	p.mu.Lock()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
	p.mu.Unlock()
}

func (p *blackholeProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.backend)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.pipe(client, server)
		go p.pipe(server, client)
	}
}

func (p *blackholeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			_ = dst.Close()
			return
		}
		for p.stalled.Load() {
			if p.closed.Load() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func TestOpContextHonoursEarlierDeadlineButNotCancellation(t *testing.T) {
	c := &Client{opts: fastOptions().withDefaults()}
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	ctx, done, callerBound := c.opContext(parent, time.Second)
	defer done()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 60*time.Millisecond {
		t.Fatalf("an earlier caller deadline must bound the command, got %v", time.Until(deadline))
	}
	if !callerBound {
		t.Fatal("an earlier caller deadline must be reported")
	}
	cancel()
	if ctx.Err() != nil {
		t.Fatal("the caller's cancellation must not abort a command already sent")
	}

	long, cancelLong := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLong()
	ctx2, done2, callerBound2 := c.opContext(long, 200*time.Millisecond)
	defer done2()
	if d, _ := ctx2.Deadline(); time.Until(d) > 250*time.Millisecond {
		t.Fatal("a later caller deadline must not extend the op timeout")
	}
	if callerBound2 {
		t.Fatal("a later caller deadline does not bound the command")
	}
}
