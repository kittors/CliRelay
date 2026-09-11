package identity

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"golang.org/x/crypto/bcrypt"
)

const (
	generatedPasswordLength = 16
	// PasswordMinLength and PasswordMaxBytes are exported so callers can render
	// the requirement before the user submits, instead of discovering it from a
	// rejection.
	PasswordMinLength = 12
	PasswordMaxBytes  = 72

	// The generated-password alphabets deliberately omit characters that are
	// indistinguishable in the fonts these passwords actually travel through
	// (chat clients, screenshots, printouts): I/l/1 and O/0. A temporary password
	// is transcribed by hand far more often than it is pasted, and a character
	// the user cannot read is a lockout waiting to happen.
	passwordUpperCharacters = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	passwordLowerCharacters = "abcdefghijkmnpqrstuvwxyz"
	passwordDigitCharacters = "23456789"
	// Restricted to symbols that survive shell quoting, URL forms and chat
	// auto-formatting unchanged. Brackets and quotes are omitted for the same
	// transcription reason as above.
	passwordSpecialCharacters = "!@#$%^&*+=?"
	passwordAllCharacters     = passwordUpperCharacters + passwordLowerCharacters + passwordDigitCharacters + passwordSpecialCharacters
)

// PasswordPolicyCode names a single password rule. It travels to the browser as
// the API error code so the panel can render a localized message per rule; the
// English Error() text is a fallback for logs and non-UI callers, never
// something a user should have to read.
type PasswordPolicyCode string

const (
	PasswordTooShort       PasswordPolicyCode = "password_too_short"
	PasswordTooLong        PasswordPolicyCode = "password_too_long"
	PasswordMissingUpper   PasswordPolicyCode = "password_missing_upper"
	PasswordMissingLower   PasswordPolicyCode = "password_missing_lower"
	PasswordMissingSpecial PasswordPolicyCode = "password_missing_special"
)

// PasswordPolicyError is one violated password rule.
//
// It exists because the previous version returned a bare wrapped ErrValidation
// whose only machine-readable part was "validation_failed". The panel had
// nothing to key a translation off, so it fell back to printing the raw English
// sentence in a toast — which is what users actually saw when a tenant admin
// password missed the uppercase rule.
type PasswordPolicyError struct {
	Code PasswordPolicyCode
	// Limit carries the boundary the input missed, so a caller can render
	// "at least 12 characters" without hardcoding the number a second time.
	// It is only meaningful for PasswordTooShort and PasswordTooLong.
	Limit int
}

func (e *PasswordPolicyError) Error() string {
	switch e.Code {
	case PasswordTooShort:
		return fmt.Sprintf("validation failed: password must contain at least %d characters", e.Limit)
	case PasswordTooLong:
		return fmt.Sprintf("validation failed: password must not exceed %d bytes", e.Limit)
	case PasswordMissingUpper:
		return "validation failed: password must contain at least one uppercase letter"
	case PasswordMissingLower:
		return "validation failed: password must contain at least one lowercase letter"
	case PasswordMissingSpecial:
		return "validation failed: password must contain at least one non-alphanumeric character"
	default:
		return "validation failed: password does not meet the password policy"
	}
}

// Is keeps every existing errors.Is(err, ErrValidation) call site working, so
// the HTTP layer still maps a policy violation to 400 without knowing about
// this type.
func (e *PasswordPolicyError) Is(target error) bool { return target == ErrValidation }

// ValidatePassword reports the first rule the password violates, or nil.
//
// It is exported so services that store their own credentials (the end-user
// portal) enforce the same rules as the admin identity store rather than
// drifting into a weaker private copy.
func ValidatePassword(password string) error {
	if len(password) < PasswordMinLength {
		return &PasswordPolicyError{Code: PasswordTooShort, Limit: PasswordMinLength}
	}
	// bcrypt refuses anything longer than 72 bytes. Reporting that as a validation error
	// keeps it a 400 from the change-password endpoint; the raw bcrypt error is not
	// wrapped in ErrValidation and would surface as a 500.
	if len(password) > PasswordMaxBytes {
		return &PasswordPolicyError{Code: PasswordTooLong, Limit: PasswordMaxBytes}
	}

	hasUpper := false
	hasLower := false
	hasSpecial := false
	for i := 0; i < len(password); i++ {
		switch c := password[i]; {
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= '0' && c <= '9':
		default:
			hasSpecial = true
		}
	}
	if !hasUpper {
		return &PasswordPolicyError{Code: PasswordMissingUpper}
	}
	if !hasLower {
		return &PasswordPolicyError{Code: PasswordMissingLower}
	}
	if !hasSpecial {
		return &PasswordPolicyError{Code: PasswordMissingSpecial}
	}
	return nil
}

func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// GeneratePassword returns a password that satisfies ValidatePassword and is
// meant to be transcribed by a human at least once.
func GeneratePassword() (string, error) { return randomPassword() }

func randomPassword() (string, error) {
	password := make([]byte, 0, generatedPasswordLength)
	for _, charset := range []string{passwordUpperCharacters, passwordLowerCharacters, passwordDigitCharacters, passwordSpecialCharacters} {
		ch, err := randomPasswordCharacter(charset)
		if err != nil {
			return "", err
		}
		password = append(password, ch)
	}
	for len(password) < generatedPasswordLength {
		ch, err := randomPasswordCharacter(passwordAllCharacters)
		if err != nil {
			return "", err
		}
		password = append(password, ch)
	}
	if err := shufflePasswordCharacters(password); err != nil {
		return "", err
	}
	return string(password), nil
}

func randomPasswordCharacter(charset string) (byte, error) {
	if charset == "" {
		return 0, fmt.Errorf("identity: empty password charset")
	}
	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
	if err != nil {
		return 0, err
	}
	return charset[idx.Int64()], nil
}

func shufflePasswordCharacters(password []byte) error {
	for i := len(password) - 1; i > 0; i-- {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return err
		}
		j := int(idx.Int64())
		password[i], password[j] = password[j], password[i]
	}
	return nil
}
