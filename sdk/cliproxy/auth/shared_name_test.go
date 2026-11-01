package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestSharedNameNeverShowsTheLogin(t *testing.T) {
	login := &Auth{ID: "claude-owner@example.com.json", FileName: "/auths/claude-owner@example.com.json", Provider: "claude",
		Label: "owner@example.com", Metadata: map[string]any{"email": "owner@example.com"}}
	name, id := login.SharedName(), login.SharedID()
	if strings.Contains(name, "@") || strings.Contains(id, "owner") || !strings.HasPrefix(name, "claude · ") || !strings.HasPrefix(id, "shared-") {
		t.Fatalf("shared name %q, id %q", name, id)
	}
	text := `{"auth_id":"claude-owner@example.com.json","auth_label":"owner@example.com","log":"label=owner@example.com file=/auths/claude-owner@example.com.json"}`
	for _, pair := range login.SharedStandIns() {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	if strings.Contains(text, "owner") || !json.Valid([]byte(text)) {
		t.Fatalf("after the stand-ins: %s", text)
	}

	// A label the platform chose is the name, even when it holds the file name.
	labelled := &Auth{ID: "codex-team.json", Provider: "codex", Label: "Team codex-team.json", Metadata: map[string]any{"email": "team@example.com"}}
	if got := labelled.SharedName(); got != "Team codex-team.json" {
		t.Fatalf("labelled shared name = %q", got)
	}
	for _, pair := range labelled.SharedStandIns() {
		if strings.Contains(labelled.SharedName(), pair[0]) {
			t.Fatalf("stand-ins would rewrite the shared name itself: %v", pair)
		}
	}
}

func TestSimulateSelectionHidesWhoASharedAccountIs(t *testing.T) {
	shared := &Auth{ID: "claude-owner@example.com.json", Label: "owner@example.com", Metadata: map[string]any{"email": "owner@example.com"}}
	manager := newSimulationTestManager(t, "", shared)
	manager.SetSharedAuthGrants(map[string][]string{shared.ID: {"tenant-b"}})
	sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel,
		map[string]any{cliproxyexecutor.TenantMetadataKey: "tenant-b"})
	encoded, _ := json.Marshal(sim)
	if strings.Contains(string(encoded), "owner") {
		t.Fatalf("the simulation names the shared account: %s", encoded)
	}
	if len(sim.Groups) == 0 || len(sim.Groups[0].Credentials) != 1 || !sim.Groups[0].Credentials[0].Shared {
		t.Fatalf("simulation = %s, want the shared account listed", encoded)
	}
	if len(sim.Attempts) != 1 || sim.Attempts[0].AuthID != sim.Groups[0].Credentials[0].AuthID || sim.Groups[0].Credentials[0].Attempt != 1 {
		t.Fatalf("simulation = %s, want its attempt to point at the stand-in", encoded)
	}

	// The platform's own view is unchanged.
	own := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, map[string]any{})
	if len(own.Groups) == 0 || len(own.Groups[0].Credentials) != 1 || own.Groups[0].Credentials[0].AuthID != shared.ID {
		t.Fatalf("platform simulation = %+v", own.Groups)
	}
}
