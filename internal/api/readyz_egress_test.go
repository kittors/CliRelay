package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/egresshealth"
	sqlproxypool "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/proxypool"
)

func serveEgressReadyz(server *Server) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz/egress", nil))
	return rr
}

// pipeConn stands in for a successful connect.
func pipeConn() net.Conn {
	client, server := net.Pipe()
	_ = server.Close()
	return client
}

func TestReadyzEgressIsNoContentBeforeTheFirstRound(t *testing.T) {
	server := newTestServer(t)
	if server.egress == nil {
		t.Fatal("NewServer should build the egress prober")
	}
	rr := serveEgressReadyz(server)
	if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
		t.Fatalf("status = %d, body=%q; a node must not fail the check before it has run", rr.Code, rr.Body.String())
	}
}

func TestReadyzEgressReportsCountsWithoutProxyDetails(t *testing.T) {
	server := newTestServer(t)
	var broken atomic.Bool
	broken.Store(true)
	server.egress = egresshealth.New(egresshealth.Options{
		Targets: func(context.Context) egresshealth.Targets {
			return egresshealth.Targets{ProxyURLs: []string{
				"socks5://alice:hunter2@198.51.100.7:1080",
				"http://proxy-b.internal.test:8080",
			}}
		},
		Dial: func(_ context.Context, _, address string) (net.Conn, error) {
			if address == "198.51.100.7:1080" && broken.Load() {
				return nil, errors.New("i/o timeout")
			}
			return pipeConn(), nil
		},
	})
	ctx := context.Background()

	server.egress.RunOnce(ctx)
	if rr := serveEgressReadyz(server); rr.Code != http.StatusNoContent {
		t.Fatalf("one failed connect must not fail the check, got %d %s", rr.Code, rr.Body.String())
	}

	server.egress.RunOnce(ctx)
	rr := serveEgressReadyz(server)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once an endpoint failed twice in a row", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rr.Body.String())
	}
	if len(body) != 3 || body["status"] != "degraded" || body["unreachable"] != float64(1) || body["total"] != float64(2) {
		t.Fatalf("body = %s, want exactly status/unreachable/total", rr.Body.String())
	}
	for _, secret := range []string{"198.51.100.7", "1080", "proxy-b", "8080", "alice", "hunter2", "socks5"} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("the public body leaks %q: %s", secret, rr.Body.String())
		}
	}

	broken.Store(false)
	server.egress.RunOnce(ctx)
	if rr := serveEgressReadyz(server); rr.Code != http.StatusNoContent {
		t.Fatalf("a recovered endpoint should clear the check, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestReadyzEgressNamesTheNodeInClusterMode(t *testing.T) {
	server := newTestServer(t)
	joinTestCluster(t, "node-b")
	rr := serveEgressReadyz(server)
	if got := rr.Header().Get(clusterNodeHeader); got != "node-b" {
		t.Fatalf("%s = %q, want node-b like every other response", clusterNodeHeader, got)
	}
}

func TestReadyzEgressIsExemptFromIPAccessRules(t *testing.T) {
	for _, path := range []string{"/readyz", "/readyz/egress"} {
		if _, exempt := healthProbePaths[path]; !exempt {
			t.Fatalf("%s must bypass the IP access list: the arbiter's probes come from outside", path)
		}
	}
}

// openProxyPoolDB is an in-memory runtime database with a proxy_pool table.
func openProxyPoolDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Each connection to :memory: is a separate database.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	sqlproxypool.InitTable(db)
	return db
}

func TestEgressSourcesCoverEveryEnabledProxy(t *testing.T) {
	db := openProxyPoolDB(t)
	for _, row := range []struct {
		tenant, id, url string
		enabled         int
	}{
		{"11111111-1111-1111-1111-111111111111", "tenant-on", "socks5://t:pw@203.0.113.21:1080", 1},
		{"11111111-1111-1111-1111-111111111111", "tenant-off", "socks5://203.0.113.22:1080", 0},
	} {
		if _, err := db.Exec(`INSERT INTO proxy_pool (tenant_id, id, url, enabled) VALUES (?, ?, ?, ?)`,
			row.tenant, row.id, row.url, row.enabled); err != nil {
			t.Fatal(err)
		}
	}
	useReadyzDB(t, db)
	server := newTestServerWithConfig(t, func(cfg *proxyconfig.Config) {
		cfg.ProxyURL = "socks5h://gw.example.test:7000"
		cfg.PreferIPv4 = true
		cfg.ProxyPool = []proxyconfig.ProxyPoolEntry{
			{ID: "on", URL: "http://203.0.113.11:3128", Enabled: true},
			{ID: "off", URL: "http://203.0.113.12:3128", Enabled: false},
		}
	})

	sources := &egressSources{server: server}
	targets := sources.targets(context.Background())
	endpoints, _ := egresshealth.Endpoints(targets.ProxyURLs)
	want := []string{"203.0.113.11:3128", "203.0.113.21:1080", "gw.example.test:7000"}
	if !slices.Equal(endpoints, want) {
		t.Fatalf("endpoints = %v, want the global proxy and the enabled entries of every tenant %v", endpoints, want)
	}
	if !targets.IPv4Only {
		t.Fatal("prefer-ipv4 should restrict the check to IPv4, as it does upstream traffic")
	}

	// A failed read keeps the tenant pools from the last good one.
	_ = db.Close()
	endpoints, _ = egresshealth.Endpoints(sources.targets(context.Background()).ProxyURLs)
	if !slices.Equal(endpoints, want) {
		t.Fatalf("after a database error endpoints = %v, want %v", endpoints, want)
	}
}

func TestServerRunsTheEgressProberWhileServing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	server := newTestServerWithConfig(t, func(cfg *proxyconfig.Config) {
		cfg.Host = "127.0.0.1"
		cfg.Port = port
	})

	served := make(chan error, 1)
	go func() { served <- server.Start() }()
	deadline := time.Now().Add(5 * time.Second)
	for !server.egress.Status().Checked {
		if time.Now().After(deadline) {
			t.Fatal("Start should run the first egress round right away")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The listener may come up a moment after the first round.
	var resp *http.Response
	for {
		resp, err = http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/readyz/egress")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("GET /readyz/egress = %d, want 204 with no proxy configured", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}
