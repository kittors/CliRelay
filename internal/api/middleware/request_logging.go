// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const maxErrorOnlyCapturedRequestBodyBytes int64 = 1 << 20 // 1 MiB

const requestLoggingWrappedGinKey = "cliproxy.request_logging_wrapped"

type requestLoggingWrappedContextKey struct{}

// RequestLoggingMiddleware creates a Gin middleware that logs HTTP requests and responses.
// It captures detailed information about the request and response, including headers and body,
// and uses the provided RequestLogger to record this data. When full request logging is disabled,
// body capture is limited to small known-size payloads to avoid large per-request memory spikes.
func RequestLoggingMiddleware(logger logging.RequestLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logger == nil {
			c.Next()
			return
		}

		if shouldSkipMethodForRequestLogging(c.Request) {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			c.Next()
			return
		}

		requestID := ensureRequestLoggingRequestID(c)
		diagnostic := diagnostics.EnsureGin(c, requestID)
		if diagnostic != nil {
			diagnostic.UpdateRequest(c.Request)
		}

		if wrapper := requestLoggingExistingWrapper(c); wrapper != nil {
			c.Writer = wrapper
			c.Next()
			return
		}
		if requestLoggingAlreadyWrapped(c) {
			c.Next()
			return
		}

		loggerEnabled := logger.IsEnabled()

		// Capture request information
		requestInfo, err := captureRequestInfo(c, shouldCaptureRequestBody(loggerEnabled, c.Request))
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			// Log error but continue processing
			// In a real implementation, you might want to use a proper logger here
			c.Next()
			return
		}

		// Create response writer wrapper
		wrapper := NewResponseWriterWrapper(c.Writer, logger, requestInfo, c)
		if !loggerEnabled {
			wrapper.logOnErrorOnly = true
		}
		c.Writer = wrapper
		markRequestLoggingWrapped(c, wrapper)

		// Process the request
		c.Next()

		// Finalize logging after request processing
		if err = wrapper.Finalize(c); err != nil {
			// Log error but don't interrupt the response
			_ = c.Error(err)
		}
	}
}

func requestLoggingAlreadyWrapped(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if _, exists := c.Get(requestLoggingWrappedGinKey); exists {
		return true
	}
	if c.Request == nil {
		return false
	}
	return c.Request.Context().Value(requestLoggingWrappedContextKey{}) != nil
}

func requestLoggingExistingWrapper(c *gin.Context) *ResponseWriterWrapper {
	if c == nil {
		return nil
	}
	if raw, exists := c.Get(requestLoggingWrappedGinKey); exists {
		if wrapper, ok := raw.(*ResponseWriterWrapper); ok {
			return wrapper
		}
	}
	if c.Request == nil {
		return nil
	}
	wrapper, _ := c.Request.Context().Value(requestLoggingWrappedContextKey{}).(*ResponseWriterWrapper)
	return wrapper
}

func markRequestLoggingWrapped(c *gin.Context, wrapper *ResponseWriterWrapper) {
	if c == nil {
		return
	}
	c.Set(requestLoggingWrappedGinKey, wrapper)
	if c.Request != nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestLoggingWrappedContextKey{}, wrapper))
	}
}

func ensureRequestLoggingRequestID(c *gin.Context) string {
	if existing := logging.GetGinRequestID(c); existing != "" {
		return existing
	}
	requestID := logging.GenerateRequestID()
	logging.SetGinRequestID(c, requestID)
	if c != nil && c.Request != nil {
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), requestID))
	}
	return requestID
}

func shouldSkipMethodForRequestLogging(req *http.Request) bool {
	if req == nil {
		return true
	}
	if req.Method != http.MethodGet {
		return false
	}
	return !isResponsesWebsocketUpgrade(req)
}

func isResponsesWebsocketUpgrade(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if req.URL.Path != "/v1/responses" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

func shouldCaptureRequestBody(loggerEnabled bool, req *http.Request) bool {
	if req == nil || req.Body == nil {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return false
	}
	if req.ContentLength <= 0 {
		return false
	}
	if loggerEnabled && req.ContentLength > maxErrorOnlyCapturedRequestBodyBytes {
		return false
	}
	return req.ContentLength <= maxErrorOnlyCapturedRequestBodyBytes
}

// captureRequestInfo extracts relevant information from the incoming HTTP request.
// It captures the URL, method, headers, and body. The request body is read and then
// restored so that it can be processed by subsequent handlers.
func captureRequestInfo(c *gin.Context, captureBody bool) (*RequestInfo, error) {
	// Capture URL with sensitive query parameters masked
	maskedQuery := util.MaskSensitiveQuery(c.Request.URL.RawQuery)
	url := c.Request.URL.Path
	if maskedQuery != "" {
		url += "?" + maskedQuery
	}
	if diagnostic := diagnostics.FromGin(c); diagnostic != nil {
		if snapshot := diagnostic.Snapshot(); snapshot.OriginalURL != "" {
			url = snapshot.OriginalURL
		}
	}

	// Capture method
	method := c.Request.Method

	// Capture headers
	headers := make(map[string][]string)
	for key, values := range c.Request.Header {
		headers[key] = values
	}

	// Capture request body
	var body []byte
	if captureBody && c.Request.Body != nil {
		bodyBytes, err := bodyutil.ReadRequestBody(c, bodyutil.DefaultRequestBodyLimit)
		if err != nil {
			return nil, err
		}
		body = bodyBytes
	}

	return &RequestInfo{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		RequestID: logging.GetGinRequestID(c),
		Timestamp: time.Now(),
	}, nil
}

// shouldLogRequest determines whether the request should be logged.
// It skips management endpoints to avoid leaking secrets but allows
// all other routes, including module-provided ones, to honor request-log.
func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	if strings.HasPrefix(path, "/api") {
		return strings.HasPrefix(path, "/api/provider")
	}

	return true
}
