package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// Keep defaults aligned with upstream CLIProxyAPI (codex-tui).
	codexUserAgent  = "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	codexOriginator = "codex-tui"
)

var dataTag = []byte("data:")

func ensureTranslatedCodexModel(body []byte, fallback string) []byte {
	if strings.TrimSpace(gjson.GetBytes(body, "model").String()) != "" {
		return body
	}
	body, _ = sjson.SetBytes(body, "model", fallback)
	return body
}

func extractCodexResponsesOutputItemDone(payload []byte) ([]byte, string, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return nil, "", false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Raw == "" {
		return nil, "", false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = item.Raw
	}
	return []byte(item.Raw), key, true
}

func mergeCodexResponsesCompletedOutput(payload []byte, pendingItems [][]byte, pendingKeys []string) []byte {
	if len(pendingItems) == 0 || gjson.GetBytes(payload, "type").String() != "response.completed" {
		return payload
	}

	output := gjson.GetBytes(payload, "response.output")
	merged := make([][]byte, 0, len(pendingItems)+len(output.Array()))
	seen := make(map[string]struct{}, len(pendingItems)+len(output.Array()))

	if output.IsArray() {
		for _, item := range output.Array() {
			raw := []byte(item.Raw)
			key := strings.TrimSpace(item.Get("id").String())
			if key == "" {
				key = item.Raw
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, raw)
		}
	}

	appended := false
	for idx, item := range pendingItems {
		key := pendingKeys[idx]
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
		appended = true
	}

	if !appended {
		return payload
	}

	buf := bytes.NewBuffer(make([]byte, 0, len(payload)+len(pendingItems)*64))
	buf.WriteByte('[')
	for idx, item := range merged {
		if idx > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')

	updated, err := sjson.SetRawBytes(payload, "response.output", buf.Bytes())
	if err != nil {
		return payload
	}
	return updated
}

func collectCodexOutputItems(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	outputItems := make([][]byte, 0, 2)
	completedIdx := -1
	var completedPayload []byte

	for i := range lines {
		line := lines[i]
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		payload := bytes.TrimSpace(line[len(dataTag):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		switch eventType {
		case "response.output_item.done":
			item := gjson.GetBytes(payload, "item")
			if item.Exists() {
				raw := strings.TrimSpace(item.Raw)
				if raw != "" {
					outputItems = append(outputItems, []byte(raw))
				}
			}
		case "response.completed", "response.done":
			completedIdx = i
			completedPayload = normalizeCodexCompletionPayload(payload)
		}
	}

	if completedIdx < 0 {
		return data
	}

	changed := !bytes.Equal(completedPayload, bytes.TrimSpace(lines[completedIdx][len(dataTag):]))
	if len(outputItems) > 0 {
		existingOutput := gjson.GetBytes(completedPayload, "response.output")
		if !existingOutput.Exists() || len(existingOutput.Array()) == 0 {
			merged, err := sjson.SetRawBytes(completedPayload, "response.output", marshalJSONArrayRaw(outputItems))
			if err == nil && len(merged) > 0 {
				completedPayload = merged
				changed = true
			}
		}
	}

	if !changed {
		return data
	}
	updatedLine := make([]byte, 0, len(dataTag)+1+len(completedPayload))
	updatedLine = append(updatedLine, dataTag...)
	updatedLine = append(updatedLine, ' ')
	updatedLine = append(updatedLine, completedPayload...)
	lines[completedIdx] = updatedLine
	return bytes.Join(lines, []byte("\n"))
}

func marshalJSONArrayRaw(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	total := 2
	for i := range items {
		total += len(items[i])
	}
	total += len(items) - 1
	buf := make([]byte, 0, total)
	buf = append(buf, '[')
	for i := range items {
		if i > 0 {
			buf = append(buf, byte(44))
		}
		buf = append(buf, items[i]...)
	}
	buf = append(buf, ']')
	return buf
}

func normalizeCodexCompletionPayload(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "response.done" {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "type", "response.completed")
	if err == nil && len(updated) > 0 {
		return updated
	}
	return payload
}

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
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

func (e *CodexExecutor) ProbeQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaProbeResult, error) {
	if auth == nil {
		return nil, fmt.Errorf("codex executor: auth is nil")
	}
	accountID := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["account_id"].(string); ok {
			accountID = strings.TrimSpace(v)
		}
	}
	if accountID == "" {
		return nil, fmt.Errorf("codex executor: missing account_id")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, err
	}
	apiKey, _ := codexCreds(auth)
	applyCodexHeaders(req, e.cfg, auth, apiKey, false)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close quota probe body error: %v", errClose)
		}
	}()

	body, err := readUpstreamResponseBody(e.Identifier(), resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newCodexStatusErr(resp.StatusCode, body)
	}
	return parseCodexQuotaProbe(body), nil
}

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == codexImageGenerationAlt || opts.Alt == codexImageEditsAlt {
		return e.executeImageGeneration(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = ensureTranslatedCodexModel(body, baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	if !gjson.GetBytes(body, "instructions").Exists() {
		// Extract system messages from original OpenAI-format payload to use as instructions.
		// This preserves system prompts injected by SystemPromptMiddleware.
		sysContent := extractSystemMessagesAsInstructions(req.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		err = newCodexStatusErr(httpResp.StatusCode, b)
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)

	lines := bytes.Split(data, []byte("\n"))
	pendingOutputItems := make([][]byte, 0, 1)
	pendingOutputKeys := make([]string, 0, 1)
	pendingSeen := make(map[string]struct{})
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		line = bytes.TrimSpace(line[5:])
		line = normalizeCodexCompletionPayload(line)
		if item, key, ok := extractCodexResponsesOutputItemDone(line); ok {
			if _, exists := pendingSeen[key]; !exists {
				pendingSeen[key] = struct{}{}
				pendingOutputItems = append(pendingOutputItems, item)
				pendingOutputKeys = append(pendingOutputKeys, key)
			}
			continue
		}
		if gjson.GetBytes(line, "type").String() != "response.completed" {
			continue
		}
		line = mergeCodexResponsesCompletedOutput(line, pendingOutputItems, pendingOutputKeys)

		if detail, ok := parseCodexUsage(line); ok {
			reporter.publishWithContent(ctx, detail, string(req.Payload), string(data))
		}

		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, line, &param)
		resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
		return resp, nil
	}
	err = statusErr{code: 408, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
	return resp, err
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = ensureTranslatedCodexModel(body, baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, false)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		err = newCodexStatusErr(httpResp.StatusCode, b)
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publishWithContent(ctx, parseOpenAIUsage(data), string(req.Payload), string(data))
	reporter.ensurePublished(ctx)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body = ensureTranslatedCodexModel(body, baseModel)
	if !gjson.GetBytes(body, "instructions").Exists() {
		sysContent := extractSystemMessagesAsInstructions(req.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(data))
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			reporter.appendOutputChunk(line)

			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					if detail, ok := parseCodexUsage(data); ok {
						reporter.publish(ctx, detail)
					}
				}
			}

			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if shouldSuppressUsageFailure(errScan, "") {
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
				return
			}
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *CodexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err := thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body = ensureTranslatedCodexModel(body, baseModel)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.SetBytes(body, "stream", false)
	if !gjson.GetBytes(body, "instructions").Exists() {
		sysContent := extractSystemMessagesAsInstructions(req.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func tokenizerForCodexModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}

func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	var segments []string

	if inst := strings.TrimSpace(root.Get("instructions").String()); inst != "" {
		segments = append(segments, inst)
	}

	inputItems := root.Get("input")
	if inputItems.IsArray() {
		arr := inputItems.Array()
		for i := range arr {
			item := arr[i]
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					parts := content.Array()
					for j := range parts {
						part := parts[j]
						if text := strings.TrimSpace(part.Get("text").String()); text != "" {
							segments = append(segments, text)
						}
					}
				}
			case "function_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					segments = append(segments, name)
				}
				if args := strings.TrimSpace(item.Get("arguments").String()); args != "" {
					segments = append(segments, args)
				}
			case "function_call_output":
				if out := strings.TrimSpace(item.Get("output").String()); out != "" {
					segments = append(segments, out)
				}
			default:
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					segments = append(segments, text)
				}
			}
		}
	}

	tools := root.Get("tools")
	if tools.IsArray() {
		tarr := tools.Array()
		for i := range tarr {
			tool := tarr[i]
			if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
				segments = append(segments, name)
			}
			if desc := strings.TrimSpace(tool.Get("description").String()); desc != "" {
				segments = append(segments, desc)
			}
			if params := tool.Get("parameters"); params.Exists() {
				val := params.Raw
				if params.Type == gjson.String {
					val = params.String()
				}
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		if name := strings.TrimSpace(textFormat.Get("name").String()); name != "" {
			segments = append(segments, name)
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			val := schema.Raw
			if schema.Type == gjson.String {
				val = schema.String()
			}
			if trimmed := strings.TrimSpace(val); trimmed != "" {
				segments = append(segments, trimmed)
			}
		}
	}

	text := strings.Join(segments, "\n")
	if text == "" {
		return 0, nil
	}

	count, err := enc.Count(text)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := codexauth.NewCodexAuth(e.cfg)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte) (*http.Request, error) {
	var cache codexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			var ok bool
			if cache, ok = getCodexCache(key); !ok {
				cache = codexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				setCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Conversation_id", cache.ID)
		httpReq.Header.Set("Session_id", cache.ID)
	}
	return httpReq, nil
}

func applyCodexHeaders(r *http.Request, cfg *config.Config, auth *cliproxyauth.Auth, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	fp, fingerprintEnabled := codexIdentityFingerprint(cfg)
	if ginHeaders != nil {
		// Align with upstream: if the client sent Codex beta features, preserve them.
		if v := strings.TrimSpace(ginHeaders.Get("X-Codex-Beta-Features")); v != "" {
			r.Header.Set("X-Codex-Beta-Features", v)
		}
	}
	// Align with upstream: only propagate these from the client when present.
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")

	if fingerprintEnabled {
		applyCodexIdentityFingerprintHeaders(r.Header, fp, false)
	} else {
		misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", codexUserAgent)
	}

	// Upstream codex-tui behavior: only attach Session_id when the UA indicates a desktop client.
	if strings.Contains(r.Header.Get("User-Agent"), "Mac OS") && strings.TrimSpace(r.Header.Get("Session_id")) == "" {
		r.Header.Set("Session_id", uuid.NewString())
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}

	originatorFromClient := ""
	if ginHeaders != nil {
		originatorFromClient = strings.TrimSpace(ginHeaders.Get("Originator"))
	}
	if originatorFromClient != "" {
		r.Header.Set("Originator", originatorFromClient)
	} else if !isAPIKey {
		if fingerprintEnabled {
			r.Header.Set("Originator", fp.Originator)
		} else {
			r.Header.Set("Originator", codexOriginator)
		}
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	if fingerprintEnabled {
		applyCodexIdentityFingerprintHeaders(r.Header, fp, false)
		if originatorFromClient == "" && !isAPIKey {
			r.Header.Set("Originator", fp.Originator)
		}
	}
}

func newCodexStatusErr(statusCode int, body []byte) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if retryAfter := parseCodexRetryAfter(statusCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	if strings.TrimSpace(gjson.GetBytes(errorBody, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
	if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	if resetsInSeconds := gjson.GetBytes(errorBody, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		retryAfter := time.Duration(resetsInSeconds) * time.Second
		return &retryAfter
	}
	return nil
}

func parseCodexQuotaProbe(body []byte) *cliproxyauth.QuotaProbeResult {
	if len(body) == 0 {
		return nil
	}

	rateLimit := gjson.GetBytes(body, "rate_limit")
	if !rateLimit.Exists() {
		return nil
	}

	allowed := rateLimit.Get("allowed")
	limitReached := rateLimit.Get("limit_reached")
	if allowed.Exists() && !allowed.Bool() {
		return codexQuotaProbeBlocked(rateLimit)
	}
	if limitReached.Exists() && limitReached.Bool() {
		return codexQuotaProbeBlocked(rateLimit)
	}

	seenWindow := false
	for _, path := range codexQuotaWindowPaths() {
		window := rateLimit.Get(path)
		if !window.Exists() {
			continue
		}
		seenWindow = true
		if codexQuotaWindowExceeded(window) {
			return codexQuotaProbeBlocked(rateLimit)
		}
	}
	if seenWindow || (allowed.Exists() && allowed.Bool()) {
		return &cliproxyauth.QuotaProbeResult{Recovered: true}
	}

	return codexQuotaProbeBlocked(rateLimit)
}

func codexQuotaProbeBlocked(rateLimit gjson.Result) *cliproxyauth.QuotaProbeResult {
	return &cliproxyauth.QuotaProbeResult{
		Recovered:     false,
		NextRecoverAt: codexQuotaNextRecoverAt(rateLimit, time.Now()),
	}
}

func codexQuotaWindowPaths() []string {
	return []string{"primary_window", "secondary_window", "weekly_window", "week_window", "long_window"}
}

func codexQuotaNextRecoverAt(rateLimit gjson.Result, now time.Time) time.Time {
	nextRecoverAt := time.Time{}
	for _, path := range codexQuotaWindowPaths() {
		window := rateLimit.Get(path)
		if !window.Exists() || !codexQuotaWindowExceeded(window) {
			continue
		}
		if resetAt := codexQuotaWindowResetAt(window, now); !resetAt.IsZero() {
			if nextRecoverAt.IsZero() || resetAt.Before(nextRecoverAt) {
				nextRecoverAt = resetAt
			}
		}
	}
	return nextRecoverAt
}

func codexQuotaWindowExceeded(window gjson.Result) bool {
	if !window.Exists() {
		return false
	}
	if limitReached := window.Get("limit_reached"); limitReached.Exists() {
		return limitReached.Bool()
	}
	if usedPercent := window.Get("used_percent"); usedPercent.Exists() {
		return usedPercent.Float() >= 100
	}
	if remaining := window.Get("remaining"); remaining.Exists() {
		return remaining.Float() <= 0
	}
	if available := window.Get("available"); available.Exists() {
		return !available.Bool()
	}
	return false
}

func codexQuotaWindowResetAt(window gjson.Result, now time.Time) time.Time {
	if !window.Exists() {
		return time.Time{}
	}
	for _, path := range []string{"reset_at", "resets_at"} {
		if resetAt := window.Get(path).Int(); resetAt > 0 {
			resetAtTime := time.Unix(resetAt, 0)
			if resetAtTime.After(now) {
				return resetAtTime
			}
		}
	}
	for _, path := range []string{"reset_after_seconds", "resets_in_seconds"} {
		if afterSeconds := window.Get(path).Int(); afterSeconds > 0 {
			return now.Add(time.Duration(afterSeconds) * time.Second)
		}
	}
	return time.Time{}
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.CodexKey {
		entry := &e.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.CodexKey {
			entry := &e.cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}
