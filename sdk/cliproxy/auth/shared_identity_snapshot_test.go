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
		t.Fatal("ungranted account exposed")
	}
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {tenant}})
	got := manager.ListSharedIdentitiesForTenant(tenant)
	if len(got) != 1 || got[0].Index != a.EnsureIndex() || got[0].Name != a.SharedName() {
		t.Fatalf("bad identity: %+v", got)
	}
	// A caller may mutate the returned pair slice; neither manager state nor the
	// next snapshot can be affected.
	got[0].StandIns[0][0] = "tampered"
	again := manager.ListSharedIdentitiesForTenant(tenant)
	for _, pair := range again[0].StandIns {
		if pair[0] == "tampered" {
			t.Fatal("snapshot aliases live identity")
		}
	}
	a.Metadata["email"] = "new@example.com"
	if _, err = manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	changed := manager.ListSharedIdentitiesForTenant(tenant)
	found := false
	for _, pair := range changed[0].StandIns {
		if strings.Contains(pair[0], "new@example.com") {
			found = true
		}
	}
	if !found {
		t.Fatal("identity update stayed stale")
	}
	manager.SetSharedAuthGrants(nil)
	if got := manager.ListSharedIdentitiesForTenant(tenant); len(got) != 0 {
		t.Fatal("revoked grant stayed visible")
	}
}

func TestSharedIdentitySnapshotFiltersSparseGrants(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	for i := 0; i < 1000; i++ {
		if _, err := manager.Register(context.Background(), &Auth{ID: fmt.Sprintf("owned-%d", i), Provider: "claude", TenantID: "tenant-a"}); err != nil {
			t.Fatal(err)
		}
	}
	shared, err := manager.Register(context.Background(), &Auth{ID: "shared-account", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.ListSharedIdentitiesForTenant("tenant-a"); len(got) != 0 {
		t.Fatal("empty grants returned identities")
	}
	manager.SetSharedAuthGrants(map[string][]string{shared.ID: {"tenant-a"}, "owned-1": {"tenant-a"}, "missing": {"*"}})
	got := manager.ListSharedIdentitiesForTenant("tenant-a")
	if len(got) != 1 || got[0].AuthID != shared.ID {
		t.Fatalf("sparse grant selection incorrect: %+v", got)
	}
	if got := manager.ListSharedIdentitiesForTenant("tenant-b"); len(got) != 0 {
		t.Fatalf("another tenant grant exposed: %+v", got)
	}
	if got := manager.ListSharedIdentitiesForTenant(""); len(got) != 0 {
		t.Fatal("owner account treated as shared")
	}
	manager.SetSharedAuthGrants(nil)
	if got := manager.ListSharedIdentitiesForTenant("tenant-a"); len(got) != 0 {
		t.Fatal("revoked sparse grants exposed")
	}
}

func BenchmarkSharedIdentitySnapshotSparse(b *testing.B) {
	for _, total := range []int{1000, 10000} {
		manager := NewManager(nil, nil, nil)
		for i := 0; i < total; i++ {
			if _, err := manager.Register(context.Background(), &Auth{ID: fmt.Sprintf("private-%d", i), Provider: "claude", TenantID: "tenant-a"}); err != nil {
				b.Fatal(err)
			}
		}
		shared, err := manager.Register(context.Background(), &Auth{ID: "shared-account", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}})
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("%d-private/zero-shared", total), func(b *testing.B) {
			manager.SetSharedAuthGrants(nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = manager.ListSharedIdentitiesForTenant("tenant-a")
			}
		})
		b.Run(fmt.Sprintf("%d-private/one-shared", total), func(b *testing.B) {
			manager.SetSharedAuthGrants(map[string][]string{shared.ID: {"tenant-a"}})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = manager.ListSharedIdentitiesForTenant("tenant-a")
			}
		})
	}
}
