package modelcatalog

import (
	"reflect"
	"testing"
)

func TestClaudeModelCandidatesForBedrockID(t *testing.T) {
	cases := []struct {
		id   string
		want []string
	}{
		{id: "us.anthropic.claude-opus-4-7-v1", want: []string{"claude-opus-4-7"}},
		{id: "us.anthropic.claude-opus-4-7-v1:0", want: []string{"claude-opus-4-7"}},
		{id: "anthropic.claude-opus-5-5", want: []string{"claude-opus-5-5"}},
		{id: "us-gov.anthropic.claude-sonnet-5", want: []string{"claude-sonnet-5"}},
		{id: "global.anthropic.claude-sonnet-4-5-20250929-v1:0", want: []string{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"}},
		{id: "anthropic.claude-3-sonnet-20240229-v1:0:200k", want: []string{"claude-3-sonnet-20240229", "claude-3-sonnet"}},
		{id: " US.Anthropic.Claude-Opus-4-7-V1 ", want: []string{"claude-opus-4-7"}},
		{id: "arn:aws:bedrock:us-east-1:123456789012:inference-profile/apac.anthropic.claude-haiku-4-5-20251001-v1:0", want: []string{"claude-haiku-4-5-20251001", "claude-haiku-4-5"}},
		{id: "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-opus-4-8", want: []string{"claude-opus-4-8"}},
		// A version-like suffix that is not one stays.
		{id: "us.anthropic.claude-opus-4-7-vision", want: []string{"claude-opus-4-7-vision"}},
		// Application inference profiles name no model.
		{id: "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3", want: nil},
		{id: "meta.llama3-70b-instruct-v1:0", want: nil},
		{id: "anthropic.titan-text", want: nil},
		{id: "us1.anthropic.claude-opus-4-7", want: nil},
		{id: "vendor-anthropic.claude-opus-4-7", want: nil},
		{id: "claude-opus-4-7", want: nil},
		{id: "", want: nil},
	}
	for _, tc := range cases {
		if got := ClaudeModelCandidatesForBedrockID(tc.id); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ClaudeModelCandidatesForBedrockID(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
