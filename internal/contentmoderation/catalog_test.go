package contentmoderation

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListChannelsPaginatesAndNormalizesGeminiVirtualParent(t *testing.T) {
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{ID: "provider-key-1", Name: "Gemini Provider", APIKey: "secret"}}}
	auths := []*coreauth.Auth{
		{ID: "parent-auth", FileName: "gemini.json", Provider: "gemini-cli", Label: "Gemini Account", Metadata: map[string]any{"display_tags": []string{"team-a"}}},
		{ID: "virtual-auth", Provider: "gemini-cli", Label: "Virtual", Attributes: map[string]string{"path": "gemini.json", "gemini_virtual_parent": "parent-auth"}},
	}
	page := ListChannels(cfg, auths, nil, ChannelQuery{Page: 1, PageSize: 1})
	if page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("page = %#v", page)
	}
	full := ListChannels(cfg, auths, nil, ChannelQuery{Page: 1, PageSize: 50})
	authCount := 0
	for _, channel := range full.Items {
		if channel.ChannelType == ChannelTypeAuthFile {
			authCount++
			if channel.ChannelID != "parent-auth" {
				t.Fatalf("auth channel id = %q, want parent-auth", channel.ChannelID)
			}
		}
	}
	if authCount != 1 {
		t.Fatalf("auth channel count = %d, want 1", authCount)
	}
}

func TestListChannelsFiltersTagsAndBindings(t *testing.T) {
	auths := []*coreauth.Auth{{
		ID:       "auth-1",
		FileName: "auth.json",
		Provider: "codex",
		Label:    "Codex Team",
		Metadata: map[string]any{"display_tags": []string{"team-a", "pro"}},
	}}
	bindings := []Binding{{ChannelType: ChannelTypeAuthFile, ChannelID: "auth-1", ProfileID: "profile-1"}}
	page := ListChannels(&config.Config{}, auths, bindings, ChannelQuery{Tags: []string{"team-a", "pro"}, TagMode: "all", ProfileID: "profile-1"})
	if page.Total != 1 || page.Items[0].ProfileID != "profile-1" {
		t.Fatalf("filtered page = %#v", page)
	}
}
