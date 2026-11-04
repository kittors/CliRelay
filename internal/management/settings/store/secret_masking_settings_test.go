package store

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestSecretMaskingAttachedStoreLosingDatabaseIsNotMissingRow(t *testing.T) {
	useDatabase(t)
	store := SecretMaskingSettings{}
	if settings, _, err := store.Load(context.Background(), "tenant-mask"); err != nil || settings.Enabled {
		t.Fatal("confirmed missing row must remain disabled")
	}
	registry := contentmoderation.NewSecretMaskingRegistry()
	registry.SetStore(store)
	if _, err := registry.SetEnabled(context.Background(), "tenant-mask", true); err != nil {
		t.Fatal(err)
	}
	usage.CloseDB()
	registry.Invalidate("tenant-mask")
	if settings, err := registry.For("tenant-mask"); err == nil || !settings.Enabled {
		t.Fatal("lost store must report an error while retaining last enabled policy")
	}
	fresh := contentmoderation.NewSecretMaskingRegistry()
	fresh.SetStore(store)
	if _, err := fresh.For("tenant-mask"); err == nil {
		t.Fatal("cold registry mistook missing DB for missing row")
	}
}
