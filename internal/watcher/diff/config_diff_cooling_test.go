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
