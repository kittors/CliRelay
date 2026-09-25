package clusterauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// testEnv is one simulated cluster: a shared backend and a memory bus.
type testEnv struct {
	name       string
	hub        *cluster.MemoryHub
	newBackend func() backend
	// db is set for the PostgreSQL variant.
	db  *sql.DB
	mem *memBackend
}

// forEachBackend runs fn against the in-memory backend and, when
// CLIRELAY_POSTGRES_TEST_DSN is set, against a disposable PostgreSQL database
// migrated with the runtime migrations.
func forEachBackend(t *testing.T, fn func(t *testing.T, env *testEnv)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		mem := newMemBackend()
		fn(t, &testEnv{name: "memory", hub: cluster.NewMemoryHub(), mem: mem, newBackend: func() backend { return mem }})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
		if dsn == "" {
			t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
		}
		db := newDisposableDB(t, dsn)
		fn(t, &testEnv{name: "postgres", hub: cluster.NewMemoryHub(), db: db, newBackend: func() backend { return newPGBackend(db, false) }})
	})
}

func newDisposableDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	admin, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("clusterauth_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close()
		t.Fatalf("create disposable database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name)
		_ = admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	db, err := sql.Open(compatdriver.DriverName, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = postgresstore.ApplyMigrations(ctx, db, postgresstore.RuntimeMigrations()); err != nil {
		t.Fatalf("migrate disposable database: %v", err)
	}
	return db
}

func testOptions(name, dir string, coord *cluster.Coordinator) Options {
	return Options{
		AuthDir:           dir,
		NodeID:            name,
		Coordinator:       func() *cluster.Coordinator { return coord },
		ReconcileInterval: time.Hour,
		LocalScanInterval: time.Hour,
		FlushInterval:     time.Hour,
		StrayGrace:        time.Hour,
	}
}

// startNode joins name to the bus and starts a store on dir. Loops run with
// long intervals; tests drive flush and reconcile explicitly.
func (env *testEnv) startNode(t *testing.T, name, dir string) *Store {
	t.Helper()
	coord := env.hub.Join(name)
	store := New(testOptions(name, dir, coord))
	store.backend = env.newBackend()
	if err := store.start(context.Background()); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

func (env *testEnv) row(t *testing.T, id string) *row {
	t.Helper()
	r, err := env.newBackend().get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (env *testEnv) rowContent(t *testing.T, id string) map[string]any {
	t.Helper()
	r := env.row(t, id)
	if r == nil {
		t.Fatalf("row %s missing", id)
	}
	doc, err := decodeObject(r.Content)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// rollback simulates a failover that lost the last write of id.
func (env *testEnv) rollback(t *testing.T, id string, version int64, content map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(content)
	if env.mem != nil {
		env.mem.rollback(id, version, raw)
		return
	}
	if _, err := env.db.Exec(`UPDATE auth_credentials SET version = $2, content = $3::jsonb, deleted_at = NULL WHERE id = $1`, id, version, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func (env *testEnv) setBinding(t *testing.T, tenantID, authID, index string) {
	t.Helper()
	if env.mem != nil {
		env.mem.mu.Lock()
		env.mem.bindings[bindingKey(tenantID, authID)] = index
		env.mem.mu.Unlock()
		return
	}
	if _, err := env.db.Exec(`INSERT INTO ai_account_subjects (auth_subject_id, provider, subject_scope, seed_kind, seed_hash, created_at, updated_at)
		VALUES ($1, 'claude', 'tenant', 'test', $1, now(), now()) ON CONFLICT DO NOTHING`, "subject-"+index); err != nil {
		t.Fatalf("insert subject: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO ai_account_tenant_bindings (tenant_id, auth_id, auth_index, provider, auth_subject_id,
		binding_seed_kind, binding_seed_hash, binding_state, binding_revision, bound_at, last_seen_at)
		VALUES ($1::uuid, $2, $3, 'claude', $4, 'test', $3, 'active', 1, now(), now())`, tenantID, authID, index, "subject-"+index); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
}

func writeAuthFile(t *testing.T, dir, id string, doc map[string]any) string {
	t.Helper()
	target := filepath.Join(dir, filepath.FromSlash(id))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

// readMirror returns the mirror document of id, or nil when there is none.
func readMirror(t *testing.T, s *Store, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(s.mirrorPath(id))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	doc := make(map[string]any)
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("mirror of %s: %v", id, err)
	}
	return doc
}

func mirrorVersion(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	return coreauth.MetadataCredentialVersion(readMirror(t, s, id))
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func create(t *testing.T, s *Store, id string, doc map[string]any) {
	t.Helper()
	metadata := make(map[string]any, len(doc))
	for k, v := range doc {
		metadata[k] = v
	}
	if _, err := s.Save(coreauth.WithCredentialCreate(context.Background()), &coreauth.Auth{ID: id, Metadata: metadata}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func mustGet(t *testing.T, s *Store, id string) *coreauth.Auth {
	t.Helper()
	auth, err := s.Get(context.Background(), id)
	if err != nil || auth == nil {
		t.Fatalf("get %s: auth=%v err=%v", id, auth, err)
	}
	return auth
}

// listDir returns the relative paths of all regular files under dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}
