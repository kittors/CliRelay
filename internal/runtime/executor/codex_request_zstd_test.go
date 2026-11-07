package executor

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

	// codexZstdMagic opens every zstd frame.
	codexZstdMagic = "\x28\xb5\x2f\xfd"

	codexResponsesPayload = `{"model":"gpt-5.4","prompt_cache_key":"session-abc","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello zstd"}]}]}`
)

func boolPtr(v bool) *bool { return &v }

// codexLargeResponsesPayload builds a Responses request whose input is roughly
// size bytes of repetitive but not trivially uniform text, like a long history.
func codexLargeResponsesPayload(size int) string {
	var text strings.Builder
	for i := 0; text.Len() < size; i++ {
		text.WriteString("turn ")
		text.WriteString(strings.Repeat("abcdefghij", 3))
		text.WriteString(string(rune('a' + i%26)))
		text.WriteString(" the quick brown fox jumps over the lazy dog. ")
	}
	return `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text.String() + `"}]}]}`
}

func TestCodexChatGPTResponsesRequestLeavesAsZstd(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "execute-stream"
		}
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload, Stream: stream}); err != nil {
				t.Fatalf("request failed: %v", err)
			}
			calls := env.Upstream.Calls()
			if len(calls) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(calls))
			}
			call := calls[0]
			if call.URL != codexResponsesURL {
				t.Fatalf("URL = %s, want %s", call.URL, codexResponsesURL)
			}
			if got := call.Header.Get("Content-Encoding"); got != "zstd" {
				t.Fatalf("Content-Encoding = %q, want zstd", got)
			}
			if call.ContentLength != int64(len(call.Wire)) {
				t.Fatalf("Content-Length = %d for a %d byte wire body", call.ContentLength, len(call.Wire))
			}
			if !bytes.HasPrefix(call.Wire, []byte(codexZstdMagic)) {
				t.Fatalf("wire body is not a zstd frame: %q", call.Wire[:min(8, len(call.Wire))])
			}
			if got := call.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json: the payload is JSON, only its encoding changed", got)
			}
			plain := call.Plain(t)
			for path, want := range map[string]string{
				"model":                  "gpt-5.4",
				"input.0.content.0.text": "hello zstd",
			} {
				if got := gjson.GetBytes(plain, path).String(); got != want {
					t.Fatalf("decoded %s = %q, want %q (body %s)", path, got, want, plain)
				}
			}
			// The proxy scopes a client's own key per account; the key still
			// starts with what the client sent.
			if key := gjson.GetBytes(plain, "prompt_cache_key").String(); !strings.HasPrefix(key, "session-abc") {
				t.Fatalf("decoded prompt_cache_key = %q, want one derived from session-abc", key)
			}
			if !gjson.GetBytes(plain, "stream").Bool() {
				t.Fatalf("decoded body lost stream=true: %s", plain)
			}
		})
	}
}

// The bytes must be a plain default-level zstd frame the way the official client
// writes one: level 3, a window within the level's 2 MiB, no frame checksum.
func TestCodexZstdFrameMatchesOfficialShape(t *testing.T) {
	plain := []byte(codexLargeResponsesPayload(200 << 10))
	wire, ok := encodeCodexZstd(plain)
	if !ok {
		t.Fatal("encoder unavailable")
	}

	var header zstd.Header
	if err := header.Decode(wire); err != nil {
		t.Fatalf("not a zstd frame: %v", err)
	}
	if header.HasCheckSum {
		t.Error("frame carries a checksum; the official client writes none")
	}
	if header.WindowSize > codexZstdWindowSize {
		t.Errorf("window %d exceeds level 3's %d", header.WindowSize, codexZstdWindowSize)
	}

	// Level is not recorded in the frame, so compare with an independently built
	// encoder at each level: only the intended one reproduces the bytes.
	build := func(level zstd.EncoderLevel) []byte {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithWindowSize(codexZstdWindowSize), zstd.WithEncoderCRC(false))
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		return enc.EncodeAll(plain, nil)
	}
	if !bytes.Equal(wire, build(zstd.SpeedDefault)) {
		t.Error("output differs from a default (level 3) encoder")
	}
	for _, other := range []zstd.EncoderLevel{zstd.SpeedFastest, zstd.SpeedBetterCompression, zstd.SpeedBestCompression} {
		if bytes.Equal(wire, build(other)) {
			t.Errorf("output is identical to encoder level %v, so the comparison cannot tell levels apart", other)
		}
	}
}

func TestCodexZstdRoundTripsLargeHistory(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	payload := codexLargeResponsesPayload(1_500_000)
	if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload}); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	call := env.Upstream.Calls()[0]
	plain := call.Plain(t)
	if len(plain) < 1_500_000 {
		t.Fatalf("decoded body is %d bytes, shorter than the 1.5 MB history sent", len(plain))
	}
	if len(call.Wire)*4 >= len(plain) {
		t.Fatalf("wire body is %d bytes for %d plain bytes; expected better than 4:1 on repetitive history", len(call.Wire), len(plain))
	}
	if !strings.Contains(string(plain), "turn abcdefghij") {
		t.Fatal("decoded body does not carry the history")
	}
}

// The request log is what operators read; it must hold the JSON that was sent,
// and say it was sent compressed, never the compressed bytes.
func TestCodexRequestLogKeepsPlainBodyOfCompressedRequest(t *testing.T) {
	env := newCodexExecEnv(t, codexLoggingConfig())
	if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload}); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	call := env.Upstream.Calls()[0]
	log := env.RequestLog(t)

	if !strings.Contains(log, "Content-Encoding: zstd") {
		t.Errorf("log does not record the Content-Encoding the request left with:\n%s", log)
	}
	if !strings.Contains(log, string(call.Plain(t))) {
		t.Errorf("log body is not the decoded wire body:\n%s", log)
	}
	if strings.Contains(log, codexZstdMagic) {
		t.Errorf("log holds compressed bytes:\n%q", log)
	}
}

func TestCodexRequestStaysPlainWhenCompressionDoesNotApply(t *testing.T) {
	relayAuth := &cliproxyauth.Auth{
		ID:         "codex-oauth-relay",
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": "https://relay.example/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}
	cases := []struct {
		name string
		cfg  *config.Config
		call codexCall
		url  string
	}{
		{
			name: "api-key account on the default backend",
			call: codexCall{Auth: codexAPIKeyAuth(), From: "openai-response", Payload: codexResponsesPayload},
			url:  codexResponsesURL,
		},
		{
			name: "oauth account pointed at another host",
			call: codexCall{Auth: relayAuth, From: "openai-response", Payload: codexResponsesPayload},
			url:  "https://relay.example/backend-api/codex/responses",
		},
		{
			name: "switched off",
			cfg:  &config.Config{CodexRequestZstd: boolPtr(false)},
			call: codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload},
			url:  codexResponsesURL,
		},
		{
			name: "responses/compact",
			call: codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload, Alt: "responses/compact"},
			url:  "https://chatgpt.com/backend-api/codex/responses/compact",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCodexExecEnv(t, tc.cfg)
			if tc.call.Alt == "responses/compact" {
				env.Upstream.respond = func(int, codexWireCall) *http.Response {
					resp := codexErrorResponse(http.StatusOK, `{"id":"resp_1","object":"response.compaction","output":[]}`)
					return resp
				}
			}
			if _, err := env.Do(t, tc.call); err != nil {
				t.Fatalf("request failed: %v", err)
			}
			calls := env.Upstream.Calls()
			if len(calls) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(calls))
			}
			call := calls[0]
			if call.URL != tc.url {
				t.Fatalf("URL = %s, want %s", call.URL, tc.url)
			}
			if got := call.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want none", got)
			}
			if bytes.HasPrefix(call.Wire, []byte(codexZstdMagic)) || !gjson.ValidBytes(call.Wire) {
				t.Fatalf("body is not plain JSON: %q", call.Wire)
			}
			if call.ContentLength != int64(len(call.Wire)) {
				t.Fatalf("Content-Length = %d for a %d byte body", call.ContentLength, len(call.Wire))
			}
		})
	}
}

func TestCodexRequestCompressionApplies(t *testing.T) {
	newReq := func(method, target string) *http.Request {
		req, err := http.NewRequest(method, target, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	withEncoding := newReq(http.MethodPost, codexResponsesURL)
	withEncoding.Header.Set("Content-Encoding", "gzip")
	nonCodexOAuth := codexOAuthAuth()
	nonCodexOAuth.Provider = "claude"

	cases := []struct {
		name string
		cfg  *config.Config
		auth *cliproxyauth.Auth
		req  *http.Request
		want bool
	}{
		{"oauth chatgpt responses", nil, codexOAuthAuth(), newReq(http.MethodPost, codexResponsesURL), true},
		{"empty config is the default", &config.Config{}, codexOAuthAuth(), newReq(http.MethodPost, codexResponsesURL), true},
		{"explicitly on", &config.Config{CodexRequestZstd: boolPtr(true)}, codexOAuthAuth(), newReq(http.MethodPost, codexResponsesURL), true},
		{"explicitly off", &config.Config{CodexRequestZstd: boolPtr(false)}, codexOAuthAuth(), newReq(http.MethodPost, codexResponsesURL), false},
		{"host case and port do not matter", nil, codexOAuthAuth(), newReq(http.MethodPost, "https://CHATGPT.com:443/backend-api/codex/responses"), true},
		{"trailing slash", nil, codexOAuthAuth(), newReq(http.MethodPost, codexResponsesURL+"/"), true},
		{"compact is left alone", nil, codexOAuthAuth(), newReq(http.MethodPost, codexResponsesURL+"/compact"), false},
		{"other codex endpoint", nil, codexOAuthAuth(), newReq(http.MethodPost, "https://chatgpt.com/backend-api/codex/alpha/search"), false},
		{"look-alike host", nil, codexOAuthAuth(), newReq(http.MethodPost, "https://chatgpt.com.evil.example/backend-api/codex/responses"), false},
		{"subdomain is another host", nil, codexOAuthAuth(), newReq(http.MethodPost, "https://api.chatgpt.com/backend-api/codex/responses"), false},
		{"api-key account", nil, codexAPIKeyAuth(), newReq(http.MethodPost, codexResponsesURL), false},
		{"other provider", nil, nonCodexOAuth, newReq(http.MethodPost, codexResponsesURL), false},
		{"no account", nil, nil, newReq(http.MethodPost, codexResponsesURL), false},
		{"not a POST", nil, codexOAuthAuth(), newReq(http.MethodGet, codexResponsesURL), false},
		{"already encoded", nil, codexOAuthAuth(), withEncoding, false},
		{"no request", nil, codexOAuthAuth(), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRequestCompressionApplies(tc.cfg, tc.auth, tc.req); got != tc.want {
				t.Fatalf("codexRequestCompressionApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

// Everything chatgpt.com specific hangs off one predicate; it has to agree with
// the URL the executor actually resolves for OAuth accounts.
func TestCodexChatGPTBackendBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		auth    *cliproxyauth.Auth
		baseURL string
		want    bool
	}{
		{"oauth with the default (empty) base url", codexOAuthAuth(), "", true},
		{"oauth with the chatgpt base url", codexOAuthAuth(), "https://chatgpt.com/backend-api/codex", true},
		{"oauth with another host", codexOAuthAuth(), "https://relay.example/v1", false},
		{"oauth with an unparsable url", codexOAuthAuth(), "http://[::1", false},
		{"api key on the default url", codexAPIKeyAuth(), "", false},
		{"api key on the chatgpt url", codexAPIKeyAuth(), "https://chatgpt.com/backend-api/codex", false},
		{"no account", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCodexChatGPTBackendBaseURL(tc.auth, tc.baseURL); got != tc.want {
				t.Fatalf("isCodexChatGPTBackendBaseURL = %v, want %v", got, tc.want)
			}
		})
	}
}
