package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

func (s *Server) modelRestrictionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost:
		case http.MethodGet:
			// A WebSocket upgrade (GET /responses) carries no body; each turn's
			// model arrives later inside a frame. Publish the gate so the
			// frame-driven handler applies the same rules per turn.
			if isWebsocketUpgrade(c.Request) {
				if gate := s.buildModelGate(c); gate != nil {
					c.Set(handlers.ModelGateContextKey, gate)
				}
			}
			c.Next()
			return
		default:
			c.Next()
			return
		}

		gate := s.buildModelGate(c)
		if gate == nil {
			c.Next()
			return
		}

		requestedModel, err := extractRequestedModel(c)
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			if bodyutil.IsStorageUnavailable(err) {
				c.AbortWithStatusJSON(http.StatusInsufficientStorage, gin.H{"error": "request body temporary storage unavailable"})
				return
			}
			c.Next()
			return
		}
		if requestedModel == "" {
			c.Next()
			return
		}
		if verdict := gate(requestedModel); verdict != nil {
			c.AbortWithStatusJSON(verdict.StatusCode, modelGateErrorBody(verdict))
			return
		}
		c.Next()
	}
}

// isWebsocketUpgrade reports whether the request asks to switch protocols.
func isWebsocketUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// buildModelGate resolves every model restriction that applies to this request
// into one verdict function. Returns nil when nothing is restricted, so the
// caller can skip reading the body.
//
// The verdict is an *interfaces.ErrorMessage whose Error is a *modelGateError,
// so both the HTTP path and the WebSocket path can render the same code/type.
func (s *Server) buildModelGate(c *gin.Context) handlers.ModelGate {
	allowedStr := ""
	if metadataVal, exists := c.Get("accessMetadata"); exists {
		if metadata, ok := metadataVal.(map[string]string); ok {
			allowedStr = strings.TrimSpace(metadata["allowed-models"])
		}
	}
	route := pathRouteContextFromGin(c)
	routeGroup := ""
	if route != nil {
		routeGroup = route.Group
	}
	allowedGroups := allowedChannelGroupsFromAccessMetadata(c)

	var ccSwitchAllowed map[string]struct{}
	if route != nil && route.CcSwitch != nil {
		ccSwitchAllowed = make(map[string]struct{})
		for _, mapping := range route.CcSwitch.ModelMappings {
			if t := strings.TrimSpace(mapping.TargetModel); t != "" {
				ccSwitchAllowed[t] = struct{}{}
			}
			if r := strings.TrimSpace(mapping.RequestModel); r != "" {
				ccSwitchAllowed[r] = struct{}{}
			}
		}
		if dm := strings.TrimSpace(route.CcSwitch.DefaultModel); dm != "" {
			ccSwitchAllowed[dm] = struct{}{}
		}
		if len(ccSwitchAllowed) == 0 {
			ccSwitchAllowed = nil
		}
	}

	tenantID := requestTenantID(c)
	hasScopedRestriction := s.hasScopedRoutingModelRestrictionForTenant(tenantID, routeGroup, allowedGroups)
	// A model switched off in the catalog is refused for everyone, so this
	// middleware has to inspect the body even when the key itself carries no
	// allowed-models restriction.
	disabledModels := modelconfigsettings.DisabledModelIDsForTenant(tenantID)
	if allowedStr == "" && !hasScopedRestriction && ccSwitchAllowed == nil && len(disabledModels) == 0 {
		return nil
	}

	var allowedModels map[string]struct{}
	if allowedStr != "" {
		allowedModels = make(map[string]struct{})
		for _, m := range strings.Split(allowedStr, ",") {
			trimmed := strings.TrimSpace(m)
			if trimmed != "" {
				allowedModels[trimmed] = struct{}{}
			}
		}
	}
	if ccSwitchAllowed != nil {
		if len(allowedModels) == 0 {
			allowedModels = ccSwitchAllowed
		} else {
			for m := range allowedModels {
				if _, ok := ccSwitchAllowed[m]; !ok {
					delete(allowedModels, m)
				}
			}
		}
	}

	return func(model string) *interfaces.ErrorMessage {
		requestedModel := strings.TrimSpace(model)
		if requestedModel == "" {
			return nil
		}
		routingModel := mapCcSwitchRequestModelForRestriction(requestedModel, route)

		// Check the model actually routed to as well, so a CC Switch mapping cannot
		// reach a disabled target through an enabled request alias.
		if modelconfigsettings.IsDisabled(requestedModel, disabledModels) || modelconfigsettings.IsDisabled(routingModel, disabledModels) {
			return modelDisabledError(requestedModel)
		}
		if len(allowedModels) > 0 && !modelInSet(requestedModel, allowedModels) && !modelInSet(routingModel, allowedModels) {
			return modelNotAllowedError(requestedModel)
		}
		if !s.modelAllowedByScopedRoutingGroupsForTenant(tenantID, routingModel, routeGroup, allowedGroups) {
			return modelNotAllowedError(requestedModel)
		}
		return nil
	}
}

// modelGateError carries the OpenAI-style error triple so the HTTP and
// WebSocket paths render an identical body.
type modelGateError struct {
	message string
	typ     string
	code    string
}

func (e *modelGateError) Error() string { return e.message }

// modelNotAllowedError is an authorization failure: the key cannot use this model.
func modelNotAllowedError(model string) *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusForbidden,
		Error: &modelGateError{
			message: fmt.Sprintf("model '%s' is not allowed for this API key", model),
			typ:     "forbidden",
			code:    "model_not_allowed",
		},
	}
}

// modelDisabledError rejects a model the operator switched off in the catalog.
//
// This is not an authorization failure — the key may well be allowed to use it —
// so it reports the model as absent rather than forbidden, matching what the
// listing endpoints now say about it.
func modelDisabledError(model string) *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusNotFound,
		Error: &modelGateError{
			message: fmt.Sprintf("model '%s' is disabled", model),
			typ:     "not_found",
			code:    "model_not_found",
		},
	}
}

// modelGateErrorBody renders a gate verdict as the JSON body the HTTP path returns.
func modelGateErrorBody(verdict *interfaces.ErrorMessage) gin.H {
	body := map[string]interface{}{"message": "", "type": "error", "code": ""}
	if verdict != nil && verdict.Error != nil {
		body["message"] = verdict.Error.Error()
		if ge, ok := verdict.Error.(*modelGateError); ok {
			body["type"] = ge.typ
			body["code"] = ge.code
		}
	}
	return gin.H{"error": body}
}

func modelInSet(model string, allowed map[string]struct{}) bool {
	model = strings.TrimSpace(model)
	if model == "" || len(allowed) == 0 {
		return false
	}
	_, ok := allowed[model]
	return ok
}

func mapCcSwitchRequestModelForRestriction(model string, route *internalrouting.PathRouteContext) string {
	model = strings.TrimSpace(model)
	if model == "" || route == nil || route.CcSwitch == nil {
		return model
	}
	for _, mapping := range route.CcSwitch.ModelMappings {
		if strings.EqualFold(strings.TrimSpace(mapping.RequestModel), model) {
			target := strings.TrimSpace(mapping.TargetModel)
			if target != "" {
				return target
			}
		}
	}
	return model
}

func (s *Server) hasScopedRoutingModelRestriction(routeGroup string, allowedGroups map[string]struct{}) bool {
	return s.hasScopedRoutingModelRestrictionForTenant(identity.SystemTenantID, routeGroup, allowedGroups)
}

func (s *Server) hasScopedRoutingModelRestrictionForTenant(tenantID, routeGroup string, allowedGroups map[string]struct{}) bool {
	return !s.scopedRoutingModelGateForTenant(tenantID, routeGroup, allowedGroups).unrestricted
}

func (s *Server) modelAllowedByScopedRoutingGroups(model string, routeGroup string, allowedGroups map[string]struct{}) bool {
	return s.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, model, routeGroup, allowedGroups)
}

func (s *Server) modelAllowedByScopedRoutingGroupsForTenant(tenantID, model, routeGroup string, allowedGroups map[string]struct{}) bool {
	return s.scopedRoutingModelGateForTenant(tenantID, routeGroup, allowedGroups).allows(model)
}

// scopedRoutingModelGate applies the channel-group model gate to the /v1/models
// listing, mirroring the runtime gate in sdk/cliproxy/auth: a group with neither
// list serves every model its channels offer (including ones added upstream
// later), an allow list freezes the group to the models it names, and exclusions
// subtract from everything. Scoped groups form a union, so one permissive group
// is enough.
type scopedRoutingModelGate struct {
	unrestricted bool
	groups       []scopedRoutingModelGroupGate
}

type scopedRoutingModelGroupGate struct {
	allowed  []string
	excluded []string
}

func (g scopedRoutingModelGate) allows(model string) bool {
	if g.unrestricted {
		return true
	}
	if strings.TrimSpace(model) == "" {
		return false
	}
	for _, group := range g.groups {
		if len(group.excluded) > 0 && routeAllowedModelMatches(model, group.excluded) {
			continue
		}
		if len(group.allowed) == 0 || routeAllowedModelMatches(model, group.allowed) {
			return true
		}
	}
	return false
}

// scopedRoutingAllowedModelsForTenant returns the union of allow lists for the
// scoped groups, or nil when no allow list applies. Exclusion-only groups are
// not representable here, so restriction checks must use the gate instead.
func (s *Server) scopedRoutingAllowedModelsForTenant(tenantID, routeGroup string, allowedGroups map[string]struct{}) []string {
	gate := s.scopedRoutingModelGateForTenant(tenantID, routeGroup, allowedGroups)
	if gate.unrestricted {
		return nil
	}
	var allowedModels []string
	for _, group := range gate.groups {
		if len(group.allowed) == 0 {
			return nil
		}
		allowedModels = append(allowedModels, group.allowed...)
	}
	if len(allowedModels) == 0 {
		return nil
	}
	return allowedModels
}

func (s *Server) scopedRoutingModelGateForTenant(tenantID, routeGroup string, allowedGroups map[string]struct{}) scopedRoutingModelGate {
	unrestricted := scopedRoutingModelGate{unrestricted: true}
	routing := s.routingConfigForTenant(tenantID)
	if routing == nil {
		return unrestricted
	}
	scopedGroups := make(map[string]struct{})
	if routeGroup = internalrouting.NormalizeGroupName(routeGroup); routeGroup != "" {
		scopedGroups[routeGroup] = struct{}{}
	} else {
		for group := range allowedGroups {
			if normalized := internalrouting.NormalizeGroupName(group); normalized != "" {
				scopedGroups[normalized] = struct{}{}
			}
		}
	}
	if len(scopedGroups) == 0 {
		if routing.IncludeDefaultGroup {
			scopedGroups["default"] = struct{}{}
		}
	}
	if len(scopedGroups) == 0 {
		return unrestricted
	}

	var gate scopedRoutingModelGate
	for _, group := range routing.ChannelGroups {
		groupName := internalrouting.NormalizeGroupName(group.Name)
		if _, ok := scopedGroups[groupName]; !ok {
			continue
		}
		if len(group.AllowedModels) == 0 && len(group.ExcludedModels) == 0 {
			return unrestricted
		}
		gate.groups = append(gate.groups, scopedRoutingModelGroupGate{
			allowed:  group.AllowedModels,
			excluded: group.ExcludedModels,
		})
	}
	if len(gate.groups) == 0 {
		return unrestricted
	}
	return gate
}

// routingConfigForTenant prefers the tenant's DB-backed routing config so
// channel-group allowed-models apply per tenant, not only on the system cfg.
func (s *Server) routingConfigForTenant(tenantID string) *config.RoutingConfig {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = identity.SystemTenantID
	}
	if stored := usage.GetRoutingConfigForTenant(tenantID); stored != nil {
		return stored
	}
	if tenantID == identity.SystemTenantID && s != nil && s.cfg != nil {
		routing := s.cfg.Routing
		return &routing
	}
	return nil
}

// extractRequestedModel determines which model a request targets. Gemini-native
// endpoints (/v1beta/models/{model}:{method}) carry it in the URL path and send
// a body with no "model" field, so reading the body alone let every request to
// a disabled Gemini model through. OpenAI/Claude endpoints carry it in the body.
func extractRequestedModel(c *gin.Context) (string, error) {
	// Gemini-native path: registered as /models/*action, so Param("action") holds
	// e.g. "/gemini-2.5-flash:generateContent".
	if action := c.Param("action"); action != "" {
		model, _, _ := strings.Cut(strings.TrimPrefix(action, "/"), ":")
		if model = strings.TrimSpace(model); model != "" {
			return model, nil
		}
	}

	var bodyObj struct {
		Model string `json:"model"`
	}
	if err := bodyutil.DecodeJSONRequestBody(c, bodyutil.ModelRequestBodyLimit(), &bodyObj); err != nil {
		return "", err
	}
	return strings.TrimSpace(bodyObj.Model), nil
}

func routeAllowedModelMatches(model string, allowedModels []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, allowed := range allowedModels {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		// "*" matches everything, so one exclusion entry can block a whole group.
		if allowed == "*" {
			return true
		}
		if strings.EqualFold(model, allowed) {
			return true
		}
		if idx := strings.Index(model, "/"); idx >= 0 && strings.EqualFold(strings.TrimSpace(model[idx+1:]), allowed) {
			return true
		}
	}
	return false
}
