package enduser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

// openLegacyLockPostgresDB migrates a disposable database, so the concurrent
// passes below cannot collide with other packages' tests on the shared one.
func openLegacyLockPostgresDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	adminDB, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("enduser_legacy_lock_%d", time.Now().UnixNano())
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create disposable db: %v", err)
	}
	// Registered first so it runs last: runtime close → drop → admin close.
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()
		`, dbName)
		if _, err := adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Errorf("drop disposable db %s: %v", dbName, err)
		}
		_ = adminDB.Close()
	})
	testDSN, err := legacyLockPostgresDSN(dsn, dbName)
	if err != nil {
		t.Fatal(err)
	}
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: testDSN, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("open migrated runtime db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func legacyLockPostgresDSN(dsn, dbName string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + dbName
		return u.String(), nil
	}
	parts := strings.Fields(dsn)
	out := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		if !strings.HasPrefix(part, "dbname=") {
			out = append(out, part)
		}
	}
	return strings.Join(append(out, "dbname="+dbName), " "), nil
}

// End to end on the engine production runs. An account from the pre-#1040
// backfill signs in with the published password; after the pass it cannot, the
// session it had is dead, its key is untouched, and an admin reset brings it
// back. Replicas booting together lock it exactly once, and pay for one
// comparison between them. Not parallel: it counts comparisons.
func TestPostgresLegacyBackfillPasswordLock(t *testing.T) {
	db := openLegacyLockPostgresDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tenant := identity.SystemTenantID
	legacyHash := bcryptForTest(t, legacyBackfillPassword)

	// The pre-#1040 backfill's own statements, in one transaction as it ran them:
	// created_at and password_changed_at from the column defaults, done_at from
	// now(), all on the transaction clock.
	legacyID, ownerID := uuid.NewString(), uuid.NewString()
	backfill, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin legacy backfill: %v", err)
	}
	for id, username := range map[string]string{legacyID: "legacy_pg", ownerID: "owner_pg"} {
		if _, err := backfill.ExecContext(ctx, `
			INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
			VALUES (?, ?, ?, ?, ?, ?, false)
		`, id, tenant, username, username, username, legacyHash); err != nil {
			t.Fatalf("insert backfilled account %s: %v", username, err)
		}
	}
	if _, err := backfill.ExecContext(ctx, `INSERT INTO end_user_backfill_state (id, done_at) VALUES (1, now()) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("record legacy backfill run: %v", err)
	}
	if err := backfill.Commit(); err != nil {
		t.Fatalf("commit legacy backfill: %v", err)
	}
	// What production shows for its batch, to the microsecond.
	var batchMatches int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM end_users e, end_user_backfill_state s
		WHERE s.id = 1 AND e.created_at = s.done_at AND e.password_changed_at = s.done_at
	`).Scan(&batchMatches); err != nil || batchMatches != 2 {
		t.Fatalf("accounts with created_at = password_changed_at = done_at: %d (%v), want 2", batchMatches, err)
	}
	// The backfill ran long before anyone signs in; move the whole batch back a
	// day, keeping its timestamps equal, so the owner's change below is later.
	for _, stmt := range []string{
		`UPDATE end_users SET created_at = created_at - interval '1 day', updated_at = updated_at - interval '1 day',
			password_changed_at = password_changed_at - interval '1 day'`,
		`UPDATE end_user_backfill_state SET done_at = done_at - interval '1 day'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("backdate legacy batch: %v", err)
		}
	}
	// Provisioned since, never changed, on a salt of its own: outside the batch,
	// so it must not cost a comparison.
	outsiderID := uuid.NewString()
	const outsiderPassword = "Provisioned-Password-1!"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
		VALUES (?, ?, 'outsider_pg', 'outsider_pg', 'outsider_pg', ?, false)
	`, outsiderID, tenant, bcryptForTest(t, outsiderPassword)); err != nil {
		t.Fatalf("insert account outside the batch: %v", err)
	}
	key, err := svc.CreateKey(ctx, tenant, legacyID, "legacy-key")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	legacySession, err := svc.Login(ctx, "legacy_pg", legacyBackfillPassword, "test")
	if err != nil {
		t.Fatalf("before the pass the published password signs in; Login: %v", err)
	}
	// The other account's owner replaced the password through the portal.
	ownerLogin, err := svc.Login(ctx, "owner_pg", legacyBackfillPassword, "test")
	if err != nil {
		t.Fatalf("owner Login: %v", err)
	}
	ownerUser, ownerSessionID, err := svc.Authenticate(ctx, ownerLogin.AccessToken)
	if err != nil {
		t.Fatalf("owner Authenticate: %v", err)
	}
	const ownerPassword = "Owner-Chosen-Password-1!"
	if err := svc.ChangePassword(ctx, ownerUser, ownerSessionID, legacyBackfillPassword, ownerPassword); err != nil {
		t.Fatalf("owner ChangePassword: %v", err)
	}

	// Replicas booting together. Only the one that wins the advisory lock does
	// the work; the rest find the marker.
	calls := countLegacyPasswordComparisons(t)
	const runners = 4
	results := make([][]string, runners)
	errs := make([]error, runners)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.LockLegacyBackfillPasswordAccounts(ctx)
		}()
	}
	close(start)
	wg.Wait()

	if _, err := svc.Login(ctx, "legacy_pg", legacyBackfillPassword, "test"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login with the legacy backfill password after the pass: err = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := svc.Authenticate(ctx, legacySession.AccessToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("session opened with the legacy password: Authenticate err = %v, want ErrSessionRevoked", err)
	}
	if _, err := svc.Refresh(ctx, legacySession.RefreshToken, "test"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("session opened with the legacy password: Refresh err = %v, want ErrSessionExpired", err)
	}

	winners := 0
	for i := range runners {
		if errs[i] != nil {
			t.Fatalf("runner %d: %v", i, errs[i])
		}
		if len(results[i]) == 0 {
			continue
		}
		winners++
		if !slices.Equal(results[i], []string{"legacy_pg"}) {
			t.Fatalf("runner %d locked %v, want [legacy_pg]", i, results[i])
		}
	}
	if winners != 1 {
		t.Fatalf("%d runners locked accounts, want exactly 1: %v", winners, results)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("bcrypt comparisons across %d runners = %d, want 1", runners, got)
	}

	var version int64
	var mustChange bool
	var status, reason string
	var createdAt, changedAt time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT version, must_change_password, status, created_at, password_changed_at
		FROM end_users WHERE id = ?
	`, legacyID).Scan(&version, &mustChange, &status, &createdAt, &changedAt); err != nil {
		t.Fatalf("read locked account: %v", err)
	}
	if version != 2 || !mustChange || status != "active" || !changedAt.Equal(createdAt) {
		t.Fatalf("locked account = (version %d, must_change %v, status %q, changed %s, created %s), want (2, true, active, changed == created)",
			version, mustChange, status, changedAt, createdAt)
	}
	if err := db.QueryRowContext(ctx, `SELECT revoke_reason FROM end_user_sessions WHERE end_user_id = ?`, legacyID).Scan(&reason); err != nil {
		t.Fatalf("read revoked session: %v", err)
	}
	if reason != "legacy_password_lock" {
		t.Fatalf("revoke_reason = %q, want legacy_password_lock", reason)
	}

	var secret, owner string
	var disabled int
	if err := db.QueryRowContext(ctx, `SELECT key, end_user_id, disabled FROM api_keys WHERE id = ?`, key.APIKey.ID).Scan(&secret, &owner, &disabled); err != nil {
		t.Fatalf("read key: %v", err)
	}
	if secret != key.PlaintextKey || owner != legacyID || disabled != 0 {
		t.Fatalf("key after lock = (%q, %q, disabled=%d), want untouched", secret, owner, disabled)
	}

	if _, err := svc.Login(ctx, "owner_pg", ownerPassword, "test"); err != nil {
		t.Fatalf("an owner who replaced the password was locked out: %v", err)
	}
	if _, err := svc.Login(ctx, "outsider_pg", outsiderPassword, "test"); err != nil {
		t.Fatalf("an account outside the batch was locked out: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT version FROM end_users WHERE id = ?`, ownerID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("owner account version = %d (%v), want 2 from its own password change only", version, err)
	}

	var lockedCount int
	if err := db.QueryRowContext(ctx, `SELECT locked_count FROM end_user_legacy_password_lock_state WHERE id = 1`).Scan(&lockedCount); err != nil || lockedCount != 1 {
		t.Fatalf("lock marker locked_count = %d (%v), want 1", lockedCount, err)
	}

	generated, err := svc.ResetPassword(ctx, identity.Principal{PlatformAdmin: true}, tenant, legacyID, "")
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	reissued, err := svc.Login(ctx, "legacy_pg", generated, "test")
	if err != nil {
		t.Fatalf("Login with the credential an admin issued: %v", err)
	}
	if !reissued.MustChangePassword {
		t.Fatal("an admin-issued credential must be changed on first sign-in")
	}
}
