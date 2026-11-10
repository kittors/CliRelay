package identityfingerprint

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// xaiVersionAboveDefault is a Grok CLI version newer than the builtin default,
// the floor a learned identity has to reach to be replayed. Derived rather than
// written out, so raising the default does not turn these tests into tests of
// the floor.
func xaiVersionAboveDefault() string {
	parts := strings.Split(config.DefaultXAIFingerprintClientVersion, ".")
	last, _ := strconv.Atoi(parts[len(parts)-1])
	parts[len(parts)-1] = strconv.Itoa(last + 100)
	return strings.Join(parts, ".")
}

func observeXAI(t *testing.T, existing *LearnedRecord, headers map[string]string) *LearnedRecord {
	t.Helper()
	h := http.Header{}
	for key, value := range headers {
		h.Set(key, value)
	}
	obs, ok := ExtractObservation(LearnInput{Provider: ProviderXAI, AccountKey: "acct", Headers: h, ObservedAt: time.Now()})
	if !ok {
		t.Fatalf("headers %v were not learned", headers)
	}
	result := MergeObservation(existing, obs)
	if result.Record == nil {
		t.Fatalf("merge kept no record: %s", result.Reason)
	}
	return result.Record
}

// TestXAIObservationReplacesTheWholeBundle replays 2026-11-09: an account had
// learned a grok-pager bundle, then the same CLI reached the proxy as an
// OpenAI-compatible endpoint and sent its User-Agent alone. Merged field by
// field, the record held that User-Agent beside the pager's identifier and
// version. It must hold the later client only.
func TestXAIObservationReplacesTheWholeBundle(t *testing.T) {
	newer := xaiVersionAboveDefault()
	record := observeXAI(t, nil, map[string]string{
		"User-Agent":               "grok-pager/1.0.70 grok-shell/1.0.70 (macos; aarch64)",
		"X-Grok-Client-Identifier": "grok-pager",
		"X-Grok-Client-Version":    "1.0.70",
		"X-Grok-Client-Mode":       "interactive",
	})
	record = observeXAI(t, record, map[string]string{"User-Agent": "grok-shell/" + newer + " (linux; x86_64)"})

	want := map[string]string{FieldUserAgent: "grok-shell/" + newer + " (linux; x86_64)"}
	if !sameStringMap(record.Fields, want) {
		t.Fatalf("fields = %v, want the later client's alone: %v", record.Fields, want)
	}
	if record.Version != newer {
		t.Fatalf("version = %q, want %q", record.Version, newer)
	}
}

func TestXAIObservationNeedsAGrokUserAgent(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "node")
	h.Set("X-Grok-Client-Identifier", "grok-shell")
	h.Set("X-Grok-Client-Version", "1.0.77")
	if obs, ok := ExtractObservation(LearnInput{Provider: ProviderXAI, AccountKey: "acct", Headers: h}); ok {
		t.Fatalf("learned %v from a caller whose User-Agent is not a Grok CLI", obs.Fields)
	}
}

func TestExtractXAIObservationLearnsTheClientMode(t *testing.T) {
	record := observeXAI(t, nil, map[string]string{
		"User-Agent":               "grok-pager/1.0.77 grok-shell/1.0.77 (macos; aarch64)",
		"X-Grok-Client-Identifier": "grok-pager",
		"X-Grok-Client-Version":    "1.0.77",
		"X-Grok-Client-Mode":       "interactive",
	})
	if got := record.Fields[FieldXAIClientMode]; got != "interactive" {
		t.Fatalf("x-grok-client-mode = %q, want interactive", got)
	}
}

func TestResolveXAIReplaysAPagerBundleWithItsMode(t *testing.T) {
	v := xaiVersionAboveDefault()
	learned := &LearnedRecord{Provider: ProviderXAI, AccountKey: "acct", Version: v, Fields: map[string]string{
		FieldUserAgent:           "grok-pager/" + v + " grok-shell/" + v + " (macos; aarch64)",
		FieldXAIClientIdentifier: "grok-pager",
		FieldXAIClientVersion:    v,
		FieldXAIClientMode:       "interactive",
	}}

	fp, effective := ResolveXAI(config.XAIIdentityFingerprintConfig{Enabled: true}, learned)
	if fp.UserAgent != learned.Fields[FieldUserAgent] || fp.ClientIdentifier != "grok-pager" || fp.ClientVersion != v {
		t.Fatalf("resolved = %#v, want the learned pager bundle", fp)
	}
	if mode := XAIClientMode(effective); mode != "interactive" {
		t.Fatalf("mode = %q, want interactive", mode)
	}
	if source := effective.Fields[FieldXAIClientMode].Source; source != FieldSourceLearned {
		t.Fatalf("mode source = %q, want learned", source)
	}
}

// TestResolveXAIHealsABundleMergedFromTwoClients reads a record as the old
// field-wise merge left it in production: grok-shell's User-Agent beside an
// earlier grok-pager's identifier and version. It goes out as the client its
// User-Agent names, and the mode it was not learned with stays behind.
func TestResolveXAIHealsABundleMergedFromTwoClients(t *testing.T) {
	v := xaiVersionAboveDefault()
	learned := &LearnedRecord{Provider: ProviderXAI, AccountKey: "acct", Version: v, Fields: map[string]string{
		FieldUserAgent:           "grok-shell/" + v + " (linux; x86_64)",
		FieldXAIClientIdentifier: "grok-pager",
		FieldXAIClientVersion:    "1.0.70",
		FieldXAIClientMode:       "interactive",
	}}

	fp, effective := ResolveXAI(config.XAIIdentityFingerprintConfig{Enabled: true}, learned)
	if fp.UserAgent != "grok-shell/"+v+" (linux; x86_64)" || fp.ClientIdentifier != "grok-shell" || fp.ClientVersion != v {
		t.Fatalf("resolved = %#v, want one grok-shell %s identity", fp, v)
	}
	if mode := XAIClientMode(effective); mode != "" {
		t.Fatalf("mode = %q went out with a bundle it was not learned with", mode)
	}
}

// TestResolveXAIGivesWayToTheDefaultBelowItsVersion keeps an outdated learned
// client from going out: cli-chat-proxy answers it with 426. The builtin
// identity replaces it whole, mode included.
func TestResolveXAIGivesWayToTheDefaultBelowItsVersion(t *testing.T) {
	learned := &LearnedRecord{Provider: ProviderXAI, AccountKey: "acct", Version: "0.2.93", Fields: map[string]string{
		FieldUserAgent:           "grok-shell/0.2.93 (macos; aarch64)",
		FieldXAIClientIdentifier: "grok-shell",
		FieldXAIClientVersion:    "0.2.93",
		FieldXAIClientMode:       "headless",
	}}

	fp, effective := ResolveXAI(config.XAIIdentityFingerprintConfig{Enabled: true}, learned)
	defaults := config.DefaultXAIIdentityFingerprint()
	if fp.UserAgent != defaults.UserAgent || fp.ClientIdentifier != defaults.ClientIdentifier || fp.ClientVersion != defaults.ClientVersion {
		t.Fatalf("resolved = %#v, want the builtin identity whole", fp)
	}
	if mode := XAIClientMode(effective); mode != "" {
		t.Fatalf("mode = %q, want none beside the builtin identity", mode)
	}
	if source := effective.Fields[FieldUserAgent].Source; source != FieldSourceBuiltinDefault {
		t.Fatalf("User-Agent source = %q, want builtin_default", source)
	}
}
