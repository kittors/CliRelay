package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func validVirtualModel() VirtualModel {
	return VirtualModel{
		Name: "smart",
		Members: []VirtualModelMember{
			{Group: "team", Model: "gpt-5"},
			{Model: "claude-sonnet-4-5"},
		},
	}
}

func TestValidateVirtualModelsAcceptsAValidList(t *testing.T) {
	known := map[string]struct{}{"team": {}, "default": {}}
	weighted := validVirtualModel()
	weighted.Name = "spread"
	weighted.Strategy = "Weighted"
	weighted.Members[0].Weight = 3
	// A virtual model may name its own name as a member: that is how it adds
	// fallbacks to an existing model.
	selfNamed := VirtualModel{Name: "o3", Members: []VirtualModelMember{{Group: "team", Model: "o3"}, {Model: "o4-mini"}}}
	if err := ValidateVirtualModels([]VirtualModel{weighted, selfNamed}, known); err != nil {
		t.Fatalf("ValidateVirtualModels: %v", err)
	}
	// Another virtual model naming it as a member would read as nesting,
	// though a member always runs the real model.
	nesting := validVirtualModel()
	nesting.Members[1].Model = "o3"
	err := ValidateVirtualModels([]VirtualModel{nesting, selfNamed}, known)
	if err == nil || !strings.Contains(err.Error(), "cannot contain other virtual models") {
		t.Fatalf("err = %v, want the nesting refusal", err)
	}
}

// Each rule is checked as the operator sent it, before sanitizing would drop
// the offending entry without a word.
func TestValidateVirtualModelsRejects(t *testing.T) {
	known := map[string]struct{}{"team": {}}
	cases := []struct {
		name   string
		mutate func(models []VirtualModel) []VirtualModel
		want   string
	}{
		{"empty name", func(m []VirtualModel) []VirtualModel { m[0].Name = "  "; return m }, "needs a name"},
		{"slash in name", func(m []VirtualModel) []VirtualModel { m[0].Name = "team/smart"; return m }, "must not contain '/'"},
		{"reserved name", func(m []VirtualModel) []VirtualModel { m[0].Name = "Auto"; return m }, "reserved"},
		{"space in name", func(m []VirtualModel) []VirtualModel { m[0].Name = "my model"; return m }, "printable ASCII"},
		{"non-ASCII name", func(m []VirtualModel) []VirtualModel { m[0].Name = "智能"; return m }, "printable ASCII"},
		{"too long", func(m []VirtualModel) []VirtualModel { m[0].Name = strings.Repeat("a", 129); return m }, "longer than"},
		{"duplicate name, any case", func(m []VirtualModel) []VirtualModel {
			other := validVirtualModel()
			other.Name = "SMART"
			return append(m, other)
		}, "defined twice"},
		{"no members", func(m []VirtualModel) []VirtualModel { m[0].Members = nil; return m }, "at least one member"},
		{"too many members", func(m []VirtualModel) []VirtualModel {
			m[0].Members = nil
			for i := 0; i <= MaxVirtualModelMembers; i++ {
				m[0].Members = append(m[0].Members, VirtualModelMember{Model: "m" + strings.Repeat("x", i)})
			}
			return m
		}, "more than"},
		{"member without model", func(m []VirtualModel) []VirtualModel { m[0].Members[1].Model = " "; return m }, "needs a model"},
		{"member model with space", func(m []VirtualModel) []VirtualModel { m[0].Members[1].Model = "gpt 5"; return m }, "printable ASCII"},
		{"unknown group", func(m []VirtualModel) []VirtualModel { m[0].Members[0].Group = "nope"; return m }, `channel group "nope" does not exist`},
		{"group prefix in model conflicts with group", func(m []VirtualModel) []VirtualModel {
			m[0].Members[0].Model = "other/gpt-5"
			return m
		}, "put the group in the group field"},
		{"member repeated", func(m []VirtualModel) []VirtualModel {
			m[0].Members = append(m[0].Members, VirtualModelMember{Group: "TEAM", Model: "GPT-5"})
			return m
		}, "twice"},
		{"negative weight", func(m []VirtualModel) []VirtualModel { m[0].Members[0].Weight = -1; return m }, "weight"},
		{"weight too large", func(m []VirtualModel) []VirtualModel {
			m[0].Members[0].Weight = MaxVirtualModelMemberWeight + 1
			return m
		}, "weight"},
		{"weighted without a weight", func(m []VirtualModel) []VirtualModel { m[0].Strategy = "weighted"; return m }, "weight above 0"},
		{"nesting", func(m []VirtualModel) []VirtualModel {
			inner := validVirtualModel()
			inner.Name = "inner"
			m[0].Members[1].Model = "Inner"
			return append(m, inner)
		}, "cannot contain other virtual models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models := tc.mutate([]VirtualModel{validVirtualModel()})
			err := ValidateVirtualModels(models, known)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	tooMany := make([]VirtualModel, MaxVirtualModels+1)
	if err := ValidateVirtualModels(tooMany, known); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("err = %v, want the count limit", err)
	}
}

// Stored configs that did not come through the management API (an import, a
// hand-edited row) are brought into shape rather than refused.
func TestSanitizeRoutingCleansVirtualModels(t *testing.T) {
	routing := NormalizeRoutingConfig(RoutingConfig{
		IncludeDefaultGroup: true,
		VirtualModels: []VirtualModel{
			{Name: "  smart ", Description: "  picks  ", Strategy: "ORDER", Members: []VirtualModelMember{
				{Group: " Team ", Model: " gpt-5 ", Weight: -4},
				{Group: "team", Model: "GPT-5"},
				{Model: "  "},
				{Model: "claude", Weight: 5000},
			}},
			{Name: "SMART", Members: []VirtualModelMember{{Model: "dup"}}},
			{Name: "", Members: []VirtualModelMember{{Model: "nameless"}}},
			{Name: "empty"},
			{Name: "spread", Strategy: " weighted ", Members: []VirtualModelMember{{Model: "a", Weight: 2}}},
		},
	})
	if len(routing.VirtualModels) != 2 {
		t.Fatalf("virtual models = %+v, want smart and spread", routing.VirtualModels)
	}
	smart := routing.VirtualModels[0]
	if smart.Name != "smart" || smart.Description != "picks" || smart.Strategy != "" {
		t.Fatalf("smart = %+v, want trimmed with the order strategy stored as empty", smart)
	}
	want := []VirtualModelMember{{Group: "team", Model: "gpt-5"}, {Model: "claude", Weight: MaxVirtualModelMemberWeight}}
	if len(smart.Members) != len(want) || smart.Members[0] != want[0] || smart.Members[1] != want[1] {
		t.Fatalf("members = %+v, want %+v", smart.Members, want)
	}
	if spread := routing.VirtualModels[1]; spread.Strategy != VirtualModelStrategyWeighted || !spread.Weighted() {
		t.Fatalf("spread = %+v, want the weighted strategy", spread)
	}
	if empty := NormalizeRoutingConfig(RoutingConfig{VirtualModels: []VirtualModel{}}); empty.VirtualModels != nil {
		t.Fatalf("an empty list must normalize to none, got %+v", empty.VirtualModels)
	}
}

func TestRoutingConfigVirtualModelLookups(t *testing.T) {
	routing := RoutingConfig{VirtualModels: []VirtualModel{
		{Name: "smart", Members: []VirtualModelMember{{Group: "team", Model: "a"}, {Group: "ops", Model: "b"}}},
		{Name: "cheap", Members: []VirtualModelMember{{Group: "team", Model: "c"}, {Model: "d"}}},
	}}
	if vm, ok := routing.VirtualModel(" smart "); !ok || vm.Name != "smart" {
		t.Fatalf("VirtualModel(smart) = %+v, %v", vm, ok)
	}
	// Names match exactly, as model ids do everywhere else.
	if _, ok := routing.VirtualModel("SMART"); ok {
		t.Fatalf("VirtualModel matched another case")
	}
	groups := routing.VirtualModelGroups()
	if len(groups) != 2 || groups["team"] != "smart" || groups["ops"] != "smart" {
		t.Fatalf("VirtualModelGroups = %v, want team and ops, each with the first model naming it", groups)
	}
}

// The routing config is stored as JSON; virtual models survive the round trip
// and an old payload without them reads as none.
func TestVirtualModelsRoundTripThroughTheStoredPayload(t *testing.T) {
	in := RoutingConfig{IncludeDefaultGroup: true, VirtualModels: []VirtualModel{{
		Name: "spread", Description: "d", Strategy: VirtualModelStrategyWeighted,
		Members: []VirtualModelMember{{Group: "team", Model: "a", Weight: 3}, {Model: "b"}},
	}}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"virtual-models"`) {
		t.Fatalf("payload %s has no virtual-models", data)
	}
	var out RoutingConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !SameRoutingConfig(in, out) {
		t.Fatalf("round trip changed the config: %+v", out)
	}
	var old RoutingConfig
	if err := json.Unmarshal([]byte(`{"include-default-group":true}`), &old); err != nil || old.VirtualModels != nil {
		t.Fatalf("old payload = %+v, %v", old, err)
	}
}
