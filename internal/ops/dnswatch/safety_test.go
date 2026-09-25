package dnswatch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAllNodesDownLeavesDNSUntouched(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.round()
	h.cf.resetLogs()

	h.nodeA.setStatus(http.StatusServiceUnavailable)
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(6)

	h.expectBothRecords(testIPA, testIPB)
	for _, request := range h.cf.requestLog() {
		if !strings.HasPrefix(request, http.MethodGet) {
			t.Fatalf("no write may happen while every node is down, got %s", request)
		}
	}
	status := h.w.Status()
	if status.HealthyNodes != 0 || status.LastReconcile.Skipped != "no_healthy_nodes" {
		t.Fatalf("status should show the guard, got healthy=%d last=%+v", status.HealthyNodes, status.LastReconcile)
	}
	h.alerts.waitFor(t, "no_healthy_nodes")
	if lines := h.logs.lines("no healthy nodes; keeping every DNS record as it is"); len(lines) != 1 {
		t.Fatalf("expected a single guard line, got %q", lines)
	}

	// Once node-a is back, node-b is still down and can be pulled safely.
	h.nodeA.setStatus(http.StatusNoContent)
	h.rounds(3)
	h.expectBothRecords(testIPA)
	h.alerts.waitFor(t, "no_healthy_nodes_resolved")
}

func TestLastHealthyNodeFailingKeepsItsRecord(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	h.expectBothRecords(testIPA)

	h.nodeA.setStatus(http.StatusServiceUnavailable)
	h.rounds(5)
	h.expectBothRecords(testIPA)
	if deletes := h.writesOf("DELETE"); len(deletes) != 2 {
		t.Fatalf("only node-b's two records may have been removed, got %v", deletes)
	}
}

func TestRecordsOutsideTheNodeListAreNeverTouched(t *testing.T) {
	var untouchable []string
	h := newHarness(t, harnessOptions{seed: func(f *fakeCloudflare) {
		publishBoth(f)
		for _, name := range []string{testRelay, testCode} {
			untouchable = append(untouchable,
				f.seed(name, "A", "198.51.100.7"),
				f.seed(name, "AAAA", "2001:db8::12"),
				// Same content as node-b's A record, but not an A record.
				f.seed(name, "TXT", testIPB),
			)
		}
		// node-b's address under a hostname dnswatch does not manage.
		untouchable = append(untouchable, f.seed("other.dnswatch.test", "A", testIPB))
	}})
	// Answer every list with the whole zone, so dnswatch's own name and type
	// filtering is what protects these records.
	h.cf.ignoreFilters = true

	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)

	h.expectIPs(testRelay, testIPA, "198.51.100.7")
	h.expectIPs(testCode, testIPA, "198.51.100.7")
	h.expectIPs("other.dnswatch.test", testIPB)
	for _, id := range untouchable {
		if !h.cf.has(id) {
			t.Fatalf("record %s outside the node list was deleted", id)
		}
	}
	want := []string{"DELETE " + testRelay + " " + testIPB, "DELETE " + testCode + " " + testIPB}
	if got := h.cf.writeLog(); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	if other := h.w.Status().Records[testRelay].Other; !slices.Equal(other, []string{"198.51.100.7"}) {
		t.Fatalf("status should list the foreign A record, got %v", other)
	}
}

func TestHoldFileSuspendsDNSChanges(t *testing.T) {
	holdFile := filepath.Join(t.TempDir(), "hold")
	h := newHarness(t, harnessOptions{config: func(c *Config) { c.HoldFile = holdFile }})
	h.round()
	if err := os.WriteFile(holdFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.cf.resetLogs()

	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(4)
	h.expectBothRecords(testIPA, testIPB)
	if requests := h.cf.requestLog(); len(requests) != 0 {
		t.Fatalf("a hold must keep dnswatch away from the API, got %v", requests)
	}
	status := h.w.Status()
	if !status.Hold || status.LastReconcile.Skipped != "hold" {
		t.Fatalf("status should report the hold, got %+v", status)
	}
	if got := h.nodeStatus("node-b"); got.Healthy {
		t.Fatalf("probing continues during a hold, node-b should be unhealthy: %+v", got)
	}
	h.alerts.waitFor(t, "dns_hold_started")

	if err := os.Remove(holdFile); err != nil {
		t.Fatal(err)
	}
	h.round()
	h.expectBothRecords(testIPA)
	h.alerts.waitFor(t, "dns_hold_ended")
	if lines := h.logs.lines("hold file present"); len(lines) != 1 {
		t.Fatalf("the hold should be logged once, got %q", lines)
	}
}

func TestDryRunReportsWithoutWriting(t *testing.T) {
	h := newHarness(t, harnessOptions{config: func(c *Config) { c.DryRun = true }})
	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(6)

	h.expectBothRecords(testIPA, testIPB)
	for _, request := range h.cf.requestLog() {
		if !strings.HasPrefix(request, http.MethodGet) {
			t.Fatalf("dry_run must never write, got %s", request)
		}
	}
	for _, name := range []string{testRelay, testCode} {
		lines := h.logs.lines("dry-run: would remove DNS record", "domain="+name, "node=node-b", "consecutive_failures=3")
		if len(lines) != 1 {
			t.Fatalf("expected the planned removal for %s logged once, got %q", name, lines)
		}
	}
	if lines := h.logs.lines(`msg="dns record removed"`); len(lines) != 0 {
		t.Fatalf("dry_run must not report real changes, got %q", lines)
	}
}

func TestDryRunReportsPlannedAdditions(t *testing.T) {
	h := newHarness(t, harnessOptions{seed: publishOnlyA, config: func(c *Config) { c.DryRun = true }})
	h.rounds(4)
	h.expectBothRecords(testIPA)
	if lines := h.logs.lines("dry-run: would add DNS record", "node=node-b"); len(lines) != 2 {
		t.Fatalf("expected one planned addition per hostname, got %q", lines)
	}
	if writes := h.cf.writeLog(); len(writes) != 0 {
		t.Fatalf("dry_run must never write, got %v", writes)
	}
}

func TestCloudflareFailureIsRetriedNextRound(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(2)

	h.cf.fail("", -1, http.StatusInternalServerError)
	h.round()
	h.expectBothRecords(testIPA, testIPB)
	status := h.w.Status()
	if status.LastReconcile.OK || !strings.Contains(status.LastReconcile.Error, "HTTP 500") {
		t.Fatalf("the failed round should be reported, got %+v", status.LastReconcile)
	}
	failed := h.alerts.waitFor(t, "dns_reconcile_failed")
	if !strings.Contains(failed.Error, "HTTP 500") {
		t.Fatalf("the alert should carry the error, got %+v", failed)
	}

	// Still failing: no second alert for the same outage.
	h.round()
	h.cf.clearFaults()
	h.round()
	h.expectBothRecords(testIPA)
	if status := h.w.Status(); !status.LastReconcile.OK {
		t.Fatalf("the next round should reconcile, got %+v", status.LastReconcile)
	}
	h.alerts.waitFor(t, "dns_reconcile_recovered")
	if n := strings.Count(strings.Join(h.alerts.events(), " "), "dns_reconcile_failed"); n != 1 {
		t.Fatalf("expected one failure alert per outage, got %d", n)
	}
}

func TestStartupWaitsForCloudflare(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.cf.fail(http.MethodGet, -1, http.StatusBadGateway)
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(4)

	if status := h.w.Status(); status.Initialized || status.LastReconcile.Skipped != "initializing" {
		t.Fatalf("dnswatch must not act before reading DNS, got %+v", status)
	}
	if hits := h.nodeB.hits.Load(); hits != 0 {
		t.Fatalf("probing before the initial state is known would count against it, got %d probes", hits)
	}

	h.cf.clearFaults()
	h.round()
	h.expectBothRecords(testIPA, testIPB)
	h.rounds(2)
	h.expectBothRecords(testIPA)
}

func TestTokenNeverReachesLogsStatusOrAlerts(t *testing.T) {
	h := newHarness(t, harnessOptions{config: func(c *Config) { c.SummaryInterval = 30 * time.Second }})
	h.cf.echoAuth = true

	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	// A failed list and a failed delete, both echoing the Authorization
	// header back, while node-b goes down and is pulled.
	h.cf.fail(http.MethodGet, 1, http.StatusBadRequest)
	h.rounds(2)
	h.cf.fail(http.MethodDelete, 1, http.StatusForbidden)
	h.rounds(2)
	h.expectBothRecords(testIPA)
	h.nodeB.setStatus(http.StatusNoContent)
	h.rounds(4)
	h.expectBothRecords(testIPA, testIPB)
	h.alerts.waitFor(t, "node_healthy")
	h.w.Close()

	statusJSON, err := json.Marshal(h.w.Status())
	if err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	for label, text := range map[string]string{"logs": logs, "status": string(statusJSON), "alerts": h.alerts.rawBodies()} {
		if strings.Contains(text, testToken) {
			t.Fatalf("the API token leaked into %s:\n%s", label, text)
		}
	}
	// Make sure the scenario exercised the paths that could leak it: the
	// fake echoed the Authorization header into errors that were logged.
	if !strings.Contains(logs, "Bearer "+redactedToken) {
		t.Fatalf("expected a logged Cloudflare error with the echoed header redacted:\n%s", logs)
	}
	for _, fragment := range []string{"node marked unhealthy", "dns record removed", "dns record added", "dnswatch summary"} {
		if !strings.Contains(logs, fragment) {
			t.Fatalf("scenario did not log %q", fragment)
		}
	}
}

func TestAlertWebhookFailuresStayOutOfTheWay(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hook.Close)
	secretPath := "/services/T000/B000/secretXXXXXXXXXXXX"
	h := newHarness(t, harnessOptions{config: func(c *Config) { c.AlertWebhook = hook.URL + secretPath }})

	h.round()
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(3)
	h.expectBothRecords(testIPA)
	h.w.Close()

	if lines := h.logs.lines("alert webhook rejected the alert", "status=500"); len(lines) == 0 {
		t.Fatal("a rejected alert should be logged")
	}
	if strings.Contains(h.logs.String(), "secretXXXX") {
		t.Fatal("the webhook path must not be logged")
	}

	// An unreachable webhook is reported without its URL either.
	unreachable := newHarness(t, harnessOptions{config: func(c *Config) { c.AlertWebhook = "http://127.0.0.1:1" + secretPath }})
	unreachable.round()
	unreachable.nodeB.setStatus(http.StatusServiceUnavailable)
	unreachable.rounds(3)
	unreachable.w.Close()
	unreachable.expectBothRecords(testIPA)
	if lines := unreachable.logs.lines("alert webhook unreachable"); len(lines) == 0 {
		t.Fatal("an unreachable webhook should be logged")
	}
	if strings.Contains(unreachable.logs.String(), "secretXXXX") {
		t.Fatal("the webhook path must not be logged")
	}
}
