package usage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const sharedMaskTestTenant = "11111111-1111-1111-1111-111111111111"

// A share granted a moment ago is masked on the next write: no list of the
// tenant's shares read before it (none) is kept. A new identity of an account
// already shared (a refresh, a new label, a credential registered again under
// the same id) is masked on the next write as well.
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
			t.Fatalf("the row does not use the account's current stand-in: %+v", row)
		}
		for _, text := range []string{row.DetailContent, row.OutputContent, MaskSharedAccountText(sharedMaskTestTenant, raw)} {
			if strings.Contains(text, email) || strings.Contains(text, a.ID) {
				t.Fatalf("the shared account is named: %s", text)
			}
		}
	}
	a := register("claude-original@example.com.json", "original@example.com")
	// A log read before the grant finds no share; the write right after the
	// grant must not reuse that.
	_ = MaskSharedAccountText(sharedMaskTestTenant, "before the grant")
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {sharedMaskTestTenant}})
	check(a, "original@example.com")

	// A grant may name a credential registered only later.
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {sharedMaskTestTenant}, "late-credential": {"*"}})
	check(a, "original@example.com")
	late := register("late-credential", "late@example.com")
	check(late, "late@example.com")

	// A refresh changes the identity, not the grant.
	a.Metadata["email"] = "updated@example.com"
	var err error
	if a, err = manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	check(a, "updated@example.com")
	// So does a new label.
	a.Label = "public pool"
	if a, err = manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	check(a, "updated@example.com")
	// A credential registered again under the id of a deleted one is not given
	// the old one's replacer.
	if _, err = manager.Delete(context.Background(), late.ID); err != nil {
		t.Fatal(err)
	}
	replacement := register(late.ID, "replacement@example.com")
	check(replacement, "replacement@example.com")

	// Once revoked, the account is no longer the tenant's to have masked: its
	// text is the tenant's own and stays as written.
	manager.SetSharedAuthGrants(nil)
	raw := `{"message":"updated@example.com"}`
	if got := MaskSharedAccountText(sharedMaskTestTenant, raw); got != raw {
		t.Fatalf("a revoked share is still masked: %s", got)
	}
}

// Log writes and reads go on while the shared account is refreshed and the
// shares are replaced. Run with -race.
func TestSharedAccountMaskIsSafeWhileAccountsChange(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	InstallSharedAccountMasking(manager)
	t.Cleanup(func() { InstallSharedAccountMasking(nil) })
	a, err := manager.Register(context.Background(), &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSharedAuthGrants(map[string][]string{a.ID: {sharedMaskTestTenant}})
	index := a.EnsureIndex()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				row := RequestLogEntry{AuthIndex: index, DetailContent: `{"auth_id":"claude-owner@example.com.json"}`}
				maskSharedAccountRow(&row, sharedMaskTestTenant)
				if strings.Contains(row.DetailContent, "owner@example.com") {
					t.Error("the shared account is named")
					return
				}
				_ = MaskSharedAccountText(sharedMaskTestTenant, row.DetailContent)
			}
		}()
	}
	for i := range 200 {
		next := a.Clone()
		next.Metadata["email"] = fmt.Sprintf("owner-%d@example.com", i)
		if _, err := manager.Update(context.Background(), next); err != nil {
			t.Fatal(err)
		}
		manager.SetSharedAuthGrants(map[string][]string{a.ID: {sharedMaskTestTenant}})
	}
	close(stop)
	wg.Wait()
}

func TestSharedAccountMaskLeavesSuccessfulOutputAndPlatformUnchanged(t *testing.T) {
	a := &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}}
	InstallSharedAccountMasking(fakeSharedAccounts{sharedMaskTestTenant: {a}})
	t.Cleanup(func() { InstallSharedAccountMasking(nil) })
	original := RequestLogEntry{AuthIndex: a.EnsureIndex(), Source: "owner@example.com", ChannelName: "owner@example.com", DetailContent: `{"message":"owner@example.com"}`, OutputContent: `{"text":"owner@example.com"}`}
	row := original
	maskSharedAccountRow(&row, sharedMaskTestTenant)
	if row.OutputContent != original.OutputContent || strings.Contains(row.DetailContent, "owner@example.com") {
		t.Fatalf("row = %+v", row)
	}
	row = original
	maskSharedAccountRow(&row, systemTenantID)
	if row.Source != original.Source || row.DetailContent != original.DetailContent {
		t.Fatal("a platform row was rewritten")
	}
}

func TestSharedAccountMaskCacheBoundStillMasks(t *testing.T) {
	a := &coreauth.Auth{ID: "known-credential", Provider: "claude", Metadata: map[string]any{"email": "new@example.com"}}
	source := &sharedAccountMaskSource{lister: fakeSharedAccounts{sharedMaskTestTenant: {a}}, compiled: make(map[string]compiledSharedAccountMask)}
	source.compiled[a.ID] = compiledSharedAccountMask{} // a stale replacer of the same id
	for i := 1; i < maxCompiledSharedAccountMasks; i++ {
		source.compiled[fmt.Sprint(i)] = compiledSharedAccountMask{}
	}
	masks := source.masksFor(sharedMaskTestTenant)
	if len(masks) != 1 || len(source.compiled) != maxCompiledSharedAccountMasks || source.compiled[a.ID].mask.replacer == nil {
		t.Fatal("a full cache did not replace the stale replacer of a kept account")
	}
	if got := masks[0].replacer.Replace("new@example.com"); strings.Contains(got, "new@example.com") {
		t.Fatal("the account's current identity is not masked")
	}
	a.ID = "uncached-credential"
	masks = source.masksFor(sharedMaskTestTenant)
	if len(source.compiled) != maxCompiledSharedAccountMasks || len(masks) != 1 {
		t.Fatal("a full cache grew, or skipped an account")
	}
	if got := masks[0].replacer.Replace("new@example.com"); strings.Contains(got, "new@example.com") {
		t.Fatal("an account the full cache cannot keep is not masked")
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
