package payloadrules

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func models(names ...string) []config.PayloadModelRule {
	out := make([]config.PayloadModelRule, 0, len(names))
	for _, name := range names {
		out = append(out, config.PayloadModelRule{Name: name})
	}
	return out
}

func applyTo(t *testing.T, rules config.PayloadConfig, req Request) string {
	t.Helper()
	out := Apply(rules, req)
	if explained := Explain(rules, req).Body; !bytes.Equal(explained, out) {
		t.Fatalf("Explain body = %s, Apply body = %s", explained, out)
	}
	return string(out)
}

func TestDefaultsOnlyFillWhatTheClientLeftOut(t *testing.T) {
	rules := config.PayloadConfig{Default: []config.PayloadRule{{
		Models: models("gpt-*"),
		Params: map[string]any{"temperature": 0.2, "max_tokens": 4096},
	}}}
	got := applyTo(t, rules, Request{
		Model: "gpt-5",
		// The translator filled max_tokens in; the client sent temperature.
		Body:     []byte(`{"temperature":1,"max_tokens":100}`),
		Original: []byte(`{"temperature":1}`),
	})
	if gjson.Get(got, "temperature").Num != 1 {
		t.Fatalf("temperature the client sent was replaced: %s", got)
	}
	if gjson.Get(got, "max_tokens").Int() != 4096 {
		t.Fatalf("max_tokens the client did not send was not defaulted: %s", got)
	}
}

func TestDefaultsAreFirstWriteWinsAndOverridesLastWriteWins(t *testing.T) {
	rules := config.PayloadConfig{
		Default: []config.PayloadRule{
			{Models: models("*"), Params: map[string]any{"a": 1}},
			{Models: models("*"), Params: map[string]any{"a": 2, "b": 2}},
		},
		DefaultRaw: []config.PayloadRule{
			{Models: models("*"), Params: map[string]any{"b": `3`, "c": `{"x":true}`}},
		},
		Override: []config.PayloadRule{
			{Models: models("*"), Params: map[string]any{"d": "first"}},
			{Models: models("*"), Params: map[string]any{"d": "second"}},
		},
		OverrideRaw: []config.PayloadRule{
			{Models: models("*"), Params: map[string]any{"e": `[1,2]`}},
		},
	}
	got := applyTo(t, rules, Request{Model: "m", Body: []byte(`{}`)})
	want := `{"a":1,"b":2,"c":{"x":true},"d":"second","e":[1,2]}`
	if len(diffJSON([]byte(got), []byte(want))) != 0 {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestFiltersRunLast(t *testing.T) {
	rules := config.PayloadConfig{
		Override: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"stream_options.include_usage": true}}},
		Filter:   []config.PayloadFilterRule{{Models: models("*"), Params: []string{"stream_options", "temperature"}}},
	}
	got := applyTo(t, rules, Request{Model: "m", Body: []byte(`{"temperature":1,"stream":true}`)})
	if got != `{"stream":true}` {
		t.Fatalf("body = %s", got)
	}
}

func TestModelEntriesMatchTheProtocolTheyAreRestrictedTo(t *testing.T) {
	rules := config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-5", Protocol: "codex"}},
		Params: map[string]any{"service_tier": "priority"},
	}}}
	for _, tc := range []struct {
		protocol string
		applies  bool
	}{
		{"codex", true},
		{"CODEX", true},
		{"openai", false},
		// No protocol is known: every entry matches.
		{"", true},
	} {
		got := applyTo(t, rules, Request{Model: "gpt-5", Protocol: tc.protocol, Body: []byte(`{}`)})
		if applied := gjson.Get(got, "service_tier").Exists(); applied != tc.applies {
			t.Errorf("protocol %q: applied = %v, want %v (%s)", tc.protocol, applied, tc.applies, got)
		}
	}
}

func TestRulesMatchTheRequestedModelAndItsSuffix(t *testing.T) {
	cases := []struct {
		pattern        string
		model          string
		requestedModel string
		applies        bool
	}{
		{"gpt-5", "gpt-5", "", true},
		{"fast", "gpt-5", "fast(high)", true},
		{"fast(high)", "gpt-5", "fast(high)", true},
		{"fast(low)", "gpt-5", "fast(high)", false},
		{"gemini-*-pro", "", "gemini-2.5-pro", true},
		{"*-mini", "gpt-5", "", false},
	}
	for _, tc := range cases {
		rules := config.PayloadConfig{Override: []config.PayloadRule{{Models: models(tc.pattern), Params: map[string]any{"x": 1}}}}
		got := applyTo(t, rules, Request{Model: tc.model, RequestedModel: tc.requestedModel, Body: []byte(`{}`)})
		if applied := gjson.Get(got, "x").Exists(); applied != tc.applies {
			t.Errorf("%q against %q/%q: applied = %v, want %v", tc.pattern, tc.model, tc.requestedModel, applied, tc.applies)
		}
	}
}

func TestRootPrefixesEveryPath(t *testing.T) {
	rules := config.PayloadConfig{
		Default: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"generationConfig.temperature": 0.3, "generationConfig.topP": 0.9}}},
		Filter:  []config.PayloadFilterRule{{Models: models("*"), Params: []string{".safetySettings"}}},
	}
	body := `{"model":"gemini-2.5-pro","request":{"generationConfig":{"topP":1},"safetySettings":[]}}`
	got := applyTo(t, rules, Request{Model: "gemini-2.5-pro", Root: "request", Body: []byte(body)})
	want := `{"model":"gemini-2.5-pro","request":{"generationConfig":{"topP":1,"temperature":0.3}}}`
	if got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestNothingToDoReturnsTheBodyItself(t *testing.T) {
	body := []byte(`{"a":1}`)
	rules := config.PayloadConfig{Override: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"a": 2}}}}
	for name, tc := range map[string]struct {
		rules config.PayloadConfig
		req   Request
	}{
		"no rules": {config.PayloadConfig{}, Request{Model: "m", Body: body}},
		"no model": {rules, Request{Body: body}},
		"no body":  {rules, Request{Model: "m"}},
	} {
		got := Apply(tc.rules, tc.req)
		if len(got) != len(tc.req.Body) || (len(got) > 0 && &got[0] != &tc.req.Body[0]) {
			t.Errorf("%s: got a new body %q", name, got)
		}
	}
}

// Params are a map, so two paths of one rule that overlap were written in a
// random order. They now run in path order: the parent first, then the field
// inside it.
func TestOverlappingParamsOfOneRuleGiveTheSameBodyEveryTime(t *testing.T) {
	rules := config.PayloadConfig{Override: []config.PayloadRule{{
		Models: models("*"),
		Params: map[string]any{"reasoning": map[string]any{"effort": "low"}, "reasoning.summary": "auto"},
	}}}
	want := `{"reasoning":{"effort":"low","summary":"auto"}}`
	for i := 0; i < 50; i++ {
		if got := applyTo(t, rules, Request{Model: "m", Body: []byte(`{}`)}); got != want {
			t.Fatalf("run %d: body = %s, want %s", i, got, want)
		}
	}
}

func TestMatchModelPattern(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"*", "anything", true},
		{"*-5", "gpt-5", true},
		{"gpt-*", "gpt-4", true},
		{"gemini-*-pro", "gemini-2.5-pro", true},
		{"gemini-*-pro", "gemini-2.5-flash", false},
		{"gpt-5", "gpt-5-mini", false},
		{"*mini*", "gpt-5-mini-high", true},
		{"", "gpt-5", false},
		{" gpt-5 ", "gpt-5", true},
	}
	for _, tc := range cases {
		if got := MatchModelPattern(tc.pattern, tc.model); got != tc.want {
			t.Errorf("MatchModelPattern(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

// The engine moved here from internal/runtime/executor. Run both over the same
// rules, models, protocols, roots and bodies: every request must get the same
// body it got before.
func TestApplyMatchesTheEngineItReplaced(t *testing.T) {
	ruleSets := []config.PayloadConfig{
		{Default: []config.PayloadRule{{Models: models("gpt-*"), Params: map[string]any{"temperature": 0.2}}}},
		{Default: []config.PayloadRule{
			{Models: models("*"), Params: map[string]any{"max_tokens": 100}},
			{Models: models("*"), Params: map[string]any{"max_tokens": 200}},
		}},
		{DefaultRaw: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"metadata": `{"tier":"gold"}`}}}},
		{Override: []config.PayloadRule{
			{Models: []config.PayloadModelRule{{Name: "gpt-5", Protocol: "codex"}}, Params: map[string]any{"service_tier": "priority"}},
			{Models: []config.PayloadModelRule{{Name: "fast", Protocol: "openai"}}, Params: map[string]any{"service_tier": "flex"}},
		}},
		{OverrideRaw: []config.PayloadRule{{Models: models("*opus*"), Params: map[string]any{"thinking": `{"type":"adaptive"}`}}}},
		{Filter: []config.PayloadFilterRule{{Models: models("*"), Params: []string{"temperature", "messages.0", "missing", ""}}}},
		{
			Default:  []config.PayloadRule{{Models: models("*"), Params: map[string]any{"generationConfig.topK": 40}}},
			Override: []config.PayloadRule{{Models: models("gemini-*"), Params: map[string]any{"generationConfig.temperature": 0}}},
			Filter:   []config.PayloadFilterRule{{Models: models("*"), Params: []string{"safetySettings"}}},
		},
		{Override: []config.PayloadRule{
			{Models: models(""), Params: map[string]any{"x": 1}},
			{Models: nil, Params: map[string]any{"x": 2}},
			// Under a root an empty path is the root itself.
			{Models: models("*"), Params: map[string]any{"": 3}},
			{Models: models("*"), Params: map[string]any{"tools.-1": map[string]any{"type": "web_search"}}},
		}},
	}
	// Every rule above has paths that do not overlap: with overlapping paths in
	// one rule the old engine's body depended on map order (see
	// TestOverlappingParamsOfOneRuleGiveTheSameBodyEveryTime).
	bodies := []struct{ body, original string }{
		{`{}`, ""},
		{`{"model":"gpt-5","temperature":1,"messages":[{"role":"user","content":"hi"}],"stream":true}`, ""},
		{`{"model":"gpt-5","temperature":1,"max_tokens":5}`, `{"model":"gpt-5"}`},
		{`{"request":{"generationConfig":{"temperature":1},"safetySettings":[]},"model":"gemini-2.5-pro"}`, ""},
		{`{"tools":[{"type":"function"}],"metadata":{"tier":"basic"}}`, ""},
	}
	type modelPair struct{ model, requested string }
	modelPairs := []modelPair{{"gpt-5", ""}, {"gpt-5", "fast(high)"}, {"", "gemini-2.5-pro"}, {"claude-opus-4-5", "opus"}, {"", ""}}
	protocols := []string{"", "openai", "codex", "gemini", "CLAUDE"}
	roots := []string{"", "request"}

	cases := 0
	for ri, rules := range ruleSets {
		for bi, b := range bodies {
			for _, mp := range modelPairs {
				for _, protocol := range protocols {
					for _, root := range roots {
						name := fmt.Sprintf("rules%d/body%d/%s+%s/%s/%s", ri, bi, mp.model, mp.requested, protocol, root)
						var original []byte
						if b.original != "" {
							original = []byte(b.original)
						}
						cfg := &config.Config{Payload: rules}
						want := legacyApplyPayloadConfigWithRoot(cfg, mp.model, protocol, root, []byte(b.body), original, mp.requested)
						req := Request{Model: mp.model, RequestedModel: mp.requested, Protocol: protocol, Root: root, Body: []byte(b.body), Original: original}
						got := Apply(rules, req)
						if !bytes.Equal(got, want) {
							t.Errorf("%s: body = %s, the old engine gave %s", name, got, want)
						}
						if explained := Explain(rules, req).Body; !bytes.Equal(explained, got) {
							t.Errorf("%s: Explain body = %s, Apply body = %s", name, explained, got)
						}
						cases++
					}
				}
			}
		}
	}
	if cases < 1000 {
		t.Fatalf("only %d cases ran", cases)
	}
}
