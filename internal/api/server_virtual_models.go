package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// virtualModelOwner is the owned_by of a virtual model in the model listing.
const virtualModelOwner = "virtual"

// virtualModelScope is what decides which members of a virtual model a request
// may use: its tenant, its path route, and its key's channel groups.
type virtualModelScope struct {
	tenantID      string
	routeGroup    string
	allowedGroups map[string]struct{}
}

// virtualModelPlanner returns the planner the handlers run the tenant's
// virtual models with, or nil when routing has none. The planner reads the
// routing config again on every call, as the gate does, so that a WebSocket
// connection sees an edit made while it is open.
func (s *Server) virtualModelPlanner(scope virtualModelScope, routing *config.RoutingConfig) handlers.VirtualModelPlanner {
	if routing == nil || len(routing.VirtualModels) == 0 {
		return nil
	}
	return func(model string) *handlers.VirtualModelPlan {
		return s.planVirtualModel(scope, s.routingConfigForTenant(scope.tenantID), model)
	}
}

// planVirtualModel resolves model into the plan of the virtual model it names
// in routing, or nil when it names none. A thinking suffix on the name
// ("name(high)") passes to the members that do not set one of their own.
func (s *Server) planVirtualModel(scope virtualModelScope, routing *config.RoutingConfig, model string) *handlers.VirtualModelPlan {
	if routing == nil || len(routing.VirtualModels) == 0 {
		return nil
	}
	parsed := thinking.ParseSuffix(strings.TrimSpace(model))
	vm, ok := routing.VirtualModel(parsed.ModelName)
	if !ok {
		return nil
	}
	disabled := modelconfigsettings.DisabledModelIDsForTenant(scope.tenantID)
	plan := &handlers.VirtualModelPlan{Name: vm.Name, Weighted: vm.Weighted()}
	for _, member := range vm.Members {
		planned := handlers.VirtualModelMember{Group: member.Group, Model: member.Model, Weight: member.Weight}
		if parsed.HasSuffix && !thinking.ParseSuffix(member.Model).HasSuffix {
			planned.Model = member.Model + "(" + parsed.RawSuffix + ")"
		}
		planned.Skip, planned.Detail = virtualModelMemberRefusal(scope, routing, disabled, member)
		plan.Members = append(plan.Members, planned)
	}
	return plan
}

// virtualModelMemberRefusal says why a request in scope may not use member, as
// the gate would refuse its model asked for directly: the route or the key may
// not use the member's channel group, the model is switched off in the catalog,
// or the group does not serve it. It returns a routetrace Member* reason and
// what refused the member, or two empty strings when the request may use it.
//
// The key's allowed-models are not checked: they named the virtual model, and
// deciding who may ask for it by that name is what they do. A virtual model
// therefore lets a key run its members' models, but only where the key and
// the request's route may run them.
func virtualModelMemberRefusal(scope virtualModelScope, routing *config.RoutingConfig, disabled map[string]struct{}, member config.VirtualModelMember) (string, string) {
	if group := member.Group; group != "" {
		if scope.routeGroup != "" && group != scope.routeGroup {
			return routetrace.MemberNotAllowed, fmt.Sprintf("the request is routed to channel group %q", scope.routeGroup)
		}
		if len(scope.allowedGroups) > 0 {
			if _, ok := scope.allowedGroups[group]; !ok {
				return routetrace.MemberNotAllowed, fmt.Sprintf("the API key may not use channel group %q", group)
			}
		}
	}
	if modelconfigsettings.IsDisabled(member.Model, disabled) {
		return routetrace.MemberDisabled, fmt.Sprintf("model %q is disabled", member.Model)
	}
	// A member of a group runs on that group's path when the request has none
	// (handlers.virtualModelRun.memberContext); it is gated as such a request.
	routeGroup, allowedGroups := scope.routeGroup, scope.allowedGroups
	if routeGroup == "" && member.Group != "" {
		routeGroup, allowedGroups = member.Group, nil
	}
	if denial := scopedRoutingModelGateFor(routing, scope.tenantID, routeGroup, allowedGroups).denial(member.Model); denial != nil {
		return routetrace.MemberNotAllowed, internalrouting.ChannelGroupModelDenialMessage(
			member.Model, denial.excludedBy, denial.notAllowedBy,
			internalrouting.DiscoveredModel(scope.tenantID, member.Model),
		)
	}
	return "", ""
}

// virtualModelUnavailable refuses a virtual model none of whose members the
// request may use, saying why each is refused; nil when one is usable.
func virtualModelUnavailable(plan *handlers.VirtualModelPlan) *interfaces.ErrorMessage {
	reasons := make([]string, 0, len(plan.Members))
	for _, member := range plan.Members {
		if member.Skip == "" {
			return nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", member.Route(), member.Detail))
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusForbidden,
		Error: &modelGateError{
			message: fmt.Sprintf("virtual model '%s' has no member this request may use (%s)", plan.Name, strings.Join(reasons, "; ")),
			typ:     "forbidden",
			code:    "virtual_model_unavailable",
		},
	}
}

// virtualModelListing is what the model listing needs to know about the caller
// to list the virtual models it may use.
type virtualModelListing struct {
	scope           virtualModelScope
	allowedModels   map[string]struct{}
	allowedChannels map[string]struct{}
	// claude lists in the Claude shape (the listing for claude-cli).
	claude bool
}

// listVirtualModels returns the listing entries of the tenant's virtual models
// the caller may use: the key's allowed-models name it, and at least one member
// the request may use has an account to run on.
func (s *Server) listVirtualModels(listing virtualModelListing) []map[string]any {
	routing := s.routingConfigForTenant(listing.scope.tenantID)
	if routing == nil || len(routing.VirtualModels) == 0 {
		return nil
	}
	var manager *coreauth.Manager
	if s.handlers != nil {
		manager = s.handlers.AuthManager
	}
	entries := make([]map[string]any, 0, len(routing.VirtualModels))
	for _, vm := range routing.VirtualModels {
		if listing.allowedModels != nil && !modelInSet(vm.Name, listing.allowedModels) {
			continue
		}
		plan := s.planVirtualModel(listing.scope, routing, vm.Name)
		if plan == nil || !virtualModelServable(manager, listing, plan) {
			continue
		}
		entries = append(entries, virtualModelEntry(vm, listing.claude))
	}
	return entries
}

// virtualModelServable reports whether one member of plan the caller may use
// has an account to run on, scoped as the member would run.
func virtualModelServable(manager *coreauth.Manager, listing virtualModelListing, plan *handlers.VirtualModelPlan) bool {
	if manager == nil {
		return false
	}
	for _, member := range plan.Members {
		if member.Skip != "" {
			continue
		}
		routeGroup, allowedGroups := listing.scope.routeGroup, listing.scope.allowedGroups
		if routeGroup == "" && member.Group != "" {
			routeGroup, allowedGroups = member.Group, nil
		}
		model := thinking.ParseSuffix(member.Model).ModelName
		if manager.CanServeModelWithScopesForTenantOpts(listing.scope.tenantID, model, listing.allowedChannels, allowedGroups, routeGroup, coreauth.ServeModelScopeOptions{}) {
			return true
		}
	}
	return false
}

func virtualModelEntry(vm config.VirtualModel, claude bool) map[string]any {
	entry := map[string]any{
		"id":       vm.Name,
		"object":   "model",
		"owned_by": virtualModelOwner,
	}
	if claude {
		entry["type"] = "model"
		entry["display_name"] = vm.Name
	}
	if vm.Description != "" {
		entry["description"] = vm.Description
	}
	return entry
}

// appendVirtualModelEntries adds the virtual models to a listing. A virtual
// model named like a real model in the listing adds fallbacks to it, and is
// already listed by that model's entry.
func appendVirtualModelEntries(models []map[string]interface{}, entries []map[string]any) []map[string]interface{} {
	if len(entries) == 0 {
		return models
	}
	listed := make(map[string]struct{}, len(models))
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			listed[id] = struct{}{}
		}
	}
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		if _, dup := listed[id]; dup {
			continue
		}
		models = append(models, entry)
	}
	return models
}
