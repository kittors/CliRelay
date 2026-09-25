package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// testClusterQuota is one node's view of the cluster: the shared store plus a
// fixed live-node count.
type testClusterQuota struct {
	*sharedstate.Store
	nodes int
}

func (q testClusterQuota) ActiveNodes() int { return q.nodes }

func bg() context.Context { return context.Background() }

type quotaCluster struct {
	mr   *miniredis.Miniredis
	self *sharedstate.Store // this process: installed into the middleware
	peer *sharedstate.Store // another node sharing the same Redis
}

func newQuotaCluster(t *testing.T, nodes int) *quotaCluster {
	t.Helper()
	mr := miniredis.RunT(t)
	store := func(nodeID string) *sharedstate.Store {
		client, err := sharedredis.New(config.ClusterRedisConfig{Addr: mr.Addr()}, sharedredis.Options{
			NodeID: nodeID, HealthInterval: 20 * time.Millisecond,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, StableAfter: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		client.Start()
		s := sharedstate.New(client, sharedstate.Options{NodeID: nodeID, FlushInterval: time.Hour})
		t.Cleanup(func() {
			s.Close()
			client.Close()
		})
		return s
	}
	c := &quotaCluster{mr: mr, self: store("self"), peer: store("peer")}
	SetClusterQuota(testClusterQuota{Store: c.self, nodes: nodes})
	t.Cleanup(func() { SetClusterQuota(nil) })
	return c
}

func quotaRouter(metadata map[string]string, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "sk-XXXX-cluster-test")
		c.Set("accessMetadata", metadata)
		c.Next()
	})
	router.Use(QuotaMiddleware())
	if handler == nil {
		handler = func(c *gin.Context) { c.Status(http.StatusNoContent) }
	}
	router.POST("/v1/chat/completions", handler)
	return router
}

func postQuota(router *gin.Engine) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, newQuotaPostRequest("sk-XXXX-cluster-test"))
	return rec
}

func TestClusterRPMLimitIsGlobal(t *testing.T) {
	resetQuotaMiddlewareState(t)
	cluster := newQuotaCluster(t, 2)
	router := quotaRouter(map[string]string{"rpm-limit": "4"}, nil)

	// Another node already served three requests of this key this minute.
	for i := 0; i < 3; i++ {
		if _, err := cluster.peer.AdmitRequest(bg(), "sk-XXXX-cluster-test", 0); err != nil {
			t.Fatal(err)
		}
	}
	if rec := postQuota(router); rec.Code != http.StatusNoContent {
		t.Fatalf("4th request cluster-wide must pass, got %d", rec.Code)
	}
	rec := postQuota(router)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-CliRelay-Quota-Code") != "rpm_limit_exceeded" {
		t.Fatalf("5th request cluster-wide must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-CliRelay-Quota-Limit"); got != "4" {
		t.Fatalf("shared mode enforces the configured limit, header limit = %q", got)
	}
}

func TestClusterConcurrencyLimitIsGlobal(t *testing.T) {
	resetQuotaMiddlewareState(t)
	cluster := newQuotaCluster(t, 2)
	router := quotaRouter(map[string]string{"concurrency-limit": "1"}, nil)

	held, err := cluster.peer.AdmitRequest(bg(), "sk-XXXX-cluster-test", 1)
	if err != nil || !held.SlotTaken {
		t.Fatalf("peer slot: %+v %v", held, err)
	}
	rec := postQuota(router)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-CliRelay-Quota-Code") != "concurrency_limit_exceeded" {
		t.Fatalf("slot held by another node must refuse this one, got %d %s", rec.Code, rec.Body.String())
	}
	held.Release()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if rec = postQuota(router); rec.Code == http.StatusNoContent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot released by the other node never became usable: %d", rec.Code)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := keyConcurrencyCount("sk-XXXX-cluster-test"); n != 0 {
		t.Fatalf("local mirror of the slot must be released with the request, got %d", n)
	}
}

// TestClusterFallsBackToNodeShareAndRecovers covers the degraded mode: with the
// shared Redis gone each node enforces ceil(limit/nodes) on its own counters,
// and once Redis is back the configured limit applies cluster-wide again.
func TestClusterFallsBackToNodeShareAndRecovers(t *testing.T) {
	resetQuotaMiddlewareState(t)
	cluster := newQuotaCluster(t, 2)
	router := quotaRouter(map[string]string{"rpm-limit": "4"}, nil)

	cluster.mr.Close()
	// The first call after the outage may still try Redis; either way it must
	// not fail the request.
	start := time.Now()
	if rec := postQuota(router); rec.Code != http.StatusNoContent {
		t.Fatalf("request during the outage = %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("an unavailable Redis delayed the request by %s", elapsed)
	}
	if rec := postQuota(router); rec.Code != http.StatusNoContent {
		t.Fatalf("second request within this node's share (2) = %d", rec.Code)
	}
	rec := postQuota(router)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request exceeds this node's share of 4/2, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-CliRelay-Quota-Limit"); got != "2" {
		t.Fatalf("degraded mode enforces the node share, header limit = %q", got)
	}

	if err := cluster.mr.Restart(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !cluster.self.Available() {
		if time.Now().After(deadline) {
			t.Fatal("shared counters did not come back")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The restarted Redis lost its windows; the full limit applies again.
	for i := 0; i < 4; i++ {
		if rec := postQuota(router); rec.Code != http.StatusNoContent {
			t.Fatalf("request %d after recovery = %d", i+1, rec.Code)
		}
	}
	if rec := postQuota(router); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fifth request after recovery must be refused, got %d", rec.Code)
	}
}

func TestClusterWithoutSharedRedisSplitsConcurrency(t *testing.T) {
	resetQuotaMiddlewareState(t)
	unconfigured := sharedstate.New(nil, sharedstate.Options{NodeID: "self"})
	t.Cleanup(unconfigured.Close)
	SetClusterQuota(testClusterQuota{Store: unconfigured, nodes: 3})
	t.Cleanup(func() { SetClusterQuota(nil) })

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	router := quotaRouter(map[string]string{"concurrency-limit": "5"}, func(c *gin.Context) {
		entered <- struct{}{}
		<-release
		c.Status(http.StatusNoContent)
	})
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- postQuota(router).Code }()
		<-entered
	}
	// ceil(5/3) = 2 slots on this node.
	if rec := postQuota(router); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third concurrent request exceeds the node share, got %d", rec.Code)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if code := <-done; code != http.StatusNoContent {
			t.Fatalf("held request finished with %d", code)
		}
	}
}

func TestClusterTokensAndDashboardAreShared(t *testing.T) {
	resetQuotaMiddlewareState(t)
	cluster := newQuotaCluster(t, 2)
	router := quotaRouter(map[string]string{"tpm-limit": "1000"}, nil)

	if rec := postQuota(router); rec.Code != http.StatusNoContent {
		t.Fatalf("first request = %d", rec.Code)
	}
	RecordTokenUsage("sk-XXXX-cluster-test", 600)
	cluster.self.Flush()
	// Tokens used on the other node count too.
	cluster.peer.AddTokens("sk-XXXX-cluster-test", 500)
	cluster.peer.Flush()
	rec := postQuota(router)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-CliRelay-Quota-Code") != "tpm_limit_exceeded" {
		t.Fatalf("1100 tokens across the cluster must exceed 1000, got %d %s", rec.Code, rec.Body.String())
	}

	for i := 0; i < 5; i++ {
		if _, err := cluster.peer.AdmitRequest(bg(), "sk-XXXX-cluster-test", 0); err != nil {
			t.Fatal(err)
		}
	}
	snaps, total := GetConcurrencySnapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	if snaps[0].RPM != 7 || snaps[0].TPM != 1100 || total != 7 {
		t.Fatalf("dashboard must show cluster-wide usage, got %+v total %d", snaps[0], total)
	}
}

func TestClusterUnlimitedKeysAreCountedWithoutWaiting(t *testing.T) {
	resetQuotaMiddlewareState(t)
	cluster := newQuotaCluster(t, 2)
	router := quotaRouter(map[string]string{"daily-limit": "100"}, nil)

	for i := 0; i < 3; i++ {
		if rec := postQuota(router); rec.Code != http.StatusNoContent {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
	rates, err := cluster.peer.Rates(bg(), []string{"sk-XXXX-cluster-test"})
	if err != nil {
		t.Fatal(err)
	}
	if rates["sk-XXXX-cluster-test"].RPM != 0 {
		t.Fatalf("unlimited keys are batched, not written per request: %+v", rates)
	}
	cluster.self.Flush()
	rates, _ = cluster.peer.Rates(bg(), []string{"sk-XXXX-cluster-test"})
	if rates["sk-XXXX-cluster-test"].RPM != 3 {
		t.Fatalf("after the flush every node sees them: %+v", rates)
	}
}

func TestSplitLimit(t *testing.T) {
	cases := []struct{ limit, nodes, want int }{
		{0, 3, 0}, {10, 1, 10}, {10, 2, 5}, {5, 3, 2}, {1, 4, 1}, {7, 0, 7},
	}
	for _, tc := range cases {
		if got := splitLimit(tc.limit, tc.nodes); got != tc.want {
			t.Errorf("splitLimit(%d, %d) = %d, want %d", tc.limit, tc.nodes, got, tc.want)
		}
	}
}
