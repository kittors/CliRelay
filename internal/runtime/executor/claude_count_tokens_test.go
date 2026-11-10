package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// newClaudeCountTokensUpstream mimics Anthropic's validation: count_tokens
// rejects a top-level metadata field, Messages accepts it.
func newClaudeCountTokensUpstream(t *testing.T, bodies map[string][]byte, headers map[string]http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodies[r.URL.Path] = body
		headers[r.URL.Path] = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			if gjson.GetBytes(body, "metadata").Exists() {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"metadata: Extra inputs are not permitted"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"input_tokens":11}`))
		case "/v1/messages":
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newClaudeCountTokensAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "claude-count-metadata-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-count-metadata",
			"base_url": baseURL,
		},
		Metadata: map[string]any{"account_uuid": "claude-count-metadata-account"},
	}
}

// With the identity fingerprint enabled, Messages carries metadata.user_id and
// count_tokens must not, or Anthropic answers 400.
func TestClaudeCountTokensOmitsFingerprintMetadata(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	bodies := map[string][]byte{}
	headers := map[string]http.Header{}
	server := newClaudeCountTokensUpstream(t, bodies, headers)
	defer server.Close()

	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Claude: config.ClaudeIdentityFingerprintConfig{Enabled: true},
		},
	}
	auth := newClaudeCountTokensAuth(server.URL)
	executor := NewClaudeExecutor(cfg)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"count these tokens"}]}`)

	resp, err := executor.CountTokens(contextWithInboundHeaders(http.MethodPost, "/v1/messages/count_tokens", http.Header{}), auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}, opts)
	if err != nil {
		t.Fatalf("CountTokens returned error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 11 {
		t.Fatalf("input_tokens = %d, want 11; payload=%s", got, string(resp.Payload))
	}
	countBody := bodies["/v1/messages/count_tokens"]
	if got := gjson.GetBytes(countBody, "metadata"); got.Exists() {
		t.Fatalf("count_tokens metadata = %s, want it absent", got.Raw)
	}
	if billing := gjson.GetBytes(countBody, "system.0.text").String(); !containsAll(billing, "x-anthropic-billing-header", "cc_version=") {
		t.Fatalf("count_tokens billing block = %q, want the fingerprint system block kept", billing)
	}
	if got := headers["/v1/messages/count_tokens"].Get("X-Claude-Code-Session-Id"); got == "" {
		t.Fatal("count_tokens is missing X-Claude-Code-Session-Id")
	}

	if _, err := executor.Execute(contextWithInboundHeaders(http.MethodPost, "/v1/messages", http.Header{}), auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}, opts); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if userID := gjson.GetBytes(bodies["/v1/messages"], "metadata.user_id").String(); gjson.Get(userID, "account_uuid").String() != "claude-count-metadata-account" {
		t.Fatalf("messages metadata.user_id = %q, want the fingerprint identity kept on Messages", userID)
	}
}

// A client that forwards its own metadata on count_tokens must not get a 400
// either; Messages keeps the client's metadata as before.
func TestClaudeCountTokensDropsClientMetadataWithoutFingerprint(t *testing.T) {
	bodies := map[string][]byte{}
	headers := map[string]http.Header{}
	server := newClaudeCountTokensUpstream(t, bodies, headers)
	defer server.Close()

	auth := newClaudeCountTokensAuth(server.URL)
	executor := NewClaudeExecutor(&config.Config{})
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	payload := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"client-user"},"messages":[{"role":"user","content":"count these tokens"}]}`)

	resp, err := executor.CountTokens(contextWithInboundHeaders(http.MethodPost, "/v1/messages/count_tokens", http.Header{}), auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}, opts)
	if err != nil {
		t.Fatalf("CountTokens returned error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 11 {
		t.Fatalf("input_tokens = %d, want 11; payload=%s", got, string(resp.Payload))
	}
	countBody := bodies["/v1/messages/count_tokens"]
	if got := gjson.GetBytes(countBody, "metadata"); got.Exists() {
		t.Fatalf("count_tokens metadata = %s, want it absent", got.Raw)
	}
	if got := gjson.GetBytes(countBody, "messages.0.content").String(); got != "count these tokens" {
		t.Fatalf("count_tokens messages = %s, want the rest of the body intact", string(countBody))
	}

	if _, err := executor.Execute(contextWithInboundHeaders(http.MethodPost, "/v1/messages", http.Header{}), auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: payload}, opts); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := gjson.GetBytes(bodies["/v1/messages"], "metadata").Exists(); !got {
		t.Fatalf("messages body = %s, want metadata still present on Messages", string(bodies["/v1/messages"]))
	}
}

func TestStripClaudeCountTokensMetadata(t *testing.T) {
	cases := map[string]string{
		`{"model":"m","metadata":{"user_id":"u"},"messages":[]}`: `{"model":"m","messages":[]}`,
		`{"model":"m","messages":[]}`:                            `{"model":"m","messages":[]}`,
	}
	for in, want := range cases {
		if got := string(stripClaudeCountTokensMetadata([]byte(in))); got != want {
			t.Fatalf("stripClaudeCountTokensMetadata(%s) = %s, want %s", in, got, want)
		}
	}
}
