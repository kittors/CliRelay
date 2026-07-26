package identity

import (
	"errors"
	"strings"
	"testing"
)

// bcrypt refuses inputs longer than 72 bytes. That has to surface as a validation error,
// otherwise the change-password endpoint reports it as a 500 instead of a 400.
func TestHashPasswordRejectsOverlongPasswordAsValidation(t *testing.T) {
	longPassword := "Aa1!" + strings.Repeat("x", 80)
	_, err := HashPassword(longPassword)
	if err == nil {
		t.Fatal("HashPassword accepted a password longer than bcrypt's 72-byte limit")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want it to wrap ErrValidation", err)
	}
}

// The longest input bcrypt accepts must still work.
func TestHashPasswordAcceptsMaximumLength(t *testing.T) {
	maxPassword := "Aa1!" + strings.Repeat("x", 68)
	if len(maxPassword) != 72 {
		t.Fatalf("test fixture is %d bytes, want 72", len(maxPassword))
	}
	if _, err := HashPassword(maxPassword); err != nil {
		t.Fatalf("HashPassword rejected a 72-byte password: %v", err)
	}
}
