package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// Error bodies for the request rules, as the upstreams return them.
const (
	claudeTemperatureDeprecated = "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"`temperature` is deprecated for this model.\"},\"request_id\":\"req_1\"}"
	claudeTopPDeprecated        = "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"`top_p` is deprecated for this model.\"}}"
	claudeForcedToolRejected    = `{"type":"error","error":{"type":"invalid_request_error","message":"tool_choice: type \"tool\" and \"any\" are not supported for this model."}}`
	// Bedrock passes Anthropic's message through in its own envelope.
	bedrockEnabledRejected = `{"message":"\"thinking.type.enabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}`
)

// resetClaudeLearnedRules empties the learned-rule cache for one test.
func resetClaudeLearnedRules(t *testing.T) {
	t.Helper()
	clearRules := func() {
		claudeLearnedRules.mu.Lock()
		claudeLearnedRules.entries = make(map[string]claudeLearnedRule)
		claudeLearnedRules.mu.Unlock()
	}
	clearRules()
	t.Cleanup(clearRules)
}

func TestClassifyClaudeRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   claudeRejection
	}{
		{name: "budget", status: 400, body: claudeEnabledRejected, want: claudeRejectsBudgetThinking},
		{name: "disabled", status: 400, body: claudeDisabledRejected, want: claudeRejectsDisabledThinking},
		{name: "disabled on Sonnet 5.5", status: 400, body: claudeDisabledRejectedSonnet, want: claudeRejectsDisabledUseBetweenTools},
		{name: "effort with thinking off", status: 400, body: claudeEffortRejectedWhenOff, want: claudeRejectsEffortWhenOff},
		{name: "temperature", status: 400, body: claudeTemperatureDeprecated, want: claudeRejectsSampling},
		{name: "top_p", status: 400, body: claudeTopPDeprecated, want: claudeRejectsSampling},
		{name: "top_k as plain text", status: 400, body: "`top_k` is deprecated for this model.", want: claudeRejectsSampling},
		{name: "forced tool_choice", status: 400, body: claudeForcedToolRejected, want: claudeRejectsForcedToolChoice},
		{name: "Bedrock envelope", status: 400, body: bedrockEnabledRejected, want: claudeRejectsBudgetThinking},
		{name: "not a 400", status: 500, body: claudeTemperatureDeprecated},
		{name: "temperature out of range", status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"temperature: range: 0..1"}}`},
		{name: "both sampling fields on a Claude 4 model", status: 400, body: "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"`temperature` and `top_p` cannot both be specified for this model. Please use only one.\"}}"},
		{name: "another deprecated field", status: 400, body: "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"`foo` is deprecated for this model.\"}}"},
		{name: "a field named like a sampling one", status: 400, body: "`top_p_mode` is deprecated for this model."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyClaudeRejection(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("classifyClaudeRejection = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStripClaudeSampling(t *testing.T) {
	out, ok := stripClaudeSampling([]byte(`{"model":"m","temperature":0.2,"top_p":0.9,"top_k":40,"max_tokens":10}`))
	if !ok {
		t.Fatal("ok = false, want the sampling fields removed")
	}
	for _, field := range claudeSamplingFields {
		if gjson.GetBytes(out, field).Exists() {
			t.Fatalf("%s kept: %s", field, out)
		}
	}
	if gjson.GetBytes(out, "max_tokens").Int() != 10 {
		t.Fatalf("other fields changed: %s", out)
	}
	if _, ok := stripClaudeSampling([]byte(`{"model":"m"}`)); ok {
		t.Fatal("ok = true for a body without sampling fields")
	}
}

func TestRelaxClaudeForcedToolChoice(t *testing.T) {
	tools := `"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"type":"web_search_20260209","name":"web_search"}]`
	cases := []struct {
		name            string
		body            string
		fc              claudeFixContext
		wantOK          bool
		wantChoice      string
		wantInstruction string
	}{
		{
			name:            "any with an instruction",
			body:            `{` + tools + `,"tool_choice":{"type":"any"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			fc:              claudeFixContext{instructTools: true},
			wantOK:          true,
			wantChoice:      `{"type":"auto"}`,
			wantInstruction: "Respond by calling one of the available tools.",
		},
		{
			name:            "named tool keeps disable_parallel_tool_use",
			body:            `{` + tools + `,"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},"messages":[{"role":"user","content":"find it"}]}`,
			fc:              claudeFixContext{instructTools: true},
			wantOK:          true,
			wantChoice:      `{"type":"auto","disable_parallel_tool_use":true}`,
			wantInstruction: "Respond by calling the `lookup` tool.",
		},
		{
			name:            "the instruction names the tool the upstream sees",
			body:            `{` + tools + `,"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"find it"}]}`,
			fc:              claudeFixContext{instructTools: true, toUpstream: func(b []byte) []byte { return applyClaudeToolPrefix(b, claudeToolPrefix) }},
			wantOK:          true,
			wantChoice:      `{"type":"auto"}`,
			wantInstruction: "Respond by calling the `" + claudeToolPrefix + "lookup` tool.",
		},
		{
			name:            "built-in tools keep their name",
			body:            `{` + tools + `,"tool_choice":{"type":"tool","name":"web_search"},"messages":[{"role":"user","content":"search"}]}`,
			fc:              claudeFixContext{instructTools: true, toUpstream: func(b []byte) []byte { return applyClaudeToolPrefix(b, claudeToolPrefix) }},
			wantOK:          true,
			wantChoice:      `{"type":"auto"}`,
			wantInstruction: "Respond by calling the `web_search` tool.",
		},
		{
			name:       "clients that send thinking back get no instruction",
			body:       `{` + tools + `,"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`,
			wantOK:     true,
			wantChoice: `{"type":"auto"}`,
		},
		{
			name:       "not after an assistant turn",
			body:       `{` + tools + `,"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`,
			fc:         claudeFixContext{instructTools: true},
			wantOK:     true,
			wantChoice: `{"type":"auto"}`,
		},
		{name: "auto is left alone", body: `{` + tools + `,"tool_choice":{"type":"auto"},"messages":[]}`, fc: claudeFixContext{instructTools: true}},
		{name: "none is left alone", body: `{` + tools + `,"tool_choice":{"type":"none"},"messages":[]}`, fc: claudeFixContext{instructTools: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := relaxClaudeForcedToolChoice([]byte(tc.body), tc.fc)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if string(out) != tc.body {
					t.Fatalf("body changed: %s", out)
				}
				return
			}
			if got := gjson.GetBytes(out, "tool_choice").Raw; got != tc.wantChoice {
				t.Fatalf("tool_choice = %s, want %s", got, tc.wantChoice)
			}
			messages := gjson.GetBytes(out, "messages").Array()
			last := messages[len(messages)-1]
			blocks := last.Get("content").Array()
			lastText := ""
			if len(blocks) > 0 {
				lastText = blocks[len(blocks)-1].Get("text").String()
			}
			if tc.wantInstruction == "" {
				if strings.HasPrefix(lastText, "Respond by calling") {
					t.Fatalf("instruction added: %s", out)
				}
				return
			}
			if lastText != tc.wantInstruction {
				t.Fatalf("last block text = %q, want %q (%s)", lastText, tc.wantInstruction, out)
			}
			// A string turn becomes blocks with the client's text first.
			if first := blocks[0].Get("text").String(); first == tc.wantInstruction {
				t.Fatalf("the client's text was replaced: %s", out)
			}
		})
	}
}

func TestTurnClaudeThinkingOffForForcedTool(t *testing.T) {
	opus5 := &registry.AdaptiveThinking{Off: registry.ClaudeThinkingOffDisabled, OffMaxLevel: "high"}
	opus47 := &registry.AdaptiveThinking{Off: registry.ClaudeThinkingOffDisabled}
	fable := &registry.AdaptiveThinking{Off: registry.ClaudeThinkingAlwaysOn}
	forced := `{"tool_choice":{"type":"any"},"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`

	out := turnClaudeThinkingOffForForcedTool([]byte(forced), opus5)
	if got := gjson.GetBytes(out, "thinking").Raw; got != `{"type":"disabled"}` {
		t.Fatalf("Opus 5 thinking = %s", got)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "high" {
		t.Fatalf("Opus 5 effort = %q, want high: disabled is refused above it", got)
	}
	out = turnClaudeThinkingOffForForcedTool([]byte(forced), opus47)
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "max" {
		t.Fatalf("Opus 4.7 effort = %q, want max kept", got)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("Opus 4.7 thinking.type = %q", got)
	}
	if out := turnClaudeThinkingOffForForcedTool([]byte(forced), fable); string(out) != forced {
		t.Fatalf("an always-on model was changed: %s", out)
	}
	if out := turnClaudeThinkingOffForForcedTool([]byte(forced), nil); string(out) != forced {
		t.Fatalf("a model without a catalog rule was changed: %s", out)
	}
	auto := `{"tool_choice":{"type":"auto"},"thinking":{"type":"adaptive"}}`
	if out := turnClaudeThinkingOffForForcedTool([]byte(auto), opus5); string(out) != auto {
		t.Fatalf("thinking turned off without a forced tool_choice: %s", out)
	}
}

func TestClaudeLearnedRuleCacheExpiresAndBounds(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cache := newClaudeLearnedRuleCache()
	cache.now = func() time.Time { return now }

	if !cache.add("k", claudeRejectsSampling) {
		t.Fatal("first add reported as known")
	}
	if cache.add("k", claudeRejectsSampling) {
		t.Fatal("second add of the same rule reported as new")
	}
	if !cache.add("k", claudeRejectsForcedToolChoice) {
		t.Fatal("a second rule for the key reported as known")
	}
	if got := cache.get("k"); got != claudeRejectsSampling|claudeRejectsForcedToolChoice {
		t.Fatalf("get = %s", got)
	}
	now = now.Add(claudeLearnedRuleTTL)
	if got := cache.get("k"); got != 0 {
		t.Fatalf("get after the TTL = %s, want nothing", got)
	}

	for i := 0; i < claudeLearnedRuleLimit+10; i++ {
		now = now.Add(time.Second)
		cache.add("key-"+strconv.Itoa(i), claudeRejectsSampling)
	}
	if got := len(cache.entries); got > claudeLearnedRuleLimit {
		t.Fatalf("cache holds %d entries, limit %d", got, claudeLearnedRuleLimit)
	}
	if got := cache.get("key-" + strconv.Itoa(claudeLearnedRuleLimit+9)); got != claudeRejectsSampling {
		t.Fatal("the newest entry was evicted")
	}
}

// knownAdaptive decides whether Bedrock keeps output_config.effort: the
// catalog says so for the models it knows, a 400 for the others.
func TestClaudeRequestPlanKnownAdaptive(t *testing.T) {
	resetClaudeLearnedRules(t)
	newPlan := func(model string) *claudeRequestPlan {
		execCtx := newExecutionContext(context.Background(), "bedrock", &config.Config{}, nil, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, ExecutionOptions{TargetFormat: sdktranslator.FromString("claude")})
		plan := newClaudeRequestPlan(execCtx, claudeRequestTarget{provider: "bedrock", endpoint: "https://bedrock.test/model/x/invoke", model: model, catalogModel: bedrockCatalogModel(model)})
		plan.begin(execCtx, []byte(`{}`))
		return plan
	}
	for model, want := range map[string]bool{
		"us.anthropic.claude-opus-4-7-v1":              true,
		"global.anthropic.claude-opus-5-5":             true,
		"us.anthropic.claude-sonnet-5-5":               true,
		"us.anthropic.claude-opus-4-6-v1":              false,
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": false,
		"us.anthropic.claude-opus-6-v1":                false,
	} {
		if got := newPlan(model).knownAdaptive(); got != want {
			t.Errorf("knownAdaptive for %q = %v, want %v", model, got, want)
		}
	}

	claudeLearnedRules.add(claudeLearnedRuleKey("bedrock", "https://bedrock.test/model/x/invoke", "us.anthropic.claude-opus-6-v1"), claudeRejectsBudgetThinking)
	if !newPlan("us.anthropic.claude-opus-6-v1").knownAdaptive() {
		t.Error("knownAdaptive = false after the model refused a budget")
	}
	claudeLearnedRules.add(claudeLearnedRuleKey("bedrock", "https://bedrock.test/model/x/invoke", "us.anthropic.claude-haiku-6-v1"), claudeRejectsSampling)
	if newPlan("us.anthropic.claude-haiku-6-v1").knownAdaptive() {
		t.Error("knownAdaptive = true from a rule that is not about thinking")
	}
}

// executeClaude runs one non-streaming request against upstream and returns
// the request bodies the upstream saw.
func executeClaude(t *testing.T, upstream *claudeRecordingServer, model, sourceFormat, payload string) ([][]byte, error) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	executor := NewClaudeExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(sourceFormat), OriginalRequest: []byte(payload)})
	return upstream.requests(), err
}

func okClaudeUpstream() *claudeRecordingServer {
	return &claudeRecordingServer{responses: []claudeCannedResponse{{status: http.StatusOK, contentType: "application/json", body: claudeOKMessage}}}
}

// Opus 5.5 rejects sampling and a forced tool_choice. A Claude client keeps
// its thinking replayable, so it gets "auto" without an added instruction.
func TestClaudeExecutorAppliesCatalogRequestRules(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := okClaudeUpstream()
	payload := `{"model":"claude-opus-5-5","max_tokens":1024,"temperature":0.2,"top_p":0.9,"top_k":40,"thinking":{"type":"adaptive","display":"summarized"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	requests, err := executeClaude(t, upstream, "claude-opus-5-5", "claude", payload)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(requests))
	}
	sent := requests[0]
	for _, field := range claudeSamplingFields {
		if gjson.GetBytes(sent, field).Exists() {
			t.Fatalf("%s sent to a model that rejects it: %s", field, sent)
		}
	}
	if got := gjson.GetBytes(sent, "tool_choice").Raw; got != `{"type":"auto"}` {
		t.Fatalf("tool_choice = %s, want auto", got)
	}
	if got := gjson.GetBytes(sent, "thinking").Raw; got != `{"type":"adaptive","display":"summarized"}` {
		t.Fatalf("thinking = %s, want the client's", got)
	}
	if got := gjson.GetBytes(sent, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("a Claude client's turn was changed: %s", sent)
	}
}

// A Chat client cannot send thinking back, so "required" becomes "auto" plus an
// instruction to call a tool.
func TestClaudeExecutorInstructsChatClientsWhenRelaxingToolChoice(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := okClaudeUpstream()
	payload := `{"model":"claude-sonnet-5-5","temperature":0.7,"tool_choice":"required","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],"messages":[{"role":"user","content":"find it"}]}`
	requests, err := executeClaude(t, upstream, "claude-sonnet-5-5", "openai", payload)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	sent := requests[0]
	if gjson.GetBytes(sent, "temperature").Exists() {
		t.Fatalf("temperature sent: %s", sent)
	}
	if got := gjson.GetBytes(sent, "tool_choice.type").String(); got != "auto" {
		t.Fatalf("tool_choice.type = %q, want auto", got)
	}
	lastBlock := gjson.GetBytes(sent, "messages.0.content.@reverse.0.text").String()
	if lastBlock != "Respond by calling one of the available tools." {
		t.Fatalf("last block = %q: %s", lastBlock, sent)
	}
}

func TestClaudeExecutorForcedToolThinking(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		thinking     string
		effort       string
		wantThinking string
		wantEffort   string
	}{
		// Removing thinking would turn it on for Opus 5; "disabled" also
		// caps the effort it is accepted at.
		{name: "Opus 5", model: "claude-opus-5", thinking: `{"type":"adaptive"}`, effort: "max", wantThinking: `{"type":"disabled"}`, wantEffort: "high"},
		{name: "Opus 4.7", model: "claude-opus-4-7", thinking: `{"type":"adaptive"}`, effort: "xhigh", wantThinking: `{"type":"disabled"}`, wantEffort: "xhigh"},
		// Fable 5 cannot turn thinking off and accepts forcing: left as sent.
		{name: "Fable 5", model: "claude-fable-5", thinking: `{"type":"adaptive","display":"summarized"}`, effort: "high", wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "high"},
		// Older models reject thinking next to forcing: removed, as before.
		{name: "Opus 4.6", model: "claude-opus-4-6", thinking: `{"type":"enabled","budget_tokens":2048}`, wantThinking: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeLearnedRules(t)
			upstream := okClaudeUpstream()
			payload := `{"model":"` + tc.model + `","max_tokens":4096,"thinking":` + tc.thinking + `,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`
			if tc.effort != "" {
				payload = strings.Replace(payload, `"max_tokens":4096`, `"max_tokens":4096,"output_config":{"effort":"`+tc.effort+`"}`, 1)
			}
			requests, err := executeClaude(t, upstream, tc.model, "claude", payload)
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			sent := requests[0]
			if got := gjson.GetBytes(sent, "tool_choice.type").String(); got != "any" {
				t.Fatalf("tool_choice.type = %q, want any kept", got)
			}
			if got := gjson.GetBytes(sent, "thinking").Raw; got != tc.wantThinking {
				t.Fatalf("thinking = %s, want %s", got, tc.wantThinking)
			}
			if got := gjson.GetBytes(sent, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

// A model the catalog does not know: each 400 names a rule, the retry is the
// client's request shaped for every rule named so far, and the next request is
// shaped before it is sent.
func TestClaudeExecutorLearnsRulesFromRejections(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeTemperatureDeprecated},
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeForcedToolRejected},
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeEnabledRejected},
		{status: http.StatusOK, contentType: "application/json", body: claudeOKMessage},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()
	payload := `{"model":"claude-haiku-6","max_tokens":32000,"temperature":0.3,"thinking":{"type":"enabled","budget_tokens":8192},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	run := func() {
		t.Helper()
		executor := NewClaudeExecutor(&config.Config{})
		if _, err := executor.Execute(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
			Model:   "claude-haiku-6",
			Payload: []byte(payload),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	}
	shapeOf := func(body []byte) string {
		return gjson.GetBytes(body, "temperature").Raw + " | " + gjson.GetBytes(body, "tool_choice").Raw + " | " + gjson.GetBytes(body, "thinking").Raw + " | " + gjson.GetBytes(body, "output_config").Raw
	}

	run()
	requests := upstream.requests()
	want := []string{
		// Older Claude models reject thinking next to forcing, so it goes.
		`0.3 | {"type":"any"} |  | `,
		` | {"type":"any"} |  | `,
		// Forcing relaxed: the client's thinking is back, as sent.
		` | {"type":"auto"} | {"type":"enabled","budget_tokens":8192} | `,
		` | {"type":"auto"} | {"type":"adaptive","display":"summarized"} | {"effort":"medium"}`,
	}
	if len(requests) != len(want) {
		t.Fatalf("upstream saw %d requests, want %d", len(requests), len(want))
	}
	for i := range want {
		if got := shapeOf(requests[i]); got != want[i] {
			t.Fatalf("attempt %d = %s, want %s", i+1, got, want[i])
		}
	}

	run()
	requests = upstream.requests()
	if len(requests) != len(want)+1 {
		t.Fatalf("upstream saw %d requests, want %d: the learned rules apply before sending", len(requests), len(want)+1)
	}
	if got := shapeOf(requests[len(want)]); got != want[len(want)-1] {
		t.Fatalf("next request = %s, want what the last retry sent, %s", got, want[len(want)-1])
	}
}

// A retry that would send the same request again is not made.
func TestClaudeExecutorDoesNotRepeatAnUnchangedRequest(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeTemperatureDeprecated},
	}}
	payload := `{"model":"claude-haiku-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	requests, err := executeClaude(t, upstream, "claude-haiku-6", "claude", payload)
	if err == nil {
		t.Fatal("Execute succeeded, want the 400")
	}
	if len(requests) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(requests))
	}
	claudeLearnedRules.mu.Lock()
	learned := len(claudeLearnedRules.entries)
	claudeLearnedRules.mu.Unlock()
	if learned != 0 {
		t.Fatalf("learned %d rules from a 400 the request did not cause", learned)
	}
}

// The rules learned for one upstream do not apply to another.
func TestClaudeLearnedRulesArePerUpstream(t *testing.T) {
	resetClaudeLearnedRules(t)
	refusing := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeTemperatureDeprecated},
		{status: http.StatusOK, contentType: "application/json", body: claudeOKMessage},
	}}
	payload := `{"model":"claude-haiku-6","max_tokens":10,"temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`
	if _, err := executeClaude(t, refusing, "claude-haiku-6", "claude", payload); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	other := okClaudeUpstream()
	requests, err := executeClaude(t, other, "claude-haiku-6", "claude", payload)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(requests[0], "temperature").Float(); got != 0.3 {
		t.Fatalf("temperature = %v on another upstream, want the client's 0.3", got)
	}
}

func TestClaudeExecutorCountTokensShapesTheRequest(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusOK, contentType: "application/json", body: `{"input_tokens":42}`},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()
	payload := `{"model":"claude-opus-5-5","temperature":0.5,"thinking":{"type":"enabled","budget_tokens":24576},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`
	executor := NewClaudeExecutor(&config.Config{})
	resp, err := executor.CountTokens(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   "claude-opus-5-5",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 42 {
		t.Fatalf("count = %s", resp.Payload)
	}
	requests := upstream.requests()
	if len(requests) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(requests))
	}
	sent := requests[0]
	if got := gjson.GetBytes(sent, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want the budget converted: %s", got, sent)
	}
	if gjson.GetBytes(sent, "temperature").Exists() || gjson.GetBytes(sent, "tool_choice.type").String() != "auto" {
		t.Fatalf("request rules not applied to count_tokens: %s", sent)
	}
}

func TestClaudeExecutorCountTokensRetriesRejections(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeEnabledRejected},
		{status: http.StatusOK, contentType: "application/json", body: `{"input_tokens":7}`},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()
	executor := NewClaudeExecutor(&config.Config{})
	if _, err := executor.CountTokens(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   "claude-opus-6",
		Payload: []byte(claudeUnknownModelBudgetRequest),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	requests := upstream.requests()
	if len(requests) != 2 || gjson.GetBytes(requests[1], "thinking.type").String() != "adaptive" {
		t.Fatalf("count_tokens did not retry with adaptive thinking: %d requests", len(requests))
	}
	if !strings.Contains(string(requests[0]), `"budget_tokens"`) {
		t.Fatalf("first attempt = %s", requests[0])
	}
}
