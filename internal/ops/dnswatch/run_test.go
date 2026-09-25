package dnswatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunLoopPullsFailedNodeAndStopsOnCancel(t *testing.T) {
	h := newHarness(t, harnessOptions{realClock: true, config: func(c *Config) {
		c.Probe.Interval = 150 * time.Millisecond
		c.Probe.Timeout = 100 * time.Millisecond
		c.StatusListen = "127.0.0.1:0"
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()

	h.nodeB.setStatus(http.StatusServiceUnavailable)
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Equal(h.cf.aIPs(testRelay), []string{testIPA}) || !slices.Equal(h.cf.aIPs(testCode), []string{testIPA}) {
		if time.Now().After(deadline) {
			t.Fatalf("node-b was not pulled by the run loop; records %v / %v", h.cf.aIPs(testRelay), h.cf.aIPs(testCode))
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	for _, fragment := range []string{"dnswatch starting", "status endpoint listening", "dnswatch stopping"} {
		if !strings.Contains(h.logs.String(), fragment) {
			t.Fatalf("missing %q in the run log:\n%s", fragment, h.logs.String())
		}
	}
	if start := h.logs.lines("dnswatch starting"); len(start) != 1 || !strings.Contains(start[0], "records="+testRelay+","+testCode) {
		t.Fatalf("the start line should describe the config, got %q", start)
	}
}

func TestRunFailsWhenStatusAddressIsTaken(t *testing.T) {
	busy := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(busy.Close)
	h := newHarness(t, harnessOptions{config: func(c *Config) {
		c.StatusListen = strings.TrimPrefix(busy.URL, "http://")
	}})
	if err := h.w.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "status_listen") {
		t.Fatalf("expected the listen error, got %v", err)
	}
}

func TestStatusEndpoint(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: func(f *fakeCloudflare) {
		publishBoth(f)
		f.seed(testRelay, "A", "198.51.100.7")
	}})
	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.round()

	server := httptest.NewServer(h.w.StatusHandler())
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	decodeErr := json.NewDecoder(resp.Body).Decode(&status)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil {
		t.Fatalf("GET /status = %d, decode error %v", resp.StatusCode, decodeErr)
	}
	if !status.Initialized || status.HealthyNodes != 2 || len(status.Nodes) != 2 || !status.LastReconcile.OK {
		t.Fatalf("unexpected status: %+v", status)
	}
	nodeB := status.Nodes[1]
	if nodeB.Name != "node-b" || !nodeB.Healthy || nodeB.ConsecutiveFailures != 1 || nodeB.LastStatus != 503 ||
		nodeB.LastError != "unexpected status 503" || nodeB.LastProbe == nil {
		t.Fatalf("unexpected node-b status: %+v", nodeB)
	}
	relay := status.Records[testRelay]
	if !slices.Equal(relay.Nodes, []string{testIPA, testIPB}) || !slices.Equal(relay.Other, []string{"198.51.100.7"}) {
		t.Fatalf("unexpected record status: %+v", relay)
	}

	for method, want := range map[string]int{http.MethodPost: http.StatusMethodNotAllowed, http.MethodDelete: http.StatusMethodNotAllowed} {
		req, _ := http.NewRequest(method, server.URL+"/status", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s /status = %d, want %d", method, resp.StatusCode, want)
		}
	}
	resp, err = http.Get(server.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /other = %d, want 404", resp.StatusCode)
	}
}

func TestStatusSnapshotsAreIndependentCopies(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.round()
	first := h.w.Status()
	first.Nodes[0].Name = "mutated"
	first.Records[testRelay].Nodes[0] = "mutated"
	second := h.w.Status()
	if second.Nodes[0].Name != "node-a" || second.Records[testRelay].Nodes[0] != testIPA {
		t.Fatal("Status must return a copy the caller can modify")
	}
}

func TestSummaryIsLoggedEveryInterval(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	// The first round starts the clock for summaries; 60 more rounds of 10s
	// make ten minutes.
	h.rounds(60)
	if lines := h.logs.lines("dnswatch summary"); len(lines) != 0 {
		t.Fatalf("no summary expected before ten minutes, got %q", lines)
	}
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.round()
	lines := h.logs.lines("dnswatch summary")
	if len(lines) != 1 {
		t.Fatalf("expected one summary after ten minutes, got %q", lines)
	}
	for _, fragment := range []string{"healthy=node-a,node-b", "rounds=61", "probe_failures=1", "records=\"" + testRelay + "=[" + testIPA + " " + testIPB + "]"} {
		if !strings.Contains(lines[0], fragment) {
			t.Fatalf("summary %q lacks %q", lines[0], fragment)
		}
	}
	h.rounds(60)
	if lines := h.logs.lines("dnswatch summary"); len(lines) != 2 || !strings.Contains(lines[1], "rounds=60") {
		t.Fatalf("expected a second summary with fresh counters, got %q", lines)
	}
}

func TestApplyPlanNeverEmptiesADomain(t *testing.T) {
	// Defence in depth: a plan whose only healthy node is unconfirmed here
	// must not remove the last records, whatever led to that plan.
	h := newHarness(t, harnessOptions{seed: publishOnlyA})
	h.round()
	nodeA, nodeB := h.w.nodes[0], h.w.nodes[1]
	nodeA.healthy, nodeA.failures, nodeA.lastFailure = false, 3, "timeout"
	nodeB.healthy, nodeB.confirmed = true, false

	records, err := h.w.dns.listARecords(h.ctx, testRelay)
	if err != nil {
		t.Fatal(err)
	}
	plan := planDomain(testRelay, records, h.w.nodes)
	if len(plan.remove) != 1 || len(plan.add) != 0 || len(plan.unconfirmed) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	changes, err := h.w.applyPlan(h.ctx, plan, h.clock.Now())
	if err != nil || len(changes) != 0 {
		t.Fatalf("applyPlan = %v, %v; want no change", changes, err)
	}
	h.expectIPs(testRelay, testIPA)
	if lines := h.logs.lines("refusing to remove the last DNS records"); len(lines) != 1 {
		t.Fatalf("expected the refusal to be logged, got %q", lines)
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	cfg := &Config{
		Cloudflare: CloudflareConfig{APITokenFile: "/x/cf-token", ZoneID: testZoneID},
		Records:    []string{testRelay},
		Nodes:      []Node{{Name: "node-a", IP: testIPA}},
	}
	if _, err := New(cfg, "  ", Options{}); err == nil {
		t.Fatal("an empty token must be refused")
	}
	if _, err := New(nil, testToken, Options{}); err == nil {
		t.Fatal("a nil config must be refused")
	}
	bad := *cfg
	bad.Nodes = nil
	if _, err := New(&bad, testToken, Options{}); err == nil {
		t.Fatal("an invalid config must be refused")
	}
	if cfg.TTL != 0 {
		t.Fatal("New must not modify the caller's config")
	}
}
