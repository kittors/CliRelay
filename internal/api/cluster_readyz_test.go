package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// joinTestCluster makes this process a node of an in-memory cluster.
func joinTestCluster(t *testing.T, nodeID string) {
	t.Helper()
	cluster.SetDefault(cluster.NewMemoryHub().Join(nodeID))
	t.Cleanup(func() { cluster.SetDefault(nil) })
}

func useReadyzDB(t *testing.T, db *sql.DB) {
	t.Helper()
	previous := readyzRuntimeDB
	readyzRuntimeDB = func() *sql.DB { return db }
	t.Cleanup(func() { readyzRuntimeDB = previous })
}

func openPingableDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func serveReadyz(server *Server) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rr
}

func TestClusterReadyzPingsRuntimePool(t *testing.T) {
	server := newTestServer(t)
	joinTestCluster(t, "node-a")
	useReadyzDB(t, openPingableDB(t))
	if rr := serveReadyz(server); rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterReadyzFailsWithoutDatabase(t *testing.T) {
	server := newTestServer(t)
	joinTestCluster(t, "node-a")
	closed := openPingableDB(t)
	_ = closed.Close()
	for name, db := range map[string]*sql.DB{"missing": nil, "closed": closed} {
		useReadyzDB(t, db)
		rr := serveReadyz(server)
		if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"reason":"postgres"`) {
			t.Fatalf("%s pool: status = %d, body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestClusterReadyzTreatsRedisOutageAsDegraded(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Redis.Enable = true
	server.cfg.Redis.Addr = "127.0.0.1:1"
	joinTestCluster(t, "node-a")
	useReadyzDB(t, openPingableDB(t))
	rr := serveReadyz(server)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"degraded"`) || !strings.Contains(rr.Body.String(), "redis") {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterReadyzStillFailsWhileDraining(t *testing.T) {
	server := newTestServer(t)
	joinTestCluster(t, "node-a")
	useReadyzDB(t, openPingableDB(t))
	server.draining.Store(true)
	rr := serveReadyz(server)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"reason":"draining"`) {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterNodeHeaderOnlyInClusterMode(t *testing.T) {
	server := newTestServer(t)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rr.Header().Get(clusterNodeHeader); got != "" {
		t.Fatalf("single-node response carries %s=%q", clusterNodeHeader, got)
	}

	joinTestCluster(t, "node-b")
	for _, path := range []string{"/healthz", "/no-such-route"} {
		rr = httptest.NewRecorder()
		server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rr.Header().Get(clusterNodeHeader); got != "node-b" {
			t.Fatalf("%s: %s = %q, want node-b", path, clusterNodeHeader, got)
		}
	}
}
