package sharedstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// realRedisEnv points the script tests at a real Redis (for example a local
// redis:7-alpine container) instead of miniredis, to check that the Lua
// behaves the same on the real interpreter. Tests use unique keys, so the
// database is never flushed.
const realRedisEnv = "CLIRELAY_REDIS_TEST_ADDR"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	// Mid-minute, so tests that do not care about the minute boundary are not
	// affected by it.
	return &fakeClock{now: time.Date(2026, 9, 24, 12, 0, 30, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

type testEnv struct {
	mr    *miniredis.Miniredis // nil against a real Redis
	addr  string
	clock *fakeClock
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{clock: newFakeClock()}
	if addr := strings.TrimSpace(os.Getenv(realRedisEnv)); addr != "" {
		env.addr = addr
		return env
	}
	env.mr = miniredis.RunT(t)
	env.addr = env.mr.Addr()
	return env
}

// node builds one node's store on the shared Redis.
func (e *testEnv) node(t *testing.T, nodeID string) *Store {
	t.Helper()
	client, err := sharedredis.New(config.ClusterRedisConfig{Addr: e.addr}, sharedredis.Options{
		NodeID:         nodeID,
		HealthInterval: 50 * time.Millisecond,
		MinBackoff:     20 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		StableAfter:    time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	if !client.Available() {
		t.Fatalf("test redis at %s unavailable: %+v", e.addr, client.Status())
	}
	store := New(client, Options{NodeID: nodeID, Now: e.clock.Now, FlushInterval: time.Hour})
	t.Cleanup(func() {
		store.Close()
		client.Close()
	})
	return store
}

// requireMiniredis skips assertions that need miniredis-only controls such as
// fast-forwarding key TTLs.
func (e *testEnv) requireMiniredis(t *testing.T) {
	t.Helper()
	if e.mr == nil {
		t.Skip("needs miniredis to control key expiry")
	}
}

// unique returns a per-test identifier so tests never share keys, even on a
// long-lived real Redis.
func unique(t *testing.T, label string) string {
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	return t.Name() + "/" + label + "/" + hex.EncodeToString(buf[:])
}

var bg = context.Background()
