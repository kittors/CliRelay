package usage

import "testing"

// xAI bills every product against one weekly pool, so the projection divisor
// has to be the per-product window the probe tagged as attributable, not the
// pool-wide weekly_limit the card cycle anchors on.
func TestMatchesProjectionQuotaKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		provider string
		quotaKey string
		want     bool
	}{
		{"xai", "product:GrokBuild", true},
		{"Grok", "product:grok-code", true},
		{"xai", "weekly_limit", false},
		{"xai", "monthly_credits", false},
		{"xai", "", false},
		{"codex", "code_week", true},
		{"codex", "code_5h", false},
		{"claude", "seven_day", true},
		{"claude", "five_hour", false},
		{"kimi", "code_week", true},
		{"antigravity", "antigravity:gemini_weekly", true},
		{"unknown", "anything", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.provider+"/"+tc.quotaKey, func(t *testing.T) {
			t.Parallel()
			if got := MatchesProjectionQuotaKey(tc.provider, tc.quotaKey); got != tc.want {
				t.Fatalf("MatchesProjectionQuotaKey(%q, %q) = %v, want %v", tc.provider, tc.quotaKey, got, tc.want)
			}
		})
	}
}

// Only xAI's projection divisor is narrower than its pool-wide percentage, and
// the panel needs to know so the two figures do not read as contradictory.
func TestProjectionQuotaIsAttributable(t *testing.T) {
	t.Parallel()

	for provider, want := range map[string]bool{
		"xai": true, "grok": true, "XAI": true,
		"codex": false, "claude": false, "kimi": false, "antigravity": false, "": false,
	} {
		if got := ProjectionQuotaIsAttributable(provider); got != want {
			t.Fatalf("ProjectionQuotaIsAttributable(%q) = %v, want %v", provider, got, want)
		}
	}
}
