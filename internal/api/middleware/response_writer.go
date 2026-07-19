// Package middleware provides Gin HTTP middleware for the CLI Proxy API server.
// It includes a sophisticated response writer wrapper designed to capture and log request and response data,
// including support for streaming responses, without impacting latency.
package middleware

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const requestBodyOverrideContextKey = "REQUEST_BODY_OVERRIDE"

// RequestInfo holds essential details of an incoming HTTP request for logging purposes.
type RequestInfo struct {
	URL       string              // URL is the request URL.
	Method    string              // Method is the HTTP method (e.g., GET, POST).
	Headers   map[string][]string // Headers contains the request headers.
	Body      []byte              // Body is the raw request body.
	RequestID string              // RequestID is the unique identifier for the request.
	Timestamp time.Time           // Timestamp is when the request was received.
}

// ResponseWriterWrapper wraps the standard gin.ResponseWriter to intercept and log response data.
// It is designed to handle both standard and streaming responses, ensuring that logging operations do not block the client response.
type ResponseWriterWrapper struct {
	gin.ResponseWriter
	body                *bytes.Buffer              // body is a buffer to store the response body for non-streaming responses.
	isStreaming         bool                       // isStreaming indicates whether the response is a streaming type (e.g., text/event-stream).
	streamWriter        logging.StreamingLogWriter // streamWriter is a writer for handling streaming log entries.
	chunkChannel        chan []byte                // chunkChannel is a channel for asynchronously passing response chunks to the logger.
	streamDone          chan struct{}              // streamDone signals when the streaming goroutine completes.
	logger              logging.RequestLogger      // logger is the instance of the request logger service.
	requestInfo         *RequestInfo               // requestInfo holds the details of the original request.
	ginCtx              *gin.Context               // ginCtx allows propagating first-response timing into usage records.
	statusCode          int                        // statusCode stores the HTTP status code of the response.
	headers             map[string][]string        // headers stores the response headers.
	headersCaptured     bool                       // headersCaptured avoids cloning immutable response headers per stream chunk.
	logOnErrorOnly      bool                       // logOnErrorOnly enables logging only when an error response is detected.
	firstChunkTimestamp time.Time                  // firstChunkTimestamp captures TTFB for streaming responses.
}

// NewResponseWriterWrapper creates and initializes a new ResponseWriterWrapper.
// It takes the original gin.ResponseWriter, a logger instance, and request information.
//
// Parameters:
//   - w: The original gin.ResponseWriter to wrap.
//   - logger: The logging service to use for recording requests.
//   - requestInfo: The pre-captured information about the incoming request.
//
// Returns:
//   - A pointer to a new ResponseWriterWrapper.
func NewResponseWriterWrapper(w gin.ResponseWriter, logger logging.RequestLogger, requestInfo *RequestInfo, ginCtx *gin.Context) *ResponseWriterWrapper {
	return &ResponseWriterWrapper{
		ResponseWriter: w,
		body:           &bytes.Buffer{},
		logger:         logger,
		requestInfo:    requestInfo,
		ginCtx:         ginCtx,
		headers:        make(map[string][]string),
	}
}

// Unwrap lets net/http.ResponseController reach the real connection through this logging wrapper.
func (w *ResponseWriterWrapper) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Write wraps the underlying ResponseWriter's Write method to capture response data.
// For non-streaming responses, it writes to an internal buffer. For streaming responses,
// it sends data chunks to a non-blocking channel for asynchronous logging.
// CRITICAL: This method prioritizes writing to the client to ensure zero latency,
// handling logging operations subsequently.
func (w *ResponseWriterWrapper) Write(data []byte) (int, error) {
	loggingEnabled := w.loggingEnabled()
	if loggingEnabled {
		w.ensureHeadersCaptured()
	}

	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.markFirstResponseWrite()
	}

	if loggingEnabled && w.isStreaming && w.chunkChannel != nil {
		select {
		case w.chunkChannel <- append([]byte(nil), data...):
		default:
		}
		return n, err
	}

	if w.shouldBufferResponseBody() {
		if !loggingEnabled {
			w.ensureHeadersCaptured()
		}
		w.body.Write(data)
	}
	return n, err
}

func (w *ResponseWriterWrapper) shouldBufferResponseBody() bool {
	if w.logger != nil && w.logger.IsEnabled() {
		return true
	}
	if !w.logOnErrorOnly {
		return false
	}
	status := w.statusCode
	if status == 0 {
		if statusWriter, ok := w.ResponseWriter.(interface{ Status() int }); ok && statusWriter != nil {
			status = statusWriter.Status()
		} else {
			status = http.StatusOK
		}
	}
	return status >= http.StatusBadRequest
}

// WriteString wraps the underlying ResponseWriter's WriteString method to capture response data.
// Some handlers (and fmt/io helpers) write via io.StringWriter; without this override, those writes
// bypass Write() and would be missing from request logs.
func (w *ResponseWriterWrapper) WriteString(data string) (int, error) {
	loggingEnabled := w.loggingEnabled()
	if loggingEnabled {
		w.ensureHeadersCaptured()
	}

	n, err := w.ResponseWriter.WriteString(data)
	if n > 0 {
		w.markFirstResponseWrite()
	}

	if loggingEnabled && w.isStreaming && w.chunkChannel != nil {
		select {
		case w.chunkChannel <- []byte(data):
		default:
		}
		return n, err
	}

	if w.shouldBufferResponseBody() {
		if !loggingEnabled {
			w.ensureHeadersCaptured()
		}
		w.body.WriteString(data)
	}
	return n, err
}

// WriteHeader wraps the underlying ResponseWriter's WriteHeader method.
// It captures the status code, detects if the response is streaming based on the Content-Type header,
// and initializes the appropriate logging mechanism (standard or streaming).
func (w *ResponseWriterWrapper) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	loggingEnabled := w.loggingEnabled()
	captureError := w.logOnErrorOnly && statusCode >= http.StatusBadRequest
	w.closeStreamingLogWriter()
	if loggingEnabled || captureError {
		w.ensureHeadersCaptured()
		w.hydrateRequestInfoBody(w.ginCtx)
		if captureError {
			w.hydrateRequestInfoHeaders(w.ginCtx)
		}

		contentType := w.ResponseWriter.Header().Get("Content-Type")
		w.isStreaming = loggingEnabled && w.detectStreaming(contentType)

		if w.isStreaming {
			requestBody := w.extractRequestBody(w.ginCtx)
			streamWriter, err := w.logger.LogStreamingRequest(
				w.requestInfo.URL,
				w.requestInfo.Method,
				w.requestInfo.Headers,
				requestBody,
				w.requestInfo.RequestID,
			)
			if err == nil {
				w.streamWriter = streamWriter
				w.chunkChannel = make(chan []byte, 100)
				doneChan := make(chan struct{})
				w.streamDone = doneChan
				go w.processStreamingChunks(doneChan, streamWriter, w.chunkChannel)
				_ = streamWriter.WriteStatus(statusCode, w.headers)
			}
		}
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

// ensureHeadersCaptured is a helper function to make sure response headers are captured.
// It is safe to call this method multiple times; it will always refresh the headers
// with the latest state from the underlying ResponseWriter.
func (w *ResponseWriterWrapper) ensureHeadersCaptured() {
	if w == nil || w.headersCaptured {
		return
	}
	w.captureCurrentHeaders()
}

func (w *ResponseWriterWrapper) loggingEnabled() bool {
	return w != nil && w.logger != nil && w.logger.IsEnabled()
}

// captureCurrentHeaders reads all headers from the underlying ResponseWriter and stores them
// in the wrapper's headers map. It creates copies of the header values to prevent race conditions.
func (w *ResponseWriterWrapper) captureCurrentHeaders() {
	// Initialize headers map if needed
	if w.headers == nil {
		w.headers = make(map[string][]string)
	}

	// Capture all current headers from the underlying ResponseWriter
	for key, values := range w.ResponseWriter.Header() {
		w.headers[key] = append([]string(nil), values...)
	}
	w.headersCaptured = true
}

func (w *ResponseWriterWrapper) markFirstResponseWrite() {
	if !w.firstChunkTimestamp.IsZero() {
		return
	}
	timestamp := time.Now()
	w.firstChunkTimestamp = timestamp
	if w.ginCtx != nil {
		w.ginCtx.Set(util.GinKeyFirstResponseAt, timestamp)
	}
}

// detectStreaming determines if a response should be treated as a streaming response.
// It checks for a "text/event-stream" Content-Type or a '"stream": true'
// field in the original request body.
func (w *ResponseWriterWrapper) detectStreaming(contentType string) bool {
	// Check Content-Type for Server-Sent Events
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	// If a concrete Content-Type is already set (e.g., application/json for error responses),
	// treat it as non-streaming instead of inferring from the request payload.
	if strings.TrimSpace(contentType) != "" {
		return false
	}

	// Only fall back to request payload hints when Content-Type is not set yet.
	if w.requestInfo != nil && len(w.requestInfo.Body) > 0 {
		return bytes.Contains(w.requestInfo.Body, []byte(`"stream": true`)) ||
			bytes.Contains(w.requestInfo.Body, []byte(`"stream":true`))
	}

	return false
}

// processStreamingChunks runs in a separate goroutine to process response chunks from the chunkChannel.
// It asynchronously writes each chunk to the streaming log writer.
func (w *ResponseWriterWrapper) processStreamingChunks(done chan struct{}, streamWriter logging.StreamingLogWriter, chunkChannel <-chan []byte) {
	if done == nil {
		return
	}

	defer close(done)

	if streamWriter == nil || chunkChannel == nil {
		return
	}

	for chunk := range chunkChannel {
		streamWriter.WriteChunkAsync(chunk)
	}
}

// Finalize completes the logging process for the request and response.
// For streaming responses, it closes the chunk channel and the stream writer.
// For non-streaming responses, it logs the complete request and response details,
// including any API-specific request/response data stored in the Gin context.
func (w *ResponseWriterWrapper) Finalize(c *gin.Context) error {
	if w.logger == nil {
		return nil
	}

	finalStatusCode := w.statusCode
	if finalStatusCode == 0 {
		if statusWriter, ok := w.ResponseWriter.(interface{ Status() int }); ok {
			finalStatusCode = statusWriter.Status()
		} else {
			finalStatusCode = 200
		}
	}

	var slicesAPIResponseError []*interfaces.ErrorMessage
	apiResponseError, isExist := c.Get("API_RESPONSE_ERROR")
	if isExist {
		if apiErrors, ok := apiResponseError.([]*interfaces.ErrorMessage); ok {
			slicesAPIResponseError = apiErrors
		}
	}

	hasAPIError := len(slicesAPIResponseError) > 0 || finalStatusCode >= http.StatusBadRequest
	diagnostics.RecordResponse(c, finalStatusCode, w.body.Bytes())
	forceLog := w.logOnErrorOnly && hasAPIError && !w.logger.IsEnabled()
	if forceLog {
		w.hydrateRequestInfoHeaders(c)
		w.hydrateRequestInfoBody(c)
		w.ensureHeadersCaptured()
	}
	// Stream writers create request-body-*.tmp / response-body-*.tmp as soon as
	// WriteHeader runs. Always close them even when request-log is later disabled
	// mid-request, otherwise the logs dir fills with orphaned spool files that
	// historically bypassed logs-max-total-size-mb.
	if w.streamWriter != nil {
		return w.finalizeStreamingLog(c, forceLog)
	}
	if !w.logger.IsEnabled() && !forceLog {
		return nil
	}

	return w.logRequest(w.extractRequestBody(c), finalStatusCode, w.cloneHeaders(), w.body.Bytes(), w.extractAPIRequest(c), w.extractAPIResponse(c), w.extractAPIResponseTimestamp(c), slicesAPIResponseError, forceLog)
}

func (w *ResponseWriterWrapper) finalizeStreamingLog(c *gin.Context, forceLog bool) error {
	writer := w.streamWriter
	if writer == nil {
		w.closeStreamingLogWriter()
		return nil
	}

	// Drop spool temps without writing a final request log when logging is off.
	if !w.logger.IsEnabled() && !forceLog {
		w.closeStreamingLogWriter()
		return nil
	}

	if w.chunkChannel != nil {
		close(w.chunkChannel)
		w.chunkChannel = nil
	}
	if w.streamDone != nil {
		<-w.streamDone
		w.streamDone = nil
	}

	w.streamWriter = nil
	writer.SetFirstChunkTimestamp(w.firstChunkTimestamp)

	apiRequest := w.extractAPIRequest(c)
	if len(apiRequest) > 0 {
		_ = writer.WriteAPIRequest(apiRequest)
	}
	apiResponse := w.extractAPIResponse(c)
	if len(apiResponse) > 0 {
		_ = writer.WriteAPIResponse(apiResponse)
	}
	return writer.Close()
}

func (w *ResponseWriterWrapper) closeStreamingLogWriter() {
	if w == nil {
		return
	}
	if w.chunkChannel != nil {
		close(w.chunkChannel)
		w.chunkChannel = nil
	}
	if w.streamDone != nil {
		<-w.streamDone
		w.streamDone = nil
	}
	if w.streamWriter != nil {
		_ = w.streamWriter.Close()
		w.streamWriter = nil
	}
}

func (w *ResponseWriterWrapper) cloneHeaders() map[string][]string {
	w.ensureHeadersCaptured()

	finalHeaders := make(map[string][]string, len(w.headers))
	for key, values := range w.headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		finalHeaders[key] = headerValues
	}

	return finalHeaders
}

func (w *ResponseWriterWrapper) extractAPIRequest(c *gin.Context) []byte {
	return logging.APIRequestSnapshot(c)
}

func (w *ResponseWriterWrapper) extractAPIResponse(c *gin.Context) []byte {
	return logging.APIResponseSnapshot(c)
}

func (w *ResponseWriterWrapper) extractAPIResponseTimestamp(c *gin.Context) time.Time {
	ts, isExist := c.Get("API_RESPONSE_TIMESTAMP")
	if !isExist {
		return time.Time{}
	}
	if t, ok := ts.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func (w *ResponseWriterWrapper) extractRequestBody(c *gin.Context) []byte {
	if c != nil {
		if bodyOverride, isExist := c.Get(requestBodyOverrideContextKey); isExist {
			switch value := bodyOverride.(type) {
			case []byte:
				if len(value) > 0 {
					return bytes.Clone(value)
				}
			case string:
				if strings.TrimSpace(value) != "" {
					return []byte(value)
				}
			}
		}
	}
	if body := w.hydrateRequestInfoBody(c); len(body) > 0 {
		return body
	}
	if w.requestInfo != nil && len(w.requestInfo.Body) > 0 {
		return w.requestInfo.Body
	}
	return nil
}

func (w *ResponseWriterWrapper) hydrateRequestInfoHeaders(c *gin.Context) map[string][]string {
	if w == nil || w.requestInfo == nil {
		return nil
	}
	if w.requestInfo.Headers != nil {
		return w.requestInfo.Headers
	}
	if c == nil || c.Request == nil {
		w.requestInfo.Headers = map[string][]string{}
		return w.requestInfo.Headers
	}
	w.requestInfo.Headers = cloneHTTPHeader(c.Request.Header)
	return w.requestInfo.Headers
}

func (w *ResponseWriterWrapper) hydrateRequestInfoBody(c *gin.Context) []byte {
	if w == nil || w.requestInfo == nil {
		return nil
	}
	if len(w.requestInfo.Body) > 0 {
		return w.requestInfo.Body
	}
	if body, ok := bodyutil.CachedRequestBody(c, maxErrorOnlyCapturedRequestBodyBytes); ok && len(body) > 0 {
		w.requestInfo.Body = body
		return body
	}
	return nil
}

func (w *ResponseWriterWrapper) logRequest(requestBody []byte, statusCode int, headers map[string][]string, body []byte, apiRequestBody, apiResponseBody []byte, apiResponseTimestamp time.Time, apiResponseErrors []*interfaces.ErrorMessage, forceLog bool) error {
	if w.requestInfo == nil {
		return nil
	}

	if w.ginCtx != nil {
		if forceLog && diagnostics.ShouldRedactErrorOnlyBody(w.ginCtx, statusCode) {
			summary := diagnostics.RecordBody(w.ginCtx, requestBody, true, "local_4xx_error")
			requestBody = diagnostics.FormatBodySummary(summary)
		} else {
			diagnostics.RecordBody(w.ginCtx, requestBody, false, "")
		}
	}

	diagnosticSnapshot := diagnostics.Snapshot{}
	if diagnostic := diagnostics.FromGin(w.ginCtx); diagnostic != nil {
		diagnosticSnapshot = diagnostic.Snapshot()
	}

	if loggerWithDiagnostics, ok := w.logger.(interface {
		LogRequestWithOptionsAndDiagnostics(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time, diagnostics.Snapshot) error
	}); ok {
		return loggerWithDiagnostics.LogRequestWithOptionsAndDiagnostics(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			requestBody,
			statusCode,
			headers,
			body,
			apiRequestBody,
			apiResponseBody,
			apiResponseErrors,
			forceLog,
			w.requestInfo.RequestID,
			w.requestInfo.Timestamp,
			apiResponseTimestamp,
			diagnosticSnapshot,
		)
	}

	if loggerWithOptions, ok := w.logger.(interface {
		LogRequestWithOptions(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []*interfaces.ErrorMessage, bool, string, time.Time, time.Time) error
	}); ok {
		return loggerWithOptions.LogRequestWithOptions(
			w.requestInfo.URL,
			w.requestInfo.Method,
			w.requestInfo.Headers,
			requestBody,
			statusCode,
			headers,
			body,
			apiRequestBody,
			apiResponseBody,
			apiResponseErrors,
			forceLog,
			w.requestInfo.RequestID,
			w.requestInfo.Timestamp,
			apiResponseTimestamp,
		)
	}

	return w.logger.LogRequest(
		w.requestInfo.URL,
		w.requestInfo.Method,
		w.requestInfo.Headers,
		requestBody,
		statusCode,
		headers,
		body,
		apiRequestBody,
		apiResponseBody,
		apiResponseErrors,
		w.requestInfo.RequestID,
		w.requestInfo.Timestamp,
		apiResponseTimestamp,
	)
}
