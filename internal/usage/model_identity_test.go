package usage

import "testing"

func TestUpstreamModelMismatch(t *testing.T) {
	cases := []struct {
		name          string
		model         string
		upstreamModel string
		responseModel string
		want          bool
	}{
		{
			name:          "upstream declared nothing",
			model:         "claude-sonnet-4-5",
			responseModel: "",
			want:          false,
		},
		{
			name:          "same model echoed back",
			model:         "claude-sonnet-4-5",
			responseModel: "claude-sonnet-4-5",
			want:          false,
		},
		{
			name:          "case differences are not a switch",
			model:         "GPT-5.4",
			responseModel: "gpt-5.4",
			want:          false,
		},
		{
			name:          "routing prefix alone is not a switch",
			model:         "ollama/deepseek-v4-flash:0731",
			responseModel: "deepseek-v4-flash:0731",
			want:          false,
		},
		{
			name:          "upstream answered with a different build",
			model:         "gemini-3.8-flash-high",
			upstreamModel: "gemini-3.8-flash",
			responseModel: "gemini-3.8-flash-exp-a",
			want:          true,
		},
		{
			// The comparison baseline is what we put on the wire, not what the
			// client asked for: a mapping that already renames the model must not
			// register as a mismatch when the upstream honours it.
			name:          "mapped model matching the response is not a mismatch",
			model:         "fast",
			upstreamModel: "claude-sonnet-4-5",
			responseModel: "claude-sonnet-4-5",
			want:          false,
		},
		{
			name:          "mapped model ignored by the upstream is a mismatch",
			model:         "fast",
			upstreamModel: "claude-sonnet-4-5",
			responseModel: "claude-haiku-4-5",
			want:          true,
		},
		{
			name:          "dated build of the requested model is not a reroute",
			model:         "claude-sonnet-4-5",
			responseModel: "claude-sonnet-4-5-20260101",
			want:          false,
		},
		{
			name:          "dashed date build of the requested model is not a reroute",
			model:         "gpt-5.4",
			responseModel: "gpt-5.4-2026-03-01",
			want:          false,
		},
		{
			name:          "latest pointer resolving to its own model is not a reroute",
			model:         "grok-4.6-latest",
			responseModel: "grok-4.6",
			want:          false,
		},
		{
			// -exp-a is a different build, not a release stamp: this is the case
			// the audit exists for and must survive normalization.
			name:          "experimental build stays a mismatch",
			model:         "gemini-3.8-flash",
			responseModel: "gemini-3.8-flash-exp-a",
			want:          true,
		},
		{
			name:          "no sent model to compare against",
			model:         "",
			responseModel: "gpt-5.4",
			want:          false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpstreamModelMismatch(tc.model, tc.upstreamModel, tc.responseModel); got != tc.want {
				t.Fatalf("UpstreamModelMismatch(%q, %q, %q) = %v, want %v",
					tc.model, tc.upstreamModel, tc.responseModel, got, tc.want)
			}
		})
	}
}

func TestSentUpstreamModel(t *testing.T) {
	if got := SentUpstreamModel("fast", "claude-sonnet-4-5"); got != "claude-sonnet-4-5" {
		t.Fatalf("mapped model should win, got %q", got)
	}
	if got := SentUpstreamModel("claude-sonnet-4-5", "  "); got != "claude-sonnet-4-5" {
		t.Fatalf("requested model should be the fallback, got %q", got)
	}
}
