package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	pairingCallA   = `{"type":"tool_use","id":"toolu_a","name":"read","input":{"path":"a"}}`
	pairingCallB   = `{"type":"tool_use","id":"toolu_b","name":"read","input":{"path":"b"}}`
	pairingResultA = `{"type":"tool_result","tool_use_id":"toolu_a","content":"file a"}`
	pairingResultB = `{"type":"tool_result","tool_use_id":"toolu_b","content":[{"type":"text","text":"file b"}]}`
	pairingMissing = `{"type":"tool_result","tool_use_id":"%s","is_error":true,"content":"No result was recorded for this tool call."}`
)

func pairingBody(messages string) string {
	return `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[` + messages + `]}`
}

func TestPairClaudeToolBlocks(t *testing.T) {
	cases := []struct {
		name     string
		messages string
		// want is the messages array after the repair; empty means the body
		// must come back unchanged, byte for byte.
		want        string
		synthesized int
		orphaned    int
		reordered   int
	}{
		{
			name:     "paired history",
			messages: `{"role":"user","content":"read both"},{"role":"assistant","content":[{"type":"text","text":"ok"},` + pairingCallA + `,` + pairingCallB + `]},{"role":"user","content":[` + pairingResultA + `,` + pairingResultB + `,{"type":"text","text":"go on"}]}`,
		},
		{
			name:     "messages of one role form one turn",
			messages: `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"assistant","content":[{"type":"text","text":"and"},` + pairingCallB + `]},{"role":"user","content":[` + pairingResultA + `]},{"role":"user","content":[` + pairingResultB + `]}`,
		},
		{
			name:     "a call in the last turn is the client's prefill",
			messages: `{"role":"user","content":"read a"},{"role":"assistant","content":[` + pairingCallA + `]}`,
		},
		{
			name:        "a call without a result gets an error result first",
			messages:    `{"role":"assistant","content":[` + pairingCallA + `,` + pairingCallB + `]},{"role":"user","content":[` + pairingResultB + `]}`,
			want:        `{"role":"assistant","content":[` + pairingCallA + `,` + pairingCallB + `]},{"role":"user","content":[` + fmt.Sprintf(pairingMissing, "toolu_a") + `,` + pairingResultB + `]}`,
			synthesized: 1,
		},
		{
			name:        "an interrupted call followed by plain text",
			messages:    `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":"never mind"}`,
			want:        `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":[` + fmt.Sprintf(pairingMissing, "toolu_a") + `,{"type":"text","text":"never mind"}]}`,
			synthesized: 1,
		},
		{
			name:     "a result without a call becomes text",
			messages: `{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"hello"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_gone","is_error":true,"content":[{"type":"text","text":"denied"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]},{"type":"text","text":"next"}]}`,
			want:     `{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"hello"}]},{"role":"user","content":[{"type":"text","text":"Result of tool call toolu_gone (error):\ndenied"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},{"type":"text","text":"next"}]}`,
			orphaned: 1,
		},
		{
			name:     "a result with no turn before it",
			messages: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"42"}]}`,
			want:     `{"role":"user","content":[{"type":"text","text":"Result of tool call toolu_x:\n42"}]}`,
			orphaned: 1,
		},
		{
			name:      "results go before the other content of the turn",
			messages:  `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":"here you go"},{"role":"user","content":[{"type":"text","text":"note","cache_control":{"type":"ephemeral"}},` + pairingResultA + `]}`,
			want:      `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":[` + pairingResultA + `,{"type":"text","text":"here you go"},{"type":"text","text":"note","cache_control":{"type":"ephemeral"}}]}`,
			reordered: 1,
		},
		{
			name:        "only the broken turn is rewritten",
			messages:    `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":[` + pairingResultA + `]},{"role":"assistant","content":[` + pairingCallB + `]},{"role":"user","content":[{"type":"text","text":"stop"}]}`,
			want:        `{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":[` + pairingResultA + `]},{"role":"assistant","content":[` + pairingCallB + `]},{"role":"user","content":[` + fmt.Sprintf(pairingMissing, "toolu_b") + `,{"type":"text","text":"stop"}]}`,
			synthesized: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := pairingBody(tc.messages)
			out, repair := pairClaudeToolBlocks([]byte(body))
			if tc.want == "" {
				if string(out) != body || repair.changed() {
					t.Fatalf("valid history changed (%+v):\n%s", repair, out)
				}
				return
			}
			if got, want := gjson.GetBytes(out, "messages").Raw, "["+tc.want+"]"; got != want {
				t.Fatalf("messages =\n%s\nwant\n%s", got, want)
			}
			if repair.synthesized != tc.synthesized || repair.orphaned != tc.orphaned || repair.reordered != tc.reordered {
				t.Fatalf("repair = %+v, want synthesized %d, orphaned %d, reordered %d", repair, tc.synthesized, tc.orphaned, tc.reordered)
			}
			if gjson.GetBytes(out, "max_tokens").Int() != 1024 {
				t.Fatalf("other fields lost: %s", out)
			}
			// The repaired history is valid, so a client replaying it, or
			// the same broken one, gets the same bytes again.
			again, repairAgain := pairClaudeToolBlocks(out)
			if string(again) != string(out) || repairAgain.changed() {
				t.Fatalf("repair is not idempotent: %+v\n%s", repairAgain, again)
			}
			replay, _ := pairClaudeToolBlocks([]byte(body))
			if string(replay) != string(out) {
				t.Fatal("repair is not deterministic")
			}
		})
	}
}

func TestPairClaudeToolBlocksLeavesOtherBodiesAlone(t *testing.T) {
	for _, body := range []string{`{"model":"m"}`, `{"messages":"text"}`, `{"messages":[]}`, `not json`} {
		if out, repair := pairClaudeToolBlocks([]byte(body)); string(out) != body || repair.changed() {
			t.Errorf("pairClaudeToolBlocks(%s) = %s, %+v", body, out, repair)
		}
	}
}

const brokenToolHistory = `{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"read a"},{"role":"assistant","content":[` + pairingCallA + `]},{"role":"user","content":"never mind"}]}`

func TestClaudeExecutorRepairsToolPairing(t *testing.T) {
	cases := []struct {
		name       string
		skip       bool
		wantRepair bool
	}{
		{name: "Anthropic upstream", wantRepair: true},
		// Third-party Claude-compatible APIs get the request as the client
		// sent it, like the other Anthropic-specific processing.
		{name: "skip_anthropic_processing", skip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeLearnedRules(t)
			upstream := okClaudeUpstream()
			server := httptest.NewServer(upstream)
			defer server.Close()
			auth := claudeRetryTestAuth(server.URL, "key-123")
			if tc.skip {
				auth.Attributes["skip_anthropic_processing"] = "true"
			}
			if _, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-6",
				Payload: []byte(brokenToolHistory),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			sent := upstream.requests()[0]
			repaired := gjson.GetBytes(sent, "messages.2.content.0.tool_use_id").String() == "toolu_a"
			if repaired != tc.wantRepair {
				t.Fatalf("repaired = %v, want %v: %s", repaired, tc.wantRepair, gjson.GetBytes(sent, "messages").Raw)
			}
		})
	}
}

func TestClaudeExecutorCountTokensRepairsToolPairing(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusOK, contentType: "application/json", body: `{"input_tokens":9}`},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()
	if _, err := NewClaudeExecutor(&config.Config{}).CountTokens(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(brokenToolHistory),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}
	if got := gjson.GetBytes(upstream.requests()[0], "messages.2.content.0.tool_use_id").String(); got != "toolu_a" {
		t.Fatalf("count_tokens history not repaired: %s", upstream.requests()[0])
	}
}

func TestBedrockExecutorRepairsToolPairing(t *testing.T) {
	resetClaudeLearnedRules(t)
	upstream := okClaudeUpstream()
	server := httptest.NewServer(upstream)
	defer server.Close()
	auth := &cliproxyauth.Auth{ID: "bedrock-pairing", Provider: "bedrock", Attributes: map[string]string{
		"auth_mode": "api-key",
		"api_key":   "bedrock-test-key",
		"region":    "us-east-1",
		"base_url":  server.URL,
	}}
	if _, err := NewBedrockExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(brokenToolHistory),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(upstream.requests()[0], "messages.2.content.0.tool_use_id").String(); got != "toolu_a" {
		t.Fatalf("Bedrock history not repaired: %s", upstream.requests()[0])
	}
}
