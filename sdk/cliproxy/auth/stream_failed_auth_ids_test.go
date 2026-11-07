package auth

import (
	"context"
	"slices"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// A stream handed over by the scheduler names the accounts the same call failed
// on to get there, so a caller that abandons the stream can keep its retry away
// from them too. A call that needed no failover names none.
func TestStreamResultNamesTheAccountsTheCallFailedOn(t *testing.T) {
	exec := &scriptedExecutor{script: []error{upstreamDown}}
	mgr := newPlainManager(t, exec, "fa-1", "fa-2")
	req := cliproxyexecutor.Request{Model: "test-model"}
	opts := cliproxyexecutor.Options{Stream: true}

	result, err := mgr.ExecuteStream(context.Background(), []string{"flaky"}, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	calls := exec.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want a refusal and then the account that served", calls)
	}
	if result.AuthID != calls[1] || !slices.Equal(result.FailedAuthIDs, []string{calls[0]}) {
		t.Fatalf("AuthID = %q, FailedAuthIDs = %v; want the serving account %q and the refused one %q", result.AuthID, result.FailedAuthIDs, calls[1], calls[0])
	}

	// The refused account is cooling now; the next call goes straight to the other one.
	next, err := mgr.ExecuteStream(context.Background(), []string{"flaky"}, req, opts)
	if err != nil {
		t.Fatalf("second ExecuteStream: %v", err)
	}
	if next.AuthID != calls[1] || len(next.FailedAuthIDs) != 0 {
		t.Fatalf("AuthID = %q, FailedAuthIDs = %v; want %q and no failures", next.AuthID, next.FailedAuthIDs, calls[1])
	}
}
