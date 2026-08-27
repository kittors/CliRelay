package auth

import (
	"context"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// restrictedGroupManager reproduces the production shape that made this bug so hard
// to diagnose: a healthy credential in a channel group whose allowed-models list
// covers the chat model but not the image model.
func restrictedGroupManager(t *testing.T) *Manager {
	t.Helper()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:          "default",
					AllowedModels: []string{"grok-4.5"},
				},
			},
		},
	})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})

	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "xai-auth",
		Label:    "Grok Account",
		Provider: "xai",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return manager
}

func pickWithModel(manager *Manager, model string) error {
	_, _, _, err := manager.pickNextMixed(
		context.Background(),
		[]string{"xai"},
		model,
		cliproxyexecutor.Options{},
		map[string]struct{}{},
	)
	return err
}

// TestBlockedModelReportsTheChannelGroup is the fix. Reporting "no auth available"
// for this case sent an operator hunting for a missing or broken account while the
// account was present and healthy, and cost several rounds of investigation before
// the allowed-models list was found.
func TestBlockedModelReportsTheChannelGroup(t *testing.T) {
	manager := restrictedGroupManager(t)

	err := pickWithModel(manager, "grok-imagine-image")
	if err == nil {
		t.Fatal("a model outside the group's allowed list must not be selectable")
	}

	var selectionErr *Error
	if asErr, ok := err.(*Error); ok {
		selectionErr = asErr
	} else {
		t.Fatalf("error type = %T, want *Error", err)
	}

	if selectionErr.Code != "model_not_allowed_by_channel_group" {
		t.Errorf("code = %q, want model_not_allowed_by_channel_group", selectionErr.Code)
	}
	// The message has to name both halves of the problem, because the fix is to
	// edit one specific list.
	if !strings.Contains(selectionErr.Message, "grok-imagine-image") {
		t.Errorf("message does not name the model: %q", selectionErr.Message)
	}
	if !strings.Contains(selectionErr.Message, "default") {
		t.Errorf("message does not name the channel group: %q", selectionErr.Message)
	}
}

// TestAllowedModelStillSelectable pins that the diagnosis did not change routing:
// a model on the list is still served.
func TestAllowedModelStillSelectable(t *testing.T) {
	manager := restrictedGroupManager(t)

	if err := pickWithModel(manager, "grok-4.5"); err != nil {
		t.Fatalf("an allowed model must still be selectable: %v", err)
	}
}

// TestGenericErrorRemainsWhenNoCredentialExists keeps the original message for the
// case it actually describes, so the new one stays meaningful.
func TestGenericErrorRemainsWhenNoCredentialExists(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})

	err := pickWithModel(manager, "grok-imagine-image")
	if err == nil {
		t.Fatal("expected an error when the tenant owns no credentials")
	}
	if asErr, ok := err.(*Error); ok && asErr.Code == "model_not_allowed_by_channel_group" {
		t.Error("a tenant with no credentials must not be told about channel groups")
	}
}

// TestUnrestrictedGroupDoesNotClaimBlocking guards against blaming a group that
// permits everything when the real cause is something else.
func TestUnrestrictedGroupDoesNotClaimBlocking(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{Name: "default"},
			},
		},
	})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})
	if _, err := manager.Register(context.Background(), &Auth{
		ID: "xai-auth", Provider: "xai", Status: StatusActive,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := pickWithModel(manager, "grok-imagine-image"); err != nil {
		if asErr, ok := err.(*Error); ok && asErr.Code == "model_not_allowed_by_channel_group" {
			t.Errorf("a group without an allowed-models list must not be reported as blocking: %v", asErr.Message)
		}
	}
}

// pickWithScope drives selection with the caller-side restrictions an API key or
// end user carries, which is where the misleading message came from in production.
func pickWithScope(manager *Manager, model string, meta map[string]any) error {
	_, _, _, err := manager.pickNextMixed(
		context.Background(),
		[]string{"xai", "kimi"},
		model,
		cliproxyexecutor.Options{Metadata: meta},
		map[string]struct{}{},
	)
	return err
}

// scopedModelRegistry serves each model from exactly one credential, which is what
// makes "the account that serves this model is out of scope" distinguishable from
// "nothing serves this model".
type scopedModelRegistry struct {
	byClient map[string][]string
}

func (r *scopedModelRegistry) ClearModelQuotaExceeded(string, string)    {}
func (r *scopedModelRegistry) SetModelQuotaExceeded(string, string)      {}
func (r *scopedModelRegistry) SuspendClientModel(string, string, string) {}
func (r *scopedModelRegistry) ResumeClientModel(string, string)          {}

func (r *scopedModelRegistry) ClientSupportsModel(clientID, modelID string) bool {
	for _, model := range r.byClient[clientID] {
		if strings.EqualFold(model, modelID) {
			return true
		}
	}
	return false
}

func (r *scopedModelRegistry) GetModelsForClient(clientID string) []*sdkmodelcatalog.ModelInfo {
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(r.byClient[clientID]))
	for _, model := range r.byClient[clientID] {
		out = append(out, &sdkmodelcatalog.ModelInfo{ID: model})
	}
	return out
}

// twoChannelManager mirrors the reported deployment: two healthy accounts, only one
// of them inside the channel group the caller is restricted to.
func twoChannelManager(t *testing.T) *Manager {
	t.Helper()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{Name: "group", Match: internalconfig.ChannelGroupMatch{Channels: []string{"Grok Account"}}},
			},
		},
	})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})
	manager.RegisterExecutor(&stubExecutor{id: "kimi"})

	for _, auth := range []*Auth{
		{ID: "xai-auth", Label: "Grok Account", Provider: "xai", Status: StatusActive},
		{ID: "kimi-auth", Label: "kimi", Provider: "kimi", Status: StatusActive},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	manager.SetModelRegistry(&scopedModelRegistry{byClient: map[string][]string{
		"xai-auth":  {"grok-4.5"},
		"kimi-auth": {"kimi-k3"},
	}})
	return manager
}

// TestModelOutsideAllowedChannelGroupsIsNamed is the fix for the reported case: the
// kimi account was healthy, the model was registered on it, and the request still
// said "no auth available" because the caller could only use one channel group.
func TestModelOutsideAllowedChannelGroupsIsNamed(t *testing.T) {
	manager := twoChannelManager(t)

	err := pickWithScope(manager, "kimi-k3", map[string]any{
		"allowed-channel-groups": []string{"group"},
	})
	if err == nil {
		t.Fatal("a credential outside the allowed channel groups must not be selectable")
	}
	selectionErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if selectionErr.Code != "model_outside_allowed_channel_groups" {
		t.Fatalf("code = %q, want model_outside_allowed_channel_groups (message: %s)", selectionErr.Code, selectionErr.Message)
	}
	// Both halves have to be named: the model the caller asked for and the scope
	// that excluded it, because the fix is to edit one of them.
	if !strings.Contains(selectionErr.Message, "kimi-k3") {
		t.Errorf("message does not name the model: %q", selectionErr.Message)
	}
	if !strings.Contains(selectionErr.Message, "group") {
		t.Errorf("message does not name the allowed channel group: %q", selectionErr.Message)
	}
}

// TestReachableModelStillSelectableUnderChannelGroupScope pins that the diagnosis
// changed no routing: the account inside the allowed group still serves.
func TestReachableModelStillSelectableUnderChannelGroupScope(t *testing.T) {
	manager := twoChannelManager(t)

	if err := pickWithScope(manager, "grok-4.5", map[string]any{
		"allowed-channel-groups": []string{"group"},
	}); err != nil {
		t.Fatalf("an account inside the allowed group must still be selectable: %v", err)
	}
}

// TestModelOutsideAllowedChannelsIsNamed covers the per-channel restriction, which
// produced the same unhelpful message.
func TestModelOutsideAllowedChannelsIsNamed(t *testing.T) {
	manager := twoChannelManager(t)

	err := pickWithScope(manager, "kimi-k3", map[string]any{
		"allowed-channels": []string{"Grok Account"},
	})
	if err == nil {
		t.Fatal("a credential outside the allowed channels must not be selectable")
	}
	selectionErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if selectionErr.Code != "model_outside_allowed_channels" {
		t.Fatalf("code = %q, want model_outside_allowed_channels (message: %s)", selectionErr.Code, selectionErr.Message)
	}
	if !strings.Contains(selectionErr.Message, "kimi-k3") {
		t.Errorf("message does not name the model: %q", selectionErr.Message)
	}
}
