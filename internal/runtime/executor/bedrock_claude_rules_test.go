package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBedrockCatalogModel(t *testing.T) {
	cases := []struct {
		ids  []string
		want string
	}{
		{ids: []string{"claude-opus-5"}, want: "claude-opus-5"},
		{ids: []string{"us.anthropic.claude-opus-4-7-v1"}, want: "claude-opus-4-7"},
		{ids: []string{"anthropic.claude-opus-5-5"}, want: "claude-opus-5-5"},
		{ids: []string{"global.anthropic.claude-sonnet-4-5-20250929-v1:0"}, want: "claude-sonnet-4-5-20250929"},
		{ids: []string{"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-4-8"}, want: "claude-opus-4-8"},
		// A mapping to an id the catalog cannot read falls back to the
		// requested name.
		{ids: []string{"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3", "claude-opus-4-7"}, want: "claude-opus-4-7"},
		{ids: []string{"us.anthropic.claude-opus-6-v1"}, want: ""},
		{ids: []string{"meta.llama3-70b-instruct-v1:0"}, want: ""},
	}
	for _, tc := range cases {
		if got := bedrockCatalogModel(tc.ids...); got != tc.want {
			t.Errorf("bedrockCatalogModel(%q) = %q, want %q", tc.ids, got, tc.want)
		}
	}
}

func TestBedrockThinkingModel(t *testing.T) {
	cases := []struct{ requested, catalog, want string }{
		{requested: "us.anthropic.claude-opus-4-7-v1", catalog: "claude-opus-4-7", want: "claude-opus-4-7"},
		{requested: "us.anthropic.claude-opus-4-7-v1(high)", catalog: "claude-opus-4-7", want: "claude-opus-4-7(high)"},
		{requested: "claude-opus-4-7(8192)", catalog: "claude-opus-4-7", want: "claude-opus-4-7(8192)"},
		{requested: "us.anthropic.claude-opus-6-v1(high)", catalog: "", want: "us.anthropic.claude-opus-6-v1(high)"},
	}
	for _, tc := range cases {
		if got := bedrockThinkingModel(tc.requested, tc.catalog); got != tc.want {
			t.Errorf("bedrockThinkingModel(%q, %q) = %q, want %q", tc.requested, tc.catalog, got, tc.want)
		}
	}
}

func executeBedrock(t *testing.T, upstream *claudeRecordingServer, model, payload string) ([][]byte, error) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	auth := &cliproxyauth.Auth{ID: "bedrock-rules", Provider: "bedrock", Attributes: map[string]string{
		"auth_mode": "api-key",
		"api_key":   "bedrock-test-key",
		"region":    "us-east-1",
		"base_url":  server.URL,
	}}
	_, err := NewBedrockExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	return upstream.requests(), err
}

// A request naming the model by its Bedrock id gets the catalog's rules for
// the Anthropic id: the budget becomes adaptive thinking, and the effort that
// carries the depth survives the Bedrock cleanup.
func TestBedrockExecutorShapesThinkingForBedrockIDs(t *testing.T) {
	resetClaudeLearnedRules(t)
	payload := `{"model":"us.anthropic.claude-opus-4-7-v1","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16000},"messages":[{"role":"user","content":"hi"}]}`
	requests, err := executeBedrock(t, okClaudeUpstream(), "us.anthropic.claude-opus-4-7-v1", payload)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(requests))
	}
	sent := requests[0]
	if got := gjson.GetBytes(sent, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, sent)
	}
	if gjson.GetBytes(sent, "output_config.effort").String() == "" {
		t.Fatalf("effort dropped: %s", sent)
	}
}

// Bedrock passes Anthropic's 400 through: for a model the catalog does not
// know, the retry turns the budget into adaptive thinking, and the effort is
// kept for it.
func TestBedrockExecutorRetriesRejectedBudget(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: bedrockEnabledRejected},
		{status: http.StatusOK, contentType: "application/json", body: claudeOKMessage},
	}}
	payload := `{"model":"us.anthropic.claude-opus-6-v1","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16000},"messages":[{"role":"user","content":"hi"}]}`
	requests, err := executeBedrock(t, upstream, "us.anthropic.claude-opus-6-v1", payload)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(requests))
	}
	if got := gjson.GetBytes(requests[0], "thinking.type").String(); got != "enabled" {
		t.Fatalf("first attempt thinking.type = %q", got)
	}
	retry := requests[1]
	if gjson.GetBytes(retry, "thinking.type").String() != "adaptive" || gjson.GetBytes(retry, "output_config.effort").String() == "" {
		t.Fatalf("retry = %s, want adaptive thinking with an effort", retry)
	}
	if gjson.GetBytes(retry, "model").Exists() {
		t.Fatalf("retry skipped the Bedrock cleanup: %s", retry)
	}
}

// Bedrock rejects a forced tool_choice unless thinking is "disabled", and
// Sonnet 5 thinks when thinking is left out.
func TestBedrockExecutorForcedToolChoiceTurnsThinkingOff(t *testing.T) {
	resetClaudeLearnedRules(t)
	payload := `{"model":"claude-sonnet-5","max_tokens":4096,"temperature":0.5,"thinking":{"type":"adaptive"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`
	requests, err := executeBedrock(t, okClaudeUpstream(), "claude-sonnet-5", payload)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	sent := requests[0]
	if got := gjson.GetBytes(sent, "thinking").Raw; got != `{"type":"disabled"}` {
		t.Fatalf("thinking = %s, want disabled", got)
	}
	if got := gjson.GetBytes(sent, "tool_choice.name").String(); got != "lookup" {
		t.Fatalf("tool_choice = %s, want the forced tool kept", gjson.GetBytes(sent, "tool_choice").Raw)
	}
	if gjson.GetBytes(sent, "temperature").Exists() {
		t.Fatalf("temperature sent to Sonnet 5: %s", sent)
	}
}
