package dnswatch

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testRelay = "relay.dnswatch.test"
	testCode  = "code.dnswatch.test"
	testIPA   = "203.0.113.11"
	testIPB   = "203.0.113.12"
)

func testProbeConfig(host string, timeout time.Duration) ProbeConfig {
	return ProbeConfig{Path: DefaultProbePath, Host: host, Timeout: timeout, ExpectStatus: []int{200, 204}}
}

func TestProbeVerifiesCertificateForTheHostname(t *testing.T) {
	pki := newTestPKI(t, testRelay, testCode)
	node := newFakeNode(t, pki)
	router := newDialRouter()
	router.route(testIPA, node)

	p := newProber(testProbeConfig(testRelay, 2*time.Second), pki.roots, router.dial)
	result := p.probe(context.Background(), testIPA)
	if !result.ok || result.status != http.StatusNoContent {
		t.Fatalf("expected a healthy probe, got %+v", result)
	}
	seen := node.lastRequest()
	if seen.host != testRelay || seen.sni != testRelay || seen.path != "/readyz" {
		t.Fatalf("probe must send Host and SNI %q to /readyz, node saw %+v", testRelay, seen)
	}
}

func TestProbeRejectsCertificatesItCannotVerify(t *testing.T) {
	pki := newTestPKI(t, testRelay)
	node := newFakeNode(t, pki)
	router := newDialRouter()
	router.route(testIPA, node)

	untrusted := newTestPKI(t, testRelay)
	result := newProber(testProbeConfig(testRelay, 2*time.Second), untrusted.roots, router.dial).probe(context.Background(), testIPA)
	if result.ok || !strings.Contains(result.reason, "certificate") {
		t.Fatalf("a certificate from an unknown CA must fail the probe, got %+v", result)
	}

	wrongHost := newProber(testProbeConfig("other.dnswatch.test", 2*time.Second), pki.roots, router.dial).probe(context.Background(), testIPA)
	if wrongHost.ok || !strings.Contains(wrongHost.reason, "certificate") {
		t.Fatalf("a certificate for another hostname must fail the probe, got %+v", wrongHost)
	}
}

func TestProbeJudgesStatusCodes(t *testing.T) {
	pki := newTestPKI(t, testRelay)
	node := newFakeNode(t, pki)
	router := newDialRouter()
	router.route(testIPA, node)
	p := newProber(testProbeConfig(testRelay, 2*time.Second), pki.roots, router.dial)

	cases := []struct {
		status int
		ok     bool
	}{
		{http.StatusOK, true},
		{http.StatusNoContent, true},
		{http.StatusServiceUnavailable, false},
		// Redirects are not followed: the node must answer readiness itself.
		{http.StatusMovedPermanently, false},
	}
	for _, tc := range cases {
		node.setStatus(tc.status)
		result := p.probe(context.Background(), testIPA)
		if result.ok != tc.ok || result.status != tc.status {
			t.Fatalf("status %d: got %+v, want ok=%v", tc.status, result, tc.ok)
		}
		if !tc.ok && result.reason == "" {
			t.Fatalf("status %d: a failed probe needs a reason", tc.status)
		}
	}
}

func TestProbeAllRunsNodesConcurrentlyWithIndependentTimeouts(t *testing.T) {
	pki := newTestPKI(t, testRelay)
	slow, fast := newFakeNode(t, pki), newFakeNode(t, pki)
	slow.delay.Store(int64(5 * time.Second))
	router := newDialRouter()
	router.route(testIPA, slow)
	router.route(testIPB, fast)

	timeout := 400 * time.Millisecond
	p := newProber(testProbeConfig(testRelay, timeout), pki.roots, router.dial)
	start := time.Now()
	results := p.probeAll(context.Background(), []Node{{Name: "slow", IP: testIPA}, {Name: "fast", IP: testIPB}})
	elapsed := time.Since(start)

	if results[0].ok || results[0].reason != "timeout" {
		t.Fatalf("the hung node should time out, got %+v", results[0])
	}
	if !results[1].ok {
		t.Fatalf("the healthy node must not be dragged down by the hung one, got %+v", results[1])
	}
	if elapsed > 3*timeout {
		t.Fatalf("probes ran for %s; expected them to overlap within one timeout", elapsed)
	}
}

func TestProbeReportsUnreachableNode(t *testing.T) {
	pki := newTestPKI(t, testRelay)
	node := newFakeNode(t, pki)
	router := newDialRouter()
	router.route(testIPA, node)
	node.server.Close()

	result := newProber(testProbeConfig(testRelay, time.Second), pki.roots, router.dial).probe(context.Background(), testIPA)
	if result.ok || !strings.Contains(result.reason, "connection refused") {
		t.Fatalf("expected a connection failure, got %+v", result)
	}
	if strings.Contains(result.reason, "https://") {
		t.Fatalf("the reason should not repeat the URL: %q", result.reason)
	}
}
