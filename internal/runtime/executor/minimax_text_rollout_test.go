package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

// Exercise the shipped examples at the real HTTP boundary, including requests
// made with an explicitly saved legacy or custom model instead of a suggestion.
func TestMiniMaxTextExamplesHTTP(t *testing.T) {
	data, err := os.ReadFile("../../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"minimax-global", "minimax-cn", "minimax-global-anthropic", "minimax-cn-anthropic"} {
		t.Run(name, func(t *testing.T) {
			marker := "#   - name: \"" + name + "\"\n"
			_, block, found := strings.Cut(string(data), marker)
			if !found {
				t.Fatalf("example %s missing", name)
			}
			var fragment strings.Builder
			fragment.WriteString(strings.TrimPrefix(marker, "# "))
			for _, line := range strings.Split(block, "\n") {
				if !strings.HasPrefix(line, "#     ") {
					break
				}
				fragment.WriteString(strings.TrimPrefix(line, "# ") + "\n")
			}
			var entries []struct {
				BaseURL string `yaml:"base-url"`
				Skip    bool   `yaml:"skip-anthropic-processing"`
				Models  []struct {
					Name  string
					Alias string
				}
			}
			if err := yaml.Unmarshal([]byte(fragment.String()), &entries); err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || len(entries[0].Models) != 2 {
				t.Fatalf("unexpected examples: %+v", entries)
			}
			entry := entries[0]
			for i, want := range []string{"MiniMax-M3.1-Flash", "MiniMax-M3"} {
				if entry.Models[i].Name != want || entry.Models[i].Alias != want {
					t.Fatalf("model %d = %+v, want %s", i, entry.Models[i], want)
				}
			}
			for _, model := range []string{entry.Models[0].Name, entry.Models[1].Name, "MiniMax-M2.7", "saved-custom-model"} {
				for _, stream := range []bool{false, true} {
					for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
						t.Run(fmt.Sprintf("%s/stream=%v/effort=%s", model, stream, effort), func(t *testing.T) {
							minimaxTextHTTP(t, name, entry.BaseURL, model, entry.Skip, stream, effort, nil)
						})
					}
				}
			}
		})
	}
}

func TestMiniMaxGatewayHTTP(t *testing.T) {
	for _, target := range []struct {
		provider string
		baseURL  string
		models   []string
		exec     cliproxyauth.ProviderExecutor
	}{
		{"opencode-go", opencodeGoBaseURL, []string{"minimax-m3.1-flash-free", "minimax-m3"}, NewOpenCodeGoExecutor(&config.Config{})},
		{"commandcode", "https://api.commandcode.ai/provider/v1", []string{"minimax/minimax-m3.1-flash", "minimax/minimax-m3.1-flash:free", "MiniMaxAI/MiniMax-M3", "MiniMaxAI/MiniMax-M2.7"}, NewOpenAICompatExecutor("commandcode", &config.Config{})},
	} {
		for _, model := range target.models {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", target.provider, model, stream), func(t *testing.T) {
					minimaxTextHTTP(t, target.provider, target.baseURL, model, false, stream, "", target.exec)
				})
			}
		}
	}
}

func minimaxTextHTTP(t *testing.T, provider, baseURL, model string, messages, stream bool, effort string, exec cliproxyauth.ProviderExecutor) {
	t.Helper()
	format := sdktranslator.FormatOpenAI
	path := "/chat/completions"
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"check"},{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdA=="}}]},{"role":"assistant","content":null,"reasoning_content":"retained reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	if provider == "opencode-go" {
		// Its existing image-registry preprocessing is tested separately.
		body = strings.Replace(body, `[{"type":"text","text":"check"},{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdA=="}}]`, `"check"`, 1)
	}
	if exec == nil {
		exec = NewOpenAICompatExecutor(provider, &config.Config{})
	}
	auth := toolBridgeAuth(provider, map[string]string{"base_url": baseURL, "api_key": "test-key", "provider_key": provider}, nil)
	if messages {
		format = sdktranslator.FormatClaude
		path = "/v1/messages"
		body = `{"max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"check"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"dGVzdA=="}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"retained reasoning","signature":"test-signature"},{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
		exec = NewClaudeExecutor(&config.Config{})
		auth.Attributes["skip_anthropic_processing"] = "true"
	}
	// With no thinking options the provider supplies its own model defaults.
	options := ""
	if effort != "" {
		options = fmt.Sprintf(`"thinking":{"type":"adaptive"},"reasoning_effort":%q,`, effort)
		if messages {
			options = fmt.Sprintf(`"thinking":{"type":"adaptive"},"output_config":{"effort":%q},`, effort)
		}
	}
	payload := []byte(fmt.Sprintf(`{"model":%q,"stream":%v,%s%s`, model, stream, options, body[1:]))
	called := false
	ctx := cliproxyexecutor.WithRoundTripper(context.Background(), roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.Scheme+"://"+r.URL.Host+r.URL.Path != baseURL+path {
			t.Errorf("URL = %s, want %s", r.URL, baseURL+path)
		}
		out, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, key := range []string{"model", "messages", "tools", "thinking", "reasoning_effort", "output_config"} {
			if !reflect.DeepEqual(gjson.GetBytes(out, key).Value(), gjson.GetBytes(payload, key).Value()) {
				t.Errorf("%s changed: got %s, want %s", key, gjson.GetBytes(out, key), gjson.GetBytes(payload, key))
			}
		}
		if gjson.GetBytes(out, "service_tier").Exists() || gjson.GetBytes(out, "reasoning_split").Exists() {
			t.Error("unexpected provider options")
		}
		response := toolBridgeChatCompletion
		contentType := "application/json"
		if messages {
			response = toolBridgeClaudeMessage
		}
		if stream {
			contentType = "text/event-stream"
			response = toolBridgeChatStream
			if messages {
				response = toolBridgeClaudeStream
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	}))
	req := cliproxyexecutor.Request{Model: model, Payload: payload, Format: format}
	opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: payload, Stream: stream}
	if stream {
		result, err := exec.ExecuteStream(ctx, auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
	} else if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("HTTP transport was not called")
	}
}
