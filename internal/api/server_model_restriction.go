package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
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
			// frame-driven handler applies the same rules per turn. Upgrades are
			// recognised with the same check the upgrader uses, so every
			// accepted upgrade is gated.
			if middleware.IsWebsocketUpgrade(c.Request) {
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

	// A CC Switch import preset adds no allow list of its own. Its mappings only
	// rename models (mapCcSwitchRequestModelForRestriction); what the route may
	// serve is the preset's channel group, checked below like any group path.
	// Reading the mappings as an allow list froze every preset at the models
	// picked when it was saved, so a model the upstream added later was refused
	// on the preset route while the group itself served it.

	tenantID := requestTenantID(c)
	hasScopedRestriction := s.hasScopedRoutingModelRestrictionForTenant(tenantID, routeGroup, allowedGroups)
	// A model switched off in the catalog is refused for everyone, so this
	// middleware has to inspect the body even when the key itself carries no
	// allowed-models restriction.
	disabledModels := modelconfigsettings.DisabledModelIDsForTenant(tenantID)
	if allowedStr == "" && !hasScopedRestriction && len(disabledModels) == 0 {
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
		// Built per call rather than once per gate: a WebSocket connection keeps
		// its gate across turns and must see a channel group edited meanwhile.
		if denial := s.scopedRoutingModelGateForTenant(tenantID, routeGroup, allowedGroups).denial(routingModel); denial != nil {
			return modelNotAllowedByChannelGroupError(tenantID, requestedModel, routingModel, denial)
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

// modelNotAllowedError is an authorization failure: the key's own allowed-models
// list (or its end user's) does not name this model.
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

// modelNotAllowedByChannelGroupError rejects a model the request's channel
// groups do not serve. It used to share modelNotAllowedError's "not allowed for
// this API key", which sent operators to a key that had no model list at all
// while the refusal came from the group a path or preset routes to. The wording
// and code match the credential selector's diagnosis of the same refusal.
func modelNotAllowedByChannelGroupError(tenantID, requested, routed string, denial *modelGroupDenial) *interfaces.ErrorMessage {
	message := internalrouting.ChannelGroupModelDenialMessage(
		routed, denial.excludedBy, denial.notAllowedBy,
		internalrouting.DiscoveredModel(tenantID, routed),
	)
	if !strings.EqualFold(requested, routed) {
		// A CC Switch preset renamed the model; the client only knows its own name.
		message = fmt.Sprintf("%s (requested as %q)", message, requested)
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusForbidden,
		Error: &modelGateError{
			message: message,
			typ:     "forbidden",
			code:    "model_not_allowed_by_channel_group",
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

// modelPolicyEffectiveModel 给模型策略中间件用：账号/Key 的路由与按模型额度按
// CC Switch 预设映射之后的模型判定，与模型门控看待模型的方式一致。
func (s *Server) modelPolicyEffectiveModel(c *gin.Context, requested string) string {
	return mapCcSwitchRequestModelForRestriction(requested, pathRouteContextFromGin(c))
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

// scopedRoutingModelGate applies the channel-group model gate to requests and to
// the /v1/models listing, mirroring the runtime gate in sdk/cliproxy/auth: a
// group with neither list serves every model its channels offer (including ones
// added upstream later), an allow list limits the group to the models it names
// (plus what discovery lists later, when the group follows upstream), and
// exclusions subtract from everything. Scoped groups form a union, so one
// permissive group is enough. Exclusions match through the shared
// internalrouting.ChannelGroupExcludesModel so that this gate and the runtime
// one cannot disagree on how a prefixed or wildcard entry is read.
type scopedRoutingModelGate struct {
	unrestricted bool
	tenantID     string
	groups       []scopedRoutingModelGroupGate
}

type scopedRoutingModelGroupGate struct {
	name     string
	allowed  []string
	excluded []string
	follows  bool
}

// modelGroupDenial names the groups that refused a model, by the list that did it.
type modelGroupDenial struct {
	excludedBy   []string
	notAllowedBy []string
}

func (g scopedRoutingModelGate) allows(model string) bool {
	return g.denial(model) == nil
}

// denial returns why every scoped group refused model, or nil when one admits it.
func (g scopedRoutingModelGate) denial(model string) *modelGroupDenial {
	if g.unrestricted {
		return nil
	}
	refused := &modelGroupDenial{}
	if strings.TrimSpace(model) == "" {
		return refused
	}
	for _, group := range g.groups {
		if internalrouting.ChannelGroupExcludesModel(group.excluded, model) {
			refused.excludedBy = append(refused.excludedBy, group.name)
			continue
		}
		if len(group.allowed) == 0 || routeAllowedModelMatches(model, group.allowed) {
			return nil
		}
		if internalrouting.GroupAdmitsDiscoveredModel(group.follows, g.tenantID, model) {
			return nil
		}
		refused.notAllowedBy = append(refused.notAllowedBy, group.name)
	}
	return refused
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
	gate.tenantID = tenantID
	for _, group := range routing.ChannelGroups {
		groupName := internalrouting.NormalizeGroupName(group.Name)
		if _, ok := scopedGroups[groupName]; !ok {
			continue
		}
		if len(group.AllowedModels) == 0 && len(group.ExcludedModels) == 0 {
			return unrestricted
		}
		gate.groups = append(gate.groups, scopedRoutingModelGroupGate{
			name:     groupName,
			allowed:  group.AllowedModels,
			excluded: group.ExcludedModels,
			follows:  group.FollowsUpstreamModels(),
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

// routeAllowedModelMatches reports whether an allow list names the model. It has
// no wildcard: "*" in an allow list never meant "everything", and reading it
// that way would open a group that was configured to serve nothing.
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
		if strings.EqualFold(model, allowed) {
			return true
		}
		if idx := strings.Index(model, "/"); idx >= 0 && strings.EqualFold(strings.TrimSpace(model[idx+1:]), allowed) {
			return true
		}
	}
	return false
}
