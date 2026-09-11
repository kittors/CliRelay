package enduser

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"golang.org/x/crypto/bcrypt"
)

// The portal used to accept any 8-character string with no complexity rule, so
// a user could replace a generated 16-character password with "12345678" the
// moment they signed in. The strength requirement applied only to the credential
// the system handed out, never to the one the user chose.
func TestHashPasswordEnforcesTheIdentityPolicy(t *testing.T) {
	t.Parallel()

	for _, password := range []string{"short", "12345678", "alllowercase!", "NoSpecialChar1"} {
		if _, err := HashPassword(password); err == nil {
			t.Fatalf("HashPassword(%q) was accepted, want a policy rejection", password)
		}
	}
	if _, err := HashPassword("Correct-Horse-1!"); err != nil {
		t.Fatalf("HashPassword rejected a compliant password: %v", err)
	}
}

// The rejection has to stay a 400 (errors.Is against this package's sentinel)
// while still carrying the per-rule code the panel translates (errors.As
// through to identity). Losing either one regresses a user-visible behaviour:
// the first turns a bad password into a 500, the second turns a localized
// field error back into an English toast.
func TestHashPasswordRejectionCarriesBothSentinelAndCode(t *testing.T) {
	t.Parallel()

	_, err := HashPassword("alllowercase!")
	if err == nil {
		t.Fatal("HashPassword accepted a password with no uppercase letter")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want it to satisfy errors.Is(err, ErrValidation)", err)
	}
	var policy *identity.PasswordPolicyError
	if !errors.As(err, &policy) {
		t.Fatalf("error = %v, want it to unwrap to *identity.PasswordPolicyError", err)
	}
	if policy.Code != identity.PasswordMissingUpper {
		t.Fatalf("code = %q, want %q", policy.Code, identity.PasswordMissingUpper)
	}
	// The wrapper must not double up the "validation failed:" prefix.
	if strings.Count(err.Error(), "validation failed") != 1 {
		t.Fatalf("message = %q, want exactly one 'validation failed' prefix", err.Error())
	}
}

// The incident anchor. On 2026-09-10 an admin reset a portal password, handed
// the generated string to the user, and neither of them could sign in with it.
// This closes the loop the operator actually exercised: generate the temporary
// password, store it the way ResetPassword does, then verify it the way Login
// does. If the generator and the policy ever drift apart again — a generated
// password its own HashPassword refuses — this fails instead of shipping an
// account nobody can log into.
func TestGeneratedResetPasswordCanBeHashedAndVerified(t *testing.T) {
	t.Parallel()

	for i := 0; i < 50; i++ {
		password, err := randomPassword()
		if err != nil {
			t.Fatalf("randomPassword() error = %v", err)
		}
		hash, err := HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword rejected the password this package generated (%q): %v", password, err)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
			t.Fatalf("the stored hash does not verify the password it was derived from: %v", err)
		}
	}
}

// A cooldown must tell the caller how long it has to wait. Reporting only "not
// now" left the panel with a single "too many attempts" string, so a five-minute
// lock was indistinguishable from a one-minute or a one-hour one and users kept
// retrying straight onto the next rung.
func TestCooldownErrorCarriesRemainingTime(t *testing.T) {
	t.Parallel()

	now := time.Now()
	err := newCooldownError(now.Add(5*time.Minute), now)
	if !errors.Is(err, ErrLoginCooldowned) {
		t.Fatalf("error = %v, want it to satisfy errors.Is(err, ErrLoginCooldowned)", err)
	}
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("error = %v, want it to unwrap to *CooldownError", err)
	}
	if cooldown.RetryAfter != 5*time.Minute {
		t.Fatalf("RetryAfter = %v, want 5m", cooldown.RetryAfter)
	}

	// An already-elapsed deadline must never render as "retry after 0s" while
	// the lock is still being reported.
	past := newCooldownError(now.Add(-time.Hour), now)
	var elapsed *CooldownError
	if !errors.As(past, &elapsed) {
		t.Fatalf("error = %v, want it to unwrap to *CooldownError", past)
	}
	if elapsed.RetryAfter < time.Second {
		t.Fatalf("RetryAfter = %v, want at least 1s", elapsed.RetryAfter)
	}
}
