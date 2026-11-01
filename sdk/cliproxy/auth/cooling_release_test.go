package auth

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// setGlobalCoolingDisabled flips the process-wide disable-cooling switch for one
// test. Tests that call it must not run in parallel.
func setGlobalCoolingDisabled(t *testing.T, disabled bool) {
	t.Helper()
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(disabled)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })
}

type resumeRecordingRegistry struct {
	mu      sync.Mutex
	resumed []string
}

func (r *resumeRecordingRegistry) ClearModelQuotaExceeded(string, string)    {}
func (r *resumeRecordingRegistry) SetModelQuotaExceeded(string, string)      {}
func (r *resumeRecordingRegistry) SuspendClientModel(string, string, string) {}
func (r *resumeRecordingRegistry) ResumeClientModel(clientID, modelID string) {
	r.mu.Lock()
	r.resumed = append(r.resumed, clientID+"/"+modelID)
	r.mu.Unlock()
}
func (r *resumeRecordingRegistry) ClientSupportsModel(string, string) bool { return true }
func (r *resumeRecordingRegistry) GetModelsForClient(string) []*sdkmodelcatalog.ModelInfo {
	return nil
}

func (r *resumeRecordingRegistry) resumedModels() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.resumed...)
	sort.Strings(out)
	return out
}

func channelAuth(id string, override string) *Auth {
	attrs := map[string]string{"base_url": "https://relay.example/v1"}
	if override != "" {
		attrs[DisableCoolingAttributeKey] = override
	}
	return &Auth{ID: id, Provider: "openai-compatibility", Status: StatusActive, Attributes: attrs}
}

func registerAuth(t *testing.T, m *Manager, auth *Auth) {
	t.Helper()
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register %s: %v", auth.ID, err)
	}
}

func fail(m *Manager, authID, model string, err *Error) {
	m.MarkResult(context.Background(), Result{AuthID: authID, Provider: "openai-compatibility", Model: model, Error: err})
}

func blockedFor(t *testing.T, m *Manager, authID, model string) bool {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", authID)
	}
	blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now())
	return blocked
}

// A throttling 429 from a third-party relay: the body names a rate limit and no
// Retry-After comes with it.
var relayRateLimit = &Error{HTTPStatus: 429, Message: "Rate limit reached for requests, please try again later"}

func TestDisableCoolingOverridePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		auth   *Auth
		want   bool
		wantOK bool
	}{
		{name: "nothing set", auth: &Auth{}},
		{name: "channel turns cooling off", auth: &Auth{Attributes: map[string]string{DisableCoolingAttributeKey: "true"}}, want: true, wantOK: true},
		{name: "channel keeps cooling on", auth: &Auth{Attributes: map[string]string{DisableCoolingAttributeKey: "false"}}, want: false, wantOK: true},
		{
			name: "auth file wins over the channel",
			auth: &Auth{
				Metadata:   map[string]any{"disable_cooling": false},
				Attributes: map[string]string{DisableCoolingAttributeKey: "true"},
			},
			want: false, wantOK: true,
		},
		{name: "legacy auth file key", auth: &Auth{Metadata: map[string]any{"disable-cooling": "true"}}, want: true, wantOK: true},
		{name: "unreadable channel value is ignored", auth: &Auth{Attributes: map[string]string{DisableCoolingAttributeKey: "sometimes"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.auth.DisableCoolingOverride()
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("DisableCoolingOverride = %t, %t; want %t, %t", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The scenario the switch exists for: a relay that answers 429 often. Whether
// the channel gets parked has to follow the channel's own setting, in both
// directions, and fall back to the global switch only when it has none.
func TestChannelDisableCoolingDecidesWhetherA429ParksIt(t *testing.T) {
	cases := []struct {
		name        string
		global      bool
		override    string
		wantBlocked bool
	}{
		{name: "channel turns cooling off", global: false, override: "true", wantBlocked: false},
		{name: "channel keeps cooling on against the global switch", global: true, override: "false", wantBlocked: true},
		{name: "channel follows the global switch", global: true, override: "", wantBlocked: false},
		{name: "nothing set keeps cooling", global: false, override: "", wantBlocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGlobalCoolingDisabled(t, tc.global)
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", tc.override))
			fail(m, "relay", "glm-5", relayRateLimit)
			// A 429 the cooldown code does not recognise as throttling takes
			// the quota path with its backoff; the switch governs it too.
			fail(m, "relay", "kimi-k2", &Error{HTTPStatus: 429, Message: "当前分组上游负载已饱和，请稍后再试"})
			fail(m, "relay", "qwen-3", &Error{HTTPStatus: 503, Message: "upstream unavailable"})
			for _, model := range []string{"glm-5", "kimi-k2", "qwen-3"} {
				if got := blockedFor(t, m, "relay", model); got != tc.wantBlocked {
					t.Fatalf("%s blocked = %t, want %t", model, got, tc.wantBlocked)
				}
			}
		})
	}
}

// Switching a channel's cooling off has to free it right away: the cooldowns
// recorded under the old setting would otherwise keep it parked after the
// operator flipped the switch. Only what the switch governs is released.
func TestUpdateReleasesCooldownsWhenChannelTurnsCoolingOff(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registry := &resumeRecordingRegistry{}
	m.SetModelRegistry(registry)
	registerAuth(t, m, channelAuth("relay", ""))

	tenMinutes := 10 * time.Minute
	fail(m, "relay", "throttled", relayRateLimit)
	m.MarkResult(context.Background(), Result{
		AuthID: "relay", Provider: "openai-compatibility", Model: "retry-after",
		Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &tenMinutes,
	})
	fail(m, "relay", "overloaded", &Error{HTTPStatus: 503, Message: "upstream unavailable"})
	// A 401 holds back every model of its key, so it gets a key of its own.
	registerAuth(t, m, channelAuth("revoked", ""))
	fail(m, "revoked", "glm-5", &Error{HTTPStatus: 401, Message: "invalid api key"})
	for _, model := range []string{"throttled", "retry-after", "overloaded"} {
		if !blockedFor(t, m, "relay", model) {
			t.Fatalf("%s not blocked before the switch", model)
		}
	}
	if !blockedFor(t, m, "revoked", "glm-5") {
		t.Fatal("the revoked key is not blocked before the switch")
	}

	// The config sync re-synthesizes the credentials with the new override.
	for _, id := range []string{"relay", "revoked"} {
		if _, err := m.Update(context.Background(), channelAuth(id, "true")); err != nil {
			t.Fatalf("update %s: %v", id, err)
		}
	}

	for model, wantBlocked := range map[string]bool{
		"throttled":   false,
		"retry-after": false,
		"overloaded":  false,
	} {
		if got := blockedFor(t, m, "relay", model); got != wantBlocked {
			t.Fatalf("%s blocked = %t after the switch, want %t", model, got, wantBlocked)
		}
	}
	// A 401 parks the key whatever the switch says.
	if !blockedFor(t, m, "revoked", "glm-5") {
		t.Fatal("the switch released a revoked key")
	}
	want := []string{"relay/overloaded", "relay/retry-after", "relay/throttled"}
	if got := registry.resumedModels(); !equalStrings(got, want) {
		t.Fatalf("resumed models = %v, want %v", got, want)
	}

	// The switch keeps working for what comes next: a new 429 does not park it.
	fail(m, "relay", "throttled", relayRateLimit)
	if blockedFor(t, m, "relay", "throttled") {
		t.Fatal("a 429 after the switch parked the channel")
	}
}

// A quota window the upstream announced (a Codex or Claude OAuth usage limit)
// carries its own reset time, which cooling never overrode. It stays, and as an
// account-wide quota it keeps every model of the credential parked.
func TestUpdateKeepsAnnouncedQuotaWindow(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	fail(m, "relay", "glm-5", &Error{HTTPStatus: 429, Message: "usage limit reached", QuotaWindow: "5h", QuotaWindowMinutes: 300})
	if !blockedFor(t, m, "relay", "glm-5") {
		t.Fatal("announced quota window did not park the credential")
	}
	if _, err := m.Update(context.Background(), channelAuth("relay", "true")); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, model := range []string{"glm-5", "kimi-k2"} {
		if !blockedFor(t, m, "relay", model) {
			t.Fatalf("%s released although the upstream announced the window", model)
		}
	}
}

// An update that leaves cooling off, like any config reload of an unchanged
// channel, must not wipe the state: a Retry-After the upstream sent is waited
// out even with cooling off.
func TestUpdateKeepsCooldownsWhenCoolingWasAlreadyOff(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", "true"))
	tenMinutes := 10 * time.Minute
	m.MarkResult(context.Background(), Result{
		AuthID: "relay", Provider: "openai-compatibility", Model: "glm-5",
		Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &tenMinutes,
	})
	if !blockedFor(t, m, "relay", "glm-5") {
		t.Fatal("Retry-After was not honoured with cooling off")
	}
	if _, err := m.Update(context.Background(), channelAuth("relay", "true")); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !blockedFor(t, m, "relay", "glm-5") {
		t.Fatal("an update that kept cooling off released the Retry-After cooldown")
	}
}

// A failure without a model parks the credential as a whole; the release
// covers that state too.
func TestUpdateReleasesAccountLevelCooldown(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	tenMinutes := 10 * time.Minute
	m.MarkResult(context.Background(), Result{
		AuthID: "relay", Provider: "openai-compatibility",
		Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &tenMinutes,
	})
	if !blockedFor(t, m, "relay", "") {
		t.Fatal("account-level 429 did not park the credential")
	}
	if _, err := m.Update(context.Background(), channelAuth("relay", "true")); err != nil {
		t.Fatalf("update: %v", err)
	}
	if blockedFor(t, m, "relay", "") {
		t.Fatal("account-level cooldown survived switching cooling off")
	}
	got, _ := m.GetByID("relay")
	if got.Status != StatusActive || got.LastError != nil || got.Quota.Exceeded {
		t.Fatalf("status = %s, last error = %v, quota = %+v; want a clean account", got.Status, got.LastError, got.Quota)
	}
}

// Turning the global switch on releases every credential that follows it, and
// leaves alone a channel that keeps cooling on by its own setting.
func TestReleaseCoolingForDisabledAuths(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registry := &resumeRecordingRegistry{}
	m.SetModelRegistry(registry)
	registerAuth(t, m, channelAuth("follows-global", ""))
	registerAuth(t, m, channelAuth("keeps-cooling", "false"))
	fail(m, "follows-global", "glm-5", relayRateLimit)
	fail(m, "keeps-cooling", "glm-5", relayRateLimit)

	quotaCooldownDisabled.Store(true)
	if released := m.ReleaseCoolingForDisabledAuths(); released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	if blockedFor(t, m, "follows-global", "glm-5") {
		t.Fatal("credential following the global switch is still parked")
	}
	if !blockedFor(t, m, "keeps-cooling", "glm-5") {
		t.Fatal("channel that keeps cooling on was released")
	}
	if got := registry.resumedModels(); !equalStrings(got, []string{"follows-global/glm-5"}) {
		t.Fatalf("resumed models = %v", got)
	}
	if released := m.ReleaseCoolingForDisabledAuths(); released != 0 {
		t.Fatalf("second release = %d, want 0", released)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
