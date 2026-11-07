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

// claudeMessagesResult is the outcome of posting to /v1/messages.
type claudeMessagesResult struct {
	// resp is the response of the last attempt. On a non-2xx status its body
	// has already been read into errBody and closed.
	resp    *http.Response
	errBody []byte
	// body is the request body of the last attempt, before the tool prefix.
	// It differs from the caller's body when the request rules rewrote it.
	body []byte
}

func (r claudeMessagesResult) failed() bool {
	return r.resp.StatusCode < http.StatusOK || r.resp.StatusCode >= http.StatusMultipleChoices
}

// sendClaudeMessages posts body with the method, URL and headers of tmpl,
// shaped for every rule plan knows for the model. When the upstream answers
// with a 400 that names another rule the request broke, the request is shaped
// for that one too and sent again (see claudeRequestPlan.retry). toUpstream
// turns the body into the bytes put on the wire (the OAuth tool prefix); every
// attempt is recorded in the request log.
func (e *ClaudeExecutor) sendClaudeMessages(execCtx *ExecutionContext, tmpl *http.Request, body []byte, toUpstream func([]byte) []byte, plan *claudeRequestPlan) (claudeMessagesResult, error) {
	body = plan.begin(execCtx, body)
	for {
		result, err := e.postClaudeMessages(execCtx, tmpl, body, toUpstream)
		if err != nil || !result.failed() {
			return result, err
		}
		retryBody, ok := plan.retry(execCtx, result.resp.StatusCode, result.errBody)
		if !ok {
			return result, nil
		}
		body = retryBody
	}
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

// fixClaudeBudgetThinking turns budget thinking into adaptive thinking with an
// effort, for a model that rejects budgets. requested is the level the client
// asked for (RequestedThinkingLevel); the budget in body may already be capped
// below max_tokens, so it is only the fallback for the effort.
func fixClaudeBudgetThinking(body []byte, requested string) ([]byte, bool) {
	if gjson.GetBytes(body, "thinking.type").String() != "enabled" {
		return body, false
	}
	effort := ""
	if !gjson.GetBytes(body, "output_config.effort").Exists() {
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
}

// fixClaudeDisabledThinking rewrites thinking.type "disabled" for a model that
// rejects it. betweenTools is set when the error names "between_tools", the
// model's lowest setting (Sonnet 5.5).
func fixClaudeDisabledThinking(body []byte, betweenTools bool) ([]byte, bool) {
	if gjson.GetBytes(body, "thinking.type").String() != "disabled" {
		return body, false
	}
	if betweenTools {
		out, _ := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"between_tools"}`))
		// between_tools is accepted only at effort "high" or below.
		return capClaudeEffortAt(out, thinking.LevelHigh), true
	}
	// The model cannot turn thinking off. Omitting thinking at the lowest
	// effort is the closest thing, unless the request chose an effort.
	out, _ := sjson.DeleteBytes(body, "thinking")
	if !gjson.GetBytes(body, "output_config.effort").Exists() {
		out, _ = sjson.SetBytes(out, "output_config.effort", string(thinking.LevelLow))
	}
	return out, true
}

// fixClaudeEffortWhenOff lowers an effort above "high" while thinking is off,
// which some models only accept up to "high".
func fixClaudeEffortWhenOff(body []byte) ([]byte, bool) {
	switch gjson.GetBytes(body, "thinking.type").String() {
	case "disabled", "between_tools":
	default:
		return body, false
	}
	effort := gjson.GetBytes(body, "output_config.effort").String()
	if !thinking.LevelAbove(thinking.ThinkingLevel(effort), thinking.LevelHigh) {
		return body, false
	}
	return capClaudeEffortAt(body, thinking.LevelHigh), true
}

// capClaudeEffortAt lowers output_config.effort to ceiling when it is above.
func capClaudeEffortAt(body []byte, ceiling thinking.ThinkingLevel) []byte {
	effort := gjson.GetBytes(body, "output_config.effort").String()
	if !thinking.LevelAbove(thinking.ThinkingLevel(effort), ceiling) {
		return body
	}
	out, _ := sjson.SetBytes(body, "output_config.effort", string(ceiling))
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
