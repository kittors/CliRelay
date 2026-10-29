package executor

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// Ciphertexts as the three families write them. OpenAI's is a Fernet token and so
// opens with "gAAAA"; the rest are what Responses translators carry through from
// other vendors' reasoning.
const (
	openAICipherA  = "gAAAAABpAbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcdefghijklmnopqrstuvwxyzAAAA"
	openAICipherB  = "gAAAAABqZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZYXWVUTSRQPONMLKJIHGFEDCBAzzzz"
	claudeSignaure = "EqQBCkYIAxgCKkD5mZ7kZ9vQ1Zx7oRmqf3t0V2c1k8s9Hn6Gq0Yw1y2u3i4o5p6a7s8d9f0g1h2j3k4l5z6x7c8v9b0n1m2"
	geminiSignaure = "CiQBjz1rX5mS3o8pQ0Zx7oRmqf3t0V2c1k8s9Hn6Gq0Yw1y2u3i4o5p6a7s8d9f0g1h2j3k4l5z6x7c8v9b0"
	claudeRedacted = "claude-redacted-thinking:EmwKAhgBEgxmb29iYXI="

	codexInvalidCiphertextBodyJSON = `{"error":{"message":"The encrypted content for item rs_0123 could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`
)

func reasoningItem(ciphertext string) string {
	return `{"type":"reasoning","id":"rs_x","summary":[],"encrypted_content":"` + ciphertext + `"}`
}

// responsesWithInput is a Responses request replaying the given items between a
// user turn and an assistant turn.
func responsesWithInput(items ...string) string {
	parts := []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`}
	parts = append(parts, items...)
	parts = append(parts, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`)
	return `{"model":"gpt-5.4","input":[` + strings.Join(parts, ",") + `]}`
}

// replayedCiphertexts lists the encrypted_content of every reasoning item in a
// request body, in order.
func replayedCiphertexts(body []byte) []string {
	var out []string
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" {
			out = append(out, item.Get("encrypted_content").String())
		}
		return true
	})
	return out
}

func inputTypes(body []byte) []string {
	var out []string
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		out = append(out, item.Get("type").String())
		return true
	})
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDropCodexReasoningItems(t *testing.T) {
	compaction := `{"type":"compaction","id":"cmp_1","encrypted_content":"` + claudeSignaure + `"}`
	functionCall := `{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`
	cases := []struct {
		name        string
		items       []string
		drop        func(string) bool
		wantDropped int
		wantKept    []string // encrypted_content of the surviving reasoning items
		wantTypes   []string
	}{
		{
			name:        "foreign filter keeps OpenAI ciphertext",
			items:       []string{reasoningItem(openAICipherA), reasoningItem(claudeSignaure), reasoningItem(geminiSignaure), reasoningItem(claudeRedacted)},
			drop:        isForeignCodexCiphertext,
			wantDropped: 3,
			wantKept:    []string{openAICipherA},
			wantTypes:   []string{"message", "reasoning", "message", "message"},
		},
		{
			name:        "foreign filter leaves an all-OpenAI history alone",
			items:       []string{reasoningItem(openAICipherA), functionCall, reasoningItem(openAICipherB)},
			drop:        isForeignCodexCiphertext,
			wantDropped: 0,
			wantKept:    []string{openAICipherA, openAICipherB},
			wantTypes:   []string{"message", "reasoning", "function_call", "reasoning", "message", "message"},
		},
		{
			name:        "any filter drops OpenAI ciphertext too",
			items:       []string{reasoningItem(openAICipherA), functionCall, reasoningItem(openAICipherB)},
			drop:        anyCodexCiphertext,
			wantDropped: 2,
			wantKept:    nil,
			wantTypes:   []string{"message", "function_call", "message", "message"},
		},
		{
			name:        "items other than reasoning are never touched",
			items:       []string{compaction, functionCall, reasoningItem(claudeSignaure)},
			drop:        anyCodexCiphertext,
			wantDropped: 1,
			wantKept:    nil,
			wantTypes:   []string{"message", "compaction", "function_call", "message", "message"},
		},
		{
			name:        "reasoning without ciphertext has nothing to refuse",
			items:       []string{`{"type":"reasoning","id":"rs_1","summary":[]}`, `{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":""}`, `{"type":"reasoning","id":"rs_3","summary":[],"encrypted_content":null}`, `{"type":"reasoning","id":"rs_4","summary":[],"encrypted_content":7}`},
			drop:        anyCodexCiphertext,
			wantDropped: 0,
			wantKept:    []string{"", "", "", "7"},
			wantTypes:   []string{"message", "reasoning", "reasoning", "reasoning", "reasoning", "message", "message"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(responsesWithInput(tc.items...))
			got, dropped := dropCodexReasoningItems(body, tc.drop)
			if dropped != tc.wantDropped {
				t.Fatalf("dropped = %d, want %d", dropped, tc.wantDropped)
			}
			if !gjson.ValidBytes(got) {
				t.Fatalf("result is not valid JSON: %s", got)
			}
			if tc.wantDropped == 0 && !bytes.Equal(got, body) {
				t.Fatalf("body changed although nothing was dropped:\n%s", got)
			}
			if kept := replayedCiphertexts(got); !equalStrings(kept, tc.wantKept) {
				t.Fatalf("surviving ciphertexts = %q, want %q", kept, tc.wantKept)
			}
			if types := inputTypes(got); !equalStrings(types, tc.wantTypes) {
				t.Fatalf("input item types = %q, want %q", types, tc.wantTypes)
			}
			// Everything that is not a dropped item is untouched, byte for byte.
			if got := gjson.GetBytes(got, "model").String(); got != "gpt-5.4" {
				t.Fatalf("model changed to %q", got)
			}
		})
	}

	t.Run("input that is not a list is left alone", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.4","input":"encrypted_content text"}`)
		got, dropped := dropCodexReasoningItems(body, anyCodexCiphertext)
		if dropped != 0 || !bytes.Equal(got, body) {
			t.Fatalf("dropped=%d body=%s", dropped, got)
		}
	})
	t.Run("empty and ciphertext-free bodies", func(t *testing.T) {
		for _, body := range [][]byte{nil, {}, []byte(`{"include":["reasoning.encrypted_content"],"input":[]}`)} {
			got, dropped := dropCodexReasoningItems(body, anyCodexCiphertext)
			if dropped != 0 || !bytes.Equal(got, body) {
				t.Fatalf("dropped=%d body=%s", dropped, got)
			}
		}
	})
	t.Run("kept items keep their exact bytes", func(t *testing.T) {
		odd := `{"type":"message",  "role":"user","content":[{"type":"input_text","text":"a  b  é \"quoted\""}]}`
		body := []byte(`{"model":"m","input":[` + odd + `,` + reasoningItem(claudeSignaure) + `]}`)
		got, dropped := dropCodexReasoningItems(body, isForeignCodexCiphertext)
		if dropped != 1 || !bytes.Contains(got, []byte(odd)) {
			t.Fatalf("dropped=%d body=%s", dropped, got)
		}
	})
}

func TestMergeCodexClientInclude(t *testing.T) {
	const translated = `["reasoning.encrypted_content"]`
	responses := sdktranslator.FormatOpenAIResponse
	cases := []struct {
		name    string
		source  sdktranslator.Format
		client  string // client payload
		body    string // body after translation
		want    string // include after merging
		changed bool
	}{
		{
			name:    "client entries join the translator's",
			source:  responses,
			client:  `{"include":["web_search_call.action.sources","message.output_text.logprobs"]}`,
			body:    `{"model":"m","include":` + translated + `}`,
			want:    `["reasoning.encrypted_content","web_search_call.action.sources","message.output_text.logprobs"]`,
			changed: true,
		},
		{
			name:   "already present is not repeated and not rewritten",
			source: responses,
			client: `{"include":["reasoning.encrypted_content"]}`,
			body:   `{"model":"m","include":` + translated + `}`,
			want:   translated,
		},
		{
			name:    "duplicates and blanks inside the client list collapse",
			source:  responses,
			client:  `{"include":["a.b","a.b","  ","a.c",3,null,{"x":1}]}`,
			body:    `{"model":"m","include":` + translated + `}`,
			want:    `["reasoning.encrypted_content","a.b","a.c"]`,
			changed: true,
		},
		{
			name:   "no include from the client",
			source: responses,
			client: `{"model":"m"}`,
			body:   `{"model":"m","include":` + translated + `}`,
			want:   translated,
		},
		{
			name:   "empty include from the client",
			source: responses,
			client: `{"include":[]}`,
			body:   `{"model":"m","include":` + translated + `}`,
			want:   translated,
		},
		{
			name:   "include that is not a list is ignored",
			source: responses,
			client: `{"include":"web_search_call.action.sources"}`,
			body:   `{"model":"m","include":` + translated + `}`,
			want:   translated,
		},
		{
			name:    "client entries stand alone when the translator wrote none",
			source:  responses,
			client:  `{"include":["web_search_call.action.sources"]}`,
			body:    `{"model":"m"}`,
			want:    `["web_search_call.action.sources"]`,
			changed: true,
		},
		{
			name:   "chat completions requests are not Responses requests",
			source: sdktranslator.FormatOpenAI,
			client: `{"include":["web_search_call.action.sources"]}`,
			body:   `{"model":"m","include":` + translated + `}`,
			want:   translated,
		},
		{
			name:   "claude requests are not Responses requests",
			source: sdktranslator.FormatClaude,
			client: `{"include":["web_search_call.action.sources"]}`,
			body:   `{"model":"m","include":` + translated + `}`,
			want:   translated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeCodexClientInclude([]byte(tc.body), []byte(tc.client), tc.source)
			if !gjson.ValidBytes(got) {
				t.Fatalf("result is not valid JSON: %s", got)
			}
			if inc := gjson.GetBytes(got, "include").Raw; inc != tc.want {
				t.Fatalf("include = %s, want %s", inc, tc.want)
			}
			if !tc.changed && !bytes.Equal(got, []byte(tc.body)) {
				t.Fatalf("body was rewritten although include did not change:\n%s", got)
			}
			if gjson.GetBytes(got, "model").String() != "m" {
				t.Fatalf("other fields were disturbed: %s", got)
			}
		})
	}
}

// Foreign reasoning ciphertext never reaches the ChatGPT backend; OpenAI's own is
// replayed as the client sent it.
func TestCodexForeignReasoningCiphertextIsDroppedBeforeSending(t *testing.T) {
	functionCall := `{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`
	compaction := `{"type":"compaction","id":"cmp_1","encrypted_content":"` + claudeSignaure + `"}`
	payload := responsesWithInput(
		reasoningItem(claudeSignaure),
		reasoningItem(openAICipherA),
		functionCall,
		reasoningItem(geminiSignaure),
		reasoningItem(claudeRedacted),
		compaction,
		reasoningItem(openAICipherB),
	)
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "execute-stream"
		}
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload, Stream: stream}); err != nil {
				t.Fatalf("request failed: %v", err)
			}
			calls := env.Upstream.Calls()
			if len(calls) != 1 {
				t.Fatalf("upstream saw %d requests, want 1: a clean request needs no retry", len(calls))
			}
			sent := calls[0].Plain(t)
			if got, want := replayedCiphertexts(sent), []string{openAICipherA, openAICipherB}; !equalStrings(got, want) {
				t.Fatalf("replayed reasoning = %q, want %q", got, want)
			}
			wantTypes := []string{"message", "reasoning", "function_call", "compaction", "reasoning", "message", "message"}
			if got := inputTypes(sent); !equalStrings(got, wantTypes) {
				t.Fatalf("input items = %q, want %q", got, wantTypes)
			}
			if got := gjson.GetBytes(sent, "input.3.encrypted_content").String(); got != claudeSignaure {
				t.Fatalf("a compaction item must be passed through untouched, got %q", got)
			}
		})
	}
}

// The ciphertext format check is specific to the ChatGPT backend; other Codex
// upstreams are not assumed to share it, so nothing is dropped up front there.
func TestCodexForeignReasoningCiphertextIsKeptForOtherUpstreams(t *testing.T) {
	relayAuth := &cliproxyauth.Auth{
		ID:         "codex-oauth-relay",
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": "https://relay.example/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}
	payload := responsesWithInput(reasoningItem(claudeSignaure), reasoningItem(openAICipherA))
	for name, auth := range map[string]*cliproxyauth.Auth{"api key": codexAPIKeyAuth(), "oauth on another host": relayAuth} {
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			if _, err := env.Do(t, codexCall{Auth: auth, From: "openai-response", Payload: payload}); err != nil {
				t.Fatalf("request failed: %v", err)
			}
			sent := env.Upstream.Calls()[0].Plain(t)
			if got, want := replayedCiphertexts(sent), []string{claudeSignaure, openAICipherA}; !equalStrings(got, want) {
				t.Fatalf("replayed reasoning = %q, want %q", got, want)
			}
		})
	}
}

func TestCodexClientIncludeReachesUpstream(t *testing.T) {
	clientInclude := `"include":["web_search_call.action.sources","reasoning.encrypted_content","message.output_text.logprobs"]`
	cases := []struct {
		name    string
		auth    *cliproxyauth.Auth
		from    string
		payload string
		want    string
	}{
		{
			name:    "responses client on a ChatGPT account",
			auth:    codexOAuthAuth(),
			from:    "openai-response",
			payload: `{"model":"gpt-5.4",` + clientInclude + `,"input":"hi"}`,
			want:    `["reasoning.encrypted_content","web_search_call.action.sources","message.output_text.logprobs"]`,
		},
		{
			name:    "responses client on an API key account",
			auth:    codexAPIKeyAuth(),
			from:    "openai-response",
			payload: `{"model":"gpt-5.4",` + clientInclude + `,"input":"hi"}`,
			want:    `["reasoning.encrypted_content","web_search_call.action.sources","message.output_text.logprobs"]`,
		},
		{
			name:    "responses client without include",
			auth:    codexOAuthAuth(),
			from:    "openai-response",
			payload: `{"model":"gpt-5.4","input":"hi"}`,
			want:    `["reasoning.encrypted_content"]`,
		},
		{
			name:    "chat completions payload is not read for include",
			auth:    codexOAuthAuth(),
			from:    "openai",
			payload: `{"model":"gpt-5.4",` + clientInclude + `,"messages":[{"role":"user","content":"hi"}]}`,
			want:    `["reasoning.encrypted_content"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			if _, err := env.Do(t, codexCall{Auth: tc.auth, From: tc.from, Payload: tc.payload}); err != nil {
				t.Fatalf("request failed: %v", err)
			}
			sent := env.Upstream.Calls()[0].Plain(t)
			if got := gjson.GetBytes(sent, "include").Raw; got != tc.want {
				t.Fatalf("include = %s, want %s", got, tc.want)
			}
		})
	}
}

// answerWithCipherRejectionThenSuccess refuses the first request the way the
// ChatGPT backend refuses replayed reasoning it cannot decrypt, then accepts.
func answerWithCipherRejectionThenSuccess(n int, _ codexWireCall) *http.Response {
	if n == 0 {
		return codexErrorResponse(http.StatusBadRequest, codexInvalidCiphertextBodyJSON)
	}
	return nil
}

func TestCodexRetriesOnceWithoutReasoningWhenUpstreamCannotDecryptIt(t *testing.T) {
	functionCall := `{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`
	// OpenAI-shaped ciphertext passes the up-front check; only the upstream can
	// tell it was minted for another account.
	payload := responsesWithInput(reasoningItem(openAICipherA), functionCall, reasoningItem(openAICipherB))
	cases := []struct {
		name         string
		auth         *cliproxyauth.Auth
		stream       bool
		wantZstd     bool
		wantRequests int
	}{
		{name: "chatgpt account", auth: codexOAuthAuth(), wantZstd: true},
		{name: "chatgpt account streaming", auth: codexOAuthAuth(), stream: true, wantZstd: true},
		{name: "api key account", auth: codexAPIKeyAuth(), wantZstd: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCodexExecEnv(t, codexLoggingConfig())
			env.Upstream.respond = answerWithCipherRejectionThenSuccess
			if _, err := env.Do(t, codexCall{Auth: tc.auth, From: "openai-response", Payload: payload, Stream: tc.stream}); err != nil {
				t.Fatalf("the retry should have rescued the request, got %v", err)
			}
			calls := env.Upstream.Calls()
			if len(calls) != 2 {
				t.Fatalf("upstream saw %d requests, want the original and one retry", len(calls))
			}
			first, second := calls[0].Plain(t), calls[1].Plain(t)
			if got, want := replayedCiphertexts(first), []string{openAICipherA, openAICipherB}; !equalStrings(got, want) {
				t.Fatalf("first request replayed %q, want %q", got, want)
			}
			if got := replayedCiphertexts(second); len(got) != 0 {
				t.Fatalf("retry still replays reasoning: %q", got)
			}
			if got, want := inputTypes(second), []string{"message", "function_call", "message", "message"}; !equalStrings(got, want) {
				t.Fatalf("retry input items = %q, want %q: only reasoning may go", got, want)
			}
			for _, call := range calls {
				if (call.Header.Get("Content-Encoding") == "zstd") != tc.wantZstd {
					t.Fatalf("Content-Encoding = %q (want zstd: %v)", call.Header.Get("Content-Encoding"), tc.wantZstd)
				}
				if call.ContentLength != int64(len(call.Wire)) {
					t.Fatalf("Content-Length = %d for a %d byte body: the retry must carry its own length", call.ContentLength, len(call.Wire))
				}
			}
			// Everything but the reasoning is identical between the two attempts.
			firstWithout, _ := dropCodexReasoningItems(first, anyCodexCiphertext)
			if !bytes.Equal(firstWithout, second) {
				t.Fatalf("retry differs from the original beyond the reasoning items:\nfirst  %s\nsecond %s", firstWithout, second)
			}

			requestLog := env.RequestLog(t)
			if strings.Count(requestLog, "=== API REQUEST ") != 2 {
				t.Fatalf("request log should show both attempts:\n%s", requestLog)
			}
			if !strings.Contains(requestLog, openAICipherA) {
				t.Fatalf("first attempt missing from the log:\n%s", requestLog)
			}
			responseLog := env.ResponseLog(t)
			if !strings.Contains(responseLog, "Status: 400") || !strings.Contains(responseLog, "invalid_encrypted_content") {
				t.Fatalf("the refused attempt's answer is missing from the log:\n%s", responseLog)
			}
		})
	}
}

func TestCodexDoesNotRetryOtherFailures(t *testing.T) {
	withCipher := responsesWithInput(reasoningItem(openAICipherA))
	cases := []struct {
		name    string
		payload string
		status  int
		body    string
	}{
		{"another bad request", withCipher, http.StatusBadRequest, `{"error":{"message":"bad prompt","type":"invalid_request_error","code":"invalid_prompt"}}`},
		{"bad request without a code", withCipher, http.StatusBadRequest, `plain text failure`},
		{"nothing to strip", responsesWithInput(), http.StatusBadRequest, codexInvalidCiphertextBodyJSON},
		{"reasoning without ciphertext", responsesWithInput(`{"type":"reasoning","id":"rs_1","summary":[]}`), http.StatusBadRequest, codexInvalidCiphertextBodyJSON},
		{"only 400 is retried: 401", withCipher, http.StatusUnauthorized, codexInvalidCiphertextBodyJSON},
		{"only 400 is retried: 429", withCipher, http.StatusTooManyRequests, codexInvalidCiphertextBodyJSON},
		{"only 400 is retried: 503", withCipher, http.StatusServiceUnavailable, codexInvalidCiphertextBodyJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			env.Upstream.respond = func(int, codexWireCall) *http.Response {
				return codexErrorResponse(tc.status, tc.body)
			}
			_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: tc.payload})
			if got := len(env.Upstream.Calls()); got != 1 {
				t.Fatalf("upstream saw %d requests, want exactly 1", got)
			}
			status, ok := err.(interface{ StatusCode() int })
			if err == nil || !ok || status.StatusCode() != tc.status {
				t.Fatalf("error = %v, want an upstream status error %d", err, tc.status)
			}
			if !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("the upstream's answer was altered on its way to the client: %q", err.Error())
			}
		})
	}
}

func TestCodexRetriesAtMostOnce(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	env.Upstream.respond = func(int, codexWireCall) *http.Response {
		return codexErrorResponse(http.StatusBadRequest, codexInvalidCiphertextBodyJSON)
	}
	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: responsesWithInput(reasoningItem(openAICipherA))})
	if got := len(env.Upstream.Calls()); got != 2 {
		t.Fatalf("upstream saw %d requests, want the original and exactly one retry", got)
	}
	status, ok := err.(interface{ StatusCode() int })
	if err == nil || !ok || status.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "invalid_encrypted_content") {
		t.Fatalf("the second refusal should reach the client unchanged, got %v", err)
	}
}

// A 400 is read ahead to look for the error code; whatever is read has to be put
// back, for every size of body.
func TestCodexRefusedResponseBodyIsDeliveredWhole(t *testing.T) {
	big := `{"error":{"code":"invalid_prompt","message":"` + strings.Repeat("x", 200<<10) + `"}}`
	small := `{"error":{"code":"invalid_prompt"}}`
	for name, body := range map[string]string{"small": small, "larger than the read-ahead": big, "empty": ""} {
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			env.Upstream.respond = func(int, codexWireCall) *http.Response {
				return codexErrorResponse(http.StatusBadRequest, body)
			}
			auth := codexOAuthAuth()
			req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(codexResponsesPayload)}
			execCtx := newExecutionContext(env.Ctx, "codex", env.Exec.cfg, auth, req, cliproxyexecutor.Options{}, ExecutionOptions{})
			httpReq, err := http.NewRequestWithContext(env.Ctx, http.MethodPost, codexResponsesURL, bytes.NewReader([]byte(responsesWithInput(reasoningItem(openAICipherA)))))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := env.Exec.doCodexResponses(execCtx, execCtx.Recorder(), httpReq, nil) //nolint:bodyclose // closed below.
			if err != nil {
				t.Fatalf("doCodexResponses: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading the refused body: %v", err)
			}
			if string(got) != body {
				t.Fatalf("body delivered to the caller differs from the upstream's (%d vs %d bytes)", len(got), len(body))
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d", resp.StatusCode)
			}
		})
	}
}

// Image requests are ordinary POST /responses calls and go out like any other.
func TestCodexImageGenerationRequestIsCompressedForChatGPTAccounts(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+XgnUAAAAASUVORK5CYII="
	env := newCodexExecEnv(t, nil)
	env.Upstream.respond = func(int, codexWireCall) *http.Response {
		sse := "data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1710000002,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + png + "\",\"revised_prompt\":\"a fox\"}]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
	}
	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai", Payload: `{"model":"gpt-image-2","prompt":"draw a fox"}`, Alt: "images/generations"})
	if err != nil {
		t.Fatalf("image request failed: %v", err)
	}
	calls := env.Upstream.Calls()
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(calls))
	}
	if calls[0].Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("Content-Encoding = %q, want zstd", calls[0].Header.Get("Content-Encoding"))
	}
	if got := gjson.GetBytes(calls[0].Plain(t), "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("decoded image request lost its tool: %s", calls[0].Plain(t))
	}
}
