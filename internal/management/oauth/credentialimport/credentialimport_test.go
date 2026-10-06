package credentialimport

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeMessageMasksSecretsAndBoundsLength(t *testing.T) {
	secret := "sk-ant-sid01-abcdefghijklmnop"
	err := errors.New("upstream said: invalid session " + secret + "\n  try again")
	got := SafeMessage(err, secret, "ab")
	if strings.Contains(got, secret) {
		t.Fatalf("message leaks the secret: %q", got)
	}
	if got != "upstream said: invalid session *** try again" {
		t.Fatalf("message = %q", got)
	}

	long := SafeMessage(errors.New(strings.Repeat("x", 1000)))
	if len([]rune(long)) != maxMessageLength+1 || !strings.HasSuffix(long, "…") {
		t.Fatalf("long message not bounded: %d runes", len([]rune(long)))
	}
	if SafeMessage(nil) != "" {
		t.Fatal("nil error should render empty")
	}
}
