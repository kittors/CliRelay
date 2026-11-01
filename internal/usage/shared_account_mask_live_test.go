package usage

import (
	"context"
	"fmt"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const sharedMaskTestTenant = "11111111-1111-1111-1111-111111111111"

func TestSharedAccountMaskTracksLiveGrantsAndIdentities(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	InstallSharedAccountMasking(manager)
	t.Cleanup(func() { InstallSharedAccountMasking(nil) })
	register := func(id, email string) *coreauth.Auth {
		t.Helper()
		a, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"email": email}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	check := func(a *coreauth.Auth, email string) {
		t.Helper()
		raw := fmt.Sprintf(`{"auth_id":%q,"message":%q}`, a.ID, email)
		row := RequestLogEntry{AuthIndex: a.EnsureIndex(), Source: email, ChannelName: email, DetailContent: raw, OutputContent: raw, Failed: true}
		maskSharedAccountRow(&row, sharedMaskTestTenant)
		if row.Source != a.SharedName() || row.ChannelName != a.SharedName() {
			t.Fatalf("identity was not current: %+v", row)
		}
		for _, text := range []string{row.DetailContent, row.OutputContent, MaskSharedAccountText(sharedMaskTestTenant, raw)} {
			if strings.Contains(text, email) || strings.Contains(text, a.ID) {
				t.Fatalf("live identity exposed: %s", text)
			}
		}
	}
	a := register("claude-original@example.com.json", "original@example.com")
	// Prime an empty result, then add a grant. Neither reads nor writes may keep
	// using that empty result for a TTL.
	_ = MaskSharedAccountText(sharedMaskTestTenant, "prime empty list")
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {sharedMaskTestTenant}})
	check(a, "original@example.com")

	// Grants may already exist when a credential is registered later.
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {sharedMaskTestTenant}, "late-credential": {"*"}})
	check(a, "original@example.com")
	late := register("late-credential", "late@example.com")
	check(late, "late@example.com")

	// An identity refresh or edit does not need to alter the grant itself.
	a.Metadata["email"] = "updated@example.com"
	var err error
	a, err = manager.Update(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	check(a, "updated@example.com")

	// Safe label edits change the replacement too, without changing grants.
	a.Label = "public pool"
	a, err = manager.Update(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	check(a, "updated@example.com")
	// Replacing an ID after removal must not reuse a compiled previous identity.
	if _, err = manager.Delete(context.Background(), late.ID); err != nil {
		t.Fatal(err)
	}
	replacement := register(late.ID, "replacement@example.com")
	check(replacement, "replacement@example.com")

	// Preserve the existing current-grants-only behavior: revocation must not
	// accidentally keep rewriting unrelated tenant text with a stale identity.
	manager.SetSharedAuthGrants(nil)
	raw := `{"message":"updated@example.com"}`
	if got := MaskSharedAccountText(sharedMaskTestTenant, raw); got != raw {
		t.Fatalf("revoked grant stayed cached: %s", got)
	}
}

func TestSharedAccountMaskLeavesSuccessfulOutputAndPlatformUnchanged(t *testing.T) {
	a := &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}}
	InstallSharedAccountMasking(fakeSharedAccounts{sharedMaskTestTenant: {a}})
	t.Cleanup(func() { InstallSharedAccountMasking(nil) })
	original := RequestLogEntry{AuthIndex: a.EnsureIndex(), Source: "owner@example.com", ChannelName: "owner@example.com", DetailContent: `{"message":"owner@example.com"}`, OutputContent: `{"text":"owner@example.com"}`}
	row := original
	maskSharedAccountRow(&row, sharedMaskTestTenant)
	if row.OutputContent != original.OutputContent {
		t.Fatal("successful model output was rewritten")
	}
	row = original
	maskSharedAccountRow(&row, systemTenantID)
	if row.Source != original.Source || row.DetailContent != original.DetailContent {
		t.Fatal("platform row was rewritten")
	}
}

func BenchmarkSharedAccountMaskLive(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			manager := coreauth.NewManager(nil, nil, nil)
			grants := make(map[string][]string, count)
			var selected *coreauth.Auth
			for i := 0; i < count; i++ {
				email := fmt.Sprintf("owner-%d@example.com", i)
				a, err := manager.Register(context.Background(), &coreauth.Auth{ID: "claude-" + email + ".json", Provider: "claude", Metadata: map[string]any{"email": email}})
				if err != nil {
					b.Fatal(err)
				}
				grants[a.ID] = []string{sharedMaskTestTenant}
				selected = a
			}
			manager.SetSharedAuthGrants(grants)
			InstallSharedAccountMasking(manager)
			b.Cleanup(func() { InstallSharedAccountMasking(nil) })
			raw := fmt.Sprintf(`{"auth_id":%q}`, selected.ID)
			b.Run("cold-write", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					InstallSharedAccountMasking(manager)
					row := RequestLogEntry{AuthIndex: selected.EnsureIndex(), DetailContent: raw}
					maskSharedAccountRow(&row, sharedMaskTestTenant)
				}
			})
			b.Run("write", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					row := RequestLogEntry{AuthIndex: selected.EnsureIndex(), DetailContent: raw}
					maskSharedAccountRow(&row, sharedMaskTestTenant)
				}
			})
			b.Run("read", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = MaskSharedAccountText(sharedMaskTestTenant, raw)
				}
			})
		})
	}
}

func TestSharedAccountMaskCacheBoundStillUpdatesKnownIdentity(t *testing.T) {
	a := &coreauth.Auth{ID: "known-credential", Provider: "claude", Metadata: map[string]any{"email": "new@example.com"}}
	source := &sharedAccountMaskSource{lister: fakeSharedAccounts{sharedMaskTestTenant: {a}}, compiled: make(map[string]compiledSharedAccountMask)}
	source.compiled[a.ID] = compiledSharedAccountMask{}
	for i := 1; i < maxCompiledSharedAccountMasks; i++ {
		source.compiled[fmt.Sprint(i)] = compiledSharedAccountMask{}
	}
	masks := source.masksFor(sharedMaskTestTenant)
	if len(masks) != 1 || len(source.compiled) != maxCompiledSharedAccountMasks {
		t.Fatal("cache bound changed")
	}
	if source.compiled[a.ID].mask.replacer == nil {
		t.Fatal("full cache did not update existing identity")
	}
	if got := masks[0].replacer.Replace("new@example.com"); strings.Contains(got, "new@example.com") {
		t.Fatal("current identity not masked")
	}
	a.ID = "uncached-credential"
	masks = source.masksFor(sharedMaskTestTenant)
	if len(source.compiled) != maxCompiledSharedAccountMasks || len(masks) != 1 {
		t.Fatal("full cache grew or skipped masking")
	}
	if got := masks[0].replacer.Replace("new@example.com"); strings.Contains(got, "new@example.com") {
		t.Fatal("uncached live identity not masked")
	}
}
