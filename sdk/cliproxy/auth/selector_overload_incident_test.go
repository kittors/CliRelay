package auth

import (
	"testing"
	"time"
)

// Replay of the 2026-09-14 15:39 production incident: a two-account pool where
// both accounts draw an upstream load-shed two seconds apart, followed by the
// client retrying once a second. Before the fix every retry in the next minute
// collected an instant 503; twenty of them reached users.
func TestReplayProductionIncidentProducesNo503(t *testing.T) {
	model := "gpt-5.6-luna"
	base := time.Date(2026, 9, 14, 15, 39, 0, 0, time.UTC)
	overload := &Error{HTTPStatus: 502, Message: overloadedUpstreamBody}

	a := &Auth{ID: "vk3971570"}
	b := &Auth{ID: "monagasevinsjh249"}
	pool := []*Auth{a, b}

	cooldownService{}.applyModelFailureLocked(a, Result{Model: model, Error: overload}, base.Add(9*time.Second), &resultStateEffects{})
	cooldownService{}.applyModelFailureLocked(b, Result{Model: model, Error: overload}, base.Add(11*time.Second), &resultStateEffects{})

	got503 := 0
	for second := 13; second <= 33; second++ {
		if _, err := getAvailableAuths(pool, "codex", model, base.Add(time.Duration(second)*time.Second)); err != nil {
			got503++
		}
	}
	if got503 != 0 {
		t.Fatalf("%d of 21 retries still got 503", got503)
	}
}
