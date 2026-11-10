package settings

import (
	"context"
	"errors"
	"testing"
)

// The reads that tell a failed read from a missing row tell no database from
// one too; the ones that never did are unchanged.
func TestRuntimeSettingsReadsTellNoDatabaseFromNoRow(t *testing.T) {
	store := RuntimeSettingsStore{}
	ctx := context.Background()
	if _, ok, err := store.LoadContext(ctx, "secret-masking"); !errors.Is(err, ErrStoreUnavailable) || ok {
		t.Fatalf("LoadContext without a database: ok = %v, err = %v", ok, err)
	}
	if _, err := store.PayloadsByKeyContext(ctx, "secret-masking"); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("PayloadsByKeyContext without a database: err = %v", err)
	}
	if _, ok := store.Load("secret-masking"); ok {
		t.Fatal("Load without a database found a setting")
	}
	if payloads, err := store.PayloadsByKey("secret-masking"); payloads != nil || err != nil {
		t.Fatalf("PayloadsByKey without a database: %v, %v", payloads, err)
	}
}
