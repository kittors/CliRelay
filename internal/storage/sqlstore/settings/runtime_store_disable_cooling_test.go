package settings

import (
	"context"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
)

// The panel's global switch is saved by writing disable-cooling into the
// config.yaml it PUTs; the save has to store it like the retry settings.
func TestPersistPresentInYAMLStoresDisableCooling(t *testing.T) {
	store, _ := openStore(t)
	ctx := context.Background()
	key := runtimeconfig.RuntimeSettingDisableCooling

	keys, err := store.PersistPresentInYAML(ctx, &config.Config{DisableCooling: true}, []byte("disable-cooling: true\n"))
	if err != nil || !slices.Equal(keys, []string{key}) {
		t.Fatalf("persist = %v, %v; want disable-cooling", keys, err)
	}
	if entry, _ := store.Load(key); string(entry.Payload) != "true" {
		t.Fatalf("stored disable-cooling = %s, want true", entry.Payload)
	}

	// Switching it back off is written too: false must not read as "absent".
	keys, err = store.PersistPresentInYAML(ctx, &config.Config{DisableCooling: false}, []byte("disable-cooling: false\n"))
	if err != nil || !slices.Equal(keys, []string{key}) {
		t.Fatalf("persist false = %v, %v; want disable-cooling", keys, err)
	}
	if entry, _ := store.Load(key); string(entry.Payload) != "false" {
		t.Fatalf("stored disable-cooling = %s, want false", entry.Payload)
	}
}
