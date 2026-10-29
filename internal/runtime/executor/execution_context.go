package executor

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// ExecutionOptions captures executor-pipeline choices that should stay stable
// across provider implementations, such as target schema and translation mode.
type ExecutionOptions struct {
	TargetFormat        sdktranslator.Format
	TranslateAsStream   bool
	PayloadConfigRoot   string
	PayloadConfigFormat string
}

// HTTPClientFactory centralizes proxy-aware HTTP client construction so
// providers can share the same transport selection behavior.
type HTTPClientFactory struct {
	cfg *config.Config
}

func newHTTPClientFactory(cfg *config.Config) HTTPClientFactory {
	return HTTPClientFactory{cfg: cfg}
}

func (f HTTPClientFactory) New(ctx context.Context, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	return newProxyAwareHTTPClient(ctx, f.cfg, auth, timeout)
}

// UpstreamRecorder centralizes upstream request/response capture for
// management request logs while preserving auth metadata.
type UpstreamRecorder struct {
	ctx       context.Context
	cfg       *config.Config
	provider  string
	authID    string
	authLabel string
	authType  string
	authValue string
}

func newUpstreamRecorder(ctx context.Context, cfg *config.Config, provider string, auth *cliproxyauth.Auth) UpstreamRecorder {
	recorder := UpstreamRecorder{
		ctx:      ctx,
		cfg:      cfg,
		provider: provider,
	}
	if auth != nil {
		recorder.authID = auth.ID
		recorder.authLabel = auth.Label
		recorder.authType, recorder.authValue = auth.AccountInfo()
	}
	return recorder
}

func (r UpstreamRecorder) RecordRequest(url, method string, headers http.Header, body []byte) {
	recordAPIRequest(r.ctx, r.cfg, upstreamRequestLog{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		Provider:  r.provider,
		AuthID:    r.authID,
		AuthLabel: r.authLabel,
		AuthType:  r.authType,
		AuthValue: r.authValue,
	})
}

func (r UpstreamRecorder) RecordResponseMetadata(status int, headers http.Header) {
	recordAPIResponseMetadata(r.ctx, r.cfg, status, headers)
}

func (r UpstreamRecorder) RecordResponseError(err error) {
	recordAPIResponseError(r.ctx, r.cfg, err)
}

func (r UpstreamRecorder) AppendResponseChunk(chunk []byte) {
	appendAPIResponseChunk(r.ctx, r.cfg, chunk)
}

// ExecutionContext carries provider-agnostic request execution state so
// providers can share stable helpers without changing external behavior.
type ExecutionContext struct {
	Context         context.Context
	Provider        string
	Config          *config.Config
	Auth            *cliproxyauth.Auth
	Request         cliproxyexecutor.Request
	Options         cliproxyexecutor.Options
	Execution       ExecutionOptions
	BaseModel       string
	RequestedModel  string
	OriginalPayload []byte
	SourceFormat    sdktranslator.Format

	// originalPayloadIsRequest records the exact no-override case so the request
	// translator is not run twice over the same potentially multi-megabyte body.
	originalPayloadIsRequest bool

	clientFactory HTTPClientFactory
	recorder      UpstreamRecorder
}

type visionFallbackLogContextKey struct{}

type visionFallbackLogInfo struct {
	RequestedModel string
	UpstreamModel  string
	FallbackModel  string
}

func contextWithVisionFallbackLog(ctx context.Context, requestedModel, upstreamModel, fallbackModel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	info := visionFallbackLogInfo{
		RequestedModel: strings.TrimSpace(requestedModel),
		UpstreamModel:  strings.TrimSpace(upstreamModel),
		FallbackModel:  strings.TrimSpace(fallbackModel),
	}
	if info.FallbackModel == "" {
		return ctx
	}
	return context.WithValue(ctx, visionFallbackLogContextKey{}, info)
}

func visionFallbackLogFromContext(ctx context.Context) visionFallbackLogInfo {
	if ctx == nil {
		return visionFallbackLogInfo{}
	}
	info, _ := ctx.Value(visionFallbackLogContextKey{}).(visionFallbackLogInfo)
	return info
}

func newExecutionContext(
	ctx context.Context,
	provider string,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	execOpts ExecutionOptions,
) *ExecutionContext {
	if ctx == nil {
		ctx = context.Background()
	}
	originalPayload := req.Payload
	// Production handlers set OriginalRequest to the same slice as Payload.
	// Treat shared storage as "same request" so TranslateRequestPair only runs once.
	originalPayloadIsRequest := len(opts.OriginalRequest) == 0 || samePayloadStorage(opts.OriginalRequest, req.Payload)
	if !originalPayloadIsRequest {
		originalPayload = opts.OriginalRequest
	}
	return &ExecutionContext{
		Context:                  ctx,
		Provider:                 provider,
		Config:                   cfg,
		Auth:                     auth,
		Request:                  req,
		Options:                  opts,
		Execution:                execOpts,
		BaseModel:                thinking.ParseSuffix(req.Model).ModelName,
		RequestedModel:           payloadRequestedModel(opts, req.Model),
		OriginalPayload:          originalPayload,
		SourceFormat:             opts.SourceFormat,
		originalPayloadIsRequest: originalPayloadIsRequest,
		clientFactory:            newHTTPClientFactory(cfg),
		recorder:                 newUpstreamRecorder(ctx, cfg, provider, auth),
	}
}

// ClientRequestedStream reports whether the caller asked for a streamed
// response, which is what usage records classify by.
//
// The upstream call shape is not a usable proxy for it: Antigravity serves
// every non-streaming request from :streamGenerateContent, so the executor is
// reading SSE either way. Both payload views are consulted because
// OriginalPayload only differs from Request.Payload when the handler supplied a
// pre-translation body, and whichever view is the client's own is the one still
// carrying the top-level "stream" flag.
func (ec *ExecutionContext) ClientRequestedStream() bool {
	if ec == nil {
		return false
	}
	return isStreamingUsageRequestBytes(ec.OriginalPayload) || isStreamingUsageRequestBytes(ec.Request.Payload)
}

func (ec *ExecutionContext) Reporter() *usageReporter {
	if ec == nil {
		return nil
	}
	parsedModel := thinking.ParseSuffix(ec.RequestedModel)
	model := parsedModel.ModelName
	if strings.TrimSpace(model) == "" {
		model = ec.BaseModel
	}
	reporter := newUsageReporter(ec.Context, ec.Provider, model, ec.BaseModel, ec.Auth, ec.ClientRequestedStream())
	reporter.setThinkingLevel(ec.RequestedThinkingLevel())
	if fallback := visionFallbackLogFromContext(ec.Context); fallback.FallbackModel != "" {
		fallbackModel := thinking.ParseSuffix(fallback.RequestedModel)
		reporter.setModel(fallbackModel.ModelName)
		if fallbackModel.HasSuffix {
			reporter.setThinkingLevel(fallbackModel.RawSuffix)
		}
		reporter.setUpstreamModel(fallback.UpstreamModel)
		reporter.setVisionFallbackModel(fallback.FallbackModel)
	}
	return reporter
}

// RequestedThinkingLevel is the reasoning level the client asked for: the model
// suffix when there is one, otherwise the level or numeric budget the original
// request body carries, read in the client's own format.
func (ec *ExecutionContext) RequestedThinkingLevel() string {
	if ec == nil {
		return ""
	}
	if parsed := thinking.ParseSuffix(ec.RequestedModel); parsed.HasSuffix {
		return parsed.RawSuffix
	}
	return thinking.ExtractThinkingLevel(ec.OriginalPayload, ec.SourceFormat.String())
}

func (ec *ExecutionContext) HTTPClient(timeout time.Duration) *http.Client {
	if ec == nil {
		return newProxyAwareHTTPClient(context.Background(), nil, nil, timeout)
	}
	return ec.clientFactory.New(ec.Context, ec.Auth, timeout)
}

func (ec *ExecutionContext) Recorder() UpstreamRecorder {
	if ec == nil {
		return UpstreamRecorder{}
	}
	return ec.recorder
}

func (ec *ExecutionContext) TranslateRequestPair(payload []byte) ([]byte, []byte) {
	if ec == nil {
		return payload, payload
	}
	if payload == nil {
		payload = ec.Request.Payload
	}
	translated := sdktranslator.TranslateRequest(
		ec.SourceFormat,
		ec.Execution.TargetFormat,
		ec.BaseModel,
		payload,
		ec.Execution.TranslateAsStream,
	)
	// Skip second translate when original is the same request (empty override,
	// shared storage with req.Payload, or payload already is OriginalPayload).
	if ec.originalPayloadIsRequest ||
		samePayloadStorage(payload, ec.OriginalPayload) ||
		samePayloadStorage(payload, ec.Request.Payload) && samePayloadStorage(ec.OriginalPayload, ec.Request.Payload) {
		return translated, translated
	}
	originalTranslated := sdktranslator.TranslateRequest(
		ec.SourceFormat,
		ec.Execution.TargetFormat,
		ec.BaseModel,
		ec.OriginalPayload,
		ec.Execution.TranslateAsStream,
	)
	return translated, originalTranslated
}

func samePayloadStorage(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return true
	}
	return &left[0] == &right[0]
}

func (ec *ExecutionContext) ApplyPayloadConfig(payload, originalTranslated []byte) []byte {
	if ec == nil {
		return payload
	}
	protocol := strings.TrimSpace(ec.Execution.PayloadConfigFormat)
	if protocol == "" {
		protocol = ec.Execution.TargetFormat.String()
	}
	return applyPayloadConfigWithRoot(
		ec.Config,
		ec.BaseModel,
		protocol,
		ec.Execution.PayloadConfigRoot,
		payload,
		originalTranslated,
		ec.RequestedModel,
	)
}
