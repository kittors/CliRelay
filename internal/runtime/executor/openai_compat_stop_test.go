package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/claude"
	auth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	trans "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type stopRecordingTransport struct {
	body   []byte
	calls  int
	stream bool
}

func (r *stopRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "recording.invalid" {
		return nil, fmt.Errorf("unexpected recording target: %s", req.URL.Host)
	}
	r.calls++
	var err error
	r.body, err = io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	body := `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	contentType := "application/json"
	if r.stream {
		contentType = "text/event-stream"
		body = "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestOpenAICompatExecutorStopSequences(t *testing.T) {
	cases := []struct {
		name               string
		source             trans.Format
		field, value, want string
	}{
		{"claude_omitted", trans.FormatClaude, "stop_sequences", "", ""},
		{"claude_empty", trans.FormatClaude, "stop_sequences", "[]", ""},
		{"claude_one", trans.FormatClaude, "stop_sequences", `["</block>"]`, `["</block>"]`},
		{"claude_two", trans.FormatClaude, "stop_sequences", `["a","b"]`, `["a","b"]`},
		{"claude_four", trans.FormatClaude, "stop_sequences", `["a","b","c","d"]`, `["a","b","c","d"]`},
		{"chat_omitted", trans.FormatOpenAI, "stop", "", ""},
		{"chat_null", trans.FormatOpenAI, "stop", "null", "null"},
		{"chat_empty", trans.FormatOpenAI, "stop", "[]", "[]"},
		{"chat_scalar", trans.FormatOpenAI, "stop", `"</block>"`, `"</block>"`},
		{"chat_one", trans.FormatOpenAI, "stop", `["</block>"]`, `["</block>"]`},
		{"chat_two", trans.FormatOpenAI, "stop", `["a","b"]`, `["a","b"]`},
	}
	for _, model := range []string{"glm-5.3", "other-compatible-model"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, model, stream), func(t *testing.T) {
					input := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"ok"}]`
					if tc.value != "" {
						input += `,"` + tc.field + `":` + tc.value
					}
					input += fmt.Sprintf(`,"stream":%t}`, stream)
					recorder := &stopRecordingTransport{stream: stream}
					ctx := exec.WithRoundTripper(context.Background(), recorder)
					executor := NewOpenAICompatExecutor("stop-fixture", &config.Config{})
					credential := &auth.Auth{ID: "stop-fixture", Provider: "stop-fixture", Attributes: map[string]string{"base_url": "https://recording.invalid/v1"}}
					request := exec.Request{Model: model, Payload: []byte(input)}
					opts := exec.Options{SourceFormat: tc.source, OriginalRequest: []byte(input)}
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
					} else {
						if _, err := executor.Execute(ctx, credential, request, opts); err != nil {
							t.Fatal(err)
						}
					}
					if recorder.calls != 1 {
						t.Fatalf("got %d calls", recorder.calls)
					}
					got := gjson.GetBytes(recorder.body, "stop").Raw
					var actual, expected any
					if got != "" {
						if err := json.Unmarshal([]byte(got), &actual); err != nil {
							t.Fatal(err)
						}
					}
					if tc.want != "" {
						if err := json.Unmarshal([]byte(tc.want), &expected); err != nil {
							t.Fatal(err)
						}
					}
					if (got == "") != (tc.want == "") || !reflect.DeepEqual(actual, expected) {
						t.Fatalf("wire stop=%s, want %s; body=%s", got, tc.want, recorder.body)
					}
					if gjson.GetBytes(recorder.body, "stream").Bool() != stream {
						t.Fatal("wire stream changed")
					}
				})
			}
		}
	}
}
