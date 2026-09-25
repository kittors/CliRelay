package usage

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// installClusterRole makes this process a leader or a follower of a two-node
// in-memory cluster for the rest of the test.
func installClusterRole(t *testing.T, leader bool) {
	t.Helper()
	hub := cluster.NewMemoryHub()
	first, second := hub.Join("node-a"), hub.Join("node-b")
	if leader {
		cluster.SetDefault(first)
	} else {
		cluster.SetDefault(second)
	}
	t.Cleanup(func() { cluster.SetDefault(nil) })
}

func TestScheduledOpenRouterSyncRunsOnlyOnLeader(t *testing.T) {
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(CloseDB)
	if _, err := UpdateOpenRouterModelSyncSettings(true, 60); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	restore := SetOpenRouterModelFetcherForTest(func(context.Context) ([]OpenRouterRemoteModel, error) {
		fetches.Add(1)
		return nil, errors.New("offline in tests")
	})
	t.Cleanup(restore)

	installClusterRole(t, false)
	runDueOpenRouterModelSyncs(context.Background())
	if n := fetches.Load(); n != 0 {
		t.Fatalf("a follower fetched the shared catalog %d times", n)
	}
	installClusterRole(t, true)
	runDueOpenRouterModelSyncs(context.Background())
	if n := fetches.Load(); n != 1 {
		t.Fatalf("the leader fetched %d times, want 1", n)
	}
}

func TestRequestLogMaintenanceSharedWorkRunsOnlyOnLeader(t *testing.T) {
	// Follower before InitDB, so the pass InitDB starts is a follower pass too.
	installClusterRole(t, false)
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(CloseDB)
	db := getDB()
	expired := time.Now().UTC().Add(-72 * time.Hour).Format("2006-01-02T15:04")
	if _, err := db.Exec(`INSERT INTO usage_rollup_buckets (bucket_kind, bucket_start, updated_at) VALUES (?, ?, ?)`,
		rollupBucketMinute, expired, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	countExpired := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM usage_rollup_buckets WHERE bucket_start = ?`, expired).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	requestLogContentBytes.Store(-1)
	runRequestLogMaintenancePass(context.Background(), db, "sqlite")
	if countExpired() != 1 {
		t.Fatal("a follower must leave the shared tables to the leader")
	}
	if got := requestLogContentBytes.Load(); got != 0 {
		t.Fatalf("a follower must still refresh its content size total, got %d", got)
	}

	installClusterRole(t, true)
	runRequestLogMaintenancePass(context.Background(), db, "sqlite")
	if countExpired() != 0 {
		t.Fatal("the leader must prune expired rollup buckets")
	}
}
