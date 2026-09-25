package dnswatch

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const testEgressPath = "/readyz/egress"

// withEgress turns the egress probe on.
func withEgress(c *Config) { c.Probe.EgressPath = testEgressPath }

func TestEgressProbeAcceptsOnly2xx(t *testing.T) {
	pki := newTestPKI(t, testRelay)
	node := newFakeNode(t, pki)
	router := newDialRouter()
	router.route(testIPA, node)
	cfg := testProbeConfig(testRelay, 2*time.Second)
	cfg.EgressPath = testEgressPath
	p := newProber(cfg, pki.roots, router.dial)

	cases := []struct {
		status int
		ok     bool
	}{
		{http.StatusOK, true},
		{http.StatusNoContent, true},
		{http.StatusServiceUnavailable, false},
		{http.StatusNotFound, false},
		// Not followed: the node must answer the egress path itself.
		{http.StatusFound, false},
	}
	for _, tc := range cases {
		node.setEgressStatus(tc.status)
		result := p.probeEgress(context.Background(), testIPA)
		if result.ok != tc.ok || result.status != tc.status {
			t.Fatalf("egress status %d: got %+v, want ok=%v", tc.status, result, tc.ok)
		}
		if want := fmt.Sprintf("unexpected status %d", tc.status); !tc.ok && result.reason != want {
			t.Fatalf("egress status %d: reason = %q, want %q", tc.status, result.reason, want)
		}
	}
	seen := node.lastRequest()
	if seen.host != testRelay || seen.sni != testRelay || seen.path != testEgressPath {
		t.Fatalf("the egress probe must use the readiness probe's Host and SNI, node saw %+v", seen)
	}

	// A node whose release predates the endpoint answers 404 and fails.
	node.legacy.Store(true)
	if result := p.probeEgress(context.Background(), testIPA); result.ok || result.status != http.StatusNotFound {
		t.Fatalf("a legacy node must fail the egress probe, got %+v", result)
	}
}

func TestProbeRoundSendsBothProbesWithIndependentTimeouts(t *testing.T) {
	pki := newTestPKI(t, testRelay)
	slow := newFakeNode(t, pki)
	slow.delay.Store(int64(5 * time.Second))
	router := newDialRouter()
	router.route(testIPA, slow)

	timeout := 400 * time.Millisecond
	cfg := testProbeConfig(testRelay, timeout)
	cfg.EgressPath = testEgressPath
	start := time.Now()
	ready, egress := newProber(cfg, pki.roots, router.dial).probeRound(context.Background(), []Node{{Name: "slow", IP: testIPA}})
	if elapsed := time.Since(start); elapsed > 3*timeout {
		t.Fatalf("the round took %s; both probes should run at once", elapsed)
	}
	if ready[0].ok || ready[0].reason != "timeout" {
		t.Fatalf("the hung readiness probe should time out, got %+v", ready[0])
	}
	if len(egress) != 1 || !egress[0].ok {
		t.Fatalf("the egress probe must not wait for readiness, got %+v", egress)
	}

	// Without an egress path there is no egress probe at all.
	before := slow.egressHits.Load()
	_, egress = newProber(testProbeConfig(testRelay, timeout), pki.roots, router.dial).probeRound(context.Background(), []Node{{Name: "slow", IP: testIPA}})
	if egress != nil || slow.egressHits.Load() != before {
		t.Fatalf("egress results %v and %d new egress requests; want none without egress_path", egress, slow.egressHits.Load()-before)
	}
}

func TestEgressDegradedNodeLeavesDNSWhilePeerIsHealthy(t *testing.T) {
	h := newHarness(t, harnessOptions{config: func(c *Config) {
		withEgress(c)
		c.SummaryInterval = 30 * time.Second
	}})
	h.round()
	h.expectBothRecords(testIPA, testIPB)

	h.nodeB.setEgressStatus(http.StatusServiceUnavailable)
	h.rounds(2)
	h.expectBothRecords(testIPA, testIPB)
	if got := h.nodeStatus("node-b"); got.State != tierHealthy || got.Egress == nil || got.Egress.ConsecutiveFailures != 2 {
		t.Fatalf("node-b after two egress failures = %+v, want still healthy", got)
	}
	failed := h.logs.lines(`msg="egress probe failed"`, "node=node-b", "path="+testEgressPath, `reason="unexpected status 503"`, "fail_threshold=3")
	if len(failed) != 2 || !strings.Contains(failed[1], "consecutive_failures=2") {
		t.Fatalf("expected a warning per failed egress probe with the streak, got %q", failed)
	}

	h.round()
	h.expectBothRecords(testIPA)
	got := h.nodeStatus("node-b")
	if got.State != tierDegraded || !got.Healthy || got.Egress.OK || got.Egress.ConsecutiveFailures != 3 ||
		got.Egress.LastStatus != http.StatusServiceUnavailable || got.Egress.LastError != "unexpected status 503" {
		t.Fatalf("node-b should be degraded: ready, egress failing, got %+v / %+v", got, got.Egress)
	}
	if status := h.w.Status(); status.HealthyNodes != 1 || status.DegradedNodes != 1 {
		t.Fatalf("status counts healthy=%d degraded=%d, want 1/1", status.HealthyNodes, status.DegradedNodes)
	}
	degraded := h.alerts.waitFor(t, "node_egress_degraded")
	if degraded.Node != "node-b" || degraded.ConsecutiveFailures != 3 || !strings.Contains(degraded.Reason, "503") ||
		!slices.Equal(degraded.DegradedNodes, []string{"node-b"}) || !slices.Equal(degraded.HealthyNodes, []string{"node-a"}) {
		t.Fatalf("unexpected node_egress_degraded alert: %+v", degraded)
	}
	updated := h.alerts.waitFor(t, "dns_updated")
	if len(updated.Changes) != 2 || updated.Changes[0].Action != "remove" || updated.Changes[0].IP != testIPB ||
		updated.Changes[0].Reason != "node degraded: egress check failed: unexpected status 503" || updated.Changes[0].ConsecutiveFailures != 3 {
		t.Fatalf("unexpected dns_updated alert: %+v", updated)
	}
	if lines := h.logs.lines(`msg="node egress degraded"`, "node=node-b", "consecutive_failures=3"); len(lines) != 1 {
		t.Fatalf("expected one egress state change line, got %q", lines)
	}
	for _, name := range []string{testRelay, testCode} {
		lines := h.logs.lines(`msg="dns record removed"`, "domain="+name, "node=node-b", "consecutive_failures=3",
			`reason="node degraded: egress check failed: unexpected status 503"`)
		if len(lines) != 1 {
			t.Fatalf("expected one removal line for %s, got %q", name, lines)
		}
	}
	summary := h.logs.lines("dnswatch summary")
	if len(summary) != 1 {
		t.Fatalf("expected one summary, got %q", summary)
	}
	for _, fragment := range []string{"healthy=node-a", "degraded=node-b", `unhealthy=""`, "egress_failures=3"} {
		if !strings.Contains(summary[0], fragment) {
			t.Fatalf("summary %q lacks %q", summary[0], fragment)
		}
	}
}

func TestEgressDegradedNodeReturnsAfterRecoverThreshold(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress})
	h.round()
	h.nodeB.setEgressStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	h.expectBothRecords(testIPA)

	h.nodeB.setEgressStatus(http.StatusNoContent)
	h.rounds(2)
	h.expectBothRecords(testIPA)
	if got := h.nodeStatus("node-b"); got.State != tierDegraded || got.Egress.ConsecutiveSuccesses != 2 {
		t.Fatalf("node-b after two egress successes = %+v, want still degraded", got)
	}
	if lines := h.logs.lines("egress probe succeeded while egress is marked failing", "node=node-b"); len(lines) != 2 {
		t.Fatalf("expected a line per successful egress probe, got %q", lines)
	}

	h.round()
	h.expectBothRecords(testIPA, testIPB)
	if got := h.nodeStatus("node-b"); got.State != tierHealthy || !got.Egress.OK {
		t.Fatalf("node-b should be healthy again, got %+v", got)
	}
	up := h.alerts.waitFor(t, "node_egress_recovered")
	if up.Node != "node-b" || up.ConsecutiveSuccesses != 3 {
		t.Fatalf("unexpected node_egress_recovered alert: %+v", up)
	}
	if lines := h.logs.lines(`msg="dns record added"`, "node=node-b", "node egress recovered after 3 consecutive successful egress probes"); len(lines) != 2 {
		t.Fatalf("expected an addition line per hostname, got %q", lines)
	}
}

func TestEveryNodeDegradedLeavesDNSAlone(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress})
	h.round()
	h.cf.resetLogs()

	// The proxy provider is unreachable from everywhere: pulling nodes
	// would not help anyone.
	h.nodeA.setEgressStatus(http.StatusServiceUnavailable)
	h.nodeB.setEgressStatus(http.StatusServiceUnavailable)
	h.rounds(6)
	h.expectBothRecords(testIPA, testIPB)
	if writes := h.cf.writeLog(); len(writes) != 0 {
		t.Fatalf("no DNS write may happen while every node is degraded, got %v", writes)
	}
	status := h.w.Status()
	if status.HealthyNodes != 0 || status.DegradedNodes != 2 || !status.LastReconcile.OK {
		t.Fatalf("status = healthy %d, degraded %d, last %+v; want 0/2 and reconciled", status.HealthyNodes, status.DegradedNodes, status.LastReconcile)
	}
	if lines := h.logs.lines("no node passes its egress check; DNS keeps every ready node", "degraded=node-a,node-b"); len(lines) != 1 {
		t.Fatalf("expected a single all-degraded line, got %q", lines)
	}
	if strings.Contains(strings.Join(h.alerts.events(), " "), "no_healthy_nodes") {
		t.Fatalf("degraded nodes are ready; no_healthy_nodes must not fire, got %v", h.alerts.events())
	}

	// Once node-a's egress works again, node-b, still degraded, is pulled.
	h.nodeA.setEgressStatus(http.StatusNoContent)
	h.rounds(3)
	h.expectBothRecords(testIPA)
	if lines := h.logs.lines("a node passes its egress check again", "healthy=node-a"); len(lines) != 1 {
		t.Fatalf("expected the all-degraded state to end once, got %q", lines)
	}
}

func TestLegacyNodeWithoutEgressEndpointCountsAsDegraded(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress})
	h.nodeB.legacy.Store(true)
	h.rounds(2)
	h.expectBothRecords(testIPA, testIPB)
	h.round()
	h.expectBothRecords(testIPA)
	if got := h.nodeStatus("node-b"); !got.Healthy || got.State != tierDegraded || got.Egress.LastStatus != http.StatusNotFound {
		t.Fatalf("a ready node without the endpoint should be degraded, got %+v / %+v", got, got.Egress)
	}
	for _, name := range []string{testRelay, testCode} {
		lines := h.logs.lines(`msg="dns record removed"`, "domain="+name, "node=node-b", `reason="node degraded: egress check failed: unexpected status 404"`)
		if len(lines) != 1 {
			t.Fatalf("expected one removal line for %s, got %q", name, lines)
		}
	}

	// Every node on an old release: they are all degraded, so DNS stays.
	old := newHarness(t, harnessOptions{config: withEgress})
	old.nodeA.legacy.Store(true)
	old.nodeB.legacy.Store(true)
	old.rounds(6)
	old.expectBothRecords(testIPA, testIPB)
	if writes := old.cf.writeLog(); len(writes) != 0 {
		t.Fatalf("enabling egress_path before any node serves it must not change DNS, got %v", writes)
	}
}

func TestReadinessFailureIsHandledAsBeforeWithEgressOn(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress})
	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(2)
	h.expectBothRecords(testIPA, testIPB)
	h.round()
	h.expectBothRecords(testIPA)
	for _, name := range []string{testRelay, testCode} {
		lines := h.logs.lines(`msg="dns record removed"`, "domain="+name, "node=node-b", "consecutive_failures=3", `reason="node unhealthy: unexpected status 503"`)
		if len(lines) != 1 {
			t.Fatalf("expected one removal line for %s, got %q", name, lines)
		}
	}
	if got := h.nodeStatus("node-b"); got.State != tierUnhealthy || !got.Egress.OK {
		t.Fatalf("node-b should be unhealthy with working egress, got %+v", got)
	}
	h.alerts.waitFor(t, "node_unhealthy")

	h.nodeB.setStatus(http.StatusNoContent)
	h.rounds(3)
	h.expectBothRecords(testIPA, testIPB)
	if lines := h.logs.lines(`msg="dns record added"`, "node=node-b", "node recovered after 3 consecutive successful probes"); len(lines) != 2 {
		t.Fatalf("expected an addition line per hostname, got %q", lines)
	}
	if strings.Contains(strings.Join(h.alerts.events(), " "), "node_egress") {
		t.Fatalf("egress never failed, got %v", h.alerts.events())
	}
}

func TestDegradedNodeServesWhileItsPeerIsDown(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress})
	h.round()
	h.nodeA.setEgressStatus(http.StatusServiceUnavailable)
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	// Degraded beats down: node-a is the only ready node left.
	h.expectBothRecords(testIPA)
	if status := h.w.Status(); status.HealthyNodes != 0 || status.DegradedNodes != 1 {
		t.Fatalf("status counts healthy=%d degraded=%d, want 0/1", status.HealthyNodes, status.DegradedNodes)
	}
	h.cf.resetLogs()

	// node-b recovers with working egress: it is added before node-a goes.
	h.nodeB.setStatus(http.StatusNoContent)
	h.rounds(3)
	h.expectBothRecords(testIPB)
	want := []string{
		"POST " + testRelay + " " + testIPB,
		"DELETE " + testRelay + " " + testIPA,
		"POST " + testCode + " " + testIPB,
		"DELETE " + testCode + " " + testIPA,
	}
	if got := h.cf.writeLog(); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
}

func TestDegradedNodeIsAddedWhenNoNodeIsHealthy(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: publishOnlyA, config: withEgress})
	// node-b, missing from DNS, becomes ready but never passes its egress
	// check, while node-a goes down.
	h.nodeB.setEgressStatus(http.StatusServiceUnavailable)
	h.nodeA.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	h.expectBothRecords(testIPB)
	if got := h.nodeStatus("node-b"); got.State != tierDegraded || got.Egress.Confirmed {
		t.Fatalf("node-b should be degraded and never confirmed, got %+v / %+v", got, got.Egress)
	}
}

func TestUnconfirmedEgressIsNotSpreadToOtherRecords(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress, seed: func(f *fakeCloudflare) {
		f.seed(testRelay, "A", testIPA)
		f.seed(testRelay, "A", testIPB)
		f.seed(testCode, "A", testIPA)
	}})
	// node-b passes readiness, but its egress is healthy only because relay
	// listed it at startup; code must wait for an egress probe to agree.
	h.nodeB.setEgressStatus(http.StatusServiceUnavailable)
	h.round()
	h.expectIPs(testCode, testIPA)
	h.expectIPs(testRelay, testIPA, testIPB)

	h.nodeB.setEgressStatus(http.StatusNoContent)
	h.round()
	h.expectIPs(testCode, testIPA, testIPB)
	if lines := h.logs.lines(`msg="dns record added"`, "domain="+testCode, "node is healthy but has no record here"); len(lines) != 1 {
		t.Fatalf("expected the missing record to be added once, got %q", lines)
	}
}

func TestEgressProbeOffKeepsReadinessOnlyBehaviour(t *testing.T) {
	h := newHarness(t, harnessOptions{config: func(c *Config) { c.SummaryInterval = 30 * time.Second }})
	// Would fail every egress probe, if there were any.
	h.nodeA.setEgressStatus(http.StatusServiceUnavailable)
	h.nodeB.setEgressStatus(http.StatusServiceUnavailable)
	h.rounds(6)

	h.expectBothRecords(testIPA, testIPB)
	if hits := h.nodeA.egressHits.Load() + h.nodeB.egressHits.Load(); hits != 0 {
		t.Fatalf("without egress_path no egress request may be sent, got %d", hits)
	}
	status := h.w.Status()
	if status.HealthyNodes != 2 || status.DegradedNodes != 0 {
		t.Fatalf("status counts healthy=%d degraded=%d, want 2/0", status.HealthyNodes, status.DegradedNodes)
	}
	for _, node := range status.Nodes {
		if node.State != tierHealthy || node.Egress != nil {
			t.Fatalf("node status %+v should carry no egress state", node)
		}
	}
	for _, line := range h.logs.lines("dnswatch summary") {
		if strings.Contains(line, "degraded=") || strings.Contains(line, "egress_failures") {
			t.Fatalf("the summary must not mention egress while it is off: %q", line)
		}
	}
}

func TestStatusSnapshotsCopyEgressState(t *testing.T) {
	h := newHarness(t, harnessOptions{config: withEgress})
	h.round()
	first := h.w.Status()
	if first.Nodes[0].Egress == nil || first.Nodes[0].Egress.LastProbe == nil {
		t.Fatalf("egress state missing from status: %+v", first.Nodes[0])
	}
	*first.Nodes[0].Egress.LastProbe = time.Time{}
	first.Nodes[0].Egress.ConsecutiveSuccesses = 99
	second := h.w.Status()
	if second.Nodes[0].Egress.LastProbe.IsZero() || second.Nodes[0].Egress.ConsecutiveSuccesses == 99 {
		t.Fatal("Status must return egress state the caller can modify")
	}
}
