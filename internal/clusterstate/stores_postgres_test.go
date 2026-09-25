package clusterstate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
)

func TestManagementJobsStore(t *testing.T) {
	db := openTestDB(t)
	store := NewManagementJobs(db)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Millisecond)

	save := func(snap jobsnapshot.Snapshot) {
		t.Helper()
		snap.Kind = "image_generation_test"
		snap.OwnerNode = "node-a"
		snap.TenantID = "tenant-1"
		snap.CreatedAt = created
		snap.UpdatedAt = time.Now().UTC()
		if err := store.Save(ctx, snap, 30*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	save(jobsnapshot.Snapshot{ID: "job-1", Status: "running", Phase: "queued", Version: 1})
	// A large result, as an image would carry, round-trips byte for byte.
	result := []byte(`{"data":[{"b64_json":"` + strings.Repeat("A", 1<<20) + `"}]}`)
	save(jobsnapshot.Snapshot{ID: "job-1", Status: "succeeded", Phase: "completed", Terminal: true, Result: result, Version: 3})
	save(jobsnapshot.Snapshot{ID: "job-1", Status: "running", Phase: "stale", Version: 2})

	snap, ok, err := store.Get(ctx, "image_generation_test", "job-1")
	if err != nil || !ok {
		t.Fatalf("get ok=%v err=%v", ok, err)
	}
	if snap.Status != "succeeded" || snap.Version != 3 || !snap.Terminal || string(snap.Result) != string(result) {
		t.Fatalf("a lower version must never replace the final snapshot: status=%s version=%d len=%d", snap.Status, snap.Version, len(snap.Result))
	}
	if !snap.CreatedAt.Equal(created) || snap.OwnerNode != "node-a" || snap.TenantID != "tenant-1" || snap.Error != nil {
		t.Fatalf("snapshot metadata = %+v", snap)
	}
	if _, ok, _ := store.Get(ctx, "model_test", "job-1"); ok {
		t.Fatal("a snapshot must only be readable under its own kind")
	}

	save(jobsnapshot.Snapshot{ID: "job-2", Status: "running", Version: 1})
	mustExec(t, db, `UPDATE management_jobs SET heartbeat_at = now() - interval '5 minutes' WHERE id = 'job-2'`)
	if snap, _, _ := store.Get(ctx, "image_generation_test", "job-2"); !snap.OwnerLost {
		t.Fatal("an unfinished job without heartbeat must read as lost")
	}
	if err := store.Touch(ctx, "node-a", []string{"job-2"}); err != nil {
		t.Fatal(err)
	}
	if snap, _, _ := store.Get(ctx, "image_generation_test", "job-2"); snap.OwnerLost {
		t.Fatal("a heartbeat must revive the job")
	}

	mustExec(t, db, `UPDATE management_jobs SET expires_at = now() - interval '1 second' WHERE id = 'job-2'`)
	if _, ok, _ := store.Get(ctx, "image_generation_test", "job-2"); ok {
		t.Fatal("an expired snapshot must not be served")
	}
	if err := Sweep(ctx, db); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM management_jobs`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("rows after sweep = %d err=%v, want only the live job", left, err)
	}
}

func TestTaskRoutesStore(t *testing.T) {
	db := openTestDB(t)
	routes := NewTaskRoutes(db)
	ctx := context.Background()

	route := TaskRoute{Kind: "video", TaskID: "req-1", Provider: "xai", AuthID: "xai-2.json", TenantID: "tenant-1", Model: "grok-imagine-video"}
	if err := routes.Remember(ctx, route, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, ok, err := routes.Lookup(ctx, "video", "req-1")
	if err != nil || !ok || got != route {
		t.Fatalf("lookup = %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := routes.Lookup(ctx, "image", "req-1"); ok {
		t.Fatal("a route must only be found under its own kind")
	}
	if err := routes.Forget(ctx, "video", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := routes.Lookup(ctx, "video", "req-1"); ok {
		t.Fatal("a forgotten route must be gone")
	}

	if err := routes.Remember(ctx, TaskRoute{Kind: "video", TaskID: "req-2"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE async_task_routes SET expires_at = now() - interval '1 second' WHERE task_id = 'req-2'`)
	if _, ok, _ := routes.Lookup(ctx, "video", "req-2"); ok {
		t.Fatal("an expired route must not be served")
	}
	if err := Sweep(ctx, db); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM async_task_routes`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("routes after sweep = %d err=%v", left, err)
	}
}
