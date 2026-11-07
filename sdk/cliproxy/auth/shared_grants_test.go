package auth

import (
	"context"
	"sort"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const (
	sharedTestTenantA = "00000000-0000-0000-0000-00000000000a"
	sharedTestTenantB = "00000000-0000-0000-0000-00000000000b"
)

func tenantOptions(tenantID string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID}}
}

func newSharedGrantTestManager(t *testing.T, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func sortedAuthIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		ids = append(ids, auth.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestSharedCredentialServesGranteeOnOwnerExecutor(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "system-codex", TenantID: defaultTenantID, Provider: "codex", Status: StatusActive},
	)
	systemExecutor := &stubExecutor{id: "codex"}
	manager.RegisterExecutor(systemExecutor)

	// Before the grant, tenant B has neither a credential nor an executor.
	if _, _, err := manager.pickNext(context.Background(), "codex", "", tenantOptions(sharedTestTenantB), map[string]struct{}{}); err == nil || !strings.Contains(err.Error(), "executor not registered") {
		t.Fatalf("ungranted tenant reached the system credential: %v", err)
	}

	manager.SetSharedAuthGrants(map[string][]string{"system-codex": {sharedTestTenantB}})
	auth, exec, err := manager.pickNext(context.Background(), "codex", "", tenantOptions(sharedTestTenantB), map[string]struct{}{})
	if err != nil {
		t.Fatalf("pickNext for grantee: %v", err)
	}
	if auth == nil || auth.ID != "system-codex" {
		t.Fatalf("grantee picked %#v, want the shared system credential", auth)
	}
	// The owner's executor, so the account leaves through the platform's proxy
	// and fingerprint, like its token refresh does.
	if exec != systemExecutor {
		t.Fatalf("shared credential ran on %#v, want the system executor", exec)
	}

	// Tenant A was not granted.
	if _, _, err := manager.pickNext(context.Background(), "codex", "", tenantOptions(sharedTestTenantA), map[string]struct{}{}); err == nil {
		t.Fatal("a tenant without a grant was served by the shared credential")
	}
}

func TestGranteeKeepsItsOwnExecutorForItsOwnCredentials(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "system-codex", TenantID: defaultTenantID, Provider: "codex", Status: StatusActive},
		&Auth{ID: "tenant-a-codex", TenantID: sharedTestTenantA, Provider: "codex", Status: StatusActive},
	)
	systemExecutor := &stubExecutor{id: "codex"}
	tenantExecutor := &stubExecutor{id: "codex"}
	manager.RegisterExecutor(systemExecutor)
	manager.RegisterExecutorForTenant(sharedTestTenantA, tenantExecutor)
	manager.SetSharedAuthGrants(map[string][]string{"system-codex": {"*"}})

	seen := map[string]ProviderExecutor{}
	for _, authID := range []string{"tenant-a-codex", "system-codex"} {
		opts := tenantOptions(sharedTestTenantA)
		opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
		auth, exec, err := manager.pickNext(context.Background(), "codex", "", opts, map[string]struct{}{})
		if err != nil {
			t.Fatalf("pick %s: %v", authID, err)
		}
		seen[auth.ID] = exec
	}
	if seen["tenant-a-codex"] != tenantExecutor {
		t.Fatalf("own credential ran on %#v, want the tenant executor", seen["tenant-a-codex"])
	}
	if seen["system-codex"] != systemExecutor {
		t.Fatalf("shared credential ran on %#v, want the system executor", seen["system-codex"])
	}
}

func TestPickNextMixedRunsSharedCredentialOnOwnerExecutor(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "system-claude", TenantID: defaultTenantID, Provider: "claude", Status: StatusActive},
	)
	systemExecutor := &stubExecutor{id: "claude"}
	manager.RegisterExecutor(systemExecutor)
	manager.SetSharedAuthGrants(map[string][]string{"system-claude": {sharedTestTenantB}})

	auth, exec, provider, err := manager.pickNextMixed(context.Background(), []string{"codex", "claude"}, "", tenantOptions(sharedTestTenantB), map[string]struct{}{})
	if err != nil {
		t.Fatalf("pickNextMixed: %v", err)
	}
	if auth.ID != "system-claude" || provider != "claude" || exec != systemExecutor {
		t.Fatalf("pickNextMixed = %s/%s on %#v", auth.ID, provider, exec)
	}
}

func TestOnlySystemCredentialsCanBeShared(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "tenant-a-codex", TenantID: sharedTestTenantA, Provider: "codex", Status: StatusActive},
	)
	manager.RegisterExecutorForTenant(sharedTestTenantA, &stubExecutor{id: "codex"})
	// A grant naming another tenant's credential must not open it up.
	manager.SetSharedAuthGrants(map[string][]string{"tenant-a-codex": {sharedTestTenantB}})

	if _, _, err := manager.pickNext(context.Background(), "codex", "", tenantOptions(sharedTestTenantB), map[string]struct{}{}); err == nil {
		t.Fatal("tenant B was served by tenant A's credential")
	}
	if got := manager.ListUsableForTenant(sharedTestTenantB); len(got) != 0 {
		t.Fatalf("ListUsableForTenant(B) = %v, want nothing", sortedAuthIDs(got))
	}
}

func TestSharedCredentialsListedApartFromOwnedOnes(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "system-codex", TenantID: defaultTenantID, Provider: "codex", Status: StatusActive},
		&Auth{ID: "system-private", TenantID: defaultTenantID, Provider: "codex", Status: StatusActive},
		&Auth{ID: "tenant-a-codex", TenantID: sharedTestTenantA, Provider: "codex", Status: StatusActive},
	)
	manager.SetSharedAuthGrants(map[string][]string{"system-codex": {sharedTestTenantA}})

	// Management stays owner-only.
	if got := sortedAuthIDs(manager.ListForTenant(sharedTestTenantA)); strings.Join(got, ",") != "tenant-a-codex" {
		t.Fatalf("ListForTenant(A) = %v, want only the owned credential", got)
	}
	if got := sortedAuthIDs(manager.ListUsableForTenant(sharedTestTenantA)); strings.Join(got, ",") != "system-codex,tenant-a-codex" {
		t.Fatalf("ListUsableForTenant(A) = %v", got)
	}
	if got := sortedAuthIDs(manager.ListSharedWithTenant(sharedTestTenantA)); strings.Join(got, ",") != "system-codex" {
		t.Fatalf("ListSharedWithTenant(A) = %v", got)
	}
	// The system tenant owns its credentials; nothing is shared with it.
	if got := manager.ListSharedWithTenant(defaultTenantID); len(got) != 0 {
		t.Fatalf("ListSharedWithTenant(system) = %v", sortedAuthIDs(got))
	}
	if !manager.IsSharedWithTenant(&Auth{ID: "system-codex", TenantID: defaultTenantID}, sharedTestTenantA) {
		t.Fatal("IsSharedWithTenant missed the grant")
	}

	// Revoking takes effect on the next read.
	manager.SetSharedAuthGrants(nil)
	if got := manager.ListUsableForTenant(sharedTestTenantA); len(got) != 1 {
		t.Fatalf("ListUsableForTenant(A) after revoke = %v", sortedAuthIDs(got))
	}
}

func TestSharedCredentialJoinsGranteeGroupsAndModels(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "system-codex", TenantID: defaultTenantID, Provider: "codex", Prefix: "pro", Status: StatusActive},
	)
	manager.SetConfigForTenant(sharedTestTenantB, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{IncludeDefaultGroup: true},
	})
	if _, ok := manager.KnownChannelGroupsForTenant(sharedTestTenantB)["pro"]; ok {
		t.Fatal("ungranted credential leaked its group into tenant B")
	}
	// A prefixed credential leaves the default group, so ask through its own.
	if manager.CanServeModelWithScopesForTenant(sharedTestTenantB, "gpt-5.5", nil, nil, "pro") {
		t.Fatal("ungranted credential served a model for tenant B")
	}

	manager.SetSharedAuthGrants(map[string][]string{"system-codex": {sharedTestTenantB}})
	if _, ok := manager.KnownChannelGroupsForTenant(sharedTestTenantB)["pro"]; !ok {
		t.Fatal("shared credential's group missing from tenant B's groups")
	}
	if !manager.CanServeModelWithScopesForTenant(sharedTestTenantB, "gpt-5.5", nil, nil, "pro") {
		t.Fatal("shared credential does not serve tenant B's models")
	}
}

func TestSharedCredentialUnavailabilityIsNotReportedAsMissingExecutor(t *testing.T) {
	manager := newSharedGrantTestManager(t,
		&Auth{ID: "system-codex", TenantID: defaultTenantID, Provider: "codex", Status: StatusActive, Disabled: true},
	)
	manager.RegisterExecutor(&stubExecutor{id: "codex"})
	manager.SetSharedAuthGrants(map[string][]string{"system-codex": {sharedTestTenantB}})

	_, _, err := manager.pickNext(context.Background(), "codex", "", tenantOptions(sharedTestTenantB), map[string]struct{}{})
	if err == nil {
		t.Fatal("a disabled shared credential served a request")
	}
	if strings.Contains(err.Error(), "executor not registered") {
		t.Fatalf("a tenant that is granted a codex credential got %v", err)
	}
}
