// Package logging provides Gin middleware for HTTP request logging and panic recovery.
// It integrates Gin web framework with logrus for structured logging of HTTP requests,
// responses, and error handling with panic recovery capabilities.
package logging

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// aiAPIPrefixes defines path prefixes for AI API requests that should have request ID tracking.
var aiAPIPrefixes = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/messages",
	"/v1/responses",
	"/v1beta/models/",
	"/api/provider/",
}

const skipGinLogKey = "__gin_skip_request_logging__"

type ginRequestLoggingContextKey struct{}

// GinLogrusLogger returns a Gin middleware handler that logs HTTP requests and responses
// using logrus. It captures request details including method, path, status code, latency,
// client IP, and any error messages. Request ID is generated for AI API and management
// paths (management needs it for audit-log correlation).
//
// Output format (tracked): [2025-12-23 20:14:10] [info ] | a1b2c3d4 | 200 |       23.559s | ...
// Output format (others):  [2025-12-23 20:14:10] [info ] | -------- | 200 |       23.559s | ...
//
// Returns:
//   - gin.HandlerFunc: A middleware handler for request logging
func GinLogrusLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if ginRequestLoggingActive(c) {
			ensureGinRequestID(c)
			c.Next()
			return
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ginRequestLoggingContextKey{}, true))

		start := time.Now()
		path := c.Request.URL.Path
		raw := util.MaskSensitiveQuery(c.Request.URL.RawQuery)

		// AI API + management: attach request ID for log/audit correlation.
		requestID := ensureGinRequestID(c)

		c.Next()

		if shouldSkipGinRequestLogging(c) {
			return
		}

		if raw != "" {
			path = path + "?" + raw
		}

		latency := time.Since(start)
		if latency > time.Minute {
			latency = latency.Truncate(time.Second)
		} else {
			latency = latency.Truncate(time.Millisecond)
		}

		statusCode := c.Writer.Status()
		clientIP := c.ClientIP()
		if ip, _ := util.ForwardedClientIP(c.Request); ip != "" {
			clientIP = ip
		}
		method := c.Request.Method
		errorMessage := c.Errors.ByType(gin.ErrorTypePrivate).String()

		if requestID == "" {
			requestID = GetGinRequestID(c)
		}
		if requestID == "" {
			requestID = "--------"
		}
		logLine := fmt.Sprintf("%3d | %13v | %15s | %-7s \"%s\"", statusCode, latency, clientIP, method, path)
		if errorMessage != "" {
			logLine = logLine + " | " + errorMessage
		}

		entry := log.WithField("request_id", requestID)

		switch {
		case statusCode >= http.StatusInternalServerError:
			entry.Error(logLine)
		case statusCode >= http.StatusBadRequest:
			entry.Warn(logLine)
		default:
			entry.Info(logLine)
		}
	}
}

func ensureGinRequestID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if requestID := GetGinRequestID(c); requestID != "" {
		return requestID
	}
	if !shouldTrackRequestID(c.Request.URL.Path) {
		return ""
	}
	requestID := GenerateRequestID()
	SetGinRequestID(c, requestID)
	c.Request = c.Request.WithContext(WithRequestID(c.Request.Context(), requestID))
	return requestID
}

func ginRequestLoggingActive(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	active, _ := c.Request.Context().Value(ginRequestLoggingContextKey{}).(bool)
	return active
}

// isAIAPIPath checks if the given path is an AI API endpoint that should have request ID tracking.
func isAIAPIPath(path string) bool {
	for _, prefix := range aiAPIPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// isManagementPath reports management console/API routes that need request IDs for audit logs.
func isManagementPath(path string) bool {
	return strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management")
}

// shouldTrackRequestID reports paths that receive a generated request ID.
func shouldTrackRequestID(path string) bool {
	return isAIAPIPath(path) || isManagementPath(path)
}

// GinLogrusRecovery returns a Gin middleware handler that recovers from panics and logs
// them using logrus. When a panic occurs, it captures the panic value, stack trace,
// and request path, then returns a 500 Internal Server Error response to the client.
//
// Returns:
//   - gin.HandlerFunc: A middleware handler for panic recovery
func GinLogrusRecovery() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			// Let net/http handle ErrAbortHandler so the connection is aborted without noisy stack logs.
			panic(http.ErrAbortHandler)
		}

		log.WithFields(log.Fields{
			"panic": recovered,
			"stack": string(debug.Stack()),
			"path":  c.Request.URL.Path,
		}).Error("recovered from panic")

		c.AbortWithStatus(http.StatusInternalServerError)
	})
}

// SkipGinRequestLogging marks the provided Gin context so that GinLogrusLogger
// will skip emitting a log line for the associated request.
func SkipGinRequestLogging(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(skipGinLogKey, true)
}

func shouldSkipGinRequestLogging(c *gin.Context) bool {
	if c == nil {
		return false
	}
	val, exists := c.Get(skipGinLogKey)
	if !exists {
		return false
	}
	flag, ok := val.(bool)
	return ok && flag
}
