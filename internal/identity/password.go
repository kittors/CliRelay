package identity

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"golang.org/x/crypto/bcrypt"
)

const (
	generatedPasswordLength   = 16
	passwordUpperCharacters   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	passwordLowerCharacters   = "abcdefghijklmnopqrstuvwxyz"
	passwordDigitCharacters   = "0123456789"
	passwordSpecialCharacters = "!@#$%^&*()-_=+[]{}:,.?"
	passwordAllCharacters     = passwordUpperCharacters + passwordLowerCharacters + passwordDigitCharacters + passwordSpecialCharacters
)

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("%w: password must contain at least 12 characters", ErrValidation)
	}
	// bcrypt refuses anything longer than 72 bytes. Reporting that as a validation error
	// keeps it a 400 from the change-password endpoint; the raw bcrypt error is not
	// wrapped in ErrValidation and would surface as a 500.
	if len(password) > 72 {
		return "", fmt.Errorf("%w: password must not exceed 72 bytes", ErrValidation)
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
		return "", fmt.Errorf("%w: password must contain at least one uppercase letter", ErrValidation)
	}
	if !hasLower {
		return "", fmt.Errorf("%w: password must contain at least one lowercase letter", ErrValidation)
	}
	if !hasSpecial {
		return "", fmt.Errorf("%w: password must contain at least one non-alphanumeric character", ErrValidation)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

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
