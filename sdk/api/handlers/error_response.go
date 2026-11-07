package handlers

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// The functions here take *StreamErrorMessage, the sdk alias of
// interfaces.ErrorMessage, so that this file needs no internal import.

// WriteErrorResponseAs writes msg as an error body in format, the shape the
// client's protocol uses (see FormatErrorBody). The status may differ from the
// one msg carries: an over-long prompt is always a 400.
func (h *BaseAPIHandler) WriteErrorResponseAs(c *gin.Context, msg *StreamErrorMessage, format ErrorFormat) {
	h.writeErrorResponse(c, msg, func(status int, errText string) (int, []byte) {
		return FormatErrorBody(format, status, errText)
	})
}

// FormatErrorMessage is FormatErrorBody for msg, for errors written inside a
// stream after the response has started.
func FormatErrorMessage(format ErrorFormat, msg *StreamErrorMessage) (int, []byte) {
	status, errText := ErrorMessageText(msg)
	return FormatErrorBody(format, status, errText)
}

// ErrorMessageText returns the status and text of msg, with the defaults the
// error writers use when msg carries neither.
func ErrorMessageText(msg *StreamErrorMessage) (int, string) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}
	return status, errText
}

func (h *BaseAPIHandler) writeErrorResponse(c *gin.Context, msg *StreamErrorMessage, build func(status int, errText string) (int, []byte)) {
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.Cfg) {
		for key, values := range msg.Addon {
			if len(values) == 0 {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	status, errText := ErrorMessageText(msg)
	status, body := build(status, errText)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	var previous []byte
	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			previous = existingBytes
		}
	}
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := bytes.TrimSpace(body)
	if len(previous) > 0 {
		if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
			(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
			c.Set("API_RESPONSE", previous)
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}
