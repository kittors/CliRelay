package clusterauth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// TestPostgresCoordinatorsPropagateCredentialChanges runs two nodes with real
// PostgreSQL coordinators (LISTEN/NOTIFY, membership) on one database, so a
// change reaches the peer through a committed notification rather than the
// in-memory bus.
func TestPostgresCoordinatorsPropagateCredentialChanges(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db, nodeDSN := newDisposableDB(t, dsn)
	t.Cleanup(func() { cluster.SetDefault(nil) })
	suffix := fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)

	startNode := func(name string) *Store {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		coord, err := cluster.Start(ctx, cluster.Options{Enabled: true, NodeID: name + "-" + suffix, DSN: nodeDSN, DB: db, Version: "test"})
		if err != nil {
			t.Fatalf("start coordinator %s: %v", name, err)
		}
		t.Cleanup(coord.Close)
		store := New(testOptions(coord.NodeID(), t.TempDir(), coord))
		store.backend = newPGBackend(db, false)
		if err = store.start(ctx); err != nil {
			t.Fatalf("start store %s: %v", name, err)
		}
		t.Cleanup(func() { store.Close(context.Background()) })
		return store
	}
	a := startNode("auth-a")
	b := startNode("auth-b")
	id := tenantA + "/claude.json"

	create(t, a, id, claudeDoc("at-1"))
	eventually(t, "b mirrors the new credential", func() bool { return mirrorVersion(t, b, id) == 1 })

	current := mustGet(t, b, id)
	current.Metadata["access_token"] = "at-2"
	if _, err := b.Save(context.Background(), current); err != nil {
		t.Fatalf("update on b: %v", err)
	}
	eventually(t, "a mirrors b's update", func() bool {
		doc := readMirror(t, a, id)
		return doc != nil && doc["access_token"] == "at-2" && mirrorVersion(t, a, id) == 2
	})

	if err := a.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b drops the deleted credential", func() bool { return readMirror(t, b, id) == nil })
}
