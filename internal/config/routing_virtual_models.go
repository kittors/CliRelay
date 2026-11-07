package config

import (
	"fmt"
	"strings"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
)

// A virtual model is a model name the gateway serves from one of several real
// ones. Clients ask for it like any model; each member is a model in a channel
// group, and the request runs on the first member that can take it. When a
// member fails in a way another model may not (no account free, every account
// cooling, an upstream error), the request moves on to the next member before
// anything has reached the client. A failure that is the request's own fault
// (a malformed body, a content-policy refusal) ends the request: another model
// would refuse it too, and moving on would only route around the refusal.
//
// A key's allowed-models decides who may ask for the virtual model by its
// name; each member still runs only where the key and the request's route may
// run, so a virtual model never widens what a key reaches.
//
// Virtual models live in the tenant's routing config next to the channel
// groups they point at. Nesting and time-of-day rules are not supported: a
// member always names a real model.
const (
	// VirtualModelStrategyOrder tries the members in the order they are listed.
	// It is the default and is stored as an empty strategy.
	VirtualModelStrategyOrder = "order"
	// VirtualModelStrategyWeighted picks the first member by weight and keeps
	// the listed order for the members tried after it.
	VirtualModelStrategyWeighted = "weighted"

	// MaxVirtualModels bounds a tenant's virtual models.
	MaxVirtualModels = 200
	// MaxVirtualModelMembers bounds the members of one virtual model: past a
	// handful, a request that fails over through all of them has long outlived
	// any client's timeout.
	MaxVirtualModelMembers = 16
	// MaxVirtualModelMemberWeight bounds a member's weight.
	MaxVirtualModelMemberWeight = 1000

	maxVirtualModelNameLength = 128
)

// VirtualModel is one virtual model of a routing config.
type VirtualModel struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Strategy is VirtualModelStrategyWeighted or empty for
	// VirtualModelStrategyOrder.
	Strategy string               `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	Members  []VirtualModelMember `yaml:"members" json:"members"`
}

// VirtualModelMember is a real model a virtual model can run as.
type VirtualModelMember struct {
	// Group confines the member to one channel group. Empty runs it wherever
	// the request would run a model it named itself: its path route, the key's
	// channel groups, or the default group.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	Model string `yaml:"model" json:"model"`
	// Weight is the member's share of first picks under the weighted strategy.
	// A member with weight 0 is only ever tried after another one failed.
	Weight int `yaml:"weight,omitempty" json:"weight,omitempty"`
}

// Weighted reports whether the virtual model picks its first member by weight.
func (vm VirtualModel) Weighted() bool {
	return vm.Strategy == VirtualModelStrategyWeighted
}

// VirtualModel returns the virtual model named name. Names match exactly, as
// model ids do everywhere else in the gateway.
func (r RoutingConfig) VirtualModel(name string) (VirtualModel, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return VirtualModel{}, false
	}
	for _, vm := range r.VirtualModels {
		if vm.Name == name {
			return vm, true
		}
	}
	return VirtualModel{}, false
}

// VirtualModelGroups returns the channel groups the virtual models name, each
// with the first virtual model that names it.
func (r RoutingConfig) VirtualModelGroups() map[string]string {
	out := make(map[string]string)
	for _, vm := range r.VirtualModels {
		for _, member := range vm.Members {
			if member.Group == "" {
				continue
			}
			if _, seen := out[member.Group]; !seen {
				out[member.Group] = vm.Name
			}
		}
	}
	return out
}

// ValidateVirtualModels checks virtual models as an operator sent them, before
// sanitizeVirtualModels quietly drops what it cannot keep. knownGroups are the
// tenant's channel groups; a member may only name one of them.
func ValidateVirtualModels(models []VirtualModel, knownGroups map[string]struct{}) error {
	if len(models) > MaxVirtualModels {
		return fmt.Errorf("at most %d virtual models are allowed", MaxVirtualModels)
	}
	names := make(map[string]string, len(models))
	for _, vm := range models {
		name := strings.TrimSpace(vm.Name)
		if err := validateVirtualModelName(name); err != nil {
			return err
		}
		key := strings.ToLower(name)
		if other, exists := names[key]; exists {
			return fmt.Errorf("virtual model %q is defined twice (as %q)", name, other)
		}
		names[key] = name
	}
	for _, vm := range models {
		if err := validateVirtualModelMembers(vm, names, knownGroups); err != nil {
			return err
		}
	}
	return nil
}

func validateVirtualModelName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a virtual model needs a name")
	case len(name) > maxVirtualModelNameLength:
		return fmt.Errorf("virtual model name %q is longer than %d characters", name, maxVirtualModelNameLength)
	case strings.Contains(name, "/"):
		// "group/model" already means "this model in that group".
		return fmt.Errorf("virtual model name %q must not contain '/'", name)
	case strings.EqualFold(name, "auto"):
		return fmt.Errorf("virtual model name %q is reserved", name)
	case !printableModelID(name):
		// The name goes back to the client in a response header.
		return fmt.Errorf("virtual model name %q must be printable ASCII without spaces", name)
	}
	return nil
}

// printableModelID reports whether s is printable ASCII without spaces, which
// is what a model id has to be to travel in a response header as it is.
func printableModelID(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r > '~' }) < 0
}

func validateVirtualModelMembers(vm VirtualModel, names map[string]string, knownGroups map[string]struct{}) error {
	name := strings.TrimSpace(vm.Name)
	if len(vm.Members) == 0 {
		return fmt.Errorf("virtual model %q needs at least one member", name)
	}
	if len(vm.Members) > MaxVirtualModelMembers {
		return fmt.Errorf("virtual model %q has more than %d members", name, MaxVirtualModelMembers)
	}
	weighted := normalizeVirtualModelStrategy(vm.Strategy) == VirtualModelStrategyWeighted
	anyWeight := false
	seen := make(map[string]struct{}, len(vm.Members))
	for i, member := range vm.Members {
		model := strings.TrimSpace(member.Model)
		group := internalrouting.NormalizeGroupName(member.Group)
		position := i + 1
		if model == "" {
			return fmt.Errorf("member %d of virtual model %q needs a model", position, name)
		}
		if !printableModelID(model) {
			return fmt.Errorf("member %d of virtual model %q: model %q must be printable ASCII without spaces", position, name, model)
		}
		// A request on a group's path cannot ask for "other/model": the
		// prefix names another group. A member of a group runs on that path.
		if prefix, _, found := strings.Cut(model, "/"); found && group != "" && internalrouting.NormalizeGroupName(prefix) != group {
			return fmt.Errorf("member %d of virtual model %q: model %q starts with another channel group than the member's %q; put the group in the group field", position, name, model, group)
		}
		// A member names a real model. One naming another virtual model would
		// read as nesting, which is not supported; naming its own virtual model
		// is how a virtual model adds fallbacks to an existing model name.
		if other, isVirtual := names[strings.ToLower(model)]; isVirtual && !strings.EqualFold(model, name) {
			return fmt.Errorf("member %d of virtual model %q names virtual model %q; virtual models cannot contain other virtual models", position, name, other)
		}
		if group != "" {
			if _, ok := knownGroups[group]; !ok {
				return fmt.Errorf("member %d of virtual model %q: channel group %q does not exist", position, name, group)
			}
		}
		key := strings.ToLower(group + "\x00" + model)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("virtual model %q lists %q twice", name, describeVirtualModelMember(group, model))
		}
		seen[key] = struct{}{}
		if member.Weight < 0 || member.Weight > MaxVirtualModelMemberWeight {
			return fmt.Errorf("member %d of virtual model %q: weight must be between 0 and %d", position, name, MaxVirtualModelMemberWeight)
		}
		if member.Weight > 0 {
			anyWeight = true
		}
	}
	if weighted && !anyWeight {
		return fmt.Errorf("virtual model %q picks its first member by weight, so at least one member needs a weight above 0", name)
	}
	return nil
}

func describeVirtualModelMember(group, model string) string {
	if group == "" {
		return model
	}
	return group + "/" + model
}

// sanitizeVirtualModels brings stored virtual models into the form the
// management API saves, for configs that did not come through it (an import,
// a hand-edited row): names and models trimmed, groups normalized, weights in
// range, empty and repeated entries dropped. The first of two virtual models
// with the same name wins, as the first of two channel groups does.
func sanitizeVirtualModels(routing *RoutingConfig) {
	if routing == nil {
		return
	}
	if len(routing.VirtualModels) == 0 {
		routing.VirtualModels = nil
		return
	}
	seenNames := make(map[string]struct{}, len(routing.VirtualModels))
	out := make([]VirtualModel, 0, len(routing.VirtualModels))
	for _, vm := range routing.VirtualModels {
		vm.Name = strings.TrimSpace(vm.Name)
		vm.Description = strings.TrimSpace(vm.Description)
		vm.Strategy = normalizeVirtualModelStrategy(vm.Strategy)
		if vm.Strategy == VirtualModelStrategyOrder {
			vm.Strategy = ""
		}
		key := strings.ToLower(vm.Name)
		if vm.Name == "" {
			continue
		}
		if _, exists := seenNames[key]; exists {
			continue
		}
		vm.Members = sanitizeVirtualModelMembers(vm.Members)
		if len(vm.Members) == 0 {
			continue
		}
		seenNames[key] = struct{}{}
		out = append(out, vm)
		if len(out) == MaxVirtualModels {
			break
		}
	}
	if len(out) == 0 {
		out = nil
	}
	routing.VirtualModels = out
}

func sanitizeVirtualModelMembers(members []VirtualModelMember) []VirtualModelMember {
	out := make([]VirtualModelMember, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		member.Group = internalrouting.NormalizeGroupName(member.Group)
		member.Model = strings.TrimSpace(member.Model)
		member.Weight = min(max(member.Weight, 0), MaxVirtualModelMemberWeight)
		if member.Model == "" {
			continue
		}
		key := strings.ToLower(member.Group + "\x00" + member.Model)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, member)
		if len(out) == MaxVirtualModelMembers {
			break
		}
	}
	return out
}

func normalizeVirtualModelStrategy(strategy string) string {
	if strings.EqualFold(strings.TrimSpace(strategy), VirtualModelStrategyWeighted) {
		return VirtualModelStrategyWeighted
	}
	return VirtualModelStrategyOrder
}
