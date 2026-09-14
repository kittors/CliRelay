package auth

import (
	"testing"
	"time"
)

func parkedAuth(id, model string, next time.Time) *Auth {
	return &Auth{
		ID: id,
		ModelStates: map[string]*ModelState{
			model: {Status: StatusError, Unavailable: true, NextRetryAfter: next},
		},
	}
}

// Answering 503 while every credential is seconds from recovering is what turned
// one upstream blip into a burst: the reply is instant, so a retrying client just
// asks again inside the same window. Let the soonest one try instead.
func TestBriefPoolWideBackoffLetsSoonestCredentialTry(t *testing.T) {
	model := "gpt-5.6-luna"
	now := time.Now().UTC()
	auths := []*Auth{
		parkedAuth("b", model, now.Add(5*time.Second)),
		parkedAuth("a", model, now.Add(2*time.Second)),
	}

	available, err := getAvailableAuths(auths, "codex", model, now)
	if err != nil {
		t.Fatalf("getAvailableAuths() error = %v, want a candidate", err)
	}
	if len(available) != 1 {
		t.Fatalf("len(available) = %d, want 1", len(available))
	}
	if available[0].ID != "a" {
		t.Fatalf("picked %q, want the soonest-recovering %q", available[0].ID, "a")
	}
}

// A long wait is worth honouring: it follows a real fault, not a load-shed, and
// letting requests through would only burn latency on a call that will fail.
func TestLongPoolWideBackoffStillReturnsModelUnavailable(t *testing.T) {
	model := "gpt-5.6-luna"
	now := time.Now().UTC()
	auths := []*Auth{
		parkedAuth("a", model, now.Add(time.Minute)),
		parkedAuth("b", model, now.Add(2*time.Minute)),
	}

	_, err := getAvailableAuths(auths, "codex", model, now)
	if err == nil {
		t.Fatal("getAvailableAuths() error = nil, want model_unavailable")
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error = %T, want a status error", err)
	}
	if got := statusErr.StatusCode(); got != 503 {
		t.Fatalf("StatusCode() = %d, want 503", got)
	}
}

// An exhausted quota will not answer on the next attempt, so it must never be
// released early even when the reset is close.
func TestExhaustedQuotaIsNotReleasedEarly(t *testing.T) {
	model := "gpt-5.6-luna"
	now := time.Now().UTC()
	next := now.Add(2 * time.Second)
	auths := []*Auth{
		{
			ID: "a",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota:          QuotaState{Exceeded: true, NextRecoverAt: next},
				},
			},
		},
	}

	_, err := getAvailableAuths(auths, "codex", model, now)
	if err == nil {
		t.Fatal("getAvailableAuths() error = nil, want a cooldown error")
	}
	if _, ok := err.(*modelCooldownError); !ok {
		t.Fatalf("error = %T, want *modelCooldownError", err)
	}
}

// A weight-0 credential is excluded from scheduling on purpose; an empty pool is
// not a reason to start using it.
func TestOptimisticReleaseSkipsWeightZeroCredentials(t *testing.T) {
	model := "gpt-5.6-luna"
	now := time.Now().UTC()
	excluded := parkedAuth("a", model, now.Add(2*time.Second))
	excluded.Attributes = map[string]string{selectionWeightAttribute: "0"}

	if got := earliestOptimisticCandidate([]*Auth{excluded}, model, now); got != nil {
		t.Fatalf("earliestOptimisticCandidate() = %q, want nil", got.ID)
	}
}
