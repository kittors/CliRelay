package management

import (
	"testing"
	"time"
)

func TestShouldRecordManagementAudit(t *testing.T) {
	cases := []struct {
		name          string
		write         bool
		sensitiveRead bool
		result        string
		want          bool
	}{
		{name: "successful write", write: true, result: auditResultSuccess, want: true},
		{name: "failed write", write: true, result: auditResultFailed, want: true},
		{name: "denied write", write: true, result: auditResultDenied, want: true},
		{name: "sensitive read", sensitiveRead: true, result: auditResultSuccess, want: true},
		{name: "failed sensitive read", sensitiveRead: true, result: auditResultFailed, want: true},
		{name: "successful read", result: auditResultSuccess, want: false},
		// The flood this policy exists for: a read endpoint answering 502 because an
		// optional sidecar is absent is an operational fault, not an audit event.
		{name: "failed read", result: auditResultFailed, want: false},
		{name: "denied read", result: auditResultDenied, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRecordManagementAudit(tc.write, tc.sensitiveRead, tc.result); got != tc.want {
				t.Fatalf("shouldRecordManagementAudit(%v, %v, %q) = %v, want %v", tc.write, tc.sensitiveRead, tc.result, got, tc.want)
			}
		})
	}
}

func TestAuditRepeatLimiterCollapsesWithinWindow(t *testing.T) {
	limiter := newAuditRepeatLimiter(5*time.Minute, 10*time.Minute)
	start := time.Unix(1700000000, 0).UTC()
	key := auditRepeatKey("tenant", "user_session", "user", "management.get", "update", "progress", auditResultDenied)

	if allowed, folded := limiter.admit(key, start); !allowed || folded != 0 {
		t.Fatalf("first admit = (%v, %d), want (true, 0)", allowed, folded)
	}
	// A client polling every two seconds for the rest of the window.
	suppressed := 0
	for offset := 2 * time.Second; offset < 5*time.Minute; offset += 2 * time.Second {
		if allowed, _ := limiter.admit(key, start.Add(offset)); allowed {
			t.Fatalf("admit at +%s = true, want the window to hold", offset)
		}
		suppressed++
	}
	if suppressed == 0 {
		t.Fatal("test did not exercise any suppression")
	}
	allowed, folded := limiter.admit(key, start.Add(5*time.Minute))
	if !allowed {
		t.Fatal("admit after the window = false, want true")
	}
	if folded != int64(suppressed) {
		t.Fatalf("folded = %d, want %d", folded, suppressed)
	}
	// The count belongs to the row that carried it and must not be replayed.
	if allowed, folded = limiter.admit(key, start.Add(10*time.Minute)); !allowed || folded != 0 {
		t.Fatalf("next window admit = (%v, %d), want (true, 0)", allowed, folded)
	}
}

func TestAuditRepeatLimiterKeepsDistinctSignaturesApart(t *testing.T) {
	limiter := newAuditRepeatLimiter(5*time.Minute, 10*time.Minute)
	now := time.Unix(1700000000, 0).UTC()

	denied := auditRepeatKey("tenant", "user_session", "user", "management.get", "update", "progress", auditResultDenied)
	otherActor := auditRepeatKey("tenant", "user_session", "other", "management.get", "update", "progress", auditResultDenied)
	otherRoute := auditRepeatKey("tenant", "user_session", "user", "management.get", "tenants", "", auditResultDenied)

	for _, key := range []string{denied, otherActor, otherRoute} {
		if allowed, _ := limiter.admit(key, now); !allowed {
			t.Fatalf("first admit for %q = false, want true", key)
		}
	}
	if allowed, _ := limiter.admit(denied, now.Add(time.Second)); allowed {
		t.Fatal("repeat of the same signature was admitted")
	}
}

func TestAuditRepeatLimiterSweepsIdleSignatures(t *testing.T) {
	limiter := newAuditRepeatLimiter(time.Minute, 2*time.Minute)
	now := time.Unix(1700000000, 0).UTC()
	key := auditRepeatKey("tenant", "user_session", "user", "management.get", "update", "progress", auditResultDenied)

	limiter.admit(key, now)
	limiter.admit(key, now.Add(time.Second))
	// A different signature drives the sweep once the idle TTL has elapsed.
	limiter.admit(auditRepeatKey("tenant", "user_session", "user", "management.get", "roles", "", auditResultDenied), now.Add(5*time.Minute))

	limiter.mu.Lock()
	_, stillTracked := limiter.entries[key]
	limiter.mu.Unlock()
	if stillTracked {
		t.Fatal("idle signature survived the sweep")
	}
}

func TestAuditRepeatLimiterNilAdmitsEverything(t *testing.T) {
	var limiter *auditRepeatLimiter
	if allowed, folded := limiter.admit("key", time.Now()); !allowed || folded != 0 {
		t.Fatalf("nil limiter admit = (%v, %d), want (true, 0)", allowed, folded)
	}
}
