package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	// The request log runs on SQLite when no PostgreSQL is configured.
	_ "modernc.org/sqlite"
)

func useDatabase(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "loop-guard-store.db"), proxyconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("usage.InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func toolRepeats(repeats int) loopguard.Settings {
	return loopguard.Settings{Scope: loopguard.Scope{Tool: loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: repeats}}}
}

func TestLoopGuardSettingsAreVersionedPerTenant(t *testing.T) {
	useDatabase(t)
	ctx := context.Background()
	store := LoopGuardSettings{}
	const tenantA, tenantB = "tenant-store-a", "tenant-store-b"

	stored, err := store.Load(ctx, tenantA)
	if err != nil || stored.Version != 0 || stored.Settings.Tool.Enabled != nil {
		t.Fatalf("a tenant with nothing stored: %+v, %v", stored, err)
	}

	if err := store.Save(ctx, tenantA, toolRepeats(6), 0); err != nil {
		t.Fatalf("first save: %v", err)
	}
	stored, err = store.Load(ctx, tenantA)
	if err != nil || stored.Version != 1 || stored.Settings.Tool.Enabled == nil || !*stored.Settings.Tool.Enabled || stored.Settings.Tool.Repeats != 6 {
		t.Fatalf("what was saved is not what is read: %+v, %v", stored, err)
	}

	// A write made on top of a row that has since changed is refused, whoever
	// changed it: a settings page opened before another admin's save must not
	// overwrite it.
	if err := store.Save(ctx, tenantA, toolRepeats(9), 0); !errors.Is(err, loopguard.ErrConflict) {
		t.Fatalf("a save at a stale version = %v, want ErrConflict", err)
	}
	if stored, _ = store.Load(ctx, tenantA); stored.Settings.Tool.Repeats != 6 || stored.Version != 1 {
		t.Fatalf("a refused save changed the row: %+v", stored)
	}
	if err := store.Save(ctx, tenantA, toolRepeats(9), stored.Version); err != nil {
		t.Fatalf("a save at the current version: %v", err)
	}
	if stored, _ = store.Load(ctx, tenantA); stored.Settings.Tool.Repeats != 9 || stored.Version != 2 {
		t.Fatalf("second save: %+v", stored)
	}

	// Another tenant's row is its own.
	if stored, err = store.Load(ctx, tenantB); err != nil || stored.Version != 0 || stored.Settings.Tool.Enabled != nil {
		t.Fatalf("a tenant picked up another's settings: %+v, %v", stored, err)
	}
}

func TestLoopGuardSettingsNeedADatabaseToBeSaved(t *testing.T) {
	usage.CloseDB()
	store := LoopGuardSettings{}
	if err := store.Save(context.Background(), "tenant-store-a", toolRepeats(6), 0); !errors.Is(err, loopguard.ErrNotPersistent) {
		t.Fatalf("Save without a database = %v, want ErrNotPersistent", err)
	}
	if stored, err := store.Load(context.Background(), "tenant-store-a"); err != nil || stored.Version != 0 {
		t.Fatalf("Load without a database = %+v, %v", stored, err)
	}
}

// A row this build cannot read is an error, not an empty row: reading it as
// nothing would let the next save overwrite what an administrator configured.
func TestLoopGuardSettingsRefuseARowThatCannotBeDecoded(t *testing.T) {
	useDatabase(t)
	const tenant = "tenant-store-corrupt"
	if _, err := usage.CompareAndSwapRuntimeSetting(context.Background(), tenant, loopguard.SettingKey, "not an object", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := (LoopGuardSettings{}).Load(context.Background(), tenant); err == nil {
		t.Fatal("a row that is no settings object was read as settings")
	}
}

func TestAttachLoopGuardSettingsNeedsADatabaseAndRunsOnce(t *testing.T) {
	registry := loopguard.Default()
	registry.SetStore(nil)
	t.Cleanup(func() { registry.SetStore(nil) })

	usage.CloseDB()
	AttachLoopGuardSettings()
	if registry.Persistent() {
		t.Fatal("a store was attached with no database")
	}

	useDatabase(t)
	AttachLoopGuardSettings()
	if !registry.Persistent() {
		t.Fatal("no store was attached with a database")
	}
	// A second call leaves the attached store, and what it has cached, alone.
	if _, err := registry.Update(context.Background(), "tenant-store-attach", func(s *loopguard.Settings) error {
		s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	AttachLoopGuardSettings()
	if registry.For("tenant-store-attach").Tool.Enabled == nil {
		t.Fatal("attaching again dropped the cached settings")
	}
}
