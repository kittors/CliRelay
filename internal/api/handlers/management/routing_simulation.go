package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Routing simulation: where a request from an API key for a model would go,
// and why, without sending it. The API server runs the simulation (it owns the
// inference middleware); this file is the management endpoint around it.

// Endpoints a simulated request can be sent to. They differ in which CC Switch
// preset mappings apply: a preset renames models only for the protocol its
// client speaks.
const (
	RoutingSimulationEndpointChat     = "chat-completions"
	RoutingSimulationEndpointMessages = "messages"
)

// What a middleware stage did with the simulated request. The panel keys its
// wording on these values: add new ones, never rename.
const (
	RoutingStagePassed   = "passed"
	RoutingStageRejected = "rejected"
	// RoutingStageSkipped is a stage the simulation does not run because it
	// would change state (quota admission).
	RoutingStageSkipped = "skipped"
	// RoutingStageNotRun is a stage after the one that rejected the request.
	RoutingStageNotRun = "not-run"
)

// RoutingSimulationRequest is one request to simulate.
type RoutingSimulationRequest struct {
	// APIKey is the key's plaintext. It never leaves the process: the
	// simulation authenticates with it the way a client would.
	APIKey string
	Model  string
	// RoutePath is the group path the request is sent under ("/team" for
	// /team/v1/...); empty for the plain /v1 routes.
	RoutePath string
	Endpoint  string
}

// RoutingSimulationStage is what one stage did with the request.
type RoutingSimulationStage struct {
	Name    string
	Outcome string
}

// RoutingSimulationResult is what the inference stages and the conductor
// would do with the request.
type RoutingSimulationResult struct {
	// Stages are the middleware stages in the order they run, ending with the
	// handler that hands the request to the conductor.
	Stages []RoutingSimulationStage
	// Rejection is the response the request gets when a stage refuses it.
	Rejection *diagnostics.ResponseSnapshot
	// Route is the group route the request resolved to, if any.
	Route *diagnostics.RouteSnapshot
	// ModelPolicy is set when the key's model policy rewrote the model.
	ModelPolicy *diagnostics.ModelPolicySnapshot
	// Model is the model the handler selects credentials for: the requested
	// one after the model policy and the route's CC Switch mapping.
	Model string
	// Selection is how the conductor would place the request; nil when a stage
	// rejected it first.
	Selection *coreauth.SelectionSimulation
}

// RoutingSimulator runs one simulated request.
type RoutingSimulator func(ctx context.Context, req RoutingSimulationRequest) (*RoutingSimulationResult, error)

// SetRoutingSimulator wires the simulation GET /routing-simulation runs.
func (h *Handler) SetRoutingSimulator(simulator RoutingSimulator) { h.routingSimulator = simulator }

// routingSimulationAvailable tells the panel, through GET /routing-config,
// whether it can offer the simulator.
func (h *Handler) routingSimulationAvailable() bool {
	return h != nil && h.routingSimulator != nil
}

// GetRoutingSimulation answers GET /routing-simulation?api-key-id=&model=
// [&route-path=][&endpoint=].
//
// The route itself needs routing.read. The answer also shows the key's
// restrictions and the tenant's credentials with their cooldowns and quota
// windows, which the API keys and AI accounts pages guard with api_keys.read
// and auth_files.read, so it needs those as well.
func (h *Handler) GetRoutingSimulation(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.read") || !principal.Has("auth_files.read") {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	if !h.routingSimulationAvailable() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "routing_simulation_unavailable", "message": "routing simulation is not available"}})
		return
	}
	keyID := strings.TrimSpace(c.Query("api-key-id"))
	model := strings.TrimSpace(c.Query("model"))
	if keyID == "" || model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": "api-key-id and model are required"}})
		return
	}
	endpoint := strings.TrimSpace(c.DefaultQuery("endpoint", RoutingSimulationEndpointChat))
	if endpoint != RoutingSimulationEndpointChat && endpoint != RoutingSimulationEndpointMessages {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": "endpoint must be chat-completions or messages"}})
		return
	}
	routePath := strings.TrimSpace(c.Query("route-path"))

	row := usage.GetAPIKeyByIDForTenant(effectiveTenantID(c), keyID)
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "api_key_not_found", "message": "api key not found"}})
		return
	}
	result, err := h.routingSimulator(c.Request.Context(), RoutingSimulationRequest{
		APIKey:    row.Key,
		Model:     model,
		RoutePath: routePath,
		Endpoint:  endpoint,
	})
	if err != nil {
		log.WithError(err).Warn("routing simulation failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "routing_simulation_failed", "message": "routing simulation failed"}})
		return
	}
	c.JSON(http.StatusOK, newRoutingSimulationResponse(routingSimulationKey{ID: row.ID, Name: row.Name, Disabled: row.Disabled}, endpoint, model, routePath, result))
}
