package diff

import (
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Turning a channel's cooling on or off shows up in the reload's debug log, so
// an operator can tell from the log whether the change was picked up.
func TestBuildConfigChangeDetailsReportsDisableCooling(t *testing.T) {
	on := true
	oldCfg := &config.Config{
		ClaudeKey:           []config.ClaudeKey{{APIKey: "claude-key"}},
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "relay", BaseURL: "https://relay.example/v1"}},
	}
	newCfg := &config.Config{
		ClaudeKey:           []config.ClaudeKey{{APIKey: "claude-key", DisableCooling: &on}},
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "relay", BaseURL: "https://relay.example/v1", DisableCooling: &on}},
	}
	details := BuildConfigChangeDetails(oldCfg, newCfg)
	if !slices.Contains(details, "claude[0].disable-cooling: inherit -> true") {
		t.Fatalf("claude change missing from %v", details)
	}
	found := false
	for _, detail := range details {
		if strings.Contains(detail, "relay (disable-cooling inherit -> true)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("openai-compatibility change missing from %v", details)
	}
}

// Unset and 0 mean different things for streaming.bootstrap-retries (default
// on versus off), so the reload log has to show which one took effect.
func TestBuildConfigChangeDetailsReportsBootstrapRetries(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{}
	newCfg.Streaming.BootstrapRetries = new(0)
	if details := BuildConfigChangeDetails(oldCfg, newCfg); !slices.Contains(details, "streaming.bootstrap-retries: default -> 0") {
		t.Fatalf("unset -> 0 missing from %v", details)
	}
	oldCfg.Streaming.BootstrapRetries = new(0)
	if details := BuildConfigChangeDetails(oldCfg, newCfg); slices.ContainsFunc(details, func(d string) bool { return strings.Contains(d, "bootstrap-retries") }) {
		t.Fatalf("an unchanged value was reported: %v", details)
	}
}

func TestBuildConfigChangeDetailsReportsChannelGroupStreamGuards(t *testing.T) {
	oldCfg := &config.Config{Routing: config.RoutingConfig{ChannelGroups: []config.RoutingChannelGroup{{Name: "team"}}}}
	newCfg := &config.Config{Routing: config.RoutingConfig{ChannelGroups: []config.RoutingChannelGroup{
		{Name: "team", FirstTokenTimeoutSeconds: 30, BusyWaitMaxSeconds: new(0)},
	}}}
	details := BuildConfigChangeDetails(oldCfg, newCfg)
	for _, want := range []string{
		"routing.channel-groups[team].first-token-timeout-seconds: 0 -> 30",
		"routing.channel-groups[team].busy-wait-max-seconds: default -> 0",
	} {
		if !slices.Contains(details, want) {
			t.Fatalf("%q missing from %v", want, details)
		}
	}
}

// A reload that changes the waiter cap has to say so in the log, and 0, unset
// and a negative number are all "the default", so moving between them is not a
// change.
func TestBuildConfigChangeDetailsReportsBusyWaitMaxWaiters(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{}
	newCfg.Streaming.BusyWaitMaxWaiters = 32
	details := BuildConfigChangeDetails(oldCfg, newCfg)
	if !slices.Contains(details, "streaming.busy-wait-max-waiters: default -> 32") {
		t.Fatalf("unset -> 32 missing from %v", details)
	}
	oldCfg.Streaming.BusyWaitMaxWaiters = 32
	newCfg.Streaming.BusyWaitMaxWaiters = 8
	if details := BuildConfigChangeDetails(oldCfg, newCfg); !slices.Contains(details, "streaming.busy-wait-max-waiters: 32 -> 8") {
		t.Fatalf("32 -> 8 missing from %v", details)
	}
	oldCfg.Streaming.BusyWaitMaxWaiters = -3
	newCfg.Streaming.BusyWaitMaxWaiters = 0
	if details := BuildConfigChangeDetails(oldCfg, newCfg); slices.ContainsFunc(details, func(d string) bool { return strings.Contains(d, "busy-wait-max-waiters") }) {
		t.Fatalf("a move between two spellings of the default was reported: %v", details)
	}
}
