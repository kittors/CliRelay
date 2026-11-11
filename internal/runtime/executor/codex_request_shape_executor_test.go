package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

const codexBareBadRequestJSON = `{"detail":"Bad Request"}`

// traceRequest gives the env what the gateway gives an executor in production: a
// client request with its headers, a request diagnostic carrying a route trace,
// and one attempt on it, whose context the executor runs under.
func (e *codexExecEnv) traceRequest(t *testing.T, requestID string, clientHeaders map[string]string) (*routetrace.Recorder, context.Context) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	for key, value := range clientHeaders {
		req.Header.Set(key, value)
	}
	e.Gin.Request = req
	diagnostics.EnsureGin(e.Gin, requestID)
	rec := routetrace.FromContext(e.Gin.Request.Context())
	if rec == nil {
		t.Fatal("the request diagnostic carries no route trace")
	}
	t.Cleanup(rec.Close)
	base := e.Ctx
	e.beginAttempt(rec, base, "codex-oauth")
	return rec, base
}

// beginAttempt starts the next attempt of the trace, as the scheduler does before
// it hands a request to an account's executor.
func (e *codexExecEnv) beginAttempt(rec *routetrace.Recorder, base context.Context, authID string) {
	attempt := rec.BeginAttempt(routetrace.AttemptInfo{AuthID: authID, Provider: "codex", Model: "gpt-5.4"})
	e.Ctx = attempt.Context(base)
}

// shapeOfStep returns the shape the trace holds for step i, which does not exist
// when none was recorded.
func shapeOfStep(t *testing.T, rec *routetrace.Recorder, i int) gjson.Result {
	t.Helper()
	trace := rec.Snapshot()
	if trace == nil || len(trace.Steps) <= i {
		t.Fatalf("the trace has no step %d: %+v", i, trace)
	}
	raw := trace.Steps[i].UpstreamRequestShape
	if len(raw) == 0 {
		return gjson.Result{}
	}
	if !json.Valid(raw) || len(raw) > codexShapeBudgetBytes {
		t.Fatalf("recorded shape is %d bytes, valid=%v: %s", len(raw), json.Valid(raw), raw)
	}
	return gjson.ParseBytes(raw)
}

// upstreamAnswer is one answer of the fake upstream. A zero status is the default
// completed response. Answers are built afresh for each call because a response
// body can be read once.
type upstreamAnswer struct {
	status int
	body   string
}

// respondWith makes call n answer answers[n], and every call past the end repeat
// the last one.
func respondWith(env *codexExecEnv, answers ...upstreamAnswer) {
	env.Upstream.respond = func(n int, _ codexWireCall) *http.Response {
		answer := answers[min(n, len(answers)-1)]
		if answer.status == 0 {
			return nil
		}
		return codexErrorResponse(answer.status, answer.body)
	}
}

var bareBadRequest = upstreamAnswer{http.StatusBadRequest, codexBareBadRequestJSON}

// withEffort sets a reasoning effort on a Responses payload. A request that sets
// none gets the hosted image tool from the bridge, which is an edit of its own.
func withEffort(payload string) string {
	return strings.Replace(payload, `"input":`, `"reasoning":{"effort":"low"},"input":`, 1)
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	status, ok := err.(interface{ StatusCode() int })
	if err == nil || !ok {
		t.Fatalf("error = %v, want an upstream status error", err)
	}
	return status.StatusCode()
}

func TestBareBadRequestRecordsTheShapeOfWhatWasSent(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	rec, _ := env.traceRequest(t, "req-shape-1", map[string]string{
		"X-Codex-Turn-Metadata": `{"request_kind":"compaction","window_number":1,"history_initialization":"cold_resume","turn_trigger":"automation_heartbeat_scheduled"}`,
	})
	respondWith(env, bareBadRequest)

	payload := withEffort(responsesWithInput(reasoningItem(openAICipherA), `{"type":"function_call","call_id":"call_SECRET_ID","name":"shell","arguments":"{\"cmd\":\"SECRET-ARGUMENT\"}"}`))
	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload})

	// What the client is told does not change: the refusal reaches it as it came.
	if statusOf(t, err) != http.StatusBadRequest || !strings.Contains(err.Error(), codexBareBadRequestJSON) {
		t.Fatalf("the client's error = %v", err)
	}
	calls := env.Upstream.Calls()
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d requests, want 1: a bare 400 is not retried", len(calls))
	}

	shape := shapeOfStep(t, rec, 0)
	if !shape.Exists() {
		t.Fatal("no shape was recorded for the refused attempt")
	}
	plain := calls[0].Plain(t)
	if got := shape.Get("bytes.plain").Int(); got != int64(len(plain)) {
		t.Fatalf("bytes.plain = %d, want the %d bytes that went out", got, len(plain))
	}
	if got := shape.Get("bytes.zstd").Int(); got != int64(len(calls[0].Wire)) || got == 0 {
		t.Fatalf("bytes.zstd = %d, want the %d bytes on the wire", got, len(calls[0].Wire))
	}
	if got := shape.Get("sent_headers.content_encoding").String(); got != "zstd" {
		t.Fatalf("content_encoding = %q", got)
	}
	// The shape describes the body as sent, not as the client wrote it.
	if got := shape.Get("input.by_type.reasoning").Int(); got != 1 {
		t.Fatalf("reasoning items = %d, want 1", got)
	}
	if got := shape.Get("ciphertext.reasoning.n").Int(); got != 1 {
		t.Fatalf("reasoning ciphertexts = %d, want 1", got)
	}
	if got := shape.Get("input.by_type.function_call").Int(); got != 1 {
		t.Fatalf("function calls = %d, want 1", got)
	}
	if shape.Get("client.request_kind").String() != "compaction" || shape.Get("client.window_number").Int() != 1 ||
		shape.Get("client.history_initialization").String() != "cold_resume" || shape.Get("client.turn_trigger").String() != "automation_heartbeat_scheduled" {
		t.Fatalf("client metadata missing from the shape: %s", shape.Get("client").Raw)
	}
	// The replayed reasoning item came with a server id, which the gateway strips
	// when store is off; that is the only rewrite, and it is the only one reported.
	if got := shape.Get("mutations").Raw; got != `{"ids_stripped":1}` {
		t.Fatalf("mutations = %s, want just the one id stripped", got)
	}
	assertNoSecrets(t, shape.Raw)
	for _, secret := range []string{openAICipherA, "first", "answer", "next"} {
		if strings.Contains(shape.Raw, secret) {
			t.Fatalf("the shape repeats %q: %s", secret, shape.Raw)
		}
	}
}

func TestBareBadRequestShapeFollowsEveryExecutorEntryPoint(t *testing.T) {
	for name, call := range map[string]codexCall{
		"non-streaming": {Payload: responsesWithInput()},
		"streaming":     {Payload: responsesWithInput(), Stream: true},
		"compact":       {Payload: responsesWithInput(), Alt: "responses/compact"},
		"chat":          {From: "openai", Payload: `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			rec, _ := env.traceRequest(t, "req-"+name, nil)
			respondWith(env, bareBadRequest)
			call.Auth = codexOAuthAuth()
			if call.From == "" {
				call.From = "openai-response"
			}
			_, err := env.Do(t, call)
			if statusOf(t, err) != http.StatusBadRequest {
				t.Fatalf("error = %v", err)
			}
			calls := env.Upstream.Calls()
			shape := shapeOfStep(t, rec, 0)
			if len(calls) != 1 || !shape.Exists() {
				t.Fatalf("calls = %d, shape = %v", len(calls), shape.Exists())
			}
			if got := shape.Get("bytes.plain").Int(); got != int64(len(calls[0].Plain(t))) {
				t.Fatalf("bytes.plain = %d, want %d", got, len(calls[0].Plain(t)))
			}
			// /responses/compact is deliberately sent uncompressed.
			if (name == "compact") == shape.Get("bytes.zstd").Exists() {
				t.Fatalf("bytes.zstd = %s for %s", shape.Get("bytes.zstd").Raw, name)
			}
		})
	}
}

// A 400 that names its error is not what the shape is for: the answer already
// says what is wrong, and invalid_encrypted_content has its own retry.
func TestBadRequestsThatNameACodeRecordNoShape(t *testing.T) {
	withReasoning := responsesWithInput(reasoningItem(openAICipherA))
	coded := upstreamAnswer{http.StatusBadRequest, codexInvalidCiphertextBodyJSON}
	cases := map[string]struct {
		payload string
		answers []upstreamAnswer
		calls   int
	}{
		"another coded 400":           {withReasoning, []upstreamAnswer{{http.StatusBadRequest, `{"error":{"message":"bad prompt","type":"invalid_request_error","code":"invalid_prompt"}}`}}, 1},
		"a code in detail":            {withReasoning, []upstreamAnswer{{http.StatusBadRequest, `{"detail":{"code":"model_not_supported","message":"x"}}`}}, 1},
		"nothing to strip":            {responsesWithInput(), []upstreamAnswer{coded}, 1},
		"refused twice with the code": {withReasoning, []upstreamAnswer{coded, coded}, 2},
		"retry succeeds":              {withReasoning, []upstreamAnswer{coded, {}}, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			rec, _ := env.traceRequest(t, "req-coded", nil)
			respondWith(env, tc.answers...)
			_, _ = env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: tc.payload})
			if got := len(env.Upstream.Calls()); got != tc.calls {
				t.Fatalf("upstream saw %d requests, want %d", got, tc.calls)
			}
			if shape := shapeOfStep(t, rec, 0); shape.Exists() {
				t.Fatalf("a shape was recorded: %s", shape.Raw)
			}
		})
	}
}

// Refused for its reasoning, sent again without it, and refused again without a
// reason: the attempt ends with the second answer, so the shape is the second
// body's, marked as the retry.
func TestShapeOfTheReasoningRetryIsTheBodyThatWentOutLast(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	rec, _ := env.traceRequest(t, "req-retry", nil)
	respondWith(env, upstreamAnswer{http.StatusBadRequest, codexInvalidCiphertextBodyJSON}, bareBadRequest)

	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: responsesWithInput(reasoningItem(openAICipherA), reasoningItem(openAICipherB))})
	if statusOf(t, err) != http.StatusBadRequest || !strings.Contains(err.Error(), codexBareBadRequestJSON) {
		t.Fatalf("the client should see the last answer unchanged, got %v", err)
	}
	calls := env.Upstream.Calls()
	if len(calls) != 2 {
		t.Fatalf("upstream saw %d requests, want the original and one retry", len(calls))
	}
	first, second := calls[0].Plain(t), calls[1].Plain(t)
	if len(replayedCiphertexts(first)) != 2 || len(replayedCiphertexts(second)) != 0 {
		t.Fatal("the retry should carry no reasoning")
	}

	shape := shapeOfStep(t, rec, 0)
	if !shape.Exists() {
		t.Fatal("no shape was recorded")
	}
	if got := shape.Get("bytes.plain").Int(); got != int64(len(second)) || got == int64(len(first)) {
		t.Fatalf("bytes.plain = %d, want the retry's %d and not the first body's %d", got, len(second), len(first))
	}
	if got := shape.Get("bytes.zstd").Int(); got != int64(len(calls[1].Wire)) {
		t.Fatalf("bytes.zstd = %d, want the retry's %d", got, len(calls[1].Wire))
	}
	if shape.Get("input.by_type.reasoning").Exists() || shape.Get("ciphertext").Exists() {
		t.Fatalf("the retried body carries no reasoning: %s", shape.Raw)
	}
	if !shape.Get("mutations.reasoning_retry").Bool() {
		t.Fatalf("the shape is not marked as the retry: %s", shape.Raw)
	}
}

func TestAttemptsThatSucceedRecordNoShape(t *testing.T) {
	statuses := map[string]upstreamAnswer{
		"success": {},
		"401":     {http.StatusUnauthorized, codexBareBadRequestJSON},
		"403":     {http.StatusForbidden, codexBareBadRequestJSON},
		"429":     {http.StatusTooManyRequests, codexBareBadRequestJSON},
		"500":     {http.StatusInternalServerError, codexBareBadRequestJSON},
		"503":     {http.StatusServiceUnavailable, codexBareBadRequestJSON},
	}
	for name, answer := range statuses {
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			rec, _ := env.traceRequest(t, "req-ok", nil)
			respondWith(env, answer)
			_, _ = env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: responsesWithInput(reasoningItem(openAICipherA))})
			if shape := shapeOfStep(t, rec, 0); shape.Exists() {
				t.Fatalf("a shape was recorded for %s: %s", name, shape.Raw)
			}
		})
	}
}

// The shape belongs to the ChatGPT backend. An API-key account on the same URL
// talks to a server whose 400s say nothing about this one.
func TestShapeIsOnlyRecordedForChatGPTAccounts(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	rec, _ := env.traceRequest(t, "req-key", nil)
	respondWith(env, bareBadRequest)
	_, err := env.Do(t, codexCall{Auth: codexAPIKeyAuth(), From: "openai-response", Payload: responsesWithInput()})
	if statusOf(t, err) != http.StatusBadRequest {
		t.Fatalf("error = %v", err)
	}
	if shape := shapeOfStep(t, rec, 0); shape.Exists() {
		t.Fatalf("a shape was recorded for an API-key account: %s", shape.Raw)
	}
}

// Without an attempt to put it on, nothing is built, and the request is answered
// exactly as before.
func TestBareBadRequestWithoutATraceChangesNothing(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	respondWith(env, bareBadRequest)
	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: responsesWithInput()})
	if statusOf(t, err) != http.StatusBadRequest || !strings.Contains(err.Error(), codexBareBadRequestJSON) {
		t.Fatalf("error = %v", err)
	}
	if got := len(env.Upstream.Calls()); got != 1 {
		t.Fatalf("upstream saw %d requests, want 1", got)
	}
}

// Each failed attempt keeps its own shape: the same body can fail on one account
// and pass on another, and the headers it went out with differ per account.
func TestEveryAttemptKeepsItsOwnShape(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	rec, base := env.traceRequest(t, "req-two", nil)
	respondWith(env, bareBadRequest)

	firstAuth, secondAuth := codexOAuthAuth(), codexOAuthAuth()
	secondAuth.ID = "codex-oauth-2"
	if _, err := env.Do(t, codexCall{Auth: firstAuth, From: "openai-response", Payload: responsesWithInput()}); statusOf(t, err) != http.StatusBadRequest {
		t.Fatalf("first attempt: %v", err)
	}
	env.beginAttempt(rec, base, secondAuth.ID)
	more := strings.TrimSuffix(strings.Repeat(`{"type":"function_call_output","call_id":"c","output":"x"},`, 5), ",")
	if _, err := env.Do(t, codexCall{Auth: secondAuth, From: "openai-response", Payload: responsesWithInput(more)}); statusOf(t, err) != http.StatusBadRequest {
		t.Fatalf("second attempt: %v", err)
	}

	calls := env.Upstream.Calls()
	if len(calls) != 2 {
		t.Fatalf("upstream saw %d requests", len(calls))
	}
	for i := range calls {
		shape := shapeOfStep(t, rec, i)
		if !shape.Exists() || shape.Get("bytes.plain").Int() != int64(len(calls[i].Plain(t))) {
			t.Fatalf("step %d shape = %s, want the %d bytes of its own body", i, shape.Raw, len(calls[i].Plain(t)))
		}
	}
	if shapeOfStep(t, rec, 0).Get("input.items").Int() == shapeOfStep(t, rec, 1).Get("input.items").Int() {
		t.Fatal("the two attempts sent different bodies and must report different shapes")
	}
}

// What the gateway rewrote is counted where it is rewritten and reported with
// the shape.
func TestShapeReportsWhatTheGatewayRewrote(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	rec, _ := env.traceRequest(t, "req-edits", nil)
	respondWith(env, bareBadRequest)

	payload := `{"model":"gpt-5.4","store":false,"input":[` +
		`{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + claudeSignaure + `"},` +
		`{"type":"function_call","id":"fc_1","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","id":"fco_1","call_id":"c1"},` +
		`{"type":"message","id":"msg_2","role":"user","content":[{"type":"input_text","text":"next"}]}]}`
	if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload}); statusOf(t, err) != http.StatusBadRequest {
		t.Fatalf("error = %v", err)
	}

	sent := env.Upstream.Calls()[0].Plain(t)
	if got := inputTypes(sent); !equalStrings(got, []string{"message", "function_call", "message"}) {
		t.Fatalf("items sent = %v: the shell and the foreign reasoning should be gone", got)
	}
	if strings.Contains(string(sent), `"id"`) {
		t.Fatalf("server ids reached the upstream: %s", sent)
	}
	shape := shapeOfStep(t, rec, 0)
	mutations := shape.Get("mutations")
	// Four items lost an id (the reasoning item among them, before it was dropped
	// for its ciphertext), one reference-only shell went whole.
	if mutations.Get("ids_stripped").Int() != 4 || mutations.Get("shells_dropped").Int() != 1 || mutations.Get("foreign_reasoning_dropped").Int() != 1 {
		t.Fatalf("mutations = %s", mutations.Raw)
	}
	if shape.Get("input.items").Int() != 3 || !shape.Get("request.store").Exists() || shape.Get("request.store").Bool() {
		t.Fatalf("the shape should describe the body that went out: %s", shape.Raw)
	}
}

func TestShapeReportsTheImageBridge(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	rec, _ := env.traceRequest(t, "req-bridge", nil)
	respondWith(env, bareBadRequest)

	payload := `{"model":"gpt-5.4","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"shell"}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"[$imagegen] draw a cat"}]}]}`
	if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload}); statusOf(t, err) != http.StatusBadRequest {
		t.Fatalf("error = %v", err)
	}

	shape := shapeOfStep(t, rec, 0)
	for _, field := range []string{"image_bridge_tool", "tool_choice_forced", "effort_dropped"} {
		if !shape.Get("mutations." + field).Bool() {
			t.Fatalf("mutations.%s missing: %s", field, shape.Raw)
		}
	}
	// The bridge's own rewrite is in what went out, and the shape says so.
	sent := env.Upstream.Calls()[0].Plain(t)
	if gjson.GetBytes(sent, "tool_choice.type").String() != "image_generation" || gjson.GetBytes(sent, "reasoning.effort").Exists() {
		t.Fatalf("the bridge's rewrite is not in the body that went out: %s", sent)
	}
	if shape.Get("request.reasoning_effort").Bool() || shape.Get("request.tool_choice").String() != "image_generation" {
		t.Fatalf("request = %s", shape.Get("request").Raw)
	}
	if shape.Get("tools.by_type.image_generation").Int() != 1 {
		t.Fatalf("tools = %s", shape.Get("tools").Raw)
	}
}

// The request log keeps the shape in detail_content, which stays when bodies are
// stripped, and the error log prints the same diagnostic snapshot.
func TestShapeReachesTheRequestLogWhateverBodyRetentionIs(t *testing.T) {
	setUsageBodyCaptureForTest(t, false)
	plugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 16)}
	cliproxyusage.RegisterPlugin(plugin)

	env := newCodexExecEnv(t, nil)
	rec, _ := env.traceRequest(t, "req-detail-shape", map[string]string{"X-Codex-Turn-Metadata": `{"request_kind":"turn"}`})
	respondWith(env, bareBadRequest)
	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: responsesWithInput(reasoningItem(openAICipherA))})
	if statusOf(t, err) != http.StatusBadRequest {
		t.Fatalf("error = %v", err)
	}

	// The scheduler settles the failed attempt, and the held record goes out with
	// the trace as it is by then.
	rec.Close()
	var detail string
	deadline := time.After(5 * time.Second)
	for detail == "" {
		select {
		case record := <-plugin.records:
			if strings.Contains(record.DetailContent, "req-detail-shape") {
				detail = record.DetailContent
			}
		case <-deadline:
			t.Fatal("the failed attempt's usage record never reached the plugin")
		}
	}

	shape := gjson.Get(detail, "diagnostic.route_trace.steps.0.upstream_request_shape")
	if !shape.Exists() {
		t.Fatalf("detail_content carries no shape:\n%s", detail)
	}
	if shape.Get("input.by_type.reasoning").Int() != 1 || shape.Get("client.request_kind").String() != "turn" {
		t.Fatalf("shape in detail_content = %s", shape.Raw)
	}
	// Bodies are not stored in this configuration; the shape is.
	if gjson.Get(detail, "upstream.request_log").Exists() {
		t.Fatalf("the exchange bodies were stored despite retention being off")
	}
	assertNoSecrets(t, shape.Raw)

	printed, err := diagnostics.FromGin(env.Gin).Snapshot().JSON()
	if err != nil || !strings.Contains(string(printed), `"upstream_request_shape"`) {
		t.Fatalf("the error log's DIAGNOSTIC METADATA does not carry the shape (err %v):\n%s", err, printed)
	}
}
