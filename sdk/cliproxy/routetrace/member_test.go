package routetrace

import (
	"context"
	"testing"
)

func memberInfo(member int, reason string) MemberInfo {
	return MemberInfo{VirtualModel: "smart", Member: member, Members: 3, Group: "team", Model: "gpt-x", Reason: reason}
}

// Turning to a member is the next decision for the attempt that failed on the
// member before it: its row is published then, and shows where the request
// went. A member passed over decides nothing.
func TestMemberStepSettlesTheFailedAttemptBeforeIt(t *testing.T) {
	r := New()
	held := &heldPublish{rec: r}
	r.Member(memberInfo(1, MemberFirst))
	first := r.BeginAttempt(info("a"))
	DeferPublish(first.Context(context.Background()), held.fn())
	first.Finish(Outcome{Outcome: OutcomeFailed, Status: 503})

	r.Member(MemberInfo{VirtualModel: "smart", Member: 2, Members: 3, Model: "other", Reason: MemberNoProvider, Error: "no account"})
	if held.runs() != 0 {
		t.Fatal("a member passed over must not settle the failed attempt")
	}
	fallback := memberInfo(3, MemberFallback)
	fallback.Status, fallback.Cause, fallback.Error = 503, "server_error", "upstream broke"
	r.Member(fallback)
	if held.runs() != 1 {
		t.Fatalf("records published %d times, want once when the next member is chosen", held.runs())
	}
	steps := held.seen().Steps
	if len(steps) != 4 {
		t.Fatalf("steps = %+v, want first member, attempt, passed over, fallback", steps)
	}
	if got := steps[0]; got.Kind != KindMember || got.Reason != MemberFirst || got.Member != 1 || got.Members != 3 || got.VirtualModel != "smart" {
		t.Fatalf("first member step = %+v", got)
	}
	if got := steps[2]; got.Reason != MemberNoProvider || got.Error != "no account" || got.Group != "" {
		t.Fatalf("passed-over step = %+v", got)
	}
	if got := steps[3]; got.Reason != MemberFallback || got.Status != 503 || got.Cause != "server_error" || got.Error != "upstream broke" {
		t.Fatalf("fallback step = %+v", got)
	}
	if live := r.Live(); live.Stage != StagePreparing {
		t.Fatalf("stage = %q, want preparing between two members", live.Stage)
	}
}

func TestMemberOnNilRecorderIsANoOp(t *testing.T) {
	var r *Recorder
	r.Member(memberInfo(1, MemberFirst))
	if CallerDecides(context.Background()) {
		t.Fatal("a context nobody marked must leave the decision to the scheduler")
	}
	if !CallerDecides(WithCallerDecides(context.Background())) {
		t.Fatal("WithCallerDecides must be seen")
	}
}
