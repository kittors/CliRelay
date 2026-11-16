package api

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/nodemetrics"
)

func TestServerFeedsTheNodeSnapshot(t *testing.T) {
	server := newTestServer(t)
	server.server.Addr = "127.0.0.1:0"
	ln, err := server.listenCounted(":http")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.server.Serve(ln) }()
	t.Cleanup(func() { _ = server.server.Close() })

	collector := nodemetrics.Default()
	requests, bytesIn, bytesOut := collector.Totals()
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d through the counting listener", resp.StatusCode)
	}
	gotRequests, gotIn, gotOut := collector.Totals()
	if gotRequests < requests+1 || gotIn <= bytesIn || gotOut <= bytesOut {
		t.Fatalf("request and bytes were not counted: requests %d->%d, in %d->%d, out %d->%d",
			requests, gotRequests, bytesIn, gotIn, bytesOut, gotOut)
	}

	started := time.Now()
	server.startNodeMetrics()
	t.Cleanup(server.stopNodeMetrics)
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, ok := collector.Latest()
		if ok && !snap.CollectedAt.Before(started.Add(-time.Second)) {
			if snap.Version != buildinfo.Version || snap.Egress == nil || snap.Schema != nodemetrics.SchemaVersion {
				t.Fatalf("snapshot = %+v", snap)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("starting the server's node metrics should take a snapshot at once")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
