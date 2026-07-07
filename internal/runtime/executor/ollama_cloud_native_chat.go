package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const ollamaCloudNativeKeepAlive = "30m"

func (e *OllamaCloudExecutor) executeNativeChat(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	execCtx, translated, body, cacheKey, promptText, err := e.prepareNativeChat(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	httpResp, err := e.doNativeChatJSON(execCtx, auth, body, false)
	if err != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("ollama cloud native chat: close response body error: %v", errClose)
		}
	}()
	recorder := execCtx.Recorder()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	openAIResponse := ollamaNativeChatToOpenAI(data, execCtx.BaseModel, cacheKey, promptText)
	reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(openAIResponse), string(req.Payload), string(openAIResponse))
	reporter.ensurePublished(execCtx.Context)

	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, sdktranslator.FormatOpenAI, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, openAIResponse, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func (e *OllamaCloudExecutor) executeNativeChatStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	execCtx, translated, body, cacheKey, promptText, err := e.prepareNativeChat(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := e.doNativeChatJSON(execCtx, auth, body, true)
	if err != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder := execCtx.Recorder()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("ollama cloud native chat: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("ollama cloud native chat: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		var lastUsage coreusage.Detail
		hasUsage := false
		roleSent := false
		responseID := fmt.Sprintf("chatcmpl_%x", time.Now().UnixNano())
		var pendingResponsesCompleted []byte
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			recorder.AppendResponseChunk(line)
			openAILines, usage, usageSeen := ollamaNativeStreamChunkToOpenAI(line, execCtx.BaseModel, responseID, cacheKey, promptText, &roleSent)
			if usageSeen {
				lastUsage = usage
				hasUsage = true
			}
			for _, openAILine := range openAILines {
				reporter.appendOutputChunk(openAILine)
				if detail, ok := parseOpenAIStreamUsage(openAILine); ok {
					lastUsage = detail
					hasUsage = true
					if len(pendingResponsesCompleted) > 0 && isOpenAIStreamUsageOnly(openAILine) {
						out <- cliproxyexecutor.StreamChunk{Payload: patchResponsesCompletedUsage(pendingResponsesCompleted, lastUsage)}
						pendingResponsesCompleted = nil
					}
				}
				for _, chunk := range sdktranslator.TranslateStream(execCtx.Context, sdktranslator.FormatOpenAI, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, bytes.Clone(openAILine), &param) {
					if shouldHoldResponsesCompleted(execCtx.SourceFormat, openAILine, []byte(chunk)) {
						pendingResponsesCompleted = []byte(chunk)
						continue
					}
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
				}
			}
		}
		if len(pendingResponsesCompleted) > 0 {
			if hasUsage {
				pendingResponsesCompleted = patchResponsesCompletedUsage(pendingResponsesCompleted, lastUsage)
			}
			out <- cliproxyexecutor.StreamChunk{Payload: pendingResponsesCompleted}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if hasUsage {
			reporter.publish(execCtx.Context, lastUsage)
		}
		reporter.ensurePublished(execCtx.Context)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OllamaCloudExecutor) prepareNativeChat(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*ExecutionContext, []byte, []byte, string, string, error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FormatOpenAI,
		TranslateAsStream: stream,
	})
	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated, _ = sjson.SetBytes(translated, "model", execCtx.BaseModel)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	updated, err := thinking.ApplyThinking(translated, req.Model, execCtx.SourceFormat.String(), sdktranslator.FormatOpenAI.String(), e.Identifier())
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	updated = normalizeOpenAIChatToolCallMessages(updated)
	updated = applyProviderPromptCaching(updated, req.Payload, auth, e.Identifier(), execCtx.BaseModel, sdktranslator.FormatOpenAI, opts)
	body, promptText := ollamaNativeChatRequest(updated, stream)
	cacheKey := ollamaNativeCacheKey(auth, execCtx.BaseModel, req.Payload, updated, opts)
	return execCtx, updated, body, cacheKey, promptText, nil
}

func (e *OllamaCloudExecutor) doNativeChatJSON(execCtx *ExecutionContext, auth *cliproxyauth.Auth, body []byte, stream bool) (*http.Response, error) {
	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}
	url := ollamaCloudNativeBaseURL(baseURL) + "/api/chat"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-ollama-cloud")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if stream {
		httpReq.Header.Set("Accept", "application/x-ndjson")
		httpReq.Header.Set("Cache-Control", "no-cache")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	execCtx.Recorder().RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	resp, err := execCtx.HTTPClient(0).Do(httpReq) //nolint:bodyclose // stream bodies are closed by the stream goroutine.
	if err != nil {
		execCtx.Recorder().RecordResponseError(err)
	}
	return resp, err
}

func ollamaCloudNativeBaseURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = config.DefaultOllamaCloudBaseURL
	}
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return strings.TrimSuffix(base[:len(base)-len("/v1")], "/")
	}
	return base
}

func ollamaNativeChatRequest(openAIChat []byte, stream bool) ([]byte, string) {
	var root map[string]any
	if err := json.Unmarshal(openAIChat, &root); err != nil {
		return openAIChat, ""
	}
	messages, promptText := ollamaNativeMessages(root["messages"])
	out := map[string]any{
		"model":      strings.TrimSpace(fmt.Sprint(root["model"])),
		"messages":   messages,
		"stream":     stream,
		"keep_alive": ollamaCloudNativeKeepAlive,
	}
	if tools, ok := root["tools"]; ok {
		out["tools"] = tools
	}
	if format, ok := ollamaNativeFormat(root["response_format"]); ok {
		out["format"] = format
	}
	if options := ollamaNativeOptions(root); len(options) > 0 {
		out["options"] = options
	}
	body, err := json.Marshal(out)
	if err != nil {
		return openAIChat, promptText
	}
	return body, promptText
}

func ollamaNativeCacheKey(auth *cliproxyauth.Auth, model string, source, translated []byte, opts cliproxyexecutor.Options) string {
	seed := sessionPromptCacheSeed(opts, source)
	if seed == "" {
		seed = sessionPromptCacheSeed(opts, translated)
	}
	if seed == "" {
		seed = strings.TrimSpace(gjson.GetBytes(source, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(gjson.GetBytes(translated, "prompt_cache_key").String())
	}
	return scopedPromptCacheKey(auth, model, seed)
}

func ollamaNativeMessages(raw any) ([]any, string) {
	items, _ := raw.([]any)
	messages := make([]any, 0, len(items))
	var prompt strings.Builder
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(fmt.Sprint(msg["role"]))
		if role == "developer" {
			role = "system"
		}
		content, images := ollamaNativeContent(msg["content"])
		next := map[string]any{"role": role, "content": content}
		if len(images) > 0 {
			next["images"] = images
		}
		if calls, ok := msg["tool_calls"]; ok {
			next["tool_calls"] = calls
		}
		messages = append(messages, next)
		prompt.WriteString(role)
		prompt.WriteByte('\n')
		prompt.WriteString(content)
		prompt.WriteByte('\n')
	}
	return messages, prompt.String()
}

func ollamaNativeContent(raw any) (string, []string) {
	switch value := raw.(type) {
	case string:
		return value, nil
	case []any:
		var text strings.Builder
		var images []string
		for _, item := range value {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(fmt.Sprint(part["type"])) {
			case "text", "input_text", "output_text":
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(fmt.Sprint(part["text"]))
			case "image_url":
				if image := ollamaImageURLPayload(part["image_url"]); image != "" {
					images = append(images, image)
				}
			}
		}
		return text.String(), images
	default:
		if raw == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprint(raw)), nil
	}
}

func ollamaImageURLPayload(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimPrefix(value, "data:image/png;base64,")
	case map[string]any:
		url := strings.TrimSpace(fmt.Sprint(value["url"]))
		if idx := strings.Index(url, ","); strings.HasPrefix(url, "data:") && idx >= 0 {
			return url[idx+1:]
		}
		return url
	default:
		return ""
	}
}

func ollamaNativeFormat(raw any) (any, bool) {
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	if value["type"] == "json_object" {
		return "json", true
	}
	return nil, false
}

func ollamaNativeOptions(root map[string]any) map[string]any {
	options := make(map[string]any)
	if existing, ok := root["options"].(map[string]any); ok {
		for key, value := range existing {
			options[key] = value
		}
	}
	if v, ok := root["temperature"]; ok {
		options["temperature"] = v
	}
	if v, ok := root["top_p"]; ok {
		options["top_p"] = v
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := root[key]; ok {
			options["num_predict"] = v
			break
		}
	}
	return options
}

func ollamaNativeChatToOpenAI(native []byte, model, cacheKey, promptText string) []byte {
	root := gjson.ParseBytes(native)
	promptTokens := root.Get("prompt_eval_count").Int()
	outputTokens := root.Get("eval_count").Int()
	cachedTokens := ollamaPromptCacheEstimateAndStore(cacheKey, promptText, promptTokens)
	out := `{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}}`
	out, _ = sjson.Set(out, "id", fmt.Sprintf("chatcmpl_%x", time.Now().UnixNano()))
	out, _ = sjson.Set(out, "created", time.Now().Unix())
	out, _ = sjson.Set(out, "model", model)
	out, _ = sjson.Set(out, "choices.0.message.content", root.Get("message.content").String())
	if calls := root.Get("message.tool_calls"); calls.Exists() {
		out, _ = sjson.SetRaw(out, "choices.0.message.tool_calls", calls.Raw)
	}
	if doneReason := strings.TrimSpace(root.Get("done_reason").String()); doneReason != "" {
		out, _ = sjson.Set(out, "choices.0.finish_reason", doneReason)
	}
	out, _ = sjson.Set(out, "usage.prompt_tokens", promptTokens)
	out, _ = sjson.Set(out, "usage.completion_tokens", outputTokens)
	out, _ = sjson.Set(out, "usage.total_tokens", promptTokens+outputTokens)
	out, _ = sjson.Set(out, "usage.prompt_tokens_details.cached_tokens", cachedTokens)
	return []byte(out)
}

func ollamaNativeStreamChunkToOpenAI(line []byte, model, responseID, cacheKey, promptText string, roleSent *bool) ([][]byte, coreusage.Detail, bool) {
	root := gjson.ParseBytes(line)
	created := time.Now().Unix()
	var out [][]byte
	if !*roleSent {
		first := ollamaOpenAIStreamLine(responseID, model, created, map[string]any{"role": "assistant"}, nil)
		out = append(out, first)
		*roleSent = true
	}
	delta := make(map[string]any)
	if content := root.Get("message.content").String(); content != "" {
		delta["content"] = content
	}
	if calls := root.Get("message.tool_calls"); calls.Exists() {
		delta["tool_calls"] = calls.Value()
	}
	if len(delta) > 0 {
		out = append(out, ollamaOpenAIStreamLine(responseID, model, created, delta, nil))
	}
	if !root.Get("done").Bool() {
		return out, coreusage.Detail{}, false
	}
	out = append(out, ollamaOpenAIStreamDoneLine(responseID, model, created, root.Get("done_reason").String()))
	promptTokens := root.Get("prompt_eval_count").Int()
	outputTokens := root.Get("eval_count").Int()
	cachedTokens := ollamaPromptCacheEstimateAndStore(cacheKey, promptText, promptTokens)
	usage := coreusage.Detail{
		InputTokens:              promptTokens,
		OutputTokens:             outputTokens,
		TotalTokens:              promptTokens + outputTokens,
		CacheReadTokens:          cachedTokens,
		CachedTokens:             cachedTokens,
		CacheReadIncludedInInput: true,
	}
	out = append(out, ollamaOpenAIStreamLine(responseID, model, created, nil, &usage))
	out = append(out, []byte("data: [DONE]\n\n"))
	return out, usage, true
}

func ollamaOpenAIStreamLine(id, model string, created int64, delta map[string]any, usage *coreusage.Detail) []byte {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
	}
	if usage != nil {
		chunk["choices"] = []any{}
		chunk["usage"] = map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.TotalTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": usage.CachedTokens,
			},
		}
	} else {
		chunk["choices"] = []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}}
	}
	payload, _ := json.Marshal(chunk)
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

func ollamaOpenAIStreamDoneLine(id, model string, created int64, reason string) []byte {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "stop"
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	payload, _ := json.Marshal(chunk)
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

type ollamaPromptCacheEntry struct {
	prompt    string
	expiresAt time.Time
}

var ollamaPromptCache sync.Map

func ollamaPromptCacheEstimateAndStore(cacheKey, prompt string, inputTokens int64) int64 {
	cacheKey = strings.TrimSpace(cacheKey)
	if cacheKey == "" || strings.TrimSpace(prompt) == "" || inputTokens <= 0 {
		return 0
	}
	now := time.Now()
	var cached int64
	if raw, ok := ollamaPromptCache.Load(cacheKey); ok {
		entry, _ := raw.(ollamaPromptCacheEntry)
		if now.Before(entry.expiresAt) {
			common := commonPrefixLen(entry.prompt, prompt)
			if common >= 4096 && len(prompt) > 0 {
				cached = inputTokens * int64(common) / int64(len(prompt))
				if cached > inputTokens {
					cached = inputTokens
				}
			}
		}
	}
	// Native Ollama exercises the real runner prefix cache via /api/chat +
	// keep_alive, but its API reports prompt_eval_count, not cached token count.
	// Expose an OpenAI-compatible cache read estimate from the stable session
	// prefix so dashboards can show the hit without claiming provider reporting.
	ollamaPromptCache.Store(cacheKey, ollamaPromptCacheEntry{
		prompt:    prompt,
		expiresAt: now.Add(30 * time.Minute),
	})
	return cached
}

func commonPrefixLen(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}
