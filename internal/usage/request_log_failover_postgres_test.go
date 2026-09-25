package usage

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// Failover drills for the request log write path against a real PostgreSQL.
//
// They need CLIRELAY_POSTGRES_TEST_DSN. The container drills additionally
// need CLIRELAY_POSTGRES_TEST_CONTAINER, the name of a disposable container
// behind that DSN with a fixed host port: they stop, start, pause and unpause
// it.

// pgFaultProxy sits between the gateway and PostgreSQL and injects the faults
// of a failover: every connection cut at once with new ones refused, or a
// COMMIT that the server applies while its reply never reaches the client.
type pgFaultProxy struct {
	target   string
	listener net.Listener

	mu    sync.Mutex
	conns map[*pgProxyConn]struct{}

	down                 atomic.Bool
	dropNextCommitReply  atomic.Bool
	droppedCommitReplies atomic.Int32
}

type pgProxyConn struct {
	client, server net.Conn
	// dropReply is set before a COMMIT is forwarded: the next bytes from the
	// server are its reply, which the client must never see.
	dropReply atomic.Bool
	closeOnce sync.Once
}

func (c *pgProxyConn) close() {
	c.closeOnce.Do(func() {
		_ = c.client.Close()
		_ = c.server.Close()
	})
}

func startPGFaultProxy(t *testing.T, target string) *pgFaultProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &pgFaultProxy{target: target, listener: listener, conns: map[*pgProxyConn]struct{}{}}
	go p.accept()
	t.Cleanup(func() {
		_ = listener.Close()
		p.cutAll()
	})
	return p
}

func (p *pgFaultProxy) addr() string { return p.listener.Addr().String() }

// setDown cuts every open connection and refuses new ones while down.
func (p *pgFaultProxy) setDown(down bool) {
	p.down.Store(down)
	if down {
		p.cutAll()
	}
}

func (p *pgFaultProxy) cutAll() {
	p.mu.Lock()
	conns := make([]*pgProxyConn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (p *pgFaultProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			_ = client.Close()
			continue
		}
		server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		c := &pgProxyConn{client: client, server: server}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		go p.clientToServer(c)
		go p.serverToClient(c)
	}
}

func (p *pgFaultProxy) forget(c *pgProxyConn) {
	c.close()
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// clientToServer forwards frontend messages and watches for COMMIT. The
// startup message has no type byte (the DSN disables TLS, so no SSLRequest
// precedes it); every later message is a type byte plus a length.
func (p *pgFaultProxy) clientToServer(c *pgProxyConn) {
	defer p.forget(c)
	r := bufio.NewReader(c.client)
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return
	}
	startup := make([]byte, binary.BigEndian.Uint32(lengthBuf[:])-4)
	if _, err := io.ReadFull(r, startup); err != nil {
		return
	}
	if _, err := c.server.Write(append(lengthBuf[:], startup...)); err != nil {
		return
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return
		}
		payload := make([]byte, binary.BigEndian.Uint32(header[1:])-4)
		if _, err := io.ReadFull(r, payload); err != nil {
			return
		}
		if header[0] == 'Q' && strings.EqualFold(strings.TrimRight(string(payload), "\x00; \n"), "commit") &&
			p.dropNextCommitReply.CompareAndSwap(true, false) {
			c.dropReply.Store(true)
			p.droppedCommitReplies.Add(1)
		}
		if _, err := c.server.Write(append(header[:], payload...)); err != nil {
			return
		}
	}
}

// serverToClient copies backend bytes. Once a COMMIT reply is to be dropped,
// the first bytes from the server are that reply: the server answers only
// after the commit is durable, so dropping them and cutting the connection
// leaves a committed transaction the client believes failed.
func (p *pgFaultProxy) serverToClient(c *pgProxyConn) {
	defer p.forget(c)
	buf := make([]byte, 32<<10)
	for {
		n, err := c.server.Read(buf)
		if n > 0 {
			if c.dropReply.Load() {
				return
			}
			if _, werr := c.client.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// setupPostgresFailoverDrill opens the runtime database through a fault proxy
// and clears the tables the drills count.
func setupPostgresFailoverDrill(t *testing.T) *pgFaultProxy {
	t.Helper()
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	proxy := startPGFaultProxy(t, parsed.Host)
	parsed.Host = proxy.addr()
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: parsed.String(), MaxOpenConns: 4, MaxIdleConns: 2}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()
	if _, err := getDB().Exec(`TRUNCATE request_log_content, request_logs, usage_rollup_buckets,
		request_log_idempotency_keys, period_window_anchors, api_keys RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	resetUsageWriteStateForTest(t)
	return proxy
}

// runUsageSpoolReplayer starts the background replayer with drill timings.
func runUsageSpoolReplayer(t *testing.T) *usageSpool {
	t.Helper()
	prevMin, prevMax, prevIdle := usageSpoolReplayMinBackoff, usageSpoolReplayMaxBackoff, usageSpoolIdlePoll
	usageSpoolReplayMinBackoff, usageSpoolReplayMaxBackoff, usageSpoolIdlePoll = 50*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		usageSpoolReplayMinBackoff, usageSpoolReplayMaxBackoff, usageSpoolIdlePoll = prevMin, prevMax, prevIdle
	})
	spool := useUsageSpoolForTest(t, "", 0)
	replayer := usageSpoolCurrent.Load().replayer
	go replayer.run()
	t.Cleanup(replayer.close)
	return spool
}

func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func insertDrillRecords(prefix string, n int, tokens int64) {
	for i := 0; i < n; i++ {
		InsertRequestLog(resilienceTestEntry(fmt.Sprintf("%s-%d", prefix, i), prefix, tokens, time.Now()))
	}
}

// assertDrillTotals checks rows and every projection count exactly once.
func assertDrillTotals(t *testing.T, wantRows, wantTokens int64) {
	t.Helper()
	if n := countRequestLogRows(t); n != wantRows {
		t.Fatalf("request_logs rows = %d, want %d", n, wantRows)
	}
	requests, tokens := lifetimeRollupTotals(t)
	if requests != wantRows || tokens != wantTokens {
		t.Fatalf("lifetime rollup = %d requests / %d tokens, want %d / %d", requests, tokens, wantRows, wantTokens)
	}
	var dayRequests int64
	if err := getDB().QueryRow(`SELECT COALESCE(SUM(request_count), 0) FROM usage_rollup_buckets WHERE bucket_kind = ?`, rollupBucketDay).Scan(&dayRequests); err != nil {
		t.Fatalf("read day rollup: %v", err)
	}
	if dayRequests != wantRows {
		t.Fatalf("day rollup requests = %d, want %d", dayRequests, wantRows)
	}
	var keys int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_log_idempotency_keys`).Scan(&keys); err != nil {
		t.Fatalf("count idempotency keys: %v", err)
	}
	if keys != wantRows {
		t.Fatalf("idempotency keys = %d, want %d", keys, wantRows)
	}
}

// The server commits, the reply is lost, the client retries: the row and its
// rollups must exist once.
func TestPostgresLostCommitReplyIsNotCountedTwice(t *testing.T) {
	proxy := setupPostgresFailoverDrill(t)
	spool := useUsageSpoolForTest(t, "", 0)

	proxy.dropNextCommitReply.Store(true)
	InsertRequestLog(resilienceTestEntry("lost-ack", "lost-ack", 10, time.Now()))
	if got := proxy.droppedCommitReplies.Load(); got != 1 {
		t.Fatalf("dropped COMMIT replies = %d, want 1", got)
	}
	assertDrillTotals(t, 1, 11)
	// The retry found its own key right after a lost reply; it is re-checked
	// once the settle window has passed rather than trusted on the spot.
	if records, _, _ := spool.pending(); records != 1 {
		t.Fatalf("records awaiting the commit re-check = %d, want 1", records)
	}
	if err := newUsageSpoolReplayer(spool).drain(); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	assertDrillTotals(t, 1, 11)
	if got := spool.duplicates.Load(); got != 1 {
		t.Fatalf("duplicates = %d, want the stored row confirmed", got)
	}
}

// Connections are cut and new ones refused for longer than the retry budget,
// as during a failover: nothing is lost, nothing counted twice.
func TestPostgresRequestLogsSurviveALostPrimary(t *testing.T) {
	proxy := setupPostgresFailoverDrill(t)
	spool := runUsageSpoolReplayer(t)

	insertDrillRecords("before", 5, 10)
	assertDrillTotals(t, 5, 55)

	proxy.setDown(true)
	started := time.Now()
	insertDrillRecords("during", 5, 20)
	if elapsed := time.Since(started); elapsed > 3*requestLogLiveRetryBudget+2*time.Second {
		t.Fatalf("writes during the outage took %s, want one retry budget then the spool", elapsed)
	}
	if records, _, _ := spool.pending(); records != 5 {
		t.Fatalf("spooled records = %d, want 5", records)
	}
	if !usageDBHealth.down() {
		t.Fatal("outage flag not raised")
	}

	proxy.setDown(false)
	waitForCondition(t, 20*time.Second, "the spool to drain", func() bool {
		records, _, _ := spool.pending()
		return records == 0
	})
	insertDrillRecords("after", 2, 30)
	assertDrillTotals(t, 12, 5*11+5*21+2*31)
	if usageDBHealth.down() {
		t.Fatal("outage flag still raised after recovery")
	}
}

func dockerTestContainer(t *testing.T) string {
	t.Helper()
	container := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_CONTAINER"))
	if container == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_CONTAINER is not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	return container
}

func dockerRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func waitForPostgresReady(t *testing.T, container string) {
	t.Helper()
	waitForCondition(t, 60*time.Second, "postgres to accept connections", func() bool {
		return exec.Command("docker", "exec", container, "pg_isready", "-h", "127.0.0.1", "-U", "cliproxy", "-d", "cliproxy").Run() == nil
	})
}

// A real fast shutdown and restart, which is what Patroni does to a fenced
// primary: pooled connections get 57P01, new ones are refused until the
// server is back.
func TestPostgresRequestLogsSurviveAServerRestart(t *testing.T) {
	container := dockerTestContainer(t)
	setupPostgresFailoverDrill(t)
	spool := runUsageSpoolReplayer(t)
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			_ = exec.Command("docker", "start", container).Run()
		}
	})

	insertDrillRecords("before", 5, 10)
	assertDrillTotals(t, 5, 55)

	dockerRun(t, "stop", "-t", "10", container)
	insertDrillRecords("during", 5, 20)
	if records, _, _ := spool.pending(); records != 5 {
		t.Fatalf("spooled records = %d, want 5", records)
	}

	dockerRun(t, "start", container)
	restarted = true
	waitForPostgresReady(t, container)
	waitForCondition(t, 60*time.Second, "the spool to drain", func() bool {
		records, _, _ := spool.pending()
		return records == 0
	})
	insertDrillRecords("after", 2, 30)
	assertDrillTotals(t, 12, 5*11+5*21+2*31)
}

// A primary that stops answering without closing its sockets (a network
// partition): writes hang until the attempt deadline, then spool.
func TestPostgresRequestLogsSurviveAnUnresponsivePrimary(t *testing.T) {
	container := dockerTestContainer(t)
	setupPostgresFailoverDrill(t)
	prevTimeout := requestLogWriteAttemptTimeout
	requestLogWriteAttemptTimeout = time.Second
	t.Cleanup(func() { requestLogWriteAttemptTimeout = prevTimeout })
	requestLogLiveRetryBudget = 2 * time.Second
	spool := runUsageSpoolReplayer(t)
	paused := false
	t.Cleanup(func() {
		if paused {
			_ = exec.Command("docker", "unpause", container).Run()
		}
	})

	insertDrillRecords("before", 3, 10)
	dockerRun(t, "pause", container)
	paused = true
	started := time.Now()
	insertDrillRecords("during", 3, 20)
	if elapsed := time.Since(started); elapsed > 3*requestLogLiveRetryBudget+2*requestLogWriteAttemptTimeout {
		t.Fatalf("writes against a hung primary took %s; the attempt deadline did not bound them", elapsed)
	}
	if records, _, _ := spool.pending(); records != 3 {
		t.Fatalf("spooled records = %d, want 3", records)
	}
	dockerRun(t, "unpause", container)
	paused = false
	waitForCondition(t, 60*time.Second, "the spool to drain", func() bool {
		records, _, _ := spool.pending()
		return records == 0
	})
	assertDrillTotals(t, 6, 3*11+3*21)
}
