package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestPickNextSkipsStatusDisabledAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&stubExecutor{id: "gemini"})
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{IncludeDefaultGroup: true}})

	active := &Auth{ID: "a1", Provider: "gemini", Label: "Active", Disabled: false, Status: StatusActive}
	disabled := &Auth{ID: "a2", Provider: "gemini", Label: "Disabled", Disabled: false, Status: StatusDisabled}
	if _, err := manager.Register(context.Background(), active); err != nil {
		t.Fatalf("register active: %v", err)
	}
	if _, err := manager.Register(context.Background(), disabled); err != nil {
		t.Fatalf("register disabled: %v", err)
	}

	auth, _, err := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("pickNext: %v", err)
	}
	if auth == nil {
		t.Fatalf("pickNext returned nil auth")
	}
	if auth.ID != active.ID {
		t.Fatalf("selected auth = %q, want %q", auth.ID, active.ID)
	}
}

func TestPickNextMixedSkipsStatusDisabledAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&stubExecutor{id: "gemini"})
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{IncludeDefaultGroup: true}})

	active := &Auth{ID: "a1", Provider: "gemini", Label: "Active", Disabled: false, Status: StatusActive}
	disabled := &Auth{ID: "a2", Provider: "gemini", Label: "Disabled", Disabled: false, Status: StatusDisabled}
	if _, err := manager.Register(context.Background(), active); err != nil {
		t.Fatalf("register active: %v", err)
	}
	if _, err := manager.Register(context.Background(), disabled); err != nil {
		t.Fatalf("register disabled: %v", err)
	}

	auth, _, provider, err := manager.pickNextMixed(context.Background(), []string{"gemini"}, "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("pickNextMixed: %v", err)
	}
	if provider != "gemini" {
		t.Fatalf("provider = %q, want gemini", provider)
	}
	if auth == nil {
		t.Fatalf("pickNextMixed returned nil auth")
	}
	if auth.ID != active.ID {
		t.Fatalf("selected auth = %q, want %q", auth.ID, active.ID)
	}
}
