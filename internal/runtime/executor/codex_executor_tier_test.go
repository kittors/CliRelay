package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolveCodexTierModel_FreeTierPassThrough(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"plan_type": "free"}}
	tests := []struct {
		requested string
		want      string
	}{
		{requested: "gpt-5.3-codex", want: "gpt-5.3-codex"},
		{requested: "gpt-5.3-codex-spark", want: "gpt-5.3-codex-spark"},
		{requested: "gpt-5.4", want: "gpt-5.4"},
		{requested: "gpt-5.2-codex", want: "gpt-5.2-codex"},
	}
	for _, tt := range tests {
		if got := resolveCodexTierModel(tt.requested, auth); got != tt.want {
			t.Fatalf("resolveCodexTierModel(%q) = %q, want %q", tt.requested, got, tt.want)
		}
	}
}
