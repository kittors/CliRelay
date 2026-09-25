package clusterauth

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const systemTenant = "00000000-0000-0000-0000-000000000001"

// seedSingleNodeDir lays out an auth directory the way a single node leaves
// it: legacy root files, tenant files, runtime keys, and junk it ignores.
func seedSingleNodeDir(t *testing.T, dir string) {
	t.Helper()
	writeAuthFile(t, dir, "claude-root.json", map[string]any{
		"type": "claude", "email": "root@example.com", "access_token": "at-root", "refresh_token": "rt-root",
		coreauth.ClaudeOAuthHealthMetadataKey: map[string]any{"status": "active"},
	})
	writeAuthFile(t, dir, systemTenant+"/codex-sys.json", map[string]any{"type": "codex", "email": "sys@example.com", "access_token": "at-sys"})
	writeAuthFile(t, dir, tenantA+"/claude-tenant.json", map[string]any{"type": "claude", "email": "t@example.com", "access_token": "at-t", "refresh_token": "rt-t"})
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFirstNodeImportsItsAuthDirectory(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		dir := t.TempDir()
		seedSingleNodeDir(t, dir)
		// claude-tenant.json was added by an OAuth login since the last restart,
		// so its binding records the base-name index it has been logging under.
		tenantID := tenantA + "/claude-tenant.json"
		basenameIndex := coreauth.FileAuthIndex(path.Base(tenantID))
		env.setBinding(t, tenantA, tenantID, basenameIndex)

		a := env.startNode(t, "a", dir)
		live, err := env.newBackend().listLive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 3 {
			t.Fatalf("imported %d rows, want the 3 credential files", len(live))
		}
		for _, id := range []string{"claude-root.json", systemTenant + "/codex-sys.json", tenantID} {
			if mirrorVersion(t, a, id) != 1 {
				t.Fatalf("%s: mirror must be rewritten stamped with version 1", id)
			}
		}
		r := env.row(t, "claude-root.json")
		if strings.Contains(string(r.Content), coreauth.ClaudeOAuthHealthMetadataKey) || !strings.Contains(string(r.Runtime), "active") {
			t.Fatalf("runtime keys must be split out: content=%s runtime=%s", r.Content, r.Runtime)
		}
		if r.AuthIndex != coreauth.FileAuthIndex("claude-root.json") {
			t.Fatalf("root index = %s", r.AuthIndex)
		}
		if got := env.row(t, tenantID).AuthIndex; got != basenameIndex {
			t.Fatalf("tenant index = %s, want the base-name index its binding records", got)
		}
		if got := env.row(t, systemTenant+"/codex-sys.json").AuthIndex; got != coreauth.FileAuthIndex(systemTenant+"/codex-sys.json") {
			t.Fatalf("unbound tenant file index = %s, want the startup index", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "broken.json")); err != nil {
			t.Fatal("files that are not credentials must be left alone")
		}
		for _, rel := range listDir(t, dir) {
			if strings.HasPrefix(rel, util.PreClusterBackupDirPrefix) {
				t.Fatalf("imported files must not be set aside, found %s", rel)
			}
		}

		// The first request on an imported account only records runtime state.
		mgr := coreauth.NewManager(a, nil, nil)
		if err = mgr.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, auth := range mgr.List() {
			mgr.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: auth.Provider, Success: true})
		}
		a.flush(context.Background())
		for _, id := range []string{"claude-root.json", systemTenant + "/codex-sys.json", tenantID} {
			if v := env.row(t, id).Version; v != 1 {
				t.Fatalf("%s: version %d after the first request, want the import's 1", id, v)
			}
		}
	})
}

func TestJoiningNodeSetsLocalFilesAsideInsteadOfImporting(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		first := t.TempDir()
		seedSingleNodeDir(t, first)
		a := env.startNode(t, "a", first)
		create(t, a, "deleted.json", claudeDoc("at-del"))
		if err := a.Delete(context.Background(), "deleted.json"); err != nil {
			t.Fatal(err)
		}

		joining := t.TempDir()
		// A stale copy with an older token, a copy of a credential deleted
		// since, a credential the cluster never had, and an identical copy.
		writeAuthFile(t, joining, "claude-root.json", map[string]any{"type": "claude", "email": "root@example.com", "access_token": "at-stale", "refresh_token": "rt-stale"})
		writeAuthFile(t, joining, "deleted.json", claudeDoc("at-del"))
		writeAuthFile(t, joining, tenantA+"/unknown.json", claudeDoc("at-unknown"))
		sysDoc := map[string]any{"type": "codex", "email": "sys@example.com", "access_token": "at-sys"}
		writeAuthFile(t, joining, systemTenant+"/codex-sys.json", sysDoc)

		b := env.startNode(t, "b", joining)
		if n, _ := env.newBackend().count(context.Background()); n != 4 {
			t.Fatalf("rows = %d, a joining node must not import anything", n)
		}
		if got := readMirror(t, b, "claude-root.json")["access_token"]; got != "at-root" {
			t.Fatalf("root mirror token = %v, the table must win over the stale copy", got)
		}
		if readMirror(t, b, "deleted.json") != nil || readMirror(t, b, tenantA+"/unknown.json") != nil {
			t.Fatal("deleted and unknown credentials must not stay live on the joining node")
		}
		var backups []string
		for _, rel := range listDir(t, joining) {
			if strings.HasPrefix(rel, util.PreClusterBackupDirPrefix) {
				backups = append(backups, rel[strings.Index(rel, "/")+1:])
			}
		}
		sort.Strings(backups)
		want := []string{"claude-root.json", "deleted.json", tenantA + "/unknown.json"}
		sort.Strings(want)
		if strings.Join(backups, ",") != strings.Join(want, ",") {
			t.Fatalf("backed up %v, want %v (the identical copy needs no backup)", backups, want)
		}
		if mirrorVersion(t, b, systemTenant+"/codex-sys.json") != 1 {
			t.Fatal("the identical copy must become a stamped mirror")
		}
	})
}

func TestAuthIndexUnchangedWhenSwitchingToClusterMode(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		dir := t.TempDir()
		seedSingleNodeDir(t, dir)

		// Single node: the file store loads the directory at startup.
		fileStore := sdkauth.NewFileTokenStore()
		fileStore.SetBaseDir(dir)
		single := coreauth.NewManager(fileStore, nil, nil)
		if err := single.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		before := make(map[string]string)
		for _, auth := range single.List() {
			before[auth.ID] = auth.Index
		}

		clusterDir := t.TempDir()
		seedSingleNodeDir(t, clusterDir)
		store := env.startNode(t, "a", clusterDir)
		clustered := coreauth.NewManager(store, nil, nil)
		if err := clustered.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		after := make(map[string]string)
		for _, auth := range clustered.List() {
			after[auth.ID] = auth.Index
		}
		if len(before) != 3 || len(after) != len(before) {
			t.Fatalf("before=%v after=%v", before, after)
		}
		for id, index := range before {
			if after[id] != index {
				t.Fatalf("%s: auth_index %s before cluster mode, %s after", id, index, after[id])
			}
		}

		// Registration paths that seed the index differently still agree:
		// an OAuth save names the credential by its base name.
		oauth := &coreauth.Auth{ID: tenantA + "/claude-tenant.json", FileName: "claude-tenant.json", Metadata: map[string]any{"type": "claude"}}
		registered, err := clustered.Register(coreauth.WithSkipPersist(context.Background()), oauth)
		if err != nil {
			t.Fatal(err)
		}
		if registered.Index != before[oauth.ID] {
			t.Fatalf("OAuth-style registration index = %s, want the pinned %s", registered.Index, before[oauth.ID])
		}
	})
}

func TestRunningNodeSetsStrayFilesAsideAfterGrace(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		dir := t.TempDir()
		a := env.startNode(t, "a", dir)
		old := writeAuthFile(t, dir, "dropped.json", claudeDoc("at-x"))
		past := time.Now().Add(-time.Hour)
		if err := os.Chtimes(old, past, past); err != nil {
			t.Fatal(err)
		}
		writeAuthFile(t, dir, "uploading.json", claudeDoc("at-y"))
		a.opts.StrayGrace = time.Minute
		a.reconcileLocalFiles(strayDirPrefix(), a.opts.StrayGrace)
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("a stray file past the grace period must be moved aside")
		}
		if _, err := os.Stat(filepath.Join(dir, "uploading.json")); err != nil {
			t.Fatal("a fresh unstamped file (an upload in flight) must be left alone")
		}
		found := false
		for _, rel := range listDir(t, dir) {
			if strings.HasPrefix(rel, util.ClusterStrayDirPrefix) && strings.HasSuffix(rel, "/dropped.json") {
				found = true
			}
		}
		if !found {
			t.Fatal("the stray file must be kept in the stray directory")
		}
	})
}
