package middleware

import "testing"

// A usage record spooled during an outage is counted for TPM without a
// database lookup; the key's admission subject routes it to its account pool.
func TestSpooledTokenUsageLandsInTheAdmittedAccountPool(t *testing.T) {
	resetQuotaMiddlewareState(t)
	rememberAdmissionSubject("sk-owned-XXXX", "eu:user-a")
	RecordTokenUsageForRequest("sk-owned-XXXX", "", 40)
	if got := getTPMTracker("eu:user-a").sum(); got != 40 {
		t.Fatalf("account pool TPM = %d, want 40", got)
	}
	if got := getTPMTracker("sk-owned-XXXX").sum(); got != 0 {
		t.Fatalf("key TPM = %d, want 0", got)
	}
	// A key that is no longer owned goes back to its own pool.
	rememberAdmissionSubject("sk-owned-XXXX", "sk-owned-XXXX")
	RecordTokenUsageForRequest("sk-owned-XXXX", "", 5)
	if got := getTPMTracker("sk-owned-XXXX").sum(); got != 5 {
		t.Fatalf("key TPM after unbinding = %d, want 5", got)
	}
	// A resolved owner always wins.
	RecordTokenUsageForRequest("sk-owned-XXXX", "user-b", 3)
	if got := getTPMTracker("eu:user-b").sum(); got != 3 {
		t.Fatalf("resolved owner TPM = %d, want 3", got)
	}
}
