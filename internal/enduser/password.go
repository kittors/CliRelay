package enduser

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"golang.org/x/crypto/bcrypt"
)

// passwordPolicyError adapts an identity policy violation into this package's
// validation sentinel while keeping the underlying *identity.PasswordPolicyError
// reachable through errors.As, so the API layer can still emit a per-rule code
// instead of a generic "validation_failed" with an English sentence attached.
type passwordPolicyError struct{ err error }

func (e passwordPolicyError) Error() string        { return e.err.Error() }
func (e passwordPolicyError) Unwrap() error        { return e.err }
func (e passwordPolicyError) Is(target error) bool { return target == ErrValidation }

// HashPassword enforces the same policy as the admin identity store.
//
// Portal accounts previously accepted any 8-character string with no complexity
// requirement, so a user could replace the generated 16-character password with
// "12345678" the moment they signed in: the strength requirement applied only to
// the credential the system handed out, never to the one the user chose.
func HashPassword(password string) (string, error) {
	if err := identity.ValidatePassword(password); err != nil {
		return "", passwordPolicyError{err: err}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// randomPassword returns a temporary password for an account whose owner will
// have to read it off a screen and type it somewhere else.
//
// It delegates to the identity generator rather than emitting raw base64: the
// previous base64url output mixed I/l/1 and O/0 in a 16-character run, and the
// production access logs show ordinary users needing two or three attempts to
// transcribe one — enough, under the old lockout ladder, to lock themselves out
// before ever getting in.
func randomPassword() (string, error) {
	return identity.GeneratePassword()
}
