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

// Every rejection must name the rule it broke. Without a per-rule code the API
// can only answer "validation_failed", which left the panel printing the raw
// English sentence in a toast instead of a localized message under the field.
func TestValidatePasswordReportsTheViolatedRule(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantCode PasswordPolicyCode
	}{
		{name: "too short", password: "Aa1!short", wantCode: PasswordTooShort},
		{name: "too long", password: "Aa1!" + strings.Repeat("x", 80), wantCode: PasswordTooLong},
		{name: "no uppercase", password: "alllowercase!", wantCode: PasswordMissingUpper},
		{name: "no lowercase", password: "ALLUPPERCASE!1", wantCode: PasswordMissingLower},
		{name: "no special", password: "NoSpecialChar1", wantCode: PasswordMissingSpecial},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			var policy *PasswordPolicyError
			if !errors.As(err, &policy) {
				t.Fatalf("ValidatePassword(%q) = %v, want a *PasswordPolicyError", tc.password, err)
			}
			if policy.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", policy.Code, tc.wantCode)
			}
			// The HTTP layer still keys its 400 off this sentinel.
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want it to satisfy errors.Is(err, ErrValidation)", err)
			}
		})
	}

	if err := ValidatePassword("Correct-Horse-1!"); err != nil {
		t.Fatalf("ValidatePassword rejected a compliant password: %v", err)
	}
}

// A generated password the policy itself would reject is a self-inflicted
// lockout: the account is created with a credential its own change-password
// endpoint refuses.
func TestGeneratePasswordSatisfiesTheOwnPolicy(t *testing.T) {
	for i := 0; i < 200; i++ {
		password, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword() error = %v", err)
		}
		if err := ValidatePassword(password); err != nil {
			t.Fatalf("GeneratePassword() produced %q, which its own policy rejects: %v", password, err)
		}
	}
}

// Temporary passwords are read off a screen and typed somewhere else far more
// often than they are pasted. Production access logs show users needing two or
// three attempts to transcribe the old base64 output, which mixed I/l/1 and
// O/0 — enough, under the old lockout ladder, to lock themselves out.
func TestGeneratePasswordOmitsVisuallyAmbiguousCharacters(t *testing.T) {
	const ambiguous = "Il1O0"
	for i := 0; i < 200; i++ {
		password, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword() error = %v", err)
		}
		if idx := strings.IndexAny(password, ambiguous); idx >= 0 {
			t.Fatalf("GeneratePassword() produced %q containing ambiguous character %q",
				password, password[idx])
		}
	}
}
