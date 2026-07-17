package enduser

import (
	"errors"
	"testing"
)

func TestMaskAPIKey(t *testing.T) {
	// Use placeholder shape that CI secret scanner accepts (contains X).
	raw := "sk-XXXXXXabcdefghijklmnop"
	if MaskAPIKey(raw) == raw {
		t.Fatal("expected masked key")
	}
	if got := MaskAPIKey("short"); got != "short" {
		t.Fatalf("short key = %q", got)
	}
}

func TestGenerateAPIKeyUniquenessSample(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		k, err := GenerateAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := seen[k]; ok {
			t.Fatalf("duplicate key in sample: %s", k)
		}
		seen[k] = struct{}{}
	}
}

func TestLockPenaltyIntermediate(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 6, 7, 8, 9, 11, 12, 16, 19} {
		_, _, _, apply := lockPenalty(n)
		if apply {
			t.Fatalf("count %d should not apply stage lock", n)
		}
	}
}

func TestIsUnsupportedAdvisoryLockError(t *testing.T) {
	if !isUnsupportedAdvisoryLockError(errors.New("no such function: pg_advisory_xact_lock")) {
		t.Fatal("sqlite missing function should be unsupported")
	}
	if !isUnsupportedAdvisoryLockError(errors.New(`function pg_advisory_xact_lock(integer) does not exist`)) {
		t.Fatal("pg missing function should be unsupported")
	}
	// Permission / execution failures must fail closed (not treated as unsupported).
	if isUnsupportedAdvisoryLockError(errors.New("permission denied for function pg_advisory_xact_lock")) {
		t.Fatal("permission denied must fail closed")
	}
	if isUnsupportedAdvisoryLockError(errors.New("could not obtain advisory lock")) {
		t.Fatal("lock contention/errors must fail closed")
	}
}
