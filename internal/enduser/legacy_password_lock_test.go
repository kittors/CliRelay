package enduser

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"golang.org/x/crypto/bcrypt"
)

const legacyLockTestTenant = "00000000-0000-0000-0000-000000000001"

// When the pre-#1040 backfill ran on the fixtures that it ran on. It wrote its
// accounts and its completion row in one transaction, so this is both done_at
// and the created_at (and password_changed_at) of every account it created.
const legacyLockCreatedAt = "2026-07-20 10:00:00"

// A month after the backfill: accounts created by anything else.
const legacyLockLaterAt = "2026-08-20 10:00:00"

// openLegacyPasswordLockTestDB adds the tables the lock touches that the shared
// end-user fixture does not create, in their Postgres shape. The backfill has
// not run on it; openLegacyBackfilledTestDB is one it has.
func openLegacyPasswordLockTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openBackfillTestDB(t)
	for _, stmt := range []string{`
		CREATE TABLE IF NOT EXISTS end_user_sessions (
			id TEXT PRIMARY KEY,
			end_user_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			access_token_hash TEXT NOT NULL UNIQUE,
			refresh_token_hash TEXT NOT NULL UNIQUE,
			access_expires_at DATETIME NOT NULL,
			refresh_expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			revoked_at DATETIME,
			revoke_reason TEXT NOT NULL DEFAULT '',
			user_agent_hash TEXT NOT NULL DEFAULT ''
		)`, `
		CREATE TABLE IF NOT EXISTS end_user_legacy_password_lock_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			done_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			locked_count INTEGER NOT NULL DEFAULT 0
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create legacy lock fixture table: %v", err)
		}
	}
	return db
}

// openLegacyBackfilledTestDB is a database the pre-#1040 backfill completed on
// at legacyLockCreatedAt.
func openLegacyBackfilledTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openLegacyPasswordLockTestDB(t)
	if _, err := db.Exec(`INSERT INTO end_user_backfill_state (id, done_at) VALUES (1, ?)`, legacyLockCreatedAt); err != nil {
		t.Fatalf("record backfill run: %v", err)
	}
	return db
}

func legacyLockBatchTime(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.DateTime, legacyLockCreatedAt)
	if err != nil {
		t.Fatalf("parse batch time: %v", err)
	}
	return at
}

// countLegacyPasswordComparisons counts the bcrypt comparisons the pass makes
// for the rest of the test. The comparison is a package variable, so a test
// using this must not call t.Parallel.
func countLegacyPasswordComparisons(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	compare := holdsLegacyBackfillPassword
	holdsLegacyBackfillPassword = func(hash string) bool {
		calls.Add(1)
		return compare(hash)
	}
	t.Cleanup(func() { holdsLegacyBackfillPassword = compare })
	return &calls
}

func bcryptForTest(t *testing.T, password string) string {
	t.Helper()
	return bcryptAtCost(t, password, bcrypt.DefaultCost)
}

func bcryptAtCost(t *testing.T, password string, cost int) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(hash)
}

type legacyLockSeed struct {
	username          string
	passwordHash      string
	mustChange        bool
	createdAt         string // defaults to the backfill run
	passwordChangedAt string // defaults to createdAt
}

func insertLegacyLockAccount(t *testing.T, db *sql.DB, seed legacyLockSeed) string {
	t.Helper()
	createdAt := seed.createdAt
	if createdAt == "" {
		createdAt = legacyLockCreatedAt
	}
	changedAt := seed.passwordChangedAt
	if changedAt == "" {
		changedAt = createdAt
	}
	id := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash,
			must_change_password, created_at, updated_at, password_changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, legacyLockTestTenant, seed.username, seed.username, seed.username, seed.passwordHash,
		seed.mustChange, createdAt, createdAt, changedAt); err != nil {
		t.Fatalf("insert account %q: %v", seed.username, err)
	}
	return id
}

// insertLegacyLockSession opens a portal session for the account; a non-empty
// revokeReason stores it as already ended for that reason.
func insertLegacyLockSession(t *testing.T, db *sql.DB, userID, revokeReason string) string {
	t.Helper()
	id := uuid.NewString()
	var revokedAt any
	if revokeReason != "" {
		revokedAt = legacyLockCreatedAt
	}
	if _, err := db.Exec(`
		INSERT INTO end_user_sessions (id, end_user_id, tenant_id, access_token_hash, refresh_token_hash,
			access_expires_at, refresh_expires_at, revoked_at, revoke_reason)
		VALUES (?, ?, ?, ?, ?, '2099-01-01 00:00:00', '2099-01-01 00:00:00', ?, ?)
	`, id, userID, legacyLockTestTenant, "a-"+id, "r-"+id, revokedAt, revokeReason); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

type legacyLockSessionState struct {
	revoked bool
	reason  string
}

func readLegacyLockSession(t *testing.T, db *sql.DB, id string) legacyLockSessionState {
	t.Helper()
	var revokedAt sql.NullTime
	var s legacyLockSessionState
	if err := db.QueryRow(`SELECT revoked_at, revoke_reason FROM end_user_sessions WHERE id = ?`, id).Scan(&revokedAt, &s.reason); err != nil {
		t.Fatalf("read session: %v", err)
	}
	s.revoked = revokedAt.Valid
	return s
}

// legacyLockAccountState holds times as text so two reads compare with ==.
type legacyLockAccountState struct {
	passwordHash      string
	mustChange        bool
	status            string
	version           int64
	updatedAt         string
	passwordChangedAt string
}

func readLegacyLockAccount(t *testing.T, db *sql.DB, id string) legacyLockAccountState {
	t.Helper()
	var s legacyLockAccountState
	var updatedAt, changedAt time.Time
	if err := db.QueryRow(`
		SELECT password_hash, must_change_password, status, version, updated_at, password_changed_at
		FROM end_users WHERE id = ?
	`, id).Scan(&s.passwordHash, &s.mustChange, &s.status, &s.version, &updatedAt, &changedAt); err != nil {
		t.Fatalf("read account: %v", err)
	}
	s.updatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	s.passwordChangedAt = changedAt.UTC().Format(time.RFC3339Nano)
	return s
}

func legacyLockMarker(t *testing.T, db *sql.DB) (rows, lockedCount int) {
	t.Helper()
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(locked_count), 0) FROM end_user_legacy_password_lock_state`).Scan(&rows, &lockedCount); err != nil {
		t.Fatalf("read lock marker: %v", err)
	}
	return rows, lockedCount
}

// Accounts created by the pre-#1040 backfill still hold its published password
// with must_change_password off. The pass must take exactly the ones in that
// batch whose owner never replaced the password, end their sessions, and leave
// every other account and every API key as it was.
func TestLegacyBackfillPasswordLockTakesOnlyUntouchedBackfillAccounts(t *testing.T) {
	t.Parallel()
	db := openLegacyBackfilledTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	// One backfill run hashed once and gave every account that same string.
	legacyHash := bcryptForTest(t, legacyBackfillPassword)

	alice := insertLegacyLockAccount(t, db, legacyLockSeed{username: "alice", passwordHash: legacyHash})
	bob := insertLegacyLockAccount(t, db, legacyLockSeed{username: "bob", passwordHash: legacyHash})
	// Each control fails exactly one rule. The owner set this one a month later
	// (the pre-policy portal allowed it): a choice, not the backfill's credential.
	changed := insertLegacyLockAccount(t, db, legacyLockSeed{username: "carol", passwordHash: legacyHash, passwordChangedAt: legacyLockLaterAt})
	// Already flagged to change, as an admin reset or the current backfill leaves it.
	flagged := insertLegacyLockAccount(t, db, legacyLockSeed{username: "dave", passwordHash: legacyHash, mustChange: true})
	// In the batch and never changed, but not the published password.
	other := insertLegacyLockAccount(t, db, legacyLockSeed{username: "erin", passwordHash: bcryptForTest(t, "Another-Password-123")})
	// Not in the batch: the same string set by hand later, under the pre-v0.5.2
	// 8-character policy. A weak password, but not the one the backfill published.
	outsider := insertLegacyLockAccount(t, db, legacyLockSeed{username: "oscar", passwordHash: bcryptForTest(t, legacyBackfillPassword), createdAt: legacyLockLaterAt})

	key, err := svc.CreateKey(ctx, legacyLockTestTenant, alice, "alice-key")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	liveSession := insertLegacyLockSession(t, db, alice, "")
	endedSession := insertLegacyLockSession(t, db, alice, "logout")
	controlSession := insertLegacyLockSession(t, db, changed, "")

	before := make(map[string]legacyLockAccountState)
	for _, id := range []string{alice, bob, changed, flagged, other, outsider} {
		before[id] = readLegacyLockAccount(t, db, id)
	}

	locked, err := svc.LockLegacyBackfillPasswordAccounts(ctx)
	if err != nil {
		t.Fatalf("LockLegacyBackfillPasswordAccounts: %v", err)
	}

	for _, id := range []string{alice, bob} {
		after := readLegacyLockAccount(t, db, id)
		if bcrypt.CompareHashAndPassword([]byte(after.passwordHash), []byte(legacyBackfillPassword)) == nil {
			t.Fatalf("account %s still accepts the legacy backfill password after the lock pass", id)
		}
		if after.passwordHash == "" {
			t.Fatalf("account %s was locked with an empty hash", id)
		}
		if !after.mustChange {
			t.Fatalf("account %s must_change_password = false, want true: its owner never chose a password", id)
		}
		if after.version != before[id].version+1 {
			t.Fatalf("account %s version = %d, want %d", id, after.version, before[id].version+1)
		}
		if after.updatedAt == before[id].updatedAt {
			t.Fatalf("account %s updated_at did not move", id)
		}
		// Nobody chose a new password, so the column keeps saying so.
		if after.passwordChangedAt != before[id].passwordChangedAt {
			t.Fatalf("account %s password_changed_at moved %s -> %s", id, before[id].passwordChangedAt, after.passwordChangedAt)
		}
		// Status is what API key admission reads; it has to stay put.
		if after.status != "active" {
			t.Fatalf("account %s status = %q, want active", id, after.status)
		}
	}
	if want := []string{"alice", "bob"}; !slices.Equal(locked, want) {
		t.Fatalf("locked = %v, want %v", locked, want)
	}
	for _, id := range []string{changed, flagged, other, outsider} {
		if after := readLegacyLockAccount(t, db, id); after != before[id] {
			t.Fatalf("control account %s changed:\n before %+v\n after  %+v", id, before[id], after)
		}
	}

	if got := readLegacyLockSession(t, db, liveSession); !got.revoked || got.reason != "legacy_password_lock" {
		t.Fatalf("live session of a locked account = %+v, want revoked for legacy_password_lock", got)
	}
	if got := readLegacyLockSession(t, db, endedSession); got.reason != "logout" {
		t.Fatalf("an already-ended session was re-labelled: %+v", got)
	}
	if got := readLegacyLockSession(t, db, controlSession); got.revoked {
		t.Fatalf("a control account's session was revoked: %+v", got)
	}

	// Key admission requires an enabled key and an active owner; the pass
	// touches neither, so business traffic through the key is unaffected.
	var secret, owner string
	var disabled int
	if err := db.QueryRow(`SELECT key, end_user_id, disabled FROM api_keys WHERE id = ?`, key.APIKey.ID).Scan(&secret, &owner, &disabled); err != nil {
		t.Fatalf("read key: %v", err)
	}
	if secret != key.PlaintextKey || owner != alice || disabled != 0 {
		t.Fatalf("key after lock = (%q, %q, disabled=%d), want (%q, %q, disabled=0)", secret, owner, disabled, key.PlaintextKey, alice)
	}

	if rows, count := legacyLockMarker(t, db); rows != 1 || count != 2 {
		t.Fatalf("lock marker = (%d rows, locked_count %d), want (1, 2)", rows, count)
	}

	// The way back in is the existing admin reset.
	generated, err := svc.ResetPassword(ctx, identity.Principal{PlatformAdmin: true}, legacyLockTestTenant, alice, "")
	if err != nil {
		t.Fatalf("ResetPassword on a locked account: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(readLegacyLockAccount(t, db, alice).passwordHash), []byte(generated)); err != nil {
		t.Fatalf("the credential an admin issued does not verify: %v", err)
	}
}

// The pass runs before the server listens, inside a transaction that holds the
// advisory lock, so every bcrypt comparison is boot time. Accounts outside the
// backfill batch — however many, each with its own salt, even ones that happen
// to hold the same string — must not cost one. The batch shares one hash, so
// the whole pass costs one comparison.
func TestLegacyBackfillPasswordLockComparesOnlyTheBackfillBatch(t *testing.T) {
	db := openLegacyBackfilledTestDB(t)
	svc := NewService(db)
	calls := countLegacyPasswordComparisons(t)
	legacyHash := bcryptForTest(t, legacyBackfillPassword)

	insertLegacyLockAccount(t, db, legacyLockSeed{username: "batch_a", passwordHash: legacyHash})
	insertLegacyLockAccount(t, db, legacyLockSeed{username: "batch_b", passwordHash: legacyHash})
	const outsiders = 200
	outsiderIDs := make([]string, 0, outsiders)
	for i := range outsiders {
		password := fmt.Sprintf("Outsider-Password-%03d", i)
		if i%10 == 0 {
			password = legacyBackfillPassword
		}
		outsiderIDs = append(outsiderIDs, insertLegacyLockAccount(t, db, legacyLockSeed{
			username:     fmt.Sprintf("outsider_%03d", i),
			passwordHash: bcryptAtCost(t, password, bcrypt.MinCost),
			createdAt:    legacyLockLaterAt,
		}))
	}
	before := make(map[string]legacyLockAccountState, outsiders)
	for _, id := range outsiderIDs {
		before[id] = readLegacyLockAccount(t, db, id)
	}

	locked, err := svc.LockLegacyBackfillPasswordAccounts(context.Background())
	if err != nil {
		t.Fatalf("LockLegacyBackfillPasswordAccounts: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("bcrypt comparisons = %d with %d accounts outside the batch, want 1", got, outsiders)
	}
	if want := []string{"batch_a", "batch_b"}; !slices.Equal(locked, want) {
		t.Fatalf("locked = %v, want %v", locked, want)
	}
	for _, id := range outsiderIDs {
		if after := readLegacyLockAccount(t, db, id); after != before[id] {
			t.Fatalf("account outside the batch changed:\n before %+v\n after  %+v", before[id], after)
		}
	}
}

// With no backfill run recorded, no account can be one the backfill created:
// the pass records itself and compares nothing, whatever the accounts hold.
func TestLegacyBackfillPasswordLockWithoutBackfillRunComparesNothing(t *testing.T) {
	db := openLegacyPasswordLockTestDB(t)
	svc := NewService(db)
	calls := countLegacyPasswordComparisons(t)
	id := insertLegacyLockAccount(t, db, legacyLockSeed{username: "judy", passwordHash: bcryptForTest(t, legacyBackfillPassword)})
	before := readLegacyLockAccount(t, db, id)

	locked, err := svc.LockLegacyBackfillPasswordAccounts(context.Background())
	if err != nil {
		t.Fatalf("LockLegacyBackfillPasswordAccounts: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("bcrypt comparisons = %d without a backfill run, want 0", got)
	}
	if len(locked) != 0 {
		t.Fatalf("locked = %v without a backfill run, want nothing", locked)
	}
	if after := readLegacyLockAccount(t, db, id); after != before {
		t.Fatalf("account changed without a backfill run:\n before %+v\n after  %+v", before, after)
	}
	if rows, count := legacyLockMarker(t, db); rows != 1 || count != 0 {
		t.Fatalf("lock marker = (%d rows, locked_count %d), want (1, 0)", rows, count)
	}
}

// A fresh install boots through both passes in their real order: the current
// backfill records its run, and the lock finds nothing of its batch to check.
func TestLegacyBackfillPasswordLockOnFreshDatabase(t *testing.T) {
	db := openLegacyPasswordLockTestDB(t)
	svc := NewService(db)
	calls := countLegacyPasswordComparisons(t)
	ctx := context.Background()

	if _, err := svc.BackfillFromAPIKeys(ctx); err != nil {
		t.Fatalf("BackfillFromAPIKeys on an empty database: %v", err)
	}
	locked, err := svc.LockLegacyBackfillPasswordAccounts(ctx)
	if err != nil {
		t.Fatalf("LockLegacyBackfillPasswordAccounts on an empty database: %v", err)
	}
	if len(locked) != 0 || calls.Load() != 0 {
		t.Fatalf("fresh database: locked %v with %d comparisons, want nothing and 0", locked, calls.Load())
	}
	if rows, count := legacyLockMarker(t, db); rows != 1 || count != 0 {
		t.Fatalf("lock marker = (%d rows, locked_count %d), want (1, 0)", rows, count)
	}
}

// One-shot: once completion is recorded, later boots leave the accounts alone —
// including one that looks exactly like a backfill account.
func TestLegacyBackfillPasswordLockRunsOnce(t *testing.T) {
	t.Parallel()
	db := openLegacyBackfilledTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	legacyHash := bcryptForTest(t, legacyBackfillPassword)

	first := insertLegacyLockAccount(t, db, legacyLockSeed{username: "frank", passwordHash: legacyHash})
	if locked, err := svc.LockLegacyBackfillPasswordAccounts(ctx); err != nil || !slices.Equal(locked, []string{"frank"}) {
		t.Fatalf("first run = (%v, %v), want ([frank], nil)", locked, err)
	}
	firstAfter := readLegacyLockAccount(t, db, first)

	late := insertLegacyLockAccount(t, db, legacyLockSeed{username: "grace", passwordHash: legacyHash})
	lateBefore := readLegacyLockAccount(t, db, late)

	locked, err := svc.LockLegacyBackfillPasswordAccounts(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(locked) != 0 {
		t.Fatalf("second run locked %v, want nothing", locked)
	}
	if got := readLegacyLockAccount(t, db, late); got != lateBefore {
		t.Fatalf("second run touched an account:\n before %+v\n after  %+v", lateBefore, got)
	}
	if got := readLegacyLockAccount(t, db, first); got != firstAfter {
		t.Fatalf("second run touched an already locked account:\n before %+v\n after  %+v", firstAfter, got)
	}
	if rows, count := legacyLockMarker(t, db); rows != 1 || count != 1 {
		t.Fatalf("lock marker = (%d rows, locked_count %d), want (1, 1)", rows, count)
	}
}

// Candidates are read before any account is written, and during a blue-green
// deploy the old slot keeps serving meanwhile. A password the owner replaced
// after the read must survive the pass; an untouched candidate is still locked.
func TestLegacyBackfillPasswordLockKeepsPasswordChangedAfterRead(t *testing.T) {
	t.Parallel()
	db := openLegacyBackfilledTestDB(t)
	ctx := context.Background()
	legacyHash := bcryptForTest(t, legacyBackfillPassword)

	moved := insertLegacyLockAccount(t, db, legacyLockSeed{username: "heidi", passwordHash: legacyHash})
	still := insertLegacyLockAccount(t, db, legacyLockSeed{username: "ivan", passwordHash: legacyHash})
	movedSession := insertLegacyLockSession(t, db, moved, "")

	readTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	candidates, err := loadLegacyPasswordCandidates(ctx, readTx, legacyLockBatchTime(t))
	_ = readTx.Rollback()
	if err != nil {
		t.Fatalf("loadLegacyPasswordCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v, want heidi and ivan", candidates)
	}

	ownerHash := bcryptForTest(t, "Owner-Chosen-Password-1")
	if _, err := db.Exec(`
		UPDATE end_users SET password_hash = ?, password_changed_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE id = ?
	`, ownerHash, moved); err != nil {
		t.Fatalf("owner changes password: %v", err)
	}

	replacement, err := lockedAccountHash()
	if err != nil {
		t.Fatalf("lockedAccountHash: %v", err)
	}
	writeTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin write: %v", err)
	}
	for _, candidate := range candidates {
		lockedIt, err := lockLegacyPasswordAccountTx(ctx, writeTx, candidate, replacement)
		if err != nil {
			_ = writeTx.Rollback()
			t.Fatalf("lock %s: %v", candidate.username, err)
		}
		if want := candidate.id == still; lockedIt != want {
			_ = writeTx.Rollback()
			t.Fatalf("lock %s reported %v, want %v", candidate.username, lockedIt, want)
		}
	}
	if err := writeTx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := readLegacyLockAccount(t, db, moved); got.passwordHash != ownerHash || got.mustChange {
		t.Fatalf("the owner's new password was overwritten: %+v", got)
	}
	if got := readLegacyLockSession(t, db, movedSession); got.revoked {
		t.Fatalf("the session of an account left alone was revoked: %+v", got)
	}
	if got := readLegacyLockAccount(t, db, still); got.passwordHash != replacement || !got.mustChange {
		t.Fatalf("the untouched candidate was not locked: %+v", got)
	}
}
