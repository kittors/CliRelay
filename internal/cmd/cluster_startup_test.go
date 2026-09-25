package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

func TestPostStartRepairsRunUnderRepairLock(t *testing.T) {
	var order []string
	runRuntimeDataStackPostStartMaintenance(runtimeDataStackMaintenanceOps{
		runAIAccountSharedSubjectBackfill: func() error {
			order = append(order, "repair")
			return nil
		},
		withRepairLock: func(fn func() error) error {
			order = append(order, "lock")
			err := fn()
			order = append(order, "unlock")
			return err
		},
	})
	if strings.Join(order, ",") != "lock,repair,unlock" {
		t.Fatalf("repairs must run inside the repair lock, got %v", order)
	}
}

func TestWithRepairLockWithoutDatabaseRunsDirectly(t *testing.T) {
	ran := false
	if err := withRepairLock(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("withRepairLock without a runtime database: ran=%v err=%v", ran, err)
	}
}

func TestWithRuntimeMigrationLockWithoutDSNRunsDirectly(t *testing.T) {
	want := errors.New("postgres dsn is required")
	if err := withRuntimeMigrationLock(&config.Config{}, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("missing DSN must surface the initialisation error, got %v", err)
	}
}

func TestCloseClusterOnShutdownLeavesAtCancel(t *testing.T) {
	hub := cluster.NewMemoryHub()
	node := hub.Join("node-a")
	if !node.IsLeader() {
		t.Fatal("first node must lead")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := closeClusterOnShutdown(ctx, node)
	defer stop()
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for node.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("coordinator was not closed when shutdown began")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseClusterOnShutdownStopDoesNotClose(t *testing.T) {
	hub := cluster.NewMemoryHub()
	node := hub.Join("node-a")
	ctx, cancel := context.WithCancel(context.Background())
	stop := closeClusterOnShutdown(ctx, node)
	stop()
	cancel()
	time.Sleep(50 * time.Millisecond)
	if !node.IsLeader() {
		t.Fatal("a stopped watch must not close the coordinator")
	}
}

func TestPrepareClusterSingleNodeIsNoop(t *testing.T) {
	t.Cleanup(func() { cluster.SetDefault(nil) })
	cluster.SetDefault(nil)
	prepareCluster(&config.Config{})
	if cluster.Default().Enabled() || !cluster.Default().IsLeader() {
		t.Fatal("single-node startup must keep the single-node coordinator")
	}
}

func TestPrepareClusterInstallsFollower(t *testing.T) {
	t.Cleanup(func() {
		cluster.SetDefault(nil)
		compatdriver.SetDialKeepAlive(net.KeepAliveConfig{})
	})
	cfg := &config.Config{}
	cfg.Cluster.Enabled = true
	cfg.Cluster.NodeID = "node-under-test"
	prepareCluster(cfg)
	c := cluster.Default()
	if !c.Enabled() || c.IsLeader() || c.NodeID() != "node-under-test" {
		t.Fatalf("cluster startup must begin as a follower: enabled=%v leader=%v id=%s", c.Enabled(), c.IsLeader(), c.NodeID())
	}
}

func TestPostgresMigrationLockSerialisesStartup(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	cfg := &config.Config{}
	cfg.Postgres.DSN = dsn
	var mu sync.Mutex
	inside, maxInside, runs := 0, 0, 0
	init := func() error {
		mu.Lock()
		inside++
		runs++
		if inside > maxInside {
			maxInside = inside
		}
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- withRuntimeMigrationLock(cfg, init)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if runs != 3 || maxInside != 1 {
		t.Fatalf("runs=%d, max concurrent=%d; startups must run one at a time", runs, maxInside)
	}
}
