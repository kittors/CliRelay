package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorPreservesMergedToolReasoning(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, want string
	}{
		{"both", "first", "second", "first\nsecond"},
		{"first_only", "first", "", "first"},
		{"second_only", "", "second", "second"},
		{"neither", "", "", ""},
		{"repeated", "same", "same", "same\nsame"},
	} {
		for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, source, stream), func(t *testing.T) {
					var payload []byte
					if source == sdktranslator.FormatOpenAIResponse {
						payload = []byte(fmt.Sprintf(`{"model":"compat-fixture","input":[
							{"type":"reasoning","summary":[{"type":"summary_text","text":%q}]},
							{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"},
							{"type":"reasoning","summary":[{"type":"summary_text","text":%q}]},
							{"type":"function_call","call_id":"b","name":"lookup","arguments":"{}"},
							{"type":"function_call_output","call_id":"a","output":"result-a"},
							{"type":"function_call_output","call_id":"b","output":"result-b"}
						]}`, tc.first, tc.second))
					} else {
						payload = []byte(fmt.Sprintf(`{"model":"compat-fixture","messages":[
							{"role":"assistant","reasoning_content":%q,"tool_calls":[{"id":"a","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
							{"role":"assistant","reasoning_content":%q,"tool_calls":[{"id":"b","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
							{"role":"tool","tool_call_id":"a","content":"result-a"},
							{"role":"tool","tool_call_id":"b","content":"result-b"}
						]}`, tc.first, tc.second))
					}
					recorder := &stopRecordingTransport{stream: stream}
					ctx := cliproxyexecutor.WithRoundTripper(context.Background(), recorder)
					executor := NewOpenAICompatExecutor("reasoning-fixture", &config.Config{})
					credential := &cliproxyauth.Auth{ID: "reasoning-fixture", Provider: "reasoning-fixture", Attributes: map[string]string{"base_url": "https://recording.invalid/v1"}}
					request := cliproxyexecutor.Request{Model: "compat-fixture", Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: source, OriginalRequest: payload}
					if stream {
						result, err := executor.ExecuteStream(ctx, credential, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, credential, request, opts); err != nil {
						t.Fatal(err)
					}
					messages := gjson.GetBytes(recorder.body, "messages").Array()
					if recorder.calls != 1 || len(messages) != 3 {
						t.Fatalf("calls=%d messages=%d, want one call and one assistant plus two results: %s", recorder.calls, len(messages), recorder.body)
					}
					if got := messages[0].Get("reasoning_content").String(); got != tc.want {
						t.Fatalf("upstream reasoning=%q, want %q: %s", got, tc.want, recorder.body)
					}
					if messages[0].Get("tool_calls.#").Int() != 2 || messages[0].Get("tool_calls.0.id").String() != "a" || messages[0].Get("tool_calls.1.id").String() != "b" || messages[1].Get("tool_call_id").String() != "a" || messages[2].Get("tool_call_id").String() != "b" {
						t.Fatalf("tool calls or result pairing changed: %s", recorder.body)
					}
				})
			}
		}
	}
}
