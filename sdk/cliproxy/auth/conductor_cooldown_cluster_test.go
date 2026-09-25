package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type recordingRegistry struct {
	mu        sync.Mutex
	quota     []string
	suspended []string
}

func (r *recordingRegistry) ClearModelQuotaExceeded(string, string) {}
func (r *recordingRegistry) SetModelQuotaExceeded(clientID, modelID string) {
	r.mu.Lock()
	r.quota = append(r.quota, clientID+"/"+modelID)
	r.mu.Unlock()
}
func (r *recordingRegistry) SuspendClientModel(clientID, modelID, reason string) {
	r.mu.Lock()
	r.suspended = append(r.suspended, clientID+"/"+modelID+"/"+reason)
	r.mu.Unlock()
}
func (r *recordingRegistry) ResumeClientModel(string, string)                       {}
func (r *recordingRegistry) ClientSupportsModel(string, string) bool                { return true }
func (r *recordingRegistry) GetModelsForClient(string) []*sdkmodelcatalog.ModelInfo { return nil }

func cooldownTestManager(t *testing.T, authIDs ...string) (*Manager, *recordingRegistry) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	registry := &recordingRegistry{}
	m.SetModelRegistry(registry)
	for _, id := range authIDs {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	return m, registry
}

func collectNotices(m *Manager) *[]CooldownNotice {
	var mu sync.Mutex
	notices := &[]CooldownNotice{}
	m.SetCooldownPublisher(func(n CooldownNotice) {
		mu.Lock()
		*notices = append(*notices, n)
		mu.Unlock()
	})
	return notices
}

func TestMarkResultPublishesQuotaCooldown(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	notices := collectNotices(m)
	retry := 10 * time.Minute

	m.MarkResult(context.Background(), Result{
		AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", RetryAfter: &retry,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit reached, quota_exhausted"},
	})
	if len(*notices) != 1 {
		t.Fatalf("notices = %+v", *notices)
	}
	n := (*notices)[0]
	if n.AuthID != "acct-1" || n.Model != "gpt-5.5" || !n.Quota || n.Reason != "quota" || n.StatusCode != 429 {
		t.Fatalf("notice = %+v", n)
	}
	if wait := time.Until(n.Until); wait < 9*time.Minute || wait > 10*time.Minute {
		t.Fatalf("notice deadline %s away, want the upstream retry-after", wait)
	}

	// Success clears state and announces nothing; peers only ever extend.
	m.MarkResult(context.Background(), Result{AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", Success: true})
	// A failure that sets no cooldown announces nothing either.
	m.MarkResult(context.Background(), Result{AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5",
		Error: &Error{HTTPStatus: http.StatusTeapot, Message: "odd"}})
	if len(*notices) != 1 {
		t.Fatalf("only cooldowns are published, got %+v", *notices)
	}
}

func TestMarkResultPublishesAuthLevelAndBackoffCooldowns(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	notices := collectNotices(m)

	m.MarkResult(context.Background(), Result{AuthID: "acct-1", Provider: "codex",
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "bad token"}})
	m.MarkResult(context.Background(), Result{AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5",
		Error: &Error{HTTPStatus: http.StatusBadGateway, Code: "server_is_overloaded", Message: "busy"}})
	if len(*notices) != 2 {
		t.Fatalf("notices = %+v", *notices)
	}
	if n := (*notices)[0]; n.Model != "" || n.Quota || time.Until(n.Until) < 29*time.Minute {
		t.Fatalf("auth-level 401 notice = %+v", n)
	}
	if n := (*notices)[1]; n.Model != "gpt-5.5" || n.Quota || n.StatusCode != 502 || time.Until(n.Until) > 6*time.Second {
		t.Fatalf("overload backoff notice = %+v", n)
	}
}

func TestApplyPeerCooldownBlocksSelectionAndOnlyExtends(t *testing.T) {
	m, registry := cooldownTestManager(t, "acct-1")
	until := time.Now().Add(10 * time.Minute)

	if !m.ApplyPeerCooldown(CooldownNotice{AuthID: "acct-1", Model: "gpt-5.5", Until: until, StatusCode: 429, Reason: "quota", Quota: true, Origin: "node-a"}) {
		t.Fatal("first notice must apply")
	}
	auth, _ := m.GetByID("acct-1")
	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5.5", time.Now())
	if !blocked || reason != blockReasonCooldown || !next.Equal(until) {
		t.Fatalf("peer quota cooldown must block the model: blocked=%v reason=%v next=%v", blocked, reason, next)
	}
	state := auth.ModelStates["gpt-5.5"]
	if state.StatusMessage == "" || state.LastError == nil || state.LastError.Code != "peer_cooldown" {
		t.Fatalf("the cooling state must explain itself in the console: %+v", state)
	}
	if len(registry.quota) != 1 || len(registry.suspended) != 1 || registry.suspended[0] != "acct-1/gpt-5.5/quota" {
		t.Fatalf("registry marks = %+v %+v", registry.quota, registry.suspended)
	}

	if m.ApplyPeerCooldown(CooldownNotice{AuthID: "acct-1", Model: "gpt-5.5", Until: until.Add(-time.Minute), Quota: true}) {
		t.Fatal("an earlier deadline must never shorten the cooldown")
	}
	later := until.Add(5 * time.Minute)
	if !m.ApplyPeerCooldown(CooldownNotice{AuthID: "acct-1", Model: "gpt-5.5", Until: later, Quota: true}) {
		t.Fatal("a later deadline extends")
	}
	auth, _ = m.GetByID("acct-1")
	if _, _, next := isAuthBlockedForModel(auth, "gpt-5.5", time.Now()); !next.Equal(later) {
		t.Fatalf("extended deadline = %v, want %v", next, later)
	}
}

func TestApplyPeerCooldownKeepsLongerLocalCooldown(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	retry := 20 * time.Minute
	m.MarkResult(context.Background(), Result{
		AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", RetryAfter: &retry,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota_exhausted"},
	})
	before, _ := m.GetByID("acct-1")
	localUntil, _ := cooldownDeadline(before, "gpt-5.5", time.Now())

	if m.ApplyPeerCooldown(CooldownNotice{AuthID: "acct-1", Model: "gpt-5.5", Until: time.Now().Add(time.Minute), StatusCode: 502}) {
		t.Fatal("a shorter peer backoff must not touch a longer local cooldown")
	}
	after, _ := m.GetByID("acct-1")
	if got, _ := cooldownDeadline(after, "gpt-5.5", time.Now()); !got.Equal(localUntil) {
		t.Fatalf("local cooldown changed from %v to %v", localUntil, got)
	}
}

func TestApplyPeerCooldownCapsLongDeadlines(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	if !m.ApplyPeerCooldown(CooldownNotice{AuthID: "acct-1", Until: time.Now().Add(7 * 24 * time.Hour), Quota: true, StatusCode: 402}) {
		t.Fatal("notice must apply")
	}
	auth, _ := m.GetByID("acct-1")
	until, quota := cooldownDeadline(auth, "", time.Now())
	if !quota || time.Until(until) > maxPeerCooldown || time.Until(until) < maxPeerCooldown-time.Minute {
		t.Fatalf("a week-long peer cooldown must be capped at %s, got %s", maxPeerCooldown, time.Until(until))
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "any", time.Now()); !blocked {
		t.Fatal("an account-wide cooldown blocks every model")
	}
}

func TestApplyPeerCooldownIgnoresUnknownDisabledAndCoolingDisabledAuths(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	until := time.Now().Add(time.Minute)
	if m.ApplyPeerCooldown(CooldownNotice{AuthID: "missing", Until: until}) {
		t.Fatal("unknown auth")
	}
	if m.ApplyPeerCooldown(CooldownNotice{AuthID: "acct-1", Until: time.Now().Add(-time.Second)}) {
		t.Fatal("an expired notice changes nothing")
	}

	if _, err := m.Register(context.Background(), &Auth{ID: "nocool", Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"disable_cooling": true}}); err != nil {
		t.Fatal(err)
	}
	if m.ApplyPeerCooldown(CooldownNotice{AuthID: "nocool", Model: "m", Until: until, StatusCode: 429, Quota: true}) {
		t.Fatal("disable_cooling switches off quota cooldowns from peers too")
	}
	if !m.ApplyPeerCooldown(CooldownNotice{AuthID: "nocool", Model: "m", Until: until, StatusCode: 401, Reason: "unauthorized"}) {
		t.Fatal("disable_cooling does not switch off suspensions")
	}
}

// TestPeerCooldownRoundTrip wires two managers the way the cluster relay does
// and checks the second stops selecting the account the first saw fail.
func TestPeerCooldownRoundTrip(t *testing.T) {
	nodeA, _ := cooldownTestManager(t, "acct-1", "acct-2")
	nodeB, _ := cooldownTestManager(t, "acct-1", "acct-2")
	nodeA.SetCooldownPublisher(func(n CooldownNotice) {
		n.Origin = "node-a"
		nodeB.ApplyPeerCooldown(n)
	})
	retry := 5 * time.Minute
	nodeA.MarkResult(context.Background(), Result{AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", RetryAfter: &retry,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota_exhausted"}})

	auths := nodeB.List()
	available, err := getAvailableAuths(auths, "codex", "gpt-5.5", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0].ID != "acct-2" {
		t.Fatalf("node b must skip the account node a saw exhausted, available = %v", authIDs(available))
	}

	// Node b must reach the same decision node a reaches, for the failing model
	// and for the others (an exhausted quota on the only model served marks the
	// whole account).
	a, _ := nodeA.GetByID("acct-1")
	b, _ := nodeB.GetByID("acct-1")
	for _, model := range []string{"gpt-5.5", "other-model"} {
		blockedA, reasonA, _ := isAuthBlockedForModel(a, model, time.Now())
		blockedB, reasonB, _ := isAuthBlockedForModel(b, model, time.Now())
		if blockedA != blockedB || reasonA != reasonB {
			t.Fatalf("%s: node a blocked=%v/%v, node b blocked=%v/%v", model, blockedA, reasonA, blockedB, reasonB)
		}
	}
}

func authIDs(auths []*Auth) []string {
	out := make([]string, 0, len(auths))
	for _, a := range auths {
		out = append(out, a.ID)
	}
	return out
}
