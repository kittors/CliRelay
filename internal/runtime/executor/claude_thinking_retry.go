package executor

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude Opus 4.7 and later reject the budget form of thinking, and some of them
// also reject thinking.type "disabled". The registry records which models those
// are and the thinking applier writes the right shape for them. This file is the
// safety net for the ones it cannot know about: configured names and aliases
// that resolve to nothing in the catalog, and models released after this build.
// The 400 says exactly what the upstream wants, so the request is rewritten the
// way the error asks and sent once more.
//
// Anthropic's wording, which is matched literally:
//
//	"thinking.type.enabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort" to control thinking behavior.
//	"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort" to control thinking behavior.
//
// Sonnet 5.5 words the second one differently and names its off switch, and
// models that turn thinking off only up to effort "high" say so:
//
//	"thinking.type.disabled" is not supported for this model. Use "thinking.type.between_tools" for the lowest thinking setting, or ...
//	output_config.effort 'xhigh' is not supported when thinking is disabled on this model. Use effort 'high' or below, or enable thinking.
const (
	claudeRejectsEnabledThinking  = `"thinking.type.enabled" is not supported`
	claudeRejectsDisabledThinking = `"thinking.type.disabled" is not supported`
	claudeSuggestsBetweenTools    = `"thinking.type.between_tools"`
	claudeRejectsEffortWhenOff    = `is not supported when thinking is disabled`
)

// claudeMessagesResult is the outcome of posting to /v1/messages.
type claudeMessagesResult struct {
	// resp is the response of the last attempt. On a non-2xx status its body
	// has already been read into errBody and closed.
	resp    *http.Response
	errBody []byte
	// body is the request body of the last attempt, before the tool prefix.
	// It differs from the caller's body when the thinking retry rewrote it.
	body []byte
}

func (r claudeMessagesResult) failed() bool {
	return r.resp.StatusCode < http.StatusOK || r.resp.StatusCode >= http.StatusMultipleChoices
}

// sendClaudeMessages posts body with the method, URL and headers of tmpl. When
// the upstream rejects the request's thinking shape, it rewrites the request
// once (see rewriteRejectedClaudeThinking) and returns that attempt instead.
// toUpstream turns the body into the bytes put on the wire (the OAuth tool
// prefix); every attempt is recorded in the request log.
func (e *ClaudeExecutor) sendClaudeMessages(execCtx *ExecutionContext, tmpl *http.Request, body []byte, toUpstream func([]byte) []byte) (claudeMessagesResult, error) {
	result, err := e.postClaudeMessages(execCtx, tmpl, body, toUpstream)
	if err != nil || !result.failed() {
		return result, err
	}
	retryBody, ok := rewriteRejectedClaudeThinking(body, execCtx.RequestedThinkingLevel(), result.resp.StatusCode, result.errBody)
	if !ok {
		return result, nil
	}
	logWithRequestID(execCtx.Context).Debugf("claude executor: upstream rejected the thinking shape for model %s, retrying once with adaptive thinking", execCtx.BaseModel)
	return e.postClaudeMessages(execCtx, tmpl, retryBody, toUpstream)
}

func (e *ClaudeExecutor) postClaudeMessages(execCtx *ExecutionContext, tmpl *http.Request, body []byte, toUpstream func([]byte) []byte) (claudeMessagesResult, error) {
	upstreamBody := toUpstream(body)
	httpReq := tmpl.Clone(execCtx.Context)
	httpReq.Body = io.NopCloser(bytes.NewReader(upstreamBody))
	httpReq.ContentLength = int64(len(upstreamBody))
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(upstreamBody)), nil
	}
	recorder := execCtx.Recorder()
	recorder.RecordRequest(httpReq.URL.String(), httpReq.Method, httpReq.Header.Clone(), upstreamBody)

	httpResp, err := execCtx.HTTPClient(0).Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		return claudeMessagesResult{body: body}, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	result := claudeMessagesResult{resp: httpResp, body: body}
	if result.failed() {
		result.errBody = readDecodedUpstreamErrorBody(e.Identifier(), httpResp.Header, httpResp.Body)
		recorder.AppendResponseChunk(result.errBody)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}
	return result, nil
}

// rewriteRejectedClaudeThinking rewrites body the way a thinking-shape 400 asks
// for. requested is the level the client asked for (RequestedThinkingLevel);
// the budget in body may already be capped below max_tokens, so it is only the
// fallback for the effort. ok is false when the error is something else or the
// body does not carry the rejected shape, so there is nothing a retry could
// change.
func rewriteRejectedClaudeThinking(body []byte, requested string, status int, errBody []byte) ([]byte, bool) {
	if status != http.StatusBadRequest {
		return nil, false
	}
	// The message sits inside a JSON string, so its quotes arrive escaped.
	message := strings.ReplaceAll(string(errBody), `\"`, `"`)
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	hasEffort := gjson.GetBytes(body, "output_config.effort").Exists()

	switch {
	case thinkingType == "enabled" && strings.Contains(message, claudeRejectsEnabledThinking):
		effort := ""
		if !hasEffort {
			effort = claudeEffortForRequest(requested)
			if budget := gjson.GetBytes(body, "thinking.budget_tokens"); effort == "" && budget.Exists() {
				effort = claudeEffortForRequest(budget.String())
			}
		}
		display := gjson.GetBytes(body, "thinking.display").String()
		if display == "" {
			// Same reason as the applier: the models that reject budgets
			// default to "omitted", which streams empty thinking blocks.
			display = "summarized"
		}
		out, _ := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"adaptive"}`))
		out, _ = sjson.SetBytes(out, "thinking.display", display)
		if effort != "" {
			out, _ = sjson.SetBytes(out, "output_config.effort", effort)
		}
		return out, true

	case thinkingType == "disabled" && strings.Contains(message, claudeRejectsDisabledThinking):
		if strings.Contains(message, claudeSuggestsBetweenTools) {
			out, _ := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"between_tools"}`))
			// between_tools is accepted only at effort "high" or below.
			return capClaudeEffortAtHigh(out), true
		}
		// The model cannot turn thinking off. Omitting thinking at the lowest
		// effort is the closest thing, unless the request chose an effort.
		out, _ := sjson.DeleteBytes(body, "thinking")
		if !hasEffort {
			out, _ = sjson.SetBytes(out, "output_config.effort", string(thinking.LevelLow))
		}
		return out, true

	case (thinkingType == "disabled" || thinkingType == "between_tools") && strings.Contains(message, claudeRejectsEffortWhenOff):
		if effort := gjson.GetBytes(body, "output_config.effort").String(); !thinking.LevelAbove(thinking.ThinkingLevel(effort), thinking.LevelHigh) {
			return nil, false
		}
		return capClaudeEffortAtHigh(body), true
	}
	return nil, false
}

// capClaudeEffortAtHigh lowers output_config.effort to "high" when it is above.
func capClaudeEffortAtHigh(body []byte) []byte {
	effort := gjson.GetBytes(body, "output_config.effort").String()
	if !thinking.LevelAbove(thinking.ThinkingLevel(effort), thinking.LevelHigh) {
		return body
	}
	out, _ := sjson.SetBytes(body, "output_config.effort", string(thinking.LevelHigh))
	return out
}

// claudeEffortForRequest reads a requested level or numeric budget as a Claude
// effort. Values that carry no effort ("none", "auto", unknown words) give "".
func claudeEffortForRequest(requested string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if budget, err := strconv.Atoi(requested); err == nil {
		level, ok := thinking.ConvertBudgetToLevel(budget)
		if !ok {
			return ""
		}
		requested = level
	}
	if level, ok := thinking.ParseLevelSuffix(requested); ok {
		return claudeEffortForLevel(string(level))
	}
	return ""
}

// claudeEffortForLevel maps a budget-derived level onto the Claude effort
// vocabulary, low through max. "none" and "auto" carry no effort.
func claudeEffortForLevel(level string) string {
	switch thinking.ThinkingLevel(level) {
	case thinking.LevelNone, thinking.LevelAuto, "":
		return ""
	case thinking.LevelMinimal:
		return string(thinking.LevelLow)
	default:
		return level
	}
}
