package config

import "testing"

// A Codex row is usable when it carries a credential. An empty base-url means
// "use the default Codex endpoint" (see CodexKey.BaseURL), which is also what
// the runtime synthesizer assumes, so sanitising must not drop those rows.
func TestSanitizeCodexKeysKeepsRowsWithoutBaseURL(t *testing.T) {
	cfg := &Config{
		CodexKey: []CodexKey{
			{APIKey: " default-endpoint ", Prefix: " team "},
			{APIKey: "third-party", BaseURL: " https://codex.example ", ProxyURL: " http://proxy.example "},
			{APIKey: " ", BaseURL: "https://no-credential.example"},
		},
	}

	cfg.SanitizeCodexKeys()

	if len(cfg.CodexKey) != 2 {
		t.Fatalf("CodexKey length = %d, want 2 (%#v)", len(cfg.CodexKey), cfg.CodexKey)
	}
	if got := cfg.CodexKey[0]; got.APIKey != "default-endpoint" || got.BaseURL != "" {
		t.Fatalf("default-endpoint row = %#v, want kept with empty base url", got)
	}
	if cfg.CodexKey[0].Prefix != "team" {
		t.Fatalf("prefix = %q, want team", cfg.CodexKey[0].Prefix)
	}
	if got := cfg.CodexKey[1]; got.BaseURL != "https://codex.example" || got.ProxyURL != "http://proxy.example" {
		t.Fatalf("third-party row = %#v, want trimmed url fields", got)
	}
}
