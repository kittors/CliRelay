package dnswatch

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestCloudflareClient(f *fakeCloudflare) *cloudflareClient {
	return newCloudflareClient(CloudflareConfig{ZoneID: testZoneID, APIBaseURL: f.server.URL}, testToken, nil, time.Millisecond)
}

func TestCloudflareClientListCreateDelete(t *testing.T) {
	f := newFakeCloudflare(t)
	existing := f.seed("relay.dnswatch.test", "A", "203.0.113.11")
	f.seed("relay.dnswatch.test", "AAAA", "2001:db8::11")
	f.seed("other.dnswatch.test", "A", "203.0.113.99")
	client := newTestCloudflareClient(f)
	ctx := context.Background()

	records, err := client.listARecords(ctx, "relay.dnswatch.test")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 || records[0].ID != existing || records[0].Content != "203.0.113.11" {
		t.Fatalf("list returned %+v, want only the A record %s", records, existing)
	}

	created, err := client.createARecord(ctx, "relay.dnswatch.test", "203.0.113.12", 60)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored := f.record(created.ID)
	if stored.Content != "203.0.113.12" || stored.TTL != 60 || stored.Proxied || stored.Type != "A" {
		t.Fatalf("created record = %+v", stored)
	}

	if err := client.deleteRecord(ctx, existing); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := f.aIPs("relay.dnswatch.test"); !slices.Equal(got, []string{"203.0.113.12"}) {
		t.Fatalf("A records after delete = %v", got)
	}
	// Deleting a record that is already gone is the desired end state.
	if err := client.deleteRecord(ctx, existing); err != nil {
		t.Fatalf("deleting a missing record should succeed, got %v", err)
	}
}

func TestCloudflareClientFollowsPages(t *testing.T) {
	f := newFakeCloudflare(t)
	f.pageSize = 1
	for _, ip := range []string{"203.0.113.11", "203.0.113.12", "203.0.113.13"} {
		f.seed("relay.dnswatch.test", "A", ip)
	}
	records, err := newTestCloudflareClient(f).listARecords(context.Background(), "relay.dnswatch.test")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected all three pages to be read, got %d records", len(records))
	}
}

func TestCloudflareClientRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		f := newFakeCloudflare(t)
		f.fail(http.MethodGet, 2, status)
		if _, err := newTestCloudflareClient(f).listARecords(context.Background(), "relay.dnswatch.test"); err != nil {
			t.Fatalf("status %d: expected the third attempt to succeed, got %v", status, err)
		}
		if got := len(f.requestLog()); got != 3 {
			t.Fatalf("status %d: expected 3 attempts, got %d", status, got)
		}
	}
}

func TestCloudflareClientGivesUpAfterThreeAttempts(t *testing.T) {
	f := newFakeCloudflare(t)
	f.fail("", -1, http.StatusInternalServerError)
	_, err := newTestCloudflareClient(f).createARecord(context.Background(), "relay.dnswatch.test", "203.0.113.12", 60)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected a 500 error, got %v", err)
	}
	if got := len(f.requestLog()); got != cfAttempts {
		t.Fatalf("expected %d attempts, got %d", cfAttempts, got)
	}
}

func TestCloudflareClientDoesNotRetryClientErrors(t *testing.T) {
	f := newFakeCloudflare(t)
	f.fail(http.MethodPost, -1, http.StatusBadRequest)
	_, err := newTestCloudflareClient(f).createARecord(context.Background(), "relay.dnswatch.test", "203.0.113.12", 60)
	if err == nil || !strings.Contains(err.Error(), "[10000] injected failure") {
		t.Fatalf("expected the Cloudflare error message to surface, got %v", err)
	}
	if got := len(f.requestLog()); got != 1 {
		t.Fatalf("a 400 must not be retried, got %d attempts", got)
	}
}

func TestCloudflareClientRejectsWrongToken(t *testing.T) {
	f := newFakeCloudflare(t)
	client := newCloudflareClient(CloudflareConfig{ZoneID: testZoneID, APIBaseURL: f.server.URL}, "wrong-token-XXXX", nil, time.Millisecond)
	_, err := client.listARecords(context.Background(), "relay.dnswatch.test")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected a 403, got %v", err)
	}
	if got := len(f.requestLog()); got != 1 {
		t.Fatalf("an auth failure must not be retried, got %d attempts", got)
	}
}

func TestCloudflareClientErrorsNeverCarryToken(t *testing.T) {
	f := newFakeCloudflare(t)
	f.echoAuth = true
	f.fail("", -1, http.StatusBadRequest)
	_, err := newTestCloudflareClient(f).listARecords(context.Background(), "relay.dnswatch.test")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), redactedToken) {
		t.Fatalf("expected the echoed token to be redacted, got %v", err)
	}
}

func TestCloudflareClientStopsRetryingWhenCancelled(t *testing.T) {
	f := newFakeCloudflare(t)
	f.fail("", -1, http.StatusServiceUnavailable)
	client := newCloudflareClient(CloudflareConfig{ZoneID: testZoneID, APIBaseURL: f.server.URL}, testToken, nil, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := client.listARecords(ctx, "relay.dnswatch.test"); err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("retry backoff ignored cancellation (%s)", elapsed)
	}
}
