package auth

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// A caller that still has somewhere else to go (a virtual model's next
// member) hears at once that every account is cooling, instead of waiting for
// one: the wait would only delay the member that can serve now.
func TestCallerThatDecidesIsNotKeptBusyWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "cd-a", "cd-b")
		park(mgr, "cd-a", upstreamDown, 15*time.Second)
		park(mgr, "cd-b", throttled, 40*time.Second)

		start := time.Now()
		err := execute(mgr, routetrace.WithCallerDecides(context.Background()))
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s, want no wait for a caller that decides", waited)
		}
		if err == nil || statusCodeFromError(err) == http.StatusOK {
			t.Fatalf("err = %v, want the cooling pool's refusal", err)
		}
		if calls := exec.Calls(); len(calls) != 0 {
			t.Fatalf("calls = %v, want none while every account cools", calls)
		}

		// The same request from a caller with nowhere else to go still waits.
		start = time.Now()
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if waited := time.Since(start); waited < 15*time.Second {
			t.Fatalf("waited %s, want the 15s until cd-a recovers", waited)
		}
	})
}

// The last failed attempt of a caller that decides waits for the caller's
// next decision: its record goes out when the caller settles it, so the row
// can say where the request went next.
func TestCallerThatDecidesSettlesTheLastFailedAttempt(t *testing.T) {
	exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{script: []error{upstreamDown}}}
	mgr, ctx := newPublishingManager(t, exec, "cd-only")

	if err := execute(mgr, routetrace.WithCallerDecides(ctx)); err == nil {
		t.Fatal("Execute succeeded, want the upstream failure")
	}
	if rows := exec.Rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want the failed attempt held for the caller", rows)
	}
	exec.rec.SettlePending()
	rows := exec.Rows()
	if len(rows) != 1 || rows[0].authID != "cd-only" {
		t.Fatalf("rows = %+v, want the failed attempt once the caller settled it", rows)
	}

	// Without the mark the scheduler settles it on its way out.
	exec2 := &publishingExecutor{scriptedExecutor: &scriptedExecutor{script: []error{upstreamDown}}}
	mgr2, ctx2 := newPublishingManager(t, exec2, "cd-plain")
	if err := execute(mgr2, ctx2); err == nil {
		t.Fatal("Execute succeeded, want the upstream failure")
	}
	if rows := exec2.Rows(); len(rows) != 1 {
		t.Fatalf("rows = %+v, want the failed attempt published on return", rows)
	}
}

func TestFailureCauseAndRequestFault(t *testing.T) {
	if FailureCause(nil) != "" || IsRequestFault(nil) {
		t.Fatal("no error has no cause and is no fault")
	}
	invalid := &Error{HTTPStatus: http.StatusBadRequest, Code: "invalid_request_error", Message: "invalid_request_error: messages: field required"}
	if !IsRequestFault(invalid) || FailureCause(invalid) != routetrace.CauseRequestInvalid {
		t.Fatalf("a malformed request must be the request's fault: fault=%v cause=%q", IsRequestFault(invalid), FailureCause(invalid))
	}
	blocked := &Error{Code: contentPolicyViolationCode, Message: "blocked", HTTPStatus: http.StatusForbidden}
	if !IsRequestFault(blocked) || FailureCause(blocked) != routetrace.CauseContentPolicy {
		t.Fatalf("a content-policy block must be the request's fault")
	}
	if IsRequestFault(upstreamDown) || IsRequestFault(throttled) || IsRequestFault(revoked) {
		t.Fatal("an upstream or account failure is not the request's fault")
	}
}
