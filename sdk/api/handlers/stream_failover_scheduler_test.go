package handlers

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// The scheduler can fail on an account while it looks for a stream to hand
// over (here the upstream refuses the first request outright), and the stream
// it then hands over can fail before its first byte. The handler's retry has to
// stay away from both accounts, not only from the one whose stream it holds:
// the first may have no cooldown behind it (cooling off for that account), or
// one that busy waiting would sit out before sending the request back to it.
func TestStreamFailoverSkipsAccountsTheSchedulerAlreadyFailed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attributes map[string]string
		busyWait   bool
	}{
		{name: "cooling off for the accounts", attributes: map[string]string{coreauth.DisableCoolingAttributeKey: "true"}},
		{name: "cooling on, busy waiting on", busyWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{
					failToOpen(http.StatusInternalServerError), // refused outright: the scheduler fails on this account
					failBeforeFirstByte(http.StatusBadGateway), // a stream that opens, then fails before any byte
					succeed("must not be reached"),             // a third attempt would only come from going back
				}}
				manager := newFailoverTestManagerWithAttributes(t, executor, "codex", tc.attributes, "sfs-a", "sfs-b")
				if tc.busyWait {
					enableBusyWait(manager)
				}
				handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

				start := time.Now()
				out := streamOnce(t, handler)

				calls := executor.AuthIDs()
				if len(calls) != 2 || !distinct(calls) {
					t.Fatalf("attempts = %v, want each of the two accounts once and no return to either", calls)
				}
				if out.status != http.StatusBadGateway {
					t.Fatalf("outcome = %+v, want the stream's own 502 as the cause", out)
				}
				if took := time.Since(start); took != 0 {
					t.Fatalf("took %s, want an immediate failure rather than a wait for an account that already failed", took)
				}
			})
		})
	}
}
