package quota

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
)

func TestNormalizeModelNameStripsSuffixesLikeThinking(t *testing.T) {
	cases := []string{
		"claude-sonnet-4-5(16384)",
		"gpt-5.2(high)",
		"gemini-2.5-pro",
		"deepseek-v4-flash[1M]",
		"model[128K](8192)",
		"model[1M](8192)",
		"model[beta]",
		"  Claude-Opus-4-5  ",
	}
	for _, raw := range cases {
		want := thinking.ParseSuffix(raw).ModelName
		want = NormalizeModelName(want) // 再归一化大小写/空白，只比较后缀剥离的一致性
		if got := NormalizeModelName(raw); got != want {
			t.Fatalf("NormalizeModelName(%q) = %q, thinking says %q", raw, got, want)
		}
	}
	if got := NormalizeModelName("Claude-Opus-4-5(HIGH)"); got != "claude-opus-4-5" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeModelName("model[beta]"); got != "model[beta]" {
		t.Fatalf("non-marker bracket must stay: %q", got)
	}
}

func TestModelPatternMatches(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"claude-opus-4-5", "claude-opus-4-5", true},
		{"claude-opus-4-5", "CLAUDE-OPUS-4-5(high)", true},
		{"claude-opus-*", "claude-opus-4-1", true},
		{"claude-opus-*", "claude-sonnet-4-5", false},
		{"*opus*", "claude-opus-4-5-20251101", true},
		{"gpt-5*", "gpt-5", true},
		{"gpt-5*", "gpt-4o", false},
		{"gpt-oss:20b", "ollama/gpt-oss:20b", true},
		{"ollama/gpt-oss:20b", "gpt-oss:20b", false},
		{"ollama/*", "ollama/gpt-oss:20b", true},
		{"claude-opus-4-5", "claude-opus-4-5-thinking", false},
		{"", "gpt-5", false},
		{"gpt-5", "", false},
		{"a**b", "axxb", true},
	}
	for _, tc := range cases {
		if got := ModelPatternMatches(tc.pattern, tc.model); got != tc.want {
			t.Fatalf("ModelPatternMatches(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
	if !MatchesAnyModelPattern([]string{"x", "gpt-*"}, "gpt-4o") || MatchesAnyModelPattern(nil, "gpt-4o") {
		t.Fatal("MatchesAnyModelPattern mismatch")
	}
}

func TestGlobMatchBacktracking(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "", true},
		{"a*", "a", true},
		{"*a", "ba", true},
		{"*ab*cd", "xxabyycd", true},
		{"*ab*cd", "xxabyyc", false},
		{"a*b*c", "abbbc", true},
		{"a*b*c", "acb", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}
