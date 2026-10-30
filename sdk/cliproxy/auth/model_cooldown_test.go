package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

type updateRecordingHook struct {
	NoopHook
	mu      sync.Mutex
	updated []string
}

func (h *updateRecordingHook) OnAuthUpdated(_ context.Context, auth *Auth) {
	h.mu.Lock()
	h.updated = append(h.updated, auth.ID)
	h.mu.Unlock()
}

func (h *updateRecordingHook) updates() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.updated)
}

// ownStateBlocks reports whether the model's own state holds it back, leaving
// out the block the account as a whole may carry for another model's quota.
func ownStateBlocks(auth *Auth, model string) bool {
	own := &Auth{ID: auth.ID, Provider: auth.Provider, Status: StatusActive, ModelStates: map[string]*ModelState{model: auth.ModelStates[model]}}
	blocked, _, _ := isAuthBlockedForModel(own, model, time.Now())
	return blocked
}

// cooledRelay registers a relay channel with one throttled, one over-quota and
// one overloaded model, the mix an operator sees on a busy account.
func cooledRelay(t *testing.T, m *Manager) {
	t.Helper()
	registerAuth(t, m, channelAuth("relay", ""))
	fail(m, "relay", "throttled", relayRateLimit)
	fail(m, "relay", "quota", &Error{HTTPStatus: 429, Message: "daily quota exceeded"})
	fail(m, "relay", "overloaded", &Error{HTTPStatus: 503, Message: "unavailable"})
}

func TestClearModelCooldownReleasesOnlyThatModel(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	cooledRelay(t, m)
	before, _ := m.GetByID("relay")
	overloadedUntil := before.ModelStates["overloaded"].NextRetryAfter
	if overloadedUntil.IsZero() {
		t.Fatal("setup: the overloaded model was not parked")
	}

	changed, err := m.ClearModelCooldown(context.Background(), "relay", "throttled")
	if err != nil || !changed {
		t.Fatalf("ClearModelCooldown = %v, %v; want true, nil", changed, err)
	}

	got, _ := m.GetByID("relay")
	state := got.ModelStates["throttled"]
	if state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil || state.Quota != (QuotaState{}) || state.Status != StatusActive || state.StatusMessage != "" {
		t.Fatalf("the model was not fully released: %+v", state)
	}
	if ownStateBlocks(got, "throttled") {
		t.Fatal("the released model is still blocked for the scheduler")
	}
	// The over-quota model still blocks the whole account through the aggregate,
	// which is the scheduler's rule and not something a per-model release undoes.
	if !got.Quota.Exceeded {
		t.Fatal("the account lost the block of its over-quota model")
	}
	// Every other model keeps what it had: this is not ClearQuotaStatus.
	if got.ModelStates["overloaded"].NextRetryAfter != overloadedUntil || !got.ModelStates["overloaded"].Unavailable {
		t.Fatalf("another model was touched: %+v", got.ModelStates["overloaded"])
	}
	if !got.ModelStates["quota"].Quota.Exceeded {
		t.Fatal("the over-quota model lost its quota state")
	}
}

// One model's quota blocks the whole account (the aggregate), so releasing it is
// what lets the account's other models run again, while releasing a model that
// is not the reason must not.
func TestClearModelCooldownRecomputesTheAccountAggregate(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	fail(m, "relay", "quota", &Error{HTTPStatus: 429, Message: "daily quota exceeded"})
	fail(m, "relay", "overloaded", &Error{HTTPStatus: 503, Message: "unavailable"})
	got, _ := m.GetByID("relay")
	if !got.Quota.Exceeded {
		t.Fatal("setup: the quota model did not mark the account")
	}

	// Releasing the model that did not cause the account block changes nothing there.
	if _, err := m.ClearModelCooldown(context.Background(), "relay", "overloaded"); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID("relay")
	if !got.Quota.Exceeded {
		t.Fatal("releasing an unrelated model lifted the account's quota block")
	}

	// Releasing the quota model lifts it.
	if changed, err := m.ClearModelCooldown(context.Background(), "relay", "quota"); err != nil || !changed {
		t.Fatalf("ClearModelCooldown = %v, %v", changed, err)
	}
	got, _ = m.GetByID("relay")
	if got.Quota.Exceeded || !got.NextRetryAfter.IsZero() || got.Unavailable {
		t.Fatalf("the account is still blocked after its only quota model was released: quota=%+v next=%v unavailable=%v", got.Quota, got.NextRetryAfter, got.Unavailable)
	}
	if blockedFor(t, m, "relay", "anything") {
		t.Fatal("the account still blocks other models")
	}
}

func TestClearModelCooldownReleasesTheAccountErrorItLeftBehind(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	t.Run("the only failing model", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", ""))
		fail(m, "relay", "throttled", relayRateLimit)
		got, _ := m.GetByID("relay")
		if got.LastError == nil || got.Status != StatusError {
			t.Fatalf("setup: the failure left no account error: status=%s err=%v", got.Status, got.LastError)
		}
		if _, err := m.ClearModelCooldown(context.Background(), "relay", "throttled"); err != nil {
			t.Fatal(err)
		}
		got, _ = m.GetByID("relay")
		if got.LastError != nil || got.Status != StatusActive || got.StatusMessage != "" {
			t.Fatalf("an error badge is left on an account with nothing wrong: status=%s msg=%q err=%v", got.Status, got.StatusMessage, got.LastError)
		}
	})
	t.Run("another model still failing", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", ""))
		fail(m, "relay", "overloaded", &Error{HTTPStatus: 503, Message: "unavailable"})
		fail(m, "relay", "throttled", relayRateLimit)
		if _, err := m.ClearModelCooldown(context.Background(), "relay", "throttled"); err != nil {
			t.Fatal(err)
		}
		got, _ := m.GetByID("relay")
		if got.Status != StatusError {
			t.Fatalf("the account's error was dropped while another model still fails: status=%s", got.Status)
		}
	})
}

func TestClearModelCooldownResolvesTheStateKey(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	// Requests carry the thinking suffix; the panel and the selector fall back to
	// the base name, so the operator clicks the base name.
	fail(m, "relay", "claude-sonnet-4-5(8192)", relayRateLimit)
	if changed, err := m.ClearModelCooldown(context.Background(), "relay", "claude-sonnet-4-5"); err != nil || !changed {
		t.Fatalf("base name did not find the suffixed state: %v, %v", changed, err)
	}
	got, _ := m.GetByID("relay")
	if state := got.ModelStates["claude-sonnet-4-5(8192)"]; state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("suffixed state not released: %+v", state)
	}
	// Exact names win over the base name.
	fail(m, "relay", "glm-5", relayRateLimit)
	fail(m, "relay", "glm-5(high)", relayRateLimit)
	if _, err := m.ClearModelCooldown(context.Background(), "relay", "glm-5(high)"); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID("relay")
	if got.ModelStates["glm-5(high)"].Unavailable || !got.ModelStates["glm-5"].Unavailable {
		t.Fatalf("an exact name released the wrong state: base=%+v suffixed=%+v", got.ModelStates["glm-5"], got.ModelStates["glm-5(high)"])
	}
}

func TestClearModelCooldownReportsNothingToDoWithoutChanging(t *testing.T) {
	store := &trackingStore{}
	hook := &updateRecordingHook{}
	m := NewManager(store, nil, hook)
	// A credential-backed account, so a release is written to the store.
	registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
	m.MarkResult(context.Background(), Result{AuthID: "codex-1", Provider: "codex", Model: "gpt-5-codex",
		Error: &Error{HTTPStatus: 503, Message: "unavailable"}})
	savesBefore, updatesBefore := store.saveCount.Load(), hook.updates()
	if changed, err := m.ClearModelCooldown(context.Background(), "codex-1", "gpt-5-codex"); err != nil || !changed {
		t.Fatalf("ClearModelCooldown = %v, %v; want true, nil", changed, err)
	}
	saves, updates := store.saveCount.Load(), hook.updates()
	if saves <= savesBefore || updates <= updatesBefore {
		t.Fatalf("a release was not persisted and announced: saves %d->%d updates %d->%d", savesBefore, saves, updatesBefore, updates)
	}

	cases := []struct {
		name, id, model string
	}{
		{"already released", "codex-1", "gpt-5-codex"},
		{"model never seen", "codex-1", "never-used"},
		{"unknown account", "nobody", "gpt-5-codex"},
		{"blank account", "  ", "gpt-5-codex"},
		{"blank model", "codex-1", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := m.ClearModelCooldown(context.Background(), tc.id, tc.model)
			if err != nil || changed {
				t.Fatalf("ClearModelCooldown = %v, %v; want false, nil", changed, err)
			}
		})
	}
	if store.saveCount.Load() != saves || hook.updates() != updates {
		t.Fatalf("a no-op was persisted or announced: saves %d->%d updates %d->%d", saves, store.saveCount.Load(), updates, hook.updates())
	}

	var nilManager *Manager
	if changed, err := nilManager.ClearModelCooldown(context.Background(), "codex-1", "gpt-5-codex"); changed || err != nil {
		t.Fatalf("nil manager = %v, %v", changed, err)
	}
}

// The registry suspended the model when it failed; a release that left it so
// would keep the model out of the routing table.
func TestClearModelCooldownResumesTheModelInTheRegistry(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registry := &resumeRecordingRegistry{}
	m.SetModelRegistry(registry)
	cooledRelay(t, m)
	if _, err := m.ClearModelCooldown(context.Background(), "relay", "quota"); err != nil {
		t.Fatal(err)
	}
	if got, want := registry.resumedModels(), []string{"relay/quota"}; !equalStrings(got, want) {
		t.Fatalf("resumed models = %v, want %v", got, want)
	}
}

func TestClearModelCooldownKeepsADisabledModelDisabled(t *testing.T) {
	m := NewManager(nil, nil, nil)
	registry := &resumeRecordingRegistry{}
	m.SetModelRegistry(registry)
	registerAuth(t, m, &Auth{
		ID: "relay", Provider: "openai-compatibility", Status: StatusActive,
		Attributes: map[string]string{"base_url": "https://relay.example/v1"},
		ModelStates: map[string]*ModelState{
			"retired": {Status: StatusDisabled, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), LastError: &Error{HTTPStatus: 503}},
		},
	})
	if _, err := m.ClearModelCooldown(context.Background(), "relay", "retired"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("relay")
	if got.ModelStates["retired"].Status != StatusDisabled {
		t.Fatalf("an operator-disabled model was re-enabled: %+v", got.ModelStates["retired"])
	}
	if resumed := registry.resumedModels(); len(resumed) != 0 {
		t.Fatalf("a disabled model was resumed in the registry: %v", resumed)
	}
}

// Releasing a model changes no credential and no refresh schedule: if the
// upstream still refuses it, the next failure starts a new cooldown.
func TestClearModelCooldownLeavesCredentialsAndRefreshScheduleAlone(t *testing.T) {
	m := NewManager(nil, nil, nil)
	account := codexOAuthAccount("codex-1", "stable-refresh-token")
	account.NextRefreshAfter = time.Now().Add(10 * time.Minute)
	registerAuth(t, m, account)
	codex401(m, "codex-1", "gpt-5-codex", `{"error":{"code":"token_revoked"}}`)
	got, _ := m.GetByID("codex-1")
	wantRefresh := got.NextRefreshAfter

	if changed, err := m.ClearModelCooldown(context.Background(), "codex-1", "gpt-5-codex"); err != nil || !changed {
		t.Fatalf("ClearModelCooldown = %v, %v", changed, err)
	}
	got, _ = m.GetByID("codex-1")
	if !got.NextRefreshAfter.Equal(wantRefresh) || got.Metadata["refresh_token"] != "stable-refresh-token" || got.Metadata["access_token"] != "old-access-token" {
		t.Fatalf("credential or refresh schedule changed: next=%v meta=%#v", got.NextRefreshAfter, got.Metadata)
	}

	// And the next failure puts the cooldown back.
	before := time.Now()
	codex401(m, "codex-1", "gpt-5-codex", `{"error":{"code":"token_revoked"}}`)
	got, _ = m.GetByID("codex-1")
	if wait := got.ModelStates["gpt-5-codex"].NextRetryAfter.Sub(before); wait < codexOAuthReauthCooldown-time.Second {
		t.Fatalf("the next failure did not start a new cooldown: %v", wait)
	}
}
