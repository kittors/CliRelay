package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// The Postgres store mirrors auth records to disk and back into the database.
// Both copies have to carry the disabled flag, otherwise a management toggle is
// lost on the next process start (or blue/green cutover).
func TestPostgresStoreRoundTripsDisabledFlag(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	schema := "auth_disabled_test_" + time.Now().UTC().Format("20060102150405")
	store, err := NewPostgresStore(ctx, PostgresStoreConfig{DSN: dsn, Schema: schema, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`) }()
	if err = store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	const tenant = "00000000-0000-0000-0000-000000000001"
	authID := tenant + "/codex-toggle.json"
	path := filepath.Join(store.AuthDir(), tenant, "codex-toggle.json")
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		t.Fatalf("MkdirAll: %v", errMkdir)
	}
	metadata := map[string]any{
		"type":          "codex",
		"email":         "toggle@example.com",
		"access_token":  "at",
		"refresh_token": "rt",
		"disabled":      false,
	}
	seed, _ := json.Marshal(metadata)
	if errWrite := os.WriteFile(path, seed, 0o600); errWrite != nil {
		t.Fatalf("WriteFile: %v", errWrite)
	}

	auth := &cliproxyauth.Auth{
		ID:         authID,
		TenantID:   tenant,
		Provider:   "codex",
		FileName:   authID,
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"path": path},
		Metadata:   metadata,
	}
	if _, errSave := store.Save(ctx, auth); errSave != nil {
		t.Fatalf("initial Save: %v", errSave)
	}

	for _, step := range []struct {
		name     string
		disabled bool
		status   cliproxyauth.Status
	}{
		{name: "disable", disabled: true, status: cliproxyauth.StatusDisabled},
		{name: "re-enable", disabled: false, status: cliproxyauth.StatusActive},
	} {
		t.Run(step.name, func(t *testing.T) {
			auth.Disabled = step.disabled
			auth.Status = step.status
			if _, errSave := store.Save(ctx, auth); errSave != nil {
				t.Fatalf("Save: %v", errSave)
			}

			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatalf("ReadFile: %v", errRead)
			}
			onDisk := map[string]any{}
			if errUnmarshal := json.Unmarshal(raw, &onDisk); errUnmarshal != nil {
				t.Fatalf("Unmarshal: %v", errUnmarshal)
			}
			if onDisk["disabled"] != step.disabled {
				t.Fatalf("on-disk disabled = %#v, want %v", onDisk["disabled"], step.disabled)
			}

			var reloaded *cliproxyauth.Auth
			auths, errList := store.List(ctx)
			if errList != nil {
				t.Fatalf("List: %v", errList)
			}
			for _, candidate := range auths {
				if candidate != nil && candidate.ID == authID {
					reloaded = candidate
					break
				}
			}
			if reloaded == nil {
				t.Fatalf("auth %s missing after reload", authID)
			}
			if reloaded.Disabled != step.disabled || reloaded.Status != step.status {
				t.Fatalf("reloaded Disabled=%v Status=%q, want %v/%q",
					reloaded.Disabled, reloaded.Status, step.disabled, step.status)
			}
		})
	}
}
