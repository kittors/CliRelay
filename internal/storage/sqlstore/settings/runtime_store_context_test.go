package settings

import (
	"context"
	"testing"
)

func TestRuntimeSettingsLoadContextDistinguishesUnavailableDatabase(t *testing.T) {
	store := RuntimeSettingsStore{}
	if _, ok, err := store.LoadContext(context.Background(), "secret-masking"); err == nil || ok {
		t.Fatal("unavailable database was reported as a missing row")
	}
	if _, ok := store.Load("secret-masking"); ok {
		t.Fatal("legacy Load behavior changed")
	}
}
