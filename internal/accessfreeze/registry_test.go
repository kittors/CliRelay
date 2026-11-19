package accessfreeze

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testZone = time.FixedZone("UTC+8", 8*3600)

func testFreeze(id int64, subject Subject, provider string) Freeze {
	return Freeze{
		ID:         id,
		Subject:    subject,
		Provider:   provider,
		ReasonCode: "cyber_policy",
		FrozenAt:   time.Date(2026, 11, 13, 10, 17, 0, 0, time.UTC),
	}
}

func TestNoticeSaysWhatWhenWhyAndWhoLiftsIt(t *testing.T) {
	account := Subject{TenantID: "t1", Kind: SubjectEndUser, ID: "u1"}
	got := Notice(testFreeze(1, account, "codex"), testZone)
	for _, want := range []string{
		"Access to Codex is frozen for this account since 2026-11-13 18:17 +08:00",
		"possible cybersecurity risk (cyber_policy)",
		"Other upstreams are not affected",
		"Contact your administrator",
		"本账号的 Codex 使用权限已于 2026-11-13 18:17 +08:00 冻结",
		"可能存在网络安全风险",
		"联系管理员",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice %q lacks %q", got, want)
		}
	}

	key := Subject{TenantID: "t1", Kind: SubjectAPIKey, ID: "k1"}
	got = Notice(testFreeze(2, key, "codex"), testZone)
	if !strings.Contains(got, "for this API key") || !strings.Contains(got, "该 API Key") {
		t.Fatalf("notice for a key without a portal account = %q", got)
	}
}

func TestNoticeDoesNotNameAnOperatorNamedUpstream(t *testing.T) {
	account := Subject{TenantID: "t1", Kind: SubjectEndUser, ID: "u1"}
	f := testFreeze(1, account, "acme-relay-hk")
	f.ReasonCode = "some_future_code"
	got := Notice(f, testZone)
	if strings.Contains(got, "acme") {
		t.Fatalf("notice %q names the channel; the portal must not learn channel names", got)
	}
	if !strings.Contains(got, "this upstream") || !strings.Contains(got, "该上游") || !strings.Contains(got, "(some_future_code)") {
		t.Fatalf("notice %q", got)
	}
}

func TestRegistryEncodesNoticesPerSubject(t *testing.T) {
	r := newRegistry(testZone)
	alice := Subject{TenantID: "t1", Kind: SubjectEndUser, ID: "alice"}
	bob := Subject{TenantID: "t1", Kind: SubjectEndUser, ID: "bob"}
	r.replaceAllUnlessChanged(r.generation(), []Freeze{testFreeze(1, alice, "Codex"), testFreeze(2, alice, "claude")})

	var notices map[string]string
	if err := json.Unmarshal([]byte(r.frozenProviders(alice)), &notices); err != nil {
		t.Fatalf("frozenProviders(alice) is not a JSON object: %v", err)
	}
	if len(notices) != 2 || !strings.Contains(notices["codex"], "Codex") || !strings.Contains(notices["claude"], "Claude") {
		t.Fatalf("notices = %#v", notices)
	}
	if got := r.frozenProviders(bob); got != "" {
		t.Fatalf("frozenProviders(bob) = %q, want empty", got)
	}

	released := testFreeze(1, alice, "codex")
	now := time.Now()
	released.ReleasedAt = &now
	r.put(released)
	if _, ok := r.active(alice, "codex"); ok {
		t.Fatal("codex still frozen after release")
	}
	if _, ok := r.active(alice, "claude"); !ok {
		t.Fatal("claude freeze lost when codex was released")
	}

	// A stale release (another freeze id) must not lift the current freeze.
	r.put(testFreeze(7, alice, "codex"))
	r.put(released)
	if _, ok := r.active(alice, "codex"); !ok {
		t.Fatal("release of freeze 1 lifted freeze 7")
	}
}

func TestCapUpstreamMessageKeepsWholeRunes(t *testing.T) {
	message := strings.Repeat("冻", maxUpstreamMessage)
	got := capUpstreamMessage(message)
	if len(got) > maxUpstreamMessage || !strings.HasPrefix(message, got) || strings.ContainsRune(got, '�') {
		t.Fatalf("capUpstreamMessage cut %d bytes badly", len(got))
	}
}

func TestRegistryKeepsALocalChangeMadeDuringALoad(t *testing.T) {
	r := newRegistry(testZone)
	dave := Subject{TenantID: "t1", Kind: SubjectEndUser, ID: "dave"}
	generation := r.generation()
	// This process freezes dave while a refresh is reading the table...
	r.put(testFreeze(3, dave, "codex"))
	// ...and the refresh's read predates the freeze.
	if r.replaceAllUnlessChanged(generation, nil) {
		t.Fatal("a load read before the freeze was installed over it")
	}
	if _, ok := r.active(dave, "codex"); !ok {
		t.Fatal("the local freeze was lost")
	}
	if !r.replaceAllUnlessChanged(r.generation(), []Freeze{testFreeze(3, dave, "codex")}) {
		t.Fatal("a load read after the change was refused")
	}
}
