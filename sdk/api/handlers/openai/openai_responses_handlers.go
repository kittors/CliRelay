// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAIResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewOpenAIResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewOpenAIResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIResponsesAPIHandler) HandlerType() string {
	return OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *OpenAIResponsesAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   h.Models(),
	})
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return
	}

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}
}

func (h *OpenAIResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return
	}

	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for compact responses",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if streamResult.Exists() {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.executeCompactWithFallback(cliCtx, modelName, rawJSON)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

var compactFallbackModels = []string{
	"gpt-5.5",
	"codex-auto-review",
	"gpt-5.4",
	"gpt-5.1-codex-max",
	"gpt-5.3-codex",
}

func (h *OpenAIResponsesAPIHandler) executeCompactWithFallback(cliCtx context.Context, requestedModel string, rawJSON []byte) ([]byte, http.Header, *interfaces.ErrorMessage) {
	models := compactAttemptModels(requestedModel)
	var lastErr *interfaces.ErrorMessage
	for idx, candidateModel := range models {
		attemptJSON := rawJSON
		if candidateModel != requestedModel {
			updated, err := sjson.SetBytes(rawJSON, "model", candidateModel)
			if err != nil {
				return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
			}
			attemptJSON = updated
		}
		resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), candidateModel, attemptJSON, "responses/compact")
		if errMsg == nil {
			return resp, upstreamHeaders, nil
		}
		lastErr = errMsg
		if idx == 0 && !shouldRetryCompactWithFallback(errMsg) {
			return nil, nil, errMsg
		}
	}
	return nil, nil, lastErr
}

func compactAttemptModels(requestedModel string) []string {
	models := make([]string, 0, 1+len(compactFallbackModels))
	seen := make(map[string]struct{}, 1+len(compactFallbackModels))
	addModel := func(model string, requireCompactProvider bool) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		if _, exists := seen[model]; exists {
			return
		}
		if requireCompactProvider && !modelHasCompactCapableProvider(model) {
			return
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	addModel(requestedModel, true)
	for _, model := range compactFallbackModels {
		addModel(model, true)
	}
	return models
}

func modelHasCompactCapableProvider(model string) bool {
	for _, provider := range util.GetProviderName(model) {
		if providerSupportsCompact(provider) {
			return true
		}
	}
	return false
}

func providerSupportsCompact(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

func shouldRetryCompactWithFallback(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	switch errMsg.StatusCode {
	case http.StatusNotFound, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusNotImplemented:
		return true
	}
	if errMsg.Error == nil {
		return false
	}
	errText := strings.ToLower(strings.TrimSpace(errMsg.Error.Error()))
	return strings.Contains(errText, "responses/compact") || strings.Contains(errText, "unknown provider")
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse handles streaming responses for Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// New core execution path
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")

	setSSEHeaders := func() {
		handlers.PrepareStreamingResponse(c)
	}

	// Peek at the first chunk
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			// Upstream failed immediately. Return proper error status and JSON.
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				// Stream closed without data? Send headers and done.
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				cliCancel(nil)
				return
			}

			// Success! Set headers.
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

			// Write first chunk logic (matching forwardResponsesStream)
			if bytes.HasPrefix(chunk, []byte("event:")) {
				_, _ = c.Writer.Write([]byte("\n"))
			}
			_, _ = c.Writer.Write(chunk)
			_, _ = c.Writer.Write([]byte("\n"))
			flusher.Flush()

			// Continue
			h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan)
			return
		}
	}
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			if bytes.HasPrefix(chunk, []byte("event:")) {
				_, _ = c.Writer.Write([]byte("\n"))
			}
			_, _ = c.Writer.Write(chunk)
			_, _ = c.Writer.Write([]byte("\n"))
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && errMsg.Error.Error() != "" {
				errText = errMsg.Error.Error()
			}
			chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
			_, _ = fmt.Fprintf(c.Writer, "\nevent: error\ndata: %s\n\n", string(chunk))
		},
		WriteDone: func() {
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}
