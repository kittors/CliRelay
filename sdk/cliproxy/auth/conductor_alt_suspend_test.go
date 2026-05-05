package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestMarkResult_DoesNotSuspendBaseModelForAlt404(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "alt-404-auth",
		Provider: "openai-compatibility",
		Status:   StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}

	modelID := "alt-404-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelID}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    modelID,
		Alt:      "responses/compact",
		Success:  false,
		Error: &Error{
			Code:       "not_found",
			Message:    "compact endpoint not found",
			HTTPStatus: 404,
		},
	})

	if got := reg.GetModelCount(modelID); got != 1 {
		t.Fatalf("GetModelCount(%q) = %d, want 1", modelID, got)
	}
}

func TestMarkResult_SuspendsBaseModelForPrimary404(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "primary-404-auth",
		Provider: "openai-compatibility",
		Status:   StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}

	modelID := "primary-404-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelID}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    modelID,
		Success:  false,
		Error: &Error{
			Code:       "not_found",
			Message:    "model not found",
			HTTPStatus: 404,
		},
	})

	if got := reg.GetModelCount(modelID); got != 0 {
		t.Fatalf("GetModelCount(%q) = %d, want 0", modelID, got)
	}
}
