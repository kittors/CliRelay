package modeldiscovery

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

func snapshotPathIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "provider-model-lists.json")
}

// restart drops everything held in memory, as a new process would, and restores
// what was saved.
func restart(t *testing.T, path string) int {
	t.Helper()
	ResetForTest()
	SetSnapshotPath(path)
	restored, err := LoadSnapshot()
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	return restored
}

// The incident: a restart emptied the store while the fetch could not reach the
// upstream, and every model only the Codex manifest lists stopped routing.
func TestAListSurvivesARestart(t *testing.T) {
	resetStore(t)
	clock := useClock(t)
	path := snapshotPathIn(t)
	SetSnapshotPath(path)

	Store("tenant-a", "codex", models("gpt-6.1-sol", "gpt-6-luna"))

	if restored := restart(t, path); restored != 1 {
		t.Fatalf("restored %d lists, want 1", restored)
	}
	if got := ids(Snapshot("tenant-a", "codex")); !slices.Equal(got, []string{"gpt-6.1-sol", "gpt-6-luna"}) {
		t.Fatalf("Snapshot after restart = %v", got)
	}
	if !Listed("tenant-a", "codex", "gpt-6.1-sol") {
		t.Fatal("a restored model must stay listed")
	}

	// The saved fetch time is kept: the list is asked for again on schedule, and
	// keeps routing until an answer replaces it.
	if Fresh("tenant-a", "codex") == nil {
		t.Fatal("a list restored within FreshFor should still be fresh")
	}
	clock.advance(FreshFor + time.Minute)
	if Fresh("tenant-a", "codex") != nil {
		t.Fatal("a restored list must age from its original fetch time")
	}
	if len(Snapshot("tenant-a", "codex")) == 0 {
		t.Fatal("an old list must keep routing until a newer answer replaces it")
	}
}

func TestLoadSnapshotKeepsAListThisProcessFetched(t *testing.T) {
	resetStore(t)
	path := snapshotPathIn(t)
	SetSnapshotPath(path)
	Store("tenant-a", "codex", models("gpt-old"))

	ResetForTest()
	Store("tenant-a", "codex", models("gpt-new")) // no path: memory only
	SetSnapshotPath(path)
	restored, err := LoadSnapshot()
	if err != nil || restored != 0 {
		t.Fatalf("LoadSnapshot = %d, %v; want nothing restored over a live list", restored, err)
	}
	if got := ids(Snapshot("tenant-a", "codex")); !slices.Equal(got, []string{"gpt-new"}) {
		t.Fatalf("Snapshot = %v, the list fetched by this process must win", got)
	}
}

func TestLoadSnapshotWithoutAFile(t *testing.T) {
	resetStore(t)
	if restored := restart(t, snapshotPathIn(t)); restored != 0 {
		t.Fatalf("restored %d lists from a missing file", restored)
	}
}

func TestLoadSnapshotRejectsAnotherVersion(t *testing.T) {
	resetStore(t)
	path := snapshotPathIn(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"lists":{"t|codex":{"tenant_id":"t","provider":"codex","models":[{"id":"x"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	SetSnapshotPath(path)
	if restored, err := LoadSnapshot(); err == nil || restored != 0 {
		t.Fatalf("LoadSnapshot = %d, %v; want an error and nothing restored", restored, err)
	}
}

// A list fetch has to use the proxy the credential's tenant assigns, as its chat
// requests do. With the system configuration, a tenant credential's proxy ID
// resolved to the system tenant's pool entry, which still pointed at an endpoint
// the provider had retired.
func TestWarmFetchesWithTheCredentialTenantConfig(t *testing.T) {
	resetStore(t)
	base := &config.Config{}
	base.ProxyURL = "socks5://system.invalid:1"
	previous := tenantRuntimeConfig
	tenantRuntimeConfig = func(cfg *config.Config, tenantID string) config.Config {
		tenantCfg := *cfg
		tenantCfg.ProxyURL = "socks5://" + tenantID + ".invalid:1"
		return tenantCfg
	}
	t.Cleanup(func() { tenantRuntimeConfig = previous })

	var seen []string
	useFetcher(t, "codex", func(_ context.Context, _ *coreauth.Auth, cfg *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		seen = append(seen, cfg.ProxyURL)
		return models("gpt-6.1-sol"), nil
	})

	systemTenant := coreauth.NormalizedTenantID("")
	for _, tenantID := range []string{"tenant-a", systemTenant} {
		if _, ok := Warm(context.Background(), codexAuth("codex-"+tenantID, tenantID), base, tenantID, "codex", true); !ok {
			t.Fatalf("Warm for %s failed", tenantID)
		}
	}
	want := []string{"socks5://tenant-a.invalid:1", "socks5://system.invalid:1"}
	if !slices.Equal(seen, want) {
		t.Fatalf("fetch proxies = %v, want %v", seen, want)
	}
}
