package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func synthesizeCommandCode(t *testing.T, cfg *config.Config) []*commandCodeAuthView {
	t.Helper()
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	out := make([]*commandCodeAuthView, 0, len(auths))
	for _, auth := range auths {
		if auth != nil && auth.Provider == "commandcode" {
			out = append(out, &commandCodeAuthView{
				Label:      auth.Label,
				Prefix:     auth.Prefix,
				ProxyURL:   auth.ProxyURL,
				Attributes: auth.Attributes,
				Disabled:   auth.Disabled,
			})
		}
	}
	return out
}

type commandCodeAuthView struct {
	Label      string
	Prefix     string
	ProxyURL   string
	Attributes map[string]string
	Disabled   bool
}

// The auth must carry compat metadata, or the router has no way to pick the
// shared OpenAI-compatible executor for it.
func TestConfigSynthesizerCommandCodeKeysCarryCompatMetadata(t *testing.T) {
	auths := synthesizeCommandCode(t, &config.Config{
		CommandCodeKey: []config.CommandCodeKey{{
			APIKey:   "cc-key",
			Name:     "GOAT plan",
			Prefix:   "cc",
			ProxyURL: "socks5://127.0.0.1:1080",
		}},
	})

	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Label != "GOAT plan" || auth.Prefix != "cc" {
		t.Fatalf("label/prefix = %q/%q", auth.Label, auth.Prefix)
	}
	if auth.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy = %q, want the configured one", auth.ProxyURL)
	}
	want := map[string]string{
		"api_key":      "cc-key",
		"base_url":     config.DefaultCommandCodeBaseURL,
		"compat_name":  "Command Code",
		"provider_key": "commandcode",
	}
	for key, value := range want {
		if auth.Attributes[key] != value {
			t.Fatalf("attribute %q = %q, want %q", key, auth.Attributes[key], value)
		}
	}
}

func TestConfigSynthesizerCommandCodeKeysSkipEmptyAndPropagateDisabled(t *testing.T) {
	auths := synthesizeCommandCode(t, &config.Config{
		CommandCodeKey: []config.CommandCodeKey{
			{APIKey: "   "},
			{APIKey: "cc-key", Disabled: true},
		},
	})

	if len(auths) != 1 {
		t.Fatalf("auths = %d, want the blank credential skipped", len(auths))
	}
	if !auths[0].Disabled {
		t.Fatal("a disabled credential must not synthesize an active auth")
	}
}

// An operator who points the channel at a mirror must have that honoured, since
// the executor reads the endpoint from this attribute.
func TestConfigSynthesizerCommandCodeKeysHonourCustomBaseURL(t *testing.T) {
	auths := synthesizeCommandCode(t, &config.Config{
		CommandCodeKey: []config.CommandCodeKey{{
			APIKey:  "cc-key",
			BaseURL: "https://mirror.example.com/provider/v1/",
		}},
	})

	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	if got := auths[0].Attributes["base_url"]; got != "https://mirror.example.com/provider/v1" {
		t.Fatalf("base_url = %q, want the trimmed mirror URL", got)
	}
}
