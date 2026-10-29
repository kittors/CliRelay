// Package logging provides request logging functionality for the CLI Proxy API server.
//
// Request logging is intentionally split across small files so each responsibility
// has a stable owner:
// - request_logger.go: top-level request/stream orchestration, scrubbed file writes
// - request_logger_paths.go: log file naming, temp files, error-log cleanup
// - request_logger_format.go: human-readable log formatting
// - request_logger_decompress.go: response content codecs
// - streaming_log_writer.go: streaming lifecycle and temp-file assembly
package logging

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/secretscrub"
	log "github.com/sirupsen/logrus"
)

func (l *FileRequestLogger) IsEnabled() bool {
	return l.enabled
}

func (l *FileRequestLogger) SetEnabled(enabled bool) {
	l.enabled = enabled
}

func (l *FileRequestLogger) SetErrorLogsMaxFiles(maxFiles int) {
	l.errorLogsMaxFiles = maxFiles
}

func (l *FileRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, apiRequest, apiResponse, apiResponseErrors, false, requestID, requestTimestamp, apiResponseTimestamp, diagnostics.Snapshot{})
}

// LogRequestWithOptions keeps forced error-log writes available even when
// regular request logging is disabled.
func (l *FileRequestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return l.logRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, apiRequest, apiResponse, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp, diagnostics.Snapshot{})
}

func (l *FileRequestLogger) LogRequestWithOptionsAndDiagnostics(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time, diagnostic diagnostics.Snapshot) error {
	return l.logRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, apiRequest, apiResponse, apiResponseErrors, force, requestID, requestTimestamp, apiResponseTimestamp, diagnostic)
}

func (l *FileRequestLogger) logRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time, diagnostic diagnostics.Snapshot) error {
	if !l.enabled && !force {
		return nil
	}
	if errEnsure := l.ensureLogsDir(); errEnsure != nil {
		return fmt.Errorf("failed to create logs directory: %w", errEnsure)
	}

	logURL := url
	if diagnostic.OriginalURL != "" {
		logURL = diagnostic.OriginalURL
	}
	filenameURL := logURL
	filename := l.generateFilename(filenameURL, requestID)
	if force && !l.enabled {
		filename = l.generateErrorFilename(filenameURL, requestID)
	}
	filePath := filepath.Join(l.logsDir, filename)

	responseToWrite, decompressErr := l.decompressResponse(responseHeaders, response)
	if decompressErr != nil {
		responseToWrite = response
	}

	if errWrite := writeScrubbedLogFile(filePath, func(w io.Writer) error {
		return l.writeNonStreamingLog(w, logURL, method, requestHeaders, body, apiRequest, apiResponse, apiResponseErrors,
			statusCode, responseHeaders, responseToWrite, decompressErr, requestTimestamp, apiResponseTimestamp, diagnostic)
	}); errWrite != nil {
		return errWrite
	}

	if force && !l.enabled {
		if errCleanup := l.cleanupOldErrorLogs(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up old error logs")
		}
	} else if l.enabled {
		if errCleanup := l.cleanupOldRequestLogs(false); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up old request logs")
		}
	}

	return nil
}

// writeScrubbedLogFile renders a request log in memory and writes it to path
// with every credential it holds replaced by [REDACTED:<kind>] (see
// secretscrub). A request log copies the client's request, what was sent
// upstream and the responses, so it carries whatever keys the client's tools
// read; scrubbing the rendered file as a whole covers every section, the
// headers and diagnostics included.
func writeScrubbedLogFile(path string, render func(io.Writer) error) error {
	var rendered bytes.Buffer
	if errRender := render(&rendered); errRender != nil {
		return fmt.Errorf("failed to write log file: %w", errRender)
	}
	if errWrite := os.WriteFile(path, secretscrub.ScrubBytes(rendered.Bytes()), 0o644); errWrite != nil {
		return fmt.Errorf("failed to write log file: %w", errWrite)
	}
	return nil
}

// LogStreamingRequest creates a streaming writer that buffers request metadata
// immediately and spools response chunks asynchronously.
func (l *FileRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (StreamingLogWriter, error) {
	if !l.enabled {
		return &NoOpStreamingLogWriter{}, nil
	}
	if err := l.ensureLogsDir(); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	filename := l.generateFilename(url, requestID)
	filePath := filepath.Join(l.logsDir, filename)

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

	writer := &FileStreamingLogWriter{
		logger:           l,
		logFilePath:      filePath,
		url:              url,
		method:           method,
		timestamp:        time.Now(),
		requestHeaders:   requestHeaders,
		requestBodyPath:  requestBodyPath,
		responseBodyPath: responseBodyPath,
		responseBodyFile: responseBodyFile,
		chunkChan:        make(chan []byte, 100),
		closeChan:        make(chan struct{}),
		errorChan:        make(chan error, 1),
	}

	go writer.asyncWriter()
	return writer, nil
}
