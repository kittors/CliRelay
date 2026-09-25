package dnswatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// syncBuffer collects log output written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines returns the log lines containing every fragment.
func (b *syncBuffer) lines(fragments ...string) []string {
	var matched []string
	for line := range strings.SplitSeq(b.String(), "\n") {
		keep := line != ""
		for _, fragment := range fragments {
			keep = keep && strings.Contains(line, fragment)
		}
		if keep {
			matched = append(matched, line)
		}
	}
	return matched
}

// alertSink is a webhook receiver that keeps every alert and its raw body.
type alertSink struct {
	server *httptest.Server

	mu     sync.Mutex
	alerts []Alert
	bodies []string
}

func newAlertSink(t *testing.T) *alertSink {
	t.Helper()
	s := &alertSink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var alert Alert
		if err := json.Unmarshal(raw, &alert); err != nil {
			t.Errorf("alert is not JSON: %v: %s", err, raw)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("alert must be a JSON POST, got %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		s.mu.Lock()
		s.alerts = append(s.alerts, alert)
		s.bodies = append(s.bodies, string(raw))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// waitFor returns the first alert with the given event.
func (s *alertSink) waitFor(t *testing.T, event string) Alert {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		for _, alert := range s.alerts {
			if alert.Event == event {
				s.mu.Unlock()
				return alert
			}
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no %q alert within 5s; got %v", event, s.events())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *alertSink) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]string, 0, len(s.alerts))
	for _, alert := range s.alerts {
		events = append(events, alert.Event)
	}
	return events
}

func (s *alertSink) rawBodies() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.bodies, "\n")
}

// harness wires a Watcher to a fake Cloudflare zone and two TLS nodes:
// node-a at 203.0.113.11 and node-b at 203.0.113.12.
type harness struct {
	t      *testing.T
	ctx    context.Context
	cf     *fakeCloudflare
	pki    *testPKI
	router *dialRouter
	nodeA  *fakeNode
	nodeB  *fakeNode
	clock  *fakeClock
	logs   *syncBuffer
	alerts *alertSink
	cfg    *Config
	w      *Watcher
}

type harnessOptions struct {
	// seed fills the zone before start; nil publishes both nodes under both
	// hostnames.
	seed func(*fakeCloudflare)
	// config adjusts the config before New.
	config func(*Config)
	// realClock uses time.Now instead of the fake clock, for Run tests.
	realClock bool
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		ctx:    context.Background(),
		cf:     newFakeCloudflare(t),
		pki:    newTestPKI(t, testRelay, testCode),
		router: newDialRouter(),
		clock:  newFakeClock(),
		logs:   &syncBuffer{},
		alerts: newAlertSink(t),
	}
	h.nodeA, h.nodeB = newFakeNode(t, h.pki), newFakeNode(t, h.pki)
	h.router.route(testIPA, h.nodeA)
	h.router.route(testIPB, h.nodeB)
	seed := opts.seed
	if seed == nil {
		seed = publishBoth
	}
	seed(h.cf)

	h.cfg = &Config{
		Cloudflare: CloudflareConfig{APITokenFile: "/etc/clirelay-dnswatch/cf-token", ZoneID: testZoneID, APIBaseURL: h.cf.server.URL},
		Records:    []string{testRelay, testCode},
		Nodes:      []Node{{Name: "node-a", IP: testIPA}, {Name: "node-b", IP: testIPB}},
		Probe:      ProbeConfig{Interval: 10 * time.Second, Timeout: 2 * time.Second},
		// Debouncing has its own test; elsewhere it must not interfere.
		MinChangeInterval: time.Millisecond,
		AlertWebhook:      h.alerts.server.URL,
	}
	if opts.config != nil {
		opts.config(h.cfg)
	}
	now := h.clock.Now
	if opts.realClock {
		now = nil
	}
	w, err := New(h.cfg, testToken, Options{
		Logger:       slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:          now,
		RootCAs:      h.pki.roots,
		DialContext:  h.router.dial,
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(w.Close)
	h.w = w
	return h
}

// publishBoth lists both nodes under both hostnames.
func publishBoth(f *fakeCloudflare) {
	for _, name := range []string{testRelay, testCode} {
		f.seed(name, "A", testIPA)
		f.seed(name, "A", testIPB)
	}
}

// publishOnlyA lists node-a alone under both hostnames.
func publishOnlyA(f *fakeCloudflare) {
	for _, name := range []string{testRelay, testCode} {
		f.seed(name, "A", testIPA)
	}
}

// round advances the fake clock by one interval and runs one round.
func (h *harness) round() {
	h.clock.Advance(h.cfg.Probe.Interval)
	h.w.RunOnce(h.ctx)
}

func (h *harness) rounds(n int) {
	for range n {
		h.round()
	}
}

func (h *harness) expectIPs(name string, want ...string) {
	h.t.Helper()
	slices.Sort(want)
	if got := h.cf.aIPs(name); !slices.Equal(got, want) {
		h.t.Fatalf("%s A records = %v, want %v", name, got, want)
	}
}

func (h *harness) expectBothRecords(want ...string) {
	h.t.Helper()
	h.expectIPs(testRelay, want...)
	h.expectIPs(testCode, want...)
}

func (h *harness) nodeStatus(name string) NodeStatus {
	h.t.Helper()
	for _, node := range h.w.Status().Nodes {
		if node.Name == name {
			return node
		}
	}
	h.t.Fatalf("no status for node %s", name)
	return NodeStatus{}
}

// writesOf returns the Cloudflare writes whose method matches.
func (h *harness) writesOf(method string) []string {
	var matched []string
	for _, write := range h.cf.writeLog() {
		if strings.HasPrefix(write, method+" ") {
			matched = append(matched, write)
		}
	}
	return matched
}
