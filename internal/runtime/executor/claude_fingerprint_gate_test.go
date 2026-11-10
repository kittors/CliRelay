package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func resetRuntimeIdentityFingerprintColdLoaded() {
	runtimeIdentityFingerprintColdLoaded.Range(func(key, _ any) bool {
		runtimeIdentityFingerprintColdLoaded.Delete(key)
		return true
	})
}

// clearRuntimeIdentityFingerprintMemory drops everything the process learned in
// memory, leaving storage alone: the state of a freshly restarted relay.
func clearRuntimeIdentityFingerprintMemory() {
	runtimeIdentityFingerprintCache.Lock()
	runtimeIdentityFingerprintCache.records = map[string]identityFingerprintCacheEntry{}
	runtimeIdentityFingerprintCache.Unlock()
	resetRuntimeIdentityFingerprintColdLoaded()
}

func claudeCodeHeaders(version string) http.Header {
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/"+version+" (external, cli)")
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	headers.Set("X-Stainless-Package-Version", "0.99.0")
	headers.Set("X-Stainless-Runtime-Version", "v24.4.0")
	headers.Set("X-Stainless-Timeout", "700")
	return headers
}

type claudeGateHarness struct {
	t        *testing.T
	executor *ClaudeExecutor
	auth     *cliproxyauth.Auth
	headers  []http.Header
	bodies   [][]byte
}

func newClaudeGateHarness(t *testing.T, authID string) *claudeGateHarness {
	t.Helper()
	h := &claudeGateHarness{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.headers = append(h.headers, r.Header.Clone())
		h.bodies = append(h.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(server.Close)
	h.executor = NewClaudeExecutor(&config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Claude: config.ClaudeIdentityFingerprintConfig{Enabled: true},
		},
	})
	h.auth = &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-gate",
			"base_url": server.URL,
		},
		Metadata: map[string]any{"account_uuid": authID + "-uuid"},
	}
	return h
}

// send runs one Messages request with the given inbound headers (nil means a
// request that carries no client identity) and returns the outbound User-Agent.
func (h *claudeGateHarness) send(inbound http.Header) string {
	h.t.Helper()
	if inbound == nil {
		inbound = http.Header{}
	}
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/messages", inbound)
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello gate"}]}`)
	if _, err := h.executor.Execute(ctx, h.auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		h.t.Fatalf("Execute returned error: %v", err)
	}
	return h.headers[len(h.headers)-1].Get("User-Agent")
}

func (h *claudeGateHarness) lastBillingVersion() string {
	billing := gjson.GetBytes(h.bodies[len(h.bodies)-1], "system.0.text").String()
	_, rest, ok := strings.Cut(billing, "cc_version=")
	if !ok {
		return ""
	}
	version, _, _ := strings.Cut(rest, ";")
	return version
}

func (h *claudeGateHarness) seedStoredRecord(version string) {
	h.t.Helper()
	accountKey := authSubjectKey(h.t, h.auth)
	if _, _, err := usage.ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:   identityfingerprint.ProviderClaude,
		AccountKey: accountKey,
		Headers:    claudeCodeHeaders(version),
		ObservedAt: time.Now().UTC(),
	}); err != nil {
		h.t.Fatalf("seed stored record: %v", err)
	}
}

func canonicalClaudeGateUserAgent() string {
	return config.BuildClaudeFingerprintUserAgent(config.DefaultClaudeFingerprintCLIVersion, config.DefaultClaudeFingerprintEntrypoint)
}

func TestClaudeLearnedFingerprintMeetsGate(t *testing.T) {
	gate := config.DefaultClaudeFingerprintCLIVersion
	record := func(version, ua string) *identityfingerprint.LearnedRecord {
		fields := map[string]string{}
		if version != "" {
			fields[identityfingerprint.FieldClaudeCLIVersion] = version
		}
		if ua != "" {
			fields[identityfingerprint.FieldUserAgent] = ua
		}
		return &identityfingerprint.LearnedRecord{Provider: identityfingerprint.ProviderClaude, Fields: fields}
	}
	cases := []struct {
		name    string
		learned *identityfingerprint.LearnedRecord
		want    bool
	}{
		{"nil record", nil, true},
		{"below gate", record("2.1.100", "claude-cli/2.1.100 (external, cli)"), false},
		{"at gate", record(gate, "claude-cli/"+gate+" (external, cli)"), true},
		{"above gate", record("9.0.0", "claude-cli/9.0.0 (external, cli)"), true},
		{"version only, below gate", &identityfingerprint.LearnedRecord{Version: "2.1.100", Fields: map[string]string{}}, false},
		{"current version but malformed agent", record("9.0.0", "claude-cli/9.0.0"), false},
	}
	for _, tc := range cases {
		if got := claudeLearnedFingerprintMeetsGate(tc.learned); got != tc.want {
			t.Errorf("%s: claudeLearnedFingerprintMeetsGate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A record learned from a client below the gate is not applied, and does not
// trigger the operator warning meant for pinned presets.
func TestClaudeExecutorIgnoresLearnedFingerprintBelowGate(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	h := newClaudeGateHarness(t, "claude-gate-below")
	h.seedStoredRecord("2.1.100")
	clearRuntimeIdentityFingerprintMemory()

	if got := h.send(nil); got != canonicalClaudeGateUserAgent() {
		t.Fatalf("cold request User-Agent = %q, want builtin %q instead of the stale stored record", got, canonicalClaudeGateUserAgent())
	}
	if got := h.send(claudeCodeHeaders("2.1.150")); got != canonicalClaudeGateUserAgent() {
		t.Fatalf("outdated-client User-Agent = %q, want builtin %q", got, canonicalClaudeGateUserAgent())
	}
	if got := h.lastBillingVersion(); !strings.HasPrefix(got, config.DefaultClaudeFingerprintCLIVersion+".") {
		t.Fatalf("billing cc_version = %q, want builtin %s", got, config.DefaultClaudeFingerprintCLIVersion)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "below the") {
			t.Fatalf("unexpected version-gate warning for an ignored learned record: %s", entry.Message)
		}
	}

	// A current client still takes over as soon as it is observed.
	if got := h.send(claudeCodeHeaders("2.1.300")); got != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("current-client User-Agent = %q, want learned claude-cli/2.1.300", got)
	}
}

// A fresh process must apply the stored record on its first request instead of
// the builtin template.
func TestClaudeExecutorAppliesStoredFingerprintOnColdStart(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	h := newClaudeGateHarness(t, "claude-gate-cold")
	h.seedStoredRecord("2.1.300")
	clearRuntimeIdentityFingerprintMemory()

	if got := h.send(nil); got != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("first request after restart User-Agent = %q, want stored claude-cli/2.1.300", got)
	}
	if got := h.lastBillingVersion(); !strings.HasPrefix(got, "2.1.300.") {
		t.Fatalf("first request after restart cc_version = %q, want stored 2.1.300", got)
	}
}

// On a cold cache, a request from an older (still compliant) client must merge
// into the stored record, exactly as it would on a warm one, rather than replace
// it with an identity built from that single request.
func TestClaudeExecutorColdStartMergesIntoStoredFingerprint(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	h := newClaudeGateHarness(t, "claude-gate-cold-merge")
	h.seedStoredRecord("2.1.300")

	// Warm reference: the stored record is in memory.
	clearRuntimeIdentityFingerprintMemory()
	h.send(nil)
	warm := h.send(claudeCodeHeaders("2.1.290"))

	clearRuntimeIdentityFingerprintMemory()
	cold := h.send(claudeCodeHeaders("2.1.290"))

	if warm != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("warm User-Agent = %q, want stored newer claude-cli/2.1.300", warm)
	}
	if cold != warm {
		t.Fatalf("cold User-Agent = %q, want the warm result %q", cold, warm)
	}
}

// The cold-start read happens once per account and never holds a request
// longer than the timeout; a slow store is left to the background load.
func TestRuntimeIdentityFingerprintColdStartLoadIsBoundedAndOnce(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)
	previousTimeout := identityFingerprintColdLoadTimeout
	identityFingerprintColdLoadTimeout = 20 * time.Millisecond
	t.Cleanup(func() { identityFingerprintColdLoadTimeout = previousTimeout })

	release := make(chan struct{})
	var calls atomic.Int32
	runtimeIdentityFingerprintStoreFuncMu.Lock()
	runtimeGetIdentityFingerprint = func(provider identityfingerprint.Provider, accountKey string) (*identityfingerprint.LearnedRecord, error) {
		calls.Add(1)
		<-release
		return &identityfingerprint.LearnedRecord{
			Provider:   provider,
			AccountKey: accountKey,
			ProfileKey: identityfingerprint.ProfileKeyDefault,
			Version:    "2.1.300",
			Fields: map[string]string{
				identityfingerprint.FieldClaudeCLIVersion: "2.1.300",
				identityfingerprint.FieldUserAgent:        "claude-cli/2.1.300 (external, cli)",
			},
		}, nil
	}
	runtimeIdentityFingerprintStoreFuncMu.Unlock()

	start := time.Now()
	if got := loadRuntimeIdentityFingerprintOnColdStart(identityfingerprint.ProviderClaude, "acct-slow"); got != nil {
		t.Fatalf("slow cold load returned %#v, want nil after the timeout", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("slow cold load held the request for %s", elapsed)
	}
	if got := loadRuntimeIdentityFingerprintOnColdStart(identityfingerprint.ProviderClaude, "acct-slow"); got != nil {
		t.Fatalf("second cold load returned %#v, want nil (already attempted)", got)
	}
	if got := loadRuntimeIdentityFingerprintOnColdStart(identityfingerprint.ProviderCodex, "acct-slow"); got != nil {
		t.Fatalf("codex cold load returned %#v, want nil (provider not covered)", got)
	}
	close(release)
	eventually(t, 2*time.Second, func() bool {
		return getCachedRuntimeIdentityFingerprint(identityfingerprint.ProviderClaude, "acct-slow", "", "") != nil
	})
	if got := calls.Load(); got != 1 {
		t.Fatalf("store reads = %d, want 1", got)
	}
}
