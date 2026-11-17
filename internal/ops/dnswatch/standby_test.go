package dnswatch

import (
	"net/http"
	"strings"
	"testing"
)

// In these tests node-a is the primary and node-b the standby.
func standbyNodeB(c *Config) { c.Nodes[1].Standby = true }

func TestParseConfigReadsStandby(t *testing.T) {
	cfg, err := ParseConfig([]byte(strings.Replace(minimalConfigYAML, "ip: 203.0.113.12", "ip: 203.0.113.12\n    standby: true", 1)))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Nodes[0].Standby || !cfg.Nodes[1].Standby {
		t.Fatalf("standby flags = %v, %v; want only the second node standby", cfg.Nodes[0].Standby, cfg.Nodes[1].Standby)
	}
}

// A standby that DNS still lists next to a serving primary is pulled, with a
// reason that says why, and the status shows it as a healthy standby.
func TestStandbyLeavesDNSWhilePrimaryServes(t *testing.T) {
	h := newHarness(t, harnessOptions{config: standbyNodeB})
	h.round()
	h.expectBothRecords(testIPA)
	for _, name := range []string{testRelay, testCode} {
		lines := h.logs.lines(`msg="dns record removed"`, "domain="+name, "node=node-b", `reason="standby node: a primary node is serving"`)
		if len(lines) != 1 {
			t.Fatalf("expected one standby removal line for %s, got %q", name, lines)
		}
	}
	if got := h.nodeStatus("node-b"); !got.Standby || !got.Healthy {
		t.Fatalf("node-b status = %+v, want a healthy standby", got)
	}
	if got := h.nodeStatus("node-a"); got.Standby {
		t.Fatalf("node-a status = %+v, want a primary", got)
	}
}

// The standby takes over once the primary fails its threshold and hands back
// once the primary recovers; each hand-over adds before it removes, so the
// hostname is never left without a serving record.
func TestStandbyTakesOverAndHandsBack(t *testing.T) {
	h := newHarness(t, harnessOptions{config: standbyNodeB, seed: func(f *fakeCloudflare) {
		for _, name := range []string{testRelay, testCode} {
			f.seed(name, "A", testIPA)
		}
	}})
	h.round()
	h.expectBothRecords(testIPA)

	h.nodeA.setStatus(http.StatusServiceUnavailable)
	h.rounds(2)
	h.expectBothRecords(testIPA)
	before := len(h.cf.writeLog())
	h.round()
	h.expectBothRecords(testIPB)
	for _, name := range []string{testRelay, testCode} {
		if lines := h.logs.lines(`msg="dns record added"`, "domain="+name, "node=node-b", `reason="standby node: no primary node is serving"`); len(lines) != 1 {
			t.Fatalf("expected one standby addition line for %s, got %q", name, lines)
		}
	}
	expectAddBeforeRemove(t, h.cf.writeLog()[before:], testIPB)

	h.nodeA.setStatus(http.StatusNoContent)
	h.rounds(2)
	h.expectBothRecords(testIPB)
	before = len(h.cf.writeLog())
	h.round()
	h.expectBothRecords(testIPA)
	for _, name := range []string{testRelay, testCode} {
		if lines := h.logs.lines(`msg="dns record removed"`, "domain="+name, "node=node-b", `reason="standby node: a primary node is serving"`); len(lines) != 1 {
			t.Fatalf("expected one standby removal line for %s, got %q", name, lines)
		}
	}
	expectAddBeforeRemove(t, h.cf.writeLog()[before:], testIPA)
}

// A standby is only a spare: with the primary down and the standby down too,
// nothing is pulled, as with any set of nodes that all fail.
func TestStandbyAndPrimaryBothDownChangesNothing(t *testing.T) {
	h := newHarness(t, harnessOptions{config: standbyNodeB, seed: func(f *fakeCloudflare) {
		for _, name := range []string{testRelay, testCode} {
			f.seed(name, "A", testIPA)
		}
	}})
	h.round()
	h.nodeA.setStatus(http.StatusServiceUnavailable)
	h.nodeB.setStatus(http.StatusServiceUnavailable)
	h.rounds(4)
	h.expectBothRecords(testIPA)
}

// expectAddBeforeRemove checks that a hand-over round published ip under each
// hostname before it deleted the outgoing record there.
func expectAddBeforeRemove(t *testing.T, writes []string, ip string) {
	t.Helper()
	for _, name := range []string{testRelay, testCode} {
		post, del := -1, -1
		for i, write := range writes {
			switch {
			case write == "POST "+name+" "+ip && post == -1:
				post = i
			case strings.HasPrefix(write, "DELETE "+name+" ") && del == -1:
				del = i
			}
		}
		if post == -1 || del == -1 || del < post {
			t.Fatalf("%s: want %s created before the outgoing record is removed; writes: %q", name, ip, writes)
		}
	}
}
