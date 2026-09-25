package executor

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func installSharedIDs(t *testing.T) (*miniredis.Miniredis, *sharedstate.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := sharedredis.New(config.ClusterRedisConfig{Addr: mr.Addr()}, sharedredis.Options{
		NodeID: "self", HealthInterval: 20 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, StableAfter: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	store := sharedstate.New(client, sharedstate.Options{NodeID: "self"})
	SetSharedIDMinter(store)
	resetSyntheticIDCaches()
	t.Cleanup(func() {
		SetSharedIDMinter(nil)
		resetSyntheticIDCaches()
		store.Close()
		client.Close()
	})
	return mr, store
}

// resetSyntheticIDCaches empties this process's caches, which is what a
// request landing on another node looks like: same shared Redis, cold cache.
func resetSyntheticIDCaches() {
	codexCacheMu.Lock()
	codexCacheMap = make(map[string]codexCache)
	codexCacheMu.Unlock()
	resetUserIDCache()
}

func sharedIDTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "codex-acct-1", Provider: "codex"}
}

func TestSyntheticCodexSessionIDIsClusterWide(t *testing.T) {
	installSharedIDs(t)
	auth := sharedIDTestAuth()

	first := codexSyntheticSessionID(auth, "gpt-5.5", "first_user:abc")
	resetSyntheticIDCaches() // next turn served by another node
	second := codexSyntheticSessionID(auth, "gpt-5.5", "first_user:abc")
	if first == "" || first != second {
		t.Fatalf("one conversation must get one session id across nodes: %q vs %q", first, second)
	}
	parsed, err := uuid.Parse(first)
	if err != nil || parsed.Version() != 7 {
		t.Fatalf("the synthetic session id must stay a UUIDv7, got %q (%v)", first, err)
	}
	if other := codexSyntheticSessionID(auth, "gpt-5.5", "first_user:other"); other == first {
		t.Fatal("different conversations must not share an id")
	}
}

func TestClaudeToCodexPromptCacheKeyIsClusterWide(t *testing.T) {
	installSharedIDs(t)
	key := codexPromptCacheMapKey(sharedIDTestAuth(), "gpt-5.5", "user_abc_account__session_1")

	first := getOrCreateCodexCacheID(key, codexClaudePromptCacheTTL, uuid.NewString)
	resetSyntheticIDCaches()
	second := getOrCreateCodexCacheID(key, codexClaudePromptCacheTTL, uuid.NewString)
	if first != second {
		t.Fatalf("prompt cache key differs across nodes: %q vs %q", first, second)
	}
	if parsed, err := uuid.Parse(first); err != nil || parsed.Version() != 4 {
		t.Fatalf("the Claude-to-Codex cache key must stay a UUIDv4, got %q (%v)", first, err)
	}
}

func TestCloakedUserIDIsClusterWide(t *testing.T) {
	installSharedIDs(t)
	first := cachedUserID("sk-ant-XXXX-shared")
	resetUserIDCache()
	second := cachedUserID("sk-ant-XXXX-shared")
	if first != second {
		t.Fatalf("cloaked user_id differs across nodes: %q vs %q", first, second)
	}
	if !isValidUserID(first) {
		t.Fatalf("the user_id must keep the Claude Code shape, got %q", first)
	}
}

func TestAdoptedCodexIDExpiresWithTheSharedCopy(t *testing.T) {
	mr, _ := installSharedIDs(t)
	auth := sharedIDTestAuth()
	first := codexSyntheticSessionID(auth, "gpt-5.5", "seed")
	mr.FastForward(40 * time.Minute)
	resetSyntheticIDCaches()
	if got := codexSyntheticSessionID(auth, "gpt-5.5", "seed"); got != first {
		t.Fatalf("adopted id = %q, want %q", got, first)
	}
	codexCacheMu.RLock()
	entry := codexCacheMap[codexPromptCacheMapKey(auth, "gpt-5.5", "seed")]
	codexCacheMu.RUnlock()
	if left := time.Until(entry.Expire); left > 21*time.Minute || left < 19*time.Minute || !entry.Shared {
		t.Fatalf("a node adopting an id must expire it with the shared copy (~20m left), got %s shared=%v", left, entry.Shared)
	}
}

func TestSyntheticIDsFallBackToLocalMintingAndReconcile(t *testing.T) {
	mr, store := installSharedIDs(t)
	auth := sharedIDTestAuth()
	key := codexPromptCacheMapKey(auth, "gpt-5.5", "seed")

	mr.Close()
	local := codexSyntheticSessionID(auth, "gpt-5.5", "seed")
	if parsed, err := uuid.Parse(local); err != nil || parsed.Version() != 7 {
		t.Fatalf("local fallback must keep the format, got %q", local)
	}
	if again := codexSyntheticSessionID(auth, "gpt-5.5", "seed"); again != local {
		t.Fatal("the local cache still keeps the conversation stable while Redis is down")
	}

	// Meanwhile another node minted its own id for the same conversation.
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !store.Available() {
		if time.Now().After(deadline) {
			t.Fatal("store did not recover")
		}
		time.Sleep(5 * time.Millisecond)
	}
	other, err := store.MintID(bg(), sharedIDNamespaceCodex, key, uuid.NewString(), time.Hour, false)
	if err != nil || other.ID == local {
		t.Fatalf("setup: %+v %v", other, err)
	}
	if got := codexSyntheticSessionID(auth, "gpt-5.5", "seed"); got != other.ID {
		t.Fatalf("after recovery the node must adopt the cluster's id, got %q want %q", got, other.ID)
	}
}

func TestUserIDRefreshAdoptsTheClusterValue(t *testing.T) {
	_, store := installSharedIDs(t)
	key := userIDCacheKey("sk-ant-XXXX-refresh")
	clusterValue := generateFakeUserID()
	if _, err := store.MintID(bg(), sharedIDNamespaceClaudeUser, key, clusterValue, userIDTTL, true); err != nil {
		t.Fatal(err)
	}
	localValue := generateFakeUserID()
	userIDCacheMu.Lock()
	userIDCache[key] = userIDCacheEntry{value: localValue, expire: time.Now().Add(time.Hour)}
	userIDCacheMu.Unlock()

	refreshSharedUserID(key, localValue)
	userIDCacheMu.RLock()
	got := userIDCache[key].value
	userIDCacheMu.RUnlock()
	if got != clusterValue {
		t.Fatalf("refresh must adopt the value the cluster already holds, got %q", got)
	}
}

func TestSharedIDsNotInstalledKeepLocalBehaviour(t *testing.T) {
	SetSharedIDMinter(nil)
	resetSyntheticIDCaches()
	t.Cleanup(resetSyntheticIDCaches)
	auth := sharedIDTestAuth()
	first := codexSyntheticSessionID(auth, "gpt-5.5", "seed")
	if first != codexSyntheticSessionID(auth, "gpt-5.5", "seed") {
		t.Fatal("per-process cache must keep one id per conversation")
	}
	codexCacheMu.RLock()
	shared := codexCacheMap[codexPromptCacheMapKey(auth, "gpt-5.5", "seed")].Shared
	codexCacheMu.RUnlock()
	if shared {
		t.Fatal("outside a cluster nothing is marked shared")
	}
}

func bg() context.Context { return context.Background() }
