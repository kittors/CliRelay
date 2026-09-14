package auth

import (
	"testing"
	"time"
)

// The message ChatGPT actually sends when it sheds load. It arrives inside a 200
// SSE stream, and the executor maps it to 502 because no other status fits.
const overloadedUpstreamBody = `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"}}`

func TestUpstreamOverloadIsRecognisedAsTransient(t *testing.T) {
	overloads := []*Error{
		{Message: overloadedUpstreamBody},
		{Code: "server_is_overloaded"},
		{Message: "Our servers are currently overloaded. Please try again later."},
		{Code: "", Message: `{"error":{"type":"service_unavailable_error"}}`},
	}
	for _, err := range overloads {
		if !isTransientUpstreamOverloadError(err) {
			t.Fatalf("%+v names an overload and should back off briefly", err)
		}
	}
}

// Anything that does not name an overload keeps the one-minute cooldown, so the
// change can only shorten the cases it matches.
func TestNonOverloadFailuresStayOnTheLongCooldown(t *testing.T) {
	others := []*Error{
		{Message: "upstream exploded"},
		{Message: ""},
		{Code: "internal_server_error", Message: "internal error"},
		{Message: `{"error":{"code":"bad_gateway"}}`},
	}
	for _, err := range others {
		if isTransientUpstreamOverloadError(err) {
			t.Fatalf("%+v must keep the long cooldown", err)
		}
	}
	if isTransientUpstreamOverloadError(nil) {
		t.Fatal("nil error must not be classified as an overload")
	}
}

// A load-shed says the provider is busy, not that this credential is broken.
// Parking it for a minute is what let one blip empty a small pool.
func TestOverloadBacksOffSecondsNotMinutes(t *testing.T) {
	auth := &Auth{ID: "a"}
	now := time.Now().UTC()
	effects := &resultStateEffects{}

	cooldownService{}.applyModelFailureLocked(auth, Result{
		Model: "gpt-5.6-luna",
		Error: &Error{HTTPStatus: 502, Message: overloadedUpstreamBody},
	}, now, effects)

	state := auth.ModelStates["gpt-5.6-luna"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if got := state.NextRetryAfter.Sub(now); got != transientUpstreamOverloadCooldown {
		t.Fatalf("NextRetryAfter = %v after now, want %v", got, transientUpstreamOverloadCooldown)
	}
	if effects.shouldSuspendModel {
		t.Fatal("an overload must not suspend the model")
	}
}

// A 502 that is not an overload keeps the previous minute-long backoff.
func TestGenericBadGatewayKeepsMinuteCooldown(t *testing.T) {
	auth := &Auth{ID: "a"}
	now := time.Now().UTC()

	cooldownService{}.applyModelFailureLocked(auth, Result{
		Model: "gpt-5.6-luna",
		Error: &Error{HTTPStatus: 502, Message: "upstream exploded"},
	}, now, &resultStateEffects{})

	state := auth.ModelStates["gpt-5.6-luna"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if got := state.NextRetryAfter.Sub(now); got != time.Minute {
		t.Fatalf("NextRetryAfter = %v after now, want %v", got, time.Minute)
	}
}
