// Package gemini provides HTTP handlers for Gemini CLI API functionality.
// This package implements handlers that process CLI-specific requests for Gemini API operations,
// including content generation and streaming content generation endpoints.
// The handlers restrict access to localhost only and manage communication with the backend service.
package gemini

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	geminiCLIBridgeResponseLimit = 64 << 20
	geminiCLIBridgeErrorLimit    = 2 << 20
)

// GeminiCLIAPIHandler contains the handlers for Gemini CLI API endpoints.
// It holds a pool of clients to interact with the backend service.
type GeminiCLIAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewGeminiCLIAPIHandler creates a new Gemini CLI API handlers instance.
// It takes an BaseAPIHandler instance as input and returns a GeminiCLIAPIHandler.
func NewGeminiCLIAPIHandler(apiHandlers *handlers.BaseAPIHandler) *GeminiCLIAPIHandler {
	return &GeminiCLIAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the type of this handler.
func (h *GeminiCLIAPIHandler) HandlerType() string {
	return GeminiCLI
}

// Models returns a list of models supported by this handler.
func (h *GeminiCLIAPIHandler) Models() []map[string]any {
	return make([]map[string]any, 0)
}

// CLIHandler handles CLI-specific requests for Gemini API operations.
// It restricts access to localhost only and routes requests to appropriate internal handlers.
//
// This route carries no API key of its own (it is not registered under an authenticated
// route group), so the local-origin check below is the only thing standing between a
// caller and the server's Gemini credential pool. It must stay fail-closed.
func (h *GeminiCLIAPIHandler) CLIHandler(c *gin.Context) {
	if !util.IsLocalOriginRequest(c.Request) {
		// Logged because this endpoint is scanned for in the wild and a silent 403
		// leaves operators with no signal that someone is probing it.
		if relayHeader := util.RelayIndicationHeader(c.Request); relayHeader != "" {
			log.Warnf("gemini cli: rejected non-local request from %s (relayed via %s header); configure trusted-proxies and keep /v1internal off public reverse proxies", c.Request.RemoteAddr, relayHeader)
		} else {
			log.Warnf("gemini cli: rejected non-local request from %s", c.Request.RemoteAddr)
		}
		c.JSON(http.StatusForbidden, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "CLI reply only allow local access",
				Type:    "forbidden",
			},
		})
		return
	}

	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return
	}
	requestRawURI := c.Request.URL.Path

	if requestRawURI == "/v1internal:generateContent" {
		h.handleInternalGenerateContent(c, rawJSON)
	} else if requestRawURI == "/v1internal:streamGenerateContent" {
		h.handleInternalStreamGenerateContent(c, rawJSON)
	} else {
		reqBody := bytes.NewBuffer(rawJSON)
		req, err := http.NewRequest("POST", fmt.Sprintf("https://cloudcode-pa.googleapis.com%s", c.Request.URL.RequestURI()), reqBody)
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		for key, value := range c.Request.Header {
			req.Header[key] = value
		}

		httpClient := util.SetProxy(h.Cfg, util.NewHTTPClient(util.DefaultHTTPClientTimeout))

		resp, err := httpClient.Do(req)
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			})
			return
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			defer func() {
				if err = resp.Body.Close(); err != nil {
					log.Printf("warn: failed to close response body: %v", err)
				}
			}()
			bodyBytes, errReadBody := bodyutil.ReadAll(resp.Body, geminiCLIBridgeErrorLimit)
			if errReadBody != nil {
				if bodyutil.IsTooLarge(errReadBody) {
					bodyBytes = []byte("upstream error body too large")
				} else {
					bodyBytes = []byte(fmt.Sprintf("failed to read upstream error body: %v", errReadBody))
				}
			}

			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: string(bodyBytes),
					Type:    "invalid_request_error",
				},
			})
			return
		}

		defer func() {
			_ = resp.Body.Close()
		}()

		for key, value := range resp.Header {
			c.Header(key, value[0])
		}
		output, err := bodyutil.ReadAll(resp.Body, geminiCLIBridgeResponseLimit)
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
					Error: handlers.ErrorDetail{
						Message: "Upstream response body too large",
						Type:    "server_error",
					},
				})
				return
			}
			log.Errorf("Failed to read response body: %v", err)
			return
		}
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
		_, _ = c.Writer.Write(output)
		c.Set("API_RESPONSE", output)
	}
}

// handleInternalStreamGenerateContent handles streaming content generation requests.
// It sets up a server-sent event stream and forwards the request to the backend client.
// The function continuously proxies response chunks from the backend to the client.
func (h *GeminiCLIAPIHandler) handleInternalStreamGenerateContent(c *gin.Context, rawJSON []byte) {
	alt := h.GetAlt(c)

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

	modelResult := gjson.GetBytes(rawJSON, "model")
	modelName := modelResult.String()

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
	dataChan, upstreamHeaders, errChan, startErr := h.StartStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	if startErr != nil {
		h.WriteErrorResponse(c, startErr)
		cliCancel(startErr.Error)
		return
	}
	if alt == "" {
		handlers.PrepareStreamingResponse(c)
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	h.forwardCLIStream(c, flusher, "", func(err error) { cliCancel(err) }, dataChan, errChan)
}

// handleInternalGenerateContent handles non-streaming content generation requests.
// It sends a request to the backend client and proxies the entire response back to the client at once.
func (h *GeminiCLIAPIHandler) handleInternalGenerateContent(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")
	modelResult := gjson.GetBytes(rawJSON, "model")
	modelName := modelResult.String()

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func (h *GeminiCLIAPIHandler) forwardCLIStream(c *gin.Context, flusher http.Flusher, alt string, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
	var keepAliveInterval *time.Duration
	if alt != "" {
		keepAliveInterval = new(time.Duration(0))
	}

	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		KeepAliveInterval: keepAliveInterval,
		WriteChunk: func(chunk []byte) {
			if alt == "" {
				if bytes.Equal(chunk, []byte("data: [DONE]")) || bytes.Equal(chunk, []byte("[DONE]")) {
					return
				}

				if !bytes.HasPrefix(chunk, []byte("data:")) {
					_, _ = c.Writer.Write([]byte("data: "))
				}

				_, _ = c.Writer.Write(chunk)
				_, _ = c.Writer.Write([]byte("\n\n"))
			} else {
				_, _ = c.Writer.Write(chunk)
			}
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
			body := handlers.BuildErrorResponseBody(status, errText)
			if alt == "" {
				_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", string(body))
			} else {
				_, _ = c.Writer.Write(body)
			}
		},
	})
}
