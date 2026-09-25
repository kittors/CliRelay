package clusterauth

import (
	"context"
	"errors"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const tenantA = "11111111-1111-1111-1111-111111111111"

func claudeDoc(accessToken string) map[string]any {
	return map[string]any{"type": "claude", "email": "a@example.com", "access_token": accessToken, "refresh_token": "rt-" + accessToken}
}

func TestCredentialWritesPropagateToPeers(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		id := tenantA + "/claude.json"

		create(t, a, id, claudeDoc("at-1"))
		eventually(t, "b mirrors version 1", func() bool { return mirrorVersion(t, b, id) == 1 })
		if got := readMirror(t, b, id)["access_token"]; got != "at-1" {
			t.Fatalf("b mirror token = %v", got)
		}

		current := mustGet(t, a, id)
		current.Metadata["access_token"] = "at-2"
		if _, err := a.Save(context.Background(), current); err != nil {
			t.Fatalf("update: %v", err)
		}
		eventually(t, "b mirrors version 2", func() bool { return mirrorVersion(t, b, id) == 2 })
		if got := readMirror(t, b, id)["access_token"]; got != "at-2" {
			t.Fatalf("b mirror token = %v, want at-2", got)
		}
		if r := env.row(t, id); r.Version != 2 {
			t.Fatalf("row version = %d, want 2", r.Version)
		}
	})
}

func TestRuntimeSavesNeverBumpVersionOrTouchTokens(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		id := tenantA + "/claude.json"
		create(t, a, id, claudeDoc("at-1"))
		eventually(t, "b sees the credential", func() bool { return mirrorVersion(t, b, id) == 1 })
		snapshot := mustGet(t, a, id)

		// b rotates the token.
		rotated := mustGet(t, b, id)
		rotated.Metadata["access_token"] = "at-2"
		if _, err := b.Save(context.Background(), rotated); err != nil {
			t.Fatal(err)
		}
		eventually(t, "a sees the rotation", func() bool { return mirrorVersion(t, a, id) == 2 })

		// a records request bookkeeping on its pre-rotation copy.
		snapshot.Metadata[coreauth.ClaudeOAuthHealthMetadataKey] = map[string]any{"status": "active", "last_runtime_at": "now"}
		if _, err := a.Save(context.Background(), snapshot); err != nil {
			t.Fatalf("runtime-only save of an unmodified stale copy must succeed: %v", err)
		}
		a.flush(context.Background())

		r := env.row(t, id)
		if r.Version != 2 {
			t.Fatalf("version = %d, runtime saves must not bump it", r.Version)
		}
		if got := env.rowContent(t, id)["access_token"]; got != "at-2" {
			t.Fatalf("token = %v, runtime saves must never write the credential", got)
		}
		runtime, _ := decodeObject(r.Runtime)
		if _, ok := runtime[coreauth.ClaudeOAuthHealthMetadataKey]; !ok {
			t.Fatalf("runtime = %s, want the health observation merged", r.Runtime)
		}
	})
}

func TestEditBasedOnSupersededVersionIsRefused(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		id := "claude.json"
		create(t, a, id, claudeDoc("at-1"))
		eventually(t, "b sees the credential", func() bool { return mirrorVersion(t, b, id) == 1 })

		env.hub.Disconnect("b")
		stale := mustGet(t, b, id)
		current := mustGet(t, a, id)
		current.Metadata["access_token"] = "at-2"
		if _, err := a.Save(context.Background(), current); err != nil {
			t.Fatal(err)
		}

		stale.Metadata["label"] = "edited on b"
		_, err := b.Save(context.Background(), stale)
		if !errors.Is(err, coreauth.ErrCredentialConflict) {
			t.Fatalf("err = %v, want ErrCredentialConflict", err)
		}
		doc := env.rowContent(t, id)
		if doc["access_token"] != "at-2" || doc["label"] != nil {
			t.Fatalf("row = %v, an edit of version 1 must not overwrite version 2", doc)
		}
		if mirrorVersion(t, b, id) != 2 {
			t.Fatal("the conflict must converge b's mirror to the newest version")
		}
	})
}

func TestDeletedCredentialIsNeverResurrectedImplicitly(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		c := env.startNode(t, "c", t.TempDir())
		id := tenantA + "/codex.json"
		create(t, a, id, map[string]any{"type": "codex", "access_token": "at-1"})
		eventually(t, "peers see the credential", func() bool {
			return mirrorVersion(t, b, id) == 1 && mirrorVersion(t, c, id) == 1
		})
		env.hub.Disconnect("c")
		stale := mustGet(t, c, id)
		index := a.ResolveAuthIndex(id)

		if err := a.Delete(context.Background(), a.mirrorPath(id)); err != nil {
			t.Fatal(err)
		}
		eventually(t, "b drops its mirror", func() bool { return readMirror(t, b, id) == nil })
		if _, err := b.Save(context.Background(), mustGetMirrorAuth(t, stale)); !errors.Is(err, coreauth.ErrCredentialGone) {
			t.Fatalf("save on a node that saw the delete: err = %v, want ErrCredentialGone", err)
		}

		// c missed the delete: its runtime bookkeeping and its edit arrive late.
		runtimeOnly := stale.Clone()
		runtimeOnly.Metadata[coreauth.ClaudeOAuthHealthMetadataKey] = map[string]any{"status": "active"}
		_, _ = c.Save(context.Background(), runtimeOnly)
		c.flush(context.Background())
		edited := stale.Clone()
		edited.Metadata["access_token"] = "at-late"
		if _, err := c.Save(context.Background(), edited); !errors.Is(err, coreauth.ErrCredentialGone) {
			t.Fatalf("late edit: err = %v, want ErrCredentialGone", err)
		}
		r := env.row(t, id)
		if !r.Deleted || r.Version != 2 || string(r.Content) != "{}" {
			t.Fatalf("row = deleted:%v version:%d content:%s, the tombstone must survive late writes", r.Deleted, r.Version, r.Content)
		}
		if readMirror(t, c, id) != nil {
			t.Fatal("the refused write must remove c's stale mirror")
		}

		// Only an explicit create brings it back, with a newer version and the same index.
		create(t, b, id, map[string]any{"type": "codex", "access_token": "at-new"})
		r = env.row(t, id)
		if r.Deleted || r.Version != 3 || r.AuthIndex != index {
			t.Fatalf("revived row = deleted:%v version:%d index:%s, want live version 3 index %s", r.Deleted, r.Version, r.AuthIndex, index)
		}
	})
}

// mustGetMirrorAuth returns a copy of auth as a node would still hold it.
func mustGetMirrorAuth(t *testing.T, auth *coreauth.Auth) *coreauth.Auth {
	t.Helper()
	return auth.Clone()
}

func TestResyncReconcilesMissedChanges(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		env.hub.Disconnect("b")
		create(t, a, "one.json", claudeDoc("at-1"))
		create(t, a, "two.json", claudeDoc("at-2"))
		if err := a.Delete(context.Background(), "two.json"); err != nil {
			t.Fatal(err)
		}
		if readMirror(t, b, "one.json") != nil {
			t.Fatal("a disconnected node must not have heard about the change")
		}
		env.hub.Reconnect("b")
		eventually(t, "b reconciles after the resync", func() bool {
			return mirrorVersion(t, b, "one.json") == 1 && readMirror(t, b, "two.json") == nil
		})
	})
}

func TestRequestResultSavesDeferCredentialWrites(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		id := "codex.json"
		create(t, a, id, map[string]any{"type": "codex", "access_token": "at-1"})
		mgr := coreauth.NewManager(a, nil, nil)
		if err := mgr.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		// A refresh result the database could not take yet lives in memory only.
		current, _ := mgr.GetByID(id)
		current.Metadata["access_token"] = "at-2"
		if _, err := mgr.Update(coreauth.WithSkipPersist(context.Background()), current); err != nil {
			t.Fatal(err)
		}
		if env.mem != nil {
			env.mem.setDown(true)
		}
		mgr.MarkResult(context.Background(), coreauth.Result{AuthID: id, Provider: "codex", Success: true})
		if env.mem != nil {
			a.flush(context.Background())
			if a.writes[id] == nil {
				t.Fatal("a deferred write the database refused must stay queued")
			}
			env.mem.setDown(false)
			a.mu.Lock()
			a.writes[id].notBefore = time.Time{}
			a.mu.Unlock()
		}
		if got := env.rowContent(t, id)["access_token"]; got != "at-1" {
			t.Fatalf("token = %v, the request path must not write inline", got)
		}
		a.flush(context.Background())
		if got := env.rowContent(t, id)["access_token"]; got != "at-2" || env.row(t, id).Version != 2 {
			t.Fatalf("token = %v, want the deferred write persisted as version 2", got)
		}
	})
}

func TestRefreshLeaseIsExclusive(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		id := "claude.json"
		create(t, a, id, claudeDoc("at-1"))

		latest, acquired, err := a.ClaimRefresh(context.Background(), id)
		if err != nil || !acquired || latest == nil || coreauth.CredentialVersion(latest) != 1 {
			t.Fatalf("a claim = %v %v %v", latest, acquired, err)
		}
		if _, acquired, err = b.ClaimRefresh(context.Background(), id); err != nil || acquired {
			t.Fatalf("b claim while a holds the lease = %v %v, want not acquired", acquired, err)
		}
		if _, acquired, _ = a.ClaimRefresh(context.Background(), id); !acquired {
			t.Fatal("the holder may renew its own lease")
		}
		a.ReleaseRefresh(context.Background(), id)
		if _, acquired, err = b.ClaimRefresh(context.Background(), id); err != nil || !acquired {
			t.Fatalf("b claim after release = %v %v", acquired, err)
		}
		if _, _, err = b.ClaimRefresh(context.Background(), "missing.json"); !errors.Is(err, coreauth.ErrCredentialGone) {
			t.Fatalf("claim of a missing credential: err = %v", err)
		}
	})
}

func TestFailoverRollbackRestoresNewerCredential(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		id := "codex.json"
		create(t, a, id, map[string]any{"type": "codex", "access_token": "at-1", "refresh_token": "rt-1"})
		current := mustGet(t, a, id)
		current.Metadata["access_token"], current.Metadata["refresh_token"] = "at-2", "rt-2"
		if _, err := a.Save(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		// The failover lost version 2; the old refresh token is already spent.
		env.rollback(t, id, 1, map[string]any{"type": "codex", "access_token": "at-1", "refresh_token": "rt-1"})
		a.reconcile()
		doc := env.rowContent(t, id)
		if doc["refresh_token"] != "rt-2" || env.row(t, id).Version != 2 {
			t.Fatalf("row = %v version %d, want the newer credential written back", doc, env.row(t, id).Version)
		}
	})
}

func TestMutateAuthThroughManagerRetriesOnPeerWrite(t *testing.T) {
	forEachBackend(t, func(t *testing.T, env *testEnv) {
		a := env.startNode(t, "a", t.TempDir())
		b := env.startNode(t, "b", t.TempDir())
		id := "claude.json"
		create(t, a, id, claudeDoc("at-1"))
		eventually(t, "b sees the credential", func() bool { return mirrorVersion(t, b, id) == 1 })
		mgr := coreauth.NewManager(a, nil, nil)
		if err := mgr.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		base, _ := mgr.GetByID(id)

		rotated := mustGet(t, b, id)
		rotated.Metadata["access_token"] = "at-2"
		if _, err := b.Save(context.Background(), rotated); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.MutateAuth(context.Background(), base, func(auth *coreauth.Auth) (bool, error) {
			auth.Metadata["label"] = "ops"
			return true, nil
		}); err != nil {
			t.Fatalf("MutateAuth: %v", err)
		}
		doc := env.rowContent(t, id)
		if doc["access_token"] != "at-2" || doc["label"] != "ops" {
			t.Fatalf("row = %v, the edit must land on top of b's rotation", doc)
		}
	})
}
