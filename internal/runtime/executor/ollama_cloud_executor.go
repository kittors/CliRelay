package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
)

type OllamaCloudExecutor struct {
	cfg *config.Config
}

func NewOllamaCloudExecutor(cfg *config.Config) *OllamaCloudExecutor {
	return &OllamaCloudExecutor{cfg: cfg}
}

func (e *OllamaCloudExecutor) Identifier() string { return "ollama-cloud" }

func (e *OllamaCloudExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

func (e *OllamaCloudExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("ollama cloud executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return newProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
}

func (e *OllamaCloudExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	execCtx, translated, err := e.prepareOpenAIChat(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}
	ollamaPayload, err := openAIChatToOllamaPayload(translated, false)
	if err != nil {
		return resp, err
	}
	body, _ := json.Marshal(ollamaPayload)
	url := strings.TrimSuffix(baseURL, "/") + "/api/chat"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	e.applyHeaders(httpReq, auth, apiKey)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

	httpResp, err := execCtx.HTTPClient(0).Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("ollama cloud executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return resp, err
	}
	upstreamBody, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(upstreamBody)
	openAIResp, usage := ollamaChatResponseToOpenAI(upstreamBody, gjson.GetBytes(translated, "model").String())
	reporter.publishWithContent(execCtx.Context, usage, string(req.Payload), string(openAIResp))
	reporter.ensurePublished(execCtx.Context)

	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, sdktranslator.FormatOpenAI, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, openAIResp, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func (e *OllamaCloudExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	execCtx, translated, err := e.prepareOpenAIChat(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}
	ollamaPayload, err := openAIChatToOllamaPayload(translated, true)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(ollamaPayload)
	url := strings.TrimSuffix(baseURL, "/") + "/api/chat"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	e.applyHeaders(httpReq, auth, apiKey)
	httpReq.Header.Set("Accept", "application/x-ndjson")
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

	httpResp, err := execCtx.HTTPClient(0).Do(httpReq) //nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
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
			log.Errorf("ollama cloud executor: close response body error: %v", errClose)
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
				log.Errorf("ollama cloud executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		var lastUsage coreusage.Detail
		hasUsage := false
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			openAILine, usage, done := ollamaStreamChunkToOpenAI(line, gjson.GetBytes(translated, "model").String())
			if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
				lastUsage = usage
				hasUsage = true
			}
			if len(openAILine) > 0 {
				chunks := sdktranslator.TranslateStream(execCtx.Context, sdktranslator.FormatOpenAI, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, openAILine, &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
			if done {
				doneChunks := sdktranslator.TranslateStream(execCtx.Context, sdktranslator.FormatOpenAI, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]\n\n"), &param)
				for i := range doneChunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(doneChunks[i])}
				}
			}
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

func (e *OllamaCloudExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return NewOpenAICompatExecutor(e.Identifier(), e.cfg).CountTokens(ctx, auth, req, opts)
}

func (e *OllamaCloudExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *OllamaCloudExecutor) prepareOpenAIChat(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*ExecutionContext, []byte, error) {
	to := sdktranslator.FormatOpenAI
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: stream,
	})
	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	var err error
	translated, err = thinking.ApplyThinking(translated, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, nil, err
	}
	translated = normalizeOpenAIChatToolCallMessages(translated)
	return execCtx, translated, nil
}

func (e *OllamaCloudExecutor) applyHeaders(req *http.Request, auth *cliproxyauth.Auth, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cli-proxy-ollama-cloud")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

func (e *OllamaCloudExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	baseURL = config.DefaultOllamaCloudBaseURL
	if auth == nil {
		return baseURL, ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			baseURL = strings.TrimSuffix(v, "/")
		}
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return baseURL, apiKey
}

type ollamaChatPayload struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func openAIChatToOllamaPayload(body []byte, stream bool) (ollamaChatPayload, error) {
	payload := ollamaChatPayload{
		Model:  strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		Stream: stream,
	}
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" {
			role = "user"
		}
		content := openAIMessageContentText(msg.Get("content"))
		if content == "" && msg.Get("tool_calls").Exists() {
			continue
		}
		payload.Messages = append(payload.Messages, ollamaMessage{Role: role, Content: content})
	}
	options := map[string]any{}
	copyNumberOption(body, options, "temperature", "temperature")
	copyNumberOption(body, options, "top_p", "top_p")
	copyNumberOption(body, options, "presence_penalty", "presence_penalty")
	copyNumberOption(body, options, "frequency_penalty", "frequency_penalty")
	copyNumberOption(body, options, "max_tokens", "num_predict")
	if len(options) > 0 {
		payload.Options = options
	}
	if payload.Model == "" {
		return payload, fmt.Errorf("ollama cloud executor: missing model")
	}
	return payload, nil
}

func openAIMessageContentText(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	if value.Type == gjson.String {
		return value.String()
	}
	if !value.IsArray() {
		return value.String()
	}
	parts := make([]string, 0)
	for _, part := range value.Array() {
		if strings.EqualFold(part.Get("type").String(), "text") {
			if text := part.Get("text").String(); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func copyNumberOption(body []byte, options map[string]any, from, to string) {
	value := gjson.GetBytes(body, from)
	if !value.Exists() {
		return
	}
	switch value.Type {
	case gjson.Number:
		options[to] = value.Value()
	default:
		if strings.TrimSpace(value.String()) != "" {
			options[to] = value.String()
		}
	}
}

func ollamaChatResponseToOpenAI(body []byte, fallbackModel string) ([]byte, coreusage.Detail) {
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		model = fallbackModel
	}
	content := gjson.GetBytes(body, "message.content").String()
	promptTokens := int(gjson.GetBytes(body, "prompt_eval_count").Int())
	completionTokens := int(gjson.GetBytes(body, "eval_count").Int())
	usage := coreusage.Detail{
		InputTokens:  int64(promptTokens),
		OutputTokens: int64(completionTokens),
		TotalTokens:  int64(promptTokens + completionTokens),
	}
	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-ollama-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	data, _ := json.Marshal(out)
	return data, usage
}

func ollamaStreamChunkToOpenAI(body []byte, fallbackModel string) ([]byte, coreusage.Detail, bool) {
	if !gjson.ValidBytes(body) {
		return nil, coreusage.Detail{}, false
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		model = fallbackModel
	}
	content := gjson.GetBytes(body, "message.content").String()
	done := gjson.GetBytes(body, "done").Bool()
	promptTokens := int(gjson.GetBytes(body, "prompt_eval_count").Int())
	completionTokens := int(gjson.GetBytes(body, "eval_count").Int())
	usage := coreusage.Detail{
		InputTokens:  int64(promptTokens),
		OutputTokens: int64(completionTokens),
		TotalTokens:  int64(promptTokens + completionTokens),
	}
	chunk := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-ollama-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"content": content,
			},
			"finish_reason": nil,
		}},
	}
	if done {
		chunk["choices"] = []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}}
		chunk["usage"] = map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
	}
	data, _ := json.Marshal(chunk)
	return append(append([]byte("data: "), data...), []byte("\n\n")...), usage, done
}
