package routetrace

import (
	"context"
	"time"
)

// Why a request turned to a member of a virtual model, or passed it over.
// The panel keys its wording on these values: add new ones, never rename.
const (
	// MemberFirst is the first member in the listed order.
	MemberFirst = "first"
	// MemberWeighted is the first member, picked by weight.
	MemberWeighted = "weighted"
	// MemberFallback is the next member after the one before it failed.
	MemberFallback = "fallback"
	// MemberNotAllowed: the key or the request's route may not use the
	// member's channel group, or the group does not serve its model.
	MemberNotAllowed = "not_allowed"
	// MemberDisabled: the member's model is switched off in the model catalog.
	MemberDisabled = "disabled"
	// MemberNoProvider: no account the request may use serves the member's
	// model.
	MemberNoProvider = "no_provider"
)

// MemberInfo describes a member step.
type MemberInfo struct {
	VirtualModel string
	// Member is the member's 1-based position among Members.
	Member  int
	Members int
	Group   string
	Model   string
	// Reason is a Member* value.
	Reason string
	// Status, Cause and Error describe the failure of the member tried before
	// a MemberFallback, or why a member was passed over.
	Status int
	Cause  string
	Error  string
}

// Member records a member of a virtual model. A member the request turns to
// (MemberFirst, MemberWeighted, MemberFallback) is the scheduler's next
// decision for the attempts still waiting on one: their rows say the request
// moved on to this member. A member passed over settles nothing.
func (r *Recorder) Member(info MemberInfo) {
	if r == nil {
		return
	}
	now := time.Now()
	step := &Step{
		Kind:         KindMember,
		At:           stamp(now),
		VirtualModel: info.VirtualModel,
		Member:       info.Member,
		Members:      info.Members,
		Group:        info.Group,
		Model:        info.Model,
		Reason:       info.Reason,
		Status:       info.Status,
		Cause:        info.Cause,
		Error:        truncateError(info.Error),
	}
	r.mu.Lock()
	r.appendLocked(step)
	var held []func()
	switch info.Reason {
	case MemberFirst, MemberWeighted, MemberFallback:
		held = r.settlePendingLocked()
		r.setStageLocked(StagePreparing, now)
	}
	r.mu.Unlock()
	runHeld(held)
}

type callerDecidesKey struct{}

// WithCallerDecides returns ctx telling the scheduler that its caller takes
// the decision after a pass that fails: a virtual model may still turn to its
// next member. The scheduler then leaves its last failed attempt waiting when
// it returns, rather than settling it, so that attempt's row names where the
// request went next. The caller settles it (Member or SettlePending), and
// Close does when the request ends, so the row is never lost.
func WithCallerDecides(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callerDecidesKey{}, true)
}

// CallerDecides reports whether ctx came from WithCallerDecides.
func CallerDecides(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	decides, _ := ctx.Value(callerDecidesKey{}).(bool)
	return decides
}
