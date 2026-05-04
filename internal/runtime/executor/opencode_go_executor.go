package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openCodeGoProvider = "opencode-go"

var opencodeGoBaseURL = "https://opencode.ai/zen/go/v1"

var opencodeGoMessagesModels = map[string]struct{}{
	"minimax-m2.7": {},
	"minimax-m2.5": {},
}

// OpenCodeGoExecutor routes OpenCode Go models to the provider's mixed OpenAI/Anthropic endpoints.
type OpenCodeGoExecutor struct {
	cfg *config.Config
}

func NewOpenCodeGoExecutor(cfg *config.Config) *OpenCodeGoExecutor {
	return &OpenCodeGoExecutor{cfg: cfg}
}

func (e *OpenCodeGoExecutor) Identifier() string { return openCodeGoProvider }

func (e *OpenCodeGoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if apiKey := strings.TrimSpace(opencodeGoAPIKey(auth)); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

func (e *OpenCodeGoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("opencode go executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *OpenCodeGoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opencodeGoUsesMessages(req.Model) {
		return e.executeMessages(ctx, auth, req, opts)
	}
	return e.openAIExecutor().Execute(ctx, opencodeGoAuthWithBaseURL(auth), req, opts)
}

func (e *OpenCodeGoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if opencodeGoUsesMessages(req.Model) {
		return e.executeMessagesStream(ctx, auth, req, opts)
	}
	return e.openAIExecutor().ExecuteStream(ctx, opencodeGoAuthWithBaseURL(auth), req, opts)
}

func (e *OpenCodeGoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opencodeGoUsesMessages(req.Model) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "token counting is not supported for OpenCode Go messages models"}
	}
	return e.openAIExecutor().CountTokens(ctx, opencodeGoAuthWithBaseURL(auth), req, opts)
}

func (e *OpenCodeGoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *OpenCodeGoExecutor) openAIExecutor() *OpenAICompatExecutor {
	return NewOpenAICompatExecutor(openCodeGoProvider, e.cfg)
}

func (e *OpenCodeGoExecutor) executeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey := opencodeGoAPIKey(auth)
	if strings.TrimSpace(apiKey) == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "missing OpenCode Go API key"}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FormatClaude
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, from != to)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, from != to)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	url := strings.TrimSuffix(opencodeGoBaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	e.applyMessagesHeaders(httpReq, auth, apiKey, false)
	recordAPIRequest(ctx, e.cfg, e.requestLog(url, httpReq, body, auth))

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode go executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publishWithContent(ctx, parseClaudeUsage(data), string(req.Payload), string(data))
	reporter.ensurePublished(ctx)

	bodyForTranslation := data
	if from != to {
		bodyForTranslation = opencodeGoClaudeMessageToSSE(data)
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bodyForTranslation, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func (e *OpenCodeGoExecutor) executeMessagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey := opencodeGoAPIKey(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing OpenCode Go API key"}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FormatClaude
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	url := strings.TrimSuffix(opencodeGoBaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	e.applyMessagesHeaders(httpReq, auth, apiKey, true)
	recordAPIRequest(ctx, e.cfg, e.requestLog(url, httpReq, body, auth))

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
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
			appendAPIResponseChunk(ctx, e.cfg, line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if from == to {
				cloned := append(bytes.Clone(line), '\n')
				out <- cliproxyexecutor.StreamChunk{Payload: cloned}
				continue
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		reporter.ensurePublished(ctx)
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

func (e *OpenCodeGoExecutor) requestLog(url string, req *http.Request, body []byte, auth *cliproxyauth.Auth) upstreamRequestLog {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	return upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   req.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
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
