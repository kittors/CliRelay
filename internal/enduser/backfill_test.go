package enduser

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const backfillTestTenant = "00000000-0000-0000-0000-000000000001"

// openBackfillTestDB adds the one table the backfill reads that the shared
// end-user fixture does not create. It mirrors the Postgres migration: a single
// row pinned to id 1, written when the migration completes.
func openBackfillTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openEndUserTestDB(t)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS end_user_backfill_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			done_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		t.Fatalf("create end_user_backfill_state: %v", err)
	}
	return db
}

func insertBackfillAPIKey(t *testing.T, db *sql.DB, secret, name string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO api_keys (key, id, name, tenant_id, created_at)
		VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z')
	`, secret, uuid.NewString(), name, backfillTestTenant); err != nil {
		t.Fatalf("insert api key %q: %v", name, err)
	}
}

func backfillStateRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM end_user_backfill_state WHERE id = 1`).Scan(&rows); err != nil {
		t.Fatalf("query backfill state: %v", err)
	}
	return rows
}

// The boot regression. BackfillFromAPIKeys only runs while
// end_user_backfill_state is empty, which on any deployment that has already
// started once is never — so a brand-new database was the single configuration
// nobody exercised. It hashed a hardcoded password through HashPassword, and
// once the password policy required 12 characters that 11-character constant
// was rejected: the backfill returned an error, initializeRuntimeDataStack
// turned it into a fatal, and the process exited without ever listening.
func TestBackfillOnFreshDatabaseWithAPIKeysBoots(t *testing.T) {
	t.Parallel()
	db := openBackfillTestDB(t)
	svc := NewService(db)
	insertBackfillAPIKey(t, db, "sk-alice", "Alice")
	insertBackfillAPIKey(t, db, "sk-bob", "Bob")

	created, err := svc.BackfillFromAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("BackfillFromAPIKeys on a fresh database with api keys: %v", err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2", created)
	}

	// Each migrated key must end up owned by an account and flagged as that
	// account's default, or the portal shows a user with no keys.
	for _, secret := range []string{"sk-alice", "sk-bob"} {
		var endUserID string
		var isDefault int
		if err := db.QueryRow(`SELECT COALESCE(end_user_id, ''), is_default FROM api_keys WHERE key = ?`, secret).Scan(&endUserID, &isDefault); err != nil {
			t.Fatalf("query migrated key %q: %v", secret, err)
		}
		if endUserID == "" {
			t.Fatalf("key %q was not bound to an end user", secret)
		}
		if isDefault != 1 {
			t.Fatalf("key %q is_default = %d, want 1", secret, isDefault)
		}
	}

	// The completion marker has to be written on the same pass, or the next
	// boot re-runs the migration and re-creates accounts an admin deleted.
	if rows := backfillStateRows(t, db); rows != 1 {
		t.Fatalf("end_user_backfill_state rows = %d, want 1", rows)
	}
}

// The same boot path with nothing to migrate. This one failed too: the hash of
// the hardcoded password was computed before the code looked at whether any key
// needed an account, so an empty database was killed by a migration that had no
// work to do.
func TestBackfillOnFreshDatabaseWithoutAPIKeysBoots(t *testing.T) {
	t.Parallel()
	db := openBackfillTestDB(t)
	svc := NewService(db)

	created, err := svc.BackfillFromAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("BackfillFromAPIKeys on an empty database: %v", err)
	}
	if created != 0 {
		t.Fatalf("created = %d, want 0 on a database with no api keys", created)
	}
	if rows := backfillStateRows(t, db); rows != 1 {
		t.Fatalf("end_user_backfill_state rows = %d, want 1", rows)
	}
}

// The security half of the same bug. The migration wrote a hash of the literal
// "password123" into every account it created and left must_change_password
// false, so the published source handed out a working password and the
// usernames to pair it with are derived from API key names. Lengthening the
// constant until it satisfies the policy would have fixed the boot failure and
// kept the hole, which is why this asserts on the property rather than on the
// specific string.
func TestBackfillAccountsHaveNoGuessablePassword(t *testing.T) {
	t.Parallel()
	db := openBackfillTestDB(t)
	svc := NewService(db)
	insertBackfillAPIKey(t, db, "sk-alice", "Alice")

	if _, err := svc.BackfillFromAPIKeys(context.Background()); err != nil {
		t.Fatalf("BackfillFromAPIKeys: %v", err)
	}

	var hash string
	var mustChange int
	if err := db.QueryRow(`SELECT password_hash, must_change_password FROM end_users`).Scan(&hash, &mustChange); err != nil {
		t.Fatalf("query migrated account: %v", err)
	}
	if hash == "" {
		t.Fatal("migrated account stored an empty password hash, which no login path rejects safely")
	}
	// The historical credential, plus the shapes a "just make it longer" fix
	// would have produced.
	for _, guess := range []string{legacyBackfillPassword, "password1234", "Password123!", "Password-1234!"} {
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(guess)); err == nil {
			t.Fatalf("a migrated account can be signed into with the hardcoded password %q", guess)
		}
	}
	if mustChange != 1 {
		t.Fatalf("must_change_password = %d, want 1: the account's password was never chosen by its owner", mustChange)
	}
}

// Re-running must be a no-op. The completion marker is the only thing standing
// between a restart and re-creating accounts an admin deleted or re-binding
// keys an admin moved.
func TestBackfillIsIdempotentAcrossRestarts(t *testing.T) {
	t.Parallel()
	db := openBackfillTestDB(t)
	svc := NewService(db)
	insertBackfillAPIKey(t, db, "sk-alice", "Alice")

	if created, err := svc.BackfillFromAPIKeys(context.Background()); err != nil || created != 1 {
		t.Fatalf("first BackfillFromAPIKeys = (%d, %v), want (1, nil)", created, err)
	}

	// A key created after the migration belongs to whoever created it, not to a
	// second round of backfill.
	insertBackfillAPIKey(t, db, "sk-carol", "Carol")

	created, err := svc.BackfillFromAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("second BackfillFromAPIKeys: %v", err)
	}
	if created != 0 {
		t.Fatalf("second run created = %d, want 0", created)
	}
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM end_users`).Scan(&users); err != nil {
		t.Fatalf("count end users: %v", err)
	}
	if users != 1 {
		t.Fatalf("end_users = %d after a second run, want 1", users)
	}
}
