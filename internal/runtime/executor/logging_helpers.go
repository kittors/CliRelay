package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	apiAttemptsKey               = "API_UPSTREAM_ATTEMPTS"
	apiDiagnosticAttemptCountKey = "API_UPSTREAM_DIAGNOSTIC_ATTEMPT_COUNT"
	apiRequestKey                = "API_REQUEST"
	apiResponseKey               = "API_RESPONSE"
)

// upstreamRequestLog captures the outbound upstream request details for logging.
type upstreamRequestLog struct {
	URL       string
	Method    string
	Headers   http.Header
	Body      []byte
	Provider  string
	AuthID    string
	AuthLabel string
	AuthType  string
	AuthValue string
}

const apiExchangeResponseMemoryLimit = 256 * 1024

type apiExchangeBuffer struct {
	buffer bytes.Buffer
	file   *os.File
	writer *bufio.Writer
	path   string
	size   int64
}

func (b *apiExchangeBuffer) Write(data []byte) (int, error) {
	if b == nil || len(data) == 0 {
		return len(data), nil
	}
	if b.file == nil && b.buffer.Len()+len(data) > apiExchangeResponseMemoryLimit {
		b.spillToFile()
	}
	if b.file != nil {
		n, err := b.writer.Write(data)
		b.size += int64(n)
		if err == nil {
			return n, nil
		}
		b.fallbackToMemory()
		remaining := data[n:]
		written, writeErr := b.buffer.Write(remaining)
		b.size += int64(written)
		return n + written, writeErr
	}
	n, err := b.buffer.Write(data)
	b.size += int64(n)
	return n, err
}

func (b *apiExchangeBuffer) WriteString(value string) (int, error) {
	if b == nil || value == "" {
		return len(value), nil
	}
	if b.file == nil && b.buffer.Len()+len(value) > apiExchangeResponseMemoryLimit {
		b.spillToFile()
	}
	if b.file != nil {
		n, err := b.writer.WriteString(value)
		b.size += int64(n)
		if err == nil {
			return n, nil
		}
		b.fallbackToMemory()
		written, writeErr := b.buffer.WriteString(value[n:])
		b.size += int64(written)
		return n + written, writeErr
	}
	n, err := b.buffer.WriteString(value)
	b.size += int64(n)
	return n, err
}

func (b *apiExchangeBuffer) Len() int {
	if b == nil {
		return 0
	}
	return int(b.size)
}

func (b *apiExchangeBuffer) Snapshot() []byte {
	if b == nil || b.size == 0 {
		return nil
	}
	if b.file == nil {
		return bytes.Clone(b.buffer.Bytes())
	}
	if b.writer != nil {
		if err := b.writer.Flush(); err != nil {
			return nil
		}
	}
	current, err := b.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil
	}
	if _, err = b.file.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(b.file)
	_, _ = b.file.Seek(current, io.SeekStart)
	if err != nil {
		return nil
	}
	return data
}

func (b *apiExchangeBuffer) Close() error {
	if b == nil {
		return nil
	}
	var closeErr error
	if b.file != nil {
		if b.writer != nil {
			closeErr = b.writer.Flush()
			b.writer = nil
		}
		if err := b.file.Close(); closeErr == nil {
			closeErr = err
		}
		b.file = nil
	}
	if b.path != "" {
		if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) && closeErr == nil {
			closeErr = err
		}
		b.path = ""
	}
	b.buffer.Reset()
	b.size = 0
	return closeErr
}

func (b *apiExchangeBuffer) spillToFile() {
	file, err := os.CreateTemp("", "clirelay-api-exchange-*")
	if err != nil {
		return
	}
	if b.buffer.Len() > 0 {
		if _, err = file.Write(b.buffer.Bytes()); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return
		}
		b.buffer.Reset()
	}
	b.file = file
	b.writer = bufio.NewWriterSize(file, 64*1024)
	b.path = file.Name()
}

func (b *apiExchangeBuffer) fallbackToMemory() {
	if b.file == nil {
		return
	}
	path := b.path
	if b.writer != nil {
		_ = b.writer.Flush()
		b.writer = nil
	}
	_, _ = b.file.Seek(0, io.SeekStart)
	data, _ := io.ReadAll(b.file)
	_ = b.file.Close()
	_ = os.Remove(path)
	b.file = nil
	b.path = ""
	b.buffer.Reset()
	_, _ = b.buffer.Write(data)
}

type upstreamAttempt struct {
	index                int
	request              string
	response             *apiExchangeBuffer
	responseIntroWritten bool
	statusWritten        bool
	headersWritten       bool
	bodyStarted          bool
	bodyHasContent       bool
	errorWritten         bool
}

type apiExchangeCapture struct {
	mu                     sync.Mutex
	attempts               []*upstreamAttempt
	cachedRequestSnapshot  []byte
	cachedResponseSnapshot []byte
}

func (c *apiExchangeCapture) APIRequestSnapshot() []byte {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedRequestSnapshot == nil {
		c.cachedRequestSnapshot = aggregateAPIRequests(c.attempts)
	}
	return c.cachedRequestSnapshot
}

func (c *apiExchangeCapture) APIResponseSnapshot() []byte {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedResponseSnapshot == nil {
		c.cachedResponseSnapshot = aggregateAPIResponses(c.attempts)
	}
	return c.cachedResponseSnapshot
}

func (c *apiExchangeCapture) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var closeErr error
	for _, attempt := range c.attempts {
		if attempt == nil || attempt.response == nil {
			continue
		}
		if err := attempt.response.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	c.attempts = nil
	c.cachedRequestSnapshot = nil
	c.cachedResponseSnapshot = nil
	return closeErr
}

// recordAPIRequest stores the upstream request metadata in an incremental
// per-request capture. Full response snapshots are materialized only when the
// request log is finalized, not after every streaming chunk.
func recordAPIRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	diagnostics.SetUpstreamRequest(ctx, nextUpstreamDiagnosticAttempt(ginCtx), info.Provider, info.AuthID, info.AuthLabel, info.URL, info.Method)
	if !shouldCaptureAPIExchangeLog(cfg) {
		return
	}

	capture := ensureAPIExchangeCapture(ginCtx)
	capture.mu.Lock()
	index := len(capture.attempts) + 1
	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("=== API REQUEST %d ===\n", index))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", info.URL))
	} else {
		builder.WriteString("Upstream URL: <unknown>\n")
	}
	if info.Method != "" {
		builder.WriteString(fmt.Sprintf("HTTP Method: %s\n", info.Method))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("\nHeaders:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	if !shouldCaptureAPIExchangeBody(cfg) {
		builder.WriteString("<not stored>")
	} else if len(info.Body) > 0 {
		builder.Write(info.Body)
	} else {
		builder.WriteString("<empty>")
	}
	builder.WriteString("\n\n")
	capture.attempts = append(capture.attempts, &upstreamAttempt{
		index:    index,
		request:  builder.String(),
		response: &apiExchangeBuffer{},
	})
	capture.cachedRequestSnapshot = nil
	capture.cachedResponseSnapshot = nil
	requestSnapshot := aggregateAPIRequests(capture.attempts)
	capture.cachedRequestSnapshot = requestSnapshot
	capture.mu.Unlock()

	// Preserve the legacy context contract for existing request-log consumers.
	ginCtx.Set(apiRequestKey, requestSnapshot)
}

func recordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	diagnostics.SetUpstreamResponse(ctx, status)
	capture, attempt := captureAttempt(ctx, cfg)
	if capture == nil || attempt == nil {
		return
	}
	defer capture.mu.Unlock()
	ensureResponseIntro(attempt)
	if status > 0 && !attempt.statusWritten {
		writeAPIExchangeString(attempt.response, fmt.Sprintf("Status: %d\n", status))
		attempt.statusWritten = true
	}
	if !attempt.headersWritten {
		writeAPIExchangeString(attempt.response, "Headers:\n")
		writeHeaders(attempt.response, headers)
		attempt.headersWritten = true
		writeAPIExchangeString(attempt.response, "\n")
	}
	capture.cachedResponseSnapshot = nil
}

func recordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	if err == nil {
		return
	}
	diagnostics.SetUpstreamError(ctx, err)
	capture, attempt := captureAttempt(ctx, cfg)
	if capture == nil || attempt == nil {
		return
	}
	defer capture.mu.Unlock()
	ensureResponseIntro(attempt)
	if attempt.bodyStarted && !attempt.bodyHasContent {
		attempt.bodyStarted = false
	}
	if attempt.errorWritten {
		writeAPIExchangeString(attempt.response, "\n")
	}
	writeAPIExchangeString(attempt.response, fmt.Sprintf("Error: %s\n", err.Error()))
	attempt.errorWritten = true
	capture.cachedResponseSnapshot = nil
}

func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	if !shouldCaptureAPIExchangeBody(cfg) {
		return
	}
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	capture, attempt := captureAttempt(ctx, cfg)
	if capture == nil || attempt == nil {
		return
	}
	defer capture.mu.Unlock()
	ensureResponseIntro(attempt)
	if !attempt.headersWritten {
		writeAPIExchangeString(attempt.response, "Headers:\n")
		writeHeaders(attempt.response, nil)
		attempt.headersWritten = true
		writeAPIExchangeString(attempt.response, "\n")
	}
	if !attempt.bodyStarted {
		writeAPIExchangeString(attempt.response, "Body:\n")
		attempt.bodyStarted = true
	}
	if attempt.bodyHasContent {
		writeAPIExchangeString(attempt.response, "\n\n")
	}
	writeAPIExchangeBytes(attempt.response, data)
	attempt.bodyHasContent = true
	capture.cachedResponseSnapshot = nil
}

func shouldCaptureAPIExchangeLog(cfg *config.Config) bool {
	return cfg != nil
}

func shouldCaptureAPIExchangeBody(cfg *config.Config) bool {
	return cfg != nil && (cfg.RequestLog || cfg.RequestLogStorage.StoreContent)
}

func ginContextFrom(ctx context.Context) *gin.Context {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	return ginCtx
}

func nextUpstreamDiagnosticAttempt(ginCtx *gin.Context) int {
	if ginCtx == nil {
		return 0
	}
	count := 0
	if value, exists := ginCtx.Get(apiDiagnosticAttemptCountKey); exists {
		switch v := value.(type) {
		case int:
			count = v
		case int64:
			count = int(v)
		}
	}
	count++
	ginCtx.Set(apiDiagnosticAttemptCountKey, count)
	return count
}

func ensureAPIExchangeCapture(ginCtx *gin.Context) *apiExchangeCapture {
	if raw, exists := ginCtx.Get(apiAttemptsKey); exists {
		if capture, ok := raw.(*apiExchangeCapture); ok && capture != nil {
			return capture
		}
	}
	capture := &apiExchangeCapture{}
	ginCtx.Set(apiAttemptsKey, capture)
	logging.SetAPIExchangeProvider(ginCtx, capture)
	return capture
}

func captureAttempt(ctx context.Context, cfg *config.Config) (*apiExchangeCapture, *upstreamAttempt) {
	if !shouldCaptureAPIExchangeLog(cfg) {
		return nil, nil
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return nil, nil
	}
	capture := ensureAPIExchangeCapture(ginCtx)
	capture.mu.Lock()
	if len(capture.attempts) == 0 {
		capture.attempts = append(capture.attempts, &upstreamAttempt{
			index:    1,
			request:  "=== API REQUEST 1 ===\n<missing>\n\n",
			response: &apiExchangeBuffer{},
		})
		capture.cachedRequestSnapshot = aggregateAPIRequests(capture.attempts)
		ginCtx.Set(apiRequestKey, capture.cachedRequestSnapshot)
	}
	return capture, capture.attempts[len(capture.attempts)-1]
}

func ensureResponseIntro(attempt *upstreamAttempt) {
	if attempt == nil || attempt.response == nil || attempt.responseIntroWritten {
		return
	}
	writeAPIExchangeString(attempt.response, fmt.Sprintf("=== API RESPONSE %d ===\n", attempt.index))
	writeAPIExchangeString(attempt.response, fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	writeAPIExchangeString(attempt.response, "\n")
	attempt.responseIntroWritten = true
}

func aggregateAPIRequests(attempts []*upstreamAttempt) []byte {
	var builder strings.Builder
	for _, attempt := range attempts {
		if attempt != nil {
			builder.WriteString(attempt.request)
		}
	}
	if builder.Len() == 0 {
		return nil
	}
	return []byte(builder.String())
}

func aggregateAPIResponses(attempts []*upstreamAttempt) []byte {
	var builder strings.Builder
	for idx, attempt := range attempts {
		if attempt == nil || attempt.response == nil || attempt.response.Len() == 0 {
			continue
		}
		responseBytes := attempt.response.Snapshot()
		builder.Write(responseBytes)
		if !bytes.HasSuffix(responseBytes, []byte("\n")) {
			builder.WriteString("\n")
		}
		if idx < len(attempts)-1 {
			builder.WriteString("\n")
		}
	}
	if builder.Len() == 0 {
		return nil
	}
	return []byte(builder.String())
}

func writeAPIExchangeString(writer interface{ WriteString(string) (int, error) }, value string) {
	if writer == nil || value == "" {
		return
	}
	if _, err := writer.WriteString(value); err != nil {
		log.WithError(err).Warn("failed to capture API exchange text")
	}
}

func writeAPIExchangeBytes(writer io.Writer, value []byte) {
	if writer == nil || len(value) == 0 {
		return
	}
	if _, err := writer.Write(value); err != nil {
		log.WithError(err).Warn("failed to capture API exchange bytes")
	}
}

func writeHeaders(builder interface{ WriteString(string) (int, error) }, headers http.Header) {
	if builder == nil {
		return
	}
	if len(headers) == 0 {
		writeAPIExchangeString(builder, "<none>\n")
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			writeAPIExchangeString(builder, fmt.Sprintf("%s:\n", key))
			continue
		}
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			writeAPIExchangeString(builder, fmt.Sprintf("%s: %s\n", key, masked))
		}
	}
}

func formatAuthInfo(info upstreamRequestLog) string {
	var parts []string
	if trimmed := strings.TrimSpace(info.Provider); trimmed != "" {
		parts = append(parts, fmt.Sprintf("provider=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthID); trimmed != "" {
		parts = append(parts, fmt.Sprintf("auth_id=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthLabel); trimmed != "" {
		parts = append(parts, fmt.Sprintf("label=%s", trimmed))
	}

	authType := strings.ToLower(strings.TrimSpace(info.AuthType))
	authValue := strings.TrimSpace(info.AuthValue)
	switch authType {
	case "api_key":
		if authValue != "" {
			parts = append(parts, fmt.Sprintf("type=api_key value=%s", util.HideAPIKey(authValue)))
		} else {
			parts = append(parts, "type=api_key")
		}
	case "oauth":
		parts = append(parts, "type=oauth")
	default:
		if authType != "" {
			if authValue != "" {
				parts = append(parts, fmt.Sprintf("type=%s value=%s", authType, authValue))
			} else {
				parts = append(parts, fmt.Sprintf("type=%s", authType))
			}
		}
	}

	return strings.Join(parts, ", ")
}

func summarizeErrorBody(contentType string, body []byte) string {
	isHTML := strings.Contains(strings.ToLower(contentType), "text/html")
	if !isHTML {
		trimmed := bytes.TrimSpace(bytes.ToLower(body))
		if bytes.HasPrefix(trimmed, []byte("<!doctype html")) || bytes.HasPrefix(trimmed, []byte("<html")) {
			isHTML = true
		}
	}
	if isHTML {
		if title := extractHTMLTitle(body); title != "" {
			return title
		}
		return "[html body omitted]"
	}

	// Try to extract error message from JSON response
	if message := extractJSONErrorMessage(body); message != "" {
		return message
	}

	return string(body)
}

func extractHTMLTitle(body []byte) string {
	lower := bytes.ToLower(body)
	start := bytes.Index(lower, []byte("<title"))
	if start == -1 {
		return ""
	}
	gt := bytes.IndexByte(lower[start:], '>')
	if gt == -1 {
		return ""
	}
	start += gt + 1
	end := bytes.Index(lower[start:], []byte("</title>"))
	if end == -1 {
		return ""
	}
	title := string(body[start : start+end])
	title = html.UnescapeString(title)
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return strings.Join(strings.Fields(title), " ")
}

// extractJSONErrorMessage attempts to extract error.message from JSON error responses
func extractJSONErrorMessage(body []byte) string {
	result := gjson.GetBytes(body, "error.message")
	if result.Exists() && result.String() != "" {
		return result.String()
	}
	return ""
}

// logWithRequestID returns a logrus Entry with request_id field populated from context.
// If no request ID is found in context, it returns the standard logger.
func logWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	requestID := logging.GetRequestID(ctx)
	if requestID == "" {
		return log.NewEntry(log.StandardLogger())
	}
	return log.WithField("request_id", requestID)
}
