package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

func (l *FileRequestLogger) writeNonStreamingLog(
	w io.Writer,
	url, method string,
	requestHeaders map[string][]string,
	requestBody []byte,
	requestBodyPath string,
	apiRequest []byte,
	apiResponse []byte,
	apiResponseErrors []*interfaces.ErrorMessage,
	statusCode int,
	responseHeaders map[string][]string,
	response []byte,
	decompressErr error,
	requestTimestamp time.Time,
	apiResponseTimestamp time.Time,
	diagnostic diagnostics.Snapshot,
) error {
	if requestTimestamp.IsZero() {
		requestTimestamp = time.Now()
	}
	if errWrite := writeRequestInfoWithBody(w, url, method, requestHeaders, requestBody, requestBodyPath, requestTimestamp, diagnostic); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISection(w, "=== API REQUEST ===\n", "=== API REQUEST", apiRequest, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPIErrorResponses(w, apiResponseErrors); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISection(w, "=== API RESPONSE ===\n", "=== API RESPONSE", apiResponse, apiResponseTimestamp); errWrite != nil {
		return errWrite
	}
	return writeResponseSection(w, statusCode, true, responseHeaders, bytes.NewReader(response), decompressErr, true)
}

func writeRequestInfoWithBody(
	w io.Writer,
	url, method string,
	headers map[string][]string,
	body []byte,
	bodyPath string,
	timestamp time.Time,
	diagnostic diagnostics.Snapshot,
) error {
	if _, errWrite := io.WriteString(w, "=== REQUEST INFO ===\n"); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Version: %s\n", buildinfo.Version)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("URL: %s\n", url)); errWrite != nil {
		return errWrite
	}
	if diagnostic.EffectiveURL != "" && diagnostic.EffectiveURL != url {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Effective URL: %s\n", diagnostic.EffectiveURL)); errWrite != nil {
			return errWrite
		}
	}
	if diagnostic.RequestID != "" {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Request ID: %s\n", diagnostic.RequestID)); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Method: %s\n", method)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}
	if hasDiagnosticMetadata(diagnostic) {
		if _, errWrite := io.WriteString(w, "=== DIAGNOSTIC METADATA ===\n"); errWrite != nil {
			return errWrite
		}
		data, errJSON := json.MarshalIndent(diagnostic, "", "  ")
		if errJSON != nil {
			return errJSON
		}
		if _, errWrite := w.Write(data); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
			return errWrite
		}
	}

	if _, errWrite := io.WriteString(w, "=== HEADERS ===\n"); errWrite != nil {
		return errWrite
	}
	for key, values := range headers {
		for _, value := range values {
			if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, util.MaskSensitiveHeaderValue(key, value))); errWrite != nil {
				return errWrite
			}
		}
	}
	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "=== REQUEST BODY ===\n"); errWrite != nil {
		return errWrite
	}
	if bodyPath != "" {
		bodyFile, errOpen := os.Open(bodyPath)
		if errOpen != nil {
			return errOpen
		}
		if _, errCopy := io.Copy(w, bodyFile); errCopy != nil {
			_ = bodyFile.Close()
			return errCopy
		}
		if errClose := bodyFile.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close request body temp file")
		}
	} else if _, errWrite := w.Write(body); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
		return errWrite
	}
	return nil
}

func hasDiagnosticMetadata(diagnostic diagnostics.Snapshot) bool {
	return diagnostic.RequestID != "" ||
		diagnostic.OriginalURL != "" ||
		diagnostic.EffectiveURL != "" ||
		diagnostic.Route != nil ||
		diagnostic.Auth != nil ||
		diagnostic.Quota != nil ||
		diagnostic.Upstream != nil ||
		diagnostic.Egress != nil ||
		diagnostic.Response != nil ||
		diagnostic.Body != nil
}

func writeAPISection(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, timestamp time.Time) error {
	if len(payload) == 0 {
		return nil
	}

	if bytes.HasPrefix(payload, []byte(sectionPrefix)) {
		if _, errWrite := w.Write(payload); errWrite != nil {
			return errWrite
		}
		if !bytes.HasSuffix(payload, []byte("\n")) {
			if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
				return errWrite
			}
		}
	} else {
		if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
			return errWrite
		}
		if !timestamp.IsZero() {
			if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
				return errWrite
			}
		}
		if _, errWrite := w.Write(payload); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}

	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}
	return nil
}

func writeAPIErrorResponses(w io.Writer, apiResponseErrors []*interfaces.ErrorMessage) error {
	for i := 0; i < len(apiResponseErrors); i++ {
		if apiResponseErrors[i] == nil {
			continue
		}
		if _, errWrite := io.WriteString(w, "=== API ERROR RESPONSE ===\n"); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, fmt.Sprintf("HTTP Status: %d\n", apiResponseErrors[i].StatusCode)); errWrite != nil {
			return errWrite
		}
		if apiResponseErrors[i].Error != nil {
			if _, errWrite := io.WriteString(w, apiResponseErrors[i].Error.Error()); errWrite != nil {
				return errWrite
			}
		}
		if _, errWrite := io.WriteString(w, "\n\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeResponseSection(w io.Writer, statusCode int, statusWritten bool, responseHeaders map[string][]string, responseReader io.Reader, decompressErr error, trailingNewline bool) error {
	if _, errWrite := io.WriteString(w, "=== RESPONSE ===\n"); errWrite != nil {
		return errWrite
	}
	if statusWritten {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Status: %d\n", statusCode)); errWrite != nil {
			return errWrite
		}
	}
	for key, values := range responseHeaders {
		for _, value := range values {
			if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, value)); errWrite != nil {
				return errWrite
			}
		}
	}
	if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
		return errWrite
	}
	if responseReader != nil {
		if _, errCopy := io.Copy(w, responseReader); errCopy != nil {
			return errCopy
		}
	}
	if decompressErr != nil {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("\n[DECOMPRESSION ERROR: %v]", decompressErr)); errWrite != nil {
			return errWrite
		}
	}
	if trailingNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

// formatLogContent remains as a lightweight formatter for tests and any future
// callers that need the non-streaming text representation without hitting disk.
func (l *FileRequestLogger) formatLogContent(url, method string, headers map[string][]string, body, apiRequest, apiResponse, response []byte, status int, responseHeaders map[string][]string, apiResponseErrors []*interfaces.ErrorMessage) string {
	var content strings.Builder
	content.WriteString(l.formatRequestInfo(url, method, headers, body))

	if len(apiRequest) > 0 {
		if bytes.HasPrefix(apiRequest, []byte("=== API REQUEST")) {
			content.Write(apiRequest)
			if !bytes.HasSuffix(apiRequest, []byte("\n")) {
				content.WriteString("\n")
			}
		} else {
			content.WriteString("=== API REQUEST ===\n")
			content.Write(apiRequest)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	for i := 0; i < len(apiResponseErrors); i++ {
		if apiResponseErrors[i] == nil {
			continue
		}
		content.WriteString("=== API ERROR RESPONSE ===\n")
		content.WriteString(fmt.Sprintf("HTTP Status: %d\n", apiResponseErrors[i].StatusCode))
		if apiResponseErrors[i].Error != nil {
			content.WriteString(apiResponseErrors[i].Error.Error())
		}
		content.WriteString("\n\n")
	}

	if len(apiResponse) > 0 {
		if bytes.HasPrefix(apiResponse, []byte("=== API RESPONSE")) {
			content.Write(apiResponse)
			if !bytes.HasSuffix(apiResponse, []byte("\n")) {
				content.WriteString("\n")
			}
		} else {
			content.WriteString("=== API RESPONSE ===\n")
			content.Write(apiResponse)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	content.WriteString("=== RESPONSE ===\n")
	content.WriteString(fmt.Sprintf("Status: %d\n", status))
	for key, values := range responseHeaders {
		for _, value := range values {
			content.WriteString(fmt.Sprintf("%s: %s\n", key, value))
		}
	}
	content.WriteString("\n")
	content.Write(response)
	content.WriteString("\n")
	return content.String()
}

func (l *FileRequestLogger) formatRequestInfo(url, method string, headers map[string][]string, body []byte) string {
	var content strings.Builder
	content.WriteString("=== REQUEST INFO ===\n")
	content.WriteString(fmt.Sprintf("Version: %s\n", buildinfo.Version))
	content.WriteString(fmt.Sprintf("URL: %s\n", url))
	content.WriteString(fmt.Sprintf("Method: %s\n", method))
	content.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	content.WriteString("\n")

	content.WriteString("=== HEADERS ===\n")
	for key, values := range headers {
		for _, value := range values {
			content.WriteString(fmt.Sprintf("%s: %s\n", key, util.MaskSensitiveHeaderValue(key, value)))
		}
	}
	content.WriteString("\n")
	content.WriteString("=== REQUEST BODY ===\n")
	content.Write(body)
	content.WriteString("\n\n")
	return content.String()
}
