package dnswatch

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNodePulledAfterThreeFailuresAndRestoredAfterThreeSuccesses(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.round()
	h.expectBothRecords(testIPA, testIPB)

	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(2)
	h.expectBothRecords(testIPA, testIPB)
	if got := h.nodeStatus("node-b"); !got.Healthy || got.ConsecutiveFailures != 2 {
		t.Fatalf("node-b after two failures = %+v, want healthy with 2 failures", got)
	}

	h.round()
	h.expectBothRecords(testIPA)
	down := h.alerts.waitFor(t, "node_unhealthy")
	if down.Node != "node-b" || down.IP != testIPB || down.ConsecutiveFailures != 3 || !strings.Contains(down.Reason, "503") {
		t.Fatalf("unexpected node_unhealthy alert: %+v", down)
	}
	updated := h.alerts.waitFor(t, "dns_updated")
	if len(updated.Changes) != 2 || updated.Changes[0].Action != "remove" || updated.Changes[0].IP != testIPB {
		t.Fatalf("unexpected dns_updated alert: %+v", updated)
	}
	if lines := h.logs.lines(`msg="node marked unhealthy"`, "node=node-b", "ip="+testIPB, "consecutive_failures=3", "503"); len(lines) != 1 {
		t.Fatalf("expected one state change line, got %q", lines)
	}
	for _, name := range []string{testRelay, testCode} {
		lines := h.logs.lines(`msg="dns record removed"`, "domain="+name, "node=node-b", "ip="+testIPB, "consecutive_failures=3", `reason="node unhealthy: unexpected status 503"`)
		if len(lines) != 1 {
			t.Fatalf("expected one removal line for %s, got %q", name, lines)
		}
	}

	h.nodeB.setStatus(http.StatusNoContent)
	h.rounds(2)
	h.expectBothRecords(testIPA)
	h.round()
	h.expectBothRecords(testIPA, testIPB)
	up := h.alerts.waitFor(t, "node_healthy")
	if up.Node != "node-b" || up.ConsecutiveSuccesses != 3 {
		t.Fatalf("unexpected node_healthy alert: %+v", up)
	}
	if lines := h.logs.lines(`msg="dns record added"`, "node=node-b", "ttl=60", "node recovered after 3 consecutive successful probes"); len(lines) != 2 {
		t.Fatalf("expected an addition line per hostname, got %q", lines)
	}
	// Re-added records use the configured TTL and are unproxied.
	for _, write := range h.writesOf("POST") {
		if !strings.HasSuffix(write, testIPB) {
			t.Fatalf("unexpected create %q", write)
		}
	}
	status := h.w.Status()
	if !slices.Equal(status.Records[testRelay].Nodes, []string{testIPA, testIPB}) || !status.LastReconcile.OK {
		t.Fatalf("status does not reflect the restored record: %+v", status)
	}
}

func TestStartupTrustsPublishedNodesUntilProbesDisagree(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.nodeB.setStatus(http.StatusServiceUnavailable)

	h.round()
	h.expectBothRecords(testIPA, testIPB)
	if writes := h.cf.writeLog(); len(writes) != 0 {
		t.Fatalf("the first round after startup must not touch DNS, got %v", writes)
	}
	if got := h.nodeStatus("node-b"); !got.Healthy || got.Confirmed || got.ConsecutiveFailures != 1 {
		t.Fatalf("node-b should start healthy but unconfirmed, got %+v", got)
	}

	h.rounds(2)
	h.expectBothRecords(testIPA)
}

func TestStartupNodeMissingFromDNSMustPassRecoverThreshold(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: publishOnlyA})
	h.rounds(2)
	h.expectBothRecords(testIPA)
	if got := h.nodeStatus("node-b"); got.Healthy {
		t.Fatalf("a node missing from DNS must start unhealthy, got %+v", got)
	}
	h.round()
	h.expectBothRecords(testIPA, testIPB)
}

func TestUnconfirmedNodeIsNotPublishedUnderOtherRecords(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: func(f *fakeCloudflare) {
		f.seed(testRelay, "A", testIPA)
		f.seed(testRelay, "A", testIPB)
		f.seed(testCode, "A", testIPA)
	}})
	h.nodeB.setStatus(http.StatusServiceUnavailable)

	// node-b starts healthy because relay lists it, but nothing has shown it
	// works yet, so it must not spread to code.
	h.round()
	h.expectIPs(testCode, testIPA)
	h.expectIPs(testRelay, testIPA, testIPB)

	h.nodeB.setStatus(http.StatusNoContent)
	h.round()
	h.expectIPs(testCode, testIPA, testIPB)
	if lines := h.logs.lines(`msg="dns record added"`, "domain="+testCode, "node is healthy but has no record here"); len(lines) != 1 {
		t.Fatalf("expected the missing record to be added once, got %q", lines)
	}
}

func TestAdditionsLandBeforeRemovals(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: publishOnlyA})
	// node-a fails its three probes in the same rounds node-b passes its
	// three, so one reconciliation must both add node-b and remove node-a.
	h.nodeA.setStatus(http.StatusServiceUnavailable)
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

func TestRemovalsWaitWhileAdditionsFail(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: publishOnlyA})
	h.nodeA.setStatus(http.StatusServiceUnavailable)
	h.cf.fail(http.MethodPost, -1, http.StatusInternalServerError)
	h.rounds(3)

	// The replacement could not be published, so the failing node stays.
	h.expectBothRecords(testIPA)
	if deletes := h.writesOf("DELETE"); len(deletes) != 0 {
		t.Fatalf("nothing may be removed while its replacement is missing, got %v", deletes)
	}
	if lines := h.logs.lines("DNS removals postponed until the additions succeed", "pending_removals="+testIPA); len(lines) != 2 {
		t.Fatalf("expected a postponement line per hostname, got %q", lines)
	}
	if status := h.w.Status(); status.LastReconcile.OK || status.LastReconcile.Error == "" {
		t.Fatalf("the failed round must be reported, got %+v", status.LastReconcile)
	}

	h.cf.clearFaults()
	h.round()
	h.expectBothRecords(testIPB)
	writes := h.cf.writeLog()
	if i, j := slices.Index(writes, "POST "+testRelay+" "+testIPB), slices.Index(writes, "DELETE "+testRelay+" "+testIPA); i < 0 || j < i {
		t.Fatalf("expected the addition before the removal, got %v", writes)
	}
}

func TestCreateWithLostReplyCountsAsPublished(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: publishOnlyA})
	h.nodeA.setStatus(http.StatusServiceUnavailable)
	// The first create lands but answers 500; the retry then hits
	// "identical record exists". Re-reading shows the record is there.
	h.cf.createLandsBeforeFault = true
	h.cf.fail(http.MethodPost, 1, http.StatusInternalServerError)
	h.rounds(3)

	h.expectBothRecords(testIPB)
	if lines := h.logs.lines("create reported an error but the record is published", "domain="+testRelay); len(lines) != 1 {
		t.Fatalf("expected the lost reply to be noticed, got %q", lines)
	}
	if status := h.w.Status(); !status.LastReconcile.OK {
		t.Fatalf("the round should count as reconciled, got %+v", status.LastReconcile)
	}
}

func TestMinChangeIntervalDebouncesFlaps(t *testing.T) {
	h := newHarness(t, harnessOptions{config: func(c *Config) { c.MinChangeInterval = time.Minute }})
	h.round()

	// The first flip is not held back: nothing flipped before it.
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	h.expectBothRecords(testIPA)
	downAt := h.clock.Now()

	// Three successes come 30s after the flip: held until the minute is up.
	h.nodeB.setStatus(http.StatusNoContent)
	h.rounds(3)
	h.expectBothRecords(testIPA)
	if got := h.nodeStatus("node-b"); got.Healthy || got.ConsecutiveSuccesses != 3 {
		t.Fatalf("node-b should still be unhealthy with 3 successes, got %+v", got)
	}
	h.rounds(2)
	h.expectBothRecords(testIPA)

	h.round()
	if elapsed := h.clock.Now().Sub(downAt); elapsed != time.Minute {
		t.Fatalf("test arithmetic: flip expected exactly one minute after going down, got %s", elapsed)
	}
	h.expectBothRecords(testIPA, testIPB)

	held := h.logs.lines("held back by min_change_interval", "node=node-b")
	if len(held) != 1 || !strings.Contains(held[0], "until=2026-01-01T00:01:40Z") {
		t.Fatalf("expected one debounce line naming the release time, got %q", held)
	}
}

func TestDebounceSuppressesFlappingNode(t *testing.T) {
	h := newHarness(t, harnessOptions{config: func(c *Config) {
		c.MinChangeInterval = 5 * time.Minute
	}})
	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	h.expectBothRecords(testIPA)
	h.cf.resetLogs()

	// A node bouncing between healthy streaks and failures inside the window
	// must not produce a single DNS write.
	for range 4 {
		h.nodeB.setStatus(http.StatusNoContent)
		h.rounds(3)
		h.nodeB.setStatus(http.StatusServiceUnavailable)
		h.rounds(1)
	}
	if writes := h.cf.writeLog(); len(writes) != 0 {
		t.Fatalf("a flapping node churned DNS: %v", writes)
	}
}
