package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/multimodaladapter"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	e.applyIdentityFingerprint(req, auth, false)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
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

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	imagePassthrough := false
	switch opts.Alt {
	case "responses/compact":
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	case "images/generations":
		endpoint = "/images/generations"
		imagePassthrough = true
	case "images/edits":
		switch e.imageEditsMode(auth) {
		case "chat-multimodal":
			endpoint = "/chat/completions"
		case "image-generations":
			endpoint = "/images/generations"
			imagePassthrough = true
		default:
			endpoint = "/images/edits"
			imagePassthrough = true
		}
	}
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	adaptedPayload, _, err := e.applyMultimodalAdapter(ctx, req.Payload, requestedModel, baseModel, opts.SourceFormat.String())
	if err != nil {
		return resp, err
	}
	req.Payload = adaptedPayload
	var translated []byte
	if imagePassthrough {
		translated = e.overrideModel(req.Payload, baseModel)
		if e.imageEditsMode(auth) == "image-generations" {
			translated, err = convertImagePayloadToImageGenerations(translated, e.imageGenerationsImageField(auth))
			if err != nil {
				return resp, statusErr{code: http.StatusBadRequest, msg: err.Error()}
			}
		}
	} else {
		originalPayload := originalPayloadSource
		originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, opts.Stream)
		translated = sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, opts.Stream)
		translated = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)
		if opts.Alt == "responses/compact" {
			if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
				translated = updated
			}
		}

		translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
		if err != nil {
			return resp, err
		}
		if shouldNormalizeKimiCompatPayload(baseModel) {
			translated, err = normalizeKimiToolMessageLinks(translated)
			if err != nil {
				return resp, err
			}
		}
		if opts.Alt == "images/edits" && e.imageEditsMode(auth) == "chat-multimodal" {
			translated, err = convertImageEditPayloadToChatMultimodal(translated)
			if err != nil {
				return resp, statusErr{code: http.StatusBadRequest, msg: err.Error()}
			}
		}
	}

	url := strings.TrimSuffix(baseURL, "/") + endpoint
	requestBody := translated
	contentType := "application/json"
	if opts.Alt == "images/edits" && endpoint == "/images/edits" && imageEditPayloadHasUploads(translated) {
		var multipartType string
		requestBody, multipartType, err = buildImageEditsMultipartBody(translated)
		if err != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: err.Error()}
		}
		contentType = multipartType
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	e.applyIdentityFingerprint(httpReq, auth, false)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
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
		Body:      requestBody,
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
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), upstreamBody: b}
		return resp, err
	}
	body, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	reporter.publishWithContent(ctx, parseOpenAIUsage(body), string(req.Payload), string(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.ensurePublished(ctx)
	if imagePassthrough {
		resp = cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}
		return resp, nil
	}
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	adaptedPayload, _, err := e.applyMultimodalAdapter(ctx, req.Payload, requestedModel, baseModel, opts.SourceFormat.String())
	if err != nil {
		return nil, err
	}
	req.Payload = adaptedPayload
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	if shouldNormalizeKimiCompatPayload(baseModel) {
		translated, err = normalizeKimiToolMessageLinks(translated)
		if err != nil {
			return nil, err
		}
	}

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	e.applyIdentityFingerprint(httpReq, auth, false)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
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
		Body:      translated,
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
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b), upstreamBody: b}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if len(line) == 0 {
				continue
			}

			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}

			// OpenAI-compatible streams are SSE: lines typically prefixed with "data: ".
			// Pass through translator; it yields one or more chunks for the target schema.
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		// Ensure we record the request if no usage chunk was ever seen
		reporter.ensurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenAICompatExecutor) applyMultimodalAdapter(ctx context.Context, payload []byte, requestedModel, upstreamModel, protocol string) ([]byte, multimodaladapter.Report, error) {
	if e == nil || e.cfg == nil || len(payload) == 0 {
		return payload, multimodaladapter.Report{}, nil
	}
	adapted, report, err := multimodaladapter.Apply(ctx, payload, multimodaladapter.Route{
		RequestedModel:   requestedModel,
		UpstreamProvider: e.Identifier(),
		UpstreamModel:    upstreamModel,
		Protocol:         protocol,
	}, e.cfg.MultimodalAdapters)
	if err != nil {
		if report.MediaItems > 0 || report.Extractor != "" {
			log.Warnf("multimodal adapter: rejected request requested_model=%s upstream_provider=%s upstream_model=%s protocol=%s media=%d extractor=%s injected=%v stripped=%v error=%v",
				requestedModel, e.Identifier(), upstreamModel, protocol, report.MediaItems, report.Extractor, report.Injected, report.Stripped, err)
		}
		return payload, report, err
	}
	if report.Applied {
		log.Infof("multimodal adapter: processed request requested_model=%s upstream_provider=%s upstream_model=%s protocol=%s media=%d extractor=%s injected=%v stripped=%v",
			requestedModel, e.Identifier(), upstreamModel, protocol, report.MediaItems, report.Extractor, report.Injected, report.Stripped)
	}
	return adapted, report, nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := tokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	_ = ctx
	return auth, nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) applyIdentityFingerprint(req *http.Request, auth *cliproxyauth.Auth, websocket bool) {
	if req == nil {
		return
	}
	fingerprint := ""
	if auth != nil && auth.Attributes != nil {
		fingerprint = strings.TrimSpace(strings.ToLower(auth.Attributes["identity_fingerprint"]))
	}
	if fingerprint == "" {
		if compat := e.resolveCompatConfig(auth); compat != nil {
			fingerprint = strings.TrimSpace(strings.ToLower(compat.IdentityFingerprint))
		}
	}
	switch fingerprint {
	case "codex":
		if fp, ok := codexIdentityFingerprint(e.cfg); ok {
			applyCodexIdentityFingerprintHeaders(req.Header, fp, websocket)
			if strings.TrimSpace(fp.Originator) != "" {
				req.Header.Set("Originator", fp.Originator)
			}
		}
	}
}

func (e *OpenAICompatExecutor) imageEditsMode(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(strings.ToLower(auth.Attributes["image_edits_mode"])); v != "" {
			switch v {
			case "passthrough", "native", "image-edits":
				return "passthrough"
			case "chat-multimodal", "image-generations":
				return v
			}
		}
	}
	if compat := e.resolveCompatConfig(auth); compat != nil {
		switch strings.TrimSpace(strings.ToLower(compat.ImageEditsMode)) {
		case "passthrough", "native", "image-edits":
			return "passthrough"
		case "chat-multimodal":
			return "chat-multimodal"
		case "image-generations":
			return "image-generations"
		}
	}
	return ""
}

func (e *OpenAICompatExecutor) imageGenerationsImageField(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := normalizeImageGenerationsImageField(auth.Attributes["image_generations_image_field"]); v != "" {
			return v
		}
	}
	if compat := e.resolveCompatConfig(auth); compat != nil {
		if v := normalizeImageGenerationsImageField(compat.ImageGenerationsImageField); v != "" {
			return v
		}
	}
	return "image"
}

func normalizeImageGenerationsImageField(field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "image_url":
		return "image_url"
	case "image":
		return "image"
	default:
		return ""
	}
}

func imageEditPayloadHasUploads(payload []byte) bool {
	return gjson.GetBytes(payload, "image_files").Exists() || gjson.GetBytes(payload, "mask_file").Exists()
}

func buildImageEditsMultipartBody(payload []byte) ([]byte, string, error) {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, "", fmt.Errorf("invalid image edits payload: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range root {
		switch key {
		case "image_files", "mask_file":
			continue
		}
		if value == nil {
			continue
		}
		fieldValue, err := stringifyMultipartField(value)
		if err != nil {
			return nil, "", fmt.Errorf("encode image edits field %s: %w", key, err)
		}
		if fieldValue == "" {
			continue
		}
		if err := writer.WriteField(key, fieldValue); err != nil {
			return nil, "", fmt.Errorf("write image edits field %s: %w", key, err)
		}
	}
	if err := writeImageEditFiles(writer, "image", root["image_files"]); err != nil {
		return nil, "", err
	}
	if err := writeImageEditFile(writer, "mask", root["mask_file"]); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finalize image edits multipart body: %w", err)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func stringifyMultipartField(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return fmt.Sprint(typed), nil
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	case json.Number:
		return typed.String(), nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}
}

func writeImageEditFiles(writer *multipart.Writer, fieldName string, value any) error {
	if value == nil {
		return nil
	}
	files, ok := value.([]any)
	if !ok {
		return fmt.Errorf("image edits %s must be an array", fieldName)
	}
	if len(files) == 0 {
		return fmt.Errorf("image edits %s is required", fieldName)
	}
	for _, file := range files {
		if err := writeImageEditFile(writer, fieldName, file); err != nil {
			return err
		}
	}
	return nil
}

func writeImageEditFile(writer *multipart.Writer, fieldName string, value any) error {
	if value == nil {
		return nil
	}
	file, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("image edits %s file must be an object", fieldName)
	}
	fileName := strings.TrimSpace(fmt.Sprint(file["file_name"]))
	if fileName == "" || fileName == "<nil>" {
		fileName = fieldName + ".png"
	}
	contentType := strings.TrimSpace(fmt.Sprint(file["content_type"]))
	if contentType == "" || contentType == "<nil>" {
		contentType = "application/octet-stream"
	}
	dataBase64 := strings.TrimSpace(fmt.Sprint(file["data_base64"]))
	if dataBase64 == "" || dataBase64 == "<nil>" {
		return fmt.Errorf("image edits %s file is missing data_base64", fieldName)
	}
	data, err := decodeImageEditBase64(dataBase64)
	if err != nil {
		return fmt.Errorf("decode image edits %s file: %w", fieldName, err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeMultipartQuote(fieldName), escapeMultipartQuote(fileName)))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create image edits %s part: %w", fieldName, err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write image edits %s file: %w", fieldName, err)
	}
	return nil
}

func decodeImageEditBase64(value string) ([]byte, error) {
	if comma := strings.Index(value, ","); comma >= 0 {
		prefix := strings.ToLower(strings.TrimSpace(value[:comma]))
		if strings.HasPrefix(prefix, "data:") {
			value = value[comma+1:]
		}
	}
	return base64.StdEncoding.DecodeString(value)
}

func escapeMultipartQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	return strings.ReplaceAll(value, `"`, "\\\"")
}

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			candidates = append(candidates, v)
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			candidates = append(candidates, v)
		}
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func convertImagePayloadToImageGenerations(payload []byte, imageField string) ([]byte, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return payload, nil
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("invalid image payload: %w", err)
	}
	model := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(payload, "prompt").String())
	if prompt == "" {
		prompt = strings.TrimSpace(extractImageGenerationPrompt(payload))
	}
	image := strings.TrimSpace(firstImageGenerationImage(payload))
	if image != "" && prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if image == "" && prompt != "" && !hasImageEditOnlyFields(payload) {
		return payload, nil
	}
	out := make(map[string]any, len(root))
	for _, field := range []string{
		"model", "prompt", "size", "quality", "response_format", "background", "output_format",
		"moderation", "input_fidelity", "style", "n", "output_compression", "partial_images",
	} {
		if value, ok := root[field]; ok {
			out[field] = value
		}
	}
	out["model"] = model
	if prompt != "" {
		out["prompt"] = prompt
	}
	if image != "" {
		field := normalizeImageGenerationsImageField(imageField)
		if field == "" {
			field = "image"
		}
		out[field] = image
	}
	return json.Marshal(out)
}

func hasImageEditOnlyFields(payload []byte) bool {
	for _, field := range []string{"image_files", "mask_file", "input", "messages"} {
		if gjson.GetBytes(payload, field).Exists() {
			return true
		}
	}
	return false
}

func extractImageGenerationPrompt(payload []byte) string {
	texts := make([]string, 0, 4)
	if input := gjson.GetBytes(payload, "input"); input.Exists() {
		collectPromptText(input, &texts)
	}
	if messages := gjson.GetBytes(payload, "messages"); messages.Exists() {
		collectPromptText(messages, &texts)
	}
	return strings.Join(texts, "\n")
}

func collectPromptText(value gjson.Result, texts *[]string) {
	switch {
	case value.IsArray():
		for _, item := range value.Array() {
			collectPromptText(item, texts)
		}
	case value.IsObject():
		if messages := value.Get("messages"); messages.Exists() {
			collectPromptText(messages, texts)
			return
		}
		if content := value.Get("content"); content.Exists() {
			collectPromptText(content, texts)
			return
		}
		for _, field := range []string{"text", "input_text"} {
			if text := strings.TrimSpace(value.Get(field).String()); text != "" {
				*texts = append(*texts, text)
				return
			}
		}
	default:
		if text := strings.TrimSpace(value.String()); text != "" {
			*texts = append(*texts, text)
		}
	}
}

func firstImageGenerationImage(payload []byte) string {
	for _, field := range []string{"image", "image_url"} {
		if image := imageURLFromResult(gjson.GetBytes(payload, field)); image != "" {
			return image
		}
	}
	if files := gjson.GetBytes(payload, "image_files"); files.Exists() && files.IsArray() {
		for _, file := range files.Array() {
			data := strings.TrimSpace(file.Get("data_base64").String())
			if data == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(data), "data:") {
				return data
			}
			contentType := strings.TrimSpace(file.Get("content_type").String())
			if contentType == "" {
				contentType = "image/png"
			}
			return "data:" + contentType + ";base64," + data
		}
	}
	for _, path := range []string{"input", "messages"} {
		if image := firstImageFromContent(gjson.GetBytes(payload, path)); image != "" {
			return image
		}
	}
	return ""
}

func firstImageFromContent(value gjson.Result) string {
	switch {
	case value.IsArray():
		for _, item := range value.Array() {
			if image := firstImageFromContent(item); image != "" {
				return image
			}
		}
	case value.IsObject():
		if messages := value.Get("messages"); messages.Exists() {
			if image := firstImageFromContent(messages); image != "" {
				return image
			}
		}
		for _, field := range []string{"image", "image_url"} {
			if image := imageURLFromResult(value.Get(field)); image != "" {
				return image
			}
		}
		if content := value.Get("content"); content.Exists() {
			return firstImageFromContent(content)
		}
	}
	return ""
}

func imageURLFromResult(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	if value.IsObject() {
		return strings.TrimSpace(value.Get("url").String())
	}
	return strings.TrimSpace(value.String())
}

func convertImageEditPayloadToChatMultimodal(payload []byte) ([]byte, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return payload, nil
	}
	model := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(payload, "prompt").String())
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	files := gjson.GetBytes(payload, "image_files")
	if !files.Exists() || !files.IsArray() || len(files.Array()) == 0 {
		return nil, fmt.Errorf("image file is required")
	}

	content := make([]map[string]any, 0, 1+len(files.Array())+1)
	content = append(content, map[string]any{"type": "text", "text": prompt})
	for _, file := range files.Array() {
		data := strings.TrimSpace(file.Get("data_base64").String())
		if data == "" {
			return nil, fmt.Errorf("image_files[].data_base64 is required")
		}
		contentType := strings.TrimSpace(file.Get("content_type").String())
		if contentType == "" {
			contentType = "image/png"
		}
		url := data
		if !strings.HasPrefix(strings.ToLower(url), "data:") {
			url = "data:" + contentType + ";base64," + data
		}
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": url,
			},
		})
	}
	if mask := gjson.GetBytes(payload, "mask_file"); mask.Exists() {
		data := strings.TrimSpace(mask.Get("data_base64").String())
		if data != "" {
			contentType := strings.TrimSpace(mask.Get("content_type").String())
			if contentType == "" {
				contentType = "image/png"
			}
			content = append(content, map[string]any{"type": "text", "text": "Mask image:"})
			content = append(content, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + contentType + ";base64," + data,
				},
			})
		}
	}

	out := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
	}
	if stream := gjson.GetBytes(payload, "stream"); stream.Exists() {
		out["stream"] = stream.Bool()
	}
	return json.Marshal(out)
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "model", model)
	return payload
}

func shouldNormalizeKimiCompatPayload(model string) bool {
	model = strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(model, "kimi-") ||
		strings.Contains(model, "/kimi-") ||
		strings.Contains(model, "moonshot")
}

type statusErr struct {
	code         int
	msg          string
	retryAfter   *time.Duration
	upstreamBody []byte
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
func (e statusErr) UpstreamErrorBody() []byte {
	if len(e.upstreamBody) == 0 {
		if trimmed := strings.TrimSpace(e.msg); trimmed != "" && json.Valid([]byte(trimmed)) {
			return []byte(trimmed)
		}
		return nil
	}
	return append([]byte(nil), e.upstreamBody...)
}
