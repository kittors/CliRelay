package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFollowUpstreamModelsDefaultsToTheRootGroupOnly(t *testing.T) {
	t.Parallel()

	on, off := true, false
	cases := []struct {
		group RoutingChannelGroup
		want  bool
	}{
		{RoutingChannelGroup{Name: "default"}, true},
		{RoutingChannelGroup{Name: "Default/"}, true},
		{RoutingChannelGroup{Name: "default", FollowUpstreamModels: &off}, false},
		{RoutingChannelGroup{Name: "team"}, false},
		{RoutingChannelGroup{Name: "team", FollowUpstreamModels: &on}, true},
	}
	for _, tc := range cases {
		if got := tc.group.FollowsUpstreamModels(); got != tc.want {
			t.Errorf("%q (setting %v) follows = %v, want %v", tc.group.Name, tc.group.FollowUpstreamModels, got, tc.want)
		}
	}
}

// A panel that writes every switch explicitly and one that writes none must
// store the same config: SameRoutingConfig compares encodings, and a setting
// equal to the group's default would otherwise read as a change.
func TestSanitizeRoutingKeepsOnlyAFollowSettingThatDepartsFromTheDefault(t *testing.T) {
	t.Parallel()

	on, off := true, false
	normalized := NormalizeRoutingConfig(RoutingConfig{ChannelGroups: []RoutingChannelGroup{
		{Name: "default", FollowUpstreamModels: &on},
		{Name: "frozen-root", FollowUpstreamModels: &off},
		{Name: "team", FollowUpstreamModels: &off},
		{Name: "following", FollowUpstreamModels: &on},
	}})
	stored := map[string]*bool{}
	for _, group := range normalized.ChannelGroups {
		stored[group.Name] = group.FollowUpstreamModels
	}
	if stored["default"] != nil || stored["team"] != nil {
		t.Fatalf("a setting equal to the default was kept: default=%v team=%v", stored["default"], stored["team"])
	}
	// "frozen-root" is an ordinary group, so off is already its default.
	if stored["frozen-root"] != nil {
		t.Fatalf("frozen-root kept %v, want unset", *stored["frozen-root"])
	}
	if stored["following"] == nil || !*stored["following"] {
		t.Fatalf("following lost its setting: %v", stored["following"])
	}

	rootOff := NormalizeRoutingConfig(RoutingConfig{ChannelGroups: []RoutingChannelGroup{{Name: "default", FollowUpstreamModels: &off}}})
	if setting := rootOff.ChannelGroups[0].FollowUpstreamModels; setting == nil || *setting {
		t.Fatalf("switching the root group off was dropped: %v", setting)
	}

	explicit := RoutingConfig{ChannelGroups: []RoutingChannelGroup{{Name: "default", FollowUpstreamModels: &on}, {Name: "team", FollowUpstreamModels: &off}}}
	unset := RoutingConfig{ChannelGroups: []RoutingChannelGroup{{Name: "default"}, {Name: "team"}}}
	if !SameRoutingConfig(explicit, unset) {
		t.Fatal("explicit defaults compared different from unset ones")
	}
}

// The routing store keeps the config as JSON and the panel writes it under this
// key; a renamed tag would drop the setting on the next save.
func TestFollowUpstreamModelsRoundTripsUnderItsWireName(t *testing.T) {
	t.Parallel()

	var cfg RoutingConfig
	if err := json.Unmarshal([]byte(`{"channel-groups":[{"name":"team","allowed-models":["gpt-6.1-sol"],"follow-upstream-models":true}]}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	normalized := NormalizeRoutingConfig(cfg)
	if !normalized.ChannelGroups[0].FollowsUpstreamModels() {
		t.Fatal("follow-upstream-models was not read")
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(encoded), `"follow-upstream-models":true`) {
		t.Fatalf("encoded config lost the setting: %s", encoded)
	}
}
