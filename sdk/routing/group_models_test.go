package routing

import "testing"

func TestChannelGroupExcludesModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		excluded []string
		model    string
		want     bool
	}{
		{"exact id", []string{"grok-4.7"}, "grok-4.7", true},
		{"case and spacing", []string{" GROK-4.7 "}, "grok-4.7", true},
		{"another model", []string{"grok-4.7"}, "grok-4.6", false},
		{"no exclusions", nil, "grok-4.7", false},
		{"blank request", []string{"*"}, "  ", false},
		{"blank entries are ignored", []string{"", "  "}, "grok-4.7", false},

		// The panel lists a prefixed credential's models with the prefix, while a
		// client routed by group path asks without it, and the reverse. Either way
		// round the exclusion has to hold.
		{"prefixed entry, bare request", []string{"xai/grok-imagine-video-1.5"}, "grok-imagine-video-1.5", true},
		{"bare entry, prefixed request", []string{"grok-imagine-video-1.5"}, "xai/grok-imagine-video-1.5", true},
		{"different prefix on each side", []string{"xai/grok-4.7"}, "team/grok-4.7", true},
		{"a prefix is not a partial match", []string{"grok-4"}, "xai/grok-4.7", false},

		// Same wildcard as provider-level excluded-models.
		{"whole group", []string{"*"}, "xai/grok-4.7", true},
		{"family wildcard", []string{"grok-imagine-*"}, "grok-imagine-video-1.5", true},
		{"family wildcard through a prefix", []string{"grok-imagine-*"}, "xai/grok-imagine-image", true},
		{"family wildcard leaves the rest", []string{"grok-imagine-*"}, "grok-4.7", false},
		{"suffix wildcard", []string{"*-thinking"}, "claude-opus-4-6-thinking", true},
		{"infix wildcard", []string{"*flash*"}, "gemini-3-flash-preview", true},

		// Loose on purpose: a vendor namespace reads like a route prefix, and
		// blocking one model too many is the safe way to be wrong here.
		{"vendor namespace counts as the bare id", []string{"claude-3.5-sonnet"}, "anthropic/claude-3.5-sonnet", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ChannelGroupExcludesModel(tt.excluded, tt.model); got != tt.want {
				t.Fatalf("ChannelGroupExcludesModel(%q, %q) = %v, want %v", tt.excluded, tt.model, got, tt.want)
			}
		})
	}
}

func TestMatchWildcard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern, value string
		want           bool
	}{
		{"", "", false},
		{"a", "a", true},
		{"a", "b", false},
		{"*", "", true},
		{"*", "anything", true},
		{"a*", "abc", true},
		{"a*", "bac", false},
		{"*c", "abc", true},
		{"a*c", "ac", true},
		{"ab*bc", "abc", false}, // prefix and suffix may not overlap
		{"ab*bc", "abbc", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxcyyb", false},
		{"A*", "abc", false}, // case-sensitive: callers lower-case first
	}
	for _, tt := range tests {
		if got := MatchWildcard(tt.pattern, tt.value); got != tt.want {
			t.Errorf("MatchWildcard(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}
