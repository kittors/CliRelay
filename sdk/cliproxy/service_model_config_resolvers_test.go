package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestResolveConfigClaudeKey_PrefersExactBaseURLMatch(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "same-key", BaseURL: "https://claude-a.example.com", Name: "first"},
				{APIKey: "same-key", BaseURL: "https://claude-b.example.com", Name: "second"},
			},
		},
	}
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			"api_key":  "same-key",
			"base_url": "https://claude-b.example.com",
		},
	}

	got := service.resolveConfigClaudeKey(auth)
	if got == nil {
		t.Fatal("expected matching claude key")
	}
	if got.Name != "second" {
		t.Fatalf("matched claude key = %q, want second", got.Name)
	}
}

func TestResolveConfigBedrockKey_UsesAuthModeRegionAndCredential(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			BedrockKey: []config.BedrockKey{
				{
					Name:        "sigv4-default-region",
					AuthMode:    "sigv4",
					AccessKeyID: "AKIA-SIGV4",
				},
				{
					Name:     "api-key-eu-west-1",
					AuthMode: "api-key",
					APIKey:   "bedrock-api-key",
					BaseURL:  "https://bedrock.example.com",
					Region:   "eu-west-1",
					ProxyURL: "https://proxy.example.com",
					Prefix:   "teamA",
				},
			},
		},
	}
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			"auth_mode": "api_key",
			"api_key":   "bedrock-api-key",
			"base_url":  "https://bedrock.example.com",
			"region":    "eu-west-1",
		},
	}

	got := service.resolveConfigBedrockKey(auth)
	if got == nil {
		t.Fatal("expected matching bedrock key")
	}
	if got.Name != "api-key-eu-west-1" {
		t.Fatalf("matched bedrock key = %q, want api-key-eu-west-1", got.Name)
	}
}

func TestOAuthExcludedModels_IgnoresAPIKeyAuth(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"claude": {"claude-*"},
			},
		},
	}

	if got := service.oauthExcludedModels("claude", "apikey"); got != nil {
		t.Fatalf("oauthExcludedModels for apikey = %#v, want nil", got)
	}
	got := service.oauthExcludedModels("claude", "oauth")
	if len(got) != 1 || got[0] != "claude-*" {
		t.Fatalf("oauthExcludedModels for oauth = %#v, want [claude-*]", got)
	}
}
