package accessfreeze

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const testTenant = "00000000-0000-0000-0000-000000000001"

// openFreezePostgres opens a schema of its own on the test server and applies
// the real access freeze migration to it.
func openFreezePostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	admin, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("access_freeze_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	scoped, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	db, err := sql.Open(compatdriver.DriverName, scoped.String())
	if err != nil {
		t.Fatalf("open scoped postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applied := false
	for _, migration := range postgres.RuntimeMigrations() {
		if migration.Version == "202611130001_access_freezes" {
			if _, err = db.Exec(migration.SQL); err != nil {
				t.Fatalf("apply access freeze migration: %v", err)
			}
			applied = true
		}
	}
	if !applied {
		t.Fatal("access freeze migration not found in RuntimeMigrations")
	}
	return db
}

func testStrike(subject Subject, provider string, at time.Time) Strike {
	return Strike{
		Subject:         subject,
		Provider:        provider,
		ReasonCode:      "cyber_policy",
		UpstreamMessage: "This content was flagged for possible cybersecurity risk.",
		Model:           "gpt-6.1-sol",
		AuthLabel:       "pool@example.com",
		RequestID:       "req-1",
		CreatedAt:       at,
	}
}

func TestStorePostgresFreezeLifecycle(t *testing.T) {
	db := openFreezePostgres(t)
	store := NewStore(db)
	ctx := context.Background()
	alice := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "alice"}
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	if threshold, err := store.Threshold(ctx, testTenant); err != nil || threshold != DefaultThreshold {
		t.Fatalf("Threshold = %d, %v; want the default %d", threshold, err, DefaultThreshold)
	}
	count, err := store.RecordStrike(ctx, testStrike(alice, "Codex", t0))
	if err != nil || count != 1 {
		t.Fatalf("RecordStrike = %d, %v; want 1", count, err)
	}
	first, created, err := store.Freeze(ctx, testStrike(alice, "codex", t0), count, t0)
	if err != nil || !created || first.Provider != "codex" || first.StrikeCount != 1 || !first.Active() || first.AuthLabel != "pool@example.com" {
		t.Fatalf("Freeze = %#v, created %v, err %v", first, created, err)
	}
	// A refusal of a second request in flight must not freeze alice twice.
	again, created, err := store.Freeze(ctx, testStrike(alice, "codex", t0), 1, t0)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second Freeze = %#v, created %v, err %v; want the standing freeze", again, created, err)
	}
	standing, ok, err := store.CountOnStanding(ctx, alice, "codex")
	if err != nil || !ok || standing.StrikeCount != 2 {
		t.Fatalf("CountOnStanding = %#v, %v, %v; want strike_count 2", standing, ok, err)
	}
	if _, ok, err = store.CountOnStanding(ctx, alice, "claude"); err != nil || ok {
		t.Fatalf("CountOnStanding(claude) = %v, %v; want no standing freeze", ok, err)
	}
	active, err := store.ListActive(ctx)
	if err != nil || len(active) != 1 || active[0].ID != first.ID {
		t.Fatalf("ListActive = %#v, %v", active, err)
	}

	if _, err = store.Release(ctx, "00000000-0000-0000-0000-000000000002", first.ID, "admin", "", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release from another tenant = %v, want ErrNotFound", err)
	}
	released, err := store.Release(ctx, testTenant, first.ID, "admin", "checked the request", t0.Add(time.Hour))
	if err != nil || released.Active() || released.ReleasedBy != "admin" || released.ReleaseNote != "checked the request" {
		t.Fatalf("Release = %#v, %v", released, err)
	}
	if _, err = store.Release(ctx, testTenant, first.ID, "admin", "", t0); !errors.Is(err, ErrAlreadyReleased) {
		t.Fatalf("second Release = %v, want ErrAlreadyReleased", err)
	}
	if _, err = store.Release(ctx, testTenant, first.ID+100, "admin", "", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release of an unknown id = %v, want ErrNotFound", err)
	}

	// The count starts over after a release.
	if count, err = store.RecordStrike(ctx, testStrike(alice, "codex", t0.Add(2*time.Hour))); err != nil || count != 1 {
		t.Fatalf("RecordStrike after release = %d, %v; want 1", count, err)
	}
	if list, err := store.List(ctx, testTenant, Filter{Status: "released"}); err != nil || len(list) != 1 {
		t.Fatalf("List(released) = %d, %v", len(list), err)
	}
	if list, err := store.List(ctx, testTenant, Filter{Status: "active", SubjectKind: SubjectEndUser, SubjectID: "alice"}); err != nil || len(list) != 0 {
		t.Fatalf("List(active) = %d, %v", len(list), err)
	}
	strikes, err := store.ListStrikes(ctx, alice, "codex", 10)
	if err != nil || len(strikes) != 2 || !strikes[0].CreatedAt.After(strikes[1].CreatedAt) {
		t.Fatalf("ListStrikes = %#v, %v; want two, newest first", strikes, err)
	}

	if err = store.SetThreshold(ctx, testTenant, 3, "admin", t0); err != nil {
		t.Fatalf("SetThreshold: %v", err)
	}
	if err = store.SetThreshold(ctx, testTenant, 0, "admin", t0); err != nil {
		t.Fatalf("SetThreshold(0): %v", err)
	}
	if threshold, err := store.Threshold(ctx, testTenant); err != nil || threshold != 0 {
		t.Fatalf("Threshold = %d, %v; want 0", threshold, err)
	}
}

func TestServicePostgresFreezesAtTheThresholdAndReleases(t *testing.T) {
	db := openFreezePostgres(t)
	service := NewService(NewStore(db), testZone)
	ctx := context.Background()
	bob := Subject{TenantID: testTenant, Kind: SubjectEndUser, ID: "bob"}
	if err := service.SetThreshold(ctx, testTenant, 2, "admin"); err != nil {
		t.Fatalf("SetThreshold: %v", err)
	}
	refusal := func() {
		service.handle(refusalJob{subject: bob, at: time.Now(), requestID: "req", event: coreauth.UpstreamRefusalEvent{
			AuthID: "codex-a", AuthLabel: "pool@example.com", Provider: "codex", Model: "gpt-6.1-sol",
			Refusal: coreauth.UpstreamContentRefusal{Code: "cyber_policy", Message: "flagged"},
		}})
	}

	refusal()
	if got := service.FrozenProviders(testTenant, "bob", "key-1"); got != "" {
		t.Fatalf("frozen after one refusal with threshold 2: %q", got)
	}
	refusal()
	raw := service.FrozenProviders(testTenant, "bob", "key-1")
	var notices map[string]string
	if err := json.Unmarshal([]byte(raw), &notices); err != nil || !strings.Contains(notices["codex"], "Codex") {
		t.Fatalf("FrozenProviders = %q (%v), want a codex notice", raw, err)
	}
	// Another of bob's keys is held back too: the freeze is on the account.
	if service.FrozenProviders(testTenant, "bob", "key-2") != raw {
		t.Fatal("the freeze did not cover bob's other key")
	}
	// A key with no portal account is a subject of its own.
	if got := service.FrozenProviders(testTenant, "", "key-1"); got != "" {
		t.Fatalf("key-1 alone frozen: %q", got)
	}

	standing := service.Standing(bob)
	if len(standing) != 1 || standing[0].StrikeCount != 2 {
		t.Fatalf("Standing = %#v", standing)
	}
	refusal() // a request that was in flight when the freeze came
	if got := service.Standing(bob); len(got) != 1 || got[0].StrikeCount != 3 {
		t.Fatalf("Standing after a late refusal = %#v", got)
	}

	if _, err := service.Release(ctx, testTenant, standing[0].ID, "admin", ""); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := service.FrozenProviders(testTenant, "bob", "key-1"); got != "" {
		t.Fatalf("still frozen after release: %q", got)
	}

	// A second process learns of freezes from the database.
	peer := NewService(NewStore(db), testZone)
	refusal()
	refusal()
	peer.refresh(ctx)
	if peer.FrozenProviders(testTenant, "bob", "key-1") == "" {
		t.Fatal("peer did not pick up the freeze on refresh")
	}
}
