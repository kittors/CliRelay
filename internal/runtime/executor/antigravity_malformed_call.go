package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Gemini sometimes plans a tool call, fails to emit it, and ends the turn with
//
//	"finishReason": "MALFORMED_FUNCTION_CALL",
//	"finishMessage": "Malformed function call: ... Function call is empty - no input to parse."
//
// By then the only thing streamed is the thinking. Translators read any finish
// reason as a normal end of turn, so the client receives a successful response
// whose only output is reasoning: an agent harness records stopReason "stop" and
// waits for the user, which reads as the model stopping halfway through a thought.
//
// Measured against gemini-3.8-flash-high by replaying a real agent session
// (20k-token context, last item a tool result): 3-8 of every 12-24 identical
// requests ended this way, the rest produced the tool call. The draw is
// independent per request, so asking again is the fix; the client never saw a
// terminal event, so the retry is invisible to it.
const antigravityMalformedCallRetries = 2

const geminiFinishMalformedFunctionCall = "MALFORMED_FUNCTION_CALL"

// geminiMalformedCallGuard tracks one upstream response. A retry is only safe
// while nothing but thinking has been forwarded: once text or a function call
// has gone out, a second answer would duplicate or contradict it.
type geminiMalformedCallGuard struct {
	visible bool
}

func geminiFirstCandidate(payload []byte) gjson.Result {
	if candidate := gjson.GetBytes(payload, "response.candidates.0"); candidate.Exists() {
		return candidate
	}
	return gjson.GetBytes(payload, "candidates.0")
}

// observe records visible output in payload and reports whether payload ends
// the turn with MALFORMED_FUNCTION_CALL before anything visible was produced.
func (g *geminiMalformedCallGuard) observe(payload []byte) bool {
	candidate := geminiFirstCandidate(payload)
	if !candidate.Exists() {
		return false
	}
	candidate.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
		switch {
		case part.Get("functionCall").Exists(),
			part.Get("inlineData").Exists(),
			part.Get("inline_data").Exists(),
			!part.Get("thought").Bool() && part.Get("text").String() != "":
			g.visible = true
			return false
		}
		return true
	})
	if g.visible {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(candidate.Get("finishReason").String()), geminiFinishMalformedFunctionCall)
}

// stripGeminiTerminal removes the end-of-turn markers from a chunk, so the
// translator forwards its thinking (if any) but keeps the response open for the
// retried answer. Usage is dropped with it: translators emit usage only on the
// terminal chunk, and the retry carries its own.
func stripGeminiTerminal(payload []byte) []byte {
	out := payload
	for _, prefix := range []string{"response.", ""} {
		for _, field := range []string{"candidates.0.finishReason", "candidates.0.finishMessage", "usageMetadata", "cpaUsageMetadata"} {
			out, _ = sjson.DeleteBytes(out, prefix+field)
		}
	}
	return out
}

// geminiTerminalOnly is the inverse of stripGeminiTerminal: the chunk without
// its thinking, used to end a response whose thinking was already forwarded.
// The parts become a single empty text part, the shape upstream itself sends on
// a plain STOP, which every translator terminates on.
func geminiTerminalOnly(payload []byte) []byte {
	out := payload
	for _, prefix := range []string{"response.", ""} {
		if gjson.GetBytes(out, prefix+"candidates.0").Exists() {
			out, _ = sjson.SetRawBytes(out, prefix+"candidates.0.content.parts", []byte(`[{"text":""}]`))
		}
	}
	return out
}

// antigravityReopener re-issues the request a response was opened with. It walks
// the base URL fallback order once; anything other than a 2xx is an error, since
// the caller is already mid-stream and can no longer run the full retry ladder.
type antigravityReopener struct {
	executor *AntigravityExecutor
	ctx      context.Context
	auth     *cliproxyauth.Auth
	token    string
	model    string
	alt      string
	payload  []byte
	baseURLs []string
	client   *http.Client
	recorder UpstreamRecorder
}

func (r antigravityReopener) open() (*http.Response, error) {
	var lastErr error
	for _, baseURL := range r.baseURLs {
		httpReq, requestBody, errReq := r.executor.buildRequest(r.ctx, r.auth, r.token, r.model, r.payload, true, r.alt, baseURL)
		if errReq != nil {
			return nil, errReq
		}
		r.recorder.RecordRequest(httpReq.URL.String(), httpReq.Method, httpReq.Header.Clone(), requestBody)
		httpResp, errDo := r.client.Do(httpReq)
		if errDo != nil {
			r.recorder.RecordResponseError(errDo)
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return nil, errDo
			}
			lastErr = errDo
			continue
		}
		r.recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			body := readUpstreamErrorBody(r.executor.Identifier(), httpResp.Body)
			_ = httpResp.Body.Close()
			r.recorder.AppendResponseChunk(body)
			lastErr = statusErr{code: httpResp.StatusCode, msg: string(body)}
			continue
		}
		return httpResp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("antigravity executor: no base url available")
	}
	return nil, lastErr
}
