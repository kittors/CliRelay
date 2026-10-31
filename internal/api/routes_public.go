package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
)

// setupRoutes configures the API routes for the server.
// It defines the endpoints and associates them with their respective handlers.
func (s *Server) setupRoutes() {
	s.engine.GET("/healthz", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	s.engine.GET("/readyz", s.handleReadyz)
	s.engine.GET("/readyz/egress", s.handleReadyzEgress)
	s.engine.GET(observability.MetricsPath, s.metricsHandler())

	s.engine.GET("/management.html", s.serveManagementControlPanel)
	s.engine.GET("/manage", s.serveManagementControlPanel)
	s.engine.GET("/manage/*filepath", s.serveManagementControlPanel)

	openaiHandlers := openai.NewOpenAIAPIHandler(s.handlers)
	geminiHandlers := gemini.NewGeminiAPIHandler(s.handlers)
	geminiCLIHandlers := gemini.NewGeminiCLIAPIHandler(s.handlers)
	claudeCodeHandlers := claude.NewClaudeCodeAPIHandler(s.handlers)
	openaiResponsesHandlers := openai.NewOpenAIResponsesAPIHandler(s.handlers)
	openaiImagesHandlers := openai.NewOpenAIImagesAPIHandler(s.handlers)
	openaiVideosHandlers := openai.NewOpenAIVideosAPIHandler(s.handlers)

	registerV1Routes := func(group *gin.RouterGroup) {
		group.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		group.POST("/chat/completions", openaiHandlers.ChatCompletions)
		group.POST("/completions", openaiHandlers.Completions)
		group.POST("/images/generations", openaiImagesHandlers.Generations)
		group.POST("/images/edits", openaiImagesHandlers.Edits)
		group.POST("/videos/generations", openaiVideosHandlers.Generations)
		group.GET("/videos/:request_id", openaiVideosHandlers.Status)
		group.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		group.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		group.GET("/responses", func(c *gin.Context) {
			clearServerWriteDeadline(c)
			openaiResponsesHandlers.ResponsesWebsocket(c)
		})
		group.POST("/responses", openaiResponsesHandlers.Responses)
		group.POST("/responses/compact", openaiResponsesHandlers.Compact)
		group.POST("/alpha/search", openaiResponsesHandlers.AlphaSearch)
	}
	registerV1BetaRoutes := func(group *gin.RouterGroup) {
		group.GET("/models", geminiHandlers.GeminiModels)
		group.POST("/models/*action", geminiHandlers.GeminiHandler)
		group.GET("/models/*action", geminiHandlers.GeminiGetHandler)
	}
	v1 := s.engine.Group("/v1")
	useRequestStages(v1, s.v1RequestStages(rewrittenGroupRoutingMiddleware(s.resolveRoute)))
	registerV1Routes(v1)

	groupedV1 := s.engine.Group("/:group/v1")
	useRequestStages(groupedV1, s.v1RequestStages(groupRoutingMiddleware(s.resolveRoute)))
	registerV1Routes(groupedV1)

	// Codex CLI direct route aliases (chatgpt_base_url compatible). Upstream
	// CLIProxyAPI exposes these; clients such as the Codex CLI and the pi
	// CLIProxyAPI provider address inference as {root}/backend-api/codex/...
	codexDirect := s.engine.Group("/backend-api/codex")
	useRequestStages(codexDirect, s.v1RequestStages(rewrittenGroupRoutingMiddleware(s.resolveRoute)))
	{
		codexDirect.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		codexDirect.GET("/responses", func(c *gin.Context) {
			clearServerWriteDeadline(c)
			openaiResponsesHandlers.ResponsesWebsocket(c)
		})
		codexDirect.POST("/responses", openaiResponsesHandlers.Responses)
		codexDirect.POST("/responses/compact", openaiResponsesHandlers.Compact)
		codexDirect.POST("/alpha/search", openaiResponsesHandlers.AlphaSearch)
	}

	v1beta := s.engine.Group("/v1beta")
	useRequestStages(v1beta, s.requestStages(rewrittenGroupRoutingMiddleware(s.resolveRoute)))
	registerV1BetaRoutes(v1beta)

	groupedV1Beta := s.engine.Group("/:group/v1beta")
	useRequestStages(groupedV1Beta, s.requestStages(groupRoutingMiddleware(s.resolveRoute)))
	registerV1BetaRoutes(groupedV1Beta)

	// Built from the same stage list as /v1 above; see routing_simulation.go.
	s.routingSimulation = s.newRoutingSimulation()

	s.engine.NoRoute(func(c *gin.Context) {
		if _, rewritten := c.Get("cliproxy.grouped_path_rewrite"); rewritten {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		rawGroupPath, apiPath, ok := splitGroupedAPIPath(c.Request.URL.Path)
		if !ok {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Set(rewrittenPathRouteGroupKey, rawGroupPath)
		c.Set("cliproxy.grouped_path_rewrite", true)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), rewrittenPathRouteContextKey{}, rawGroupPath))
		c.Request.URL.Path = apiPath
		if c.Request.URL.RawQuery != "" {
			c.Request.RequestURI = apiPath + "?" + c.Request.URL.RawQuery
			diagnostics.SetEffectiveURL(c, apiPath+"?"+util.MaskSensitiveQuery(c.Request.URL.RawQuery))
		} else {
			c.Request.RequestURI = apiPath
			diagnostics.SetEffectiveURL(c, apiPath)
		}
		c.Status(http.StatusOK)
		s.engine.HandleContext(c)
		c.Abort()
	})

	s.engine.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "CLI Proxy API Server",
			"endpoints": []string{
				"POST /v1/chat/completions",
				"POST /v1/completions",
				"POST /v1/images/generations",
				"POST /v1/videos/generations",
				"GET /v1/models",
			},
		})
	})
	s.engine.POST("/v1internal:method", geminiCLIHandlers.CLIHandler)

	s.registerOAuthCallbackRoutes()

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}
