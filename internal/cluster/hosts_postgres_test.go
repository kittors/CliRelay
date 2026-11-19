package cluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A host agent upserts one row per host; the console reads it back with an
// age by the database clock.
func TestPostgresHostsUpsertAndList(t *testing.T) {
	pc := newPGCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		_, _ = pc.db.Exec(`DELETE FROM cluster_hosts WHERE host_id LIKE $1`, pc.prefix+"%")
	})
	id := pc.prefix + "db"
	if err := UpsertHost(ctx, pc.db, id, HostRoleDatabase, "dev-1", json.RawMessage(`{"schema":1,"disks":[{"mount":"/"}]}`)); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := UpsertHost(ctx, pc.db, id, HostRoleDatabase, "dev-2", json.RawMessage(`{"schema":1,"host":{"cpus":4}}`)); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	hosts, err := ListHosts(ctx, pc.db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *HostStatus
	for i := range hosts {
		if hosts[i].HostID == id {
			found = &hosts[i]
		}
	}
	if found == nil {
		t.Fatalf("host %s missing from %+v", id, hosts)
	}
	var snapshot struct {
		Host *struct {
			CPUs int `json:"cpus"`
		} `json:"host"`
		Disks []any `json:"disks"`
	}
	if err := json.Unmarshal(found.Snapshot, &snapshot); err != nil {
		t.Fatalf("snapshot %s: %v", found.Snapshot, err)
	}
	if found.Role != HostRoleDatabase || found.Version != "dev-2" || snapshot.Host == nil || snapshot.Host.CPUs != 4 || snapshot.Disks != nil {
		t.Fatalf("host = %+v (%s), want the second write to replace the first", found, found.Snapshot)
	}
	if found.AgeSeconds < 0 || found.AgeSeconds > 60 || found.SnapshotAt.IsZero() {
		t.Fatalf("age = %v at %s", found.AgeSeconds, found.SnapshotAt)
	}

	if err := UpsertHost(ctx, pc.db, " ", HostRoleDatabase, "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("an empty host id was stored")
	}
	if err := UpsertHost(ctx, pc.db, id, HostRoleDatabase, "", json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid JSON was stored")
	}
}
