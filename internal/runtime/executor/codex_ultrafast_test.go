package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexUltrafastPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attributes bool
		policy     string
		original   string
		want       string
	}{
		{name: "force from metadata", policy: "ultrafast", want: "ultrafast"},
		{name: "force from attributes", attributes: true, policy: "ultrafast", want: "ultrafast"},
		{name: "force overrides client", policy: "ultrafast", original: `{"service_tier":"flex"}`, want: "ultrafast"},
		{name: "pass client", policy: "pass", original: `{"service_tier":" UltraFast "}`, want: "ultrafast"},
		{name: "pass translated body", policy: "pass", want: "ultrafast"},
		{name: "priority overrides ultrafast", policy: "priority", want: "priority"},
		{name: "default strips ultrafast"},
		{name: "drop strips ultrafast", policy: "drop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Metadata: map[string]any{"codex_service_tier": tc.policy}}
			if tc.attributes {
				auth.Metadata = nil
				auth.Attributes = map[string]string{"codex_service_tier": tc.policy}
			}
			body := []byte(`{"model":"gpt-6.1-sol","service_tier":"ultrafast","input":"hello"}`)
			got := gjson.GetBytes(applyCodexServiceTierPolicy(body, []byte(tc.original), auth), "service_tier")
			if got.String() != tc.want || (tc.want == "" && got.Exists()) {
				t.Fatalf("outbound tier = %s, want %q", got.Raw, tc.want)
			}
		})
	}
}

func TestCodexUltrafastTransports(t *testing.T) {
	for _, useWebsocket := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ policy, want string }{
				{policy: "ultrafast", want: "ultrafast"},
				{policy: "pass", want: "ultrafast"},
				{policy: "priority", want: "priority"},
				{policy: "drop"},
			} {
				t.Run(fmt.Sprintf("websocket=%t/stream=%t/%s", useWebsocket, stream, tc.policy), func(t *testing.T) {
					type capturedRequest struct {
						body []byte
						hint string
						err  error
					}
					captured := make(chan capturedRequest, 1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_ultrafast","object":"response","status":"completed","output":[]}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if useWebsocket {
							conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								captured <- capturedRequest{err: err}
								return
							}
							defer conn.Close()
							_, body, err := conn.ReadMessage()
							captured <- capturedRequest{body: body, hint: r.Header.Get("X-Codex-Routing-Hint"), err: err}
							_ = conn.WriteMessage(websocket.TextMessage, completed)
							return
						}
						body, err := io.ReadAll(r.Body)
						captured <- capturedRequest{body: body, hint: r.Header.Get("X-Codex-Routing-Hint"), err: err}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
					}))
					defer server.Close()
					auth := &cliproxyauth.Auth{
						ID: "codex-ultrafast", Provider: "codex",
						Attributes: map[string]string{
							"base_url":                    server.URL + "/backend-api/codex",
							"header:x-codex-routing-hint": "model=wrong;tier=priority",
						},
						Metadata: map[string]any{"access_token": "test-token", "codex_service_tier": tc.policy},
					}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(&config.Config{})
					if useWebsocket {
						executor = NewCodexWebsocketsExecutor(&config.Config{})
					}
					req := cliproxyexecutor.Request{
						Model:   "gpt-6.1-sol",
						Payload: []byte(`{"model":"gpt-6.1-sol","input":"hello","service_tier":"ultrafast"}`),
					}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream}
					if stream {
						result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
						t.Fatal(err)
					}
					got := <-captured
					if got.err != nil {
						t.Fatal(got.err)
					}
					if tier := gjson.GetBytes(got.body, "service_tier"); tier.String() != tc.want || (tc.want == "" && tier.Exists()) {
						t.Fatalf("outbound tier = %s, want %q", tier.Raw, tc.want)
					}
					wantHint := "model=gpt-6.1-sol"
					if tc.want != "" {
						wantHint += ";tier=" + tc.want
					}
					if got.hint != wantHint {
						t.Fatalf("routing hint = %q, want %q", got.hint, wantHint)
					}
				})
			}
		}
	}
}

func TestCodexServiceTierRoutingHintBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		auth *cliproxyauth.Auth
		want string
	}{
		{name: "OAuth final model", body: `{"model":"gpt-6-astra","service_tier":"ultrafast"}`, auth: &cliproxyauth.Auth{Provider: "codex"}, want: "model=gpt-6-astra;tier=ultrafast"},
		{name: "API key", body: `{"model":"gpt-6-astra","service_tier":"ultrafast"}`, auth: &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}},
		{name: "no auth", body: `{"model":"gpt-6-astra"}`},
		{name: "separator injection", body: `{"model":"gpt-6-astra;tier=ultrafast"}`, auth: &cliproxyauth.Auth{}},
		{name: "header injection", body: `{"model":"gpt-6-astra\r\nInjected: true"}`, auth: &cliproxyauth.Auth{}},
		{name: "missing model", body: `{}`, auth: &cliproxyauth.Auth{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"x-codex-routing-hint": {"forged"}, "X-Codex-Routing-Hint": {"stale"}}
			applyCodexServiceTierRoutingHint(headers, []byte(tc.body), tc.auth)
			if got := headers.Get("X-Codex-Routing-Hint"); got != tc.want {
				t.Fatalf("hint = %q, want %q", got, tc.want)
			}
			for key := range headers {
				if key == "x-codex-routing-hint" {
					t.Fatal("raw lowercase hint must be removed")
				}
			}
		})
	}
}
