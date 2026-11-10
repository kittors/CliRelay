package store

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// The runtime database going away is a failed read, not a tenant without a
// row: read as "no row", a tenant's masking was off until the next read.
func TestSecretMaskingStoreWithoutTheDatabaseIsNoRead(t *testing.T) {
	useDatabase(t)
	ctx := context.Background()
	store := SecretMaskingSettings{}
	if settings, _, err := store.Load(ctx, "tenant-mask"); err != nil || settings.Enabled {
		t.Fatalf("a tenant with no row: %+v, %v", settings, err)
	}
	registry := contentmoderation.NewSecretMaskingRegistry()
	registry.SetStore(store)
	if _, err := registry.SetEnabled(ctx, "tenant-mask", true); err != nil {
		t.Fatal(err)
	}
	usage.CloseDB()
	if _, _, err := store.Load(ctx, "tenant-mask"); !errors.Is(err, contentmoderation.ErrUnavailable) {
		t.Fatalf("Load without the database: %v", err)
	}
	if _, err := store.LoadAll(ctx); !errors.Is(err, contentmoderation.ErrUnavailable) {
		t.Fatalf("LoadAll without the database: %v", err)
	}
	// What was read stays; a registry that read nothing does not know.
	registry.Invalidate("tenant-mask")
	if settings, known := registry.Lookup("tenant-mask"); !known || !settings.Enabled {
		t.Fatalf("the registry that read the settings: %+v, known %v", settings, known)
	}
	fresh := contentmoderation.NewSecretMaskingRegistry()
	fresh.SetStore(store)
	if err := fresh.Preload(ctx); err == nil {
		t.Fatal("reading every tenant's settings without the database succeeded")
	}
	if _, known := fresh.Lookup("tenant-mask"); known {
		t.Fatal("a registry that read nothing takes the missing database for masking off")
	}
}
