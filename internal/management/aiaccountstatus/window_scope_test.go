package aiaccountstatus

import "testing"

func TestWindowAppliesToModel(t *testing.T) {
	cases := []struct {
		key   string
		model string
		want  bool
	}{
		// Account-wide windows count for every model and for the account as a whole.
		{"code_5h", "gpt-5.6-sol", true},
		{"code_week", "", true},
		{"code_subscription_2592000", "gpt-5.6-sol", true},
		{"five_hour", "claude-opus-4-5", true},
		{"seven_day", "", true},
		{"weekly_limit", "grok-4", true},
		{"monthly_credits", "grok-4", true},
		{"base_quota", "claude-sonnet-4-5", true},
		{" Code_Week ", "gpt-5.6-sol", true},

		// Feature allowances limit something other than model requests.
		{"review_5h", "gpt-5.6-sol", false},
		{"review_week", "", false},
		{"seven_day_oauth_apps", "claude-opus-4-5", false},
		{"seven_day_cowork", "claude-opus-4-5", false},
		{"iguana_necktie", "claude-opus-4-5", false},
		{"extra_usage", "claude-opus-4-5", false},
		{"pay_as_you_go", "grok-4", false},
		{"product:Grok Code", "grok-code-fast-1", false},
		{"", "gpt-5.6-sol", false},

		// Claude's model-scoped weekly windows.
		{"seven_day_opus", "claude-opus-4-5-20251101", true},
		{"seven_day_opus", "claude-sonnet-4-5", false},
		{"seven_day_opus", "", false},
		{"seven_day_sonnet", "claude-sonnet-4-5", true},
		{"seven_day_sonnet", "claude-haiku-4-5", false},
		{"weekly_scoped_claude-opus-4-5", "claude-opus-4-5-20251101", true},
		{"weekly_scoped_opus-4-5", "claude-opus-4-5", true},
		{"weekly_scoped_opus-4-5", "claude-opus-4-1", false},

		// Codex additional allowances name their model.
		{"additional:codex_bengalfox:5h", "gpt-5.3-codex-spark", true},
		{"additional:codex_bengalfox:week", "gpt-5.3-codex", false},
		{"additional:gpt_5_3_codex_spark:week", "gpt-5.3-codex-spark", true},
		{"additional:gpt_5_3_codex_spark:week", "gpt-5.6-sol", false},
		{"additional:additional:5h", "gpt-5.6-sol", false},

		// Gemini CLI groups list their models; ungrouped buckets carry the model id.
		{"model:gemini-2.5-pro", "gemini-2.5-pro-preview", true},
		{"model:gemini-2.5-pro:requests", "models/gemini-2.5-pro", true},
		{"model:gemini-2.5-pro", "gemini-2.5-flash", false},
		{"model:gemini-3-pro-preview", "gemini-3-pro-preview", true},
		{"model:gemini-3-pro-preview", "gemini-3-flash-preview", false},

		// Antigravity families and one-off models.
		{"antigravity:claude", "claude-sonnet-4-5", true},
		{"antigravity:claude", "gemini-3-pro-high", false},
		{"antigravity:gemini_pro", "gemini-3-pro-high", true},
		{"antigravity:gemini_flash", "gemini-3-pro-high", false},
		{"antigravity:other", "gpt-oss-120b-medium", false},
		{"antigravity:model_gpt_oss_120b_medium", "gpt-oss-120b-medium", true},
		{"antigravity:model_gpt_oss_120b_medium", "gpt-oss-20b", false},
	}
	for _, tc := range cases {
		if got := WindowAppliesToModel(tc.key, tc.model); got != tc.want {
			t.Errorf("WindowAppliesToModel(%q, %q) = %v, want %v", tc.key, tc.model, got, tc.want)
		}
	}
}
