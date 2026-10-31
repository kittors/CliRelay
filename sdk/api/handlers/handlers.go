// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

type StreamErrorMessage = interfaces.ErrorMessage

const idempotencyKeyMetadataKey = "idempotency_key"

const defaultStreamingKeepAliveSeconds = 15

type pinnedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type executionSessionContextKey struct{}

// ReadJSONRequestBody applies the shared request-body limit and writes a standard API error response.
func ReadJSONRequestBody(c *gin.Context) ([]byte, bool) {
	if c == nil {
		return nil, false
	}

	body, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
	if err == nil {
		return body, true
	}

	status := http.StatusBadRequest
	message := fmt.Sprintf("Invalid request: %v", err)
	errType := "invalid_request_error"
	code := ""
	if bodyutil.IsTooLarge(err) {
		status = http.StatusRequestEntityTooLarge
		message = "Request body too large"
		code = "request_body_too_large"
	} else if bodyutil.IsStorageUnavailable(err) {
		status = http.StatusInsufficientStorage
		message = "Request body temporary storage unavailable"
		errType = "server_error"
		code = "request_body_storage_unavailable"
	} else if bodyutil.IsTimeout(err) {
		status = http.StatusRequestTimeout
		message = "Request timed out while reading the request body"
		errType = "request_timeout"
		code = "request_timeout"
	}

	c.JSON(status, ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
	return nil, false
}

// WithPinnedAuthID returns a child context that requests execution on a specific auth ID.
func WithPinnedAuthID(ctx context.Context, authID string) context.Context {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedAuthContextKey{}, authID)
}

// WithSelectedAuthIDCallback returns a child context that receives the selected auth ID.
func WithSelectedAuthIDCallback(ctx context.Context, callback func(string)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthCallbackContextKey{}, callback)
}

// WithExecutionSessionID returns a child context tagged with a long-lived execution session ID.
func WithExecutionSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionSessionContextKey{}, sessionID)
}

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned as-is to preserve upstream error payloads.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	case http.StatusRequestTimeout:
		errType = "request_timeout"
		code = "request_timeout"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Default is 15s. Set keepalive-seconds to a negative value in config to disable.
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	if cfg != nil {
		if cfg.Streaming.KeepAliveSeconds < 0 {
			return 0
		}
		if cfg.Streaming.KeepAliveSeconds > 0 {
			return time.Duration(cfg.Streaming.KeepAliveSeconds) * time.Second
		}
	}
	return time.Duration(defaultStreamingKeepAliveSeconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// It is forwarded as execution metadata; when absent we generate a UUID.
	key := ""
	allowedChannels := ""
	allowedChannelGroups := ""
	routeGroup := ""
	routeFallback := ""
	sessionStickyKey := ""
	tenantID := ""
	if route := internalrouting.PathRouteContextFromContext(ctx); route != nil {
		routeGroup = strings.TrimSpace(route.Group)
		routeFallback = strings.TrimSpace(route.Fallback)
	}
	if ctx != nil {
		if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			if tenantValue, exists := ginCtx.Get("tenantID"); exists {
				tenantID, _ = tenantValue.(string)
				tenantID = strings.TrimSpace(tenantID)
			}
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			sessionStickyKey = requestSessionStickyHeaderKey(ginCtx)
			if metadataVal, exists := ginCtx.Get("accessMetadata"); exists {
				if metadata, okMeta := metadataVal.(map[string]string); okMeta {
					allowedChannels = strings.TrimSpace(metadata["allowed-channels"])
					allowedChannelGroups = strings.TrimSpace(metadata["allowed-channel-groups"])
				}
			}
			if routeVal, exists := ginCtx.Get(internalrouting.GinPathRouteContextKey); exists {
				if route, okRoute := routeVal.(*internalrouting.PathRouteContext); okRoute && route != nil {
					routeGroup = strings.TrimSpace(route.Group)
					routeFallback = strings.TrimSpace(route.Fallback)
				}
			}
			if (routeGroup == "" || routeFallback == "") && ginCtx.Request != nil {
				if route := internalrouting.PathRouteContextFromContext(ginCtx.Request.Context()); route != nil {
					if routeGroup == "" {
						routeGroup = strings.TrimSpace(route.Group)
					}
					if routeFallback == "" {
						routeFallback = strings.TrimSpace(route.Fallback)
					}
				}
			}
		}
	}
	if key == "" {
		key = uuid.NewString()
	}

	meta := map[string]any{idempotencyKeyMetadataKey: key}
	if tenantID != "" {
		meta[coreexecutor.TenantMetadataKey] = tenantID
	}
	if allowedChannels != "" {
		meta["allowed-channels"] = allowedChannels
	}
	if allowedChannelGroups != "" {
		meta["allowed-channel-groups"] = allowedChannelGroups
	}
	if routeGroup != "" {
		meta[coreexecutor.RouteGroupMetadataKey] = routeGroup
	}
	if routeFallback != "" {
		meta[coreexecutor.RouteFallbackMetadataKey] = routeFallback
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if sessionStickyKey != "" {
		meta[coreexecutor.SessionStickyMetadataKey] = sessionStickyKey
	}
	return meta
}

func pinnedAuthIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(pinnedAuthContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func selectedAuthIDCallbackFromContext(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthCallbackContextKey{})
	if callback, ok := raw.(func(string)); ok && callback != nil {
		return callback
	}
	return nil
}

func executionSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(executionSessionContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	return &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) { h.Cfg = cfg }

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

func requestContextFromGin(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		if requestCtx := c.Request.Context(); requestCtx != nil {
			return requestCtx
		}
	}
	return nil
}

func requestContextOrBackground(c *gin.Context) context.Context {
	if requestCtx := requestContextFromGin(c); requestCtx != nil {
		return requestCtx
	}
	return context.Background()
}

func requestNeedsWriteTimeoutBypass(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	req := c.Request
	if strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") {
		return true
	}
	accept := strings.ToLower(strings.TrimSpace(req.Header.Get("Accept")))
	if strings.Contains(accept, "text/event-stream") {
		return true
	}
	if alt, ok := c.GetQuery("alt"); ok && strings.EqualFold(strings.TrimSpace(alt), "sse") {
		return true
	}
	if alt, ok := c.GetQuery("$alt"); ok && strings.EqualFold(strings.TrimSpace(alt), "sse") {
		return true
	}
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	var request struct {
		Stream flexibleJSONBool `json:"stream"`
	}
	if err := bodyutil.DecodeJSONRequestBody(c, bodyutil.ModelRequestBodyLimit(), &request); err != nil {
		return false
	}
	return request.Stream.Bool()
}

type flexibleJSONBool bool

func (b *flexibleJSONBool) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(strings.ToLower(string(data)))
	value = strings.Trim(value, `"`)
	value = strings.TrimSpace(value)
	switch value {
	case "true", "1":
		*b = true
	default:
		*b = false
	}
	return nil
}

func (b flexibleJSONBool) Bool() bool {
	return bool(b)
}

func clearWriteDeadlineForLongLivedRequest(c *gin.Context) {
	if !requestNeedsWriteTimeoutBypass(c) || c == nil || c.Writer == nil {
		return
	}
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	requestCtx := requestContextFromGin(c)
	parentCtx := ctx
	if parentCtx == nil {
		if requestCtx != nil {
			parentCtx = requestCtx
		} else {
			parentCtx = context.Background()
		}
	}
	clearWriteDeadlineForLongLivedRequest(c)

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID := logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-newCtx.Done():
			}
		}()
	}
	newCtx = context.WithValue(newCtx, util.ContextKeyGin, c)
	return newCtx, func(params ...interface{}) {
		if h.Cfg.RequestLog && len(params) == 1 {
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existing, exists := c.Get("API_RESPONSE"); exists {
					if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
						trimmedPayload := bytes.TrimSpace(payload)
						if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
							cancel()
							return
						}
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive emits blank lines every 5 seconds while waiting for a non-streaming response.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.Cfg)
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = requestContextOrBackground(c)
	}

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
	}

	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+1)
			combined = append(combined, existingBytes...)
			if existingBytes[len(existingBytes)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, data...)
			c.Set("API_RESPONSE", combined)
			return
		}
	}

	c.Set("API_RESPONSE", bytes.Clone(data))
}

// executeModel runs a non-streaming request for one real model via the core
// auth manager (ExecuteWithAuthManager adds virtual models on top).
func (h *BaseAPIHandler) executeModel(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := executionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.Execute(ctx, providers, req, opts)
	if err != nil {
		return nil, nil, executionError(err)
	}
	if !PassthroughHeadersEnabled(h.Cfg) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

// executeCountModel runs a token count for one real model via the core auth
// manager (ExecuteCountWithAuthManager adds virtual models on top).
func (h *BaseAPIHandler) executeCountModel(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := executionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.ExecuteCount(ctx, providers, req, opts)
	if err != nil {
		return nil, nil, executionError(err)
	}
	if !PassthroughHeadersEnabled(h.Cfg) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

func validateSSEDataJSON(chunk []byte) error {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if json.Valid(data) {
			continue
		}
		const max = 512
		preview := data
		if len(preview) > max {
			preview = preview[:max]
		}
		return fmt.Errorf("invalid SSE data JSON (len=%d): %q", len(data), preview)
	}
	return nil
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

func scopedProvidersForModel(modelName string, groups []string) []string {
	registryRef := registry.GetGlobalRegistry()
	providers := make([]string, 0, 4)
	seen := make(map[string]struct{})
	appendProviders := func(candidates []string) {
		for _, provider := range candidates {
			provider = strings.TrimSpace(provider)
			if provider == "" {
				continue
			}
			if _, exists := seen[provider]; exists {
				continue
			}
			seen[provider] = struct{}{}
			providers = append(providers, provider)
		}
	}
	appendProviders(util.GetProviderName(modelName))
	if registryRef != nil {
		for _, group := range groups {
			group = internalrouting.NormalizeGroupName(group)
			if group == "" || group == "default" {
				continue
			}
			appendProviders(registryRef.GetModelProviders(group + "/" + modelName))
		}
	}
	return providers
}

// ginContextFrom returns the gin context GetContextWithCancel stored in ctx.
func ginContextFrom(ctx context.Context) *gin.Context {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	return ginCtx
}

func routeContextFromExecutionContext(ctx context.Context) *internalrouting.PathRouteContext {
	if ctx == nil {
		return nil
	}
	if route := internalrouting.PathRouteContextFromContext(ctx); route != nil {
		return route
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return nil
	}
	raw, exists := ginCtx.Get(internalrouting.GinPathRouteContextKey)
	if !exists {
		return nil
	}
	route, _ := raw.(*internalrouting.PathRouteContext)
	return route
}

func allowedChannelGroupsFromExecutionContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return nil
	}
	metadataVal, exists := ginCtx.Get("accessMetadata")
	if !exists {
		return nil
	}
	metadata, ok := metadataVal.(map[string]string)
	if !ok {
		return nil
	}
	set := internalrouting.ParseNormalizedSet(metadata["allowed-channel-groups"], internalrouting.NormalizeGroupName)
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for group := range set {
		out = append(out, group)
	}
	return out
}

func splitRequestedModelPrefix(modelName string) (string, string) {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return "", trimmed
	}
	return internalrouting.NormalizeGroupName(parts[0]), strings.TrimSpace(parts[1])
}

func (h *BaseAPIHandler) getRequestDetails(ctx context.Context, modelName string) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	var resolvedModelName string
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		} else {
			resolvedModelName = resolvedBase
		}
	} else {
		resolvedModelName = util.ResolveAutoModel(modelName)
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	routeCtx := routeContextFromExecutionContext(ctx)
	requestedPrefix, unprefixedModel := splitRequestedModelPrefix(baseModel)
	// Image models must use image endpoints instead of reaching text executors.
	if registry.IsImageGenerationModel(unprefixedModel) {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("model %q generates images; use /v1/images/generations or /v1/images/edits instead", modelName),
		}
	}
	if routeCtx != nil && routeCtx.Group != "" && requestedPrefix != "" && requestedPrefix != routeCtx.Group && !internalrouting.IsCcSwitchMappedTargetModel(routeCtx, baseModel) {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf(`{"error":{"message":"model prefix conflicts with route group","type":"invalid_request_error","code":"model_prefix_conflict"}}`),
		}
	}

	scopedGroups := allowedChannelGroupsFromExecutionContext(ctx)
	if routeCtx != nil && routeCtx.Group != "" {
		scopedGroups = append(scopedGroups, routeCtx.Group)
	}
	lookupModel := baseModel
	if routeCtx != nil && routeCtx.Group != "" && requestedPrefix == "" && routeCtx.Group != "default" && unprefixedModel != "" {
		lookupModel = unprefixedModel
	}

	providers = scopedProvidersForModel(lookupModel, scopedGroups)
	// Preserve support for custom model registrations that include thinking
	// suffixes, evaluated in Story 11.8.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = scopedProvidersForModel(resolvedModelName, scopedGroups)
	}

	if len(providers) == 0 {
		return nil, "", &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("unknown provider for model %s", modelName)}
	}

	// Strip context-window markers while preserving round-bracket thinking
	// suffixes for existing thinking config.
	if parsed.HasSuffix {
		resolvedModelName = parsed.ModelName + "(" + parsed.RawSuffix + ")"
	} else {
		resolvedModelName = parsed.ModelName
	}

	return providers, resolvedModelName, nil
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

// WriteErrorResponse writes msg with the HTTP status it carries. An upstream
// JSON error body is passed through as it is and anything else becomes an
// OpenAI error; handlers that know their client's protocol use
// WriteErrorResponseAs instead.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	h.writeErrorResponse(c, msg, func(status int, errText string) (int, []byte) {
		return status, BuildErrorResponseBody(status, errText)
	})
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if h.Cfg.RequestLog {
		if ginContext, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
