package auth

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSharedIdentitySnapshotTracksLiveStateWithoutMutatingAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const tenant = "tenant-a"
	a, err := manager.Register(context.Background(), &Auth{ID: "shared-owner@example.com", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.ListSharedIdentitiesForTenant(tenant); len(got) != 0 {
		t.Fatal("an account that is not shared was listed")
	}
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {tenant}})
	got := manager.ListSharedIdentitiesForTenant(tenant)
	if len(got) != 1 || got[0].Index != a.EnsureIndex() || got[0].Name != a.SharedName() {
		t.Fatalf("identity = %+v", got)
	}
	// What a caller does to the snapshot changes neither the manager's account
	// nor the next snapshot.
	got[0].StandIns[0][0] = "tampered"
	for _, pair := range manager.ListSharedIdentitiesForTenant(tenant)[0].StandIns {
		if pair[0] == "tampered" {
			t.Fatal("the snapshot shares its pairs with the account")
		}
	}
	a.Metadata["email"] = "new@example.com"
	if _, err = manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pair := range manager.ListSharedIdentitiesForTenant(tenant)[0].StandIns {
		found = found || strings.Contains(pair[0], "new@example.com")
	}
	if !found {
		t.Fatal("the snapshot kept the identity from before the update")
	}
	manager.SetSharedAuthGrants(nil)
	if got := manager.ListSharedIdentitiesForTenant(tenant); len(got) != 0 {
		t.Fatal("a revoked share was listed")
	}
}

func TestSharedIdentitySnapshotListsOnlyGrantedSystemAccounts(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	for i := 0; i < 200; i++ {
		if _, err := manager.Register(context.Background(), &Auth{ID: fmt.Sprintf("owned-%d", i), Provider: "claude", TenantID: "tenant-a"}); err != nil {
			t.Fatal(err)
		}
	}
	shared, err := manager.Register(context.Background(), &Auth{ID: "shared-account", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.ListSharedIdentitiesForTenant("tenant-a"); len(got) != 0 {
		t.Fatal("identities were listed with no grant")
	}
	// a grant of the tenant's own account and one of an unknown id share nothing
	manager.SetSharedAuthGrants(map[string][]string{shared.ID: {"tenant-a"}, "owned-1": {"tenant-a"}, "missing": {"*"}})
	if got := manager.ListSharedIdentitiesForTenant("tenant-a"); len(got) != 1 || got[0].AuthID != shared.ID {
		t.Fatalf("identities = %+v", got)
	}
	if got := manager.ListSharedIdentitiesForTenant("tenant-b"); len(got) != 0 {
		t.Fatalf("another tenant's share was listed: %+v", got)
	}
	if got := manager.ListSharedIdentitiesForTenant(""); len(got) != 0 {
		t.Fatal("the platform's own account was listed as shared with the platform")
	}
}
