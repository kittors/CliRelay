package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *OpenCodeGoExecutor) executeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	apiKey := opencodeGoAPIKey(auth)
	if strings.TrimSpace(apiKey) == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "missing OpenCode Go API key"}
	}
	to := sdktranslator.FormatClaude
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: opts.SourceFormat != to,
	})

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated, _ = sjson.SetBytes(translated, "model", execCtx.BaseModel)

	translated, err = thinking.ApplyThinking(
		translated,
		req.Model,
		execCtx.SourceFormat.String(),
		execCtx.Execution.TargetFormat.String(),
		e.Identifier(),
	)
	if err != nil {
		return resp, err
	}
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	url := strings.TrimSuffix(opencodeGoBaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	e.applyMessagesHeaders(httpReq, auth, apiKey, false)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode go executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publishWithContent(execCtx.Context, parseClaudeUsage(data), string(req.Payload), string(data))
	reporter.ensurePublished(execCtx.Context)

	bodyForTranslation := data
	if execCtx.SourceFormat != execCtx.Execution.TargetFormat {
		bodyForTranslation = opencodeGoClaudeMessageToSSE(data)
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		execCtx.Context,
		execCtx.Execution.TargetFormat,
		execCtx.SourceFormat,
		req.Model,
		opts.OriginalRequest,
		translated,
		bodyForTranslation,
		&param,
	)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func (e *OpenCodeGoExecutor) executeMessagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	apiKey := opencodeGoAPIKey(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing OpenCode Go API key"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FormatClaude,
		TranslateAsStream: true,
	})

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated, _ = sjson.SetBytes(translated, "model", execCtx.BaseModel)

	translated, err = thinking.ApplyThinking(
		translated,
		req.Model,
		execCtx.SourceFormat.String(),
		execCtx.Execution.TargetFormat.String(),
		e.Identifier(),
	)
	if err != nil {
		return nil, err
	}
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	url := strings.TrimSuffix(opencodeGoBaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	e.applyMessagesHeaders(httpReq, auth, apiKey, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode go executor: close response body error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("opencode go executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			if execCtx.SourceFormat == execCtx.Execution.TargetFormat {
				cloned := append(bytes.Clone(line), '\n')
				out <- cliproxyexecutor.StreamChunk{Payload: cloned}
				continue
			}
			chunks := sdktranslator.TranslateStream(
				execCtx.Context,
				execCtx.Execution.TargetFormat,
				execCtx.SourceFormat,
				req.Model,
				opts.OriginalRequest,
				translated,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		reporter.ensurePublished(execCtx.Context)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenCodeGoExecutor) applyMessagesHeaders(req *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", "cli-proxy-opencode-go")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

func opencodeGoStripOrphanedToolCalls(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	arrayName := "messages"
	msgs := gjson.GetBytes(payload, "messages")
	if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
		msgs = gjson.GetBytes(payload, "input")
		if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
			return payload
		}
		arrayName = "input"
	}

	items := msgs.Array()
	needsStrip := make([]int, 0)

	for i, item := range items {
		if item.Get("role").String() != "assistant" {
			continue
		}
		tc := item.Get("tool_calls")
		if !tc.Exists() || !tc.IsArray() {
			continue
		}

		resolved := make(map[string]bool)
		for _, later := range items[i+1:] {
			if later.Get("role").String() == "tool" {
				if id := later.Get("tool_call_id").String(); id != "" {
					resolved[id] = true
				}
			}
		}

		allResolved := true
		for _, call := range tc.Array() {
			if id := call.Get("id").String(); id != "" && !resolved[id] {
				allResolved = false
				break
			}
		}
		if !allResolved {
			needsStrip = append(needsStrip, i)
		}
	}

	if len(needsStrip) == 0 {
		return payload
	}

	for i := len(needsStrip) - 1; i >= 0; i-- {
		path := fmt.Sprintf("%s.%d.tool_calls", arrayName, needsStrip[i])
		payload, _ = sjson.DeleteBytes(payload, path)
	}
	return payload
}

func opencodeGoUsesMessages(model string) bool {
	base := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	_, ok := opencodeGoMessagesModels[base]
	return ok
}

func opencodeGoAPIKey(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["api_key"])
}

func opencodeGoAuthWithBaseURL(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	if auth == nil {
		return &cliproxyauth.Auth{Attributes: map[string]string{"base_url": opencodeGoBaseURL}}
	}
	clone := *auth
	attrs := make(map[string]string, len(auth.Attributes)+1)
	for k, v := range auth.Attributes {
		attrs[k] = v
	}
	attrs["base_url"] = strings.TrimSuffix(opencodeGoBaseURL, "/")
	clone.Attributes = attrs
	return &clone
}

// opencodeGoFixToolCallArguments repairs tool_calls where function.arguments contains
// concatenated JSON objects by splitting them into separate tool_call entries.
// This handles a Codex Desktop client bug where multiple shell_command calls get
// merged into a single tool_call's arguments field (e.g., {"c":"ls"}{"c":"cat pkg.json"}).
func opencodeGoFixToolCallArguments(payload []byte) []byte {
	if len(payload) == 0 || !json.Valid(payload) {
		return payload
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}
	changed := false
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(role, "assistant") {
			continue
		}
		rawCalls, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		calls, ok := rawCalls.([]any)
		if !ok {
			continue
		}
		fixed := make([]any, 0, len(calls))
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				fixed = append(fixed, rawCall)
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				fixed = append(fixed, rawCall)
				continue
			}
			args, ok := fn["arguments"].(string)
			if !ok {
				fixed = append(fixed, rawCall)
				continue
			}
			if json.Valid([]byte(args)) {
				fixed = append(fixed, rawCall)
				continue
			}
			parts := splitConcatenatedJSONObjects(args)
			if len(parts) <= 1 {
				fixed = append(fixed, rawCall)
				continue
			}
			changed = true
			fn["arguments"] = parts[0]
			fixed = append(fixed, call)
			baseID, _ := call["id"].(string)
			for i := 1; i < len(parts); i++ {
				newCall := cloneToolCallMap(call, baseID, i)
				if newFn, ok := newCall["function"].(map[string]any); ok {
					newFn["arguments"] = parts[i]
				}
				fixed = append(fixed, newCall)
			}
		}
		if changed {
			msg["tool_calls"] = fixed
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return out
}

func splitConcatenatedJSONObjects(input string) []string {
	decoder := json.NewDecoder(strings.NewReader(input))
	var parts []string
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			break
		}
		parts = append(parts, string(raw))
	}
	return parts
}

func cloneToolCallMap(original map[string]any, baseID string, splitIdx int) map[string]any {
	clone := make(map[string]any, len(original))
	for k, v := range original {
		clone[k] = v
	}
	if baseID != "" {
		clone["id"] = fmt.Sprintf("%s_split_%d", baseID, splitIdx)
	}
	if fn, ok := original["function"].(map[string]any); ok {
		fnClone := make(map[string]any, len(fn))
		for k, v := range fn {
			fnClone[k] = v
		}
		clone["function"] = fnClone
	}
	return clone
}

func opencodeGoClaudeMessageToSSE(data []byte) []byte {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return data
	}
	var b strings.Builder
	writeData := func(raw string) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		b.WriteString("data: ")
		b.WriteString(raw)
		b.WriteString("\n\n")
	}

	messageStart := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
	messageStart, _ = sjson.Set(messageStart, "message.id", root.Get("id").String())
	messageStart, _ = sjson.Set(messageStart, "message.model", root.Get("model").String())
	if v := root.Get("usage.input_tokens"); v.Exists() {
		messageStart, _ = sjson.Set(messageStart, "message.usage.input_tokens", v.Int())
	}
	writeData(messageStart)

	index := 0
	if content := root.Get("content"); content.Exists() && content.IsArray() {
		for _, block := range content.Array() {
			blockType := block.Get("type").String()
			switch blockType {
			case "text":
				start := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
				start, _ = sjson.Set(start, "index", index)
				writeData(start)
				delta := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
				delta, _ = sjson.Set(delta, "index", index)
				delta, _ = sjson.Set(delta, "delta.text", block.Get("text").String())
				writeData(delta)
			case "thinking":
				start := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
				start, _ = sjson.Set(start, "index", index)
				writeData(start)
				delta := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
				delta, _ = sjson.Set(delta, "index", index)
				delta, _ = sjson.Set(delta, "delta.thinking", block.Get("thinking").String())
				writeData(delta)
			case "tool_use":
				start := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
				start, _ = sjson.Set(start, "index", index)
				start, _ = sjson.Set(start, "content_block.id", block.Get("id").String())
				start, _ = sjson.Set(start, "content_block.name", block.Get("name").String())
				if input := block.Get("input"); input.Exists() {
					start, _ = sjson.SetRaw(start, "content_block.input", input.Raw)
				}
				writeData(start)
			default:
				index++
				continue
			}
			stop := `{"type":"content_block_stop","index":0}`
			stop, _ = sjson.Set(stop, "index", index)
			writeData(stop)
			index++
		}
	}

	messageDelta := `{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":0}}`
	if v := root.Get("stop_reason"); v.Exists() {
		messageDelta, _ = sjson.Set(messageDelta, "delta.stop_reason", v.String())
	}
	if v := root.Get("stop_sequence"); v.Exists() && v.Type != gjson.Null {
		messageDelta, _ = sjson.Set(messageDelta, "delta.stop_sequence", v.String())
	}
	if v := root.Get("usage.output_tokens"); v.Exists() {
		messageDelta, _ = sjson.Set(messageDelta, "usage.output_tokens", v.Int())
	}
	writeData(messageDelta)
	writeData(`{"type":"message_stop"}`)
	return []byte(b.String())
}
