package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Check the final wire after translation, thinking application, and fingerprinting.
func TestClaudeNativeEffortSurvivesExecutor(t *testing.T) {
	t.Run("stream", func(t *testing.T) { runClaudeNativeEffortExecutor(t, true) })
	t.Run("non-stream", func(t *testing.T) { runClaudeNativeEffortExecutor(t, false) })
}

func runClaudeNativeEffortExecutor(t *testing.T, stream bool) {
	reg := sdkmodelcatalog.GlobalRegistry()
	reg.RegisterClient("executor-native-effort-fixture", "claude", sdkmodelcatalog.StaticModelDefinitionsByChannel("claude"))
	defer reg.UnregisterClient("executor-native-effort-fixture")
	for _, format := range []string{"openai-response", "openai"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			t.Run(format+"/"+effort, func(t *testing.T) {
				requestBody := make(chan []byte, 1)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					requestBody <- body
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
				}))
				defer provider.Close()
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": provider.URL}}
				payload := []byte(`{"model":"claude-opus-5-5","input":"OK","messages":[{"role":"user","content":"OK"}],"reasoning":{"effort":"` + effort + `"},"reasoning_effort":"` + effort + `","stream":` + strconv.FormatBool(stream) + `}`)
				executor := NewClaudeExecutor(&config.Config{IdentityFingerprint: config.DefaultIdentityFingerprintConfig()})
				request := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(format), Stream: stream}
				if stream {
					result, err := executor.ExecuteStream(context.Background(), auth, request, options)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					// Only the outbound wire matters here; the minimal fixture response may not aggregate.
					_, _ = executor.Execute(context.Background(), auth, request, options)
				}
				var body []byte
				select {
				case body = <-requestBody:
				default:
					t.Fatal("provider received no request")
				}
				if gjson.GetBytes(body, "thinking.type").String() != "adaptive" || gjson.GetBytes(body, "output_config.effort").String() != effort || gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
					t.Fatalf("final wire lost native effort: %s", body)
				}
			})
		}
	}
}
