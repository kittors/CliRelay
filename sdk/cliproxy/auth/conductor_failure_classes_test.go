package auth

import (
	"testing"
	"time"
)

// A 401 says the credential is no good, whichever model asked: the account's
// other models, the ones it served before and the ones it has not, are held
// back with it rather than each finding out with a failed request of its own.
func TestUnauthorizedHoldsBackEveryModelOfTheKey(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	m.MarkResult(t.Context(), Result{AuthID: "relay", Provider: "openai-compatibility", Model: "glm-4", Success: true})
	before := time.Now()
	fail(m, "relay", "glm-5", &Error{HTTPStatus: 401, Message: "invalid api key"})

	for _, model := range []string{"glm-5", "glm-4", "kimi-k3"} {
		if !blockedFor(t, m, "relay", model) {
			t.Fatalf("%s is still offered after the key was rejected", model)
		}
	}
	assertParkedFor(t, m, "relay", "glm-4", before, 30*time.Minute)

	auth, _ := m.GetByID("relay")
	if got := CooldownReasonOf(auth.ModelStates["glm-4"].LastError, auth.ModelStates["glm-4"].Quota); got != CooldownReasonUnauthorized {
		t.Fatalf("glm-4 is held back as %q, want %q", got, CooldownReasonUnauthorized)
	}

	// Other keys are not affected.
	registerAuth(t, m, channelAuth("other", ""))
	if blockedFor(t, m, "other", "glm-5") {
		t.Fatal("a 401 on one key held back another")
	}
}

// A 403 may be about one model alone (a relay key not allowed to use it), so it
// stays with that model.
func TestForbiddenStaysWithItsModel(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	fail(m, "relay", "glm-5", &Error{HTTPStatus: 403, Message: "this key may not use glm-5"})
	if !blockedFor(t, m, "relay", "glm-5") {
		t.Fatal("glm-5 is still offered after a 403")
	}
	if blockedFor(t, m, "relay", "glm-4") {
		t.Fatal("a 403 on glm-5 held back glm-4")
	}
}

// Cooldowns for failures that keep coming back without a success in between
// get longer each time; a success starts over.
func TestRepeatedFailuresWaitLonger(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	for _, tc := range []struct {
		name string
		err  *Error
		want []time.Duration
	}{
		{name: "bare 404", err: &Error{HTTPStatus: 404, Message: "not found"}, want: []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute}},
		{name: "404 for a missing model", err: &Error{HTTPStatus: 404, Message: "model glm-5 does not exist"}, want: []time.Duration{12 * time.Hour, 12 * time.Hour}},
		{name: "403", err: &Error{HTTPStatus: 403, Message: "forbidden"}, want: []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour}},
		{name: "401", err: &Error{HTTPStatus: 401, Message: "invalid api key"}, want: []time.Duration{30 * time.Minute, 30 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", ""))
			for _, want := range tc.want {
				before := time.Now()
				fail(m, "relay", "glm-5", tc.err)
				assertParkedFor(t, m, "relay", "glm-5", before, want)
			}
			// A success ends the streak: the next failure starts over.
			m.MarkResult(t.Context(), Result{AuthID: "relay", Provider: "openai-compatibility", Model: "glm-5", Success: true})
			before := time.Now()
			fail(m, "relay", "glm-5", tc.err)
			assertParkedFor(t, m, "relay", "glm-5", before, tc.want[0])
		})
	}

	// A different failure in between starts the streak over too.
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	notFound := &Error{HTTPStatus: 404, Message: "not found"}
	fail(m, "relay", "glm-5", notFound)
	fail(m, "relay", "glm-5", notFound)
	fail(m, "relay", "glm-5", &Error{HTTPStatus: 503, Message: "upstream unavailable"})
	before := time.Now()
	fail(m, "relay", "glm-5", notFound)
	assertParkedFor(t, m, "relay", "glm-5", before, 10*time.Minute)
}
