package contentmoderation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	if _, err := NormalizeEnforced([]string{"personal", "secrets", "secrets"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := NormalizeEnforced([]string{"personal", "secrets"}); strings.Join(got, ",") != "secrets,personal" {
		t.Fatalf("enforced = %v", got)
	}
	if _, err := NormalizeEnforced([]string{"words"}); !errors.Is(err, ErrInvalidMaskPolicy) {
		t.Fatalf("enforcing words: %v", err)
	}
}

// What the tenant enforces holds for a portal user's own policy; the rest is
// the user's.
func TestEffectiveMaskOptionsKeepsWhatTheTenantEnforces(t *testing.T) {
	tenantPolicy := normalized(t, MaskPolicy{Secrets: true, Words: []string{"Nightjar"}, Allow: []string{"demo-token-1"},
		Actions: map[string]string{"TERM": "block"}})
	tenant := SecretMaskingSettings{Policy: &tenantPolicy, Enforced: []string{EnforceSecrets}}
	user := normalized(t, MaskPolicy{Personal: true, Words: []string{"Kestrel"}, Allow: []string{testOpenAIKey, "Nightjar"},
		Actions: map[string]string{"API_KEY": "warn", "EMAIL": "warn"}})

	o := effectiveMaskOptions(tenant, &user, true)
	if !o.Secrets || !o.Personal {
		t.Fatalf("secrets enforced, personal the user's: %+v", o)
	}
	if strings.Join(o.Words, ",") != "Nightjar,Kestrel" {
		t.Fatalf("words = %v", o.Words)
	}
	if o.Actions["API_KEY"] != privacymask.Mask || o.Actions["EMAIL"] != privacymask.Warn || o.Actions["TERM"] != privacymask.Block {
		t.Fatalf("actions = %v", o.Actions)
	}
	// the user's "never mask" cannot let an enforced category's value, or a
	// tenant's word, go as written
	masker := privacymask.NewMasker(make([]byte, 32), o, nil, "")
	text := "use " + testOpenAIKey + " for Nightjar, mail me at ada.byron@gmail.com"
	edits := masker.Plan([]string{text}, nil)[0]
	out := privacymask.Apply(text, edits)
	if strings.Contains(out, testOpenAIKey) || strings.Contains(out, "Nightjar") || !strings.Contains(out, "ada.byron@gmail.com") {
		t.Fatalf("masked = %q", out)
	}
	if len(masker.Blocked()) != 1 {
		t.Fatalf("the tenant's word is set to block: %+v", masker.Values())
	}
	warned := false
	for _, v := range masker.Values() {
		warned = warned || v.Kind == "EMAIL" && v.Action == privacymask.Warn
	}
	if !warned {
		t.Fatalf("the user's warn for emails did not apply: %+v", masker.Values())
	}

	// without a policy of their own a user gets the tenant's default
	if got := effectiveMaskOptions(tenant, nil, true); got.Personal || !got.Secrets || len(got.UserAllow) != 0 {
		t.Fatalf("tenant default = %+v", got)
	}
	// a row from before policies is "mask secrets" while switched on
	if got := effectiveMaskOptions(SecretMaskingSettings{Enabled: true}, nil, false); !got.Secrets || got.Personal {
		t.Fatalf("legacy = %+v", got)
	}
	if got := effectiveMaskOptions(SecretMaskingSettings{}, nil, false); got.Any() {
		t.Fatalf("nothing switched on masks %+v", got)
	}
	// what the tenant enforces holds for a portal user without a policy of
	// their own, even where the tenant's default has it off; a request no
	// portal user sent gets the default as it is
	quiet := normalized(t, MaskPolicy{Words: []string{"Nightjar"}, Actions: map[string]string{"API_KEY": "warn"}})
	enforcing := SecretMaskingSettings{Policy: &quiet, Enforced: []string{EnforceSecrets}}
	if got := effectiveMaskOptions(enforcing, nil, true); !got.Secrets || got.Actions["API_KEY"] != privacymask.Mask || len(got.Words) != 1 {
		t.Fatalf("portal user without a policy = %+v", got)
	}
	if got := effectiveMaskOptions(enforcing, nil, false); got.Secrets {
		t.Fatalf("a tenant key's request = %+v", got)
	}
}

// A tenant's rules apply whatever the secrets switch says.
func TestMaskRulesApplyWithSecretsOff(t *testing.T) {
	p := normalized(t, MaskPolicy{Rules: []MaskRule{{Kind: "GATEWAY", Prefix: "gw_live_"}}})
	masker := privacymask.NewMasker(make([]byte, 32), effectiveMaskOptions(SecretMaskingSettings{Policy: &p}, nil, false), nil, "")
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
	saved, err := registry.SetPolicy(context.Background(), "t", p, []string{EnforceSecrets}, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Key != "" || saved.KeyEnc == "" || !saved.KeyProtected() || !saved.Enabled || saved.PortalAllowed() {
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

// A portal user's own policy masks their requests; another user's requests,
// and theirs once the tenant revokes it, go under the tenant's default.
func TestGuardRequestUsesThePortalUsersOwnPolicy(t *testing.T) {
	store := newTestStore(t)
	MaskUsers().SetStore(store)
	defer MaskUsers().SetStore(nil)
	moderator := maskingModerator(t)
	if err := moderator.secretMasking.EnsureKey(context.Background(), "tenant-p"); err != nil {
		t.Fatal(err)
	}
	own := normalized(t, MaskPolicy{Personal: true})
	if err := store.PutMaskUserPolicy(context.Background(), "tenant-p", "u-1", &own); err != nil {
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
		t.Fatalf("the user's own policy did not apply: %+v", snapshot)
	}
	record := snapshot.Record(MaskClient("codex_cli_rs/0.50"))
	if record == nil || record.EndUserID != "u-1" || record.APIKeyID != "k-u-1" || record.Client != "codex" || record.Values[0].ID == "" || record.Values[0].Field == "" {
		t.Fatalf("record = %+v", record)
	}
	if _, masked := send("u-2"); masked {
		t.Fatal("another user's request got the first user's policy")
	}
	allowed := false
	if err := store.SetMaskUserAllowed(context.Background(), "tenant-p", "u-1", &allowed); err != nil {
		t.Fatal(err)
	}
	if _, masked := send("u-1"); masked {
		t.Fatal("a revoked policy still applied")
	}
}

// A request whose portal user's settings were never read and cannot be read
// now may be one that masks: it is refused.
func TestGuardRequestRefusesWhileAPortalUsersSettingsAreUnknown(t *testing.T) {
	store := newTestStore(t)
	_ = store.db.Close()
	MaskUsers().SetStore(store)
	defer MaskUsers().SetStore(nil)
	moderator := maskingModerator(t)
	ctx, ginCtx := snapshotContext(t)
	ginCtx.Set("accessMetadata", map[string]string{"end-user-id": "u-9"})
	req, opts := guardOptions("tenant-q", chatRequest("hello"))
	if result := moderator.GuardRequest(ctx, req, opts); !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("result = %+v", result)
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
	tenant := SecretMaskingSettings{Policy: &p}
	a, b := PreviewMask(tenant, nil, "key "+testOpenAIKey), PreviewMask(tenant, nil, "key "+testOpenAIKey)
	if strings.Contains(a.Text, testOpenAIKey) || len(a.Values) != 1 || !a.Blocked || a.Values[0].Placeholder == "" {
		t.Fatalf("preview = %+v", a)
	}
	if a.Text == b.Text {
		t.Fatal("two previews made the same placeholder: they would match the tenant's")
	}
}
