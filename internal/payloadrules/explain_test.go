package payloadrules

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func ptr(s string) *string { return &s }

func TestExplainSaysWhatEachRuleDid(t *testing.T) {
	rules := config.PayloadConfig{
		Default: []config.PayloadRule{
			{Models: models("gpt-*"), Params: map[string]any{"temperature": 0.2, "max_tokens": 4096}},
			{Models: models("*"), Params: map[string]any{"max_tokens": 1}},
		},
		Override: []config.PayloadRule{
			{Models: []config.PayloadModelRule{{Name: "gpt-5", Protocol: "claude"}}, Params: map[string]any{"x": 1}},
			{Models: models("o3"), Params: map[string]any{"x": 2}},
			{Models: models(" "), Params: map[string]any{"x": 3}},
			{Models: models("fast"), Params: map[string]any{"text.verbosity": "low"}},
		},
		OverrideRaw: []config.PayloadRule{
			{Models: models("*"), Params: map[string]any{"tools.-1": `{"type":"web_search"}`}},
		},
		Filter: []config.PayloadFilterRule{
			{Models: models("*"), Params: []string{"stream_options", "top_p"}},
		},
	}
	result := Explain(rules, Request{
		Model:          "gpt-5",
		RequestedModel: "fast(high)",
		Protocol:       "openai",
		Body:           []byte(`{"temperature":1,"tools":[],"stream_options":{"include_usage":true}}`),
	})

	if want := []string{"gpt-5", "fast", "fast(high)"}; !reflect.DeepEqual(result.Models, want) {
		t.Fatalf("models = %v, want %v", result.Models, want)
	}
	want := []RuleOutcome{
		{Section: SectionDefault, Index: 0, Matched: true, Pattern: "gpt-*", Model: "gpt-5", Params: []ParamOutcome{
			{Path: "max_tokens", Action: ActionSet, Value: "4096"},
			{Path: "temperature", Action: ActionSkipped, Reason: SkipPresent},
		}},
		{Section: SectionDefault, Index: 1, Matched: true, Pattern: "*", Model: "gpt-5", Params: []ParamOutcome{
			{Path: "max_tokens", Action: ActionSkipped, Reason: SkipAlreadyDefaulted},
		}},
		{Section: SectionOverride, Index: 0, Miss: MissProtocol},
		{Section: SectionOverride, Index: 1, Miss: MissModel},
		{Section: SectionOverride, Index: 2, Miss: MissNoModels},
		{Section: SectionOverride, Index: 3, Matched: true, Pattern: "fast", Model: "fast", Params: []ParamOutcome{
			{Path: "text.verbosity", Action: ActionSet, Value: `"low"`},
		}},
		{Section: SectionOverrideRaw, Index: 0, Matched: true, Pattern: "*", Model: "gpt-5", Params: []ParamOutcome{
			// gjson cannot read sjson's append index back, so this is the rule's value.
			{Path: "tools.-1", Action: ActionSet, Value: `{"type":"web_search"}`},
		}},
		{Section: SectionFilter, Index: 0, Matched: true, Pattern: "*", Model: "gpt-5", Params: []ParamOutcome{
			{Path: "stream_options", Action: ActionRemoved},
			{Path: "top_p", Action: ActionSkipped, Reason: SkipAbsent},
		}},
	}
	if !reflect.DeepEqual(result.Rules, want) {
		t.Fatalf("rules =\n%+v\nwant\n%+v", result.Rules, want)
	}

	wantChanges := []Change{
		{Path: "tools.0", After: ptr(`{"type":"web_search"}`)},
		{Path: "max_tokens", After: ptr("4096")},
		{Path: "text", After: ptr(`{"verbosity":"low"}`)},
		{Path: "stream_options", Before: ptr(`{"include_usage":true}`)},
	}
	if !reflect.DeepEqual(result.Changes, wantChanges) {
		t.Fatalf("changes = %s, want %s", describeChanges(result.Changes), describeChanges(wantChanges))
	}
}

func TestExplainReportsRawValuesItCouldNotWrite(t *testing.T) {
	rules := config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"x": nil}}}}
	result := Explain(rules, Request{Model: "m", Body: []byte(`{}`)})
	want := []ParamOutcome{{Path: "x", Action: ActionSkipped, Reason: SkipInvalidValue}}
	if len(result.Rules) != 1 || !reflect.DeepEqual(result.Rules[0].Params, want) {
		t.Fatalf("rules = %+v", result.Rules)
	}
	if len(result.Changes) != 0 || string(result.Body) != `{}` {
		t.Fatalf("body = %s, changes = %v", result.Body, result.Changes)
	}
}

func TestExplainUnderARootReportsFullPaths(t *testing.T) {
	rules := config.PayloadConfig{Override: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"generationConfig.seed": 7}}}}
	result := Explain(rules, Request{Model: "m", Root: "request", Body: []byte(`{"request":{}}`)})
	if got := result.Rules[0].Params[0].Path; got != "request.generationConfig.seed" {
		t.Fatalf("path = %q", got)
	}
	if want := []Change{{Path: "request.generationConfig", After: ptr(`{"seed":7}`)}}; !reflect.DeepEqual(result.Changes, want) {
		t.Fatalf("changes = %s", describeChanges(result.Changes))
	}
}

func TestDiffJSON(t *testing.T) {
	cases := []struct {
		name          string
		before, after string
		want          []Change
	}{
		{"same", `{"a":1,"b":[1,{"c":true}]}`, `{"b":[1,{"c":true}],"a":1}`, nil},
		{"changed value", `{"a":{"b":1}}`, `{"a":{"b":2}}`, []Change{{Path: "a.b", Before: ptr("1"), After: ptr("2")}}},
		{"type change", `{"a":"1"}`, `{"a":1}`, []Change{{Path: "a", Before: ptr(`"1"`), After: ptr("1")}}},
		{"null is a value", `{"a":null}`, `{}`, []Change{{Path: "a", Before: ptr("null")}}},
		{"array grows and shrinks", `{"a":[1,2,3]}`, `{"a":[1,9]}`, []Change{
			{Path: "a.1", Before: ptr("2"), After: ptr("9")},
			{Path: "a.2", Before: ptr("3")},
		}},
		{"keys with path syntax", `{"user.id":"a","x*?":1}`, `{"user.id":"b"}`, []Change{
			{Path: `user\.id`, Before: ptr(`"a"`), After: ptr(`"b"`)},
			{Path: `x\*\?`, Before: ptr("1")},
		}},
		{"number text", `{"t":1.0}`, `{"t":1}`, []Change{{Path: "t", Before: ptr("1.0"), After: ptr("1")}}},
	}
	for _, tc := range cases {
		got := diffJSON([]byte(tc.before), []byte(tc.after))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: changes = %s, want %s", tc.name, describeChanges(got), describeChanges(tc.want))
		}
	}
}

func describeChanges(changes []Change) string {
	out := "["
	for i, change := range changes {
		if i > 0 {
			out += ", "
		}
		before, after := "∅", "∅"
		if change.Before != nil {
			before = *change.Before
		}
		if change.After != nil {
			after = *change.After
		}
		out += change.Path + ": " + before + " → " + after
	}
	return out + "]"
}

func TestExplainShowsTheValueAsWritten(t *testing.T) {
	rules := config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models("*"), Params: map[string]any{"pad": "  [1, 2]  "}}}}
	result := Explain(rules, Request{Model: "m", Body: []byte(`{}`)})
	if got := result.Rules[0].Params[0].Value; got != "[1, 2]" {
		t.Fatalf("value = %q, want what the body holds", got)
	}
}
