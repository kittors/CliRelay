package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	trans "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fingerprintReviewRecorder struct {
	body   []byte
	calls  int
	stream bool
}

func (r *fingerprintReviewRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "recording.invalid" {
		return nil, fmt.Errorf("unexpected host %s", req.URL.Host)
	}
	r.calls++
	r.body, _ = io.ReadAll(req.Body)
	body := `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	ct := "application/json"
	if req.URL.Path == "/v1/messages/count_tokens" {
		body = `{"input_tokens":11}`
	}
	if r.stream {
		ct = "text/event-stream"
		body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}
func TestClaudeFingerprintPreservesSystemInstructions(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)
	cases := []struct {
		name         string
		system       any
		marker, keep string
	}{
		{"plain_string", "KEEP_PLAIN", "", "KEEP_PLAIN"},
		{"identity_quote_string", "Discuss You are Claude Code. KEEP_IDENTITY", "You are Claude Code", "KEEP_IDENTITY"},
		{"billing_quote_string", "Explain x-anthropic-billing-header. KEEP_BILLING", "x-anthropic-billing-header", "KEEP_BILLING"},
		{"identity_suffix_string", claudeCodeIdentityText + "\nKEEP_SUFFIX", "You are Claude Code", "KEEP_SUFFIX"},
		{"billing_suffix_string", "x-anthropic-billing-header: cc_version=2.1.300.abc; cc_entrypoint=cli;\nKEEP_SUFFIX", "x-anthropic-billing-header", "KEEP_SUFFIX"},
		{"plain_array", []any{map[string]any{"type": "text", "text": "KEEP_PLAIN"}}, "", "KEEP_PLAIN"},
		{"identity_quote_array", []any{map[string]any{"type": "text", "text": "Discuss You are Claude Code. KEEP_IDENTITY", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}, "You are Claude Code", "KEEP_IDENTITY"},
		{"billing_quote_array", []any{map[string]any{"type": "text", "text": "Explain x-anthropic-billing-header. KEEP_BILLING"}}, "x-anthropic-billing-header", "KEEP_BILLING"},
		{"mixed_array", []any{map[string]any{"type": "text", "text": "Discuss You are Claude Code. KEEP_MIXED"}, map[string]any{"type": "text", "text": "SIBLING_SURVIVES", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}, map[string]any{"type": "custom", "data": "OPAQUE_SURVIVES"}}, "You are Claude Code", "KEEP_MIXED"},
		{"opaque_with_text", []any{map[string]any{"type": "custom", "text": "You are Claude Code KEEP_OPAQUE", "data": "OPAQUE_DATA"}}, "You are Claude Code", "KEEP_OPAQUE"},
		{"exact_identity", []any{map[string]any{"type": "text", "text": claudeCodeIdentityText}, map[string]any{"type": "text", "text": "SIBLING_SURVIVES"}}, "", ""},
		{"exact_billing", []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.300.abc; cc_entrypoint=cli;"}, map[string]any{"type": "text", "text": "SIBLING_SURVIVES"}}, "", ""},
	}
	for _, enabled := range []bool{false, true} {
		for _, skip := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, tc := range cases {
					t.Run(fmt.Sprintf("%s/enabled=%t/skip=%t/stream=%t", tc.name, enabled, skip, stream), func(t *testing.T) {
						cfg := &config.Config{}
						cfg.IdentityFingerprint.Claude.Enabled = enabled
						a := &auth.Auth{ID: "review-fingerprint", Provider: "claude", Attributes: map[string]string{"base_url": "https://recording.invalid", "api_key": "fixture-not-real", "cloak_mode": "never", "skip_anthropic_processing": fmt.Sprint(skip)}}
						input, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 8, "stream": stream, "system": tc.system, "messages": []any{map[string]any{"role": "user", "content": "ok"}}})
						r := &fingerprintReviewRecorder{stream: stream}
						ctx := exec.WithRoundTripper(context.Background(), r)
						e := NewClaudeExecutor(cfg)
						request := exec.Request{Model: "claude-sonnet-4-5", Payload: input}
						opts := exec.Options{SourceFormat: trans.FormatClaude, OriginalRequest: input}
						if stream {
							res, err := e.ExecuteStream(ctx, a, request, opts)
							if err != nil {
								t.Fatal(err)
							}
							for c := range res.Chunks {
								if c.Err != nil {
									t.Fatal(c.Err)
								}
							}
						} else {
							if _, err := e.Execute(ctx, a, request, opts); err != nil {
								t.Fatal(err)
							}
						}
						if r.calls != 1 {
							t.Fatalf("calls=%d", r.calls)
						}
						got := gjson.GetBytes(r.body, "system")
						lost := tc.keep != "" && !strings.Contains(got.Raw, tc.keep)
						wantLoss := false
						if tc.keep != "" && lost != wantLoss {
							t.Fatalf("lost=%t want=%t body=%s", lost, wantLoss, r.body)
						}
						if tc.name == "mixed_array" && (!strings.Contains(got.Raw, "SIBLING_SURVIVES") || !strings.Contains(got.Raw, "OPAQUE_SURVIVES")) {
							t.Fatal("unrelated sibling lost")
						}
						if enabled && !skip {
							nID, nBill := 0, 0
							for _, v := range got.Array() {
								text := v.Get("text").String()
								if v.Get("type").String() == "text" && text == claudeCodeIdentityText {
									nID++
								}
								if v.Get("type").String() == "text" && isClaudeFingerprintBillingText(text) {
									nBill++
								}
							}
							if nID != 1 || nBill != 1 {
								t.Fatalf("dedup mismatch: identity=%d billing=%d", nID, nBill)
							}
						}
						t.Logf("lost=%t system=%s", lost, got.Raw)
					})
				}
			}
		}
	}
}

// Keep adjacent legacy behavior explicit: this patch does not change cloaking
// or make CountTokens honor skip_anthropic_processing.
func TestClaudeFingerprintAdjacentLegacyBehavior(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)
	for _, mode := range []string{"messages-default-cloak", "count-tokens"} {
		for _, enabled := range []bool{false, true} {
			for _, skip := range []bool{false, true} {
				for _, array := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/enabled=%t/skip=%t/array=%t", mode, enabled, skip, array), func(t *testing.T) {
						cfg := &config.Config{}
						cfg.IdentityFingerprint.Claude.Enabled = enabled
						attrs := map[string]string{"base_url": "https://recording.invalid", "api_key": "fixture-not-real", "skip_anthropic_processing": fmt.Sprint(skip)}
						a := &auth.Auth{ID: "review-adjacent", Provider: "claude", Attributes: attrs}
						var system any = "Discuss You are Claude Code. KEEP_ADJACENT"
						if array {
							system = []any{map[string]any{"type": "text", "text": system}}
						}
						input, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 8, "system": system, "messages": []any{map[string]any{"role": "user", "content": "ok"}}})
						r := &fingerprintReviewRecorder{}
						ctx := exec.WithRoundTripper(context.Background(), r)
						e := NewClaudeExecutor(cfg)
						req := exec.Request{Model: "claude-sonnet-4-5", Payload: input}
						opts := exec.Options{SourceFormat: trans.FormatClaude}
						var err error
						if mode == "count-tokens" {
							_, err = e.CountTokens(ctx, a, req, opts)
						} else {
							_, err = e.Execute(ctx, a, req, opts)
						}
						if err != nil {
							t.Fatal(err)
						}
						if r.calls != 1 {
							t.Fatalf("calls=%d", r.calls)
						}
						lost := !strings.Contains(gjson.GetBytes(r.body, "system").Raw, "KEEP_ADJACENT")
						want := false
						if mode == "count-tokens" {
							want = !array
						} else {
							want = !skip && !enabled && !array
						}
						if lost != want {
							t.Fatalf("lost=%t want=%t body=%s", lost, want, r.body)
						}
						t.Logf("lost=%t system=%s", lost, gjson.GetBytes(r.body, "system").Raw)
					})
				}
			}
		}
	}
}

func TestClaudeFingerprintCompleteBillingRecognition(t *testing.T) {
	valid := []string{
		"x-anthropic-billing-header: cc_version=2.abc; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.abc; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.300.abc; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.300.4.abc; cc_entrypoint=custom-entry_1.x;",
	}
	for _, line := range valid {
		if !isClaudeFingerprintBillingText(line) {
			t.Errorf("complete line not recognized: %q", line)
		}
		for _, extra := range []string{" instructions", "\nKEEP", "\r\nKEEP", "; other=value;"} {
			if isClaudeFingerprintBillingText(line + extra) {
				t.Errorf("suffix recognized: %q", line+extra)
			}
		}
		if isClaudeFingerprintBillingText("Explain " + line) {
			t.Errorf("quoted line recognized")
		}
	}
	invalid := []string{
		"x-anthropic-billing-header",
		"x-anthropic-billing-header: cc_version=2.1.300.abc;",
		"x-anthropic-billing-header: cc_version=2.1.300.ab; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.300.abc; cc_entrypoint=cli; extra=value;",
		"x-anthropic-billing-header: cc_version=2.1.300.abc; cc_entrypoint=cli; cc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.300.abc;\ncc_entrypoint=cli;",
		"x-anthropic-billing-header: cc_version=2.1.300.abc; cc_entrypoint=cli",
	}
	for _, line := range invalid {
		if isClaudeFingerprintBillingText(line) {
			t.Errorf("ambiguous line recognized: %q", line)
		}
	}
}

func TestClaudeFingerprintSystemPreservationAndIdempotence(t *testing.T) {
	fp := config.ClaudeIdentityFingerprintConfig{CLIVersion: "2.1.300", Entrypoint: "cli"}
	texts := []string{" \nKEEP white space\t ", "  ", claudeCodeIdentityText + "\nKEEP suffix", "Explain x-anthropic-billing-header; KEEP", "x-anthropic-billing-header: cc_version=custom; cc_entrypoint=custom value;"}
	for _, text := range texts {
		raw, _ := json.Marshal(map[string]any{"system": text})
		out := applyClaudeFingerprintSystem(raw, fp)
		if got := gjson.GetBytes(out, "system.2.text").String(); got != text {
			t.Errorf("text changed: %q != %q", got, text)
		}
		twice := applyClaudeFingerprintSystem(out, fp)
		if string(twice) != string(out) {
			t.Errorf("not idempotent: %s vs %s", out, twice)
		}
	}
	for _, custom := range []config.ClaudeIdentityFingerprintConfig{fp, {CLIVersion: "custom-version", Entrypoint: "custom entry"}} {
		out := applyClaudeFingerprintSystem([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), custom)
		twice := applyClaudeFingerprintSystem(out, custom)
		if string(twice) != string(out) {
			t.Errorf("configured output not idempotent")
		}
	}
	raw := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.88.abc; cc_entrypoint=cli;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"custom","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral","ttl":"1h"},"extra":"KEEP"},{"type":"custom","text":"x-anthropic-billing-header: cc_version=2.1.88.abc; cc_entrypoint=cli;"},{"type":"text","text":"Explain You are Claude Code. KEEP","cache_control":{"type":"ephemeral","ttl":"1h"},"extra":"KEEP"}]}`)
	out := applyClaudeFingerprintSystem(raw, fp)
	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 5 {
		t.Fatalf("blocks=%d body=%s", len(blocks), out)
	}
	if blocks[2].Get("type").String() != "custom" || blocks[3].Get("type").String() != "custom" {
		t.Fatal("non-text blocks changed")
	}
	if blocks[2].Get("cache_control.ttl").String() != "1h" || blocks[4].Get("cache_control.ttl").String() != "1h" || blocks[4].Get("extra").String() != "KEEP" {
		t.Fatal("metadata lost")
	}
	if isClaudeIdentityBlock(blocks[2]) {
		t.Fatal("custom block counted as generated identity")
	}
	if countClientCacheControls(out) != 2 {
		t.Fatalf("client breakpoints=%d", countClientCacheControls(out))
	}
	cached := applyClaudeCacheControl(out)
	if string(cached) != string(out) {
		t.Fatal("client cache controls changed")
	}
}

func TestClaudeFingerprintPreservesFourClientBreakpoints(t *testing.T) {
	raw := []byte(`{"system":[{"type":"custom","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"Explain You are Claude Code. KEEP","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"three","cache_control":{"type":"ephemeral"}},{"type":"text","text":"four","cache_control":{"type":"ephemeral"}}]}`)
	out := applyClaudeFingerprintSystem(raw, config.ClaudeIdentityFingerprintConfig{CLIVersion: "2.1.300", Entrypoint: "cli"})
	if countClientCacheControls(out) != 4 {
		t.Fatal("client count")
	}
	out = limitClaudeCacheControls(out)
	if countCacheControls(out) != 4 {
		t.Fatal("total count")
	}
	if gjson.GetBytes(out, "system.1.cache_control").Exists() {
		t.Fatal("generated breakpoint kept over client budget")
	}
	for i := 2; i < 6; i++ {
		if !gjson.GetBytes(out, fmt.Sprintf("system.%d.cache_control", i)).Exists() {
			t.Fatalf("client breakpoint %d lost", i)
		}
	}
	if gjson.GetBytes(out, "system.2.cache_control.ttl").String() != "1h" {
		t.Fatal("client TTL lost")
	}
}
