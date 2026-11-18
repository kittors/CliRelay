package api

import (
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/nodemetrics"
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
			// A single node has no cluster Redis to report.
			if snap.SharedRedis != nil {
				t.Fatalf("shared redis = %+v, want it left out on a single node", snap.SharedRedis)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("starting the server's node metrics should take a snapshot at once")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The snapshot reports the filesystems of what the process writes, and of
// any extra directory an operator lists.
func TestNodeDiskPathsListWhatTheProcessWrites(t *testing.T) {
	t.Setenv(nodeDiskPathsEnv, " /srv/backups , ,/mnt/extra")
	server := newTestServer(t)
	uses := map[string][]string{}
	for _, p := range server.nodeDiskPaths() {
		uses[p.Use] = append(uses[p.Use], p.Path)
	}
	for _, use := range []string{nodemetrics.DiskUseWorkdir, nodemetrics.DiskUseAuth, nodemetrics.DiskUseLogs, nodemetrics.DiskUseBodyCache} {
		if len(uses[use]) != 1 || uses[use][0] == "" {
			t.Fatalf("paths for %s = %v (all: %v)", use, uses[use], uses)
		}
	}
	if got := uses[nodemetrics.DiskUseExtra]; !reflect.DeepEqual(got, []string{"/srv/backups", "/mnt/extra"}) {
		t.Fatalf("extra paths = %v", got)
	}

	// Resolved at most once a minute: resolving the log directory writes a
	// probe file.
	source := server.cachedDiskPaths()
	first := source()
	t.Setenv(nodeDiskPathsEnv, "/elsewhere")
	if second := source(); !reflect.DeepEqual(first, second) {
		t.Fatalf("paths changed within a minute: %v then %v", first, second)
	}
}
