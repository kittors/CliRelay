package contentmoderation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
)

func normalized(t *testing.T, p MaskPolicy) MaskPolicy {
	t.Helper()
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMaskPolicyNormalizeTidiesAndRefuses(t *testing.T) {
	p := normalized(t, MaskPolicy{Words: []string{" Nightjar ", "", "Nightjar"}, Allow: []string{"x", " x "},
		Actions: map[string]string{"API_KEY": "mask", "EMAIL": "warn"}, Kinds: map[string]bool{"ip": true}})
	if len(p.Words) != 1 || p.Words[0] != "Nightjar" || len(p.Allow) != 1 || len(p.Actions) != 1 || p.Actions["EMAIL"] != "warn" || p.Rules == nil {
		t.Fatalf("normalized = %+v", p)
	}
	for name, bad := range map[string]MaskPolicy{
		"category": {Kinds: map[string]bool{"shoe_size": true}},
		"action":   {Actions: map[string]string{"API_KEY": "shred"}},
		"kind":     {Actions: map[string]string{"api key": "warn"}},
		"rule":     {Rules: []MaskRule{{Kind: "X", Regex: "("}}},
	} {
		if err := bad.Normalize(); !errors.Is(err, ErrInvalidMaskPolicy) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	many := MaskPolicy{}
	for i := range maxMaskWords + 1 {
		many.Words = append(many.Words, fmt.Sprintf("word%d", i))
	}
	if err := many.Normalize(); !errors.Is(err, ErrInvalidMaskPolicy) {
		t.Fatalf("too many words: %v", err)
	}
}

// What masking runs is the tenant's policy as it is: a row written before
// policies is "mask secrets" while switched on, and nothing switched on masks
// nothing.
func TestTheTenantsPolicyIsWhatMaskingRuns(t *testing.T) {
	p := normalized(t, MaskPolicy{Personal: true, Words: []string{"Nightjar"}, Allow: []string{"ops@company.com"},
		Actions: map[string]string{"EMAIL": "warn", "TERM": "block"}})
	o := SecretMaskingSettings{Policy: &p}.policy().options()
	if o.Secrets || !o.Personal || strings.Join(o.Words, ",") != "Nightjar" || strings.Join(o.Allow, ",") != "ops@company.com" ||
		o.Actions["EMAIL"] != privacymask.Warn || o.Actions["TERM"] != privacymask.Block {
		t.Fatalf("options = %+v", o)
	}
	if got := (SecretMaskingSettings{Enabled: true}).policy().options(); !got.Secrets || got.Personal {
		t.Fatalf("legacy = %+v", got)
	}
	if got := (SecretMaskingSettings{}).policy().options(); got.Any() {
		t.Fatalf("nothing switched on masks %+v", got)
	}
}

// A row saved while portal users kept settings of their own (2026-11-05)
// still reads, and loses the two fields of then once saved again.
func TestASettingsRowWithThePortalFieldsStillReads(t *testing.T) {
	var s SecretMaskingSettings
	raw := `{"enabled":true,"policy":{"secrets":true,"personal":false},"enforced":["secrets"],"portal_default_allowed":false}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if !s.policy().Secrets || s.policy().Personal {
		t.Fatalf("policy = %+v", s.policy())
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("enforced")) || bytes.Contains(out, []byte("portal_default_allowed")) {
		t.Fatalf("saved again = %s", out)
	}
}

// A tenant's rules apply whatever the secrets switch says.
func TestMaskRulesApplyWithSecretsOff(t *testing.T) {
	p := normalized(t, MaskPolicy{Rules: []MaskRule{{Kind: "GATEWAY", Prefix: "gw_live_"}}})
	masker := privacymask.NewMasker(make([]byte, 32), p.options(), nil, "")
	text := "token gw_live_abcdefgh12345678 here"
	out := privacymask.Apply(text, masker.Plan([]string{text}, nil)[0])
	if strings.Contains(out, "gw_live_abcdefgh12345678") || !strings.Contains(out, "{{GATEWAY_") {
		t.Fatalf("masked = %q", out)
	}
}

func TestSecretMaskingSettingsSealTheKeyUnderTheDataKey(t *testing.T) {
	dataKeyForTest = bytes.Repeat([]byte{7}, 32)
	defer func() { dataKeyForTest = nil }()
	registry := NewSecretMaskingRegistry()
	store := newSecretStoreStub()
	// a row written before the data key was set
	plain := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	store.settings["t"], store.versions["t"] = SecretMaskingSettings{Enabled: true, Key: plain}, 1
	registry.SetStore(store)

	p := normalized(t, MaskPolicy{Secrets: true, Personal: true})
	saved, err := registry.SetPolicy(context.Background(), "t", p)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Key != "" || saved.KeyEnc == "" || !saved.KeyProtected() || !saved.Enabled {
		t.Fatalf("saved = %+v", saved)
	}
	key, ok := saved.placeholderKey("t")
	if !ok || !bytes.Equal(key, bytes.Repeat([]byte{1}, 32)) {
		t.Fatal("sealing changed the key: the tenant's placeholders would change")
	}
	if _, ok := saved.placeholderKey("other"); ok {
		t.Fatal("a sealed key opened for another tenant")
	}
	raw, _ := json.Marshal(saved)
	if bytes.Contains(raw, []byte(plain)) {
		t.Fatalf("the stored row holds the key: %s", raw)
	}
	dataKeyForTest = bytes.Repeat([]byte{8}, 32)
	if _, ok := saved.placeholderKey("t"); ok {
		t.Fatal("a key sealed under another data key opened")
	}
}

// A portal user's requests go under the tenant's policy like any other: what
// is masked is the tenant's call. The record still names the user and the key.
func TestGuardRequestMasksAPortalUsersRequestUnderTheTenantsPolicy(t *testing.T) {
	moderator := maskingModerator(t)
	p := normalized(t, MaskPolicy{Personal: true})
	if _, err := moderator.secretMasking.SetPolicy(context.Background(), "tenant-p", p); err != nil {
		t.Fatal(err)
	}
	body := chatRequest("write to ada.lovelace@gmail.com")
	send := func(endUser string) (*SecretMaskingSnapshot, bool) {
		ctx, ginCtx := snapshotContext(t)
		ginCtx.Set("accessMetadata", map[string]string{"end-user-id": endUser, "api-key-id": "k-" + endUser})
		req, opts := guardOptions("tenant-p", body)
		result := moderator.GuardRequest(ctx, req, opts)
		if result.Blocked {
			t.Fatalf("blocked: %+v", result)
		}
		snapshot, _ := SecretMaskingSnapshotFromGin(ginCtx)
		return snapshot, result.Payload != nil && !bytes.Contains(result.Payload, []byte("ada.lovelace@gmail.com"))
	}
	snapshot, masked := send("u-1")
	if !masked || snapshot == nil || snapshot.Key == "" {
		t.Fatalf("the tenant's policy did not apply to a portal user's request: %+v", snapshot)
	}
	record := snapshot.Record(MaskClient("codex_cli_rs/0.50"))
	if record == nil || record.EndUserID != "u-1" || record.APIKeyID != "k-u-1" || record.Client != "codex" || record.Values[0].ID == "" || record.Values[0].Field == "" {
		t.Fatalf("record = %+v", record)
	}
	if _, masked := send(""); !masked {
		t.Fatal("a request of a tenant key went unmasked")
	}
}

func TestMaskClientNamesTheCodingAgents(t *testing.T) {
	for ua, want := range map[string]string{
		"codex_cli_rs/0.50.0 (Mac OS 15.1; arm64)": "codex",
		"claude-cli/2.0.14 (external, cli)":        "claude-code",
		"GeminiCLI/0.9.0 (darwin; arm64)":          "gemini-cli",
		"opencode/0.15.3":                          "opencode",
		"Mozilla/5.0 Cursor/1.7":                   "cursor",
		"curl/8.7.1":                               "other",
	} {
		if got := MaskClient(ua); got != want {
			t.Errorf("MaskClient(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestPreviewMaskUsesAKeyOfItsOwn(t *testing.T) {
	p := normalized(t, MaskPolicy{Secrets: true, Actions: map[string]string{"API_KEY": "block"}})
	a, b := PreviewMask(p, "key "+testOpenAIKey), PreviewMask(p, "key "+testOpenAIKey)
	if strings.Contains(a.Text, testOpenAIKey) || len(a.Values) != 1 || !a.Blocked || a.Values[0].Placeholder == "" {
		t.Fatalf("preview = %+v", a)
	}
	if a.Text == b.Text {
		t.Fatal("two previews made the same placeholder: they would match the tenant's")
	}
}
