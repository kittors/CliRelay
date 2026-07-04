// Package gemini provides HTTP handlers for Gemini CLI API functionality.
// This package implements handlers that process CLI-specific requests for Gemini API operations,
// including content generation and streaming content generation endpoints.
// The handlers restrict access to localhost only and manage communication with the backend service.
package gemini

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"
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
func (h *GeminiCLIAPIHandler) CLIHandler(c *gin.Context) {
	if !isLocalGeminiCLIRequest(c.Request) {
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

func isLocalGeminiCLIRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	remoteIP := net.ParseIP(util.RemoteAddrIP(req.RemoteAddr))
	if remoteIP == nil || !remoteIP.IsLoopback() {
		return false
	}
	forwardedIPs, ok := geminiCLIForwardedIPs(req.Header)
	if !ok {
		return false
	}
	for _, ip := range forwardedIPs {
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

func geminiCLIForwardedIPs(headers http.Header) ([]net.IP, bool) {
	var ips []net.IP
	for _, header := range []string{"CF-Connecting-IP", "True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		for _, value := range headers.Values(header) {
			for _, candidate := range strings.Split(value, ",") {
				if strings.TrimSpace(candidate) == "" {
					continue
				}
				ip := parseGeminiCLINetworkIP(candidate)
				if ip == nil {
					return nil, false
				}
				ips = append(ips, ip)
			}
		}
	}

	for _, value := range headers.Values("Forwarded") {
		for _, entry := range strings.Split(value, ",") {
			for _, part := range strings.Split(entry, ";") {
				key, rawValue, ok := strings.Cut(part, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
					continue
				}
				if strings.TrimSpace(rawValue) == "" {
					continue
				}
				ip := parseGeminiCLINetworkIP(rawValue)
				if ip == nil {
					return nil, false
				}
				ips = append(ips, ip)
			}
		}
	}
	return ips, true
}

func parseGeminiCLINetworkIP(candidate string) net.IP {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return nil
	}
	candidate = strings.Trim(candidate, `"`)
	if candidate == "" || strings.EqualFold(candidate, "unknown") {
		return nil
	}
	if strings.HasPrefix(candidate, "[") && strings.Contains(candidate, "]") {
		end := strings.Index(candidate, "]")
		if end >= 0 {
			candidate = candidate[1:end]
		}
	} else if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	return net.ParseIP(strings.TrimSpace(candidate))
}

// handleInternalStreamGenerateContent handles streaming content generation requests.
// It sets up a server-sent event stream and forwards the request to the backend client.
// The function continuously proxies response chunks from the backend to the client.
func (h *GeminiCLIAPIHandler) handleInternalStreamGenerateContent(c *gin.Context, rawJSON []byte) {
	alt := h.GetAlt(c)

	if alt == "" {
		handlers.PrepareStreamingResponse(c)
	}

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
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
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
