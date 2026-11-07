package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Routing simulation.
//
// A simulated request goes through the stages the /v1 routes run
// (v1RequestStages), on an engine of its own, so the key's authentication, its
// route path, the channel-group check, the model policy and the model gate
// decide exactly as they would for a client. Where the handler would hand the
// request to the conductor, the simulation asks for a simulated selection
// instead (BaseAPIHandler.SimulateRouting), so nothing is sent upstream.
//
// The one stage it skips is quota admission, which counts the request against
// the key's limits and holds a concurrency slot. The others only read: the
// model policy reads usage, the system prompt stage rewrites the in-memory body.

// stageHandler is the inference handler: it resolves the providers that serve
// the model and selects a credential.
const stageHandler = "handler"

var errRoutingSimulationUnavailable = errors.New("routing simulation unavailable")

// routingSimulationPaths are the paths simulated requests are sent to.
var routingSimulationPaths = map[string]string{
	managementHandlers.RoutingSimulationEndpointChat:     "/v1/chat/completions",
	managementHandlers.RoutingSimulationEndpointMessages: "/v1/messages",
}

// routingSimulation is the engine simulated requests run on and the stages it
// runs, in order.
type routingSimulation struct {
	engine *gin.Engine
	stages []requestStage
}

// newRoutingSimulation builds the simulation engine. It is built with the live
// routes, from the same stage list, and reads configuration at request time as
// they do.
func (s *Server) newRoutingSimulation() *routingSimulation {
	stages := s.v1RequestStages(rewrittenGroupRoutingMiddleware(s.resolveRoute))
	engine := gin.New()
	engine.Use(middleware.RequestBodyCleanupMiddleware())
	engine.Use(captureRoutingSimulation)
	for _, stage := range stages {
		if !stage.writesState {
			engine.Use(trackRoutingSimulationStage(stage.name, stage.handler))
		}
	}
	engine.POST(routingSimulationPaths[managementHandlers.RoutingSimulationEndpointChat], func(c *gin.Context) {
		s.simulateHandlerRouting(c, openai.ChatCompletionsModel, handlers.ErrorFormatOpenAI)
	})
	engine.POST(routingSimulationPaths[managementHandlers.RoutingSimulationEndpointMessages], func(c *gin.Context) {
		s.simulateHandlerRouting(c, claude.MessagesModel, handlers.ErrorFormatClaude)
	})
	return &routingSimulation{engine: engine, stages: stages}
}

// routingSimulationOutcome collects what one simulated request went through.
// It travels in the request context.
type routingSimulationOutcome struct {
	entered    map[string]bool
	rejectedBy string
	model      string
	selection  *coreauth.SelectionSimulation
	diagnostic diagnostics.Snapshot
	response   *simulationResponseWriter
}

type routingSimulationOutcomeKey struct{}

func routingSimulationOutcomeOf(c *gin.Context) *routingSimulationOutcome {
	outcome, _ := c.Request.Context().Value(routingSimulationOutcomeKey{}).(*routingSimulationOutcome)
	return outcome
}

// captureRoutingSimulation wraps the stages and keeps the request's diagnostic
// once they are done: route, model policy and the rejection, if any.
func captureRoutingSimulation(c *gin.Context) {
	diagnostics.EnsureGin(c, "")
	c.Next()
	outcome := routingSimulationOutcomeOf(c)
	if outcome == nil {
		return
	}
	if outcome.rejectedBy != "" {
		// Stages that answer with gin's JSON helpers leave no diagnostic of
		// their own; parse the error body they wrote.
		diagnostics.RecordResponse(c, c.Writer.Status(), outcome.response.body.Bytes())
	}
	if d := diagnostics.FromGin(c); d != nil {
		outcome.diagnostic = d.Snapshot()
	}
}

// trackRoutingSimulationStage records that the stage ran and whether it was
// the one that stopped the request. A stage that lets the request through
// calls c.Next, so the rest of the chain runs inside handler; the first
// wrapper to see the request aborted on the way out is the stage that aborted.
func trackRoutingSimulationStage(name string, handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		outcome := routingSimulationOutcomeOf(c)
		if outcome != nil {
			outcome.entered[name] = true
		}
		handler(c)
		if outcome != nil && c.IsAborted() && outcome.rejectedBy == "" {
			outcome.rejectedBy = name
		}
	}
}

// simulateHandlerRouting stands in for an inference handler. It reads the body
// and the model the handler would run it with, then asks for the selection the
// handler's execution would make. Errors are written the way the handler
// writes them.
func (s *Server) simulateHandlerRouting(c *gin.Context, modelOf func(*gin.Context, []byte) string, format handlers.ErrorFormat) {
	outcome := routingSimulationOutcomeOf(c)
	if outcome == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	outcome.entered[stageHandler] = true
	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		outcome.rejectedBy = stageHandler
		return
	}
	outcome.model = modelOf(c, rawJSON)
	// The handlers pass the request context with the gin context attached;
	// SimulateRouting reads the route and the key's scope from it.
	ctx := context.WithValue(c.Request.Context(), util.ContextKeyGin, c)
	selection, errMsg := s.handlers.SimulateRouting(ctx, outcome.model)
	if errMsg != nil {
		s.handlers.WriteErrorResponseAs(c, errMsg, format)
		outcome.rejectedBy = stageHandler
		return
	}
	outcome.selection = selection
	c.Status(http.StatusOK)
}

// simulateRouting runs one simulated request. Set as the management handler's
// routing simulator.
func (s *Server) simulateRouting(ctx context.Context, req managementHandlers.RoutingSimulationRequest) (*managementHandlers.RoutingSimulationResult, error) {
	if s == nil || s.routingSimulation == nil || s.handlers == nil {
		return nil, errRoutingSimulationUnavailable
	}
	path, ok := routingSimulationPaths[req.Endpoint]
	if !ok {
		return nil, fmt.Errorf("unknown routing simulation endpoint %q", req.Endpoint)
	}
	body, err := json.Marshal(simulatedRequestBody(req))
	if err != nil {
		return nil, err
	}
	outcome := &routingSimulationOutcome{entered: make(map[string]bool), response: &simulationResponseWriter{header: make(http.Header)}}
	reqCtx := context.WithValue(ctx, routingSimulationOutcomeKey{}, outcome)
	if routePath := strings.TrimSpace(req.RoutePath); routePath != "" {
		// The way NoRoute hands a group path to rewrittenGroupRoutingMiddleware.
		// A single-segment path resolves the same here as under /:group/v1.
		reqCtx = context.WithValue(reqCtx, rewrittenPathRouteContextKey{}, routePath)
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	s.routingSimulation.engine.ServeHTTP(outcome.response, httpReq)
	return outcome.result(s.routingSimulation.stages), nil
}

// simulatedRequestBody is the smallest valid body for the endpoint.
func simulatedRequestBody(req managementHandlers.RoutingSimulationRequest) map[string]any {
	body := map[string]any{
		"model":    req.Model,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	}
	if req.Endpoint == managementHandlers.RoutingSimulationEndpointMessages {
		body["max_tokens"] = 1
	}
	return body
}

func (o *routingSimulationOutcome) result(stages []requestStage) *managementHandlers.RoutingSimulationResult {
	out := &managementHandlers.RoutingSimulationResult{
		Route:       o.diagnostic.Route,
		ModelPolicy: o.diagnostic.ModelPolicy,
		Model:       o.model,
		Selection:   o.selection,
	}
	names := make([]string, 0, len(stages)+1)
	skipped := make(map[string]bool)
	for _, stage := range stages {
		names = append(names, stage.name)
		if stage.writesState {
			skipped[stage.name] = true
		}
	}
	names = append(names, stageHandler)
	for _, name := range names {
		outcome := managementHandlers.RoutingStageNotRun
		switch {
		case skipped[name]:
			outcome = managementHandlers.RoutingStageSkipped
		case name == o.rejectedBy:
			outcome = managementHandlers.RoutingStageRejected
		case o.entered[name]:
			outcome = managementHandlers.RoutingStagePassed
		}
		out.Stages = append(out.Stages, managementHandlers.RoutingSimulationStage{Name: name, Outcome: outcome})
	}
	if o.rejectedBy != "" {
		out.Rejection = o.diagnostic.Response
		if out.Rejection == nil {
			out.Rejection = &diagnostics.ResponseSnapshot{Status: o.response.status}
		}
	}
	return out
}

// simulationResponseWriter keeps what a simulated request is answered with.
type simulationResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *simulationResponseWriter) Header() http.Header { return w.header }

func (w *simulationResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *simulationResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}
