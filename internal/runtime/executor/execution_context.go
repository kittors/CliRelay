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
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	return &ExecutionContext{
		Context:         ctx,
		Provider:        provider,
		Config:          cfg,
		Auth:            auth,
		Request:         req,
		Options:         opts,
		Execution:       execOpts,
		BaseModel:       thinking.ParseSuffix(req.Model).ModelName,
		RequestedModel:  payloadRequestedModel(opts, req.Model),
		OriginalPayload: originalPayload,
		SourceFormat:    opts.SourceFormat,
		clientFactory:   newHTTPClientFactory(cfg),
		recorder:        newUpstreamRecorder(ctx, cfg, provider, auth),
	}
}

func (ec *ExecutionContext) Reporter() *usageReporter {
	if ec == nil {
		return nil
	}
	model := thinking.ParseSuffix(ec.RequestedModel).ModelName
	if strings.TrimSpace(model) == "" {
		model = ec.BaseModel
	}
	reporter := newUsageReporter(ec.Context, ec.Provider, model, ec.BaseModel, ec.Auth)
	if fallback := visionFallbackLogFromContext(ec.Context); fallback.FallbackModel != "" {
		reporter.setModel(fallback.RequestedModel)
		reporter.setUpstreamModel(fallback.UpstreamModel)
		reporter.setVisionFallbackModel(fallback.FallbackModel)
	}
	return reporter
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
	originalTranslated := sdktranslator.TranslateRequest(
		ec.SourceFormat,
		ec.Execution.TargetFormat,
		ec.BaseModel,
		ec.OriginalPayload,
		ec.Execution.TranslateAsStream,
	)
	return translated, originalTranslated
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
