package privacymask

import (
	"strings"
	"testing"
	"time"
)

const testPassword = "Qx7mP2vL9kR4"

// maskAll masks texts as one request of scope and commits it.
func maskAll(scope *Scope, route string, texts ...string) ([]string, *Masker) {
	m := NewMasker(testKey, secretsOnly, scope, route)
	edits := m.Plan(texts, nil)
	out := make([]string, len(texts))
	for i, s := range texts {
		out[i] = Apply(s, edits[i])
	}
	m.Commit()
	return out, m
}

func TestAValueFoundAnywhereIsMaskedEverywhereInTheRequest(t *testing.T) {
	out, m := maskAll(nil, "", "psql -h db -W "+testPassword, "DB_PASSWORD="+testPassword)
	for _, s := range out {
		if strings.Contains(s, testPassword) {
			t.Fatalf("the password went upstream: %q", out)
		}
	}
	p := strings.TrimPrefix(out[1], "DB_PASSWORD=")
	if out[0] != "psql -h db -W "+p {
		t.Fatalf("the two places got different placeholders: %q", out)
	}
	if v := m.Values(); len(v) != 1 || v[0].N != 2 || v[0].Kind != "SECRET" {
		t.Fatalf("values = %+v", v)
	}
	if m.Places() != 2 {
		t.Fatalf("places = %d", m.Places())
	}
}

func TestAScopeMasksWhatItsEarlierRequestsMasked(t *testing.T) {
	scope := NewScopes().Get("tenant\x00u:1")
	maskAll(scope, "m", "DB_PASSWORD="+testPassword)
	out, m := maskAll(scope, "m", "now run: psql -W "+testPassword)
	if strings.Contains(out[0], testPassword) || len(m.Values()) != 1 || m.Values()[0].By != ByKnown {
		t.Fatalf("out = %q, values = %+v", out, m.Values())
	}
}

// A text the scope sent before goes again as it went, so a conversation's
// history reaches the upstream byte for byte the same and its prompt cache
// keeps working: a value that becomes known later is masked in new text only.
func TestATextSentBeforeGoesAgainAsItWent(t *testing.T) {
	scope := NewScopes().Get("tenant\x00u:1")
	first := "note: " + testPassword + " is what the old box used"
	out1, _ := maskAll(scope, "m", first)
	if out1[0] != first {
		t.Fatalf("nothing names it a password yet, got %q", out1[0])
	}
	out2, _ := maskAll(scope, "m", first, "DB_PASSWORD="+testPassword)
	if out2[0] != first {
		t.Fatalf("the earlier turn was rewritten, the upstream's cache would miss: %q", out2[0])
	}
	if strings.Contains(out2[1], testPassword) {
		t.Fatalf("the new turn kept the password: %q", out2[1])
	}
	out3, _ := maskAll(scope, "m", first, "DB_PASSWORD="+testPassword, "use "+testPassword+" again")
	if out3[0] != first || out3[1] != out2[1] || strings.Contains(out3[2], testPassword) {
		t.Fatalf("turn three = %q", out3)
	}
	// On another route the text is masked with what is known now.
	out4, _ := maskAll(scope, "other-model", first)
	if strings.Contains(out4[0], testPassword) {
		t.Fatalf("another route got the known value as written: %q", out4[0])
	}
	// With no scope, the same request masks it everywhere.
	out5, _ := maskAll(nil, "m", first, "DB_PASSWORD="+testPassword)
	if strings.Contains(out5[0], testPassword) {
		t.Fatalf("without a scope the value stayed: %q", out5[0])
	}
}

func TestRestoreTakesBackWhatTheModelRewrote(t *testing.T) {
	out, m := maskAll(nil, "", "OPENAI_API_KEY="+testOpenAIKey)
	p := strings.TrimPrefix(out[0], "OPENAI_API_KEY=")
	body := strings.TrimSuffix(strings.TrimPrefix(p, "{{"), "}}")
	r := m.Restorer()
	for _, written := range []string{
		p,
		"{{ " + body + " }}",
		"{" + body + "}",
		"{{" + body + "}",
		body,
		strings.ToLower(body[:7]) + body[7:],
		strings.ToUpper(body),
	} {
		if got := r.Restore("key="+written+".", false); got != "key="+testOpenAIKey+"." {
			t.Errorf("%q came back as %q", written, got)
		}
	}
	// a brace on one side alone is the text's own
	if got := r.Restore("{"+body+" here", false); got != "{"+testOpenAIKey+" here" {
		t.Errorf("got %q", got)
	}
	// a word that only looks like a placeholder stays
	if got := r.Restore("MAX_TOKENS_abcdefgh and API_KEY_zzzzzzzz", false); got != "MAX_TOKENS_abcdefgh and API_KEY_zzzzzzzz" {
		t.Errorf("got %q", got)
	}
	if r.Restored() != 8 {
		t.Errorf("restored = %d", r.Restored())
	}
}

func TestRestoreEscapesAValueInsideJSONText(t *testing.T) {
	m := NewMasker(testKey, Options{Words: []string{`C:\vault "main"`}}, nil, "")
	text := `open C:\vault "main" now`
	edits := m.Plan([]string{text}, nil)[0]
	p := edits[0].Text
	r := m.Restorer()
	if got := r.Restore(`{"path":"`+p+`"}`, true); got != `{"path":"C:\\vault \"main\""}` {
		t.Fatalf("in JSON text: %s", got)
	}
	if got := r.Restore("path "+p, false); got != `path C:\vault "main"` {
		t.Fatalf("in text: %s", got)
	}
}

func TestPartialTailHoldsWhatMayBecomeAPlaceholder(t *testing.T) {
	_, m := maskAll(nil, "", "OPENAI_API_KEY="+testOpenAIKey)
	r := m.Restorer()
	for _, tc := range []struct {
		text string
		held string
	}{
		{"use {", "{"},
		{"use {{API_K", "{{API_K"},
		{"use {{ api_key_ab", "{{ api_key_ab"},
		{"use API_", "API_"},
		{"use API_KEY_ab3", "API_KEY_ab3"},
		{"use {not a placeholder} ", ""},
		{"use APPLE", ""},
		{"done.", ""},
	} {
		if n := r.PartialTail(tc.text); tc.text[len(tc.text)-n:] != tc.held {
			t.Errorf("%q: held %q, want %q", tc.text, tc.text[len(tc.text)-n:], tc.held)
		}
	}
}

// A reply restores only its own scope's placeholders: another sender (another
// portal user, another tenant) cannot have a value put back by naming it.
func TestScopesKeepTheirValuesApart(t *testing.T) {
	scopes := NewScopes()
	a, b := scopes.Get("t1\x00u:a"), scopes.Get("t1\x00u:b")
	outA, _ := maskAll(a, "m", "DB_PASSWORD="+testPassword)
	pA := strings.TrimPrefix(outA[0], "DB_PASSWORD=")
	// B sends a request that names A's placeholder; nothing of B's is masked
	mb := NewMasker(testKey, secretsOnly, b, "m")
	mb.Plan([]string{"what is " + pA + "?"}, nil)
	if r := mb.Restorer(); r != nil && strings.Contains(r.Restore("it is "+pA, false), testPassword) {
		t.Fatal("B's reply revealed A's password")
	}
	// A's own later request that names it gets it back
	ma := NewMasker(testKey, secretsOnly, a, "m")
	ma.Plan([]string{"what is " + pA + "?"}, nil)
	if got := ma.Restorer().Restore("it is "+pA, false); got != "it is "+testPassword {
		t.Fatalf("A's reply = %q", got)
	}
	// another tenant's key makes another placeholder for the same value
	other := NewMasker([]byte("another tenant's key, 32 bytes.."), secretsOnly, nil, "")
	if e := other.Plan([]string{"DB_PASSWORD=" + testPassword}, nil)[0]; e[0].Text == pA {
		t.Fatal("two tenants got the same placeholder")
	}
}

func TestScopesAreBounded(t *testing.T) {
	scopes := NewScopes()
	now := time.Unix(1_800_000_000, 0)
	scopes.now = func() time.Time { return now }
	for i := range maxScopes + 10 {
		scopes.Get("t\x00k:" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26))
		now = now.Add(time.Millisecond)
	}
	if len(scopes.m) > maxScopes {
		t.Fatalf("%d scopes kept", len(scopes.m))
	}
	scopes.Get("other\x00k:1")
	scopes.Drop("t\x00")
	if len(scopes.m) != 1 {
		t.Fatalf("Drop left %d scopes", len(scopes.m))
	}
	// an idle scope is made anew
	s := scopes.Get("other\x00k:1")
	maskAll(s, "", "DB_PASSWORD="+testPassword)
	now = now.Add(scopeIdle)
	if scopes.Get("other\x00k:1") == s {
		t.Fatal("a scope idle for a day was kept")
	}
}

func TestAScopeKeepsAtMostSoManyValues(t *testing.T) {
	scope := NewScopes().Get("t\x00k:1")
	for i := range maxScopeValues + 5 {
		maskAll(scope, "", "DB_PASSWORD=Zz9"+strings.Repeat("q", i%5)+string(rune('A'+i%26))+strings.Repeat("7", 1+i/26))
	}
	if n := len(scope.values); n > maxScopeValues {
		t.Fatalf("%d values kept", n)
	}
}
