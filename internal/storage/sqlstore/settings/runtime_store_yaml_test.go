package settings

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/configsync"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/runtimeconfig"
)

// One config.yaml save spells out several runtime settings. Storing them is
// all or nothing: a key the database refuses must not leave the others stored,
// or a save reported as failed would still have changed the service.
func TestPersistPresentInYAMLStoresAllOrNothing(t *testing.T) {
	store, _ := openStore(t)
	ctx := context.Background()
	debug, retry := runtimeconfig.RuntimeSettingDebug, runtimeconfig.RuntimeSettingRequestRetry
	if _, err := store.CompareAndSwap(ctx, []Change{{Key: debug, Value: false, Expected: 0}}); err != nil {
		t.Fatal(err)
	}
	yamlContent := []byte("port: 8318\ndebug: true\nrequest-retry: 5\n")
	// proxy-url is set in cfg but absent from the YAML, so it is not the save's
	// to write.
	cfg := &config.Config{Debug: true, RequestRetry: 5, SDKConfig: config.SDKConfig{ProxyURL: "http://not-in-yaml.example:8080"}}

	restore := configsync.SetPublisherForTest(func(_ context.Context, _ *sql.Tx, ev cluster.ConfigEvent) error {
		if ev.Key == retry {
			return errors.New("injected: write of request-retry failed")
		}
		return nil
	})
	keys, err := store.PersistPresentInYAML(ctx, cfg, yamlContent)
	restore()
	if err == nil || errors.Is(err, configsync.ErrVersionConflict) || keys != nil {
		t.Fatalf("persist with a refused key = %v, %v; want the write error", keys, err)
	}
	if entry, _ := store.Load(debug); string(entry.Payload) != "false" || entry.Version != 1 {
		t.Fatalf("debug after the failed save = %s at version %d, want false at 1", entry.Payload, entry.Version)
	}
	if _, ok := store.Load(retry); ok {
		t.Fatal("request-retry was stored by a failed save")
	}

	keys, err = store.PersistPresentInYAML(ctx, cfg, yamlContent)
	if err != nil || !slices.Equal(keys, []string{debug, retry}) {
		t.Fatalf("persist = %v, %v; want debug and request-retry", keys, err)
	}
	if entry, _ := store.Load(debug); string(entry.Payload) != "true" || entry.Version != 2 {
		t.Fatalf("debug = %s at version %d, want true at 2", entry.Payload, entry.Version)
	}
	if entry, _ := store.Load(retry); string(entry.Payload) != "5" || entry.Version != 1 {
		t.Fatalf("request-retry = %s at version %d, want 5 at 1", entry.Payload, entry.Version)
	}
	if _, ok := store.Load(runtimeconfig.RuntimeSettingProxyURL); ok {
		t.Fatal("proxy-url was stored although the YAML did not spell it out")
	}
	// The caller adopts the written keys with their versions, without reading
	// the database again.
	if state := cfg.RuntimeSettingState(); state.Version(debug) != 2 || state.Version(retry) != 1 {
		t.Fatalf("recorded versions = %v, want debug 2 and request-retry 1", state.Versions())
	}

	// Saving the same YAML again finds nothing to change.
	if keys, err = store.PersistPresentInYAML(ctx, cfg, yamlContent); err != nil || len(keys) != 0 {
		t.Fatalf("unchanged persist = %v, %v; want no writes", keys, err)
	}
	if entry, _ := store.Load(debug); entry.Version != 2 {
		t.Fatalf("debug version after an unchanged save = %d, want 2", entry.Version)
	}
}

// The save is not based on a version the client saw, so a key another writer
// changes in between is read again and the save retried. Only a key that keeps
// changing makes it give up, and then with the conflict.
func TestPersistPresentInYAMLRetriesConflicts(t *testing.T) {
	store, _ := openStore(t)
	ctx := context.Background()
	debug := runtimeconfig.RuntimeSettingDebug
	conflict := &configsync.ConflictError{Domain: configsync.RuntimeSettingDomain(debug), Key: debug, Expected: 0, Current: 1}
	attempts, conflicting := 0, 0
	restore := configsync.SetPublisherForTest(func(_ context.Context, _ *sql.Tx, ev cluster.ConfigEvent) error {
		attempts++
		if attempts <= conflicting {
			return conflict
		}
		return nil
	})
	t.Cleanup(restore)

	conflicting = 1
	keys, err := store.PersistPresentInYAML(ctx, &config.Config{Debug: true}, []byte("debug: true\n"))
	if err != nil || !slices.Equal(keys, []string{debug}) || attempts != 2 {
		t.Fatalf("persist after one conflict = %v, %v in %d attempts; want debug stored on the second", keys, err, attempts)
	}

	attempts, conflicting = 0, 1000
	keys, err = store.PersistPresentInYAML(ctx, &config.Config{Debug: false}, []byte("debug: false\n"))
	if !errors.Is(err, configsync.ErrVersionConflict) || keys != nil || attempts != 3 {
		t.Fatalf("persist against a key that keeps changing = %v, %v in %d attempts; want the conflict after 3", keys, err, attempts)
	}
	if entry, _ := store.Load(debug); string(entry.Payload) != "true" || entry.Version != 1 {
		t.Fatalf("debug after the abandoned save = %s at version %d, want true at 1", entry.Payload, entry.Version)
	}
}
