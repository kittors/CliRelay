package auth

import (
	"testing"
)

func TestModelAntigravityQuotaFamily(t *testing.T) {
	tests := []struct {
		model string
		want  AntigravityQuotaFamily
	}{
		{"gemini-2.5-pro", AntigravityFamilyGemini},
		{"gemini-2.5-flash", AntigravityFamilyGemini},
		{"gemini-3.1-pro-high", AntigravityFamilyGemini},
		{"claude-3-7-sonnet", AntigravityFamilyClaude},
		{"claude-3-5-sonnet", AntigravityFamilyClaude},
		{"claude-opus-4-6-thinking", AntigravityFamilyClaude},
		{"gpt-4o", AntigravityFamilyClaude},
		{"unknown-model", AntigravityFamilyOther},
	}

	for _, tt := range tests {
		got := ModelAntigravityQuotaFamily(tt.model)
		if got != tt.want {
			t.Errorf("ModelAntigravityQuotaFamily(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestAreAntigravityModelsSameFamily(t *testing.T) {
	if !AreAntigravityModelsSameFamily("gemini-2.5-pro", "gemini-2.5-flash") {
		t.Error("expected gemini models to share family")
	}
	if !AreAntigravityModelsSameFamily("claude-3-7-sonnet", "gpt-4o") {
		t.Error("expected claude and gpt to share family")
	}
	if AreAntigravityModelsSameFamily("gemini-2.5-pro", "claude-3-7-sonnet") {
		t.Error("expected gemini and claude NOT to share family")
	}
}
