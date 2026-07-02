package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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

const (
	defaultBedrockRegion  = "us-east-1"
	bedrockAuthModeAPIKey = "api-key"
)

var bedrockCrossRegionPrefixes = []string{"us.", "eu.", "apac.", "jp.", "au.", "us-gov.", "global."}

var defaultBedrockModelMapping = map[string]string{
	"claude-opus-4-7":            "us.anthropic.claude-opus-4-7-v1",
	"claude-opus-4-6-thinking":   "us.anthropic.claude-opus-4-6-v1",
	"claude-opus-4-6":            "us.anthropic.claude-opus-4-6-v1",
	"claude-opus-4-5-thinking":   "us.anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-5-20251101":   "us.anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-1":            "us.anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-opus-4-20250514":     "us.anthropic.claude-opus-4-20250514-v1:0",
	"claude-sonnet-4-6-thinking": "us.anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-6":          "us.anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-5":          "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-thinking": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-20250929": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-20250514":   "us.anthropic.claude-sonnet-4-20250514-v1:0",
	"claude-haiku-4-5":           "us.anthropic.claude-haiku-4-5-20251001-v1:0",
	"claude-haiku-4-5-20251001":  "us.anthropic.claude-haiku-4-5-20251001-v1:0",
}

// BedrockExecutor executes Anthropic Claude payloads through AWS Bedrock Runtime.
type BedrockExecutor struct {
	cfg *config.Config
}

func NewBedrockExecutor(cfg *config.Config) *BedrockExecutor { return &BedrockExecutor{cfg: cfg} }

func (e *BedrockExecutor) Identifier() string { return "bedrock" }

func (e *BedrockExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	body, err := readAndResetRequestBody(req)
	if err != nil {
		return err
	}
	return e.prepareBedrockHTTPRequest(req.Context(), req, auth, body)
}

func (e *BedrockExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("bedrock executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	body, err := readAndResetRequestBody(req)
	if err != nil {
		return nil, err
	}
	httpReq := req.WithContext(ctx)
	if err := e.prepareBedrockHTTPRequest(ctx, httpReq, auth, body); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *BedrockExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	mappedModel, ok := resolveBedrockModelID(auth, baseModel)
	if !ok {
		err = statusErr{code: http.StatusBadRequest, msg: "unsupported bedrock model: " + baseModel}
		return resp, err
	}

	to := sdktranslator.FromString("claude")
	translatorStreamMode := opts.SourceFormat != to
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: translatorStreamMode,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)
	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body = disableThinkingIfToolChoiceForced(body)
	betaTokens := resolveBedrockBetaTokens(opts.Headers.Get("anthropic-beta"), body, mappedModel)
	bodyForTranslation := body
	body, err = prepareBedrockRequestBody(body, mappedModel, betaTokens)
	if err != nil {
		return resp, err
	}

	recorder := execCtx.Recorder()
	httpResp, err := e.doBedrockRequest(execCtx, mappedModel, false, body)
	if err != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bedrock executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	data = transformBedrockInvocationMetrics(data)
	recorder.AppendResponseChunk(data)
	reporter.publishWithContent(execCtx.Context, parseClaudeUsage(data), string(req.Payload), string(data))
	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, opts.OriginalRequest, bodyForTranslation, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *BedrockExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	mappedModel, ok := resolveBedrockModelID(auth, baseModel)
	if !ok {
		err = statusErr{code: http.StatusBadRequest, msg: "unsupported bedrock model: " + baseModel}
		return nil, err
	}

	to := sdktranslator.FromString("claude")
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: true,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	bodyForTranslation, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	bodyForTranslation, _ = sjson.SetBytes(bodyForTranslation, "model", execCtx.BaseModel)
	bodyForTranslation, err = thinking.ApplyThinking(bodyForTranslation, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	bodyForTranslation = execCtx.ApplyPayloadConfig(bodyForTranslation, originalTranslated)
	bodyForTranslation = disableThinkingIfToolChoiceForced(bodyForTranslation)
	betaTokens := resolveBedrockBetaTokens(opts.Headers.Get("anthropic-beta"), bodyForTranslation, mappedModel)
	body, err := prepareBedrockRequestBody(bodyForTranslation, mappedModel, betaTokens)
	if err != nil {
		return nil, err
	}

	recorder := execCtx.Recorder()
	httpResp, err := e.doBedrockRequest(execCtx, mappedModel, true, body) //nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	if err != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bedrock executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("bedrock executor: close response body error: %v", errClose)
			}
		}()

		decoder := newBedrockEventStreamDecoder(httpResp.Body)
		var param any
		for {
			payload, errDecode := decoder.Decode()
			if errDecode != nil {
				if errors.Is(errDecode, io.EOF) {
					reporter.ensurePublished(execCtx.Context)
					return
				}
				recorder.RecordResponseError(errDecode)
				reporter.publishFailure(execCtx.Context)
				out <- cliproxyexecutor.StreamChunk{Err: errDecode}
				return
			}
			recorder.AppendResponseChunk(payload)
			sseData := extractBedrockChunkData(payload)
			if len(sseData) == 0 {
				continue
			}
			sseData = transformBedrockInvocationMetrics(sseData)
			line := []byte("data: " + string(sseData))
			reporter.appendOutputChunk(line)
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			if execCtx.SourceFormat == to {
				eventType := gjson.GetBytes(sseData, "type").String()
				if eventType != "" {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte("event: " + eventType + "\n")}
				}
				out <- cliproxyexecutor.StreamChunk{Payload: append(line, '\n', '\n')}
				continue
			}
			chunks := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, opts.OriginalRequest, bodyForTranslation, line, &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *BedrockExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "bedrock count_tokens is not supported"}
}

func (e *BedrockExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	if auth == nil {
		return nil, fmt.Errorf("bedrock executor: auth is nil")
	}
	return auth, nil
}

func (e *BedrockExecutor) doBedrockRequest(execCtx *ExecutionContext, modelID string, stream bool, body []byte) (*http.Response, error) {
	region := bedrockRegion(execCtx.Auth)
	targetURL := buildBedrockURL(bedrockAttr(execCtx.Auth, "base_url"), region, modelID, stream)
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-bedrock")
	if err := e.prepareBedrockHTTPRequest(execCtx.Context, httpReq, execCtx.Auth, body); err != nil {
		return nil, err
	}
	recorder := execCtx.Recorder()
	recorder.RecordRequest(targetURL, http.MethodPost, httpReq.Header.Clone(), body)
	httpResp, err := execCtx.HTTPClient(0).Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		return nil, err
	}
	return httpResp, nil
}

func (e *BedrockExecutor) prepareBedrockHTTPRequest(ctx context.Context, req *http.Request, auth *cliproxyauth.Auth, body []byte) error {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if bedrockAuthMode(auth) == bedrockAuthModeAPIKey {
		apiKey := bedrockAttr(auth, "api_key")
		if apiKey == "" {
			return fmt.Errorf("bedrock executor: missing api key")
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		util.ApplyCustomHeadersFromAttrs(req, attrs)
		return nil
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	signer, err := newBedrockSignerFromAuth(auth)
	if err != nil {
		return err
	}
	return signer.signRequest(ctx, req, body)
}

type bedrockSigner struct {
	credentials aws.Credentials
	region      string
	signer      *v4.Signer
}

func newBedrockSignerFromAuth(auth *cliproxyauth.Auth) (*bedrockSigner, error) {
	accessKeyID := bedrockAttr(auth, "access_key_id")
	if accessKeyID == "" {
		accessKeyID = bedrockAttr(auth, "aws_access_key_id")
	}
	if accessKeyID == "" {
		return nil, fmt.Errorf("bedrock executor: missing aws access key id")
	}
	secretAccessKey := bedrockAttr(auth, "secret_access_key")
	if secretAccessKey == "" {
		secretAccessKey = bedrockAttr(auth, "aws_secret_access_key")
	}
	if secretAccessKey == "" {
		return nil, fmt.Errorf("bedrock executor: missing aws secret access key")
	}
	return &bedrockSigner{
		credentials: aws.Credentials{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SessionToken:    firstNonEmpty(bedrockAttr(auth, "session_token"), bedrockAttr(auth, "aws_session_token")),
		},
		region: bedrockRegion(auth),
		signer: v4.NewSigner(),
	}, nil
}

func (s *bedrockSigner) signRequest(ctx context.Context, req *http.Request, body []byte) error {
	payloadHash := sha256.Sum256(body)
	return s.signer.SignHTTP(ctx, s.credentials, req, hex.EncodeToString(payloadHash[:]), "bedrock", s.region, time.Now())
}

func resolveBedrockModelID(auth *cliproxyauth.Auth, requestedModel string) (string, bool) {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return "", false
	}
	if mapped := resolveBedrockCustomModelMapping(auth, requestedModel); mapped != "" {
		requestedModel = mapped
	}
	modelID, shouldAdjust, ok := normalizeBedrockModelID(requestedModel)
	if !ok {
		return "", false
	}
	if shouldAdjust {
		region := bedrockRegion(auth)
		if bedrockForceGlobal(auth) {
			region = "global"
		}
		modelID = adjustBedrockModelRegionPrefix(modelID, region)
	}
	return modelID, true
}

func resolveBedrockCustomModelMapping(auth *cliproxyauth.Auth, requestedModel string) string {
	raw := bedrockAttr(auth, "model_mapping")
	if strings.TrimSpace(raw) == "" {
		return requestedModel
	}
	mapped := gjson.Get(raw, requestedModel).String()
	if strings.TrimSpace(mapped) == "" {
		return requestedModel
	}
	return mapped
}

func normalizeBedrockModelID(modelID string) (string, bool, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", false, false
	}
	if mapped, exists := defaultBedrockModelMapping[modelID]; exists {
		return mapped, true, true
	}
	if isRegionalBedrockModelID(modelID) {
		return modelID, true, true
	}
	if isLikelyBedrockModelID(modelID) {
		return modelID, false, true
	}
	return "", false, false
}

func isRegionalBedrockModelID(modelID string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	for _, prefix := range bedrockCrossRegionPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isLikelyBedrockModelID(modelID string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	if strings.HasPrefix(lower, "arn:") {
		return true
	}
	for _, prefix := range []string{"anthropic.", "amazon.", "meta.", "mistral.", "cohere.", "ai21.", "deepseek.", "stability.", "writer.", "nova."} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func adjustBedrockModelRegionPrefix(modelID, region string) string {
	targetPrefix := bedrockCrossRegionPrefix(region)
	if strings.EqualFold(strings.TrimSpace(region), "global") {
		targetPrefix = "global"
	}
	for _, prefix := range bedrockCrossRegionPrefixes {
		if strings.HasPrefix(modelID, prefix) {
			if prefix == targetPrefix+"." {
				return modelID
			}
			return targetPrefix + "." + modelID[len(prefix):]
		}
	}
	return modelID
}

func bedrockCrossRegionPrefix(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	switch {
	case strings.HasPrefix(region, "us-gov"):
		return "us-gov"
	case strings.HasPrefix(region, "us-"):
		return "us"
	case strings.HasPrefix(region, "eu-"):
		return "eu"
	case region == "ap-northeast-1":
		return "jp"
	case region == "ap-southeast-2":
		return "au"
	case strings.HasPrefix(region, "ap-"):
		return "apac"
	case strings.HasPrefix(region, "ca-"), strings.HasPrefix(region, "sa-"):
		return "us"
	default:
		return "us"
	}
}

func buildBedrockURL(baseURL, region, modelID string, stream bool) string {
	if region == "" {
		region = defaultBedrockRegion
	}
	encodedModelID := url.PathEscape(modelID)
	encodedModelID = strings.ReplaceAll(encodedModelID, ":", "%3A")
	endpoint := "invoke"
	if stream {
		endpoint = "invoke-with-response-stream"
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
	}
	return fmt.Sprintf("%s/model/%s/%s", baseURL, encodedModelID, endpoint)
}

func prepareBedrockRequestBody(body []byte, modelID string, betaTokens []string) ([]byte, error) {
	var err error
	body, err = sjson.SetBytes(body, "anthropic_version", "bedrock-2023-05-31")
	if err != nil {
		return nil, fmt.Errorf("inject anthropic_version: %w", err)
	}
	if len(betaTokens) > 0 {
		body, err = sjson.SetBytes(body, "anthropic_beta", betaTokens)
		if err != nil {
			return nil, fmt.Errorf("inject anthropic_beta: %w", err)
		}
	}
	for _, path := range []string{"model", "stream", "output_config"} {
		body, err = sjson.DeleteBytes(body, path)
		if err != nil {
			return nil, fmt.Errorf("remove %s: %w", path, err)
		}
	}
	body = removeBedrockToolCustomFields(body)
	body = sanitizeBedrockCacheControl(body, modelID)
	return body, nil
}

func resolveBedrockBetaTokens(betaHeader string, body []byte, modelID string) []string {
	tokens := make([]string, 0)
	for _, part := range strings.Split(betaHeader, ",") {
		if token := strings.TrimSpace(part); token != "" {
			tokens = append(tokens, token)
		}
	}
	if gjson.GetBytes(body, "betas").IsArray() {
		gjson.GetBytes(body, "betas").ForEach(func(_, value gjson.Result) bool {
			if token := strings.TrimSpace(value.String()); token != "" {
				tokens = append(tokens, token)
			}
			return true
		})
	}
	if len(tokens) == 0 {
		return nil
	}
	_ = modelID
	allowed := map[string]bool{
		"fine-grained-tool-streaming-2025-05-14": true,
		"interleaved-thinking-2025-05-14":        true,
		"token-efficient-tools-2025-02-19":       true,
		"tool-search-tool-2025-10-19":            true,
	}
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		switch token {
		case "advanced-tool-use-2025-11-20":
			token = "tool-search-tool-2025-10-19"
		}
		if !allowed[token] {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	sort.Strings(out)
	return out
}

func removeBedrockToolCustomFields(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	tools.ForEach(func(key, _ gjson.Result) bool {
		body, _ = sjson.DeleteBytes(body, fmt.Sprintf("tools.%d.custom", key.Int()))
		return true
	})
	return body
}

func sanitizeBedrockCacheControl(body []byte, modelID string) []byte {
	_ = modelID
	paths := make([]string, 0)
	collectCacheControlPaths(gjson.GetBytes(body, "system"), "system", &paths)
	collectCacheControlPaths(gjson.GetBytes(body, "messages"), "messages", &paths)
	for _, path := range paths {
		body, _ = sjson.DeleteBytes(body, path+".scope")
		body, _ = sjson.DeleteBytes(body, path+".ttl")
	}
	return body
}

func collectCacheControlPaths(result gjson.Result, base string, paths *[]string) {
	if !result.Exists() {
		return
	}
	if result.IsObject() {
		if result.Get("cache_control").Exists() {
			*paths = append(*paths, base+".cache_control")
		}
		for key, value := range result.Map() {
			collectCacheControlPaths(value, base+"."+escapeGJSONPathSegment(key), paths)
		}
		return
	}
	if result.IsArray() {
		result.ForEach(func(key, value gjson.Result) bool {
			collectCacheControlPaths(value, fmt.Sprintf("%s.%d", base, key.Int()), paths)
			return true
		})
	}
}

func escapeGJSONPathSegment(segment string) string {
	if strings.ContainsAny(segment, ".#|") {
		return strings.ReplaceAll(segment, ".", "\\.")
	}
	return segment
}

func bedrockAuthMode(auth *cliproxyauth.Auth) string {
	mode := strings.ToLower(strings.TrimSpace(bedrockAttr(auth, "auth_mode")))
	switch mode {
	case "apikey", "api_key", "api-key":
		return bedrockAuthModeAPIKey
	default:
		return "sigv4"
	}
}

func bedrockRegion(auth *cliproxyauth.Auth) string {
	region := firstNonEmpty(bedrockAttr(auth, "region"), bedrockAttr(auth, "aws_region"))
	if region == "" {
		return defaultBedrockRegion
	}
	return region
}

func bedrockForceGlobal(auth *cliproxyauth.Auth) bool {
	return strings.EqualFold(bedrockAttr(auth, "force_global"), "true") ||
		strings.EqualFold(bedrockAttr(auth, "aws_force_global"), "true")
}

func bedrockAttr(auth *cliproxyauth.Auth, key string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes[key])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readAndResetRequestBody(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if errClose := req.Body.Close(); errClose != nil {
		return nil, errClose
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return body, nil
}
