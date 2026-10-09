package enduser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loginlockout"
)

// TestPostgresPortalLoginWarnsThenLocks pins the portal half of the shared
// ladder on the engine production runs: every failure short of the first rung
// says how many remain, the failure that arms the cooldown is still a wrong
// password (so the API layer charges its guess budget for it, which it used to
// skip), and during the cooldown no password is compared or counted.
func TestPostgresPortalLoginWarnsThenLocks(t *testing.T) {
	db := openLegacyLockPostgresDB(t)
	svc := NewService(db)
	ctx := context.Background()
	const password = "Portal-Password-123!"
	const wrong = "Wrong-Password-123!"

	id := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
		VALUES (?, ?, 'lockout_pg', 'lockout_pg', 'lockout_pg', ?, false)
	`, id, identity.SystemTenantID, bcryptForTest(t, password)); err != nil {
		t.Fatalf("insert portal account: %v", err)
	}

	for i := 1; i <= 5; i++ {
		_, err := svc.Login(ctx, "lockout_pg", wrong, "test")
		var failure *loginlockout.FailureError
		if !errors.As(err, &failure) || !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failed login %d = %v, want a *loginlockout.FailureError for ErrInvalidCredentials", i, err)
		}
		if i < 5 {
			if failure.Cooldown != nil || failure.Remaining != 5-i {
				t.Fatalf("failed login %d = {Remaining: %d, Cooldown: %v}, want {%d, nil}", i, failure.Remaining, failure.Cooldown, 5-i)
			}
			continue
		}
		if failure.Cooldown == nil || failure.Cooldown.RetryAfter != time.Minute {
			t.Fatalf("fifth failed login armed %v, want a one-minute cooldown", failure.Cooldown)
		}
	}

	for _, attempt := range []string{password, wrong} {
		_, err := svc.Login(ctx, "lockout_pg", attempt, "test")
		var cooldown *loginlockout.CooldownError
		if !errors.As(err, &cooldown) || errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("login during cooldown = %v, want only a *loginlockout.CooldownError", err)
		}
	}
	var failedCount int
	if err := db.QueryRowContext(ctx, `SELECT failed_login_count FROM end_users WHERE id = ?`, id).Scan(&failedCount); err != nil {
		t.Fatal(err)
	}
	if failedCount != 5 {
		t.Fatalf("failed_login_count = %d after attempts during the cooldown, want 5", failedCount)
	}

	if _, err := db.ExecContext(ctx, `UPDATE end_users SET locked_until = now() - interval '1 second' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "lockout_pg", password, "test"); err != nil {
		t.Fatalf("login after the cooldown expired: %v", err)
	}
}
