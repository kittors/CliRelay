package logging

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/regexp"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	log "github.com/sirupsen/logrus"
)

var (
	requestLogID                        atomic.Uint64
	sanitizeFilenameInvalidCharsPattern = regexp.MustCompile(`[<>:"|?*\s]`)
	sanitizeFilenameMultiHyphenPattern  = regexp.MustCompile(`-+`)
)

// LogRequest logs a complete non-streaming request/response cycle to a file.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - requestHeaders: The request headers
//   - body: The request body
//   - statusCode: The response status code
//   - responseHeaders: The response headers
//   - response: The raw response data
//   - apiRequest: The API request data
//   - apiResponse: The API response data
//   - requestID: Optional request ID for log file naming
//   - requestTimestamp: When the request was received
//   - apiResponseTimestamp: When the API response was received
//
// Returns:
//   - error: An error if logging fails, nil otherwise
func (l *FileRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, false, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptions logs a request with optional forced logging behavior.
// The force flag allows writing error logs even when regular request logging is disabled.
func (l *FileRequestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

func (l *FileRequestLogger) logRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, nil, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptionsAndSources logs a request with optional file-backed large sections.
func (l *FileRequestLogger) LogRequestWithOptionsAndSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, websocketTimelineSource, apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

// LogRequestWithOptionsAndAllSources logs a request with optional file-backed request and response sections.
func (l *FileRequestLogger) LogRequestWithOptionsAndAllSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequestWithSources(url, method, requestHeaders, body, statusCode, responseHeaders, response, websocketTimeline, websocketTimelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp)
}

func (l *FileRequestLogger) logRequestWithSources(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline []byte, websocketTimelineSource *FileBodySource, apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	defer cleanupFileBodySources(websocketTimelineSource, apiRequestSource, apiResponseSource, apiWebsocketTimelineSource)

	if !l.enabled && !force {
		return nil
	}

	if l.homeEnabled && l.enabled {
		responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
		if decompressErr != nil {
			responseToWrite = response
		}

		var buf bytes.Buffer
		writeErr := l.writeNonStreamingLog(
			&buf,
			url,
			method,
			requestHeaders,
			body,
			"",
			websocketTimeline,
			websocketTimelineSource,
			apiRequest,
			apiRequestSource,
			apiResponse,
			apiResponseSource,
			apiWebsocketTimeline,
			apiWebsocketTimelineSource,
			apiResponseErrors,
			statusCode,
			responseHeaders,
			responseToWrite,
			decompressErr,
			requestTimestamp,
			apiResponseTimestamp,
		)
		if writeErr != nil {
			return fmt.Errorf("failed to build request log content: %w", writeErr)
		}
		return l.forwardRequestLogToHome(context.Background(), requestHeaders, requestID, buf.String())
	}

	// Ensure logs directory exists
	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return fmt.Errorf("failed to create logs directory: %w", errEnsure)
	}

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)
	if (force && !l.enabled) || (statusCode >= 400 && statusCode != 499) {
		filename = l.generateErrorFilename(url, requestID)
	}

	requestBodyPath, errTemp := l.writeRequestBodyTempFile(body)
	if errTemp != nil {
		log.WithError(errTemp).Warn("failed to create request body temp file, falling back to direct write")
	}
	if requestBodyPath != "" {
		defer func() {
			if errRemove := os.Remove(requestBodyPath); errRemove != nil {
				log.WithError(errRemove).Warn("failed to remove request body temp file")
			}
		}()
	}

	responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
	if decompressErr != nil {
		// If decompression fails, continue with original response and annotate the log output.
		responseToWrite = response
	}

	logFile, _, errOpen := createUniqueLogFile(l.logsDir, filename)
	if errOpen != nil {
		return fmt.Errorf("failed to create log file: %w", errOpen)
	}

	writeErr := l.writeNonStreamingLog(
		logFile,
		url,
		method,
		requestHeaders,
		body,
		requestBodyPath,
		websocketTimeline,
		websocketTimelineSource,
		apiRequest,
		apiRequestSource,
		apiResponse,
		apiResponseSource,
		apiWebsocketTimeline,
		apiWebsocketTimelineSource,
		apiResponseErrors,
		statusCode,
		responseHeaders,
		responseToWrite,
		decompressErr,
		requestTimestamp,
		apiResponseTimestamp,
	)
	if errClose := logFile.Close(); errClose != nil {
		log.WithError(errClose).Warn("failed to close request log file")
		if writeErr == nil {
			return errClose
		}
	}
	if writeErr != nil {
		return fmt.Errorf("failed to write log file: %w", writeErr)
	}

	if force && !l.enabled {
		if errCleanup := l.cleanupOldErrorLogs(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up old error logs")
		}
	}

	return nil
}

// LogStreamingRequest initiates logging for a streaming request.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - headers: The request headers
//   - body: The request body
//   - requestID: Optional request ID for log file naming
//
// Returns:
//   - StreamingLogWriter: A writer for streaming response chunks
//   - error: An error if logging initialization fails, nil otherwise
func (l *FileRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (StreamingLogWriter, error) {
	if !l.enabled {
		return &NoOpStreamingLogWriter{}, nil
	}

	if l.homeEnabled {
		client := currentHomeRequestLogClient()
		if client == nil || !client.HeartbeatOK() {
			return &NoOpStreamingLogWriter{}, nil
		}
		return newHomeStreamingLogWriter(url, method, headers, body, requestID), nil
	}

	// Ensure logs directory exists
	if err := l.ensureLogsDir(); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Generate filename with request ID
	filename := l.generateFilename(url, requestID)

	requestHeaders := make(map[string][]string, len(headers))
	for key, values := range headers {
		headerValues := make([]string, len(values))
		copy(headerValues, values)
		requestHeaders[key] = headerValues
	}

	requestBodyPath, errTemp := l.writeRequestBodyTempFile(body)
	if errTemp != nil {
		return nil, fmt.Errorf("failed to create request body temp file: %w", errTemp)
	}

	responseBodyFile, errCreate := os.CreateTemp(l.logsDir, "response-body-*.tmp")
	if errCreate != nil {
		_ = os.Remove(requestBodyPath)
		return nil, fmt.Errorf("failed to create response body temp file: %w", errCreate)
	}
	responseBodyPath := responseBodyFile.Name()

	// Create streaming writer
	writer := &FileStreamingLogWriter{
		logsDir:          l.logsDir,
		logFilename:      filename,
		url:              url,
		method:           method,
		timestamp:        time.Now(),
		requestHeaders:   requestHeaders,
		requestBodyPath:  requestBodyPath,
		responseBodyPath: responseBodyPath,
		responseBodyFile: responseBodyFile,
		chunkChan:        make(chan []byte, 100), // Buffered channel for async writes
		closeChan:        make(chan struct{}),
		errorChan:        make(chan error, 1),
	}

	// Start async writer goroutine
	go writer.asyncWriter()

	return writer, nil
}

// createUniqueLogFile atomically opens a unique log file within dir. If a file with filename already exists,
// it avoids overwriting by injecting an incrementing sequence number before the trailing request ID component.
func createUniqueLogFile(dir, filename string) (*os.File, string, error) {
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) || strings.Contains(filename, "..") || filepath.Base(filename) != filename {
		return nil, "", fmt.Errorf("invalid log filename: %q", filename)
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	idx := strings.LastIndex(base, "-")
	prefix := base
	idPart := ""
	if idx > 0 {
		prefix = base[:idx]
		idPart = base[idx+1:]
	}

	target := filepath.Join(dir, filename)
	logFile, errOpen := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if errOpen == nil {
		return logFile, target, nil
	}
	if !os.IsExist(errOpen) {
		return nil, "", errOpen
	}

	for seq := 1; seq <= 1000; seq++ {
		candidateName := fmt.Sprintf("%s_%d-%s%s", prefix, seq, idPart, ext)
		candidatePath := filepath.Join(dir, candidateName)
		logCandidate, errCandidate := os.OpenFile(candidatePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if errCandidate == nil {
			return logCandidate, candidatePath, nil
		}
		if !os.IsExist(errCandidate) {
			return nil, "", errCandidate
		}
	}
	return nil, "", fmt.Errorf("too many conflicting log files for %s", filename)
}
