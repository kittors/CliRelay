package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestMiniMaxOllamaNativeHTTP(t *testing.T) {
	for _, model := range []string{"minimax-m3.1-flash", "minimax-m3", "minimax-m2.7", "custom-model"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", model, stream), func(t *testing.T) {
				called := false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/show" {
						_, _ = io.WriteString(w, `{"capabilities":["completion","vision","tools","thinking"]}`)
						return
					}
					called = true
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if r.URL.Path != "/api/chat" || gjson.GetBytes(body, "model").String() != model {
						t.Errorf("unexpected native request: %s %s", r.URL.Path, body)
					}
					for key, want := range map[string]string{
						"messages.0.images.0":                   "dGVzdA==",
						"messages.1.thinking":                   "retained reasoning",
						"messages.1.tool_calls.0.function.name": "lookup",
						"messages.2.tool_name":                  "lookup",
						"messages.2.content":                    "result",
						"tools.0.function.name":                 "lookup",
					} {
						if got := gjson.GetBytes(body, key).String(); got != want {
							t.Errorf("%s = %q, want %q", key, got, want)
						}
					}
					if gjson.GetBytes(body, "think").Exists() || gjson.GetBytes(body, "reasoning_effort").Exists() {
						t.Error("default request must use Ollama's model-specific thinking defaults")
					}
					if stream {
						w.Header().Set("Content-Type", "application/x-ndjson")
					}
					_, _ = fmt.Fprintf(w, `{"model":%q,"message":{"role":"assistant","content":"ok","thinking":"reply reasoning"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`+"\n", model)
				}))
				t.Cleanup(server.Close)
				payload := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"text","text":"check"},{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdA=="}}]},{"role":"assistant","content":"","reasoning_content":"retained reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, model))
				exec := NewOllamaCloudExecutor(&config.Config{})
				auth := toolBridgeAuth("ollama-cloud", map[string]string{"api_key": "test-key", "base_url": server.URL + "/v1"}, nil)
				req := cliproxyexecutor.Request{Model: model, Payload: payload, Format: sdktranslator.FormatOpenAI}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload, Stream: stream}
				var output strings.Builder
				if stream {
					result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output.Write(chunk.Payload)
					}
				} else {
					result, err := exec.Execute(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					output.Write(result.Payload)
				}
				if !called || !strings.Contains(output.String(), `"reasoning_content":"reply reasoning"`) {
					t.Fatalf("native reasoning was lost: called=%v, response=%s", called, output.String())
				}
			})
		}
	}
}
